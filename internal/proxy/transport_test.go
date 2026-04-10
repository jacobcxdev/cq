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
		if isExcluded(a, excludeSet) || a.AccessToken == "" {
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
	transport.suppressFailoverForKey = "stale-suppression"

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
	if transport.suppressFailoverForKey != "" {
		t.Errorf("suppression = %q, want cleared after successful 401 recovery", transport.suppressFailoverForKey)
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

// --- immediate 429 replay tests (new behavior) ---

// TestTokenTransport_429ImmediateReplayToSecondAccount verifies that the first
// /v1/messages 429 immediately replays to the second account (no counter needed).
func TestTokenTransport_429ImmediateReplayToSecondAccount(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
	}}

	var calls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (immediate replay to alternate)", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + one replay)", calls)
	}
}

// TestTokenTransport_429WalksMultipleAlternates verifies that the transport
// walks through alternates until one succeeds.
func TestTokenTransport_429WalksMultipleAlternates(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
		{Email: "c@test.com", AccountUUID: "uuid-c", AccessToken: "tok-c", ExpiresAt: future},
	}}

	var calls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			auth := req.Header.Get("Authorization")
			if auth == "Bearer tok-a" || auth == "Bearer tok-b" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (walk to third account)", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (a→b→c)", calls)
	}
}

// TestTokenTransport_429FreshQuotaWithCapacity_ReplaysButNoSwitch verifies that
// when the failing account's fresh quota shows remaining capacity, the transport
// still replays to an alternate (giving the client a good response) but does NOT
// persist a switch (no Switcher call).
func TestTokenTransport_429FreshQuotaWithCapacity_ReplaysButNoSwitch(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: future,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: future,
	}

	switchCh := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Quota:    quotaCacheWithSnapshot("uuid-a", 80, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replayed to alternate)", resp.StatusCode)
	}

	// No switch should be persisted — fresh quota says account A still has capacity.
	select {
	case email := <-switchCh:
		t.Errorf("unexpected switch to %q — fresh quota shows 80%% remaining, should not persist switch", email)
	case <-time.After(50 * time.Millisecond):
		// good: no switch persisted
	}
}

// TestTokenTransport_429ConfirmedExhaustion_PersistsSwitch verifies that when
// fresh quota confirms the account is exhausted (0% remaining), the switch IS persisted.
func TestTokenTransport_429ConfirmedExhaustion_PersistsSwitch(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: future,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: future,
	}

	switchDone := make(chan string, 1)
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Quota:    quotaCacheWithSnapshot("uuid-a", 0, 0),
		Switcher: func(_ context.Context, email string) error {
			switchDone <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replay succeeded)", resp.StatusCode)
	}

	select {
	case email := <-switchDone:
		if email != "b@test.com" {
			t.Errorf("switched to %q, want b@test.com", email)
		}
	case <-time.After(time.Second):
		t.Error("expected switch to be persisted — quota shows 0%% remaining (confirmed exhausted)")
	}
}

// TestTokenTransport_429UnknownQuota_TriesAlternates verifies that stale/missing
// quota (unknown exhaustion status) still causes alternates to be tried.
func TestTokenTransport_429UnknownQuota_TriesAlternates(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: future,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: future,
	}

	// No quota cache → unknown status.
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Quota:    nil,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (alternates tried for unknown quota)", resp.StatusCode)
	}
}

// TestTokenTransport_429AllAccountsExhausted_Surfaces429OnlyAfterTryingAll verifies
// that when all accounts return 429, the client only sees 429 after all candidates
// have been tried.
func TestTokenTransport_429AllAccountsExhausted_Surfaces429OnlyAfterTryingAll(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
		{Email: "c@test.com", AccountUUID: "uuid-c", AccessToken: "tok-c", ExpiresAt: future},
	}}

	var calls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return makeResponse(429, "all exhausted"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (all accounts exhausted)", resp.StatusCode)
	}
	// Should have tried all 3 accounts: initial + 2 alternates.
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (tried all accounts before surfacing 429)", calls)
	}
}

