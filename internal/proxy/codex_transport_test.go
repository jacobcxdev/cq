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
	"github.com/jacobcxdev/cq/internal/quota"
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

func codexQuotaCacheWithSnapshot(id string, remainingPct int, age time.Duration) *QuotaCache {
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
	transport.suppressFailoverForKey = "stale-suppression"

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (failover to b)", resp.StatusCode)
	}
	if transport.suppressFailoverForKey != "" {
		t.Errorf("suppression = %q, want cleared after successful 401 failover", transport.suppressFailoverForKey)
	}
}

func TestCodexTokenTransport_401Failover_RewritesSparkForPlusAlternate(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "pro@test.com", AccessToken: "tok-pro", AccountID: "acct-pro", PlanType: "pro"},
		{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus"},
	}}

	models := make([]string, 0, 2)
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			models = append(models, extractModel(body))
			if req.Header.Get("Authorization") == "Bearer tok-pro" {
				return makeResponse(401, "unauthorized"), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}
	if len(models) != 2 {
		t.Fatalf("models seen = %d, want 2", len(models))
	}
	if models[0] != "gpt-5.3-codex-spark" {
		t.Fatalf("initial model = %q, want gpt-5.3-codex-spark", models[0])
	}
	if models[1] != "gpt-5.3-codex" {
		t.Fatalf("failover model = %q, want gpt-5.3-codex", models[1])
	}
}

func TestCodexTokenTransport_429InsufficientQuota_RewritesSparkSuffixForPlusAlternate(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "pro@test.com", AccessToken: "tok-pro", AccountID: "acct-pro", PlanType: "pro"},
		{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus"},
	}}

	models := make([]string, 0, 2)
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			models = append(models, extractModel(body))
			if req.Header.Get("Authorization") == "Bearer tok-pro" {
				return makeResponse(429, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark-high"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}
	if len(models) != 2 {
		t.Fatalf("models seen = %d, want 2", len(models))
	}
	if models[0] != "gpt-5.3-codex-spark-high" {
		t.Fatalf("initial model = %q, want gpt-5.3-codex-spark-high", models[0])
	}
	if models[1] != "gpt-5.3-codex-high" {
		t.Fatalf("failover model = %q, want gpt-5.3-codex-high", models[1])
	}
}

func TestCodexTokenTransport_PrefersProAccountForInitialSparkRequest(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus", IsActive: true},
			{Email: "pro@test.com", AccessToken: "tok-pro", AccountID: "acct-pro", PlanType: "pro", IsActive: false},
		}
	}, nil)

	var gotAuth, gotModel string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			gotAuth = req.Header.Get("Authorization")
			gotModel = extractModel(body)
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer tok-pro" {
		t.Fatalf("authorization = %q, want Bearer tok-pro", gotAuth)
	}
	if gotModel != "gpt-5.3-codex-spark" {
		t.Fatalf("model = %q, want gpt-5.3-codex-spark", gotModel)
	}
}

func TestCodexTokenTransport_RewritesSparkForInitialPlusSelection(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus", IsActive: true},
		}
	}, nil)

	var gotAuth, gotModel string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			gotAuth = req.Header.Get("Authorization")
			gotModel = extractModel(body)
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer tok-plus" {
		t.Fatalf("authorization = %q, want Bearer tok-plus", gotAuth)
	}
	if gotModel != "gpt-5.3-codex" {
		t.Fatalf("model = %q, want gpt-5.3-codex", gotModel)
	}
}

func TestCodexTokenTransport_RewritesSparkSuffixForInitialPlusSelection(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus", IsActive: true},
		}
	}, nil)

	var gotModel string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			gotModel = extractModel(body)
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark-high"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotModel != "gpt-5.3-codex-high" {
		t.Fatalf("model = %q, want gpt-5.3-codex-high", gotModel)
	}
}

