package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// multiCodexSelector supports exclude filtering across multiple accounts.
type multiCodexSelector struct {
	accounts []codex.CodexAccount
}

func (s *multiCodexSelector) Select(_ context.Context, exclude ...string) (*codex.CodexAccount, error) {
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	for i := range s.accounts {
		a := &s.accounts[i]
		if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" {
			continue
		}
		result := *a
		return &result, nil
	}
	return nil, fmt.Errorf("no codex accounts available")
}

func makeCodexRequest(body string) *http.Request {
	buf := []byte(body)
	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req.ContentLength = int64(len(buf))
	return req
}

func TestCodexTokenTransport_HappyPath(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
	}}

	var gotAuth, gotAcctID string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			gotAcctID = req.Header.Get("ChatGPT-Account-ID")
			return makeResponse(200, `{"ok":true}`), nil
		}),
	}

	req := makeCodexRequest(`{"model":"gpt-5.4"}`)
	req.Header.Set("Authorization", "Bearer original-should-be-replaced")

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
	if gotAcctID != "acct-1" {
		t.Errorf("ChatGPT-Account-ID = %q, want %q", gotAcctID, "acct-1")
	}
}

func TestCodexTokenTransport_NoAccountIDHeader(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
	}}

	var gotAcctID string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAcctID = req.Header.Get("ChatGPT-Account-ID")
			return makeResponse(200, `{"ok":true}`), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if gotAcctID != "" {
		t.Errorf("ChatGPT-Account-ID = %q, want empty (no account ID)", gotAcctID)
	}
}

func TestCodexTokenTransport_401Failover(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(401, "unauthorized"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (failover to b)", resp.StatusCode)
	}
}

func TestCodexTokenTransport_401NoAlternate(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(401, "unauthorized"), nil
		}),
	}

	_, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err == nil {
		t.Fatal("expected error with no alternate account")
	}
	if !strings.Contains(err.Error(), "no alternate") {
		t.Errorf("error = %v, want mention of no alternate", err)
	}
}

func TestCodexTokenTransport_429TransientForwarded(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
		{Email: "b@test.com", AccessToken: "tok-b"},
	}}

	var calls int
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded","type":"requests"}}`), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (forwarded)", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no failover on transient)", calls)
	}
}

func TestCodexTokenTransport_429CounterExhaustion(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
		{Email: "b@test.com", AccessToken: "tok-b"},
	}}

	var switchedTo string
	var switchDone = make(chan struct{}, 1)

	transport := &CodexTokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, email string) error {
			switchedTo = email
			switchDone <- struct{}{}
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded","type":"requests"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	// First two: transient, forwarded.
	for i := 0; i < 2; i++ {
		resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}

	// Third triggers failover.
	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("req 3: status = %d, want 200 (failover)", resp.StatusCode)
	}

	select {
	case <-switchDone:
	case <-time.After(time.Second):
		t.Fatal("switch not persisted")
	}
	if switchedTo != "b@test.com" {
		t.Errorf("switched to %q, want b@test.com", switchedTo)
	}
}

func TestCodexTokenTransport_429ImmediateOnInsufficientQuota(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
		{Email: "b@test.com", AccessToken: "tok-b"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	// First request with insufficient_quota should immediately failover (no counter).
	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (immediate failover on insufficient_quota)", resp.StatusCode)
	}
}

func TestCodexTokenTransport_429ResetOnSuccess(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
	}}

	var callCount int
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			callCount++
			switch callCount {
			case 1, 2, 4, 5:
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			default:
				return makeResponse(200, "ok"), nil
			}
		}),
	}

	// Two 429s (count=2).
	for i := 0; i < 2; i++ {
		resp, _ := transport.RoundTrip(makeCodexRequest(`{}`))
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}

	// Success resets counter.
	resp, _ := transport.RoundTrip(makeCodexRequest(`{}`))
	if resp.StatusCode != 200 {
		t.Fatalf("req 3: status = %d, want 200", resp.StatusCode)
	}

	// Two more 429s — counter is back at 2, no exhaustion.
	for i := 0; i < 2; i++ {
		resp, _ := transport.RoundTrip(makeCodexRequest(`{}`))
		if resp.StatusCode != 429 {
			t.Fatalf("req %d: status = %d, want 429", i+4, resp.StatusCode)
		}
	}
}

func TestCodexTokenTransport_429PingPongPrevention(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a"},
		{Email: "b@test.com", AccessToken: "tok-b"},
	}}

	var switchCount atomic.Int32
	transport := &CodexTokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, _ string) error {
			switchCount.Add(1)
			return nil
		},
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		}),
	}

	// 3 requests exhaust A → switch to B → B also 429.
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
		if err != nil {
			t.Fatalf("phase1 req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("phase1 req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 1 {
		t.Fatalf("after phase1: switchCount = %d, want 1", n)
	}

	// B returns 429s — failover suppressed, no more switching.
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
		if err != nil {
			t.Fatalf("phase2 req %d: %v", i+1, err)
		}
		if resp.StatusCode != 429 {
			t.Fatalf("phase2 req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := switchCount.Load(); n != 1 {
		t.Fatalf("after phase2: switchCount = %d, want 1 (suppressed)", n)
	}
}

func TestCodexTokenTransport_429NoAlternateForwards(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "only@test.com", AccessToken: "tok"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		}),
	}

	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 429 {
			t.Errorf("req %d: status = %d, want 429", i+1, resp.StatusCode)
		}
	}
}

func TestCodexTokenTransport_BodyPreservedAfter429(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok"},
	}}

	errBody := `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, errBody), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != errBody {
		t.Errorf("body = %q, want %q", string(body), errBody)
	}
}
