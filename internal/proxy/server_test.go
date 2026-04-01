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
