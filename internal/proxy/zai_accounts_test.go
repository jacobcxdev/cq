package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/quota"
)

func zaiTestAccounts() ([]zaiprov.Account, error) {
	return []zaiprov.Account{{ID: "first", APIKey: "first-key"}, {ID: "second", APIKey: "second-key"}}, nil
}

func TestPrioritiseZAIAccountsQuotaWindows(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, test := range []struct {
		name      string
		window    quota.WindowName
		remaining int
		exact     *float64
		reset     int64
		wantFirst string
	}{
		{"five hour exhausted", quota.Window5Hour, 0, nil, 2000, "second"},
		{"weekly exhausted", quota.Window7Day, 0, nil, 2000, "second"},
		{"missing reset", quota.Window5Hour, 0, nil, 0, "second"},
		{"past reset", quota.Window5Hour, 0, nil, 999, "first"},
		{"reset now", quota.Window7Day, 0, nil, 1000, "first"},
		{"MCP quota exhausted", "monthly:mcp", 0, nil, 2000, "first"},
		{"other model bucket", "5h:other", 0, nil, 2000, "first"},
		{"healthy quota", quota.Window5Hour, 1, nil, 2000, "first"},
		{"rounded quota still available", quota.Window5Hour, 0, zaiTestFloatPointer(0.1), 2000, "first"},
		{"exact quota exhausted", quota.Window5Hour, 1, zaiTestFloatPointer(0), 2000, "second"},
		{"negative quota malformed", quota.Window5Hour, -5, nil, 2000, "first"},
		{"NaN quota malformed", quota.Window5Hour, 0, zaiTestFloatPointer(math.NaN()), 2000, "first"},
		{"infinite quota malformed", quota.Window7Day, 0, zaiTestFloatPointer(math.Inf(1)), 2000, "first"},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts, _ := zaiTestAccounts()
			before := append([]zaiprov.Account(nil), accounts...)
			results := []quota.Result{{AccountID: "first", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
				test.window: {RemainingPct: test.remaining, RemainingPctExact: test.exact, ResetAtUnix: test.reset},
			}}}
			got := PrioritiseZAIAccounts(accounts, results, now)
			if len(got) != len(accounts) || got[0].ID != test.wantFirst {
				t.Fatalf("order = %v, want first %q", zaiTestAccountIDs(got), test.wantFirst)
			}
			if !reflect.DeepEqual(accounts, before) {
				t.Fatal("account input mutated")
			}
		})
	}
}

func TestPrioritiseZAIAccountsStablePartitionAndErrors(t *testing.T) {
	accounts := []zaiprov.Account{{ID: "exhausted"}, {ID: "unknown"}, {ID: "healthy"}, {ID: "expired"}, {ID: "fetch-failed"}, {ID: "also-exhausted"}}
	results := []quota.Result{
		{AccountID: "exhausted", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: 2000}, quota.Window7Day: {RemainingPct: 80, ResetAtUnix: 4000}}},
		{AccountID: "healthy", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 50}}},
		{AccountID: "expired", Status: quota.StatusError, Error: &quota.ErrorInfo{Code: "auth_expired"}},
		{AccountID: "fetch-failed", Status: quota.StatusError, Error: &quota.ErrorInfo{Code: "fetch_error"}},
		{AccountID: "also-exhausted", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 2000}, quota.Window7Day: {RemainingPct: 0, ResetAtUnix: 4000}}},
		{AccountID: "another-pool", Email: "unknown", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}},
		{Status: quota.StatusError},
	}
	got := PrioritiseZAIAccounts(accounts, results, time.Unix(1000, 0))
	want := []string{"unknown", "healthy", "fetch-failed", "exhausted", "expired", "also-exhausted"}
	if !reflect.DeepEqual(zaiTestAccountIDs(got), want) {
		t.Fatalf("order = %v, want %v", zaiTestAccountIDs(got), want)
	}
	if accounts[0].ID != "exhausted" || results[0].Windows[quota.Window5Hour].RemainingPct != 0 {
		t.Fatal("inputs mutated")
	}
}

