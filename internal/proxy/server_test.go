package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	claude "github.com/jacobcxdev/cq/internal/provider/claude"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestServer_HealthEndpoint(t *testing.T) {
	srv := &Server{
		Config: &Config{
			Port:           0,
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "test-token",
		},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "a@test.com"}, {Email: "b@test.com"}}
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	srv.handleHealth(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	accounts := resp["accounts"].(map[string]any)
	if accounts["claude"].(float64) != 2 {
		t.Errorf("claude accounts = %v, want 2", accounts["claude"])
	}
}

type diagnosticsControllerTestWriter struct {
	header        http.Header
	statuses      []int
	body          []byte
	flushed       bool
	writeDeadline time.Time
}

func (w *diagnosticsControllerTestWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *diagnosticsControllerTestWriter) Write(b []byte) (int, error) {
	if !w.hasFinalStatus() {
		w.statuses = append(w.statuses, http.StatusOK)
	}
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *diagnosticsControllerTestWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

func (w *diagnosticsControllerTestWriter) Flush() {
	w.flushed = true
}

func (w *diagnosticsControllerTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

func (w *diagnosticsControllerTestWriter) hasFinalStatus() bool {
	for _, status := range w.statuses {
		if status >= 200 {
			return true
		}
	}
	return false
}

func TestDiagnosticsResponseWriterRecordsFinalNonInformationalStatus(t *testing.T) {
	underlying := &diagnosticsControllerTestWriter{}
	rec := &diagnosticsResponseWriter{ResponseWriter: underlying}

	rec.WriteHeader(http.StatusEarlyHints)
	rec.WriteHeader(http.StatusAccepted)
	rec.WriteHeader(http.StatusInternalServerError)

	if got := rec.statusCode(); got != http.StatusAccepted {
		t.Fatalf("statusCode = %d, want %d", got, http.StatusAccepted)
	}
	wantStatuses := []int{http.StatusEarlyHints, http.StatusAccepted, http.StatusInternalServerError}
	if fmt.Sprint(underlying.statuses) != fmt.Sprint(wantStatuses) {
		t.Fatalf("underlying statuses = %v, want %v", underlying.statuses, wantStatuses)
	}

	underlying = &diagnosticsControllerTestWriter{}
	rec = &diagnosticsResponseWriter{ResponseWriter: underlying}
	rec.WriteHeader(http.StatusEarlyHints)
	if _, err := rec.Write([]byte("ok")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := rec.statusCode(); got != http.StatusOK {
		t.Fatalf("statusCode after informational then Write = %d, want %d", got, http.StatusOK)
	}
}

func TestDiagnosticsResponseWriterUnwrapsForResponseController(t *testing.T) {
	underlying := &diagnosticsControllerTestWriter{}
	wrapped, rec := (&Server{Diag: &DiagnosticsWriter{}}).wrapDiagnosticsResponseWriter(underlying)
	if rec == nil {
		t.Fatal("recorder is nil")
	}
	if _, ok := wrapped.(http.Flusher); !ok {
		t.Fatal("wrapped writer does not preserve http.Flusher")
	}

	deadline := time.Unix(123, 0).UTC()
	if err := http.NewResponseController(wrapped).SetWriteDeadline(deadline); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if !underlying.writeDeadline.Equal(deadline) {
		t.Fatalf("write deadline = %v, want %v", underlying.writeDeadline, deadline)
	}
	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !underlying.flushed {
		t.Fatal("underlying writer was not flushed")
	}
}

func TestServerDiagnosticsClaudeRouteEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	future := time.Now().UnixMilli() + 3600_000
	claudeAccount := keyring.ClaudeOAuth{Email: "user@test.com", AccountUUID: "account-uuid-secret", AccessToken: "real-token", ExpiresAt: future}
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		claudeAccount,
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream:      "https://api.anthropic.com",
			LocalToken:          "local-tok",
			PinnedClaudeAccount: "user@test.com",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg_123"}`), nil
			}),
		},
		Diag: diag,
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "raw-session-secret")
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodPost || ev.Path != "/v1/messages" || ev.Provider != "claude" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "anthropic_messages" {
		t.Fatalf("RouteKind = %q, want anthropic_messages", ev.RouteKind)
	}
	if ev.Model != "claude-sonnet" {
		t.Fatalf("Model = %q, want claude-sonnet", ev.Model)
	}
	if !ev.PinActive {
		t.Fatal("PinActive = false, want true")
	}
	if ev.AccountHint != claudeAccountHint(&claudeAccount) {
		t.Fatalf("AccountHint = %q, want redacted hint %q", ev.AccountHint, claudeAccountHint(&claudeAccount))
	}
	if ev.Failover {
		t.Fatal("Failover = true, want false")
	}
	if ev.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", ev.StatusCode)
	}
	if ev.Time.IsZero() {
		t.Fatal("Time is zero")
	}
	if ev.SessionSource != "x-claude-code-session-id" {
		t.Fatalf("SessionSource = %q, want x-claude-code-session-id", ev.SessionSource)
	}
	if ev.SessionKey == "" || !strings.HasPrefix(ev.SessionKey, "claude-session:") {
		t.Fatalf("SessionKey = %q, want claude-session:<hash>", ev.SessionKey)
	}
	assertDiagnosticsLogDoesNotContain(t, path, "raw-session-secret")
	assertDiagnosticsLogDoesNotContain(t, path, "local-tok")
	assertDiagnosticsLogDoesNotContain(t, path, "user@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "account-uuid-secret")
	assertDiagnosticsLogDoesNotContain(t, path, "real-token")
}

func TestServerDiagnosticsClaudeRouteRecordsFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "primary@test.com", AccountUUID: "primary-uuid", AccessToken: "primary-token", ExpiresAt: future},
		{Email: "fallback@test.com", AccountUUID: "fallback-uuid", AccessToken: "fallback-token", ExpiresAt: future},
	}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: &fakeSelector{accounts: accounts},
			Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.Header.Get("Authorization") {
				case "Bearer primary-token":
					return makeResponse(http.StatusTooManyRequests, `{"error":"rate_limited"}`), nil
				case "Bearer fallback-token":
					return makeResponse(http.StatusOK, `{"id":"msg_456"}`), nil
				default:
					t.Fatalf("unexpected Authorization = %q", req.Header.Get("Authorization"))
					return nil, nil
				}
			}),
		},
		Diag: diag,
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.AccountHint != claudeAccountHint(&accounts[1]) {
		t.Fatalf("AccountHint = %q, want fallback hint %q", ev.AccountHint, claudeAccountHint(&accounts[1]))
	}
	if !ev.Failover {
		t.Fatal("Failover = false, want true")
	}
	assertDiagnosticsLogDoesNotContain(t, path, "primary@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "primary-uuid")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback-uuid")
	assertDiagnosticsLogDoesNotContain(t, path, "primary-token")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback-token")
}

func TestServerDiagnosticsClaudeTransportFailureEmitsSafeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	future := time.Now().UnixMilli() + 3600_000
	acct := keyring.ClaudeOAuth{Email: "error@test.com", AccountUUID: "error-uuid", AccessToken: "error-token", ExpiresAt: future}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: &fakeSelector{accounts: []keyring.ClaudeOAuth{acct}},
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("dial failed for error-token")
			}),
		},
		Diag: diag,
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Error != "api_error:upstream_error" {
		t.Fatalf("Error = %q, want safe upstream error code", ev.Error)
	}
	if ev.AccountHint != claudeAccountHint(&acct) {
		t.Fatalf("AccountHint = %q, want redacted hint %q", ev.AccountHint, claudeAccountHint(&acct))
	}
	assertDiagnosticsLogDoesNotContain(t, path, "error@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "error-uuid")
	assertDiagnosticsLogDoesNotContain(t, path, "error-token")
}

