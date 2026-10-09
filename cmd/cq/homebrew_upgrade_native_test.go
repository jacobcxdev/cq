//go:build darwin

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/unix"
)

type nativeUpgradeEvidence struct {
	BootstrapRefusedSamples                                             int               `json:"bootstrap_refused_samples"`
	BootstrapMaxGapMilliseconds                                         float64           `json:"bootstrap_max_gap_ms"`
	Passed                                                              bool              `json:"passed"`
	ProductionUnchanged                                                 bool              `json:"production_unchanged"`
	Schema                                                              int               `json:"schema_version"`
	SourceHead                                                          string            `json:"source_head"`
	Inputs                                                              map[string]string `json:"input_sha256"`
	Fixtures                                                            map[string]string `json:"fixture_sha256"`
	Label                                                               string            `json:"job_label"`
	Port                                                                int               `json:"port"`
	Admitted, Completed, HTTP200, WS101, Status503, Refused, Mismatches int
	MaxAdmissionMilliseconds                                            float64  `json:"max_admission_ms"`
	MaxCoordinatorOwners                                                int      `json:"max_sampled_coordinator_owners"`
	SamePID                                                             bool     `json:"same_managed_pid"`
	ListenerIdentity                                                    string   `json:"listener_identity"`
	Outcomes                                                            []string `json:"outcomes"`
	Isolation                                                           string   `json:"isolation"`
}

type nativeUpgradeFixture struct {
	t                                                         *testing.T
	root, source, home, label, refresh, legacy, output, token string
	roots                                                     userdirs.Roots
	binaries                                                  map[string]string
	lastHTTP, lastWS                                          string
	url                                                       string
	mu                                                        sync.Mutex
	evidence                                                  nativeUpgradeEvidence
	hold                                                      chan struct{}
	admitted                                                  chan string
	terminal                                                  map[string]bool
	client                                                    *http.Client
}

func TestNativeHomebrewUpgradeAcceptance(t *testing.T) {
	if os.Getenv("CQ_RUN_NATIVE_HOMEBREW_UPGRADE") != "1" {
		t.Skip("opt-in isolated native acceptance")
	}
	productionHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	productionRoots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	productionPaths := []string{filepath.Join(productionRoots.Config, "config.toml"), filepath.Join(productionHome, "Library/LaunchAgents/dev.jacobcx.cq.proxy.plist"), filepath.Join(productionHome, "Library/LaunchAgents/dev.jacobcx.cq.refresh.plist")}
	production := nativeProductionSnapshot(t, productionPaths)
	t.Cleanup(func() {
		if got := nativeProductionSnapshot(t, productionPaths); got != production {
			t.Errorf("production service or config changed during isolated test")
		}
	})
	fixture := newNativeUpgradeFixture(t)
	defer fixture.saveEvidence()
	fixture.build()
	stopOwners := fixture.monitorOwners()
	defer stopOwners()
	fixture.bootstrapAdoption()
	pid := fixture.pid()
	fixture.traffic("before")
	fixture.compatibleTrafficUpgrade()
	fixture.traffic("after")
	if got := fixture.pid(); got != pid {
		t.Fatalf("managed PID changed: %d -> %d", pid, got)
	}
	fixture.evidence.SamePID = true
	if nativeProductionSnapshot(t, productionPaths) != production {
		t.Fatal("production state changed")
	}
	fixture.evidence.ProductionUnchanged = true
	receipt, err := (proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: fixture.roots}).Load()
	if err != nil || receipt.Phase != "committed" {
		t.Fatalf("commit: %+v %v", receipt, err)
	}
	fixture.evidence.ListenerIdentity = receipt.ListenerIdentity
	fixture.evidence.Outcomes = append(fixture.evidence.Outcomes, "compatible-upgrade-committed")
	fixture.packageOperation("reinstall", "0.34.1", "0.34.1", false)
	fixture.traffic("reinstall")
	fixture.failedUpgrades()
	fixture.deadlineDeferral()
	fixture.packageOperation("uninstall", "0.34.1", "", false)
	for _, label := range []string{fixture.label, fixture.refresh} {
		if out, err := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).CombinedOutput(); err == nil {
			t.Fatalf("uninstall retained job %s: %s", label, out)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.roots.State, "runtime-artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained artifacts survived uninstall: %v", err)
	}
	for _, path := range []string{filepath.Join(fixture.root, "caskroom/cq/0.34.1"), filepath.Join(fixture.root, "caskroom/cq/.metadata/0.34.1"), (proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: fixture.roots}).Path()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall retained package or journal: %s %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(fixture.roots.Config, "config.toml"), filepath.Join(fixture.home, ".codex/accounts/fixture.auth.json"), filepath.Join(fixture.roots.Logs, "proxy.log")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uninstall removed preserved user state: %s %v", path, err)
		}
	}
	fixture.evidence.Outcomes = append(fixture.evidence.Outcomes, "same-version-reinstall", "true-uninstall")
	if nativeProductionSnapshot(t, productionPaths) != production {
		t.Fatal("production state changed after package operations")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.evidence.Admitted != fixture.evidence.Completed || fixture.evidence.Status503 != 0 || fixture.evidence.Refused != 0 || fixture.evidence.Mismatches != 0 || fixture.evidence.MaxCoordinatorOwners > 1 {
		t.Fatalf("traffic acceptance: %+v", fixture.evidence)
	}
}