// TestTokenTransport_429SingleAccount_Surfaces429Immediately verifies that when
// there is only one account and it returns 429, the 429 is forwarded immediately.
func TestTokenTransport_429SingleAccount_Surfaces429Immediately(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "only@test.com", AccountUUID: "uuid-only", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	var calls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return makeResponse(429, "rate limited"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no alternate to try)", calls)
	}
}

// TestTokenTransport_429NonMessagesEndpoint_ForwardedUnchanged verifies that
// non-/v1/messages 429s are forwarded without attempting replay.
func TestTokenTransport_429NonMessagesEndpoint_ForwardedUnchanged(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-1", ExpiresAt: future},
		{Email: "b@test.com", AccessToken: "tok-2", ExpiresAt: future},
	}}

	var upstreamCalls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			upstreamCalls++
			return makeResponse(429, "rate limited"), nil
		}),
	}

	// Non-messages endpoints should never trigger replay.
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
			t.Errorf("req %d: status = %d, want 429 (forwarded, no replay)", i+1, resp.StatusCode)
		}
	}
	if upstreamCalls != 5 {
		t.Errorf("upstream calls = %d, want 5 (no replay on non-messages)", upstreamCalls)
	}
}

// TestTokenTransport_429FailoverSuppression_SetAfterFullWalk verifies that
// failover suppression is set after all accounts have been tried and all returned 429.
func TestTokenTransport_429FailoverSuppression_SetAfterFullWalk(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
	}}

	var switchCount atomic.Int32
	transport := &TokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, _ string) error {
			switchCount.Add(1)
			return nil
		},
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "all exhausted"), nil
		}),
	}

	// First request: walks a→b, both 429 → suppression set.
	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}

	if transport.suppressFailoverForKey == "" {
		t.Error("expected failover suppression to be set after full walk")
	}

	// Second request: suppressed — no replay, just forward 429.
	var calls int
	transport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls++
		return makeResponse(429, "still exhausted"), nil
	})
	resp, err = transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (suppressed)", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (suppressed, no replay)", calls)
	}
}

// TestTokenTransport_429FailoverSuppression_ClearedAfterSuccess verifies that
// failover suppression is cleared after a later non-429 success.
func TestTokenTransport_429FailoverSuppression_ClearedAfterSuccess(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
	}}

	var calls int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "all exhausted"), nil
		}),
	}

	// Walk all accounts → suppression set.
	transport.RoundTrip(makeRequest("")) //nolint:errcheck

	if transport.suppressFailoverForKey == "" {
		t.Fatal("suppression should be set before testing clear")
	}

	// Now a successful response clears suppression.
	transport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls++
		return makeResponse(200, "ok"), nil
	})
	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if transport.suppressFailoverForKey != "" {
		t.Error("expected failover suppression to be cleared after success")
	}

	// Next 429 should replay again (suppression is gone).
	calls = 0
	transport.Inner = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") == "Bearer tok-a" {
			return makeResponse(429, "rate limited"), nil
		}
		return makeResponse(200, "ok"), nil
	})
	resp, err = transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replay resumed after suppression cleared)", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + replay after suppression cleared)", calls)
	}
}

// TestTokenTransport_429ResetOnSuccess verifies that a successful response clears
// any suppression state (regression: ensure non-429 paths work correctly).
func TestTokenTransport_429ResetOnSuccess(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccountUUID: "uuid-p", AccessToken: "tok-1", ExpiresAt: future},
		{Email: "secondary@test.com", AccountUUID: "uuid-s", AccessToken: "tok-2", ExpiresAt: future},
	}}

	var callCount int
	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callCount++
			// Calls 1,2 hit account A with 429 → walk to B → 200.
			// Call 3 is a success on A (transport may have switched).
			// After success, replay should work again.
			switch callCount {
			case 1:
				return makeResponse(429, "rate limited"), nil
			default:
				return makeResponse(200, "ok"), nil
			}
		}),
	}

	// First request: 429 on A → replay to B → 200.
	resp, _ := transport.RoundTrip(makeRequest(""))
	if resp.StatusCode != 200 {
		t.Fatalf("req 1: status = %d, want 200", resp.StatusCode)
	}

	// Subsequent requests succeed directly.
	for i := 0; i < 3; i++ {
		resp, _ := transport.RoundTrip(makeRequest(""))
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: status = %d, want 200", i+2, resp.StatusCode)
		}
	}
}