func TestServerDiagnosticsClaudeRouteReadsLiveSelectorPin(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "fallback@test.com", AccountUUID: "uuid-fallback", AccessToken: "fallback-token", ExpiresAt: future},
		{Email: "pinned@test.com", AccountUUID: "uuid-pin", AccessToken: "pinned-token", ExpiresAt: future},
	}

	for _, tc := range []struct {
		name      string
		configPin string
		livePin   string
		quota     QuotaReader
		wantPin   bool
	}{
		{
			name:    "set by config reload",
			livePin: "pinned@test.com",
			wantPin: true,
		},
		{
			name:      "cleared by config reload",
			configPin: "pinned@test.com",
			wantPin:   false,
		},
		{
			name:      "cleared by automatic expiry",
			configPin: "pinned@test.com",
			livePin:   "pinned@test.com",
			quota: stubQuotaReader{
				"uuid-pin": {
					Result: quota.Result{
						Status: quota.StatusExhausted,
						Windows: map[quota.WindowName]quota.Window{
							"5h": {RemainingPct: 0},
						},
					},
					FetchedAt: time.Now(),
				},
			},
			wantPin: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			diag, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatalf("OpenDiagnosticsWriter: %v", err)
			}
			defer diag.Close()

			inner := innerSelectorFunc(func(_ context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
				excludeSet := make(map[string]bool, len(exclude))
				for _, e := range exclude {
					excludeSet[e] = true
				}
				for i := range accounts {
					acct := &accounts[i]
					if isExcluded(acct, excludeSet) {
						continue
					}
					result := *acct
					return &result, nil
				}
				return nil, fmt.Errorf("no accounts available")
			})
			selector := NewPinnedClaudeSelector(inner, func() []keyring.ClaudeOAuth { return accounts }, tc.livePin, tc.quota)
			srv := &Server{
				Config: &Config{
					ClaudeUpstream:      "https://api.anthropic.com",
					LocalToken:          "local-tok",
					PinnedClaudeAccount: tc.configPin,
				},
				Selector: selector,
				Transport: &TokenTransport{
					Selector: selector,
					Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
						return makeResponse(http.StatusOK, `{"id":"msg_123"}`), nil
					}),
				},
				Diag: diag,
			}

			handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
			req.Header.Set("Authorization", "Bearer local-tok")
			req.Header.Set("Content-Type", "application/json")
			handler(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}
			if err := diag.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			events := readDiagnosticsEvents(t, path)
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].PinActive != tc.wantPin {
				t.Fatalf("PinActive = %v, want %v; event = %+v", events[0].PinActive, tc.wantPin, events[0])
			}
		})
	}
}

func TestServerDiagnosticsCodexRouteEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	codexAccount := codex.CodexAccount{Email: "codex-user@test.com", AccountID: "codex-account-secret", AccessToken: "codex-tok"}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com/backend-api/codex",
			LocalToken:     "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codexAccount},
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123"}`)),
				}, nil
			}),
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, codexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodPost || ev.Path != codexResponsesPath || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "codex_native" {
		t.Fatalf("RouteKind = %q, want codex_native", ev.RouteKind)
	}
	if ev.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", ev.Model)
	}
	if ev.StatusCode != http.StatusAccepted {
		t.Fatalf("StatusCode = %d, want 202", ev.StatusCode)
	}
	if ev.AccountHint != codexAccountHint(&codexAccount) {
		t.Fatalf("AccountHint = %q, want redacted hint %q", ev.AccountHint, codexAccountHint(&codexAccount))
	}
	if ev.Failover {
		t.Fatal("Failover = true, want false")
	}
	assertDiagnosticsLogDoesNotContain(t, path, "codex-user@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "codex-account-secret")
	assertDiagnosticsLogDoesNotContain(t, path, "codex-tok")
}

func TestServerDiagnosticsCodexRouteRecordsFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	accounts := []codex.CodexAccount{
		{Email: "primary-codex@test.com", AccountID: "primary-codex-account", AccessToken: "primary-codex-token"},
		{Email: "fallback-codex@test.com", AccountID: "fallback-codex-account", AccessToken: "fallback-codex-token"},
	}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com/backend-api/codex",
			LocalToken:     "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &multiCodexSelector{accounts: accounts},
			Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.Header.Get("Authorization") {
				case "Bearer primary-codex-token":
					return makeResponse(http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`), nil
				case "Bearer fallback-codex-token":
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"id":"resp_456"}`)),
					}, nil
				default:
					t.Fatalf("unexpected Authorization = %q", req.Header.Get("Authorization"))
					return nil, nil
				}
			}),
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, codexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.AccountHint != codexAccountHint(&accounts[1]) {
		t.Fatalf("AccountHint = %q, want fallback hint %q", ev.AccountHint, codexAccountHint(&accounts[1]))
	}
	if !ev.Failover {
		t.Fatal("Failover = false, want true")
	}
	assertDiagnosticsLogDoesNotContain(t, path, "primary-codex@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback-codex@test.com")
	assertDiagnosticsLogDoesNotContain(t, path, "primary-codex-account")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback-codex-account")
	assertDiagnosticsLogDoesNotContain(t, path, "primary-codex-token")
	assertDiagnosticsLogDoesNotContain(t, path, "fallback-codex-token")
}

func TestServerDiagnosticsCodexNoTransportEmitsSafeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com/backend-api/codex",
			LocalToken:     "local-token-secret",
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, codexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Error != "api_error:no_codex_accounts" {
		t.Fatalf("Error = %q, want no account code", ev.Error)
	}
	if ev.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want 503", ev.StatusCode)
	}
	assertDiagnosticsLogDoesNotContain(t, path, "local-token-secret")
}

func TestServerDiagnosticsCountTokensRouteEmitsEvents(t *testing.T) {
	for _, tc := range []struct {
		name         string
		model        string
		wantProvider string
		wantBody     string
	}{
		{name: "claude", model: "claude-sonnet-4-6", wantProvider: "claude", wantBody: `{"input_tokens":321}`},
		{name: "codex", model: "gpt-5.4", wantProvider: "codex", wantBody: `{"input_tokens":123}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			diag, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatalf("OpenDiagnosticsWriter: %v", err)
			}
			defer diag.Close()

			codexTransport := &CodexTokenTransport{
				Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct"}},
				Inner: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if tc.wantProvider != "codex" {
						t.Fatal("codex upstream should not be called")
					}
					if !strings.HasSuffix(r.URL.Path, "/v1/responses/input_tokens") {
						t.Fatalf("codex path = %q, want suffix /v1/responses/input_tokens", r.URL.Path)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":123}`)),
					}, nil
				}),
			}
			srv := &Server{
				Config: &Config{
					ClaudeUpstream: "https://api.anthropic.com",
					CodexUpstream:  "https://chatgpt.com/backend-api/codex",
					LocalToken:     "local-tok",
				},
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if tc.wantProvider != "claude" {
						t.Fatal("claude upstream should not be called")
					}
					if r.URL.Path != countTokensPath {
						t.Fatalf("claude path = %q, want %q", r.URL.Path, countTokensPath)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(tc.wantBody)),
					}, nil
				}),
				CodexTransport: codexTransport,
				Diag:           diag,
			}

			handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tc.model)
			req := httptest.NewRequest(http.MethodPost, countTokensPath, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-tok")
			req.Header.Set("Content-Type", "application/json")
			handler(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}
			if strings.TrimSpace(w.Body.String()) != tc.wantBody {
				t.Fatalf("body = %s, want %s", w.Body.String(), tc.wantBody)
			}
			if err := diag.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			events := readDiagnosticsEvents(t, path)
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			ev := events[0]
			if ev.Method != http.MethodPost || ev.Path != countTokensPath || ev.Provider != tc.wantProvider {
				t.Fatalf("event route = %+v", ev)
			}
			if ev.RouteKind != "anthropic_count_tokens" {
				t.Fatalf("RouteKind = %q, want anthropic_count_tokens", ev.RouteKind)
			}
			if ev.Model != tc.model {
				t.Fatalf("Model = %q, want %s", ev.Model, tc.model)
			}
			if ev.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, want 200", ev.StatusCode)
			}
			assertDiagnosticsLogDoesNotContain(t, path, "local-tok")
		})
	}
}