func newNativeUpgradeFixture(t *testing.T) *nativeUpgradeFixture {
	t.Helper()
	output := os.Getenv("CQ_UPGRADE_OUTPUT")
	if !filepath.IsAbs(output) {
		t.Fatal("absolute output directory required")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/private/tmp", "cqu-")
	if err != nil {
		t.Fatal(err)
	}
	f := &nativeUpgradeFixture{t: t, root: root, home: filepath.Join(root, "h"), source: filepath.Join(root, "src"), output: output, token: "synthetic-native-local-token", binaries: map[string]string{}, hold: make(chan struct{}), admitted: make(chan string, 64), terminal: map[string]bool{}}
	f.label = fmt.Sprintf("dev.jacobcx.cq.upgrade-validation.%d.%d", os.Getpid(), time.Now().UnixNano())
	f.refresh = f.label + ".refresh"
	f.legacy = f.label + ".legacy"
	f.evidence = nativeUpgradeEvidence{Schema: 1, Inputs: map[string]string{}, Fixtures: map[string]string{}, Label: f.label, Isolation: "temporary HOME; unique launchd labels; synthetic credentials; loopback upstream only; fixture source differs only at isolation boundaries"}
	for name, variable := range map[string]string{"candidate": "CQ_UPGRADE_CANDIDATE", "predecessor": "CQ_UPGRADE_PREDECESSOR"} {
		path := os.Getenv(variable)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		f.evidence.Inputs[name] = hex.EncodeToString(sum[:])
	}
	for _, dir := range []string{f.home, filepath.Join(f.home, ".codex"), f.source, filepath.Join(root, "prefix", "bin"), filepath.Join(root, "caskroom"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"GOPATH", "GOCACHE"} {
		if value, err := exec.Command("go", "env", key).Output(); err == nil {
			t.Setenv(key, strings.TrimSpace(string(value)))
		}
	}
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("CODEX_HOME", filepath.Join(f.home, ".codex"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	f.roots, err = userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, label := range []string{f.refresh, f.label, f.legacy} {
			exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Run()
		}
		released := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			file, err := os.Open(proxy.RuntimeLifecyclePath(f.roots.State))
			if errors.Is(err, os.ErrNotExist) {
				released = true
				break
			}
			if err != nil {
				break
			}
			lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if lockErr == nil {
				receipt, loadErr := (proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: f.roots}).Load()
				if loadErr == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					retireErr := proxy.RetireRuntimeUpgradeGuard(ctx, file, receipt)
					cancel()
					if retireErr != nil {
						t.Logf("isolated guard retained; evidence at %s", root)
						file.Close()
						return
					}
				}
				file.Close()
				released = true
				break
			}
			file.Close()
			time.Sleep(50 * time.Millisecond)
		}
		if !released {
			t.Logf("worker release unproven; retained fixture at %s", root)
			return
		}
		if t.Failed() {
			t.Logf("failed fixture retained at %s", root)
			return
		}
		os.RemoveAll(root)
	})
	if err := proxy.InitialiseProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: f.roots.State, Random: rand.Reader, Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(f.upstream))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		select {
		case <-f.hold:
		default:
			close(f.hold)
		}
	})
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	tcp.Close()
	if port == proxy.DefaultPort {
		t.Fatal("production port selected")
	}
	f.evidence.Port = port
	f.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := &proxy.Config{Port: port, ClaudeUpstream: upstream.URL, CodexUpstream: upstream.URL + "/codex", LocalToken: f.token, CodexTurnRouting: proxy.CodexRoutingEnforce, CodexWSTurnRouting: proxy.CodexRoutingEnforce, ProxyResilienceStateDir: f.roots.State, CodexWindowPriming: proxy.CodexWindowPrimingConfig{Enabled: false}}
	if err := proxy.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": 4102444800, "email": "fixture@example.invalid", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "native-fixture-account", "chatgpt_user_id": "native-fixture-user", "chatgpt_plan_type": "plus"}})
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	authData, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "id_token": token, "account_id": "native-fixture-account", "refresh_token": ""}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(f.home, ".codex", "auth.json"), authData, 0o600); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(f.home, ".codex", "accounts", "fixture.auth.json")
	managed := codex.ManagedRecord{Path: managedPath, Metadata: codex.ManagedMetadata{Version: 1, AccountKey: "native-fixture", CandidateID: "native-candidate", LineageID: "native-lineage", Generation: 1, Provenance: codex.ProvenanceLegacyUnknown, RefreshOwnership: codex.RefreshOwnershipUnknown, OperationState: codex.OperationReady}, Credential: codex.CredentialMaterial{AccessToken: token, IDToken: token, AccountID: "native-fixture-account"}, Document: map[string]any{"auth_mode": "chatgpt", "cq_expires_at": int64(4102444800000)}}
	store := codex.ManagedStore{FS: fsutil.OSFileSystem{}, Home: f.home, EnsureEpoch: func() error { return nil }}
	if err := store.Commit(&managed, ""); err != nil {
		t.Fatal(err)
	}
	cfg.CodexRoutingPinnedAccountKey = "native-fixture"
	if err := proxy.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cache := `{"fetched_at":"2026-10-09T00:00:00Z","client_version":"0.159.0","models":[{"slug":"gpt-5.6-sol","display_name":"Fixture","context_window":128000,"supported_in_api":true,"visibility":"list"}]}`
	os.WriteFile(filepath.Join(f.home, ".codex", "models_cache.json"), []byte(cache), 0o600)
	f.client = &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 60 * time.Second}
	return f
}