func TestPrioritiseZAIAccountsRetainsAllExhaustedAndUnknown(t *testing.T) {
	accounts, _ := zaiTestAccounts()
	results := []quota.Result{
		{AccountID: "first", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}},
		{AccountID: "second", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 0}}},
	}
	for _, test := range []struct {
		name     string
		accounts []zaiprov.Account
		results  []quota.Result
	}{
		{"all exhausted", accounts, results},
		{"unknown", accounts, nil},
		{"empty", nil, results},
		{"missing windows", accounts, []quota.Result{{AccountID: "first", Status: quota.StatusExhausted}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := PrioritiseZAIAccounts(test.accounts, test.results, time.Unix(1000, 0))
			if !reflect.DeepEqual(zaiTestAccountIDs(got), zaiTestAccountIDs(test.accounts)) {
				t.Fatalf("order = %v, want %v", zaiTestAccountIDs(got), zaiTestAccountIDs(test.accounts))
			}
		})
	}
}

func zaiTestAccountIDs(accounts []zaiprov.Account) []string {
	ids := make([]string, len(accounts))
	for i, account := range accounts {
		ids[i] = account.ID
	}
	return ids
}

func zaiTestFloatPointer(value float64) *float64 { return &value }

func TestZAIMultiAccountQuotaOrderingPreservesOpaqueContinuation(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous=%t", previous), func(t *testing.T) {
			var seen string
			s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts,
				ZAIQuota: func() []quota.Result {
					return []quota.Result{{AccountID: "first", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: time.Now().Add(time.Hour).Unix()}}}}
				}, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					seen = r.Header.Get("Authorization")
					return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
				})}
			body := `{"model":"glm-5.3-flash","input":[]}`
			if previous {
				body = `{"model":"glm-5.3-flash","input":[],"previous_response_id":"opaque"}`
			}
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)))
			want := "Bearer second-key"
			if previous {
				want = "Bearer first-key"
			}
			if w.Code != http.StatusNoContent || seen != want {
				t.Fatalf("status=%d account=%q want=%q", w.Code, seen, want)
			}
		})
	}
}