func TestServerDiagnosticsLegacyCodexRouteEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	const localToken = "secret-proxy-token"
	var gotPath string
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com",
			LocalToken:     localToken,
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotPath = r.URL.Path
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_legacy"}`)),
				}, nil
			}),
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, legacyCodexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/responses" {
		t.Fatalf("upstream path = %q, want /responses", gotPath)
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodPost || ev.Path != legacyCodexResponsesPath || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "codex_native" {
		t.Fatalf("RouteKind = %q, want codex_native", ev.RouteKind)
	}
	if ev.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", ev.Model)
	}
	if ev.StatusCode != http.StatusCreated {
		t.Fatalf("StatusCode = %d, want 201", ev.StatusCode)
	}
	assertDiagnosticsLogDoesNotContain(t, path, localToken)
}

func TestServerDiagnosticsLegacyCodexWebsocketRouteEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %q, want /responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-tok" {
			t.Errorf("upstream auth = %q, want Bearer codex-tok", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		if string(message) != "ping" {
			t.Errorf("upstream message = %q, want ping", message)
		}
		if err := conn.WriteMessage(messageType, []byte("pong")); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "local-tok",
		},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + legacyCodexResponsesPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, message, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	} else if string(message) != "pong" {
		t.Fatalf("message = %q, want pong", message)
	}
	_ = conn.Close()

	events := waitForDiagnosticsEvents(t, path, 1)
	ev := events[0]
	if ev.Method != http.MethodGet || ev.Path != legacyCodexResponsesPath || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "codex_legacy_websocket" {
		t.Fatalf("RouteKind = %q, want codex_legacy_websocket", ev.RouteKind)
	}
	if ev.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("StatusCode = %d, want 101", ev.StatusCode)
	}
	assertDiagnosticsLogDoesNotContain(t, path, "codex-tok")
}

func TestServerPayloadDiagnosticsLegacyCodexWebSocketFrameEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %q, want /responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-tok" {
			t.Errorf("upstream auth = %q, want Bearer codex-tok", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		if !strings.Contains(string(message), "response/create") {
			t.Errorf("upstream message = %q, want response/create frame", message)
		}
		if err := conn.WriteMessage(messageType, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct-codex"}},
			Inner:    http.DefaultTransport,
		},
		PayloadDiag: payloadDiag,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + legacyCodexResponsesPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"response/create","params":{"model":"gpt-5.5","previous_response_id":"resp_prev"}}`)
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Path != legacyCodexResponsesPath || ev.RouteKind != "codex_websocket_frame" || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.Model != "gpt-5.5" {
		t.Fatalf("Model = %q, want gpt-5.5", ev.Model)
	}
	if ev.SessionSource != "ws:previous_response_id" || ev.SessionSignal != "continuation" {
		t.Fatalf("source/signal = %q/%q, want ws:previous_response_id/continuation", ev.SessionSource, ev.SessionSignal)
	}
	if ev.SessionKey == "" || !strings.HasPrefix(ev.SessionKey, "ws-session:") {
		t.Fatalf("SessionKey = %q, want ws-session:<hash>", ev.SessionKey)
	}
	assertPayloadLogDoesNotContain(t, path, "codex-tok")
	assertPayloadLogDoesNotContain(t, path, "acct-codex")
}

func TestServerPayloadDiagnosticsCodexAppServerWebSocketFrameEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		if !strings.Contains(string(message), "thread/start") {
			t.Errorf("upstream message = %q, want thread/start frame", message)
		}
		if err := conn.WriteMessage(messageType, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct-codex"}},
			Inner:    http.DefaultTransport,
		},
		PayloadDiag: payloadDiag,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + codexAppServerPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"model":"gpt-5.5","thread_id":"thread-ws-1"}}`)
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Path != codexAppServerPath || ev.RouteKind != "codex_websocket_frame" || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.Model != "gpt-5.5" {
		t.Fatalf("Model = %q, want gpt-5.5", ev.Model)
	}
	if ev.SessionSource != "ws:thread_id" || ev.SessionSignal != "new_session" {
		t.Fatalf("source/signal = %q/%q, want ws:thread_id/new_session", ev.SessionSource, ev.SessionSignal)
	}
	if ev.SessionKey == "" || !strings.HasPrefix(ev.SessionKey, "ws-session:") {
		t.Fatalf("SessionKey = %q, want ws-session:<hash>", ev.SessionKey)
	}
	if ev.FrameIndex != 1 {
		t.Fatalf("FrameIndex = %d, want 1", ev.FrameIndex)
	}
	if string(ev.Body) != string(frame) {
		t.Fatalf("Body = %s, want raw frame %s", ev.Body, frame)
	}
	assertPayloadLogDoesNotContain(t, path, "codex-tok")
	assertPayloadLogDoesNotContain(t, path, "acct-codex")
}