// TestTokenTransport_401RefreshRetry_CountTokens verifies that 401 refresh works
// for the count_tokens endpoint.
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

// TestTokenTransport_429CountTokensForwarded verifies that count_tokens 429s
// are forwarded without replay (not a /v1/messages path).
func TestTokenTransport_429CountTokensForwarded(t *testing.T) {
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

// TestTokenTransport_ContextCancellation verifies that context cancellation propagates.
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

func quotaCacheWithSnapshot(id string, remainingPct int, age time.Duration) *QuotaCache {
	now := time.Now()
	return &QuotaCache{
		nowFunc: func() time.Time { return now },
		snapshots: map[string]QuotaSnapshot{
			id: {
				Result: quota.Result{
					AccountID: id,
					Status:    quota.StatusOK,
					Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: remainingPct},
					},
				},
				FetchedAt: now.Add(-age),
			},
		},
		cooldowns: make(map[string]time.Time),
	}
}

// TestTransport_Transient429_HighQuota_ReplaysNoSwitch verifies that when the
// failing account has 80% quota remaining, replay still happens but no switch
// is persisted.
func TestTransport_Transient429_HighQuota_ReplaysNoSwitch(t *testing.T) {
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
		Quota:    quotaCacheWithSnapshot("uuid-a", 80, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Should succeed via replay to B.
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replayed to alternate)", resp.StatusCode)
	}

	select {
	case email := <-switchCh:
		t.Errorf("unexpected switch to %q — fresh quota shows 80%% remaining, no switch should be persisted", email)
	case <-time.After(50 * time.Millisecond):
		// good: no switch persisted
	}
}

// TestTransport_Transient429_LowQuota_Switches verifies that confirmed exhaustion
// (0% remaining) causes the switch to be persisted.
func TestTransport_Transient429_LowQuota_Switches(t *testing.T) {
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
		Quota:    quotaCacheWithSnapshot("uuid-a", 0, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case email := <-switchCh:
		if email != "b@test.com" {
			t.Errorf("switched to %q, want b@test.com", email)
		}
	case <-time.After(time.Second):
		t.Error("expected account switch — quota shows 0%% remaining (confirmed exhausted)")
	}
}

// TestTransport_Transient429_StaleQuota_TriesAlternates verifies that stale quota
// data (unknown exhaustion status) still causes alternates to be tried.
func TestTransport_Transient429_StaleQuota_TriesAlternates(t *testing.T) {
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	// Stale snapshot (10 minutes old) — Refresh() will attempt fetch, which fails → unknown.
	var refreshCalls atomic.Int32
	quotaCache := quotaCacheWithSnapshot("uuid-a", 80, 10*time.Minute)
	quotaCache.UsageFetchFunc = func(context.Context, keyring.ClaudeOAuth, time.Time) (quota.Result, time.Duration, error) {
		refreshCalls.Add(1)
		return quota.ErrorResult("api_error", "api error", http.StatusTooManyRequests), 0, nil
	}

	var calls int
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Quota:    quotaCache,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (alternates tried for unknown/stale quota)", resp.StatusCode)
	}
	if calls < 2 {
		t.Errorf("calls = %d, want >= 2 (initial + replay to alternate)", calls)
	}
}

// TestTokenTransport_429WalkContinuesPastAlternateFailure verifies that the
// replay walk continues past a non-429 alternate failure (e.g. 500) and can
// still succeed on a later account.
func TestTokenTransport_429WalkContinuesPastAlternateFailure(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
		{Email: "c@test.com", AccountUUID: "uuid-c", AccessToken: "tok-c", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Header.Get("Authorization") {
			case "Bearer tok-a":
				return makeResponse(429, "rate limited"), nil
			case "Bearer tok-b":
				return makeResponse(500, "server error"), nil
			default:
				return makeResponse(200, "ok"), nil
			}
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (walk past 500 on b, succeed on c)", resp.StatusCode)
	}
}