func (f *nativeUpgradeFixture) build() {
	f.t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = repo
	files, err := cmd.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = repo
	data, err := head.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	f.evidence.SourceHead = strings.TrimSpace(string(data))
	for _, rel := range bytes.Split(files, []byte{0}) {
		if len(rel) == 0 {
			continue
		}
		name := string(rel)
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			f.t.Fatal(err)
		}
		if strings.HasSuffix(name, ".go") || name == ".goreleaser.yml" {
			data = []byte(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(string(data), "dev.jacobcx.cq.proxy", f.label), "dev.jacobcx.cq.refresh", f.refresh), "homebrew.mxcl.cq", f.legacy))
		}
		target := filepath.Join(f.source, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	f.isolateSource(f.source)
	legacySource := filepath.Join(f.root, "legacy-src")
	archive := exec.Command("git", "archive", "55ab9937202597ff39c8e95ddf9a00611814aba3")
	archive.Dir = repo
	archived, err := archive.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(archived))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(legacySource, header.Name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			f.t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			f.t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, ".go") || header.Name == ".goreleaser.yml" {
			data = []byte(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(string(data), "dev.jacobcx.cq.proxy", f.label), "dev.jacobcx.cq.refresh", f.refresh), "homebrew.mxcl.cq", f.legacy))
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	f.isolateSource(legacySource)
	for _, spec := range []struct{ version, failure, source string }{{"0.33.11", "", legacySource}, {"0.34.0", "", f.source}, {"0.34.1", "", f.source}, {"0.34.2", "boot", f.source}, {"0.34.3", "crash", f.source}, {"0.34.4", "refresh", f.source}} {
		binary := filepath.Join(f.root, "cq-"+spec.version)
		command := exec.Command("go", "build", "-ldflags", fmt.Sprintf("-X main.version=%s -X main.nativeFixtureFailure=%s", spec.version, spec.failure), "-o", binary, "./cmd/cq")
		command.Dir = spec.source
		if output, err := command.CombinedOutput(); err != nil {
			f.t.Fatalf("fixture build: %v %s", err, output)
		}
		f.binaries[spec.version] = binary
		data, err := os.ReadFile(binary)
		if err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		f.evidence.Fixtures[spec.version] = hex.EncodeToString(sum[:])
	}
}