func TestServerDiagnosticsCompactRoutesEmitEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "canonical", path: codexCompactResponsesPath},
		{name: "legacy", path: legacyCodexCompactResponsesPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			diag, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatalf("OpenDiagnosticsWriter: %v", err)
			}
			defer diag.Close()

			var gotPath string
			srv := &Server{
				Config: &Config{
					ClaudeUpstream: "https://api.anthropic.com",
					CodexUpstream:  "https://chatgpt.com",
					LocalToken:     "tok",
				},
				CodexTransport: &CodexTokenTransport{
					Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
					Inner: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						gotPath = r.URL.Path
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
						}, nil
					}),
				},
				Diag: diag,
			}

			handler, err := srv.handler()
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"gpt-5.4","previous_response_id":"resp_abc"}`))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("upstream path = %q, want /responses/compact", gotPath)
			}
			if err := diag.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			events := readDiagnosticsEvents(t, path)
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			ev := events[0]
			if ev.Method != http.MethodPost || ev.Path != tc.path || ev.Provider != "codex" {
				t.Fatalf("event route = %+v", ev)
			}
			if ev.RouteKind != "codex_compact" {
				t.Fatalf("RouteKind = %q, want codex_compact", ev.RouteKind)
			}
			if ev.Model != "gpt-5.4" {
				t.Fatalf("Model = %q, want gpt-5.4", ev.Model)
			}
			if ev.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, want 200", ev.StatusCode)
			}
		})
	}
}

func TestServerDiagnosticsLegacyCodexAppServerNonUpgradeRejectionEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	const localToken = "secret-proxy-token"
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com/backend-api/codex",
			LocalToken:     localToken,
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexAppServerPath, nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodGet || ev.Path != codexAppServerPath || ev.Provider != "codex" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "codex_app_server" {
		t.Fatalf("RouteKind = %q, want codex_app_server", ev.RouteKind)
	}
	if ev.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("StatusCode = %d, want 426", ev.StatusCode)
	}
	if ev.Error != "invalid_request_error:websocket_upgrade_required" {
		t.Fatalf("Error = %q, want websocket upgrade error code", ev.Error)
	}
	assertDiagnosticsLogDoesNotContain(t, path, localToken)
}

func TestServerDiagnosticsCodexAppServerInvalidUpstreamEmitsSafeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "ftp://chatgpt.example",
			LocalToken:     "local-token-secret",
		},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{Email: "codex@test.com", AccountID: "codex-account", AccessToken: "codex-token"}},
			Inner:    http.DefaultTransport,
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexAppServerPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Error != "api_error:invalid_codex_upstream" {
		t.Fatalf("Error = %q, want invalid upstream code", ev.Error)
	}
	if ev.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want 500", ev.StatusCode)
	}
	assertDiagnosticsLogDoesNotContain(t, path, "local-token-secret")
	assertDiagnosticsLogDoesNotContain(t, path, "codex-token")
}

func TestServerDiagnosticsHealthEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diag, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	defer diag.Close()

	const localToken = "secret-proxy-token"
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     localToken,
		},
		Diag: diag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := diag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodGet || ev.Path != "/health" || ev.Provider != "proxy" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "health" {
		t.Fatalf("RouteKind = %q, want health", ev.RouteKind)
	}
	if ev.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", ev.StatusCode)
	}
	assertDiagnosticsLogDoesNotContain(t, path, localToken)
}

func TestServerDiagnosticsDisabledNoEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg_123"}`), nil
			}),
		},
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("diagnostics file exists or stat failed: %v", err)
	}
}

func TestServerHealthReportsDiagnosticsEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			srv := &Server{}
			if tc.enabled {
				diag, err := OpenDiagnosticsWriter(path)
				if err != nil {
					t.Fatalf("OpenDiagnosticsWriter: %v", err)
				}
				defer diag.Close()
				srv.Diag = diag
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			srv.handleHealth(w, req)

			var resp struct {
				Diagnostics struct {
					Enabled bool `json:"enabled"`
				} `json:"diagnostics"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Diagnostics.Enabled != tc.enabled {
				t.Fatalf("diagnostics.enabled = %v, want %v", resp.Diagnostics.Enabled, tc.enabled)
			}
			if strings.Contains(w.Body.String(), path) {
				t.Fatalf("health leaked diagnostics path: %s", w.Body.String())
			}
		})
	}
}

func assertDiagnosticsLogDoesNotContain(t *testing.T, path, needle string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostics log: %v", err)
	}
	if strings.Contains(string(raw), needle) {
		t.Fatalf("diagnostics log leaked %q: %s", needle, raw)
	}
}

func waitForDiagnosticsEvents(t *testing.T, path string, want int) []RouteEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := readDiagnosticsEvents(t, path)
		if len(events) >= want {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %d, want at least %d", len(events), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServer_InvalidToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request should not reach upstream")
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "correct-token",
		},
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	handler(w, req)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "error" {
		t.Errorf("response type = %v, want error", resp["type"])
	}
}

func TestServer_ValidTokenForwardsRequest(t *testing.T) {
	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner:    http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local-tok",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if gotAuth != "Bearer real-token" {
		t.Errorf("upstream auth = %q, want Bearer real-token", gotAuth)
	}
	if gotBody != `{"model":"claude"}` {
		t.Errorf("upstream body = %q, want original", gotBody)
	}
}

func TestServer_SSEStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("no flusher")
		}
		for _, chunk := range []string{"data: hello\n\n", "data: world\n\n"} {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "tok", ExpiresAt: future},
	}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "tok",
		},
		Transport: &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "data: hello") || !strings.Contains(body, "data: world") {
		t.Errorf("SSE chunks not received: %q", body)
	}
}

func TestServer_NetworkError(t *testing.T) {
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "tok", ExpiresAt: time.Now().UnixMilli() + 3600_000},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}),
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "http://localhost:1",
			LocalToken:     "tok",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL("http://localhost:1"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler(w, req)

	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "error" {
		t.Errorf("response type = %v, want error", resp["type"])
	}
}

func TestServer_HeadroomPreservesOriginalModelRouting(t *testing.T) {
	claudeUpstreamCalled := false
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		claudeUpstreamCalled = true
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer claudeUpstream.Close()

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_headroom"}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
			`data: {"type":"response.completed","response":{"status":"completed"}}`,
			`data: [DONE]`,
		}, "\n\n")))
	}))
	defer codexUpstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: claudeUpstream.URL,
			CodexUpstream:  codexUpstream.URL,
			LocalToken:     "tok",
		},
		Headroom: fakeBridge(t, func(req headroomRequest) headroomResponse {
			if req.Model != "gpt-5.4" {
				t.Fatalf("bridge model = %q, want gpt-5.4", req.Model)
			}
			return headroomResponse{
				Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
				TokensSaved: 123,
			}
		}),
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler := srv.proxyHandler(mustParseURL(claudeUpstream.URL))
	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if claudeUpstreamCalled {
		t.Fatal("claude upstream should not be called for compressed codex model")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestServer_BodyReplay(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		auth := r.Header.Get("Authorization")
		if auth == "Bearer old-tok" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "old-tok", ExpiresAt: future, RefreshToken: "rt"},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Refresher: func(_ context.Context, _ httputil.Doer, _ string, _ []string) (*claude.RefreshResult, error) {
			return &claude.RefreshResult{AccessToken: "new-tok", ExpiresIn: 3600}, nil
		},
		Persister:   func(_ *keyring.ClaudeOAuth) {},
		RefreshHTTP: http.DefaultClient,
		Inner:       http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local",
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer local")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(bodies))
	}
	if bodies[0] != `{"model":"test"}` || bodies[1] != `{"model":"test"}` {
		t.Errorf("body not replayed correctly: %v", bodies)
	}
}

// ── isValidToken (dual-mode auth) ────────────────────────────────────────────

func TestServer_IsValidToken_LocalToken(t *testing.T) {
	srv := &Server{Config: &Config{LocalToken: "local-tok"}}
	if !srv.isValidToken("local-tok") {
		t.Error("local token should be valid")
	}
	if srv.isValidToken("wrong") {
		t.Error("wrong token should be invalid")
	}
}

func TestServer_IsValidToken_KnownOAuthToken(t *testing.T) {
	srv := &Server{
		Config: &Config{LocalToken: "local-tok"},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "a@test.com", AccessToken: "oauth-tok-a"},
				{Email: "b@test.com", AccessToken: "oauth-tok-b"},
			}
		},
	}
	if !srv.isValidToken("oauth-tok-a") {
		t.Error("known OAuth token A should be valid")
	}
	if !srv.isValidToken("oauth-tok-b") {
		t.Error("known OAuth token B should be valid")
	}
	if srv.isValidToken("unknown-tok") {
		t.Error("unknown token should be invalid")
	}
}

func TestServer_IsValidToken_NilDiscover(t *testing.T) {
	srv := &Server{Config: &Config{LocalToken: "local-tok"}}
	// Without Discover, only LocalToken works.
	if srv.isValidToken("some-oauth-tok") {
		t.Error("should reject OAuth token when Discover is nil")
	}
}

func TestServer_OAuthTokenForwardsRequest(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	transport := &TokenTransport{
		Selector: sel,
		Inner:    http.DefaultTransport,
	}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "local-tok",
		},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "user@test.com", AccessToken: "user-oauth-token"},
			}
		},
		Transport: transport,
	}

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	// Authenticate with the user's own OAuth token — NOT the local proxy token.
	req.Header.Set("Authorization", "Bearer user-oauth-token")
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// TokenTransport should have replaced the OAuth token with the selected account's token.
	if gotAuth != "Bearer real-token" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer real-token")
	}
}

// ── handleNativeCodex tests ─────────────────────────────────────────────────

func TestServer_NativeCodex_ForwardsWithAuth(t *testing.T) {
	var gotAuth, gotAcctID, gotBody, gotContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcctID = r.Header.Get("ChatGPT-Account-ID")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"resp_123","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-1",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","input":"hello","stream":false}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer codex-tok" {
		t.Errorf("upstream auth = %q, want Bearer codex-tok", gotAuth)
	}
	if gotAcctID != "acct-1" {
		t.Errorf("upstream account-id = %q, want acct-1", gotAcctID)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream content-type = %q, want application/json", gotContentType)
	}
	if gotBody != body {
		t.Errorf("upstream body = %q, want %q (no translation)", gotBody, body)
	}
	if !strings.Contains(w.Body.String(), "resp_123") {
		t.Errorf("response body should contain resp_123: %s", w.Body.String())
	}
}

func TestServer_Handler_CodexResponsesPath_ForwardsWithAuth(t *testing.T) {
	var gotPath, gotAuth, gotAcctID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAcctID = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_123"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct-1"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, codexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer codex-tok" {
		t.Errorf("upstream auth = %q, want Bearer codex-tok", gotAuth)
	}
	if gotAcctID != "acct-1" {
		t.Errorf("upstream account-id = %q, want acct-1", gotAcctID)
	}
}

func TestServer_Handler_LegacyCodexResponsesPost_Compatibility(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_legacy"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, legacyCodexResponsesPath, strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
}

func TestServer_Handler_CodexResponsesRejectsWebsocket(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexResponsesPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), codexAppServerPath) {
		t.Errorf("body = %q, want mention of %s", w.Body.String(), codexAppServerPath)
	}
}

func TestServer_Handler_AppServerRequiresWebsocket(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, codexAppServerPath, nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", w.Code)
	}
	if got := w.Header().Get("Upgrade"); got != "websocket" {
		t.Errorf("Upgrade header = %q, want websocket", got)
	}
}

func TestServer_AppServerDowngradesSparkForPlusAccount(t *testing.T) {
	var gotModel string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()

		if got := r.Header.Get("Authorization"); got != "Bearer plus-tok" {
			t.Errorf("upstream auth = %q, want Bearer plus-tok", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-plus" {
			t.Errorf("upstream account ID = %q, want acct-plus", got)
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		var payload struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Errorf("unmarshal websocket payload: %v", err)
			return
		}
		gotModel = payload.Params.Model
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "plus-tok", AccountID: "acct-plus", PlanType: "plus"}},
			Inner:    http.DefaultTransport,
		},
	}
	proxyHandler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + codexAppServerPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"model":"gpt-5.3-codex-spark"}}`)); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if gotModel != "gpt-5.3-codex" {
		t.Fatalf("upstream model = %q, want gpt-5.3-codex", gotModel)
	}
}

