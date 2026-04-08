package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	claude "github.com/jacobcxdev/cq/internal/provider/claude"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestServer_HealthEndpoint(t *testing.T) {
	srv := &Server{
		Config: &Config{
			Port:           0,
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "test-token",
		},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "a@test.com"}, {Email: "b@test.com"}}
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	srv.handleHealth(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	accounts := resp["accounts"].(map[string]any)
	if accounts["claude"].(float64) != 2 {
		t.Errorf("claude accounts = %v, want 2", accounts["claude"])
	}
}

func TestServer_InvalidToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request should not reach upstream")
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "correct-token",
		},
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	handler(w, req)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "error" {
		t.Errorf("response type = %v, want error", resp["type"])
	}
}

func TestServer_ValidTokenForwardsRequest(t *testing.T) {
	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner:    http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local-tok",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if gotAuth != "Bearer real-token" {
		t.Errorf("upstream auth = %q, want Bearer real-token", gotAuth)
	}
	if gotBody != `{"model":"claude"}` {
		t.Errorf("upstream body = %q, want original", gotBody)
	}
}

func TestServer_SSEStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("no flusher")
		}
		for _, chunk := range []string{"data: hello\n\n", "data: world\n\n"} {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "tok", ExpiresAt: future},
	}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "tok",
		},
		Transport: &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "data: hello") || !strings.Contains(body, "data: world") {
		t.Errorf("SSE chunks not received: %q", body)
	}
}

func TestServer_NetworkError(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}),
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "http://localhost:1",
			LocalToken:     "tok",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL("http://localhost:1"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler(w, req)

	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "error" {
		t.Errorf("response type = %v, want error", resp["type"])
	}
}

func TestServer_HeadroomPreservesOriginalModelRouting(t *testing.T) {
	claudeUpstreamCalled := false
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		claudeUpstreamCalled = true
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer claudeUpstream.Close()

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer codexUpstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: claudeUpstream.URL,
			CodexUpstream:  codexUpstream.URL,
			LocalToken:     "tok",
		},
		Headroom: fakeBridge(t, func(req headroomRequest) headroomResponse {
			if req.Model != "gpt-5.4" {
				t.Fatalf("bridge model = %q, want gpt-5.4", req.Model)
			}
			return headroomResponse{
				Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
				TokensSaved: 123,
			}
		}),
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler := srv.proxyHandler(mustParseURL(claudeUpstream.URL))
	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if claudeUpstreamCalled {
		t.Fatal("claude upstream should not be called for compressed codex model")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestServer_BodyReplay(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		auth := r.Header.Get("Authorization")
		if auth == "Bearer old-tok" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "old-tok", ExpiresAt: future, RefreshToken: "rt"},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			return &claude.RefreshResult{AccessToken: "new-tok", ExpiresIn: 3600}, nil
		},
		Persister:   func(_ *keyring.ClaudeOAuth) {},
		RefreshHTTP: http.DefaultClient,
		Inner:       http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer local")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(bodies))
	}
	if bodies[0] != `{"model":"test"}` || bodies[1] != `{"model":"test"}` {
		t.Errorf("body not replayed correctly: %v", bodies)
	}
}

// ── isValidToken (dual-mode auth) ────────────────────────────────────────────

func TestServer_IsValidToken_LocalToken(t *testing.T) {
	srv := &Server{Config: &Config{LocalToken: "local-tok"}}
	if !srv.isValidToken("local-tok") {
		t.Error("local token should be valid")
	}
	if srv.isValidToken("wrong") {
		t.Error("wrong token should be invalid")
	}
}

func TestServer_IsValidToken_KnownOAuthToken(t *testing.T) {
	srv := &Server{
		Config: &Config{LocalToken: "local-tok"},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "a@test.com", AccessToken: "oauth-tok-a"},
				{Email: "b@test.com", AccessToken: "oauth-tok-b"},
			}
		},
	}
	if !srv.isValidToken("oauth-tok-a") {
		t.Error("known OAuth token A should be valid")
	}
	if !srv.isValidToken("oauth-tok-b") {
		t.Error("known OAuth token B should be valid")
	}
	if srv.isValidToken("unknown-tok") {
		t.Error("unknown token should be invalid")
	}
}

func TestServer_IsValidToken_NilDiscover(t *testing.T) {
	srv := &Server{Config: &Config{LocalToken: "local-tok"}}
	// Without Discover, only LocalToken works.
	if srv.isValidToken("some-oauth-tok") {
		t.Error("should reject OAuth token when Discover is nil")
	}
}