func (f *nativeUpgradeFixture) isolateSource(source string) {
	// Isolate process environment before main, including launchd-launched children.
	fixtureSource := fmt.Sprintf(`package main
import("os";"net/http";"fmt";"strings")
var nativeFixtureFailure string
func init(){
 os.Setenv("HOME",%q);os.Setenv("XDG_CONFIG_HOME","");os.Setenv("XDG_CACHE_HOME","");os.Setenv("CODEX_HOME",%q);os.Setenv("TMPDIR",%q)
 http.DefaultTransport= &http.Transport{Proxy:func(r *http.Request)(*url.URL,error){if r.URL.Hostname()!="127.0.0.1"&&r.URL.Hostname()!="localhost" {return nil,fmt.Errorf("fixture blocked non-loopback request")};return nil,nil}}
 if nativeFixtureFailure=="crash"&&len(os.Args)>1&&os.Args[1]=="--runtime-upgrade-resume"{os.Exit(73)}
 if nativeFixtureFailure=="boot"&&strings.Contains(strings.Join(os.Args," "),"--runtime-role worker"){os.Exit(74)}
 if nativeFixtureFailure=="refresh"&&len(os.Args)>1&&os.Args[1]=="refresh"{os.Exit(75)}
}
`, f.home, filepath.Join(f.home, ".codex"), filepath.Join(f.root, "tmp"))
	fixtureSource = strings.Replace(fixtureSource, `"strings"`, `"strings";"net/url"`, 1)
	if err := os.WriteFile(filepath.Join(source, "cmd/cq/native_upgrade_fixture.go"), []byte(fixtureSource), 0o600); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(source, "internal/keyring/keyring.go")
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("accounts = append(accounts, discoverPlatformKeychain(make(map[string]bool))...)"), []byte("// Native fixture reads only its synthetic credentials file."), 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *nativeUpgradeFixture) upstream(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "models") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"models":[{"slug":"gpt-5.6-sol","display_name":"Fixture","context_window":128000,"supported_in_api":true,"visibility":"list"}]}`)
		return
	}
	if strings.Contains(r.URL.Path, "usage") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":4102444800}}}`)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.respondTurn(body, func(data []byte) error { return conn.WriteMessage(websocket.TextMessage, data) })
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "fixture read", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	f.respondTurn(body, func(data []byte) error {
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		return err
	})
}