func TestServer_AppServerDowngradesSparkSuffixForPlusAccount(t *testing.T) {
	var gotModel string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		var payload struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Errorf("unmarshal websocket payload: %v", err)
			return
		}
		gotModel = payload.Params.Model
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "plus-tok", AccountID: "acct-plus", PlanType: "plus"}},
			Inner:    http.DefaultTransport,
		},
	}
	proxyHandler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + codexAppServerPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"model":"gpt-5.3-codex-spark-high"}}`)); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if gotModel != "gpt-5.3-codex-high" {
		t.Fatalf("upstream model = %q, want gpt-5.3-codex-high", gotModel)
	}
}

func TestServer_AppServerPrefersProAccountForSpark(t *testing.T) {
	var gotModel, gotAuth string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade error = %v", err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read error = %v", err)
			return
		}
		var payload struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Errorf("unmarshal websocket payload: %v", err)
			return
		}
		gotModel = payload.Params.Model
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
			t.Errorf("upstream write error = %v", err)
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: upstream.URL, LocalToken: "tok"},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: NewCodexSelector(func() []codex.CodexAccount {
				return []codex.CodexAccount{
					{Email: "plus@test.com", AccessToken: "plus-tok", AccountID: "acct-plus", PlanType: "plus", IsActive: true},
					{Email: "pro@test.com", AccessToken: "pro-tok", AccountID: "acct-pro", PlanType: "pro", IsActive: false},
				}
			}, nil),
			Inner: http.DefaultTransport,
		},
	}
	proxyHandler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + codexAppServerPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"model":"gpt-5.3-codex-spark"}}`)); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if gotAuth != "Bearer pro-tok" {
		t.Fatalf("upstream auth = %q, want Bearer pro-tok", gotAuth)
	}
	if gotModel != "gpt-5.3-codex-spark" {
		t.Fatalf("upstream model = %q, want gpt-5.3-codex-spark", gotModel)
	}
}

func TestServer_ModelsEndpointRequiresAuth(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestServer_ModelsEndpointIncludesSyntheticModels(t *testing.T) {
	srv := &Server{Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
			MaxTokens      int    `json:"max_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	found := false
	for _, model := range resp.Data {
		if model.ID == "gpt-5.4" {
			found = true
			if model.MaxInputTokens != 1050000 {
				t.Fatalf("gpt-5.4 max_input_tokens = %d, want 1050000", model.MaxInputTokens)
			}
			if model.MaxTokens != 128000 {
				t.Fatalf("gpt-5.4 max_tokens = %d, want 128000", model.MaxTokens)
			}
		}
	}
	if !found {
		t.Fatal("missing gpt-5.4 synthetic model")
	}
}

func TestServer_ModelsEndpointMergesUpstreamModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-6","max_input_tokens":200000,"max_tokens":32000}]}`))
	}))
	defer upstream.Close()

	srv := &Server{Config: &Config{ClaudeUpstream: upstream.URL, LocalToken: "tok"}}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, `"claude-opus-4-6"`) {
		t.Fatalf("body missing upstream Claude model: %s", body)
	}
	if !strings.Contains(body, `"gpt-5.4"`) {
		t.Fatalf("body missing synthetic Codex model: %s", body)
	}
}

func TestServer_NativeCodex_StreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			"data: {\"type\":\"response.created\"}\n\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n",
			"data: {\"type\":\"response.completed\"}\n\n",
		}
		for _, ev := range events {
			w.Write([]byte(ev))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-1",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","input":"hello","stream":true}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	stderr := captureStderr(t, func() {
		srv.handleNativeCodex(w, req)
	})

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	result := w.Body.String()
	if !strings.Contains(result, "response.created") {
		t.Error("missing response.created event")
	}
	if !strings.Contains(result, "response.completed") {
		t.Error("missing response.completed event")
	}
	if !strings.Contains(stderr, "provider=codex (native)") {
		t.Fatalf("stderr missing native route log: %s", stderr)
	}
	if strings.Contains(stderr, "protocol=anthropic-messages") {
		t.Fatalf("stderr unexpectedly logged translated protocol for native route: %s", stderr)
	}
}