func TestServer_OAuthTokenForwardsRequest(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner:    http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local-tok",
		},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "user@test.com", AccessToken: "user-oauth-token"},
			}
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	// Authenticate with the user's own OAuth token — NOT the local proxy token.
	req.Header.Set("Authorization", "Bearer user-oauth-token")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// TokenTransport should have replaced the OAuth token with the selected account's token.
	if gotAuth != "Bearer real-token" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer real-token")
	}
}

// ── handleNativeCodex tests ─────────────────────────────────────────────────

func TestServer_NativeCodex_ForwardsWithAuth(t *testing.T) {
	var gotAuth, gotAcctID, gotBody, gotContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcctID = r.Header.Get("ChatGPT-Account-ID")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"resp_123","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-1",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","input":"hello","stream":false}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer codex-tok" {
		t.Errorf("upstream auth = %q, want Bearer codex-tok", gotAuth)
	}
	if gotAcctID != "acct-1" {
		t.Errorf("upstream account-id = %q, want acct-1", gotAcctID)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream content-type = %q, want application/json", gotContentType)
	}
	if gotBody != body {
		t.Errorf("upstream body = %q, want %q (no translation)", gotBody, body)
	}
	if !strings.Contains(w.Body.String(), "resp_123") {
		t.Errorf("response body should contain resp_123: %s", w.Body.String())
	}
}

func TestServer_Handler_CodexResponsesPath_ForwardsWithAuth(t *testing.T) {
	var gotPath, gotAuth, gotAcctID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAcctID = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_123"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct-1"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, codexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer codex-tok" {
		t.Errorf("upstream auth = %q, want Bearer codex-tok", gotAuth)
	}
	if gotAcctID != "acct-1" {
		t.Errorf("upstream account-id = %q, want acct-1", gotAcctID)
	}
}

func TestServer_Handler_LegacyCodexResponsesPost_Compatibility(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_legacy"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, legacyCodexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
}

func TestServer_Handler_CodexResponsesRejectsWebsocket(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexResponsesPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), codexAppServerPath) {
		t.Errorf("body = %q, want mention of %s", w.Body.String(), codexAppServerPath)
	}
}

func TestServer_Handler_AppServerRequiresWebsocket(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexAppServerPath, nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", w.Code)
	}
	if got := w.Header().Get("Upgrade"); got != "websocket" {
		t.Errorf("Upgrade header = %q, want websocket", got)
	}
}

func TestServer_ModelsEndpointRequiresAuth(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestServer_ModelsEndpointIncludesSyntheticModels(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
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
			MaxTokens      int    `json:"max_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	found := false
	for _, model := range resp.Data {
		if model.ID == "gpt-5.4" {
			found = true
			if model.MaxInputTokens != 1050000 {
				t.Fatalf("gpt-5.4 max_input_tokens = %d, want 1050000", model.MaxInputTokens)
			}
			if model.MaxTokens != 128000 {
				t.Fatalf("gpt-5.4 max_tokens = %d, want 128000", model.MaxTokens)
			}
		}
	}
	if !found {
		t.Fatal("missing gpt-5.4 synthetic model")
	}
}

func TestServer_ModelsEndpointMergesUpstreamModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-6","max_input_tokens":200000,"max_tokens":32000}]}`))
	}))
	defer upstream.Close()

	srv := &Server{Config: &Config{ClaudeUpstream: upstream.URL, LocalToken: "tok"}}
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

	body := w.Body.String()
	if !strings.Contains(body, `"claude-opus-4-6"`) {
		t.Fatalf("body missing upstream Claude model: %s", body)
	}
	if !strings.Contains(body, `"gpt-5.4"`) {
		t.Fatalf("body missing synthetic Codex model: %s", body)
	}
}

func TestServer_NativeCodex_StreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			"data: {\"type\":\"response.created\"}\n\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n",
			"data: {\"type\":\"response.completed\"}\n\n",
		}
		for _, ev := range events {
			w.Write([]byte(ev))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-1",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","input":"hello","stream":true}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	result := w.Body.String()
	if !strings.Contains(result, "response.created") {
		t.Error("missing response.created event")
	}
	if !strings.Contains(result, "response.completed") {
		t.Error("missing response.completed event")
	}
}

func TestServer_NativeCodex_NoTransport(t *testing.T) {
	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
		CodexTransport: nil,
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	srv.handleNativeCodex(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_NativeCodex_NoProxyTokenRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_ok"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "secret-proxy-token",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	// Deliberately do NOT send Authorization header or proxy token.
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (no proxy token required for /responses)", w.Code)
	}
}
