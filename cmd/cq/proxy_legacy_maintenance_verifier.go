//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const legacyMaintenanceHealthMaxBytes = 64 << 10

var (
	errLegacyMaintenanceRuntimeNotReady         = errors.New("legacy maintenance candidate runtime is not ready")
	errLegacyMaintenanceProcessProofUnavailable = errors.New("legacy maintenance candidate process proof is unavailable")
)

type legacyMaintenanceHealthDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type legacyMaintenanceExecutableProof struct {
	path   string
	device uint64
	inode  uint64
	links  uint64
	size   int64
	mode   os.FileMode
	sha256 [sha256.Size]byte
}

type proxyLegacyMaintenanceFinaliseVerifier struct {
	mu sync.RWMutex

	build        string
	clientBuild  string
	healthURL    string
	http         legacyMaintenanceHealthDoer
	executable   legacyMaintenanceExecutableProof
	initialErr   error
	capture      func() (legacyMaintenanceExecutableProof, error)
	runtime      *proxy.CodexRoutingRuntime
	frozen       proxy.CodexRoutingRuntime
	headroom     bool
	headroomMode proxy.HeadroomMode
	processProof func(context.Context) error
	bound        bool
}

type legacyMaintenanceRuntimeHealth struct {
	Status          string                         `json:"status"`
	Headroom        bool                           `json:"headroom"`
	HeadroomMode    string                         `json:"headroom_mode"`
	Accounts        legacyMaintenanceAccountHealth `json:"accounts"`
	InventoryHealth string                         `json:"codex_inventory_health"`
	ExternalSources []proxy.CodexSourceHealth      `json:"codex_external_sources"`
	HTTP            proxy.CodexModeStatus          `json:"codex_turn_routing"`
	WebSocket       proxy.CodexModeStatus          `json:"codex_ws_turn_routing"`
	RoutingDefault  legacyMaintenanceDefaultHealth `json:"codex_routing_default"`
}

type legacyMaintenanceAccountHealth struct {
	Codex *int `json:"codex"`
}

type legacyMaintenanceDefaultHealth struct {
	Configured bool   `json:"configured"`
	Resolved   bool   `json:"resolved"`
	Routable   bool   `json:"routable"`
	Status     string `json:"status"`
}

func newProxyLegacyMaintenanceFinaliseVerifier(build, clientBuild string, port int) *proxyLegacyMaintenanceFinaliseVerifier {
	capture := captureLegacyMaintenanceExecutable
	proof, err := capture()
	return &proxyLegacyMaintenanceFinaliseVerifier{
		build: build, clientBuild: clientBuild,
		healthURL: "http://" + net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)) + "/health",
		http: &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		executable: proof, initialErr: err, capture: capture,
	}
}

