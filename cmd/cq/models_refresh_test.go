package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func parsePort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return port
}

func TestRunRegistryRefresh_ProxyHandled_SkipsLocal(t *testing.T) {
	localCalled := false
	err := runRegistryRefresh(registryRefreshStrategy{
		TryProxy:     func() (bool, error) { return true, nil },
		LocalRefresh: func() error { localCalled = true; return nil },
	})
	if err != nil {
		t.Fatalf("runRegistryRefresh: %v", err)
	}
	if localCalled {
		t.Fatal("LocalRefresh must not run when proxy handled the refresh")
	}
}

func TestRunRegistryRefresh_ProxyNotHandled_FallsBackToLocal(t *testing.T) {
	localCalled := false
	err := runRegistryRefresh(registryRefreshStrategy{
		TryProxy:     func() (bool, error) { return false, nil },
		LocalRefresh: func() error { localCalled = true; return nil },
	})
	if err != nil {
		t.Fatalf("runRegistryRefresh: %v", err)
	}
	if !localCalled {
		t.Fatal("LocalRefresh must run when proxy did not handle the refresh")
	}
}

func TestRunRegistryRefresh_ProxyError_Surfaced(t *testing.T) {
	proxyErr := errors.New("proxy auth failed")
	localCalled := false
	err := runRegistryRefresh(registryRefreshStrategy{
		TryProxy:     func() (bool, error) { return false, proxyErr },
		LocalRefresh: func() error { localCalled = true; return nil },
	})
	if !errors.Is(err, proxyErr) {
		t.Fatalf("err = %v, want wrapping %v", err, proxyErr)
	}
	if localCalled {
		t.Fatal("LocalRefresh must not run when proxy returned an error")
	}
}

func TestAttemptProxyRegistryRefresh_2xxHandled(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	port := parsePort(t, srv.URL)
	handled, err := attemptProxyRegistryRefresh(context.Background(), srv.Client(), port, "tok")
	if err != nil {
		t.Fatalf("attemptProxyRegistryRefresh: %v", err)
	}
	if !handled {
		t.Fatal("handled = false, want true for 2xx")
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization header = %q, want Bearer tok", gotAuth)
	}
	if gotPath != "/v1/registry/refresh" {
		t.Errorf("path = %q, want /v1/registry/refresh", gotPath)
	}
}

func TestAttemptProxyRegistryRefresh_404FallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	port := parsePort(t, srv.URL)
	handled, err := attemptProxyRegistryRefresh(context.Background(), srv.Client(), port, "tok")
	if err != nil {
		t.Fatalf("attemptProxyRegistryRefresh: %v (404 should fall back, not error)", err)
	}
	if handled {
		t.Fatal("handled = true, want false (404 means unsupported endpoint)")
	}
}

func TestAttemptProxyRegistryRefresh_403_SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	port := parsePort(t, srv.URL)
	handled, err := attemptProxyRegistryRefresh(context.Background(), srv.Client(), port, "wrong-tok")
	if err == nil {
		t.Fatal("err = nil, want HTTP 403 error (auth failure must not silently fall back)")
	}
	if handled {
		t.Fatal("handled = true, want false on auth error")
	}
}

