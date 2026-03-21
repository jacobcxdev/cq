package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// urlRewriter implements httputil.Doer for tests by rewriting request URLs to
// a local httptest.Server while preserving path and headers.
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

// writeCredsFile writes a minimal oauth_creds.json under homeDir/.gemini/.
func writeCredsFile(t *testing.T, homeDir string, creds map[string]any) {
	t.Helper()
	dir := filepath.Join(homeDir, ".gemini")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .gemini: %v", err)
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oauth_creds.json"), data, 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
}

// withHome temporarily overrides $HOME so os.UserHomeDir returns homeDir.
func withHome(t *testing.T, homeDir string) {
	t.Helper()
	t.Setenv("HOME", homeDir)
}

// quotaResponse builds the JSON body used by the quota endpoint.
func quotaResponse(buckets []map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"buckets": buckets})
	return b
}

// --- Fetch: not configured (no creds file) ---

func TestFetchNotConfigured(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)
	// No .gemini directory → ReadFile fails → not_configured result.

	p := New(&urlRewriter{client: http.DefaultClient, baseURL: "http://localhost"})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != quota.StatusError {
		t.Errorf("status = %q, want error", r.Status)
	}
	if r.Error == nil || r.Error.Code != "not_configured" {
		t.Errorf("error code = %v, want not_configured", r.Error)
	}
}

// --- Fetch: expired token returns auth_expired ---

func TestFetchExpiredTokenReturnsAuthExpired(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)

	// Write creds with an expired token (expiry_date in the past).
	pastMs := float64(time.Now().Add(-1 * time.Hour).UnixMilli())
	writeCredsFile(t, tmpHome, map[string]any{
		"access_token":  "old-token",
		"expiry_date":   pastMs,
		"refresh_token": "refresh-tok",
		"id_token":      "",
	})

	// No server needed — Fetch should return auth_expired before any HTTP call.
	p := New(&urlRewriter{client: http.DefaultClient, baseURL: "http://localhost"})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != quota.StatusError {
		t.Errorf("status = %q, want error", r.Status)
	}
	if r.Error == nil || r.Error.Code != "auth_expired" {
		t.Errorf("error code = %v, want auth_expired", r.Error)
	}
}

// --- Fetch: happy path (200 quota response) ---

func TestFetchHappyPath(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)

	// Token is not expired (expiry_date far in the future).
	futureMs := float64(time.Now().Add(24 * time.Hour).UnixMilli())
	writeCredsFile(t, tmpHome, map[string]any{
		"access_token":  "valid-token",
		"expiry_date":   futureMs,
		"refresh_token": "unused-refresh",
		"id_token":      "",
	})

	tierResp := `{"currentTier": {"id": "standard-tier"}}`
	qBody := quotaResponse([]map[string]any{
		{"modelId": "gemini-2.0-pro", "remainingFraction": 0.75, "resetTime": "2026-03-22T00:00:00Z"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the token is forwarded on both data requests.
		if auth := r.Header.Get("Authorization"); auth != "Bearer valid-token" {
			t.Errorf("Authorization = %q, want Bearer valid-token", auth)
		}
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.Write([]byte(tierResp))
		case "/v1internal:retrieveUserQuota":
			w.WriteHeader(http.StatusOK)
			w.Write(qBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := New(&urlRewriter{client: srv.Client(), baseURL: srv.URL})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != quota.StatusOK {
		t.Errorf("status = %q, want ok", r.Status)
	}
	if r.Tier != "paid" {
		t.Errorf("tier = %q, want paid", r.Tier)
	}
	w, ok := r.Windows[quota.WindowQuota]
	if !ok {
		t.Fatal("missing quota window")
	}
	if w.RemainingPct != 75 {
		t.Errorf("remaining_pct = %d, want 75", w.RemainingPct)
	}
}

// --- Fetch: non-200 quota response ---

func TestFetchNon200QuotaResponse(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)

	futureMs := float64(time.Now().Add(24 * time.Hour).UnixMilli())
	writeCredsFile(t, tmpHome, map[string]any{
		"access_token":  "valid-token",
		"expiry_date":   futureMs,
		"refresh_token": "",
		"id_token":      "",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.Write([]byte(`{}`))
		case "/v1internal:retrieveUserQuota":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := New(&urlRewriter{client: srv.Client(), baseURL: srv.URL})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != quota.StatusError {
		t.Errorf("status = %q, want error", r.Status)
	}
	if r.Error == nil || r.Error.Code != "api_error" {
		t.Errorf("error code = %v, want api_error", r.Error)
	}
	if r.Error.HTTPStatus != http.StatusForbidden {
		t.Errorf("http_status = %d, want %d", r.Error.HTTPStatus, http.StatusForbidden)
	}
}

// --- Fetch: missing access_token in creds ---

func TestFetchNoToken(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)

	writeCredsFile(t, tmpHome, map[string]any{
		"access_token":  "",
		"expiry_date":   0,
		"refresh_token": "",
		"id_token":      "",
	})

	p := New(&urlRewriter{client: http.DefaultClient, baseURL: "http://localhost"})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Error == nil || results[0].Error.Code != "no_token" {
		t.Errorf("error code = %v, want no_token", results[0].Error)
	}
}

// --- Fetch: malformed JSON creds file ---

func TestFetchMalformedCreds(t *testing.T) {
	tmpHome := t.TempDir()
	withHome(t, tmpHome)

	dir := filepath.Join(tmpHome, ".gemini")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "oauth_creds.json"), []byte("not json"), 0o600)

	p := New(&urlRewriter{client: http.DefaultClient, baseURL: "http://localhost"})
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Error == nil || results[0].Error.Code != "parse_error" {
		t.Errorf("error code = %v, want parse_error", results[0].Error)
	}
}

