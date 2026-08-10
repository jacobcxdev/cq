//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttputil "github.com/jacobcxdev/cq/internal/httputil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const legacyMaintenanceHealthMaxBytes = 64 << 10

const legacyMaintenanceProcessProofTimeout = 2 * time.Second

var (
	errLegacyMaintenanceRuntimeNotReady         = errors.New("legacy maintenance candidate runtime is not ready")
	errLegacyMaintenanceProcessProofUnavailable = errors.New("legacy maintenance candidate process proof is unavailable")
)

type legacyMaintenanceHeadroomProber interface {
	Probe(context.Context) error
}

type legacyMaintenanceHeadroomProberFunc func(context.Context) error

func (probe legacyMaintenanceHeadroomProberFunc) Probe(ctx context.Context) error {
	return probe(ctx)
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

	build         string
	clientBuild   string
	healthURL     string
	healthAddr    string
	attestor      *proxy.ServingAttestor
	dialContext   func(context.Context, string, string) (net.Conn, error)
	executable    legacyMaintenanceExecutableProof
	initialErr    error
	capture       func() (legacyMaintenanceExecutableProof, error)
	runtime       *proxy.CodexRoutingRuntime
	frozen        proxy.CodexRoutingRuntime
	headroom      bool
	headroomProbe legacyMaintenanceHeadroomProber
	headroomMode  proxy.HeadroomMode
	bound         bool
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

func newProxyLegacyMaintenanceFinaliseVerifier(build, clientBuild string, port int, attestor *proxy.ServingAttestor) *proxyLegacyMaintenanceFinaliseVerifier {
	capture := captureLegacyMaintenanceExecutable
	proof, err := capture()
	healthAddr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	dialer := &net.Dialer{}
	return &proxyLegacyMaintenanceFinaliseVerifier{
		build: build, clientBuild: clientBuild,
		healthURL:  "http://" + healthAddr + "/health",
		healthAddr: healthAddr,
		attestor:   attestor,
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		executable: proof, initialErr: err, capture: capture,
	}
}

func (v *proxyLegacyMaintenanceFinaliseVerifier) bind(runtime *proxy.CodexRoutingRuntime, headroom *proxy.HeadroomBridge, mode proxy.HeadroomMode) error {
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
	v.headroom = headroom != nil
	if headroom != nil {
		// HeadroomBridge.Probe is supplied by the independently owned headroom
		// slice. The assertion keeps this branch buildable before that commit is
		// integrated while finalise remains strictly fail-closed without it.
		v.headroomProbe, _ = any(headroom).(legacyMaintenanceHeadroomProber)
	}
	v.headroomMode = mode
	v.bound = true
	return nil
}

func (v *proxyLegacyMaintenanceFinaliseVerifier) AcquireLegacyMaintenanceFinalise(ctx context.Context, proof codexprov.LegacyMaintenanceFinaliseVerification) (codexprov.LegacyMaintenanceFinaliseLease, error) {
	if ctx == nil {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == nil || !validLegacyMaintenanceRuntimeProof(proof) {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	v.mu.RLock()
	build, clientBuild := v.build, v.clientBuild
	healthURL, healthAddr := v.healthURL, v.healthAddr
	attestor, dialContext := v.attestor, v.dialContext
	executable, initialErr, capture := v.executable, v.initialErr, v.capture
	runtime, frozen := v.runtime, cloneLegacyMaintenanceRoutingRuntime(v.frozen)
	headroom, headroomProbe, headroomMode, bound := v.headroom, v.headroomProbe, v.headroomMode, v.bound
	v.mu.RUnlock()
	if !bound || initialErr != nil || capture == nil || attestor == nil || dialContext == nil ||
		healthURL == "" || healthAddr == "" || strings.TrimSpace(build) == "" ||
		strings.TrimSpace(clientBuild) == "" || runtime == nil || !headroom ||
		headroomProbe == nil || headroomMode != proxy.HeadroomModeCache {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	if !legacyMaintenanceRoutingRuntimeEqual(*runtime, frozen) || !legacyMaintenanceRoutingReady(frozen) {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	currentExecutable, err := capture()
	if err != nil || currentExecutable != executable {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, legacyMaintenanceProcessProofTimeout)
	probeErr := headroomProbe.Probe(probeCtx)
	cancelProbe()
	if probeErr != nil {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	binding, err := legacyMaintenanceProcessBinding(proof, build, clientBuild, executable, frozen)
	if err != nil {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	servingLease, err := attestor.Acquire(binding)
	if err != nil {
		return nil, errors.Join(errLegacyMaintenanceRuntimeNotReady, errLegacyMaintenanceProcessProofUnavailable)
	}
	release := true
	defer func() {
		if release {
			servingLease.Release()
		}
	}()
	data, encodedProof, local, remote, err := requestLegacyMaintenanceProcessHealth(
		ctx, healthURL, healthAddr, servingLease.Challenge(), dialContext,
	)
	if err != nil || servingLease.VerifyResponse(data, encodedProof, local, remote) != nil {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	health, err := decodeLegacyMaintenanceRuntimeHealth(data)
	if err != nil || !legacyMaintenanceRuntimeHealthReady(health, frozen) {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := servingLease.Seal(); err != nil {
		return nil, errLegacyMaintenanceRuntimeNotReady
	}
	release = false
	return servingLease, nil
}

func legacyMaintenanceProcessBinding(proof codexprov.LegacyMaintenanceFinaliseVerification, build, clientBuild string, executable legacyMaintenanceExecutableProof, runtime proxy.CodexRoutingRuntime) ([sha256.Size]byte, error) {
	var binding [sha256.Size]byte
	ticketHash, err := hex.DecodeString(proof.TicketHash)
	if err != nil || len(ticketHash) != sha256.Size {
		return binding, errLegacyMaintenanceRuntimeNotReady
	}
	ownerGeneration, err := hex.DecodeString(proof.OwnerGeneration)
	if err != nil || len(ownerGeneration) != 16 || executable.path == "" || executable.device == 0 ||
		executable.inode == 0 || executable.links == 0 || executable.size < 0 || !executable.mode.IsRegular() ||
		executable.sha256 == ([sha256.Size]byte{}) {
		return binding, errLegacyMaintenanceRuntimeNotReady
	}
	runtimeData, err := json.Marshal(runtime)
	if err != nil {
		return binding, errLegacyMaintenanceRuntimeNotReady
	}
	destination := sha256.New()
	writeLegacyMaintenanceBindingField(destination, []byte("cq-legacy-maintenance-process-binding-v1"))
	writeLegacyMaintenanceBindingField(destination, ticketHash)
	writeLegacyMaintenanceBindingField(destination, ownerGeneration)
	writeLegacyMaintenanceBindingField(destination, []byte(build))
	writeLegacyMaintenanceBindingField(destination, []byte(clientBuild))
	writeLegacyMaintenanceBindingField(destination, []byte(executable.path))
	writeLegacyMaintenanceBindingUint64(destination, executable.device)
	writeLegacyMaintenanceBindingUint64(destination, executable.inode)
	writeLegacyMaintenanceBindingUint64(destination, executable.links)
	writeLegacyMaintenanceBindingUint64(destination, uint64(executable.size))
	writeLegacyMaintenanceBindingUint64(destination, uint64(executable.mode))
	writeLegacyMaintenanceBindingField(destination, executable.sha256[:])
	writeLegacyMaintenanceBindingField(destination, runtimeData)
	copy(binding[:], destination.Sum(nil))
	return binding, nil
}

func writeLegacyMaintenanceBindingUint64(destination hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeLegacyMaintenanceBindingField(destination, encoded[:])
}

func writeLegacyMaintenanceBindingField(destination hash.Hash, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func requestLegacyMaintenanceProcessHealth(ctx context.Context, healthURL, healthAddr, challenge string, dialContext func(context.Context, string, string) (net.Conn, error)) ([]byte, string, string, string, error) {
	if ctx == nil || healthURL == "" || healthAddr == "" || challenge == "" || dialContext == nil {
		return nil, "", "", "", errLegacyMaintenanceRuntimeNotReady
	}
	requestCtx, cancel := context.WithTimeout(ctx, legacyMaintenanceProcessProofTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, "", "", "", errLegacyMaintenanceRuntimeNotReady
	}
	request.Header.Set(proxy.ServingProofChallengeHeader, challenge)
	request.Header.Set("Connection", "close")
	var localAddress, remoteAddress string
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused || info.WasIdle || info.Conn == nil {
				return
			}
			localAddress = info.Conn.LocalAddr().String()
			remoteAddress = info.Conn.RemoteAddr().String()
		},
	}))
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  true,
		ForceAttemptHTTP2:  false,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != healthAddr {
				return nil, errLegacyMaintenanceRuntimeNotReady
			}
			return dialContext(ctx, "tcp4", healthAddr)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", "", "", errLegacyMaintenanceRuntimeNotReady
	}
	defer response.Body.Close()
	proofValues := response.Header.Values(proxy.ServingProofResponseHeader)
	if response.StatusCode != http.StatusOK || len(proofValues) != 1 ||
		response.Header.Get("Content-Encoding") != "" || localAddress == "" || remoteAddress == "" {
		return nil, "", "", "", errLegacyMaintenanceRuntimeNotReady
	}
	data, err := readLegacyMaintenanceHealth(response.Body)
	if err != nil {
		return nil, "", "", "", errLegacyMaintenanceRuntimeNotReady
	}
	return data, proofValues[0], localAddress, remoteAddress, nil
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
	data, err := cqhttputil.ReadBody(reader)
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
