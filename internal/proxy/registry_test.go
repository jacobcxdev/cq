package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// ── RouteModel registry exact-match tests ────────────────────────────────────

// TestRouteModel_RegistryExactMatchOverridesPrefixHeuristic verifies that
// when a Catalog is injected into RouteModelWithCatalog, an exact registry
// match takes precedence over the prefix-based heuristic.
func TestRouteModel_RegistryExactMatchOverridesPrefixHeuristic(t *testing.T) {
	// "my-custom-claude-model" would normally route to Claude via the fallback,
	// but the registry says it's a Codex model.
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderCodex, ID: "my-custom-claude-model", Source: modelregistry.SourceOverlay},
		},
	}
	cat := modelregistry.NewCatalog(snap)

	got := RouteModelWithCatalog("my-custom-claude-model", cat)
	if got != ProviderCodex {
		t.Errorf("RouteModelWithCatalog(%q) = %v, want ProviderCodex", "my-custom-claude-model", got)
	}
}

// TestRouteModel_RegistryExactMatch_AnthropicEntry verifies that a registry
// entry for Anthropic routes to Claude even for a GPT-prefixed model name.
func TestRouteModel_RegistryExactMatch_AnthropicEntry(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderAnthropic, ID: "gpt-like-but-claude", Source: modelregistry.SourceOverlay},
		},
	}
	cat := modelregistry.NewCatalog(snap)

	got := RouteModelWithCatalog("gpt-like-but-claude", cat)
	if got != ProviderClaude {
		t.Errorf("RouteModelWithCatalog(%q) = %v, want ProviderClaude", "gpt-like-but-claude", got)
	}
}

func TestRouteRequestWithCatalog_ExactMatchAnthropicOverridesGPTPrefix(t *testing.T) {
	cat := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderAnthropic, ID: "gpt-like-but-claude", Source: modelregistry.SourceOverlay},
	}})

	got := RouteRequestWithCatalog(http.MethodPost, "/v1/messages", "gpt-like-but-claude", cat)
	if got != ProviderClaude {
		t.Fatalf("RouteRequestWithCatalog() = %v, want ProviderClaude", got)
	}
}

// TestRouteModel_RegistryMiss_FallsBackToPrefixHeuristic verifies that when
// the registry has no matching entry, the prefix heuristic still applies.
func TestRouteModel_RegistryMiss_FallsBackToPrefixHeuristic(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderCodex, ID: "other-model", Source: modelregistry.SourceNative},
		},
	}
	cat := modelregistry.NewCatalog(snap)

	// gpt-5.5 is not in registry but prefix heuristic matches gpt- -> Codex
	got := RouteModelWithCatalog("gpt-5.5", cat)
	if got != ProviderCodex {
		t.Errorf("RouteModelWithCatalog(%q) = %v, want ProviderCodex", "gpt-5.5", got)
	}

	// unknown model not in registry -> Claude (default fallback)
	got = RouteModelWithCatalog("some-unknown-model", cat)
	if got != ProviderClaude {
		t.Errorf("RouteModelWithCatalog(%q) = %v, want ProviderClaude", "some-unknown-model", got)
	}
}

// TestRouteModel_NilCatalog_FallsBackToPrefixHeuristic verifies that a nil
// catalog behaves identically to no registry.
func TestRouteModel_NilCatalog_FallsBackToPrefixHeuristic(t *testing.T) {
	got := RouteModelWithCatalog("gpt-5.4", nil)
	if got != ProviderCodex {
		t.Errorf("RouteModelWithCatalog(gpt-5.4, nil) = %v, want ProviderCodex", got)
	}

	got = RouteModelWithCatalog("claude-opus", nil)
	if got != ProviderClaude {
		t.Errorf("RouteModelWithCatalog(claude-opus, nil) = %v, want ProviderClaude", got)
	}
}

// ── ModelMaxInputTokens registry-backed tests ─────────────────────────────────

// TestModelMaxInputTokens_RegistryOverlayEntry verifies that
// ModelMaxInputTokensWithCatalog returns the registry ContextWindow for an
// overlay entry that clones from gpt-5.4.
func TestModelMaxInputTokens_RegistryOverlayEntry(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{
				Provider:      modelregistry.ProviderCodex,
				ID:            "gpt-5.5",
				Source:        modelregistry.SourceOverlay,
				CloneFrom:     "gpt-5.4",
				ContextWindow: 1050000,
			},
		},
	}
	cat := modelregistry.NewCatalog(snap)

	got := ModelMaxInputTokensWithCatalog("gpt-5.5", cat)
	if got != 1050000 {
		t.Errorf("ModelMaxInputTokensWithCatalog(gpt-5.5) = %d, want 1050000", got)
	}
}