// TestTokenTransport_429WalkNonSuccessReturnedWhenAllFail verifies that when no
// account succeeds and at least one returned a non-429 failure, the last
// non-429 failure is returned even if a later alternate returns 429.
func TestTokenTransport_429WalkNonSuccessReturnedWhenAllFail(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
		{Email: "c@test.com", AccountUUID: "uuid-c", AccessToken: "tok-c", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Header.Get("Authorization") {
			case "Bearer tok-a":
				return makeResponse(429, "rate limited"), nil
			case "Bearer tok-b":
				return makeResponse(503, "service unavailable"), nil
			default:
				return makeResponse(429, "rate limited again"), nil
			}
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503 (preserve the last non-429 failure)", resp.StatusCode)
	}
	if transport.suppressFailoverForKey != "" {
		t.Errorf("suppression = %q, want empty when a non-429 failure was seen", transport.suppressFailoverForKey)
	}
}

// TestTokenTransport_429FailoverSuppression_ClearsOnSuccessFromDifferentAccount
// verifies that suppression clears when a *different* account delivers a success
// (not just the originally-suppressed one).
func TestTokenTransport_429FailoverSuppression_ClearsOnSuccessFromDifferentAccount(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	acctA := keyring.ClaudeOAuth{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future}
	acctB := keyring.ClaudeOAuth{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future}

	// Set up: both accounts return 429 → suppression set on a.
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, "rate limited"), nil
		}),
	}
	transport.RoundTrip(makeRequest("")) //nolint:errcheck
	if transport.suppressFailoverForKey == "" {
		t.Fatal("suppression should be set")
	}

	// Now selector returns b (simulating a switch happened externally), which returns 200.
	// Suppression must clear regardless of which account key delivered the success.
	transport.Selector = &fakeSelector{accounts: []keyring.ClaudeOAuth{acctB}}
	transport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return makeResponse(200, "ok"), nil
	})
	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if transport.suppressFailoverForKey != "" {
		t.Error("suppression should be cleared after any successful response, not just from the suppressed account")
	}

	// Confirm replay works again on next 429.
	var calls int
	transport.Selector = &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}}
	transport.Inner = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") == "Bearer tok-a" {
			return makeResponse(429, "rate limited"), nil
		}
		return makeResponse(200, "ok"), nil
	})
	resp, err = transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replay resumed)", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestTokenTransport_429ReplaySuccessClearsExistingSuppression(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	acctA := keyring.ClaudeOAuth{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future}
	acctB := keyring.ClaudeOAuth{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future}
	acctC := keyring.ClaudeOAuth{Email: "c@test.com", AccountUUID: "uuid-c", AccessToken: "tok-c", ExpiresAt: future}

	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctB, acctC}},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Header.Get("Authorization") {
			case "Bearer tok-b":
				return makeResponse(429, "rate limited"), nil
			case "Bearer tok-c":
				return makeResponse(200, "ok"), nil
			default:
				return makeResponse(500, "unexpected account"), nil
			}
		}),
	}
	transport.suppressFailoverForKey = acctIdentifier(&acctA)

	resp, err := transport.RoundTrip(makeRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if transport.suppressFailoverForKey != "" {
		t.Errorf("suppression = %q, want cleared after replay succeeds on a different account", transport.suppressFailoverForKey)
	}
}

// TestTransport_Transient429_NilQuota_TriesAlternates verifies that nil quota
// (no cache) still causes alternates to be tried immediately on first 429.
func TestTransport_Transient429_NilQuota_TriesAlternates(t *testing.T) {
	acctA := keyring.ClaudeOAuth{
		Email: "a@test.com", AccountUUID: "uuid-a",
		AccessToken: "tok-a", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}
	acctB := keyring.ClaudeOAuth{
		Email: "b@test.com", AccountUUID: "uuid-b",
		AccessToken: "tok-b", ExpiresAt: time.Now().UnixMilli() + 3600_000,
	}

	var calls int
	transport := &TokenTransport{
		Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acctA, acctB}},
		Quota:    nil,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, "rate limited"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeRequest(`{"model":"claude-sonnet-4-6"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (nil quota — alternates tried immediately)", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + replay)", calls)
	}
}
