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