// TestAttemptProxyRegistryRefresh_500_SurfacesErrorWithBody verifies that when
// the proxy returns a 5xx, the error returned by attemptProxyRegistryRefresh
// includes the response body text so callers can diagnose the failure.
func TestAttemptProxyRegistryRefresh_500_SurfacesErrorWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"registry source failed: connection reset"}}`))
	}))
	defer srv.Close()

	port := parsePort(t, srv.URL)
	handled, err := attemptProxyRegistryRefresh(context.Background(), srv.Client(), port, "tok")
	if err == nil {
		t.Fatal("err = nil, want non-nil error for 500")
	}
	if handled {
		t.Fatal("handled = true, want false for 500")
	}
	if !strings.Contains(err.Error(), "registry source failed") {
		t.Errorf("err = %q, want it to contain response body text", err.Error())
	}
}

func TestAttemptProxyRegistryRefresh_ConnectionRefused_FallsBack(t *testing.T) {
	// Bind and immediately close to reserve a port guaranteed not to be listening.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := parsePort(t, srv.URL)
	srv.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	handled, err := attemptProxyRegistryRefresh(context.Background(), client, port, "tok")
	if err != nil {
		t.Fatalf("attemptProxyRegistryRefresh: %v (connection refused should fall back, not error)", err)
	}
	if handled {
		t.Fatal("handled = true, want false when proxy unreachable")
	}
}

// TestAttemptProxyRegistryRefresh_UsesCallerContext proves that
// attemptProxyRegistryRefresh honours the context supplied by the caller.
// A pre-cancelled context must cause the HTTP request to fail immediately
// rather than completing successfully, because the function must pass the
// context to http.NewRequestWithContext instead of using context.Background().
func TestAttemptProxyRegistryRefresh_UsesCallerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If the request arrives here the context was not honoured.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call so any request built with this ctx fails immediately

	port := parsePort(t, srv.URL)
	handled, err := attemptProxyRegistryRefresh(ctx, srv.Client(), port, "tok")
	// A cancelled context causes the transport to return an error.
	// The function must treat transport errors as "fall back" (handled=false, err=nil),
	// but only if the error is a network error — a context cancellation error must
	// propagate so the caller knows the operation was aborted.
	// Either way, handled must be false because the request did not succeed.
	if handled {
		t.Fatal("handled = true; want false when context is already cancelled")
	}
	// The important assertion: if err is nil it means the server responded 2xx,
	// which would only happen if context.Background() was used instead of ctx.
	// With a properly-threaded cancelled context the transport returns an error
	// wrapping context.Canceled.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; expected an error wrapping context.Canceled because the caller context "+
			"was already cancelled — the function must pass ctx to http.NewRequestWithContext, not context.Background()", err)
	}
}

func TestNewRegistryPipelineWiresAnthropicCodexOverlayAndPublisher(t *testing.T) {
	fsys := fsutil.NewMemFS()
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            "/home/test",
		ClaudeUpstream:     "https://claude.example",
		CodexUpstream:      "https://codex.example",
		HTTPClient:         http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken: func() (string, error) {
			return "claude-token", nil
		},
		CodexToken: func() (string, error) {
			return "codex-token", nil
		},
		Env:    func(string) string { return "" },
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("newRegistryPipeline() error = %v", err)
	}
	if pipeline.Catalog == nil {
		t.Fatal("Catalog is nil")
	}
	if pipeline.Refresher == nil {
		t.Fatal("Refresher is nil")
	}
	if pipeline.Refresher.Anthropic == nil {
		t.Fatal("Anthropic source is nil")
	}
	if pipeline.Refresher.Codex == nil {
		t.Fatal("Codex source is nil")
	}
	if pipeline.Refresher.Overlays == nil {
		t.Fatal("Overlay store is nil")
	}

	pipeline.Catalog.Replace(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderAnthropic, ID: "claude-sonnet-4-5", ContextWindow: 200000, MaxOutputTokens: 32000, Source: modelregistry.SourceNative},
		{Provider: modelregistry.ProviderAnthropic, ID: "claude-overlay", DisplayName: "Claude Overlay", Source: modelregistry.SourceOverlay},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", DisplayName: "GPT-5.5", ContextWindow: 1050000, Source: modelregistry.SourceNative},
	}})
	pipeline.Publish()

	claudeJSON, err := fsys.ReadFile("/home/test/.claude.json")
	if err != nil {
		t.Fatalf("Claude Code options were not published: %v", err)
	}
	var cfg struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(claudeJSON, &cfg); err != nil {
		t.Fatalf("unmarshal Claude Code options: %v", err)
	}
	values := make(map[string]bool)
	for _, opt := range cfg.Options {
		values[opt.Value] = true
	}
	for _, want := range []string{"gpt-5.5", "gpt-5.5[1m]", "claude-overlay"} {
		if !values[want] {
			t.Fatalf("additionalModelOptionsCache missing %q: %+v", want, cfg.Options)
		}
	}
	if values["claude-sonnet-4-5"] {
		t.Fatalf("additionalModelOptionsCache included native Anthropic model: %+v", cfg.Options)
	}
	if _, err := fsys.ReadFile("/home/test/.claude/cache/model-capabilities.json"); err != nil {
		t.Fatalf("Claude capabilities were not published: %v", err)
	}
	if _, err := fsys.ReadFile("/home/test/.codex/models_cache.json"); err != nil {
		t.Fatalf("Codex cache was not published: %v", err)
	}
}

func TestNewRegistryPipelineSeedsCatalogFromExistingCaches(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = fsys.WriteFile("/home/test/.claude/cache/model-capabilities.json", []byte(`{
  "timestamp": 1700000000,
  "models": [{"id":"claude-sonnet-4-5","max_input_tokens":200000,"max_tokens":32000}]
}`), 0o600)
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json", []byte(`{
  "client_version":"0.124.0",
  "models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","context_window":272000}]
}`), 0o600)
	_ = fsys.WriteFile("/home/test/.config/cq/models.json", []byte(`{
  "version": 1,
  "models": [{"provider":"codex","id":"gpt-5.5","source":"overlay"}]
}`), 0o600)

	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            "/home/test",
		ClaudeUpstream:     "https://claude.example",
		CodexUpstream:      "https://codex.example",
		HTTPClient:         http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken:        func() (string, error) { return "claude-token", nil },
		CodexToken:         func() (string, error) { return "codex-token", nil },
		Env:                func(string) string { return "" },
		Stderr:             io.Discard,
	})
	if err != nil {
		t.Fatalf("newRegistryPipeline() error = %v", err)
	}

	seen := map[string]modelregistry.Provider{}
	for _, entry := range pipeline.Catalog.Snapshot().Entries {
		seen[entry.ID] = entry.Provider
	}
	if seen["claude-sonnet-4-5"] != modelregistry.ProviderAnthropic {
		t.Fatalf("seeded entries = %+v, want cached Claude entry", pipeline.Catalog.Snapshot().Entries)
	}
	if seen["gpt-5.4"] != modelregistry.ProviderCodex {
		t.Fatalf("seeded entries = %+v, want cached Codex entry", pipeline.Catalog.Snapshot().Entries)
	}
	if seen["gpt-5.5"] != modelregistry.ProviderCodex {
		t.Fatalf("seeded entries = %+v, want overlay Codex entry", pipeline.Catalog.Snapshot().Entries)
	}
}

func TestRegistryPipelinePublishPreservesClaudeCapabilitiesWhenSnapshotHasNoAnthropicEntries(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"
	existing := `{
  "timestamp": 1700000000,
  "models": [{"id":"claude-sonnet-4-5","max_input_tokens":200000,"max_tokens":32000}]
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            "/home/test",
		ClaudeUpstream:     "https://claude.example",
		CodexUpstream:      "https://codex.example",
		HTTPClient:         http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken:        func() (string, error) { return "claude-token", nil },
		CodexToken:         func() (string, error) { return "codex-token", nil },
		Env:                func(string) string { return "" },
		Stderr:             io.Discard,
	})
	if err != nil {
		t.Fatalf("newRegistryPipeline() error = %v", err)
	}
	pipeline.Catalog.Replace(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceNative},
	}})

	pipeline.Publish()

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != existing {
		t.Fatalf("capabilities cache was overwritten with %s", data)
	}
}