func TestServer_NativeCodex_NoTransport(t *testing.T) {
	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
		CodexTransport: nil,
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	srv.handleNativeCodex(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_NativeCodex_NoProxyTokenRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_ok"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "secret-proxy-token",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
			}},
			Inner: http.DefaultTransport,
		},
	}

	w := httptest.NewRecorder()
	// Deliberately do NOT send Authorization header or proxy token.
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (no proxy token required for /responses)", w.Code)
	}
}

// ── handleNativeCodex headroom compression tests ─────────────────────────────

// makeResponsesBridgeResponder returns a fakeBridgeRaw responder that handles
// compress_responses operations. When called with input present and no
// previous_response_id, it returns compressedInput with tokensSaved.
// For any other operation (compress_messages) it returns a no-op response.
func makeResponsesBridgeResponder(t *testing.T, compressedInput json.RawMessage, tokensSaved int) func([]byte) []byte {
	t.Helper()
	return func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Errorf("bridge: unmarshal request: %v", err)
			return nil
		}
		if req.Operation != "compress_responses" {
			// Unexpected operation in these tests.
			t.Errorf("bridge: unexpected operation %q", req.Operation)
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       compressedInput,
			TokensSaved: tokensSaved,
		}
		b, _ := json.Marshal(resp)
		return b
	}
}

// TestServer_NativeCodex_HeadroomCompressesBody verifies that when Headroom is
// configured and returns savings, handleNativeCodex sends the compressed body
// to upstream — not the original.
func TestServer_NativeCodex_HeadroomCompressesBody(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"short"}]`)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_compressed"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom: fakeBridgeRaw(t, makeResponsesBridgeResponder(t, compressedInput, 42)),
	}

	originalInput := `[{"role":"user","content":"hello world, this is a very long message that should be compressed"}]`
	body := `{"model":"gpt-5.4","input":` + originalInput + `}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// Upstream must have received the compressed input, not the original.
	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v — body: %s", err, gotBody)
	}
	if string(upstreamBody["input"]) != string(compressedInput) {
		t.Errorf("upstream input = %s, want compressed %s", upstreamBody["input"], compressedInput)
	}
}

// TestServer_NativeCodex_HeadroomBridgeError_FallsBackToOriginal verifies that
// when the bridge returns an error, handleNativeCodex sends the original body.
func TestServer_NativeCodex_HeadroomBridgeError_FallsBackToOriginal(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_ok"}`))
	}))
	defer upstream.Close()

	// Bridge that returns broken JSON to trigger a parse error.
	brokenBridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		return []byte(`{not valid json`)
	})

	originalBody := `{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom: brokenBridge,
	}

	req := httptest.NewRequest("POST", "/responses", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	// Request must still succeed (fail-open).
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (fail-open), body: %s", w.Code, w.Body.String())
	}
	// Upstream must have received the original body unchanged.
	if string(gotBody) != originalBody {
		t.Errorf("upstream body = %s, want original %s", gotBody, originalBody)
	}
}

// TestServer_NativeCodex_HeadroomSkipsPreviousResponseID verifies that when
// previous_response_id is set, compression is bypassed (the bridge is not called)
// and the original body is forwarded.
func TestServer_NativeCodex_HeadroomSkipsPreviousResponseID(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_cont"}`))
	}))
	defer upstream.Close()

	// Bridge that should never be called.
	neverCalledBridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		_ = json.Unmarshal(reqBytes, &req)
		if req.Operation == "compress_responses" {
			t.Error("bridge compress_responses should not be called when previous_response_id is set")
		}
		return nil
	})

	originalBody := `{"model":"gpt-5.4","input":[{"role":"user","content":"continue"}],"previous_response_id":"resp_abc"}`

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom: neverCalledBridge,
	}

	req := httptest.NewRequest("POST", "/responses", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if string(gotBody) != originalBody {
		t.Errorf("upstream body = %s, want original (bypass compression)", gotBody)
	}
}

// TestServer_NativeCodex_HeadroomCanonicalAndLegacyPathBehaveTheSame verifies
// that both /v1/responses and /responses compress identically when Headroom is set.
func TestServer_NativeCodex_HeadroomCanonicalAndLegacyPathBehaveTheSame(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"compressed"}]`)

	for _, path := range []string{codexResponsesPath, legacyCodexResponsesPath} {
		path := path
		t.Run(path, func(t *testing.T) {
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"resp_ok"}`))
			}))
			defer upstream.Close()

			srv := &Server{
				Config: &Config{
					CodexUpstream: upstream.URL,
					LocalToken:    "tok",
				},
				CodexTransport: &CodexTokenTransport{
					Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
					Inner:    http.DefaultTransport,
				},
				Headroom: fakeBridgeRaw(t, makeResponsesBridgeResponder(t, compressedInput, 20)),
			}

			handler, err := srv.handler()
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}

			originalInput := `[{"role":"user","content":"hello world original"}]`
			body := `{"model":"gpt-5.4","input":` + originalInput + `}`
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != 200 {
				t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}

			var upstreamBody map[string]json.RawMessage
			if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
				t.Fatalf("upstream body invalid JSON: %v — body: %s", err, gotBody)
			}
			if string(upstreamBody["input"]) != string(compressedInput) {
				t.Errorf("path %s: upstream input = %s, want compressed %s",
					path, upstreamBody["input"], compressedInput)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gap 1: cache mode must use cache semantics in handleNativeCodex
// ---------------------------------------------------------------------------

// makeCacheResponsesBridgeResponder returns a raw bridge responder that tracks
// which items were sent and verifies the bridge received the full input array
// (not just the mutable suffix). It returns compressedFinalItem appended to
// restored frozen prefix items, simulating correct cache semantics.
func makeCacheResponsesBridgeResponder(t *testing.T, wantFullCount int, compressedFinal json.RawMessage) func([]byte) []byte {
	t.Helper()
	return func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Errorf("bridge: unmarshal request: %v", err)
			return nil
		}
		var sentItems []json.RawMessage
		if err := json.Unmarshal(req.Input, &sentItems); err != nil {
			t.Errorf("bridge: parse items: %v", err)
			return nil
		}
		if len(sentItems) != wantFullCount {
			t.Errorf("bridge received %d items, want %d (cache mode must send full input)", len(sentItems), wantFullCount)
		}
		// Return compressed items (only the mutable final one compressed).
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[` + string(compressedFinal) + `]`),
			TokensSaved: 25,
		}
		b, _ := json.Marshal(resp)
		return b
	}
}

// TestServer_NativeCodex_CacheModeUsesCacheSemantics verifies that when
// s.HeadroomMode is HeadroomModeCache, handleNativeCodex routes to
// CompressResponsesCache (full-request send + frozen-prefix restore).
func TestServer_NativeCodex_CacheModeUsesCacheSemantics(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_cache"}`))
	}))
	defer upstream.Close()

	compressedFinal := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"compressed"}]}`)
	// 3 items total (2 frozen + 1 mutable).
	bridge := fakeBridgeRaw(t, makeCacheResponsesBridgeResponder(t, 3, compressedFinal))

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom:     bridge,
		HeadroomMode: HeadroomModeCache,
	}

	frozenItem0 := `{"role":"user","content":[{"type":"input_text","text":"prior turn"}]}`
	frozenItem1 := `{"role":"assistant","content":[{"type":"text","text":"reply"}]}`
	mutableItem := `{"role":"user","content":[{"type":"input_text","text":"final mutable turn that is long enough to compress"}]}`
	inputJSON := `[` + frozenItem0 + `,` + frozenItem1 + `,` + mutableItem + `]`
	body := `{"model":"gpt-5.4","input":` + inputJSON + `}`

	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// Upstream body must have the frozen prefix items restored.
	var upstreamBody struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v — body: %s", err, gotBody)
	}
	if len(upstreamBody.Input) < 3 {
		t.Fatalf("upstream input has %d items, want >= 3", len(upstreamBody.Input))
	}
	// Frozen prefix must be byte-stable.
	var origItems []json.RawMessage
	if err := json.Unmarshal(json.RawMessage(inputJSON), &origItems); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if string(upstreamBody.Input[i]) != string(origItems[i]) {
			t.Errorf("upstream input[%d] = %s, want original %s (frozen in cache mode)",
				i, upstreamBody.Input[i], origItems[i])
		}
	}
}

// TestServer_NativeCodex_TokenModeUsesTokenSemantics verifies that when
// When s.HeadroomMode is HeadroomModeToken, handleNativeCodex routes to
// CompressResponses (standard token-mode path — bridge called once with full input,
// no frozen prefix restoration).
func TestServer_NativeCodex_TokenModeUsesTokenSemantics(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"token compressed"}]}]`)
	tokenBridgeCalled := false

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_token"}`))
	}))
	defer upstream.Close()

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		tokenBridgeCalled = true
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Errorf("bridge: unmarshal: %v", err)
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       compressedInput,
			TokensSaved: 20,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom:     bridge,
		HeadroomMode: HeadroomModeToken, // explicit token mode
	}

	body := `{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_text","text":"original"}]}]}`
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !tokenBridgeCalled {
		t.Error("bridge was not called in token mode")
	}

	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if string(upstreamBody["input"]) != string(compressedInput) {
		t.Errorf("upstream input = %s, want compressed %s", upstreamBody["input"], compressedInput)
	}
}