func (f *nativeUpgradeFixture) respondTurn(body []byte, write func([]byte) error) {
	var request struct {
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
		Previous       string                     `json:"previous_response_id"`
		Hold           bool                       `json:"fixture_hold"`
	}
	if json.Unmarshal(body, &request) != nil {
		return
	}
	var metadata struct {
		Turn string `json:"turn_id"`
	}
	json.Unmarshal(request.ClientMetadata["x-codex-turn-metadata"], &metadata)
	if metadata.Turn == "" {
		metadata.Turn = "unidentified"
	}
	f.mu.Lock()
	if request.Previous != "" && !f.terminal[request.Previous] {
		f.evidence.Mismatches++
	}
	f.evidence.Admitted++
	hold := f.hold
	f.mu.Unlock()
	select {
	case f.admitted <- metadata.Turn:
	default:
	}
	id := "response-" + metadata.Turn
	if err := write([]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id))); err != nil {
		return
	}
	if request.Hold {
		select {
		case <-hold:
		case <-time.After(2 * time.Minute):
			return
		}
	}
	if err := write([]byte(`{"type":"response.output_text.delta","delta":"fixture"}`)); err != nil {
		return
	}
	f.mu.Lock()
	f.terminal[id] = true
	f.mu.Unlock()
	if err := write([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"end_turn":true,"status":"completed","output":[]}}`, id))); err != nil {
		return
	}
	f.mu.Lock()
	f.evidence.Completed++
	f.mu.Unlock()
}

func (f *nativeUpgradeFixture) compatibleTrafficUpgrade() {
	f.t.Helper()
	// Finish requests already admitted by the old worker before opening its quiet boundary.
	turns := make(chan error, 2)
	go func() {
		defer func() {
			if recover() != nil {
				turns <- fmt.Errorf("held HTTP panic")
			}
		}()
		turns <- f.httpTurn("held-http", "", true)
	}()
	go func() {
		defer func() {
			if recover() != nil {
				turns <- fmt.Errorf("held WebSocket panic")
			}
		}()
		conn, err := f.wsTurn("held-ws", "", true)
		if conn != nil {
			conn.Close()
		}
		turns <- err
	}()
	wait := time.After(10 * time.Second)
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case turn := <-f.admitted:
			if turn == "held-http" || turn == "held-ws" {
				seen[turn] = true
			}
		case <-wait:
			f.t.Fatal("held turns were not admitted")
		}
	}
	idle, _, err := (&websocket.Dialer{Proxy: nil, HandshakeTimeout: 5 * time.Second}).Dial("ws"+strings.TrimPrefix(f.url, "http")+"/responses", http.Header{"Authorization": {"Bearer " + f.token}})
	if err != nil {
		f.t.Fatal(err)
	}
	defer idle.Close()
	slow, err := net.DialTimeout("tcp", strings.TrimPrefix(f.url, "http://"), time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	defer slow.Close()
	slowBody := nativeTurnBody("slow-headers", "", false, false)
	fmt.Fprintf(slow, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n", f.token, len(slowBody))
	release := make(chan struct{})
	go func() {
		defer func() {
			if recover() != nil {
				turns <- fmt.Errorf("release fixture panic")
			}
		}()
		store := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: f.roots}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			receipt, err := store.Load()
			if err == nil && receipt.Phase == "waiting" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		close(f.hold)
		fmt.Fprint(slow, "\r\n")
		slow.Write(slowBody)
		close(release)
	}()
	stop := make(chan struct{})
	shortDone := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				shortDone <- fmt.Errorf("short HTTP fixture panic")
			}
		}()
		for n := 0; ; n++ {
			select {
			case <-stop:
				shortDone <- nil
				return
			default:
			}
			if err := f.httpTurn(fmt.Sprintf("concurrent-%d", n), "", false); err != nil {
				shortDone <- err
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	f.packageOperation("upgrade", "0.34.1", "0.34.0", false)
	close(stop)
	if err := <-shortDone; err != nil {
		f.t.Fatal(err)
	}
	<-release
	for n := 0; n < 2; n++ {
		if err := <-turns; err != nil {
			f.t.Fatal(err)
		}
	}
	slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Contains(data, []byte("response.completed")) {
		f.t.Fatalf("slow headers lost: %d %s %v", response.StatusCode, data, err)
	}
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := idle.ReadMessage(); err == nil {
		f.t.Fatal("idle WebSocket survived worker retirement unexpectedly")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			f.t.Fatal("idle WebSocket retirement was not observed")
		}
	}
	reconnected, err := f.wsTurn("idle-reconnected", "", false)
	if err != nil {
		f.t.Fatal(err)
	}
	reconnected.Close()
	f.evidence.Outcomes = append(f.evidence.Outcomes, "admitted-SSE-and-multiframe-WebSocket-completed", "idle-WebSocket-reconnected", "slow-headers-completed", "concurrent-HTTP-survived")
}

func (f *nativeUpgradeFixture) traffic(turn string) {
	f.t.Helper()
	if err := f.httpTurn(turn+"-http", f.lastHTTP, false); err != nil {
		f.t.Fatal(err)
	}
	conn, err := f.wsTurn(turn+"-ws", f.lastWS, false)
	if err != nil {
		f.t.Fatal(err)
	}
	conn.Close()
	f.lastHTTP = "response-" + turn + "-http"
	f.lastWS = "response-" + turn + "-ws"
}
func nativeTurnBody(turn, previous string, hold bool, ws bool) []byte {
	thread := "native-" + turn
	if strings.HasSuffix(turn, "-http") && !strings.HasPrefix(turn, "held-") {
		thread = "native-http"
	}
	if strings.HasSuffix(turn, "-ws") && !strings.HasPrefix(turn, "held-") {
		thread = "native-ws"
	}
	metadata := map[string]string{"session_id": "native-session", "thread_id": thread, "turn_id": turn, "request_kind": "turn"}
	body := map[string]any{"model": "gpt-5.6-sol", "input": []any{}, "stream": true, "client_metadata": map[string]any{"x-codex-turn-metadata": metadata}, "fixture_hold": hold}
	if previous != "" {
		body["previous_response_id"] = previous
	}
	if ws {
		body["type"] = "response.create"
	}
	data, _ := json.Marshal(body)
	return data
}
func (f *nativeUpgradeFixture) httpTurn(turn, previous string, hold bool) error {
	start := time.Now()
	request, _ := http.NewRequest(http.MethodPost, f.url+"/v1/responses", bytes.NewReader(nativeTurnBody(turn, previous, hold, false)))
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(request)
	if err != nil {
		f.mu.Lock()
		if strings.Contains(err.Error(), "connection refused") {
			f.evidence.Refused++
		}
		f.mu.Unlock()
		return err
	}
	defer response.Body.Close()
	f.mu.Lock()
	if response.StatusCode == 503 {
		f.evidence.Status503++
	}
	if response.StatusCode == 200 {
		f.evidence.HTTP200++
	}
	delay := float64(time.Since(start)) / float64(time.Millisecond)
	if delay > f.evidence.MaxAdmissionMilliseconds {
		f.evidence.MaxAdmissionMilliseconds = delay
	}
	f.mu.Unlock()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || !bytes.Contains(data, []byte("response.completed")) || !bytes.Contains(data, []byte("response-"+turn)) {
		return fmt.Errorf("HTTP turn %s failed: %d %s", turn, response.StatusCode, data)
	}
	return nil
}
func (f *nativeUpgradeFixture) wsTurn(turn, previous string, hold bool) (*websocket.Conn, error) {
	header := http.Header{"Authorization": {"Bearer " + f.token}}
	conn, response, err := (&websocket.Dialer{Proxy: nil, HandshakeTimeout: 60 * time.Second}).Dial("ws"+strings.TrimPrefix(f.url, "http")+"/responses", header)
	if err != nil {
		if response != nil {
			data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			return nil, fmt.Errorf("WebSocket handshake: %d %s: %w", response.StatusCode, data, err)
		}
		return nil, err
	}
	if response.StatusCode != 101 {
		conn.Close()
		return nil, fmt.Errorf("WebSocket handshake %d", response.StatusCode)
	}
	f.mu.Lock()
	f.evidence.WS101++
	f.mu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, nativeTurnBody(turn, previous, hold, true)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return nil, err
		}
		if bytes.Contains(body, []byte(`"type":"error"`)) {
			conn.Close()
			return nil, fmt.Errorf("WebSocket error %s", body)
		}
		if bytes.Contains(body, []byte("response.completed")) {
			if !bytes.Contains(body, []byte("response-"+turn)) {
				conn.Close()
				return nil, fmt.Errorf("WebSocket terminal identity mismatch: %s", body)
			}
			break
		}
	}
	return conn, nil
}

func (f *nativeUpgradeFixture) pid() int {
	f.t.Helper()
	out, err := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), f.label)).Output()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "pid = "))
			if err == nil {
				return pid
			}
		}
	}
	f.t.Fatal("job PID missing")
	return 0
}

func (f *nativeUpgradeFixture) packageOperation(action, version, previous string, wantFailure bool) {
	f.t.Helper()
	// Each invocation reloads installed metadata through Homebrew's own loader.
	script := `require "cask/cask_loader"
