package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestServer_CodexLiveCallCreate_ForwardsWithAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/realtime/calls" {
			t.Errorf("upstream path = %q, want /backend-api/codex/realtime/calls", r.URL.Path)
		}
		if got := r.URL.Query().Get("intent"); got != "quicksilver" {
			t.Errorf("intent = %q, want quicksilver", got)
		}
		if got := r.URL.Query().Get("architecture"); got != "avas" {
			t.Errorf("architecture = %q, want avas", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-tok" {
			t.Errorf("Authorization = %q, want Bearer codex-tok", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Errorf("ChatGPT-Account-ID = %q, want acct-123", got)
		}
		if got := r.Header.Get("OpenAI-Alpha"); got != "quicksilver=v2" {
			t.Errorf("OpenAI-Alpha = %q, want quicksilver=v2", got)
		}
		var body struct {
			SDP     string         `json:"sdp"`
			Session map[string]any `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body.SDP != "offer-sdp" {
			t.Errorf("sdp = %q, want offer-sdp", body.SDP)
		}
		if got := body.Session["type"]; got != "realtime" {
			t.Errorf("session.type = %v, want realtime", got)
		}
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", "/v1/live/rtc_test")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "answer-sdp")
	}))
	defer upstream.Close()

	srv := newCodexLiveTestServer(upstream.URL + "/backend-api/codex")
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	body, contentType := liveMultipartBody(t, "offer-sdp", `{"type":"realtime"}`)
	req := httptest.NewRequest(http.MethodPost, "/live", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("OpenAI-Alpha", "quicksilver=v2")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/v1/live/rtc_test" {
		t.Errorf("Location = %q, want /v1/live/rtc_test", got)
	}
	if got := w.Body.String(); got != "answer-sdp" {
		t.Errorf("body = %q, want answer-sdp", got)
	}
}

func TestServer_CodexLiveSideband_RelaysFrames(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/realtime/calls":
			w.Header().Set("Content-Type", "application/sdp")
			w.Header().Set("Location", "/v1/live/rtc_test")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "answer-sdp")
		case "/v1/live/rtc_test":
			if got := r.Header.Get("Authorization"); got != "Bearer codex-tok" {
				t.Errorf("Authorization = %q, want Bearer codex-tok", got)
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upstream upgrade: %v", err)
				return
			}
			defer conn.Close()
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("upstream read: %v", err)
				return
			}
			if err := conn.WriteMessage(messageType, append([]byte("relay:"), message...)); err != nil {
				t.Errorf("upstream write: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	srv := newCodexLiveTestServer(upstream.URL + "/backend-api/codex")
	srv.codexLiveSidebandUpstream = upstream.URL + "/v1"
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	body, contentType := liveMultipartBody(t, "offer-sdp", `{"type":"realtime"}`)
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/live", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create call: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("call status = %d, want 201", resp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/live/rtc_test"
	conn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if wsResp != nil {
			defer wsResp.Body.Close()
		}
		t.Fatalf("sideband dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0xff, 0x7f}); err != nil {
		t.Fatalf("sideband write: %v", err)
	}
	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("sideband read: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Errorf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(message, []byte{'r', 'e', 'l', 'a', 'y', ':', 0x00, 0xff, 0x7f}) {
		t.Errorf("message = %v, want relayed binary bytes", message)
	}
}

func TestServer_CodexLiveCall_RejectsMissingSDP(t *testing.T) {
	called := false
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})
	srv := newCodexLiveTestServer("https://chatgpt.com/backend-api/codex")
	srv.CodexTransport.(*CodexTokenTransport).Inner = transport
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	body, contentType := liveMultipartBody(t, "", `{"type":"realtime"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream called for invalid request")
	}
}

func TestServer_CodexLiveCall_UsesHTTPTransport(t *testing.T) {
	selector := &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://chatgpt.com/backend-api/codex",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: selector,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     http.Header{"Location": []string{"/v1/live/rtc_http"}},
					Body:       io.NopCloser(strings.NewReader("answer-sdp")),
				}, nil
			}),
		},
		CodexUpgradeTransport: &CodexTokenTransport{
			Selector: selector,
			Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("websocket transport used for HTTP call")
			}),
		},
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/live", strings.NewReader("offer-sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
}

func TestServer_CodexLiveSideband_UsesCallAccountAffinity(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/realtime/calls":
			w.Header().Set("Location", "/v1/live/rtc_affinity")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "answer-sdp")
		case "/v1/live/rtc_affinity":
			if got := r.Header.Get("Authorization"); got != "Bearer token-one" {
				t.Errorf("Authorization = %q, want call-create account token", got)
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upstream upgrade: %v", err)
				return
			}
			_ = conn.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	selector := &rotatingCodexSelector{accounts: []*codex.CodexAccount{
		{AccessToken: "token-one", AccountID: "account-one"},
		{AccessToken: "token-two", AccountID: "account-two"},
	}}
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL + "/backend-api/codex",
		},
		CodexTransport: &CodexTokenTransport{Selector: selector, Inner: http.DefaultTransport},
	}
	srv.codexLiveSidebandUpstream = upstream.URL + "/v1"
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	req := httptest.NewRequest(http.MethodPost, "/realtime/calls", strings.NewReader("offer-sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("call status = %d, want 201", w.Code)
	}

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/live/rtc_affinity"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
		}
		t.Fatalf("sideband dial: %v", err)
	}
	_ = conn.Close()
	if got := selector.callCount(); got != 1 {
		t.Fatalf("selector calls = %d, want 1; sideband should reuse call account", got)
	}
}

func TestCodexLiveSidebandURL(t *testing.T) {
	srv := &Server{codexLiveSidebandUpstream: "https://example.test/v1"}
	tests := []struct {
		target codexLiveSidebandTarget
		want   string
	}{
		{target: codexLiveSidebandTarget{callID: "rtc_one", style: "live"}, want: "wss://example.test/v1/live/rtc_one"},
		{target: codexLiveSidebandTarget{callID: "rtc_two", style: "realtime-calls"}, want: "wss://example.test/v1/realtime/calls/rtc_two"},
		{target: codexLiveSidebandTarget{callID: "rtc_three", style: "realtime-query"}, want: "wss://example.test/v1/realtime?intent=quicksilver&call_id=rtc_three"},
	}
	for _, test := range tests {
		if got := srv.codexLiveSidebandURL(test.target); got != test.want {
			t.Errorf("codexLiveSidebandURL(%+v) = %q, want %q", test.target, got, test.want)
		}
	}
}

type rotatingCodexSelector struct {
	mu       sync.Mutex
	accounts []*codex.CodexAccount
	calls    int
}

func (s *rotatingCodexSelector) Select(_ context.Context, exclude ...codex.SelectionExclusion) (*codex.CodexAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	excludedAccounts := make(map[codex.AccountKey]bool, len(exclude))
	excludedCandidates := make(map[codex.CandidateID]bool, len(exclude))
	for _, exclusion := range exclude {
		excludedAccounts[exclusion.AccountKey] = true
		excludedCandidates[exclusion.CandidateID] = true
	}
	for _, account := range s.accounts {
		if !codexAcctExcluded(account, excludedAccounts, excludedCandidates) {
			s.calls++
			copy := *account
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("no codex accounts available")
}

func (s *rotatingCodexSelector) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newCodexLiveTestServer(codexUpstream string) *Server {
	return &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  codexUpstream,
			LocalToken:     "local-tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-123",
				Email:       "user@example.com",
			}},
			Inner: http.DefaultTransport,
		},
	}
}

func liveMultipartBody(t *testing.T, sdp, session string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("sdp", sdp); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("session", session); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}
