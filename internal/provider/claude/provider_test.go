package claude

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestDiscoverAccounts(t *testing.T) {
	oldDiscover := discoverClaudeAccounts
	defer func() { discoverClaudeAccounts = oldDiscover }()

	discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{
			AccountUUID:      "uuid-123",
			Email:            "user@example.com",
			SubscriptionType: "max",
			RateLimitTier:    "tier-1",
		}}
	}

	p := New(http.DefaultClient)
	got, err := p.DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].AccountID != "uuid-123" {
		t.Fatalf("got[0].AccountID = %q, want uuid-123", got[0].AccountID)
	}
	if got[0].Email != "user@example.com" {
		t.Fatalf("got[0].Email = %q, want user@example.com", got[0].Email)
	}
	if got[0].Label != "max" {
		t.Fatalf("got[0].Label = %q, want max", got[0].Label)
	}
	if got[0].RateLimitTier != "tier-1" {
		t.Fatalf("got[0].RateLimitTier = %q, want tier-1", got[0].RateLimitTier)
	}
}

func TestDiscoverAccountsMarksActiveAccount(t *testing.T) {
	oldDiscover := discoverClaudeAccounts
	defer func() { discoverClaudeAccounts = oldDiscover }()

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"email":"active@example.com"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{
			{AccountUUID: "uuid-1", Email: "active@example.com"},
			{AccountUUID: "uuid-2", Email: "other@example.com"},
		}
	}

	p := New(http.DefaultClient)
	got, err := p.DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if !got[0].Active {
		t.Fatalf("got[0].Active = %v, want true", got[0].Active)
	}
	if got[1].Active {
		t.Fatalf("got[1].Active = %v, want false", got[1].Active)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestNewProvider verifies that New creates a Provider with a non-nil client
// using the supplied HTTP doer.
func TestNewProvider(t *testing.T) {
	p := New(http.DefaultClient)
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.client == nil {
		t.Fatal("provider.client is nil")
	}
	if p.client.http == nil {
		t.Fatal("provider.client.http is nil")
	}
}

// panicDoer is an httputil.Doer that panics on every request, used to test
// that panic recovery in inner goroutines prevents process crashes.
type panicDoer struct{}

func (panicDoer) Do(*http.Request) (*http.Response, error) {
	panic("test panic in HTTP call")
}

func TestFetchAccountPanicInInnerGoroutines(t *testing.T) {
	p := &Provider{client: &Client{http: panicDoer{}}}
	acct := keyring.ClaudeOAuth{AccessToken: "test-token"}

	// Must not panic — the inner goroutine recovery should catch it.
	result := p.fetchAccount(context.Background(), acct, time.Now())

	if result.Status != quota.StatusError {
		t.Fatalf("expected error status, got %v", result.Status)
	}
	if result.Error == nil || result.Error.Code != "fetch_error" {
		t.Fatalf("expected fetch_error code, got %+v", result.Error)
	}
}

func TestFetchAccountUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := &Provider{client: &Client{http: doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			t.Fatalf("path = %q, want /api/oauth/usage", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body: io.NopCloser(strings.NewReader(
				`{"five_hour":{"utilization":25.0,"resets_at":"2026-03-20T12:00:00Z"}}`,
			)),
		}, nil
	})}}

	acct := keyring.ClaudeOAuth{
		AccessToken:      "test-token",
		SubscriptionType: "max",
		RateLimitTier:    "tier-1",
		Email:            "user@example.com",
		AccountUUID:      "uuid-123",
	}

	result, retryAfter, err := p.FetchAccountUsage(context.Background(), acct, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0", retryAfter)
	}
	if result.Status != quota.StatusOK {
		t.Fatalf("status = %v, want %v", result.Status, quota.StatusOK)
	}
	if result.Plan != "max" {
		t.Fatalf("plan = %q, want max", result.Plan)
	}
	if result.RateLimitTier != "tier-1" {
		t.Fatalf("rate limit tier = %q, want tier-1", result.RateLimitTier)
	}
	if result.Email != "user@example.com" || result.AccountID != "uuid-123" {
		t.Fatalf("identity = (%q, %q), want (user@example.com, uuid-123)", result.Email, result.AccountID)
	}
	if result.MinRemainingPct() != 75 {
		t.Fatalf("remaining = %d, want 75", result.MinRemainingPct())
	}

	manifestPath := filepath.Join(os.Getenv("HOME"), ".cache", "cq", "accounts.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("FetchAccountUsage should not write cq manifest, stat err = %v", err)
	}
}

func TestFetchAccountUsageUsesSandboxedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	p := &Provider{client: &Client{http: doerFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":25.0,"resets_at":"2026-03-20T12:00:00Z"}}`)),
		}, nil
	})}}

	_, _, err := p.FetchAccountUsage(context.Background(), keyring.ClaudeOAuth{
		AccessToken:      "test-token",
		SubscriptionType: "max",
		RateLimitTier:    "tier-1",
		Email:            "user@example.com",
		AccountUUID:      "uuid-123",
	}, time.Now())
	if err != nil {
		t.Fatalf("FetchAccountUsage: %v", err)
	}

	manifestPath := filepath.Join(home, ".cache", "cq", "accounts.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("sandboxed test should not persist cq manifest, stat err = %v", err)
	}
}