func TestWriteRegistrySourceDiagnosticsReportsPartialFailures(t *testing.T) {
	var stderr strings.Builder
	diag := modelregistry.RefreshDiagnostics{SourceErrors: map[modelregistry.Provider]error{
		modelregistry.ProviderAnthropic: errors.New("HTTP 401: unauthorized"),
	}}

	writeRegistrySourceDiagnostics(&stderr, diag)

	if got := stderr.String(); !strings.Contains(got, "cq: registry: anthropic source: HTTP 401: unauthorized") {
		t.Fatalf("diagnostics = %q, want Anthropic source error", got)
	}
}

func TestWriteRegistrySourceDiagnosticsReportsMalformedCounts(t *testing.T) {
	var stderr strings.Builder
	diag := modelregistry.RefreshDiagnostics{
		MalformedCounts: map[modelregistry.Provider]int{
			modelregistry.ProviderCodex: 3,
		},
	}

	writeRegistrySourceDiagnostics(&stderr, diag)

	got := stderr.String()
	if !strings.Contains(got, "cq: registry: codex source: skipped 3 malformed model entries") {
		t.Fatalf("diagnostics = %q, want codex malformed count", got)
	}
}

func TestWriteRegistrySourceDiagnosticsOrdersErrorsBeforeMalformed(t *testing.T) {
	var stderr strings.Builder
	diag := modelregistry.RefreshDiagnostics{
		SourceErrors: map[modelregistry.Provider]error{
			modelregistry.ProviderAnthropic: errors.New("connection refused"),
		},
		MalformedCounts: map[modelregistry.Provider]int{
			modelregistry.ProviderCodex: 1,
		},
	}

	writeRegistrySourceDiagnostics(&stderr, diag)

	got := stderr.String()
	errIdx := strings.Index(got, "anthropic source: connection refused")
	malIdx := strings.Index(got, "codex source: skipped 1 malformed")
	if errIdx == -1 {
		t.Fatalf("missing source error line: %q", got)
	}
	if malIdx == -1 {
		t.Fatalf("missing malformed count line: %q", got)
	}
	if errIdx > malIdx {
		t.Errorf("source errors should appear before malformed counts; got:\n%s", got)
	}
}

