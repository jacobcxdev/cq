package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestOpenProxyCodexCanaryDoesNotUseOrdinaryPresenceRead(t *testing.T) {
	fsys := &noOrdinaryCanaryReadFS{MemFS: fsutil.NewMemFS()}
	required, _ := proxy.DefaultCodexRoutingRequirements("synthetic-build", "synthetic-client")
	recorder, err := openProxyCodexCanary(fsys, "/state/canary.json", "/config", "/state", required)
	if err != nil {
		t.Fatal(err)
	}
	if recorder != nil {
		t.Fatalf("recorder = %v, want nil", recorder)
	}
	if fsys.readFileCalls != 0 {
		t.Fatalf("ordinary ReadFile calls = %d, want 0", fsys.readFileCalls)
	}
}

func TestOpenProxyCodexCanaryIgnoresProtectedSourceWhenCanaryAbsent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	codexBarRoot := filepath.Join(home, "Library", "Application Support", "CodexBar")
	if err := os.MkdirAll(codexBarRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-manifest.json")
	writePrivateCanaryFixture(t, target, `{"version":3,"accounts":[]}`)
	if err := os.Symlink(target, filepath.Join(codexBarRoot, "managed-codex-accounts.json")); err != nil {
		t.Fatal(err)
	}
	fsys := &proxyCanaryHomeFS{MemFS: fsutil.NewMemFS(), home: home}
	required, _ := proxy.DefaultCodexRoutingRequirements("synthetic-build", "synthetic-client")
	recorder, err := openProxyCodexCanary(fsys, "/state/canary.json", "/config", "/state", required)
	if err != nil {
		t.Fatal(err)
	}
	if recorder != nil {
		t.Fatalf("recorder = %v, want nil", recorder)
	}
}

type proxyCanaryHomeFS struct {
	*fsutil.MemFS
	home string
}

func (fsys *proxyCanaryHomeFS) UserHomeDir() (string, error) { return fsys.home, nil }

type noOrdinaryCanaryReadFS struct {
	*fsutil.MemFS
	readFileCalls int
}

func (fsys *noOrdinaryCanaryReadFS) ReadFile(string) ([]byte, error) {
	fsys.readFileCalls++
	return nil, errors.New("ordinary read forbidden")
}