// ---------------------------------------------------------------------------
// Gap 2: cache mode must affect proxyHandler (Anthropic /v1/messages)
// ---------------------------------------------------------------------------

// TestServer_ProxyHandler_CacheModeUsesCompressCache verifies that when
// s.HeadroomMode is HeadroomModeCache, proxyHandler calls CompressCache
// (full-request send + frozen-prefix restore) instead of Compress.
func TestServer_ProxyHandler_CacheModeUsesCompressCache(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_cache"}`))
	}))
	defer upstream.Close()

	// Bridge that captures messages sent to it; verifies it receives the full array.
	compressedMutable := json.RawMessage(`{"role":"user","content":"compressed final"}`)
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Errorf("bridge: unmarshal: %v", err)
			return nil
		}
		var msgs []json.RawMessage
		if err := json.Unmarshal(req.Messages, &msgs); err != nil {
			t.Errorf("bridge: parse messages: %v", err)
			return nil
		}
		if len(msgs) != 3 {
			t.Errorf("bridge received %d messages, want 3 (full request in cache mode)", len(msgs))
		}
		// Return one compressed message.
		resp := headroomResponse{
			Messages:    json.RawMessage(`[` + string(compressedMutable) + `]`),
			TokensSaved: 40,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "tok",
		},
		Transport:    &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
		Headroom:     bridge,
		HeadroomMode: HeadroomModeCache,
	}

	frozenSys := `{"role":"user","content":"first turn (frozen)"}`
	frozenAst := `{"role":"assistant","content":"reply (frozen)"}`
	mutableMsg := `{"role":"user","content":"final mutable user turn"}`
	msgsJSON := `[` + frozenSys + `,` + frozenAst + `,` + mutableMsg + `]`
	body := `{"model":"claude-sonnet","messages":` + msgsJSON + `}`

	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// Frozen prefix must be restored in upstream body.
	var upstreamBody struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v — body: %s", err, gotBody)
	}
	if len(upstreamBody.Messages) < 3 {
		t.Fatalf("upstream messages has %d, want >= 3", len(upstreamBody.Messages))
	}

	var origMsgs []json.RawMessage
	if err := json.Unmarshal(json.RawMessage(msgsJSON), &origMsgs); err != nil {
		t.Fatal(err)
	}
	// First two messages (frozen prefix) must be byte-identical to originals.
	for i := 0; i < 2; i++ {
		if string(upstreamBody.Messages[i]) != string(origMsgs[i]) {
			t.Errorf("upstream messages[%d] = %s, want original %s (frozen prefix byte-stable)",
				i, upstreamBody.Messages[i], origMsgs[i])
		}
	}
}

// TestServer_ProxyHandler_TokenModeUsesCompress verifies that when
// s.HeadroomMode is HeadroomModeToken, proxyHandler calls Compress (token mode).
func TestServer_ProxyHandler_TokenModeUsesCompress(t *testing.T) {
	compressedMessages := json.RawMessage(`[{"role":"user","content":"token compressed"}]`)
	bridgeCalled := false

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_tok"}`))
	}))
	defer upstream.Close()

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		bridgeCalled = true
		resp := headroomResponse{
			Messages:    compressedMessages,
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: upstream.URL,
			LocalToken:     "tok",
		},
		Transport:    &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
		Headroom:     bridge,
		HeadroomMode: HeadroomModeToken,
	}

	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"original long message"}]}`
	handler := srv.proxyHandler(mustParseURL(upstream.URL))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !bridgeCalled {
		t.Error("bridge was not called in token mode")
	}
	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if string(upstreamBody["messages"]) != string(compressedMessages) {
		t.Errorf("upstream messages = %s, want compressed %s", upstreamBody["messages"], compressedMessages)
	}
}

// ── Payload diagnostics tests ────────────────────────────────────────────────

func TestServerPayloadDiagnosticsClaudeRouteEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg_123"}`), nil
			}),
		},
		PayloadDiag: payloadDiag,
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	reqBody := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/1.0.0")
	req.Header.Set("X-Claude-Code-Session-Id", "test-session-abc")
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Method != http.MethodPost || ev.Path != "/v1/messages" || ev.Provider != "claude" {
		t.Fatalf("event route = %+v", ev)
	}
	if ev.RouteKind != "anthropic_messages" {
		t.Fatalf("RouteKind = %q, want anthropic_messages", ev.RouteKind)
	}
	if ev.Model != "claude-sonnet" {
		t.Fatalf("Model = %q, want claude-sonnet", ev.Model)
	}
	if ev.ClientKind != "claude-code" {
		t.Fatalf("ClientKind = %q, want claude-code", ev.ClientKind)
	}
	if ev.SessionSource != "x-claude-code-session-id" {
		t.Fatalf("SessionSource = %q, want x-claude-code-session-id", ev.SessionSource)
	}
	if ev.SessionKey == "" {
		t.Fatal("SessionKey is empty, want non-empty")
	}
	if ev.BodyBytes != len(reqBody) {
		t.Fatalf("BodyBytes = %d, want %d", ev.BodyBytes, len(reqBody))
	}
	if ev.Body == nil {
		t.Fatal("Body is nil, want non-nil")
	}
	// Must not leak credentials.
	assertPayloadLogDoesNotContain(t, path, "local-tok")
	assertPayloadLogDoesNotContain(t, path, "real-token")
	assertPayloadLogDoesNotContain(t, path, "user@test.com")
	// Must not leak raw session ID.
	assertPayloadLogDoesNotContain(t, path, "test-session-abc")
}

