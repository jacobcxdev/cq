package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	claude "github.com/jacobcxdev/cq/internal/provider/claude"
	"github.com/jacobcxdev/cq/internal/quota"
)

// --- test helpers (shared with server_test.go) ---

type fakeSelector struct {
	accounts []keyring.ClaudeOAuth
}

func (f *fakeSelector) Select(_ context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	for i := range f.accounts {
		a := &f.accounts[i]
		if (a.Email != "" && excludeSet[a.Email]) ||
			(a.AccountUUID != "" && excludeSet[a.AccountUUID]) {
			continue
		}
		result := *a
		return &result, nil
	}
	return nil, fmt.Errorf("no accounts available")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func makeResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func makeRequest(body string) *http.Request {
	buf := []byte(body)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(buf))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	req.ContentLength = int64(len(buf))
	return req
}

// --- transport tests ---

func TestTokenTransport_HappyPath(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	var gotAuth, gotBeta, gotAPIKey string
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			gotBeta = req.Header.Get("anthropic-beta")
			gotAPIKey = req.Header.Get("x-api-key")
			return makeResponse(200, `{"ok":true}`), nil
		}),
	}

	req := makeRequest(`{"msg":"hello"}`)
	req.Header.Set("x-api-key", "should-be-stripped")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer tok-a" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-a")
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, missing oauth beta", gotBeta)
	}
	if gotAPIKey != "" {
		t.Errorf("x-api-key should be stripped, got %q", gotAPIKey)
	}
}

func TestTokenTransport_AppendsBeta(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	var gotBeta string
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotBeta = req.Header.Get("anthropic-beta")
			return makeResponse(200, "ok"), nil
		}),
	}

	req := makeRequest("")
	req.Header.Set("anthropic-beta", "existing-feature")

	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBeta, "existing-feature") {
		t.Errorf("lost existing beta value: %q", gotBeta)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("missing oauth beta: %q", gotBeta)
	}
}