// TestModelMaxInputTokens_RegistryMiss_FallsBackToSynthetic verifies that
// when the registry has no matching entry, the synthetic catalog is used.
func TestModelMaxInputTokens_RegistryMiss_FallsBackToSynthetic(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderCodex, ID: "other-model", Source: modelregistry.SourceNative, ContextWindow: 50000},
		},
	}
	cat := modelregistry.NewCatalog(snap)

	// gpt-5.4 is in synthetic catalog; not in registry here
	got := ModelMaxInputTokensWithCatalog("gpt-5.4", cat)
	if got != 1050000 {
		t.Errorf("ModelMaxInputTokensWithCatalog(gpt-5.4) = %d, want 1050000 (from synthetic)", got)
	}
}

// TestModelMaxInputTokens_NilCatalog_FallsBackToSynthetic verifies nil catalog
// behaves like synthetic-only lookup.
func TestModelMaxInputTokens_NilCatalog_FallsBackToSynthetic(t *testing.T) {
	got := ModelMaxInputTokensWithCatalog("gpt-5.4", nil)
	if got != 1050000 {
		t.Errorf("ModelMaxInputTokensWithCatalog(gpt-5.4, nil) = %d, want 1050000", got)
	}
}

func TestServer_CodexHandlerUsesRegistryRouting(t *testing.T) {
	cat := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "custom-frontier-model", Source: modelregistry.SourceOverlay},
	}})
	called := false
	srv := &Server{
		Config:  &Config{CodexUpstream: "https://codex.example", LocalToken: "tok"},
		Catalog: cat,
		CodexTransport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return makeResponse(http.StatusOK, "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\ndata: [DONE]\n\n"), nil
		}),
	}

	body := []byte(`{"model":"custom-frontier-model","max_tokens":10,"messages":[{"role":"user","content":"ping"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	srv.handleCodex(w, req, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("CodexTransport was not called")
	}
}

// ── /v1/models endpoint with registry Catalog ─────────────────────────────────

// TestServer_ModelsEndpoint_RegistryCatalog verifies that when a Catalog is
// injected into Server, /v1/models returns registry-backed models in addition
// to synthetic ones.
func TestServer_ModelsEndpoint_RegistryCatalog(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{
				Provider:        modelregistry.ProviderCodex,
				ID:              "gpt-5.5",
				Source:          modelregistry.SourceOverlay,
				ContextWindow:   1200000,
				MaxOutputTokens: 64000,
			},
		},
		FetchedAt: time.Now(),
	}
	cat := modelregistry.NewCatalog(snap)

	srv := &Server{
		Config:  &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Catalog: cat,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	found := false
	for _, m := range resp.Data {
		if m.ID == "gpt-5.5" {
			found = true
			if m.MaxInputTokens != 1200000 {
				t.Errorf("gpt-5.5 max_input_tokens = %d, want 1200000", m.MaxInputTokens)
			}
		}
	}
	if !found {
		t.Fatalf("registry model gpt-5.5 missing from /v1/models response: %s", w.Body.String())
	}
}

// ── GET /models?client_version=... endpoint ───────────────────────────────────

// TestServer_NativeCodexModelsEndpoint_ReturnsCodexShape verifies that
// GET /models returns the Codex ModelsResponse shape.
func TestServer_NativeCodexModelsEndpoint_ReturnsCodexShape(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{
				Provider:      modelregistry.ProviderCodex,
				ID:            "gpt-5.5",
				Source:        modelregistry.SourceNative,
				ContextWindow: 300000,
				DisplayName:   "GPT 5.5",
			},
		},
		FetchedAt: time.Now(),
	}
	cat := modelregistry.NewCatalog(snap)

	srv := &Server{
		Config:  &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Catalog: cat,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models?client_version=1.2.3", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if len(resp.Models) == 0 {
		t.Fatalf("expected at least one model, got 0; body: %s", w.Body.String())
	}

	// Each model must have a slug field (Codex shape).
	for _, raw := range resp.Models {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("model is not valid JSON: %v", err)
		}
		if _, ok := m["slug"]; !ok {
			t.Errorf("model missing 'slug' field (not Codex shape): %s", raw)
		}
	}
}

// TestServer_NativeCodexModelsEndpoint_NilCatalog_Returns503 verifies that
// GET /models returns 503 when no Catalog is configured.
func TestServer_NativeCodexModelsEndpoint_NilCatalog_Returns503(t *testing.T) {
	srv := &Server{
		Config:  &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Catalog: nil,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// ── /v1/registry endpoint ────────────────────────────────────────────────────

// TestServer_RegistryEndpoint_RequiresToken verifies that /v1/registry
// returns 403 without a valid proxy token.
func TestServer_RegistryEndpoint_RequiresToken(t *testing.T) {
	snap := modelregistry.Snapshot{Entries: []modelregistry.Entry{}}
	cat := modelregistry.NewCatalog(snap)

	srv := &Server{
		Config:  &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Catalog: cat,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/registry", nil)
	// No Authorization header.
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestServer_RegistryEndpoint_ReturnsSnapshot verifies that /v1/registry
// returns the current registry snapshot as JSON.
func TestServer_RegistryEndpoint_ReturnsSnapshot(t *testing.T) {
	snap := modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceNative},
		},
		FetchedAt: time.Now(),
	}
	cat := modelregistry.NewCatalog(snap)

	srv := &Server{
		Config:  &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Catalog: cat,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/registry", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "gpt-5.5") {
		t.Fatalf("body missing gpt-5.5: %s", body)
	}
}

// ── /v1/registry/refresh endpoint ────────────────────────────────────────────

// TestServer_RegistryRefreshEndpoint_RequiresToken verifies that
// /v1/registry/refresh returns 403 without a valid proxy token.
func TestServer_RegistryRefreshEndpoint_RequiresToken(t *testing.T) {
	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestServer_RegistryRefreshEndpoint_NilRefresher_Returns503 verifies that
// /v1/registry/refresh returns 503 when no Refresher is configured.
func TestServer_RegistryRefreshEndpoint_NilRefresher_Returns503(t *testing.T) {
	srv := &Server{
		Config:    &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Refresher: nil,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestServer_RegistryRefreshEndpoint_CallsRefresher verifies that
// /v1/registry/refresh calls the injected Refresher function and returns 200.
func TestServer_RegistryRefreshEndpoint_CallsRefresher(t *testing.T) {
	called := false
	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Refresher: RegistryRefresherFunc(func(context.Context) (modelregistry.RefreshDiagnostics, error) {
			called = true
			return modelregistry.RefreshDiagnostics{}, nil
		}),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("Refresher was not called")
	}
}

// TestServer_RegistryRefreshEndpoint_RefresherError_Returns500 verifies that
// when the Refresher returns an error, the endpoint returns 500.
func TestServer_RegistryRefreshEndpoint_RefresherError_Returns500(t *testing.T) {
	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"},
		Refresher: RegistryRefresherFunc(func(context.Context) (modelregistry.RefreshDiagnostics, error) {
			return modelregistry.RefreshDiagnostics{}, &testRefreshError{"refresh failed"}
		}),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestServer_RegistryRefreshEndpoint_ReturnsDiagnostics verifies that a
// successful POST /v1/registry/refresh returns 200 with ok=true and
// provider counts from the RefreshDiagnostics.
func TestServer_RegistryRefreshEndpoint_ReturnsDiagnostics(t *testing.T) {
	srv := &Server{
		Config: &Config{LocalToken: "tok"},
		Refresher: RegistryRefresherFunc(func(context.Context) (modelregistry.RefreshDiagnostics, error) {
			return modelregistry.RefreshDiagnostics{Counts: map[string]int{"codex": 7}}, nil
		}),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		OK     bool           `json:"ok"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false, want true")
	}
	if resp.Counts["codex"] != 7 {
		t.Errorf("counts.codex = %d, want 7", resp.Counts["codex"])
	}
}

