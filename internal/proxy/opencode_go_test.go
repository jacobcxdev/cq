package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestOpenCodeGoHTTPPrefersZAIAndOnlyRetriesRejectedRequests(t *testing.T) {
	const overlappingModel = "glm-5.3"
	const goOnlyModel = "kimi-k3"
	for _, tc := range []struct {
		name             string
		model            string
		primaryStatus    int
		input            string
		initialRejection bool
		wantPrimary      int32
		wantGo           int32
		wantStatus       int
	}{
		{"go only", goOnlyModel, 0, `"hello"`, false, 0, 1, 200},
		{"go only context suffix and case", "KIMI-K3[1m]", 0, `"hello"`, false, 0, 1, 200},
		{"first party available", overlappingModel, 200, `"hello"`, false, 1, 0, 200},
		{"first party exhausted", overlappingModel, 429, `"hello"`, false, 1, 1, 200},
		{"initial stream rejection", overlappingModel, 200, `"hello"`, true, 1, 1, 200},
		{"opaque continuation", overlappingModel, 429, `[{"type":"reasoning","encrypted_content":"opaque"}]`, false, 1, 0, 429},
		{"ambiguous delivery", overlappingModel, 502, `"hello"`, false, 1, 0, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primaryCalls, goCalls atomic.Int32
			var credentialReads int
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer primary-key" {
					t.Error("wrong first-party credential")
				}
				if tc.initialRejection {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"exhausted\"}\n\n")
				} else {
					w.WriteHeader(tc.primaryStatus)
				}
			}))
			defer primary.Close()
			s := &Server{
				Config: &Config{}, Catalog: openCodeGoTestCatalog(),
				ZAIUpstream: primary.URL, ZAIAPIKey: func() (string, error) { return "primary-key", nil },
				CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
				SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
					goCalls.Add(1)
					assertOpenCodeGoUpstream(t, r)
					if r.Header.Get("X-OpenCode-Session") != "codex-session" {
						t.Error("wrong Go session")
					}
					var request struct {
						Model         string
						StreamOptions json.RawMessage `json:"stream_options"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != strings.ToLower(ParseModel(tc.model)) {
						t.Errorf("upstream model = %q, err = %v", request.Model, err)
					}
					if len(request.StreamOptions) != 0 {
						t.Error("Go received Responses-only stream_options")
					}
					w := httptest.NewRecorder()
					_, _ = io.WriteString(w, `{"id":"go-answer"}`)
					return w.Result(), nil
				}),
			}
			route := s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo]
			resolveCredential := route.Credential
			route.Credential = func(ctx context.Context) (SharedBudgetCredential, error) {
				credentialReads++
				return resolveCredential(ctx)
			}
			s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo] = route
			req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":%s,"stream_options":{"include_obfuscation":false}}`, tc.model, tc.input)))
			req.Header.Set("Authorization", "Bearer chatgpt-token")
			req.Header.Set("Cookie", "chatgpt-cookie")
			req.Header.Set("Chatgpt-Account-Id", "chatgpt-account")
			req.Header.Set("Session-Id", "codex-session")
			w := httptest.NewRecorder()
			s.handleLegacyCodexResponsesRoute(w, req)
			if w.Code != tc.wantStatus || primaryCalls.Load() != tc.wantPrimary || goCalls.Load() != tc.wantGo {
				t.Fatalf("status %d, first-party calls %d, Go calls %d", w.Code, primaryCalls.Load(), goCalls.Load())
			}
			if credentialReads != int(tc.wantGo) {
				t.Fatalf("credential read %d times, want %d dispatched requests", credentialReads, tc.wantGo)
			}
		})
	}
}

func TestOpenCodeGoFallbackAfterZCodeWithEmptyToolResultIDs(t *testing.T) {
	var primaryCalls, goCalls int
	s := &Server{
		Config: &Config{}, Catalog: openCodeGoTestCatalog(),
		ZAIAccounts: func() ([]zaiprov.Account, error) {
			return []zaiprov.Account{{
				ID: "zcode", Transport: "zcode-start",
				OAuth: &zcode.LoginSession{ID: "fixture", ZCodeJWT: "fixture-token"},
			}}, nil
		},
		ZAIGateway: &zcode.Gateway{
			Version: "3.17.2",
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				primaryCalls++
				var request struct{ Messages json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				history := string(request.Messages)
				if strings.Contains(history, "tool_result") || !strings.Contains(history, "function output") || !strings.Contains(history, "custom output") {
					t.Errorf("ZCode did not preserve unattributed results as text: %s", history)
				}
				w := httptest.NewRecorder()
				w.WriteHeader(http.StatusTooManyRequests)
				return w.Result(), nil
			})},
		},
		CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			goCalls++
			assertOpenCodeGoUpstream(t, r)
			var request struct{ Input []map[string]json.RawMessage }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if len(request.Input) != 1 || string(request.Input[0]["type"]) != `"message"` {
				t.Errorf("Go did not receive cleaned history: %v", request.Input)
			}
			w := httptest.NewRecorder()
			_, _ = io.WriteString(w, `{"id":"go-answer"}`)
			return w.Result(), nil
		}),
	}
	body := `{"model":"glm-5.3","input":[{"type":"message","role":"user","content":"Continue."},{"type":"function_call_output","call_id":"","output":"function output"},{"type":"custom_tool_call_output","call_id":"","output":"custom output"}]}`
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if w.Code != http.StatusOK || primaryCalls != 1 || goCalls != 1 || !strings.Contains(w.Body.String(), "go-answer") {
		t.Fatalf("status %d, ZCode calls %d, Go calls %d, body %q", w.Code, primaryCalls, goCalls, w.Body.String())
	}
}

