package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type zaiForbiddenCodexHTTP struct{ t *testing.T }

func (f zaiForbiddenCodexHTTP) TryServe(http.ResponseWriter, *http.Request, bool) (bool, string) {
	f.t.Error("Z.ai entered Codex account planner")
	return true, ""
}

func zaiTestCatalog() *modelregistry.Catalog {
	return modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderZAI, ID: "glm-5.3-flash", Source: modelregistry.SourceNative}}})
}

func TestZAINativeHTTPRoutesBeforeCodexAndIsolatesCredentials(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			const body = `{"model":"glm-5.3-flash","input":"hello"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Errorf("path %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer zai-test-key" {
					t.Errorf("auth %q", got)
				}
				for _, name := range []string{"Chatgpt-Account-Id", "X-Api-Key", "Cookie", "Openai-Organization"} {
					if got := r.Header.Get(name); got != "" {
						t.Errorf("leaked %s", name)
					}
				}
				got, _ := io.ReadAll(r.Body)
				if string(got) != body {
					t.Errorf("body %q", got)
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "upstream-response")
			}))
			defer upstream.Close()
			s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
			r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer chatgpt-secret")
			r.Header.Set("Chatgpt-Account-Id", "account-secret")
			r.Header.Set("X-Api-Key", "anthropic-secret")
			r.Header.Set("Cookie", "secret-cookie")
			r.Header.Set("Openai-Organization", "org-secret")
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, r)
			if w.Code != http.StatusAccepted || w.Body.String() != "upstream-response" || w.Header().Get("Content-Type") != contentType {
				t.Fatalf("response %d %s %s", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
			}
		})
	}
}

func TestZAINativeHTTPMissingCredentialFailsClosed(t *testing.T) {
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "", nil }, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":"hi"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestZAIWebSocketRoutesWithoutCodexAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer zai-test-key" {
			t.Errorf("auth %q", got)
		}
		if r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("Cookie") != "" {
			t.Error("account credentials leaked")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["type"] != nil || body["stream"] != true || body["model"] != "glm-5.3-flash" {
			t.Errorf("upstream frame %v", body)
		}
		writeZAITestSSE(w, "response-1", nil)
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL, CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}}}
	c := openZAITestWebSocket(t, s, http.Header{"Authorization": []string{"Bearer chatgpt-secret"}, "Chatgpt-Account-Id": []string{"account-secret"}, "Cookie": []string{"secret-cookie"}})
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	response := readZAITestCompletion(t, c)
	if response.Response.ID != "response-1" {
		t.Fatalf("response id %s", response.Response.ID)
	}
}

func writeZAITestSSE(w http.ResponseWriter, id string, output []json.RawMessage) {
	if output == nil {
		output = []json.RawMessage{}
	}
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "output": output}})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

func openZAITestWebSocket(t *testing.T, s *Server, header http.Header) *websocket.Conn {
	t.Helper()
	handler, err := s.handler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c
}

type zaiTestCompletion struct {
	Type     string `json:"type"`
	Response struct {
		ID string `json:"id"`
	} `json:"response"`
}

func readZAITestCompletion(t *testing.T, c *websocket.Conn) zaiTestCompletion {
	t.Helper()
	for {
		_, payload, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var event zaiTestCompletion
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.completed" {
			return event
		}
	}
}

type zaiCapturingCodexHTTP struct {
	body     string
	encoding string
}

func (f *zaiCapturingCodexHTTP) TryServe(w http.ResponseWriter, r *http.Request, _ bool) (bool, string) {
	b, _ := io.ReadAll(r.Body)
	f.body = string(b)
	f.encoding = r.Header.Get("Content-Encoding")
	w.WriteHeader(http.StatusNoContent)
	return true, "gpt-test"
}

func TestZAINativeHTTPPreservesGPTRequest(t *testing.T) {
	body := `{"model":"gpt-test","input":"preserve exact bytes"}`
	encoded, err := EncodeCodexRequest([]byte(body), "zstd", codexHTTPZstdLimits())
	if err != nil {
		t.Fatal(err)
	}
	codex := &zaiCapturingCodexHTTP{}
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, CodexNativeHTTP: codex}
	r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(encoded)))
	r.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, r)
	if w.Code != http.StatusNoContent || codex.body != string(encoded) || codex.encoding != "zstd" {
		t.Fatalf("GPT request changed: status=%d body=%q encoding=%q", w.Code, codex.body, codex.encoding)
	}
}

func TestZAINativeHTTPDecodesZstd(t *testing.T) {
	body := `{"model":"glm-5.3-flash","input":"compressed"}`
	encoded, err := EncodeCodexRequest([]byte(body), "zstd", codexHTTPZstdLimits())
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.Header.Get("Content-Encoding") != "" {
			t.Errorf("compressed body leaked: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
	r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(encoded)))
	r.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

type zaiCapturingCodexWebSocket struct {
	t    *testing.T
	want string
}

func (b *zaiCapturingCodexWebSocket) Serve(context.Context, *websocket.Conn, http.Header) error {
	b.t.Error("prefetched frame was lost")
	return nil
}
func (b *zaiCapturingCodexWebSocket) ServeInitial(_ context.Context, c *websocket.Conn, _ http.Header, kind int, payload []byte, _ func(string) bool) error {
	if kind != websocket.TextMessage || string(payload) != b.want {
		b.t.Errorf("changed initial frame %s", payload)
	}
	return c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
}

func TestZAIEnabledPreservesGPTWebSocketInitialFrame(t *testing.T) {
	const frame = `{"type":"response.create","model":"gpt-test","input":[]}`
	s := &Server{Config: &Config{ClaudeUpstream: "https://claude.test"}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, CodexWebSocketBroker: &zaiCapturingCodexWebSocket{t: t, want: frame}, CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}}}
	h, err := s.handler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"type":"response.completed"}` {
		t.Fatalf("frame %s", got)
	}
}