func TestServerPayloadDiagnosticsCountTokensEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":42}`)),
			}, nil
		}),
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future}}
		},
		PayloadDiag: payloadDiag,
	}
	_ = sel

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	reqBody := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, countTokensPath, strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer local-tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Path != countTokensPath {
		t.Fatalf("Path = %q, want %q", ev.Path, countTokensPath)
	}
	if ev.RouteKind != "anthropic_count_tokens" {
		t.Fatalf("RouteKind = %q, want anthropic_count_tokens", ev.RouteKind)
	}
}

func TestServerPayloadDiagnosticsNativeCodexEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_codex"}`)),
				}, nil
			}),
		},
		PayloadDiag: payloadDiag,
	}

	w := httptest.NewRecorder()
	reqBody := `{"model":"gpt-5.4","input":"tell me about go"}`
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-cli/1.0")
	srv.handleNativeCodex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Provider != "codex" || ev.RouteKind != "codex_native" {
		t.Fatalf("event = %+v, want codex/codex_native", ev)
	}
	if ev.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", ev.Model)
	}
	if ev.ClientKind != "codex" {
		t.Fatalf("ClientKind = %q, want codex", ev.ClientKind)
	}
	if ev.BodyBytes != len(reqBody) {
		t.Fatalf("BodyBytes = %d, want %d", ev.BodyBytes, len(reqBody))
	}
	assertPayloadLogDoesNotContain(t, path, "codex-tok")
}

func TestServerPayloadDiagnosticsCompactEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
				}, nil
			}),
		},
		PayloadDiag: payloadDiag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	w := httptest.NewRecorder()
	reqBody := `{"model":"gpt-5.4","previous_response_id":"resp_abc"}`
	req := httptest.NewRequest(http.MethodPost, codexCompactResponsesPath, strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Provider != "codex" || ev.RouteKind != "codex_compact" {
		t.Fatalf("event = %+v, want codex/codex_compact", ev)
	}
	if ev.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", ev.Model)
	}
	assertPayloadLogDoesNotContain(t, path, "codex-tok")
}

func TestServerPayloadDiagnosticsNoEventForBinaryWebSocketFrame(t *testing.T) {
	// Binary WebSocket frames are not captured by payload diagnostics.
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, msg, _ := conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, msg)
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "local-tok",
		},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		PayloadDiag: payloadDiag,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + legacyCodexResponsesPath
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("Dial() error = %v", err)
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("ping"))
	conn.ReadMessage()
	_ = conn.Close()

	// Allow brief time for any async writes.
	time.Sleep(50 * time.Millisecond)
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// No payload event should be emitted for binary WebSocket frames.
	if _, err := os.Stat(path); err == nil {
		events := readPayloadEvents(t, path)
		if len(events) != 0 {
			t.Fatalf("binary websocket emitted %d payload events, want 0", len(events))
		}
	}
}

func TestServerPayloadDiagnosticsDistinctParallelSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg"}`), nil
			}),
		},
		PayloadDiag: payloadDiag,
	}

	sessions := []string{"session-alpha", "session-beta", "session-gamma"}
	var wg sync.WaitGroup
	for _, sess := range sessions {
		sess := sess
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
			req.Header.Set("Authorization", "Bearer local-tok")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Claude-Code-Session-Id", sess)
			handler(w, req)
		}()
	}
	wg.Wait()

	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != len(sessions) {
		t.Fatalf("events = %d, want %d", len(events), len(sessions))
	}

	// All session keys must be distinct.
	seen := make(map[string]bool)
	for _, ev := range events {
		if seen[ev.SessionKey] {
			t.Fatalf("duplicate session key %q across parallel sessions", ev.SessionKey)
		}
		seen[ev.SessionKey] = true
	}
}

func TestServerPayloadDiagnosticsDistinctParallelBodySessionsWithIdenticalHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg"}`), nil
			}),
		},
		PayloadDiag: payloadDiag,
	}

	conversationIDs := []string{"conv-alpha", "conv-beta", "conv-gamma"}
	var wg sync.WaitGroup
	for _, conversationID := range conversationIDs {
		conversationID := conversationID
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"claude-sonnet","conversation_id":%q,"messages":[]}`, conversationID)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-tok")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "claude-code/1.0.0")
			handler(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readPayloadEvents(t, path)
	if len(events) != len(conversationIDs) {
		t.Fatalf("events = %d, want %d", len(events), len(conversationIDs))
	}
	seen := make(map[string]bool)
	for _, ev := range events {
		if ev.SessionSource != "body:conversation_id" {
			t.Fatalf("SessionSource = %q, want body:conversation_id", ev.SessionSource)
		}
		if ev.SessionKey == "" || !strings.HasPrefix(ev.SessionKey, "body-session:") {
			t.Fatalf("SessionKey = %q, want body-session:<hash>", ev.SessionKey)
		}
		if seen[ev.SessionKey] {
			t.Fatalf("duplicate session key %q across body-distinguished sessions", ev.SessionKey)
		}
		seen[ev.SessionKey] = true
	}
	assertPayloadLogDoesNotContain(t, path, "local-tok")
	assertPayloadLogDoesNotContain(t, path, "real-token")
}

func TestServerPayloadDiagnosticsDisabledNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	future := time.Now().UnixMilli() + 3600_000
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "user@test.com", AccessToken: "real-token", ExpiresAt: future},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "local-tok",
		},
		Transport: &TokenTransport{
			Selector: sel,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return makeResponse(http.StatusOK, `{"id":"msg"}`), nil
			}),
		},
		// PayloadDiag intentionally nil.
	}

	handler := srv.proxyHandler(mustParseURL(srv.Config.ClaudeUpstream))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-tok")
	handler(w, req)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("payload file should not exist when PayloadDiag is nil: %v", err)
	}
}

func TestServerHealthReportsPayloadEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "payloads.jsonl")
			srv := &Server{}
			if tc.enabled {
				pw, err := OpenPayloadWriter(path)
				if err != nil {
					t.Fatalf("OpenPayloadWriter: %v", err)
				}
				defer pw.Close()
				srv.PayloadDiag = pw
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			srv.handleHealth(w, req)

			var resp struct {
				Diagnostics struct {
					Payload bool `json:"payload"`
				} `json:"diagnostics"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Diagnostics.Payload != tc.enabled {
				t.Fatalf("diagnostics.payload = %v, want %v", resp.Diagnostics.Payload, tc.enabled)
			}
		})
	}
}

func assertPayloadLogDoesNotContain(t *testing.T, path, needle string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read payload log: %v", err)
	}
	if strings.Contains(string(raw), needle) {
		t.Fatalf("payload log leaked %q: %s", needle, raw)
	}
}

// TestServer_NativeCodex_HeadroomNil_NoCompression verifies that when Headroom
// is nil, no compression is attempted and the original body is forwarded.
func TestServer_NativeCodex_HeadroomNil_NoCompression(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_ok"}`))
	}))
	defer upstream.Close()

	originalBody := `{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`

	srv := &Server{
		Config: &Config{
			CodexUpstream: upstream.URL,
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    http.DefaultTransport,
		},
		Headroom: nil, // explicitly nil
	}

	req := httptest.NewRequest("POST", "/responses", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleNativeCodex(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if string(gotBody) != originalBody {
		t.Errorf("upstream body = %s, want original (no compression when nil)", gotBody)
	}
}