func TestOpenCodeGoWorkersInheritParentsRoute(t *testing.T) {
	var primaryCalls, goCalls atomic.Int32
	s := &Server{
		Config: &Config{ClaudeUpstream: "https://claude.test"}, Catalog: openCodeGoTestCatalog(),
		ZAIAPIKey: func() (string, error) { return "zai-key", nil },
		ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			if primaryCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
			}
			return w.Result(), nil
		}),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			var request struct {
				Tools json.RawMessage
				Input []struct {
					Type, Role string
					Content    []struct{ Type, Text string }
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			call := goCalls.Add(1)
			if call > 1 && (len(request.Input) != 1 || request.Input[0].Type != "message" ||
				request.Input[0].Role != "user" || len(request.Input[0].Content) != 1 || request.Input[0].Content[0].Text != "Report the fixture marker.") {
				t.Errorf("worker task was not forwarded as a plaintext user message: %+v", request.Input)
			}
			w := httptest.NewRecorder()
			if call == 1 {
				if bytes.Contains(request.Tools, []byte(`"encrypted"`)) || !bytes.Contains(request.Tools, []byte(`"spawn_agent"`)) {
					t.Errorf("unexpected Go tool schema: %s", request.Tools)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_item.done\ndata: "+`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"message\":\"Report the fixture marker.\"}"}}`+"\n\n")
			}
			writeZAITestSSE(w, "go-answer", nil)
			return w.Result(), nil
		}),
	}
	parent := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.3","input":[],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`))
	parent.Header.Set(codexTurnMetadataKey, `{"session_id":"session","thread_id":"parent","turn_id":"parent-turn","request_kind":"turn"}`)
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, parent)
	if w.Code != http.StatusOK || primaryCalls.Load() != 1 || goCalls.Load() != 1 {
		t.Fatalf("parent status=%d Z.ai=%d Go=%d", w.Code, primaryCalls.Load(), goCalls.Load())
	}
	if !strings.Contains(w.Body.String(), `"encrypted_function_args":[]`) {
		t.Fatalf("Codex did not receive the plaintext collaboration marker: %s", w.Body.String())
	}
	worker := http.Header{"X-Openai-Subagent": {"collab_spawn"}}
	worker.Set(codexTurnMetadataKey, `{"session_id":"session","thread_id":"worker","turn_id":"worker-turn","parent_thread_id":"parent","parent_turn_id":"parent-turn","request_kind":"turn"}`)
	const task = `{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Report the fixture marker."}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.3","input":[`+task+`]}`))
	r.Header = worker
	w = httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP worker status=%d body=%q", w.Code, w.Body.String())
	}
	c := openZAITestWebSocket(t, s, http.Header{"X-Openai-Subagent": {"collab_spawn"}})
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3","generate":false,"input":[]}`)); err != nil {
		t.Fatal(err)
	}
	prewarm := readZAITestCompletion(t, c)
	frame := fmt.Sprintf(`{"type":"response.create","model":"glm-5.3","previous_response_id":%q,"client_metadata":{"x-codex-turn-metadata":%q},"input":[%s]}`,
		prewarm.Response.ID, worker.Get(codexTurnMetadataKey), task)
	if err := c.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	if result := readZAITestCompletion(t, c); result.Response.ID != "go-answer" {
		t.Fatalf("WebSocket worker response=%q", result.Response.ID)
	}
	if primaryCalls.Load() != 1 || goCalls.Load() != 3 {
		t.Fatalf("worker routing Z.ai=%d Go=%d", primaryCalls.Load(), goCalls.Load())
	}
	encrypted := `{"model":"glm-5.3","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"opaque"}]}]}`
	r = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(encrypted))
	r.Header = worker
	w = httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, r)
	if w.Code != http.StatusBadRequest || primaryCalls.Load() != 1 || goCalls.Load() != 3 {
		t.Fatalf("encrypted task status=%d Z.ai=%d Go=%d", w.Code, primaryCalls.Load(), goCalls.Load())
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.3","input":[`+task+`]}`))
	r.Header = worker.Clone()
	r.Header.Set(codexTurnMetadataKey, `{"session_id":"session","thread_id":"unrelated","turn_id":"unrelated-turn","parent_thread_id":"parent","parent_turn_id":"other-turn","request_kind":"turn"}`)
	w = httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, r)
	if w.Code != http.StatusOK || primaryCalls.Load() != 2 || goCalls.Load() != 3 {
		t.Fatalf("unrelated worker status=%d Z.ai=%d Go=%d", w.Code, primaryCalls.Load(), goCalls.Load())
	}
}

