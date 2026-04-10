package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientFetchUsage(t *testing.T) {
	want := `{"five_hour":{"utilization":30.0,"resets_at":"2026-03-20T12:00:00Z"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-tok" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-tok")
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q, want %q", got, "oauth-2025-04-20")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(want))
	}))
	defer srv.Close()

	c := &Client{http: srv.Client()}

	// We need a custom Doer that rewrites the URL to our test server.
	c.http = &urlRewriter{client: srv.Client(), baseURL: srv.URL}

	body, code, retryAfter, diagnostics, err := c.FetchUsage(context.Background(), "test-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0", retryAfter)
	}
	if string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
	if diagnostics != "" {
		t.Errorf("diagnostics = %q, want empty", diagnostics)
	}
}

func TestClientFetchUsageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := &Client{http: &urlRewriter{client: srv.Client(), baseURL: srv.URL}}

	_, code, retryAfter, diagnostics, _ := c.FetchUsage(context.Background(), "bad-tok")
	if code != 401 {
		t.Errorf("status = %d, want 401", code)
	}
	if retryAfter != 0 {
		t.Errorf("retryAfter = %v, want 0", retryAfter)
	}
	if diagnostics != "" {
		t.Errorf("diagnostics = %q, want empty", diagnostics)
	}
}

func TestClientFetchUsageRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "0")
		w.Header().Set("anthropic-ratelimit-requests-reset", "2026-04-08T21:15:00Z")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()

	c := &Client{http: &urlRewriter{client: srv.Client(), baseURL: srv.URL}}

	_, code, retryAfter, diagnostics, err := c.FetchUsage(context.Background(), "rate-limited-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", code, http.StatusTooManyRequests)
	}
	if retryAfter != 120*time.Second {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, 120*time.Second)
	}
	if diagnostics != "retry_after=2m0s; anthropic-ratelimit-requests-remaining=0; anthropic-ratelimit-requests-reset=2026-04-08T21:15:00Z" {
		t.Fatalf("diagnostics = %q", diagnostics)
	}
}

func TestClientFetchProfile(t *testing.T) {
	profileResp := `{
		"account": {"uuid": "abc-123", "email": "user@example.com"},
		"organization": {"rate_limit_tier": "default_claude_max_20x", "organization_type": "claude_max"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-tok" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-tok")
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q, want %q", got, "oauth-2025-04-20")
		}
		w.Write([]byte(profileResp))
	}))
	defer srv.Close()

	c := &Client{http: &urlRewriter{client: srv.Client(), baseURL: srv.URL}}

	p, err := c.FetchProfile(context.Background(), "test-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", p.Email, "user@example.com")
	}
	if p.AccountUUID != "abc-123" {
		t.Errorf("account_uuid = %q, want %q", p.AccountUUID, "abc-123")
	}
	if p.Plan != "max" {
		t.Errorf("plan = %q, want %q", p.Plan, "max")
	}
	if p.RateLimitTier != "default_claude_max_20x" {
		t.Errorf("rate_limit_tier = %q, want %q", p.RateLimitTier, "default_claude_max_20x")
	}
}

func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		json.Unmarshal(body, &reqBody)
		if reqBody["grant_type"] != "refresh_token" {
			t.Errorf("grant_type = %v, want refresh_token", reqBody["grant_type"])
		}
		if reqBody["client_id"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
			t.Errorf("client_id = %v", reqBody["client_id"])
		}

		w.Write([]byte(`{"access_token":"new-tok","refresh_token":"new-rt","expires_in":7200}`))
	}))
	defer srv.Close()

	rr, err := RefreshToken(
		context.Background(),
		&urlRewriter{client: srv.Client(), baseURL: srv.URL},
		"old-refresh-tok",
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rr.AccessToken != "new-tok" {
		t.Errorf("access_token = %q, want %q", rr.AccessToken, "new-tok")
	}
	if rr.RefreshToken != "new-rt" {
		t.Errorf("refresh_token = %q, want %q", rr.RefreshToken, "new-rt")
	}
	if rr.ExpiresIn != 7200 {
		t.Errorf("expires_in = %d, want 7200", rr.ExpiresIn)
	}
}

func TestRefreshTokenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"","expires_in":0}`))
	}))
	defer srv.Close()

	_, err := RefreshToken(
		context.Background(),
		&urlRewriter{client: srv.Client(), baseURL: srv.URL},
		"old-refresh-tok",
		[]string{"user:profile"},
	)
	if err == nil {
		t.Fatal("expected error for empty access token")
	}
}

// urlRewriter is a test helper that implements httputil.Doer by rewriting
// request URLs to point at a local httptest.Server, preserving the original
// path and headers.
type urlRewriter struct {
	client  *http.Client
	baseURL string
}

func (u *urlRewriter) Do(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = u.baseURL[len("http://"):]
	return u.client.Do(req)
}