func TestCodexTokenTransport_RewritesSparkWithOneMSuffixForInitialPlusSelection(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus", IsActive: true},
		}
	}, nil)

	var gotModel string
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			gotModel = extractModel(body)
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark[1m]"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotModel != "gpt-5.3-codex" {
		t.Fatalf("model = %q, want gpt-5.3-codex", gotModel)
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

// --- immediate 429 replay tests (new behavior) ---

// TestCodexTokenTransport_429ImmediateReplayToSecondAccount verifies that the
// first Codex 429 immediately replays to the second account.
func TestCodexTokenTransport_429ImmediateReplayToSecondAccount(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	var calls int
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded","type":"requests"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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

// TestCodexTokenTransport_429WalksMultipleAlternates verifies that the transport
// walks through multiple alternates until one succeeds.
func TestCodexTokenTransport_429WalksMultipleAlternates(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
		{Email: "c@test.com", AccessToken: "tok-c", AccountID: "acct-3"},
	}}

	var calls int
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			auth := req.Header.Get("Authorization")
			if auth == "Bearer tok-a" || auth == "Bearer tok-b" {
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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

// TestCodexTokenTransport_429InsufficientQuota_PersistsSwitch verifies that an
// insufficient_quota 429 persists a real switch.
func TestCodexTokenTransport_429InsufficientQuota_PersistsSwitch(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	switchDone := make(chan string, 1)
	transport := &CodexTokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, email string) error {
			switchDone <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (failover on insufficient_quota)", resp.StatusCode)
	}

	select {
	case email := <-switchDone:
		if email != "b@test.com" {
			t.Errorf("switched to %q, want b@test.com", email)
		}
	case <-time.After(time.Second):
		t.Error("expected switch to be persisted for insufficient_quota")
	}
}