func TestOpenCodeGoHTTPRelaysOversizedSSE(t *testing.T) {
	large := "event: response.output_text.delta\r\ndata: {\"delta\":\"" +
		strings.Repeat("x", zaiMaxSSEEventBytes) + "\"}\r\n\r\n"
	const call = "event: response.output_item.done\ndata: {\"item\":{\"type\":\"function_call\",\"namespace\":\"collaboration\",\"name\":\"spawn_agent\"}}\n\n"
	largeCompletion := "event: response.completed\ndata: {\"response\":{\"id\":\"go-answer\",\"padding\":\"" +
		strings.Repeat("y", zaiMaxSSEEventBytes) + "\"}}\n\n"
	s := &Server{
		Config: &Config{}, Catalog: openCodeGoTestCatalog(),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(large + call + largeCompletion)),
			}, nil
		}),
	}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"kimi-k3","input":[],"stream":true}`)))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.HasPrefix(body, large) || !strings.HasSuffix(body, largeCompletion) {
		t.Fatalf("oversized events changed or truncated: HTTP %d, %d bytes", w.Code, len(body))
	}
	if !strings.Contains(body[len(large):len(body)-len(largeCompletion)], `"encrypted_function_args":[]`) {
		t.Fatal("plaintext rewriting did not resume after the large event")
	}
}

func TestOpenCodeGoOmitsOnlyStandardAccessPrograms(t *testing.T) {
	for _, tc := range []struct {
		name, programmes string
		want             int
	}{
		{"standard", `{"cyber":"standard"}`, http.StatusOK},
		{"empty", `{}`, http.StatusOK},
		{"null", `null`, http.StatusOK},
		{"named", `{"cyber":"nonstandard"}`, http.StatusBadRequest},
		{"unknown", `{"cyber":"standard","other":"enabled"}`, http.StatusBadRequest},
		{"duplicate", `{"cyber":"nonstandard","cyber":"standard"}`, http.StatusBadRequest},
		{"invalid", `{"cyber":true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := &Server{
				Config: &Config{}, Catalog: openCodeGoTestCatalog(),
				SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
					calls++
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if _, exists := body["access_programs"]; exists || string(body["input"]) != `"Say PONG."` {
						t.Fatalf("Go request lost its input or still carries a Codex access programme")
					}
					return httptest.NewRecorder().Result(), nil
				}),
			}
			body := `{"model":"kimi-k3","input":"Say PONG.","access_programs":` + tc.programmes + `}`
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			if w.Code != tc.want || (calls == 1) != (tc.want == http.StatusOK) {
				t.Fatalf("status %d, dispatched %d requests", w.Code, calls)
			}
		})
	}
}