func TestOpenProxyCodexCanaryValidatesCurrentReadinessBeforeAttachment(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	t.Setenv("HOME", home)
	configDirectory := filepath.Join(t.TempDir(), "synthetic-config")
	stateDirectory := filepath.Join(t.TempDir(), "synthetic-state")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "proxy.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writePrivateCanaryFixture(t, filepath.Join(home, ".codex", "auth.json"), `{"fixture":"system"}`)
	writePrivateCanaryFixture(t, filepath.Join(home, ".codex", "accounts", "registry.json"), `{"fixture":"registry"}`)
	required, _ := proxy.DefaultCodexRoutingRequirements("synthetic-build", "synthetic-client")
	marker := proxy.CodexReadinessMarker{
		Version: proxy.CodexReadinessMarkerVersion, Transport: proxy.CodexRoutingHTTP,
		CQBuild: required.CQBuild, ParserSchema: required.ParserSchema, LeaseSchema: required.LeaseSchema,
		SemanticsRevision: required.SemanticsRevision, ClientBuild: required.ClientBuild,
		RetryBudget: required.RetryBudget, FixtureHash: required.FixtureHash,
		CQExecutableSHA256: strings.Repeat("1", 64), ClientExecutableSHA256: strings.Repeat("2", 64),
		ServiceKind: "launchd", ServiceIdentitySHA256: strings.Repeat("3", 64),
		InstalledResult: "passed", CompletedGates: append([]string(nil), required.RequiredGates...),
		ValidatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	markerBytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	markerBytes = append(markerBytes, '\n')
	if err := os.WriteFile(filepath.Join(stateDirectory, "codex-readiness-http.json"), markerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	tuple, err := proxy.BuildCodexCanaryTuple(required, marker)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := codexCanaryProtections(home, configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDirectory, "codex-routing-canary.json")
	started, err := proxy.StartCodexCanary(fsutil.OSFileSystem{}, statePath, protected, tuple, marker.ValidatedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := started.Close(); err != nil {
		t.Fatal(err)
	}

	recorder, err := openProxyCodexCanary(fsutil.OSFileSystem{}, statePath, configDirectory, stateDirectory, required)
	if err != nil || recorder == nil {
		t.Fatalf("open active canary = %v, %v", recorder, err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	drifted := required
	drifted.SemanticsRevision = "stale-semantics"
	if _, err := openProxyCodexCanary(fsutil.OSFileSystem{}, statePath, configDirectory, stateDirectory, drifted); err == nil {
		t.Fatal("expected stale current requirements rejection")
	}
}

func TestCodexCanaryEndpointRecoveryRecorderPersistsBeforeAllowingRecovery(t *testing.T) {
	fsys := fsutil.NewMemFS()
	recorder, err := proxy.StartCodexCanary(fsys, "/state/canary.json", nil, proxy.CodexCanaryTuple{
		CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1,
		SemanticsRevision: "semantics", RetryBudget: 1, FixtureHash: "fixture",
		ReadinessFingerprint: strings.Repeat("a", 64),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := codexCanaryEndpointRecoveryRecorder(recorder)(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().LiveSessionRepairs; got != 1 {
		t.Fatalf("live session repairs = %d, want 1", got)
	}
	persisted, err := fsys.ReadFile("/state/canary.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"live_session_repairs": 1`) {
		t.Fatalf("persisted state did not contain recovery counter: %s", persisted)
	}
}

func TestValidateProxyCodexCanaryRuntimeRequiresExactHTTPEnforcement(t *testing.T) {
	canary := &proxy.CodexCanaryRecorder{}
	valid := &proxy.CodexRoutingRuntime{
		HTTP: proxy.CodexModeStatus{
			Configured: proxy.CodexRoutingEnforce, Effective: proxy.CodexRoutingEnforce,
			ModeEpoch: 7, AuthoritativeEpoch: 7,
		},
		WebSocket: proxy.CodexModeStatus{Configured: proxy.CodexRoutingObserve, Effective: proxy.CodexRoutingObserve, ModeEpoch: 7},
	}
	if err := validateProxyCodexCanaryRuntime(nil, nil); err != nil {
		t.Fatalf("absent canary = %v", err)
	}
	if err := validateProxyCodexCanaryRuntime(canary, valid); err != nil {
		t.Fatalf("valid runtime = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*proxy.CodexRoutingRuntime)
	}{
		{"missing runtime", func(runtime *proxy.CodexRoutingRuntime) { *runtime = proxy.CodexRoutingRuntime{} }},
		{"HTTP inhibited", func(runtime *proxy.CodexRoutingRuntime) { runtime.HTTP.Effective = proxy.CodexRoutingObserve }},
		{"authority epoch drift", func(runtime *proxy.CodexRoutingRuntime) { runtime.HTTP.AuthoritativeEpoch++ }},
		{"WebSocket enforce", func(runtime *proxy.CodexRoutingRuntime) { runtime.WebSocket.Effective = proxy.CodexRoutingEnforce }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := *valid
			test.mutate(&changed)
			if err := validateProxyCodexCanaryRuntime(canary, &changed); err == nil {
				t.Fatal("invalid canary runtime accepted")
			}
		})
	}
}

func TestNewProxyCodexCanaryStopRequiresServingOwners(t *testing.T) {
	if stop, err := newProxyCodexCanaryStop(nil, nil, nil); err != nil || stop != nil {
		t.Fatalf("absent canary stop = %v, %v", stop, err)
	}
	recorder := &proxy.CodexCanaryRecorder{}
	if _, err := newProxyCodexCanaryStop(recorder, nil, nil); err == nil {
		t.Fatal("missing continuity accepted")
	}
	continuity := &proxyCodexContinuity{Runtime: &proxy.CodexLeaseRuntime{}}
	stop, err := newProxyCodexCanaryStop(recorder, continuity, proxyCanaryStopNativeHandler{})
	if err != nil || stop == nil {
		t.Fatalf("complete canary stop = %v, %v", stop, err)
	}
}

type proxyCanaryStopNativeHandler struct{}

func (proxyCanaryStopNativeHandler) TryServe(http.ResponseWriter, *http.Request, bool) (bool, string) {
	return true, ""
}

func (proxyCanaryStopNativeHandler) CloseAndDrain(context.Context) error { return nil }