func TestZAIMultiAccountHTTPFailover(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		previous bool
		want     []string
		wantCode int
	}{
		{"quota rejected", http.StatusTooManyRequests, false, []string{"Bearer first-key", "Bearer second-key"}, http.StatusOK},
		{"auth rejected", http.StatusUnauthorized, false, []string{"Bearer first-key", "Bearer second-key"}, http.StatusOK},
		{"forbidden", http.StatusForbidden, false, []string{"Bearer first-key", "Bearer second-key"}, http.StatusOK},
		{"server failure", http.StatusInternalServerError, false, []string{"Bearer first-key"}, http.StatusInternalServerError},
		{"bad request", http.StatusBadRequest, false, []string{"Bearer first-key"}, http.StatusBadRequest},
		{"opaque continuation", http.StatusTooManyRequests, true, []string{"Bearer first-key"}, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("Authorization")
				mu.Lock()
				seen = append(seen, key)
				mu.Unlock()
				body, err := httputil.ReadBody(r.Body)
				if err != nil || !strings.Contains(string(body), `"input":"keep input"`) {
					t.Error("request input was not replayed")
				}
				if r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("Cookie") != "" {
					t.Error("client credentials crossed provider boundary")
				}
				if key == "Bearer first-key" {
					w.WriteHeader(test.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"second-response","output":[]}`)
			}))
			defer upstream.Close()
			s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAIUpstream: upstream.URL, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
			body := `{"model":"glm-5.3-flash","input":"keep input"}`
			if test.previous {
				body = `{"model":"glm-5.3-flash","input":"keep input","previous_response_id":"account-owned-response"}`
			}
			r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body))
			r.Header.Set("Chatgpt-Account-Id", "private-account")
			r.Header.Set("Cookie", "private-cookie")
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, r)
			if w.Code != test.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, test.wantCode)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(seen, test.want) {
				t.Fatalf("attempts = %v, want %v", seen, test.want)
			}
		})
	}
}

func TestZAIMultiAccountAllExhaustedAttemptsEachKeyOnce(t *testing.T) {
	var attempts atomic.Int32
	var closed atomic.Int32
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{APIKey: "first-key"}, {APIKey: "first-key"}, {APIKey: "second-key"}, {}}, nil
	}, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"60"}}, Body: &zaiClosedBody{Reader: strings.NewReader("quota exhausted"), closed: &closed}}, nil
	})}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" || attempts.Load() != 2 || closed.Load() != 2 {
		t.Fatalf("status=%d retry-after=%q attempts=%d closed=%d", w.Code, w.Header().Get("Retry-After"), attempts.Load(), closed.Load())
	}
}

type zaiClosedBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *zaiClosedBody) Close() error { b.closed.Add(1); return nil }

func TestZAIMultiAccountDoesNotReplayAmbiguousTransportFailure(t *testing.T) {
	var attempts atomic.Int32
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("private upstream error")
	})}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
	if w.Code != http.StatusBadGateway || attempts.Load() != 1 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("status=%d attempts=%d response=%s", w.Code, attempts.Load(), w.Body)
	}
}

func TestZAIMultiAccountHTTPDoesNotReplayStartedStream(t *testing.T) {
	var attempts atomic.Int32
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"started\"}}\n\ndata: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\"}}\n\n"))}, nil
	})}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
	if w.Code != http.StatusOK || attempts.Load() != 1 || !strings.Contains(w.Body.String(), "started") {
		t.Fatalf("status=%d attempts=%d response=%s", w.Code, attempts.Load(), w.Body)
	}
}

func TestZAIMultiAccountCredentialsReloadBetweenRequests(t *testing.T) {
	var accounts atomic.Pointer[[]zaiprov.Account]
	initial := []zaiprov.Account{{ID: "first", APIKey: "first-key"}}
	accounts.Store(&initial)
	var attempts atomic.Int32
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: func() ([]zaiprov.Account, error) {
		return *accounts.Load(), nil
	}, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		status := http.StatusTooManyRequests
		if r.Header.Get("Authorization") == "Bearer second-key" {
			status = http.StatusNoContent
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	request := func() int {
		w := httptest.NewRecorder()
		s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
		return w.Code
	}
	if got := request(); got != http.StatusTooManyRequests {
		t.Fatalf("initial status = %d", got)
	}
	added, _ := zaiTestAccounts()
	accounts.Store(&added)
	if got := request(); got != http.StatusNoContent || attempts.Load() != 3 {
		t.Fatalf("hot-added status = %d, attempts = %d", got, attempts.Load())
	}
}

func TestZAIMultiAccountWebSocketFailoverAndToolHistory(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	var accounts atomic.Pointer[[]zaiprov.Account]
	initial := []zaiprov.Account{{ID: "first", APIKey: "first-key"}}
	accounts.Store(&initial)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input    []json.RawMessage `json:"input"`
			Previous json.RawMessage   `json:"previous_response_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Previous != nil {
			t.Error("account-owned response ID reached upstream")
		}
		switch r.Header.Get("Authorization") {
		case "Bearer first-key":
			if firstCalls.Add(1) == 1 {
				writeZAITestSSE(w, "first-response", []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"}`)})
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		case "Bearer second-key":
			secondCalls.Add(1)
			if len(body.Input) != 3 || !strings.Contains(string(body.Input[1]), "function_call") || !strings.Contains(string(body.Input[2]), "function_call_output") {
				t.Errorf("tool history missing: %s", body.Input)
			}
			writeZAITestSSE(w, "second-response", nil)
		default:
			t.Error("unexpected credential")
		}
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAccounts: func() ([]zaiprov.Account, error) {
		return *accounts.Load(), nil
	}, ZAIUpstream: upstream.URL}
	c := openZAITestWebSocket(t, s, nil)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[{"role":"user","content":"read fixture"}]}`))
	if got := readZAITestCompletion(t, c); got.Response.ID != "first-response" {
		t.Fatalf("first response = %q", got.Response.ID)
	}
	added, _ := zaiTestAccounts()
	accounts.Store(&added)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","previous_response_id":"first-response","input":[{"type":"function_call_output","call_id":"call-1","output":"fixture"}]}`))
	if got := readZAITestCompletion(t, c); got.Response.ID != "second-response" || firstCalls.Load() != 2 || secondCalls.Load() != 1 {
		t.Fatalf("second response = %q, calls = %d/%d", got.Response.ID, firstCalls.Load(), secondCalls.Load())
	}
}