func TestZAIWebSocketProviderSwitchNeverReachesUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeZAITestSSE(w, "response-1", nil) }))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL}
	c := openZAITestWebSocket(t, s, nil)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[]}`))
	readZAITestCompletion(t, c)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":[]}`))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("provider switch kept connection open")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("wrong-provider frame reached upstream: requests=%d", got)
	}
}

func TestZAIWebSocketPrewarmAndToolContinuation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input    []json.RawMessage `json:"input"`
			Previous json.RawMessage   `json:"previous_response_id"`
			Type     json.RawMessage   `json:"type"`
			Generate json.RawMessage   `json:"generate"`
			Stream   bool              `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Previous != nil || body.Type != nil || body.Generate != nil || !body.Stream {
			t.Error("transport fields reached HTTP upstream")
		}
		switch calls.Add(1) {
		case 1:
			if len(body.Input) != 2 {
				t.Errorf("prewarm input count %d", len(body.Input))
			}
			writeZAITestSSE(w, "response-1", []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{}"}`)})
		case 2:
			if len(body.Input) != 4 {
				t.Errorf("tool continuation input count %d", len(body.Input))
			}
			if !strings.Contains(string(body.Input[len(body.Input)-1]), "function_call_output") {
				t.Error("tool result missing")
			}
			writeZAITestSSE(w, "response-2", nil)
		default:
			t.Error("unexpected upstream request")
		}
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL}
	c := openZAITestWebSocket(t, s, nil)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","generate":false,"input":[{"role":"user","content":"prefix"}]}`))
	warmup := readZAITestCompletion(t, c)
	if calls.Load() != 0 {
		t.Fatal("prewarm consumed Z.ai quota")
	}
	turn, _ := json.Marshal(map[string]any{"type": "response.create", "model": "glm-5.3-flash", "previous_response_id": warmup.Response.ID, "input": []map[string]string{{"role": "user", "content": "run"}}})
	_ = c.WriteMessage(websocket.TextMessage, turn)
	first := readZAITestCompletion(t, c)
	if first.Response.ID != "response-1" {
		t.Fatalf("response id %s", first.Response.ID)
	}
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","previous_response_id":"response-1","input":[{"type":"function_call_output","call_id":"call-1","output":"hello"}]}`))
	second := readZAITestCompletion(t, c)
	if second.Response.ID != "response-2" || calls.Load() != 2 {
		t.Fatalf("response %s calls %d", second.Response.ID, calls.Load())
	}
}

func TestZAICompactNeverUsesCodexCredentials(t *testing.T) {
	for _, path := range []string{"/responses/compact", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			s := &Server{Config: &Config{ClaudeUpstream: "https://claude.test"}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
			handler, err := s.handler()
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"glm-5.3-flash","input":[]}`)))
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestZAIWebSocketErrorDoesNotExposeUpstreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "sensitive-upstream-body")
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL}
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
	if event.Type != "error" || event.Status != http.StatusUnauthorized || strings.Contains(string(payload), "sensitive-upstream-body") {
		t.Fatalf("unsafe error frame %s", payload)
	}
}

func TestZAIWebSocketDisconnectCancelsHTTP(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http starts disconnect detection after the request body is drained.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
			t.Error("upstream request not cancelled")
		}
	}))
	defer upstream.Close()
	s := &Server{Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "zai-test-key", nil }, ZAIUpstream: upstream.URL}
	c := openZAITestWebSocket(t, s, nil)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","input":[]}`))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not started")
	}
	_ = c.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel upstream request")
	}
}

func TestZAIWebSocketRuntimeUpgradeWaitsForSocketClose(t *testing.T) {
	server := &Server{
		Config:    &Config{ClaudeUpstream: "https://claude.test"},
		Catalog:   zaiTestCatalog(),
		ZAIAPIKey: func() (string, error) { return "zai-test-key", nil },
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	worker := handler.(runtimeDrainHandler)
	proxy := httptest.NewServer(RuntimeUpgradeHTTPHandler(handler, worker.RuntimeUpgradeAdmission()))
	defer proxy.Close()
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+legacyCodexResponsesPath, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3-flash","generate":false,"input":[]}`)); err != nil {
		t.Fatal(err)
	}
	readZAITestCompletion(t, connection)
	if err := worker.PrepareUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := worker.AwaitUpgradeQuiescence(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active Z.ai socket quiescence = %v", err)
	}
	connection.Close()
	wait, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	if err := worker.AwaitUpgradeQuiescence(wait); err != nil {
		t.Fatalf("closed Z.ai socket quiescence = %v", err)
	}
	if err := worker.ResumeUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
}