func TestOpenCodeGoTranslatedModelRequiresFullInput(t *testing.T) {
	for _, nativeAPI := range []string{"chat-completions", "responses"} {
		t.Run(nativeAPI, func(t *testing.T) {
			entry := openCodeGoTestEntry("fixture-model")
			entry.SharedBudget.NativeAPI = nativeAPI
			calls := 0
			s := &Server{
				Config: &Config{}, Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{entry}}),
				SharedBudgetRoutes: openCodeGoTestRoutes(func(*http.Request) (*http.Response, error) {
					calls++
					w := httptest.NewRecorder()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"go-first"}`)
					return w.Result(), nil
				}),
			}
			post := func(body string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
				return w
			}
			if w := post(`{"model":"fixture-model","input":"hello"}`); w.Code != http.StatusOK || calls != 1 {
				t.Fatalf("first response status=%d Go calls=%d", w.Code, calls)
			}
			w := post(`{"model":"fixture-model","previous_response_id":"go-first","input":"continue"}`)
			if nativeAPI == "responses" {
				if w.Code != http.StatusOK || calls != 2 {
					t.Fatalf("native continuation status=%d Go calls=%d", w.Code, calls)
				}
			} else if w.Code != http.StatusConflict || calls != 1 || !strings.Contains(w.Body.String(), "requires full input") {
				t.Fatalf("translated continuation status=%d Go calls=%d body=%q", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestOpenCodeGoOmitsToolOutputsWithoutCallIDs(t *testing.T) {
	var got []map[string]json.RawMessage
	s := &Server{
		Config: &Config{}, Catalog: openCodeGoTestCatalog(),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			var request struct{ Input []map[string]json.RawMessage }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			got = request.Input
			return httptest.NewRecorder().Result(), nil
		}),
	}
	body := `{"model":"kimi-k3","input":[{"type":"message","role":"user","content":"hello"},{"type":"function_call_output","output":"orphan"},{"type":"custom_tool_call_output","call_id":"","output":"orphan"},{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if w.Code != http.StatusOK || len(got) != 3 {
		t.Fatalf("status %d, forwarded items %d", w.Code, len(got))
	}
	if string(got[0]["type"]) != `"message"` || string(got[1]["type"]) != `"function_call"` || string(got[2]["call_id"]) != `"call-1"` {
		t.Fatalf("forwarded history changed: %v", got)
	}
}

func TestOpenCodeGoStreamOptionsRewritePreservesAbsentInput(t *testing.T) {
	var upstream map[string]json.RawMessage
	s := &Server{
		Config: &Config{}, Catalog: openCodeGoTestCatalog(),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(r.Body).Decode(&upstream); err != nil {
				t.Fatal(err)
			}
			return httptest.NewRecorder().Result(), nil
		}),
	}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"kimi-k3","stream_options":{"include_obfuscation":false}}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if _, ok := upstream["input"]; ok {
		t.Fatal("rewrite added absent input")
	}
}

func TestOpenCodeGoWebSocketFallbackKeepsContinuationOnGo(t *testing.T) {
	var primaryCalls, goCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	var session string
	const workspace = "go-workspace"
	var credential atomic.Pointer[SharedBudgetCredential]
	s := &Server{
		Config: &Config{ClaudeUpstream: primary.URL}, Catalog: openCodeGoTestCatalog(),
		ZAIUpstream: primary.URL, ZAIAPIKey: func() (string, error) { return "primary-key", nil },
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			assertOpenCodeGoRequest(t, r)
			var request struct {
				Input         []json.RawMessage
				Stream        bool
				Previous      string          `json:"previous_response_id"`
				StreamOptions json.RawMessage `json:"stream_options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream || request.Previous != "" {
				t.Errorf("invalid streamed request: %+v, %v", request, err)
			}
			if len(request.StreamOptions) != 0 {
				t.Error("Go received Responses-only stream_options")
			}
			call := goCalls.Add(1)
			if r.Header.Get("Authorization") != fmt.Sprintf("Bearer go-access-%d", call) || r.Header.Get("X-OpenCode-Org-Id") != workspace {
				t.Error("wrong OAuth bearer or workspace after token refresh")
			}
			if call == 1 {
				session = r.Header.Get("X-OpenCode-Session")
				if session == "" {
					t.Error("missing Go session")
				}
			} else if r.Header.Get("X-OpenCode-Session") != session || len(request.Input) != 3 {
				t.Error("Go continuation lost session or tool-call history")
			}
			w := httptest.NewRecorder()
			writeZAITestSSE(w, fmt.Sprintf("go-%d", call), []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{}"}`)})
			return w.Result(), nil
		}),
	}
	goRoute := s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo]
	goRoute.Credential = func(ctx context.Context) (SharedBudgetCredential, error) {
		return *credential.Load(), ctx.Err()
	}
	s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo] = goRoute
	conn := openZAITestWebSocket(t, s, http.Header{"X-Opencode-Org-Id": []string{"downstream-workspace"}})
	frames := []string{
		`{"type":"response.create","model":"glm-5.3","input":"hello","stream_options":{"include_obfuscation":false}}`,
		`{"type":"response.create","model":"glm-5.3","previous_response_id":"go-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}],"stream_options":{"include_obfuscation":false}}`,
	}
	for i, frame := range frames {
		credential.Store(&SharedBudgetCredential{Bearer: fmt.Sprintf("go-access-%d", i+1), Account: "go-member", WorkspaceID: workspace})
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatal(err)
		}
		event := readZAITestCompletion(t, conn)
		if event.Response.ID != fmt.Sprintf("go-%d", i+1) {
			t.Fatalf("unexpected response %q", event.Response.ID)
		}
	}
	credential.Store(&SharedBudgetCredential{Bearer: "go-access-2", Account: "go-member", WorkspaceID: "different-workspace"})
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3","previous_response_id":"go-2","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.ReadMessage()
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("WebSocket stayed open after workspace changed, read result: %v", err)
	}
	if primaryCalls.Load() != 1 || goCalls.Load() != 2 {
		t.Fatalf("first-party calls %d, Go calls %d", primaryCalls.Load(), goCalls.Load())
	}
}