require "cask/installer"
require "fileutils"
require "json"
CQFixtureInputs=JSON.parse(File.read(` + strconv.Quote(filepath.Join(f.root, "operation-input.json")) + `))
Object.send(:remove_const,:HOMEBREW_PREFIX)
HOMEBREW_PREFIX=Pathname(CQFixtureInputs.fetch("CQ_FIXTURE_PREFIX"))
Object.send(:remove_const,:HOMEBREW_CASKROOM)
HOMEBREW_CASKROOM=Pathname(CQFixtureInputs.fetch("CQ_FIXTURE_CASKROOM"))
Cask::Caskroom.singleton_class.define_method(:path){HOMEBREW_CASKROOM}
class CQFixtureCommand < SystemCommand
 def self.run(executable,**kwargs)
  File.open(CQFixtureInputs.fetch("CQ_FIXTURE_COMMANDS"),"a"){|f| f.puts(JSON.generate({executable:executable.to_s,args:kwargs[:args]}))}
  super
 end
end
block=File.read(CQFixtureInputs.fetch("CQ_FIXTURE_RELEASER")).split("    custom_block: |\n",2).last.lines.take_while{|line|line.strip.empty?||line.start_with?("      ")}.map{|line|line.sub(/^      /,"")}.join
version=CQFixtureInputs.fetch("CQ_FIXTURE_VERSION")
source="cask \"cq\" do\n version #{version.inspect}\n sha256 :no_check\n url \"https://example.invalid/cq.zip\"\n binary \"cq\"\n"+block.gsub("{{ .Version }}",version)+"\nend\n"
new_cask=Cask::CaskLoader::FromContentLoader.new(source).load(config:nil)
def installed(version)
 files=(HOMEBREW_CASKROOM/"cq"/".metadata"/version).glob("**/cq.*").select{|p|%w[.rb .json].include?(p.extname)}
 raise "installed metadata absent or ambiguous" unless files.length==1
 Cask::CaskLoader.load_from_installed_caskfile(files.first,api_fallback:false)