// TestCodexTokenTransport_429FreshQuotaWithCapacity_ReplaysNoSwitch verifies
// that when fresh quota shows remaining capacity, replay happens but no switch
// is persisted.
func TestCodexTokenTransport_429InsufficientQuota_RewritesSparkForPlusAlternate(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "pro@test.com", AccessToken: "tok-pro", AccountID: "acct-pro", PlanType: "pro"},
		{Email: "plus@test.com", AccessToken: "tok-plus", AccountID: "acct-plus", PlanType: "plus"},
	}}

	switchDone := make(chan string, 1)
	models := make([]string, 0, 2)
	transport := &CodexTokenTransport{
		Selector: sel,
		Switcher: func(_ context.Context, email string) error {
			switchDone <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			models = append(models, extractModel(body))
			if req.Header.Get("Authorization") == "Bearer tok-pro" {
				return makeResponse(429, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{"model":"gpt-5.3-codex-spark"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}
	if len(models) != 2 {
		t.Fatalf("models seen = %d, want 2", len(models))
	}
	if models[0] != "gpt-5.3-codex-spark" {
		t.Fatalf("initial model = %q, want gpt-5.3-codex-spark", models[0])
	}
	if models[1] != "gpt-5.3-codex" {
		t.Fatalf("failover model = %q, want gpt-5.3-codex", models[1])
	}

	select {
	case email := <-switchDone:
		if email != "plus@test.com" {
			t.Errorf("switched to %q, want plus@test.com", email)
		}
	case <-time.After(time.Second):
		t.Error("expected switch to be persisted for insufficient_quota")
	}
}

func TestCodexTokenTransport_429FreshQuotaWithCapacity_ReplaysNoSwitch(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	switchCh := make(chan string, 1)
	transport := &CodexTokenTransport{
		Selector: sel,
		Quota:    codexQuotaCacheWithSnapshot("acct-1", 80, 0),
		Switcher: func(_ context.Context, email string) error {
			switchCh <- email
			return nil
		},
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
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

// TestCodexTokenTransport_429StaleSnapshot_TriesAlternates verifies that
// stale/missing snapshot (unknown status) still causes alternates to be tried
// before surfacing a final 429.
func TestCodexTokenTransport_429StaleSnapshot_TriesAlternates(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	// Stale quota snapshot (10 minutes old) — unknown status.
	var calls int
	transport := &CodexTokenTransport{
		Selector: sel,
		Quota:    codexQuotaCacheWithSnapshot("acct-1", 80, 10*time.Minute),
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") == "Bearer tok-a" {
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			}
			return makeResponse(200, "ok"), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (alternates tried for stale/unknown quota)", resp.StatusCode)
	}
	if calls < 2 {
		t.Errorf("calls = %d, want >= 2 (initial + replay to alternate)", calls)
	}
}

// TestCodexTokenTransport_429AllAccountsExhausted_Surfaces429AfterTryingAll
// verifies that when all accounts return 429, the client sees 429 only after all
// candidates have been tried.
func TestCodexTokenTransport_429AllAccountsExhausted_Surfaces429AfterTryingAll(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
		{Email: "c@test.com", AccessToken: "tok-c", AccountID: "acct-3"},
	}}

	var calls int
	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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

// TestCodexTokenTransport_429BodyPreserved verifies that the buffered 429 body
// is preserved when the response is forwarded to the client.
func TestCodexTokenTransport_429BodyPreserved(t *testing.T) {
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

// TestCodexTokenTransport_429FailoverSuppression_SetAfterFullWalk verifies that
// failover suppression is set after all accounts have been tried and all returned 429.
func TestCodexTokenTransport_429FailoverSuppression_SetAfterFullWalk(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
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

	// First request: walks a→b, both 429 → suppression set.
	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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
		return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
	})
	resp, err = transport.RoundTrip(makeCodexRequest(`{}`))
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

// TestCodexTokenTransport_429WalkContinuesPastAlternateFailure verifies that
// the replay walk continues past a non-429 alternate failure and can still
// succeed on a later account.
func TestCodexTokenTransport_429WalkContinuesPastAlternateFailure(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
		{Email: "c@test.com", AccessToken: "tok-c", AccountID: "acct-3"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Header.Get("Authorization") {
			case "Bearer tok-a":
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			case "Bearer tok-b":
				return makeResponse(500, "server error"), nil
			default:
				return makeResponse(200, "ok"), nil
			}
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (walk past 500 on b, succeed on c)", resp.StatusCode)
	}
}

// TestCodexTokenTransport_429WalkNonSuccessReturnedWhenAllFail verifies that
// when no account succeeds and at least one returned a non-429 failure, the
// last non-429 failure is returned even if a later alternate returns 429.
func TestCodexTokenTransport_429WalkNonSuccessReturnedWhenAllFail(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
		{Email: "c@test.com", AccessToken: "tok-c", AccountID: "acct-3"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Header.Get("Authorization") {
			case "Bearer tok-a":
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			case "Bearer tok-b":
				return makeResponse(503, "service unavailable"), nil
			default:
				return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			}
		}),
	}

	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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

// TestCodexTokenTransport_429FailoverSuppression_ClearedAfterSuccess verifies
// that failover suppression is cleared after a later non-429 success.
func TestCodexTokenTransport_429FailoverSuppression_ClearedAfterSuccess(t *testing.T) {
	sel := &multiCodexSelector{accounts: []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1"},
		{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2"},
	}}

	transport := &CodexTokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		}),
	}

	// Walk all accounts → suppression set.
	transport.RoundTrip(makeCodexRequest(`{}`)) //nolint:errcheck

	if transport.suppressFailoverForKey == "" {
		t.Fatal("suppression should be set before testing clear")
	}

	// A successful response clears suppression.
	transport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return makeResponse(200, "ok"), nil
	})
	resp, err := transport.RoundTrip(makeCodexRequest(`{}`))
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
	var calls int
	transport.Inner = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") == "Bearer tok-a" {
			return makeResponse(429, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		}
		return makeResponse(200, "ok"), nil
	})
	resp, err = transport.RoundTrip(makeCodexRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (replay resumed after suppression cleared)", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + replay)", calls)
	}
}