// TestServer_RegistryRefreshEndpoint_ReturnsMalformedCounts verifies that when
// the Refresher reports malformed entries, the response includes the malformed field.
func TestServer_RegistryRefreshEndpoint_ReturnsMalformedCounts(t *testing.T) {
	srv := &Server{
		Config: &Config{LocalToken: "tok"},
		Refresher: RegistryRefresherFunc(func(context.Context) (modelregistry.RefreshDiagnostics, error) {
			return modelregistry.RefreshDiagnostics{
				Counts:          map[string]int{"codex": 5},
				MalformedCounts: map[modelregistry.Provider]int{modelregistry.ProviderCodex: 2},
			}, nil
		}),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		OK        bool           `json:"ok"`
		Counts    map[string]int `json:"counts"`
		Malformed map[string]int `json:"malformed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false, want true")
	}
	if resp.Malformed["codex"] != 2 {
		t.Errorf("malformed.codex = %d, want 2", resp.Malformed["codex"])
	}
}

// TestServer_RegistryRefreshEndpoint_OmitsMalformedWhenNone verifies that when
// no malformed entries exist, the malformed field is omitted from the response.
func TestServer_RegistryRefreshEndpoint_OmitsMalformedWhenNone(t *testing.T) {
	srv := &Server{
		Config: &Config{LocalToken: "tok"},
		Refresher: RegistryRefresherFunc(func(context.Context) (modelregistry.RefreshDiagnostics, error) {
			return modelregistry.RefreshDiagnostics{
				Counts:          map[string]int{"codex": 5},
				MalformedCounts: map[modelregistry.Provider]int{},
			}, nil
		}),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/refresh", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := resp["malformed"]; ok {
		t.Errorf("malformed field present in response, want omitted when no malformed entries")
	}
}

type testRefreshError struct{ msg string }

func (e *testRefreshError) Error() string { return e.msg }