func TestOpenCodeGoNoRedirectWithCredential(t *testing.T) {
	var calls int
	s := &Server{
		Catalog: openCodeGoTestCatalog(), Config: &Config{},
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls != 1 {
				return nil, errors.New("unexpected redirect")
			}
			w := httptest.NewRecorder()
			w.Header().Set("Location", "https://elsewhere.example")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return w.Result(), nil
		}),
	}
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"kimi-k3","input":"hello"}`)))
	if calls != 1 || w.Header().Get("Location") != "" {
		t.Fatal("OpenCode redirect escaped CQ")
	}
}

func openCodeGoTestCatalog() *modelregistry.Catalog {
	return modelregistry.NewCatalog(modelregistry.Snapshot{
		Entries: []modelregistry.Entry{
			{Provider: modelregistry.ProviderZAI, ID: "glm-5.3"},
			openCodeGoTestEntry("kimi-k3"),
		},
		SharedBudgetModels: map[string][]modelregistry.Entry{"glm-5.3": {openCodeGoTestEntry("glm-5.3")}, "kimi-k3": {openCodeGoTestEntry("kimi-k3")}},
	})
}

func openCodeGoTestEntry(model string) modelregistry.Entry {
	return modelregistry.Entry{Provider: modelregistry.ProviderOpenCodeGo, ID: model,
		SharedBudget: &modelregistry.SharedBudgetModel{ProviderName: "OpenCode Go"}}
}

func openCodeGoTestRoutes(transport openCodeGoTestTransport) map[modelregistry.Provider]SharedBudgetRoute {
	return map[modelregistry.Provider]SharedBudgetRoute{
		modelregistry.ProviderOpenCodeGo: {
			Name: "OpenCode Go", URL: modelregistry.OpenCodeGoBaseURL + "/openai/v1/responses",
			SessionHeader: "X-OpenCode-Session", WorkspaceHeader: "X-OpenCode-Org-Id",
			Credential: func(context.Context) (SharedBudgetCredential, error) {
				return SharedBudgetCredential{Bearer: "go-key"}, nil
			},
			Transport: transport, AllowFallback: true, OmitStreamOptions: true, OmitEmptyCallIDOutputs: true, PlaintextCollaboration: true,
		},
	}
}

type openCodeGoTestTransport func(*http.Request) (*http.Response, error)

func (f openCodeGoTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assertOpenCodeGoUpstream(t *testing.T, r *http.Request) {
	t.Helper()
	assertOpenCodeGoRequest(t, r)
	if r.Header.Get("Authorization") != "Bearer go-key" || r.Header.Get("X-OpenCode-Org-Id") != "" {
		t.Error("wrong Go key or workspace")
	}
}

func assertOpenCodeGoRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.URL.String() != modelregistry.OpenCodeGoBaseURL+"/openai/v1/responses" {
		t.Error("wrong Go endpoint")
	}
	for _, name := range []string{"Cookie", "Chatgpt-Account-Id", "X-Api-Key", "X-Codex-Turn-State"} {
		if r.Header.Get(name) != "" {
			t.Errorf("Go request received %s", name)
		}
	}
}

func TestOpenCodeGoFallbackAfterNativeCodexCapacityRejection(t *testing.T) {
	var goCalls int
	planner := &codexNativeHTTPPlannerStub{err: &CachedUsageLimitError{}}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Config: &Config{}, CodexNativeHTTP: handler,
		Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{
			Entries:            []modelregistry.Entry{{Provider: modelregistry.ProviderCodex, ID: "gpt-6-luna"}},
			SharedBudgetModels: map[string][]modelregistry.Entry{"gpt-6-luna": {openCodeGoTestEntry("gpt-6-luna")}},
		}),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			goCalls++
			assertOpenCodeGoUpstream(t, r)
			return httptest.NewRecorder().Result(), nil
		}),
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"model":"gpt-6-luna","input":[]}`, 200},
		{`{"model":"gpt-6-luna","input":[],"previous_response_id":"native-response"}`, 429},
	} {
		w := httptest.NewRecorder()
		s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("status %d, want %d; %s", w.Code, tc.status, w.Body.String())
		}
	}
	if planner.calls != 2 || goCalls != 1 {
		t.Fatalf("Codex planner calls %d, Go calls %d", planner.calls, goCalls)
	}
}