func TestZAIMultiAccountWebSocketRejectedRequests(t *testing.T) {
	for _, test := range []struct {
		name         string
		firstStatus  int
		lastStatus   int
		wantAttempts int32
	}{
		{"auth failover", http.StatusUnauthorized, http.StatusOK, 2},
		{"all exhausted", http.StatusTooManyRequests, http.StatusTooManyRequests, 2},
		{"no server error replay", http.StatusInternalServerError, http.StatusOK, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			s := &Server{Config: &Config{ClaudeUpstream: "https://claude.test"}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := test.firstStatus
				if attempts.Add(1) > 1 {
					status = test.lastStatus
				}
				body := "private rejected response"
				if status == http.StatusOK {
					body = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"second-response\",\"output\":[]}}\n\n"
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			c := openZAITestWebSocket(t, s, nil)
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[]}`))
			_, payload, err := c.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Type   string `json:"type"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			wantType, wantStatus := "error", test.firstStatus
			if test.wantAttempts == 2 {
				wantStatus = test.lastStatus
			}
			if wantStatus == http.StatusOK {
				wantType, wantStatus = "response.completed", 0
			}
			if event.Type != wantType || event.Status != wantStatus || attempts.Load() != test.wantAttempts || strings.Contains(string(payload), "private") {
				t.Fatalf("event=%s attempts=%d", payload, attempts.Load())
			}
		})
	}
}

func TestZAIMultiAccountReaderFailureDoesNotUseLegacyKey(t *testing.T) {
	for _, test := range []struct {
		name     string
		accounts []zaiprov.Account
		err      error
	}{
		{"reader error", nil, errors.New("private credential details")},
		{"empty accounts", nil, nil},
		{"blank key", []zaiprov.Account{{APIKey: " "}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: func() ([]zaiprov.Account, error) {
				return test.accounts, test.err
			}, ZAIAPIKey: func() (string, error) {
				t.Error("multi-account failure fell back to legacy key")
				return "private-key", nil
			}, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Error("invalid credentials reached upstream")
				return nil, errors.New("unexpected request")
			})}
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("status=%d response=%s", w.Code, w.Body)
			}
		})
	}
}

func TestZAIMultiAccountWebSocketDoesNotReplayStartedStream(t *testing.T) {
	var attempts atomic.Int32
	s := &Server{Config: &Config{ClaudeUpstream: "https://claude.test"}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"started\"}}\n\ndata: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\"}}\n\n"))}, nil
	})}
	c := openZAITestWebSocket(t, s, nil)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[]}`))
	for i := 0; i < 2; i++ {
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.ReadMessage(); err == nil || attempts.Load() != 1 {
		t.Fatalf("started stream retried: error=%v attempts=%d", err, attempts.Load())
	}
}

func TestZAIMultiAccountCancelledRequestDoesNotTryNextKey(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAccounts: zaiTestAccounts, ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		cancel()
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)).WithContext(ctx)
	s.handleCodexResponsesRoute(w, r)
	if attempts.Load() != 1 {
		t.Fatalf("cancelled request attempts=%d", attempts.Load())
	}
}