func TestPublishers_ClaudeOutputsAreUsableWhenSnapshotHasAnthropicAndCodexEntries(t *testing.T) {
	fsys := fsutil.NewMemFS()
	home := "/home/test"
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{
				Provider:        modelregistry.ProviderAnthropic,
				ID:              "claude-sonnet-4-5",
				ContextWindow:   200000,
				MaxOutputTokens: 32000,
				Source:          modelregistry.SourceNative,
			},
			{
				Provider:      modelregistry.ProviderCodex,
				ID:            "gpt-5.5",
				DisplayName:   "GPT-5.5",
				Description:   "Frontier coding model",
				ContextWindow: 272000,
				Source:        modelregistry.SourceNative,
			},
		},
	}

	if err := modelregistry.PublishClaudeCodeOptions(fsys, home+"/.claude.json", snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}
	if err := modelregistry.PublishClaudeCapabilities(fsys, home+"/.claude/cache/model-capabilities.json", snap, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("PublishClaudeCapabilities() error = %v", err)
	}

	claudeJSON, err := fsys.ReadFile(home + "/.claude.json")
	if err != nil {
		t.Fatalf("ReadFile(.claude.json) error = %v", err)
	}
	var cfg struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(claudeJSON, &cfg); err != nil {
		t.Fatalf("unmarshal .claude.json: %v", err)
	}
	if len(cfg.Options) == 0 || cfg.Options[0].Value != "gpt-5.5" {
		t.Fatalf("additionalModelOptionsCache = %+v, want gpt-5.5 entry", cfg.Options)
	}

	capsJSON, err := fsys.ReadFile(home + "/.claude/cache/model-capabilities.json")
	if err != nil {
		t.Fatalf("ReadFile(model-capabilities.json) error = %v", err)
	}
	var caps struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(capsJSON, &caps); err != nil {
		t.Fatalf("unmarshal model-capabilities.json: %v", err)
	}
	if len(caps.Models) == 0 || caps.Models[0].ID != "claude-sonnet-4-5" {
		t.Fatalf("capabilities models = %+v, want Anthropic entry", caps.Models)
	}
}
