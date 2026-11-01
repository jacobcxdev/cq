package zai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/quota"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchQuota(t *testing.T) {
	const body = `{"code":200,"success":true,"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":12.5,"nextResetTime":1793574000000},{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":99.1},{"type":"TIME_LIMIT","unit":5,"number":1,"usage":100,"currentValue":20}]}}`
	p := New(doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://api.z.ai/api/monitor/usage/quota/limit" {
			t.Fatalf("unexpected quota request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "test-key" {
			t.Fatal("expected raw API key authorisation")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}), func() (string, error) { return "test-key", nil })
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(results) != 1 {
		t.Fatalf("results=%d, error=%v", len(results), err)
	}
	r := results[0]
	if r.Status != quota.StatusOK || r.AccountID == "" || !r.Active || r.Plan != "Max" {
		t.Fatalf("unexpected result: %#v", r)
	}
	w := r.Windows[quota.Window5Hour]
	if w.RemainingPct != 88 || w.RemainingPctExact == nil || *w.RemainingPctExact != 87.5 || w.ResetAtUnix != 1793574000 {
		t.Fatalf("unexpected 5h window: %#v", w)
	}
	if r.Windows[quota.Window7Day].RemainingPct != 1 || r.Windows[quota.Window7Day].ResetAtUnix != 0 {
		t.Fatalf("unexpected weekly window: %#v", r.Windows)
	}
	if r.Windows[quota.WindowName("monthly:mcp")].RemainingPct != 80 {
		t.Fatalf("unexpected monthly MCP window: %#v", r.Windows)
	}
}

func TestFetchErrorsDoNotExposeCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, body, wantCode string
		status               int
		requestErr           error
		keyErr               error
		key                  string
		panicFetch           bool
	}{
		{name: "missing credentials", wantCode: "not_configured"},
		{name: "credential failure", keyErr: errors.New("private-key"), wantCode: "fetch_error"},
		{name: "network failure", key: "test-key", requestErr: errors.New("private-key"), wantCode: "fetch_error"},
		{name: "unauthorised", key: "test-key", status: 401, body: "private-key", wantCode: "auth_expired"},
		{name: "forbidden", key: "test-key", status: 403, body: "private-key", wantCode: "auth_expired"},
		{name: "API failure", key: "test-key", status: 503, body: "private-key", wantCode: "api_error"},
		{name: "business failure", key: "test-key", status: 200, body: `{"code":500,"msg":"private-key","data":{"limits":[]}}`, wantCode: "api_error"},
		{name: "unsuccessful", key: "test-key", status: 200, body: `{"code":200,"success":false,"msg":"private-key"}`, wantCode: "api_error"},
		{name: "malformed JSON", key: "test-key", status: 200, body: "private-key", wantCode: "parse_error"},
		{name: "empty limits", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[]}}`, wantCode: "parse_error"},
		{name: "only MCP limits", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"percentage":0}]}}`, wantCode: "parse_error"},
		{name: "missing percentage", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3}]}}`, wantCode: "parse_error"},
		{name: "invalid percentage", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":"private-key"}]}}`, wantCode: "parse_error"},
		{name: "duplicate window", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":10},{"type":"TOKENS_LIMIT","unit":3,"percentage":20}]}}`, wantCode: "parse_error"},
		{name: "unknown limits", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"FUTURE_LIMIT","unit":3,"percentage":0}]}}`, wantCode: "parse_error"},
		{name: "different hours", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":2,"percentage":0}]}}`, wantCode: "parse_error"},
		{name: "different weeks", key: "test-key", status: 200, body: `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":6,"number":2,"percentage":0}]}}`, wantCode: "parse_error"},
		{name: "oversized response", key: "test-key", status: 200, body: strings.Repeat("x", httputil.MaxResponseBody+1), wantCode: "fetch_error"},
		{name: "provider panic", key: "test-key", panicFetch: true, wantCode: "fetch_panic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New(doerFunc(func(r *http.Request) (*http.Response, error) {
				if tt.panicFetch {
					panic("private-key")
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}, tt.requestErr
			}), func() (string, error) { return tt.key, tt.keyErr })
			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil || len(results) != 1 || results[0].Error == nil || results[0].Error.Code != tt.wantCode {
				t.Fatalf("results=%#v, error=%v; expected code %s", results, err, tt.wantCode)
			}
			if strings.Contains(results[0].Error.Message, "private-key") {
				t.Fatal("credential leaked through quota error")
			}
		})
	}
}

func TestQuotaPercentageEdges(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		remaining  int
		status     quota.Status
	}{
		{"zero", `{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":0}]}}`, 100, quota.StatusOK},
		{"absent code", `{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":0}]}}`, 100, quota.StatusOK},
		{"null code", `{"code":null,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":0}]}}`, 100, quota.StatusOK},
		{"exhausted", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":100}]}}`, 0, quota.StatusExhausted},
		{"clamp high", `{"code":"200","data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":120}]}}`, 0, quota.StatusExhausted},
		{"clamp low", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":-5}]}}`, 100, quota.StatusOK},
		{"ignore unknown", `{"code":200,"data":{"limits":[null,{"type":"FUTURE_LIMIT","unit":99,"percentage":50},{"type":"TOKENS_LIMIT","unit":3,"percentage":20}]}}`, 80, quota.StatusOK},
		{"MCP exhaustion does not exhaust tokens", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":20},{"type":"TIME_LIMIT","unit":5,"percentage":100}]}}`, 80, quota.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New(doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			}), func() (string, error) { return "test-key", nil })
			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil || len(results) != 1 || results[0].Status != tt.status || results[0].Windows[quota.Window5Hour].RemainingPct != tt.remaining {
				t.Fatalf("results=%#v, error=%v", results, err)
			}
		})
	}
}