func TestTokenTransport_401RefreshRetry(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "old-tok", ExpiresAt: time.Now().UnixMilli() + 3600_000, RefreshToken: "rt"},
	}}

	var attempts int
	var bodyContents []string
	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			return &claude.RefreshResult{AccessToken: "new-tok", ExpiresIn: 3600}, nil
		},
		Persister:   func(_ *keyring.ClaudeOAuth) {},
		RefreshHTTP: http.DefaultClient,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			body, _ := io.ReadAll(req.Body)
			bodyContents = append(bodyContents, string(body))
			if attempts == 1 {
				return makeResponse(401, "unauthorized"), nil
			}
			if req.Header.Get("Authorization") != "Bearer new-tok" {
				t.Errorf("retry auth = %q, want Bearer new-tok", req.Header.Get("Authorization"))
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(`{"test":"body"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if len(bodyContents) != 2 || bodyContents[1] != `{"test":"body"}` {
		t.Errorf("body replay failed: %v", bodyContents)
	}
}

func TestTokenTransport_401RefreshFails(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000, RefreshToken: "rt"},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			return nil, fmt.Errorf("refresh server down")
		},
		RefreshHTTP: http.DefaultClient,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(401, "unauthorized"), nil
		}),
	}

	_, err := transport.RoundTrip(makeRequest(""))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("error should mention refresh: %v", err)
	}
}

func TestTokenTransport_ConcurrentRefresh(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "old", ExpiresAt: time.Now().UnixMilli() + 3600_000, RefreshToken: "rt"},
	}}

	var refreshCount atomic.Int32
	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			refreshCount.Add(1)
			time.Sleep(50 * time.Millisecond) // simulate latency
			return &claude.RefreshResult{AccessToken: "new", ExpiresIn: 3600}, nil
		},
		Persister:   func(_ *keyring.ClaudeOAuth) {},
		RefreshHTTP: http.DefaultClient,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer old" {
				return makeResponse(401, ""), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := transport.RoundTrip(makeRequest(""))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if resp.StatusCode != 200 {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if n := refreshCount.Load(); n != 1 {
		t.Errorf("refresh called %d times, want 1", n)
	}
}

func TestTokenTransport_429TransientForwarded(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccessToken: "tok-1", ExpiresAt: future},
		{Email: "secondary@test.com", AccessToken: "tok-2", ExpiresAt: future},
	}}

	var upstreamCalls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			upstreamCalls++
			return makeResponse(429, "rate limited"), nil
		}),
	}

	// A single 429 should be forwarded to the client, not trigger failover.
	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (forwarded)", resp.StatusCode)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstream calls = %d, want 1 (no failover)", upstreamCalls)
	}
}

func TestTokenTransport_429ExhaustionTriggersFailover(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccessToken: "tok-1", ExpiresAt: future},
		{Email: "secondary@test.com", AccessToken: "tok-2", ExpiresAt: future},
	}}

	var switchedTo string
	var switchDone sync.WaitGroup
	switchDone.Add(1)

	transport := &TokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, email string) error {
			switchedTo = email
			switchDone.Done()
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			auth := req.Header.Get("Authorization")
			if auth == "Bearer tok-1" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	// First two 429s: transient, forwarded to client.
	for i := 0; i < 2; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}

	// Third 429: exhaustion triggers failover to secondary.
	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("req 3: status = %d, want 200 (failover)", resp.StatusCode)
	}

	// Wait for async switch to complete.
	switchDone.Wait()
	if switchedTo != "secondary@test.com" {
		t.Errorf("switched to %q, want secondary@test.com", switchedTo)
	}
}

func TestTokenTransport_429NoAlternate(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "only@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "rate limited"), nil
		}),
	}

	// Exhaust the single account (3 consecutive 429s).
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 429 {
			t.Errorf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
}

func TestTokenTransport_429ResetOnSuccess(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccessToken: "tok-1", ExpiresAt: future},
	}}

	var callCount int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			callCount++
			// First two calls: 429, then a 200, then two more 429s.
			// The 200 should reset the counter so the final 429s don't trigger exhaustion.
			switch callCount {
			case 1, 2, 4, 5:
				return makeResponse(429, "rate limited"), nil
			default:
				return makeResponse(200, "ok"), nil
			}
		}),
	}

	// Two 429s (count=2).
	for i := 0; i < 2; i++ {
		resp, _ := transport.RoundTrip(makeRequest(""))
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}

	// Success resets counter.
	resp, _ := transport.RoundTrip(makeRequest(""))
	if resp.StatusCode != 200 {
		t.Fatalf("req 3: status = %d, want 200", resp.StatusCode)
	}

	// Two more 429s — counter is back at 2, not 4 (no exhaustion).
	for i := 0; i < 2; i++ {
		resp, _ := transport.RoundTrip(makeRequest(""))
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+4, resp.StatusCode)
		}
	}
}

func TestTokenTransport_401RefreshRetry_CountTokens(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{{
		Email: "a@test.com", AccessToken: "old-tok", ExpiresAt: time.Now().UnixMilli() + 3600_000, RefreshToken: "rt",
	}}}

	var attempts int
	var paths []string
	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			return &claude.RefreshResult{AccessToken: "new-tok", ExpiresIn: 3600}, nil
		},
		Persister:   func(_ *keyring.ClaudeOAuth) {},
		RefreshHTTP: http.DefaultClient,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			paths = append(paths, req.URL.Path)
			if attempts == 1 {
				return makeResponse(401, "unauthorized"), nil
			}
			if req.Header.Get("Authorization") != "Bearer new-tok" {
				t.Errorf("retry auth = %q, want Bearer new-tok", req.Header.Get("Authorization"))
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	buf := []byte(`{"model":"claude-opus-4-6"}`)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com"+countTokensPath, bytes.NewReader(buf))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	req.ContentLength = int64(len(buf))

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(paths) != 2 || paths[0] != countTokensPath || paths[1] != countTokensPath {
		t.Fatalf("paths = %v, want both %q", paths, countTokensPath)
	}
}

func TestTokenTransport_429CountTokensForwarded(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{{Email: "primary@test.com", AccessToken: "tok-1", ExpiresAt: future}, {Email: "secondary@test.com", AccessToken: "tok-2", ExpiresAt: future}}}

	var upstreamCalls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			upstreamCalls++
			return makeResponse(429, "rate limited"), nil
		}),
	}

	for i := 0; i < 5; i++ {
		buf := []byte(`{}`)
		req, _ := http.NewRequest("POST", "https://api.anthropic.com"+countTokensPath, bytes.NewReader(buf))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	if upstreamCalls != 5 {
		t.Fatalf("upstream calls = %d, want 5", upstreamCalls)
	}
}

func TestTokenTransport_429NonMessagesEndpointForwarded(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccessToken: "tok-1", ExpiresAt: future},
		{Email: "secondary@test.com", AccessToken: "tok-2", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "rate limited"), nil
		}),
	}

	// Hit a non-messages endpoint 5 times — all should forward 429,
	// never trigger exhaustion or failover.
	for i := 0; i < 5; i++ {
		buf := []byte(`{}`)
		req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/usage", bytes.NewReader(buf))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Errorf("req %d: status = %d, want 429 (forwarded, no exhaustion tracking)", i+1, resp.StatusCode)
		}
	}
}

func TestTokenTransport_429PingPongPrevention(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccessToken: "tok-b", ExpiresAt: future},
	}}

	var switchCount atomic.Int32
	transport := &TokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, _ string) error {
			switchCount.Add(1)
			return nil
		},
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			// Both accounts always return 429.
			return makeResponse(429, "rate limited"), nil
		}),
	}

	// Exhaust account A (3 requests) → failover to B → B returns 429.
	// Then exhaust B (needs 3 fresh requests — the failover doRequest
	// response bypasses handle429 so B starts at 0).
	// Then failover back to A.
	// Key assertion: each account gets a fresh 3-request window after
	// being rotated away from (no infinite ping-pong).

	// 3 requests exhaust A → switch to B (B's 429 is the failover response).
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatalf("phase1 req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("phase1 req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	// Switcher is async — give it a moment.
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 1 {
		t.Fatalf("after phase1: switchCount = %d, want 1", n)
	}

	// B now returns 429s too, but because the initial failover also failed,
	// we should suppress further switching until some account replenishes.
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatalf("phase2 req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("phase2 req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 1 {
		t.Fatalf("after phase2: switchCount = %d, want 1", n)
	}

	// Once a request succeeds again, switching may resume later.
	var recovered atomic.Bool
	transport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if !recovered.Swap(true) {
			return makeResponse(200, "ok"), nil
		}
		return makeResponse(429, "rate limited"), nil
	})

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatalf("phase3 recovery: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("phase3 recovery: status = %d, want 200", resp.StatusCode)
	}

	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatalf("phase4 req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("phase4 req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 2 {
		t.Fatalf("after phase4: switchCount = %d, want 2", n)
	}
}

func TestTokenTransport_429StopsSwitchingWhenAllAccountsExhausted(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccessToken: "tok-b", ExpiresAt: future},
	}}

	var switchCount atomic.Int32
	transport := &TokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, _ string) error {
			switchCount.Add(1)
			return nil
		},
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "rate limited"), nil
		}),
	}

	for i := 0; i < 9; i++ {
		resp, err := transport.RoundTrip(makeRequest(""))
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 1 {
		t.Fatalf("switchCount = %d, want 1 after all accounts exhausted", n)
	}
	if transport.suppressFailoverForKey == "" {
		t.Fatal("expected failover suppression to be set")
	}
}

func TestTokenTransport_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, req.Context().Err()
		}),
	}

	req := makeRequest("")
	req = req.WithContext(ctx)
	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// --- quota-aware 429 tests ---

func monitorWithSnapshot(id string, remainingPct int, age time.Duration) *QuotaMonitor {
	return &QuotaMonitor{
		snapshots: map[string]QuotaSnapshot{
			id: {
				Result: quota.Result{
					AccountID: id,
					Status:    quota.StatusOK,
					Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: remainingPct},
					},
				},
				FetchedAt: time.Now().Add(-age),
			},
		},
	}
}

func TestTransport_Transient429_HighQuota_NoSwitch(t *testing.T) {
	// Account has 80% remaining — 3 consecutive 429s should be treated as transient.
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	switchCh := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Monitor:  monitorWithSnapshot("uuid-a", 80, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	// Send 3+ requests to trigger exhaustion threshold.
	for i := 0; i < exhaustionThreshold+1; i++ {
		resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 429 {
			t.Fatalf("request %d: expected 429, got %d", i, resp.StatusCode)
		}
	}

	select {
	case email := <-switchCh:
		t.Errorf("expected no account switch — quota shows 80%% remaining, but switched to %s", email)
	default:
	}
}

func TestTransport_Transient429_LowQuota_Switches(t *testing.T) {
	// Account has 5% remaining — 429s should trigger a switch.
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	switchCh := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Monitor:  monitorWithSnapshot("uuid-a", 5, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	for i := 0; i < exhaustionThreshold; i++ {
		resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	select {
	case <-switchCh:
	case <-time.After(time.Second):
		t.Error("expected account switch — quota shows only 5% remaining")
	}
}

func TestTransport_Transient429_StaleQuota_Switches(t *testing.T) {
	// Account has 80% remaining but data is 10 minutes old — too stale to trust.
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	switchCh := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Monitor:  monitorWithSnapshot("uuid-a", 80, 10*time.Minute),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	for i := 0; i < exhaustionThreshold; i++ {
		resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	select {
	case <-switchCh:
	case <-time.After(time.Second):
		t.Error("expected account switch — quota data is stale (10 min old)")
	}
}

func TestTransport_Transient429_NilMonitor_Unchanged(t *testing.T) {
	// No monitor at all — existing behavior (switches after threshold).
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	switchCh := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Monitor:  nil,
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	for i := 0; i < exhaustionThreshold; i++ {
		resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	select {
	case <-switchCh:
	case <-time.After(time.Second):
		t.Error("expected account switch — nil monitor should not prevent it")
	}
}

func TestTransport_Transient429_CounterReset(t *testing.T) {
	// After transient detection, the 429 counter should be reset so it takes
	// another full threshold of 429s to trigger again.
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	var switchCalled bool
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA}},
		Monitor:  monitorWithSnapshot("uuid-a", 80, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCalled = true
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	// First batch: triggers threshold, but transient check resets counter.
	for i := 0; i < exhaustionThreshold; i++ {
		resp, _ := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
		resp.Body.Close()
	}
	if switchCalled {
		t.Fatal("unexpected switch after first batch")
	}

	// One more 429 should NOT trigger switch (counter was reset).
	resp, _ := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
	resp.Body.Close()
	if switchCalled {
		t.Error("expected no switch — counter should have been reset after transient detection")
	}
}