end
def installer(cask,**kwargs);Cask::Installer.new(cask,command:CQFixtureCommand,**kwargs);end
action=CQFixtureInputs.fetch("CQ_FIXTURE_ACTION")
if action=="uninstall"
 old=installed(version);installer(old).uninstall
else
 old=nil
 if action!="install"
  old=installed(CQFixtureInputs.fetch("CQ_FIXTURE_PREVIOUS"))
  installer(old,upgrade:true,reinstall:action=="reinstall").start_upgrade(successor:new_cask)
 end
 FileUtils.mkdir_p(new_cask.staged_path)
 FileUtils.cp(CQFixtureInputs.fetch("CQ_FIXTURE_BINARY"),new_cask.staged_path/"cq")
 begin
  replacement=installer(new_cask,upgrade:!old.nil?,reinstall:action=="reinstall")
  replacement.save_caskfile
  replacement.install_artifacts(predecessor:old)
  Cask::Tab.create(new_cask).write
  installer(old,upgrade:true).finalize_upgrade if old
 rescue
  installer(old,upgrade:true).revert_upgrade(predecessor:new_cask) if old
  raise
 end
end
`
	path := filepath.Join(f.root, "operation.rb")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		f.t.Fatal(err)
	}
	command := exec.Command("/opt/homebrew/bin/brew", "ruby", path)
	releaser := filepath.Join(f.source, ".goreleaser.yml")
	if version == "0.33.11" {
		releaser = filepath.Join(f.root, "legacy-src/.goreleaser.yml")
	}
	inputs := map[string]string{"CQ_FIXTURE_PREFIX": filepath.Join(f.root, "prefix"), "CQ_FIXTURE_CASKROOM": filepath.Join(f.root, "caskroom"), "CQ_FIXTURE_COMMANDS": filepath.Join(f.output, "native-commands.jsonl"), "CQ_FIXTURE_RELEASER": releaser, "CQ_FIXTURE_VERSION": version, "CQ_FIXTURE_PREVIOUS": previous, "CQ_FIXTURE_ACTION": action, "CQ_FIXTURE_BINARY": f.binaries[version]}
	data, _ := json.Marshal(inputs)
	if err := os.WriteFile(filepath.Join(f.root, "operation-input.json"), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	command.Env = append(os.Environ(), "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_NO_AUTO_UPDATE=1")

	out, err := command.CombinedOutput()
	os.WriteFile(filepath.Join(f.output, action+"-"+version+".log"), out, 0o600)
	if wantFailure {
		if err == nil {
			f.t.Fatalf("package %s unexpectedly succeeded", action)
		}
		return
	}
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(f.roots.Logs, "proxy.log"))
		f.t.Fatalf("package %s failed: %v\n%s\nproxy: %s", action, err, out, log)
	}
}
func (f *nativeUpgradeFixture) saveEvidence() {
	f.mu.Lock()
	f.evidence.Passed = !f.t.Failed()
	data, err := json.MarshalIndent(f.evidence, "", "  ")
	f.mu.Unlock()
	if err != nil {
		f.t.Error(err)
		return
	}
	if err := os.WriteFile(filepath.Join(f.output, "measurements.json"), append(data, '\n'), 0o600); err != nil {
		f.t.Error(err)
	}
}

func (f *nativeUpgradeFixture) monitorOwners() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				f.mu.Lock()
				f.evidence.MaxCoordinatorOwners = 2
				f.mu.Unlock()
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			output, err := exec.Command("/bin/ps", "-axo", "command=").Output()
			if err != nil {
				f.mu.Lock()
				f.evidence.MaxCoordinatorOwners = 2
				f.mu.Unlock()
				return
			}
			if err == nil {
				count := 0
				for _, line := range strings.Split(string(output), "\n") {
					if strings.Contains(line, f.root) && strings.Contains(line, "--runtime-role worker") {
						count++
					}
				}
				f.mu.Lock()
				if count > f.evidence.MaxCoordinatorOwners {
					f.evidence.MaxCoordinatorOwners = count
				}
				f.mu.Unlock()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return func() { close(stop); <-done }
}

func (f *nativeUpgradeFixture) assertPreviousRuntime() {
	f.t.Helper()
	path, err := exec.Command("/bin/ps", "-p", strconv.Itoa(f.pid()), "-o", "comm=").Output()
	if err != nil {
		f.t.Fatal(err)
	}
	data, err := os.ReadFile(strings.TrimSpace(string(path)))
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != f.evidence.Fixtures["0.34.1"] {
		f.t.Fatal("rollback did not select previous executable digest")
	}
	linked, err := filepath.EvalSymlinks(filepath.Join(f.root, "prefix/bin/cq"))
	if err != nil {
		f.t.Fatal(err)
	}
	if linked != filepath.Join(f.root, "caskroom/cq/0.34.1/cq") {
		f.t.Fatalf("package rollback link: %s", linked)
	}
}
func (f *nativeUpgradeFixture) failedUpgrades() {
	for _, version := range []string{"0.34.2", "0.34.3", "0.34.4"} {
		f.packageOperation("upgrade", version, "0.34.1", true)
		f.assertPreviousRuntime()
		f.traffic("rollback-" + version)
		f.evidence.Outcomes = append(f.evidence.Outcomes, "rolled-back-"+version)
	}
}
func (f *nativeUpgradeFixture) deadlineDeferral() {
	f.mu.Lock()
	f.hold = make(chan struct{})
	f.mu.Unlock()
	held := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				held <- fmt.Errorf("deferred HTTP panic")
			}
		}()
		held <- f.httpTurn("deferred-held", "", true)
	}()
	wait := time.After(10 * time.Second)
	for {
		select {
		case turn := <-f.admitted:
			if turn == "deferred-held" {
				goto admitted
			}
		case <-wait:
			f.t.Fatal("deferred turn not admitted")
		}
	}
admitted:
	observed := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				observed <- fmt.Errorf("deferral observer panic")
			}
		}()
		deadline := time.Now().Add(45 * time.Second)
		store := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: f.roots}
		for time.Now().Before(deadline) {
			r, err := store.Load()
			if err == nil && r.Phase == "deferred" {
				err = f.httpTurn("while-deferred", "", false)
				close(f.hold)
				observed <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(f.hold)
		observed <- fmt.Errorf("busy upgrade did not defer")
	}()
	f.packageOperation("upgrade", "0.34.0", "0.34.1", true)
	if err := <-observed; err != nil {
		f.t.Fatal(err)
	}
	if err := <-held; err != nil {
		f.t.Fatal(err)
	}
	f.assertPreviousRuntime()
	f.traffic("deferred-after")
	f.evidence.Outcomes = append(f.evidence.Outcomes, "deadline-deferred-old-worker-served")
}

func (f *nativeUpgradeFixture) bootstrapAdoption() {
	f.packageOperation("install", "0.33.11", "", false)
	f.traffic("legacy")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				f.mu.Lock()
				f.evidence.BootstrapRefusedSamples = -1
				f.mu.Unlock()
			}
		}()
		var absent time.Time
		for {
			select {
			case <-stop:
				return
			default:
			}
			c, err := net.DialTimeout("tcp", strings.TrimPrefix(f.url, "http://"), 100*time.Millisecond)
			now := time.Now()
			f.mu.Lock()
			if err != nil {
				f.evidence.BootstrapRefusedSamples++
				if absent.IsZero() {
					absent = now
				}
			} else {
				c.Close()
				if !absent.IsZero() {
					gap := float64(now.Sub(absent)) / float64(time.Millisecond)
					if gap > f.evidence.BootstrapMaxGapMilliseconds {
						f.evidence.BootstrapMaxGapMilliseconds = gap
					}
					absent = time.Time{}
				}
			}
			f.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	f.packageOperation("upgrade", "0.34.0", "0.33.11", false)
	close(stop)
	<-done
	f.traffic("adopted")
	f.evidence.Outcomes = append(f.evidence.Outcomes, "legacy-0.33.11-metadata-adopted-with-measured-maintenance-gap")
}

func nativeProductionSnapshot(t *testing.T, paths []string) string {
	t.Helper()
	hash := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		hash.Write([]byte(path))
		hash.Write(data)
	}
	out, err := exec.Command("/usr/sbin/lsof", "-nP", "-a", "-iTCP:19280", "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatal("production listener snapshot unavailable")
		}
	}
	hash.Write(out)
	return hex.EncodeToString(hash.Sum(nil))
}