func TestFetchAccountRefreshFallbackUsesSandboxedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Keep this path read-only for now: the goal is proving sandboxed HOME on the
	// call path, not exercising real keychain persistence from the provider test.
	manifestPath := filepath.Join(home, ".cache", "cq", "accounts.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("sandbox should start clean, stat err = %v", err)
	}
}

// TestFetchAccountRefreshFailsFallsBackToCurrentToken covers the provider
// fallback fix: when refresh fails but the current access token still works
// (profile fetch succeeds), the result must be usable and not auth_expired.
func TestFetchAccountRefreshFailsFallsBackToCurrentToken(t *testing.T) {
	// Request router: refresh endpoint returns error, profile returns OK.
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth/token":
			// Refresh exchange fails.
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			}, nil
		case "/api/oauth/profile":
			// Current access token still valid.
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body: io.NopCloser(strings.NewReader(`{
					"account":{"email":"user@example.com","uuid":"uuid-123"},
					"claude_api":{"plan_type":"max","rate_limit_tier":"tier-1"}
				}`)),
			}, nil
		case "/api/oauth/usage":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body: io.NopCloser(strings.NewReader(
					`{"five_hour":{"utilization":20.0,"resets_at":"2026-03-20T12:00:00Z"}}`,
				)),
			}, nil
		default:
			t.Errorf("unexpected request to %q", req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		}
	})

	p := &Provider{client: &Client{http: doer}}
	// Token is expired (ExpiresAt in the past) and has a refresh token.
	acct := keyring.ClaudeOAuth{
		AccessToken:      "still-valid-at",
		RefreshToken:     "stale-rt",
		ExpiresAt:        1, // very old — triggers refresh path
		SubscriptionType: "max",
	}
	now := time.Now()
	result := p.fetchAccount(context.Background(), acct, now)

	if result.Status == quota.StatusError && result.Error != nil && result.Error.Code == "auth_expired" {
		t.Fatalf("got auth_expired but current access token was still valid; result = %+v", result)
	}
	if !result.IsUsable() {
		t.Fatalf("result should be usable when access token still works; result = %+v", result)
	}
}

// TestFetchAccountRefreshFailsAndProfileFailsReturnsAuthExpired covers the case
// where both the refresh exchange and the access-token profile probe fail.
func TestFetchAccountRefreshFailsAndProfileFailsReturnsAuthExpired(t *testing.T) {
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth/token":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			}, nil
		case "/api/oauth/profile":
			// Access token is also rejected.
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
			}, nil
		default:
			t.Errorf("unexpected request to %q", req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		}
	})

	p := &Provider{client: &Client{http: doer}}
	acct := keyring.ClaudeOAuth{
		AccessToken:      "dead-at",
		RefreshToken:     "dead-rt",
		ExpiresAt:        1,
		SubscriptionType: "max",
	}
	now := time.Now()
	result := p.fetchAccount(context.Background(), acct, now)

	if result.Status != quota.StatusError {
		t.Fatalf("expected StatusError, got %v", result.Status)
	}
	if result.Error == nil || result.Error.Code != "auth_expired" {
		t.Fatalf("expected auth_expired code, got %+v", result.Error)
	}
}

func TestFetchAccountProfileFailureFallsBackToStoredIdentity(t *testing.T) {
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/oauth/profile":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
			}, nil
		case "/api/oauth/usage":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body: io.NopCloser(strings.NewReader(
					`{"five_hour":{"utilization":20.0,"resets_at":"2026-03-20T12:00:00Z"}}`,
				)),
			}, nil
		default:
			t.Errorf("unexpected request to %q", req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		}
	})

	p := &Provider{client: &Client{http: doer}}
	acct := keyring.ClaudeOAuth{
		AccessToken:      "still-valid-at",
		SubscriptionType: "max",
		RateLimitTier:    "tier-1",
		Email:            "user@example.com",
		AccountUUID:      "uuid-123",
	}

	result := p.fetchAccount(context.Background(), acct, time.Now())
	if !result.IsUsable() {
		t.Fatalf("expected usable result, got %+v", result)
	}
	if result.Email != "user@example.com" {
		t.Fatalf("Email = %q, want %q", result.Email, "user@example.com")
	}
	if result.AccountID != "uuid-123" {
		t.Fatalf("AccountID = %q, want %q", result.AccountID, "uuid-123")
	}
}

func TestFetchAccountUsageRetryAfter(t *testing.T) {
	p := &Provider{client: &Client{http: doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"120"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
		}, nil
	})}}

	result, retryAfter, err := p.FetchAccountUsage(context.Background(), keyring.ClaudeOAuth{
		AccessToken:      "test-token",
		SubscriptionType: "max",
	}, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if retryAfter != 120*time.Second {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, 120*time.Second)
	}
	if result.Error == nil || result.Error.Code != "api_error" || result.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("unexpected error result: %+v", result.Error)
	}
	if result.Error.Message != "api error (retry_after=2m0s)" {
		t.Fatalf("message = %q, want %q", result.Error.Message, "api error (retry_after=2m0s)")
	}
}