func (v *proxyLegacyMaintenanceFinaliseVerifier) bind(runtime *proxy.CodexRoutingRuntime, headroom bool, mode proxy.HeadroomMode) error {
	if v == nil || runtime == nil {
		return errLegacyMaintenanceRuntimeNotReady
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.bound {
		return errLegacyMaintenanceRuntimeNotReady
	}
	v.runtime = runtime
	v.frozen = cloneLegacyMaintenanceRoutingRuntime(*runtime)
	v.headroom = headroom
	v.headroomMode = mode
	v.bound = true
	return nil
}

func (v *proxyLegacyMaintenanceFinaliseVerifier) VerifyLegacyMaintenanceFinalise(ctx context.Context, proof codexprov.LegacyMaintenanceFinaliseVerification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v == nil || !validLegacyMaintenanceRuntimeProof(proof) {
		return errLegacyMaintenanceRuntimeNotReady
	}
	v.mu.RLock()
	build, clientBuild := v.build, v.clientBuild
	healthURL, client := v.healthURL, v.http
	executable, initialErr, capture := v.executable, v.initialErr, v.capture
	runtime, frozen := v.runtime, cloneLegacyMaintenanceRoutingRuntime(v.frozen)
	headroom, headroomMode, bound := v.headroom, v.headroomMode, v.bound
	processProof := v.processProof
	v.mu.RUnlock()
	if !bound || initialErr != nil || capture == nil || client == nil || healthURL == "" ||
		strings.TrimSpace(build) == "" || strings.TrimSpace(clientBuild) == "" || runtime == nil {
		return errLegacyMaintenanceRuntimeNotReady
	}
	if !legacyMaintenanceRoutingRuntimeEqual(*runtime, frozen) || !legacyMaintenanceRoutingReady(frozen) ||
		!headroom || headroomMode != proxy.HeadroomModeCache {
		return errLegacyMaintenanceRuntimeNotReady
	}
	if processProof == nil {
		return errors.Join(errLegacyMaintenanceRuntimeNotReady, errLegacyMaintenanceProcessProofUnavailable)
	}
	if err := processProof(ctx); err != nil {
		return errors.Join(errLegacyMaintenanceRuntimeNotReady, errLegacyMaintenanceProcessProofUnavailable)
	}
	currentExecutable, err := capture()
	if err != nil || currentExecutable != executable {
		return errLegacyMaintenanceRuntimeNotReady
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return errLegacyMaintenanceRuntimeNotReady
	}
	response, err := client.Do(request)
	if err != nil {
		return errLegacyMaintenanceRuntimeNotReady
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errLegacyMaintenanceRuntimeNotReady
	}
	data, err := readLegacyMaintenanceHealth(response.Body)
	if err != nil {
		return errLegacyMaintenanceRuntimeNotReady
	}
	health, err := decodeLegacyMaintenanceRuntimeHealth(data)
	if err != nil || !legacyMaintenanceRuntimeHealthReady(health, frozen) {
		return errLegacyMaintenanceRuntimeNotReady
	}
	return ctx.Err()
}

func captureLegacyMaintenanceExecutable() (legacyMaintenanceExecutableProof, error) {
	path, err := os.Executable()
	if err != nil {
		return legacyMaintenanceExecutableProof{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return legacyMaintenanceExecutableProof{}, err
	}
	path = filepath.Clean(path)
	file, err := os.Open(path)
	if err != nil {
		return legacyMaintenanceExecutableProof{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return legacyMaintenanceExecutableProof{}, errors.Join(errLegacyMaintenanceRuntimeNotReady, err)
	}
	identity, ok := (fsutil.OSFileSystem{}).FileIdentity(before)
	if !ok || identity.Device == 0 || identity.Inode == 0 || identity.Links == 0 {
		return legacyMaintenanceExecutableProof{}, errLegacyMaintenanceRuntimeNotReady
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return legacyMaintenanceExecutableProof{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return legacyMaintenanceExecutableProof{}, err
	}
	afterIdentity, ok := (fsutil.OSFileSystem{}).FileIdentity(after)
	if !ok || afterIdentity != identity || after.Size() != before.Size() || after.Mode() != before.Mode() {
		return legacyMaintenanceExecutableProof{}, errLegacyMaintenanceRuntimeNotReady
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return legacyMaintenanceExecutableProof{
		path: path, device: identity.Device, inode: identity.Inode, links: identity.Links,
		size: before.Size(), mode: before.Mode(), sha256: digest,
	}, nil
}

func readLegacyMaintenanceHealth(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, legacyMaintenanceHealthMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > legacyMaintenanceHealthMaxBytes {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	return data, nil
}

func decodeLegacyMaintenanceRuntimeHealth(data []byte) (legacyMaintenanceRuntimeHealth, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := rejectLegacyMaintenanceDuplicateJSONKeys(decoder); err != nil {
		return legacyMaintenanceRuntimeHealth{}, err
	}
	decoder = json.NewDecoder(strings.NewReader(string(data)))
	var health legacyMaintenanceRuntimeHealth
	if err := decoder.Decode(&health); err != nil {
		return legacyMaintenanceRuntimeHealth{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return legacyMaintenanceRuntimeHealth{}, errLegacyMaintenanceRuntimeNotReady
	}
	return health, nil
}

func rejectLegacyMaintenanceDuplicateJSONKeys(decoder *json.Decoder) error {
	if err := consumeLegacyMaintenanceJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errLegacyMaintenanceRuntimeNotReady
	}
	return nil
}

func consumeLegacyMaintenanceJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errLegacyMaintenanceRuntimeNotReady
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errLegacyMaintenanceRuntimeNotReady
			}
			if _, duplicate := seen[key]; duplicate {
				return errLegacyMaintenanceRuntimeNotReady
			}
			seen[key] = struct{}{}
			if err := consumeLegacyMaintenanceJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errLegacyMaintenanceRuntimeNotReady
		}
	case '[':
		for decoder.More() {
			if err := consumeLegacyMaintenanceJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errLegacyMaintenanceRuntimeNotReady
		}
	default:
		return errLegacyMaintenanceRuntimeNotReady
	}
	return nil
}

func validLegacyMaintenanceRuntimeProof(proof codexprov.LegacyMaintenanceFinaliseVerification) bool {
	return validLowerHex(proof.TicketHash, sha256.Size*2) && validLowerHex(proof.OwnerGeneration, 32)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == length
}

func cloneLegacyMaintenanceRoutingRuntime(runtime proxy.CodexRoutingRuntime) proxy.CodexRoutingRuntime {
	runtime.HTTP.RetainedAuthoritativeEpochs = append([]uint64(nil), runtime.HTTP.RetainedAuthoritativeEpochs...)
	runtime.WebSocket.RetainedAuthoritativeEpochs = append([]uint64(nil), runtime.WebSocket.RetainedAuthoritativeEpochs...)
	return runtime
}

func legacyMaintenanceRoutingRuntimeEqual(left, right proxy.CodexRoutingRuntime) bool {
	return reflect.DeepEqual(left, right)
}

func legacyMaintenanceRoutingReady(runtime proxy.CodexRoutingRuntime) bool {
	return runtime.HTTP.Configured == proxy.CodexRoutingEnforce &&
		runtime.HTTP.Effective == proxy.CodexRoutingEnforce && runtime.HTTP.InhibitionReason == "" &&
		runtime.WebSocket.Configured == proxy.CodexRoutingObserve &&
		runtime.WebSocket.Effective == proxy.CodexRoutingObserve && runtime.WebSocket.InhibitionReason == ""
}

func legacyMaintenanceRuntimeHealthReady(health legacyMaintenanceRuntimeHealth, frozen proxy.CodexRoutingRuntime) bool {
	if health.Status != "ok" || !health.Headroom || health.HeadroomMode != proxy.HeadroomModeCache.String() ||
		health.Accounts.Codex == nil || *health.Accounts.Codex <= 0 || health.InventoryHealth != "ok" ||
		!reflect.DeepEqual(health.HTTP, frozen.HTTP) || !reflect.DeepEqual(health.WebSocket, frozen.WebSocket) ||
		!health.RoutingDefault.Configured || !health.RoutingDefault.Resolved || !health.RoutingDefault.Routable ||
		health.RoutingDefault.Status != "resolved" {
		return false
	}
	for _, source := range health.ExternalSources {
		if source.HealthCode != "ok" {
			return false
		}
	}
	return true
}