func TestOpenCodeGoWebSocketFallbackAfterNativeCodexCapacityRejection(t *testing.T) {
	var goCalls atomic.Int32
	broker := &codexTerminatingWebSocketHandler{
		plans:       &codexNativeHTTPPlannerStub{err: &CachedUsageLimitError{}},
		upstream:    &codexWSBrokerDialerStub{},
		upstreamURL: "wss://codex.example/responses",
	}
	s := &Server{
		Config:               &Config{ClaudeUpstream: "https://claude.example", CodexUpstream: "https://codex.example"},
		CodexRouting:         &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}},
		CodexWebSocketBroker: broker,
		Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{
			Entries:            []modelregistry.Entry{{Provider: modelregistry.ProviderCodex, ID: "gpt-5.6-sol"}},
			SharedBudgetModels: map[string][]modelregistry.Entry{"gpt-5.6-sol": {openCodeGoTestEntry("gpt-5.6-sol")}},
		}),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
			assertOpenCodeGoUpstream(t, r)
			var request map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["client_metadata"] != nil {
				t.Error("invalid request or Codex client metadata escaped CQ")
			}
			goCalls.Add(1)
			w := httptest.NewRecorder()
			writeZAITestSSE(w, "go-native-fallback", nil)
			return w.Result(), nil
		}),
	}
	conn := openZAITestWebSocket(t, s, nil)
	if err := conn.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame("turn-first", "")); err != nil {
		t.Fatal(err)
	}
	if got := readZAITestCompletion(t, conn).Response.ID; got != "go-native-fallback" || goCalls.Load() != 1 {
		t.Fatalf("completed response %q, Go calls %d", got, goCalls.Load())
	}
}

func TestSharedBudgetRegistrationAndFallbackPermission(t *testing.T) {
	const model = "glm-5.3"
	first := openCodeGoTestEntry(model)
	second := modelregistry.Entry{
		Provider: "test-bundle", ID: model,
		SharedBudget: &modelregistry.SharedBudgetModel{ProviderName: "Test Bundle"},
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	routes := openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
		t.Fatal("automatic fallback used route without permission")
		return nil, nil
	})
	route := routes[first.Provider]
	route.AllowFallback = false
	routes[first.Provider] = route

	s := &Server{
		Config: &Config{},
		Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{
			Entries:            []modelregistry.Entry{{Provider: modelregistry.ProviderZAI, ID: model}},
			SharedBudgetModels: map[string][]modelregistry.Entry{model: {first, second}},
		}),
		SharedBudgetRoutes: routes,
		ZAIUpstream:        primary.URL, ZAIAPIKey: func() (string, error) { return "primary-key", nil },
		CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
	}
	send := func() int {
		w := httptest.NewRecorder()
		s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3","input":"hello"}`)))
		return w.Code
	}
	if status := send(); status != http.StatusTooManyRequests {
		t.Fatalf("disabled fallback returned %d", status)
	}

	var calls int
	routes[second.Provider] = SharedBudgetRoute{
		Name: "Test Bundle", URL: "https://test-bundle.example/responses",
		Credential: func(context.Context) (SharedBudgetCredential, error) {
			return SharedBudgetCredential{Bearer: "test-bundle-key"}, nil
		},
		AllowFallback: true,
		Transport: openCodeGoTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "test-bundle.example" || r.Header.Get("Authorization") != "Bearer test-bundle-key" {
				t.Error("second provider received wrong endpoint or credentials")
			}
			return httptest.NewRecorder().Result(), nil
		}),
	}
	if status := send(); status != http.StatusOK || calls != 1 {
		t.Fatalf("registered fallback returned %d with %d calls", status, calls)
	}
}

func TestSharedBudgetHTTPContinuationBindsFallbackCredential(t *testing.T) {
	for _, tc := range []struct {
		name, mediaType, firstBody, continuation string
	}{
		{"JSON response ID", "application/json", `{"id":"go-first"}`, `"previous_response_id":"go-first","input":[]`},
		{"SSE encrypted reasoning", "text/event-stream",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"go-first\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"encrypted-reasoning\"}]}}\n\ndata: [DONE]\n\n",
			`"input":[{"type":"reasoning","encrypted_content":"encrypted-reasoning"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primaryCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if primaryCalls.Add(1) == 1 {
					w.WriteHeader(http.StatusTooManyRequests)
				}
			}))
			defer server.Close()
			var goCalls int
			routes := openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
				assertOpenCodeGoUpstream(t, r)
				goCalls++
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", tc.mediaType)
				_, _ = io.WriteString(w, tc.firstBody)
				return w.Result(), nil
			})
			s := &Server{
				Config: &Config{}, Catalog: openCodeGoTestCatalog(), SharedBudgetRoutes: routes,
				ZAIUpstream: server.URL, ZAIAPIKey: func() (string, error) { return "primary-key", nil },
				CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
			}
			send := func(tail string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3",`+tail+`}`))
				s.handleLegacyCodexResponsesRoute(w, r)
				return w
			}
			first := send(`"input":"hello"`)
			if first.Code != http.StatusOK || first.Body.String() != tc.firstBody {
				t.Fatalf("shared route changed response: %d %q", first.Code, first.Body.String())
			}
			if follow := send(tc.continuation); follow.Code != http.StatusOK {
				t.Fatalf("continuation: %d %s", follow.Code, follow.Body)
			}
			route := routes[modelregistry.ProviderOpenCodeGo]
			route.Credential = func(context.Context) (SharedBudgetCredential, error) {
				return SharedBudgetCredential{Bearer: "rotated-key"}, nil
			}
			routes[modelregistry.ProviderOpenCodeGo] = route
			if rotated := send(tc.continuation); rotated.Code != http.StatusConflict {
				t.Fatalf("rotated credential: %d %s", rotated.Code, rotated.Body)
			}
			if primaryCalls.Load() != 1 || goCalls != 2 {
				t.Fatalf("first-party calls %d, Go calls %d", primaryCalls.Load(), goCalls)
			}
		})
	}
}

func TestSharedBudgetHTTPOAuthContinuationFollowsMemberAndWorkspace(t *testing.T) {
	const workspace = "go-workspace"
	credential := SharedBudgetCredential{Bearer: "go-access-1", Account: "go-member", WorkspaceID: workspace}
	catalogue := &modelregistry.OpenCodeGoSource{
		Client: &http.Client{Transport: openCodeGoTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer go-access-1" || r.Header.Get("X-OpenCode-Org-Id") != workspace {
				t.Error("catalogue did not use the original OAuth login")
			}
			w := httptest.NewRecorder()
			_, _ = io.WriteString(w, `{"data":[{"id":"kimi-k3","type":"chat","limit":{"context":200000},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":10}}}]}`)
			return w.Result(), nil
		})},
		Credential: func(context.Context) (string, string, string, error) {
			return credential.Bearer, credential.Account, credential.WorkspaceID, nil
		},
	}
	fetched, err := catalogue.Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	routes := openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
		assertOpenCodeGoRequest(t, r)
		calls++
		if r.Header.Get("Authorization") != "Bearer "+credential.Bearer || r.Header.Get("X-OpenCode-Org-Id") != workspace {
			t.Error("wrong OAuth bearer or workspace")
		}
		var input struct {
			Previous string `json:"previous_response_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || (calls == 2 && input.Previous != "go-anchor") {
			t.Error("OAuth continuation did not retain the previous response ID")
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"go-anchor"}`)
		return w.Result(), nil
	})
	type requestContextKey struct{}
	goRoute := routes[modelregistry.ProviderOpenCodeGo]
	goRoute.RequireCatalogueScope = true
	goRoute.Credential = func(ctx context.Context) (SharedBudgetCredential, error) {
		if ctx.Value(requestContextKey{}) != "upstream" {
			t.Error("credential resolver did not receive request context")
		}
		return credential, nil
	}
	routes[modelregistry.ProviderOpenCodeGo] = goRoute
	profilePath := filepath.Join(t.TempDir(), "cache.jsonl")
	profiler, err := OpenCacheProfiler(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profiler.Close() })
	s := &Server{
		Config: &Config{}, Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{Entries: fetched.Entries}),
		SharedBudgetRoutes: routes, CacheProfiler: profiler,
	}
	send := func(tail string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"kimi-k3",`+tail+`}`))
		r.Header.Set("Authorization", "Bearer downstream-token")
		r.Header.Set("X-OpenCode-Org-Id", "downstream-workspace")
		r.Header.Set("X-OpenCode-Session", "go-test-session")
		r = r.WithContext(context.WithValue(r.Context(), requestContextKey{}, "upstream"))
		s.handleLegacyCodexResponsesRoute(w, r)
		return w.Code
	}
	if got := send(`"input":"hello"`); got != http.StatusOK {
		t.Fatalf("first turn status %d, want 200", got)
	}
	const continuation = `"previous_response_id":"go-anchor","input":[]`
	original := credential
	credential.Bearer = "go-access-2"
	if got := send(continuation); got != http.StatusOK {
		t.Fatalf("refreshed OAuth continuation status %d, want 200", got)
	}
	if got := send(`"input":"new request"`); got != http.StatusOK {
		t.Fatalf("refreshed OAuth new request status %d, want 200", got)
	}
	for _, otherCredential := range []SharedBudgetCredential{
		{Bearer: "go-access-2", Account: "different-member", WorkspaceID: workspace},
		{Bearer: "go-access-2", Account: "go-member", WorkspaceID: "different-workspace"},
		{Bearer: "go-access-2", WorkspaceID: workspace},
	} {
		credential = otherCredential
		if got := send(continuation); got != http.StatusConflict {
			t.Fatalf("changed credential continuation status %d, want 409", got)
		}
		if got := send(`"input":"new request"`); got != http.StatusServiceUnavailable {
			t.Fatalf("changed credential new request status %d, want 503", got)
		}
	}
	credential = original
	s.Catalog.Replace(modelregistry.Snapshot{})
	if got := send(continuation); got != http.StatusConflict {
		t.Fatalf("disabled model continuation status %d, want 409", got)
	}
	if calls != 3 {
		t.Fatalf("Go received %d requests, want 3", calls)
	}
	events := readCacheProfileTestEvents(t, profilePath)
	if len(events) != calls {
		t.Fatalf("recorded %d cache events, want %d", len(events), calls)
	}
	for _, event := range events {
		if event.Route != string(modelregistry.ProviderOpenCodeGo) || event.Usage.Known ||
			event.AccountHash == "" || event.AccountHash != events[0].AccountHash ||
			event.SessionHash == "" || event.SessionHash != events[0].SessionHash {
			t.Fatalf("incorrect shared-budget cache event: %+v", event)
		}
	}
}

func TestOpenCodeGoCatalogueRejectsKeyChangesBeforeRefresh(t *testing.T) {
	credential := SharedBudgetCredential{Bearer: "original-key"}
	catalogue := &modelregistry.OpenCodeGoSource{
		Client: &http.Client{Transport: openCodeGoTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer "+credential.Bearer {
				t.Error("catalogue request used a different key")
			}
			w := httptest.NewRecorder()
			_, _ = io.WriteString(w, `{"data":[
				{"id":"kimi-k3","type":"chat","limit":{"context":200000},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":10}}},
				{"id":"glm-5.3","type":"chat","limit":{"context":200000},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":10}}}
			]}`)
			return w.Result(), nil
		})},
		Credential: func(context.Context) (string, string, string, error) { return credential.Bearer, "", "", nil },
	}
	firstParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer firstParty.Close()
	var calls int
	routes := openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+credential.Bearer {
			t.Error("inference request used a different key")
		}
		w := httptest.NewRecorder()
		_, _ = io.WriteString(w, `{"id":"go-answer"}`)
		return w.Result(), nil
	})
	goRoute := routes[modelregistry.ProviderOpenCodeGo]
	goRoute.RequireCatalogueScope = true
	goRoute.Credential = func(context.Context) (SharedBudgetCredential, error) { return credential, nil }
	routes[modelregistry.ProviderOpenCodeGo] = goRoute
	s := &Server{
		Config: &Config{}, Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{}), SharedBudgetRoutes: routes,
		ZAIUpstream: firstParty.URL, ZAIAPIKey: func() (string, error) { return "first-party-key", nil },
	}
	refresher := &modelregistry.Refresher{
		Catalog:       s.Catalog,
		SharedBudgets: []modelregistry.SharedBudgetSource{{Provider: modelregistry.ProviderOpenCodeGo, Source: catalogue, RequireFresh: true}},
		ZAI: modelregistry.SourceFunc(func(context.Context) (modelregistry.SourceResult, error) {
			return modelregistry.SourceResult{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderZAI, ID: "glm-5.3", Source: modelregistry.SourceNative}}}, nil
		}),
	}
	refresh := func() {
		t.Helper()
		if _, err := refresher.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	check := func(model string, wantStatus, wantCalls int) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)))
		s.handleLegacyCodexResponsesRoute(w, r)
		if w.Code != wantStatus || calls != wantCalls {
			t.Errorf("%s returned HTTP %d with %d Go requests, want HTTP %d with %d requests", model, w.Code, calls, wantStatus, wantCalls)
		}
	}
	refresh()
	check("kimi-k3", http.StatusOK, 1)
	check("glm-5.3", http.StatusOK, 2)
	credential.Bearer = "replacement-key"
	check("kimi-k3", http.StatusServiceUnavailable, 2)
	check("glm-5.3", http.StatusTooManyRequests, 2)
	refresh()
	check("kimi-k3", http.StatusOK, 3)
	check("glm-5.3", http.StatusOK, 4)
}

func TestSharedBudgetWebSocketReconnectRejectsProviderOwnedState(t *testing.T) {
	s := &Server{
		Config: &Config{}, Catalog: openCodeGoTestCatalog(),
		SharedBudgetRoutes: openCodeGoTestRoutes(func(*http.Request) (*http.Response, error) {
			t.Error("reconnect dispatched a shared request")
			return nil, errors.New("unexpected upstream request")
		}),
		ZAIAPIKey: func() (string, error) {
			t.Error("reconnect attempted native account selection")
			return "", errors.New("unexpected native selection")
		},
	}
	s.sharedBudgetBindings.record(sharedBudgetBoundState{
		Provider: modelregistry.ProviderOpenCodeGo, Model: "glm-5.3",
	}, "prior-go-response", nil)

	for _, request := range []string{
		`{"type":"response.create","model":"glm-5.3","previous_response_id":"prior-go-response","input":[]}`,
		`{"type":"response.create","model":"kimi-k3","input":[{"type":"item_reference","id":"unbound-item"}]}`,
	} {
		client := openZAITestWebSocket(t, s, nil)
		if err := client.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("WebSocket close = %v, want policy violation", err)
		}
	}
}
