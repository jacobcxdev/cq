package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

type grokTestAuthority struct {
	mu         sync.Mutex
	accounts   []grok.Account
	used       map[grok.AccountID]float64
	calls      []grok.AccountID
	requests   [][]byte
	respond    func(http.ResponseWriter, *http.Request, []byte)
	onAccounts func()
}

func TestGrokNativeWebSearchPassesThroughWithoutFunctionConversion(t *testing.T) {
	// Installed Codex includes this built-in beside its function namespace
	// even when supports_standalone_web_search is false. Grok Responses supports
	// the same native type; it must not be dropped or renamed as a local tool.
	body := []byte(`{"model":"grok-test","input":"hello","tools":[{"type":"web_search","search_context_size":"medium","filters":{"allowed_domains":["example.com"]}},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]}]}`)
	encoded, codec, err := normaliseGrokRequest(body)
	if err != nil {
		t.Fatalf("native web search rejected: %v", err)
	}
	var request struct{ Tools []json.RawMessage }
	if json.Unmarshal(encoded, &request) != nil || len(request.Tools) != 2 {
		t.Fatal("native web search or local function disappeared")
	}
	if string(request.Tools[0]) != `{"filters":{"allowed_domains":["example.com"]},"search_context_size":"medium","type":"web_search"}` && string(request.Tools[0]) != `{"type":"web_search","search_context_size":"medium","filters":{"allowed_domains":["example.com"]}}` {
		t.Fatalf("native web search changed: %s", request.Tools[0])
	}
	if len(codec.tools) != 1 {
		t.Fatal("native web search registered as a local function")
	}
	response := []byte(`{"type":"response.completed","response":{"output":[{"type":"web_search_call","id":"search-1","status":"completed","action":{"type":"search","query":"example"}}]}}`)
	output, err := codec.normaliseResponse(response)
	if err != nil || !bytes.Contains(output, []byte(`"type":"web_search_call"`)) {
		t.Fatalf("native search output rejected: %v", err)
	}
}

func TestGrokWebSearchProjectsLiveAccessAndRejectsCachedOnly(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{{"true", true}, {"false", false}, {`"true"`, false}, {"null", false}} {
		t.Run(test.value, func(t *testing.T) {
			body := []byte(`{"model":"grok-test","input":"hello","tools":[{"type":"web_search","external_web_access":` + test.value + `,"filters":{"allowed_domains":["example.com"]}}]}`)
			encoded, _, err := normaliseGrokRequest(body)
			if !test.valid {
				if err == nil {
					t.Fatal("unsupported or invalid search access accepted")
				}
				return
			}
			if err != nil || bytes.Contains(encoded, []byte("external_web_access")) || !bytes.Contains(encoded, []byte(`"allowed_domains":["example.com"]`)) {
				t.Fatalf("live search projection changed semantics: %v", err)
			}
		})
	}
}

func TestGrokReasoningReplayOmitsOutputOnlyStatus(t *testing.T) {
	encoded, _, err := normaliseGrokRequest([]byte(`{"model":"grok-test","input":[{"type":"reasoning","id":"reason-1","status":"completed","encrypted_content":"native-state","summary":[],"content":[{"type":"reasoning_text","text":"retained reasoning"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if json.Unmarshal(encoded, &replay) != nil || len(replay.Input) != 1 {
		t.Fatal("reasoning lost")
	}
	item := replay.Input[0]
	if _, exists := item["status"]; exists {
		t.Fatal("output-only reasoning status replayed")
	}
	if string(item["encrypted_content"]) != `"native-state"` || string(item["id"]) != `"reason-1"` || !bytes.Contains(item["content"], []byte("retained reasoning")) {
		t.Fatal("native reasoning changed")
	}
}

func TestGrokReasoningReplayOmitsNullCodexMetadata(t *testing.T) {
	for _, metadata := range []string{"null", `"foreign-private-state"`} {
		body := []byte(`{"model":"grok-test","input":[{"type":"reasoning","id":"reason-1","summary":[],"encrypted_content":"native-state","content":null,"internal_chat_message_metadata_passthrough":` + metadata + `}]}`)
		encoded, _, err := normaliseGrokRequest(body)
		if metadata != "null" {
			if err == nil {
				t.Fatal("private metadata silently discarded")
			}
			continue
		}
		if err != nil || bytes.Contains(encoded, []byte(`"content":null`)) || bytes.Contains(encoded, []byte("internal_chat_message_metadata_passthrough")) || !bytes.Contains(encoded, []byte(`"encrypted_content":"native-state"`)) {
			t.Fatalf("native reasoning projection failed: %v", err)
		}
	}
}

func TestGrokRestoresVerifiedNativeReasoningContainer(t *testing.T) {
	state := grokBoundState{Account: "account", Model: "grok-test"}
	var bindings grokStateBindings
	bindings.record(state, "response", []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"native-reason","summary":[],"content":[],"encrypted_content":"native-state","status":"completed"}`)})
	body := []byte(`{"model":"grok-test","input":[{"type":"reasoning","id":"native-reason","summary":[],"content":null,"encrypted_content":"native-state","internal_chat_message_metadata_passthrough":{"client":"bookkeeping"}}]}`)
	restored, err := bindings.restoreReasoning(body, state)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := normaliseGrokRequest(restored)
	if err != nil || bytes.Contains(encoded, []byte("bookkeeping")) || !bytes.Contains(encoded, []byte(`"content":[]`)) || !bytes.Contains(encoded, []byte(`"encrypted_content":"native-state"`)) {
		t.Fatalf("native reasoning replay failed: %v", err)
	}
	if _, err := bindings.restoreReasoning(body, grokBoundState{Account: "other", Model: "grok-test"}); err == nil {
		t.Fatal("cross-account reasoning restored")
	}
	for _, changed := range []string{
		`{"type":"reasoning","encrypted_content":"native-state","summary":[{"type":"summary_text","text":"new caller content"}]}`,
		`{"type":"reasoning","encrypted_content":"native-state","id":"different"}`,
		`{"type":"reasoning","encrypted_content":"native-state","new_field":"new caller content"}`,
		`{"type":"reasoning","encrypted_content":"unknown"}`,
	} {
		request := []byte(`{"model":"grok-test","input":[` + changed + `]}`)
		if _, err := bindings.restoreReasoning(request, state); err == nil {
			t.Fatal("changed or unknown native reasoning silently replaced")
		}
	}
}

func TestGrokNativeReasoningCacheHasByteBound(t *testing.T) {
	state := grokBoundState{Account: "account", Model: "grok-test"}
	var bindings grokStateBindings
	for i := 0; i < 12; i++ {
		item, _ := json.Marshal(map[string]any{"type": "reasoning", "encrypted_content": fmt.Sprintf("opaque-%d", i), "content": strings.Repeat("x", 1<<20)})
		bindings.record(state, fmt.Sprintf("response-%d", i), []json.RawMessage{item})
		if bindings.reasoningBytes > maxRequestBody {
			t.Fatal("native replay cache exceeded request byte budget")
		}
	}
	if len(bindings.reasoning) >= 12 || len(bindings.reasoning) != len(bindings.opaque) {
		t.Fatal("native replay eviction left inconsistent bindings")
	}
}

func TestGrokRejectsRepeatedNativeReasoningExpansion(t *testing.T) {
	state := grokBoundState{Account: "account", Model: "grok-test"}
	var bindings grokStateBindings
	item, _ := json.Marshal(map[string]any{"type": "reasoning", "encrypted_content": "native-state", "summary": []any{}, "content": strings.Repeat("x", 1<<20)})
	bindings.record(state, "response", []json.RawMessage{item})
	var references []json.RawMessage
	for i := 0; i < 12; i++ {
		references = append(references, json.RawMessage(`{"type":"reasoning","encrypted_content":"native-state"}`))
	}
	body, _ := json.Marshal(map[string]any{"input": references})
	if _, err := bindings.restoreReasoning(body, state); err == nil {
		t.Fatal("repeated native reasoning expanded beyond request budget")
	}
}

func TestGrokNoToolsOmitsNoneChoice(t *testing.T) {
	encoded, _, err := normaliseGrokRequest([]byte(`{"model":"grok-test","input":"summarize","tool_choice":"none"}`))
	if err != nil || bytes.Contains(encoded, []byte("tool_choice")) {
		t.Fatalf("redundant unsupported tool choice retained: %v", err)
	}
	encoded, _, err = normaliseGrokRequest([]byte(`{"model":"grok-test","input":"hello","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"tool_choice":"none"}`))
	if err != nil || !bytes.Contains(encoded, []byte(`"tool_choice":"none"`)) {
		t.Fatal("tool prohibition lost when tools are present")
	}
}

func (a *grokTestAuthority) Accounts(context.Context) ([]grok.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.onAccounts != nil {
		a.onAccounts()
	}
	return append([]grok.Account(nil), a.accounts...), nil
}

func TestGrokRoutingUsesNativeHTTPAdmissionBeforeProviderReads(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%t", compact), func(t *testing.T) {
			planner := &codexNativeHTTPPlannerStub{err: fmt.Errorf("Grok must not enter Codex planner")}
			handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
			if err != nil {
				t.Fatal(err)
			}
			a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{}}
			reads := 0
			a.onAccounts = func() {
				reads++
				handler.requests.mu.Lock()
				active := handler.requests.active
				handler.requests.mu.Unlock()
				if active != 1 {
					t.Errorf("provider reads outside native admission: active=%d", active)
				}
			}
			s := grokTestServer(a)
			s.CodexNativeHTTP = handler
			path := "/responses"
			if compact {
				path += "/compact"
				a.respond = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					writeZAICompactionTestSSE(w, "compact", []json.RawMessage{json.RawMessage(zaiTestSummaryMessage)})
				}
			}
			serve := s.handleLegacyCodexResponsesRoute
			if compact {
				serve = s.handleLegacyCodexCompactResponsesRoute
			}
			w := httptest.NewRecorder()
			serve(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"grok-test","input":"hello"}`)))
			if w.Code != 200 || planner.calls != 0 || reads == 0 {
				t.Fatalf("status=%d planner=%d provider_reads=%d body=%s", w.Code, planner.calls, reads, w.Body.String())
			}
			if err := handler.CloseAndDrain(context.Background()); err != nil {
				t.Fatal(err)
			}
			previousReads := reads
			w = httptest.NewRecorder()
			serve(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"grok-test","input":"hello"}`)))
			if w.Code != 503 || reads != previousReads || planner.calls != 0 {
				t.Fatalf("closed admission status=%d provider_reads=%d before=%d planner=%d", w.Code, reads, previousReads, planner.calls)
			}
		})
	}
}
func (a *grokTestAuthority) Do(_ context.Context, id grok.AccountID, r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.URL.Host != "cli-chat-proxy.grok.com" {
		return nil, fmt.Errorf("unexpected host")
	}
	w := httptest.NewRecorder()
	switch r.URL.Path {
	case "/v1/billing":
		fmt.Fprintf(w, `{"config":{"creditUsagePercent":%g,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q}}}`, a.used[id], time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), time.Now().Add(167*time.Hour).UTC().Format(time.RFC3339))
	case "/v1/models":
		io.WriteString(w, `{"data":[{"id":"grok-test","apiBackend":"responses","contextWindow":500000}]}`)
	case "/v1/responses", "/v1/responses/compact":
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("ChatGPT-Account-Id") != "" {
			return nil, fmt.Errorf("caller credentials leaked")
		}
		body, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, id)
		a.requests = append(a.requests, body)
		if a.respond != nil {
			a.respond(w, r, body)
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"resp-%d","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, len(a.calls))
		}
	default:
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	}
	return w.Result(), nil
}
func grokTestServer(a *grokTestAuthority) *Server {
	return &Server{Config: &Config{LocalToken: "local-test"}, GrokAuthority: a, Catalog: modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderGrok, ID: "grok-test", ContextWindow: 500000, Source: modelregistry.SourceNative}}})}
}
func TestGrokHTTPRoutesSubscriptionBeforeCodex(t *testing.T) {
	a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a", Active: true}}, used: map[grok.AccountID]float64{}}
	s := grokTestServer(a)
	r := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-test","input":"hello"}`))
	r.Header.Set("Authorization", "Bearer caller")
	r.Header.Set("Cookie", "secret")
	r.Header.Set("ChatGPT-Account-Id", "wrong")
	w := httptest.NewRecorder()
	s.handleLegacyCodexResponsesRoute(w, r)
	if w.Code != 200 || len(a.calls) != 1 || a.calls[0] != "a" {
		t.Fatalf("status=%d body=%s calls=%v", w.Code, w.Body.String(), a.calls)
	}
}
func TestGrokExhaustedDoesNotUsePaidOverage(t *testing.T) {
	a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{"a": 100}}
	s := grokTestServer(a)
	w := httptest.NewRecorder()
	s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-test","input":"hello"}`)))
	if w.Code != 429 || len(a.calls) != 0 {
		t.Fatalf("status=%d calls=%v body=%s", w.Code, a.calls, w.Body.String())
	}
}
func TestGrokContinuationPinsAccount(t *testing.T) {
	a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}, {ID: "b", UserID: "user-b"}}, used: map[grok.AccountID]float64{}}
	s := grokTestServer(a)
	for _, body := range []string{`{"model":"grok-test","input":"hello"}`, `{"model":"grok-test","previous_response_id":"resp-1","input":"again"}`} {
		w := httptest.NewRecorder()
		s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		a.mu.Lock()
		a.accounts = []grok.Account{{ID: "b", UserID: "user-b"}, {ID: "a", UserID: "user-a"}}
		a.mu.Unlock()
	}
	if len(a.calls) != 2 || a.calls[1] != "a" {
		t.Fatalf("calls=%v", a.calls)
	}
	w := httptest.NewRecorder()
	s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-test","previous_response_id":"foreign","input":"bad"}`)))
	if w.Code != 400 || len(a.calls) != 2 {
		t.Fatalf("foreign continuation status=%d calls=%v", w.Code, a.calls)
	}
}
func TestGrokWebSocketUsesSharedHTTPBridge(t *testing.T) {
	a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{}}
	a.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeZAITestSSE(w, fmt.Sprintf("resp-%d", len(a.calls)), []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`)})
	}
	s := grokTestServer(a)
	c := openZAITestWebSocket(t, s, nil)
	defer c.Close()
	for _, body := range []string{`{"type":"response.create","model":"grok-test","input":"hello"}`, `{"type":"response.create","model":"grok-test","previous_response_id":"resp-1","input":"again"}`} {
		if err := c.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
			t.Fatal(err)
		}
		_ = readZAITestCompletion(t, c)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) != 2 || strings.Contains(string(a.requests[1]), "previous_response_id") || !strings.Contains(string(a.requests[1]), "hello") {
		t.Fatalf("requests=%q", a.requests)
	}
}
func TestGrokCustomToolCodecRoundTrip(t *testing.T) {
	body := []byte(`{"model":"grok-test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"text"}}]}],"input":[{"type":"custom_tool_call","namespace":"functions","name":"apply_patch","call_id":"call-1","input":"patch text"},{"type":"custom_tool_call_output","call_id":"call-1","output":"done"}]}`)
	encoded, codec, err := normaliseGrokRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"type":"custom"`) || strings.Contains(string(encoded), `"namespace"`) || !strings.Contains(string(encoded), `\"input\":\"patch text\"`) {
		t.Fatalf("request=%s", encoded)
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		}
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"id":"r","output":[{"type":"function_call","name":%q,"call_id":"call-2","arguments":"{\"input\":\"another patch\"}"}]}`, request.Tools[0].Name))
	out, err := codec.normaliseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"custom_tool_call"`) || !strings.Contains(string(out), `"namespace":"functions"`) || !strings.Contains(string(out), `"input":"another patch"`) {
		t.Fatalf("response=%s", out)
	}
}

func TestGrokCompactionAndOpaqueContinuation(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprintf("standalone=%t", standalone), func(t *testing.T) {
			a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{}}
			requests := 0
			a.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
				requests++
				if r.URL.Path != "/v1/responses" {
					t.Errorf("wrong subscription endpoint: %s", r.URL.Path)
				}
				if requests == 1 {
					var req map[string]json.RawMessage
					if err := json.Unmarshal(body, &req); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(body), "compaction_trigger") || !strings.Contains(string(body), "keep this") || !strings.Contains(string(body), "instructions") || !strings.Contains(string(body), zaiCompactionPrompt) || len(req["tool_choice"]) != 0 || string(req["max_output_tokens"]) != "8192" || string(req["stream"]) != "true" || len(req["tools"]) != 0 || string(req["reasoning"]) != `{"effort":"low"}` {
						t.Errorf("invalid summary request: %s", body)
					}
					writeZAICompactionTestSSE(w, "summary-1", []json.RawMessage{json.RawMessage(zaiTestSummaryMessage)})
				} else {
					if strings.Contains(string(body), zaiCompactionPrefix) || !strings.Contains(string(body), "fixture-marker") {
						t.Errorf("CQ summary not decoded: %s", body)
					}
					io.WriteString(w, `{"id":"after-compact","output":[]}`)
				}
			}
			s := grokTestServer(a)
			w := httptest.NewRecorder()
			input := `[{"role":"user","content":"keep this"},{"type":"compaction_trigger"}]`
			serve := s.handleLegacyCodexResponsesRoute
			path := "/responses"
			if standalone {
				input = `[{"role":"user","content":"keep this"}]`
				serve = s.handleLegacyCodexCompactResponsesRoute
				path += "/compact"
			}
			serve(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"grok-test","instructions":"instructions","tools":[{"type":"custom","name":"run"}],"input":`+input+`}`)))
			if w.Code != 200 {
				t.Fatalf("compact=%d %s", w.Code, w.Body.String())
			}
			var out zaiCompactResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Output) != 1 {
				t.Fatalf("compact response=%s err=%v", w.Body.String(), err)
			}
			if standalone && out.Object != "response.compaction" {
				t.Errorf("compact object=%s", out.Object)
			}
			var item struct {
				Encrypted string `json:"encrypted_content"`
			}
			_ = json.Unmarshal(out.Output[0], &item)
			summary, err := openZAICompaction("local-test", item.Encrypted)
			if err != nil || summary != "Keep fixture-marker." {
				t.Fatalf("summary=%q err=%v", summary, err)
			}
			w = httptest.NewRecorder()
			s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-test","input":[`+string(out.Output[0])+`,{"role":"user","content":"continue"}]}`)))
			if w.Code != 200 {
				t.Fatalf("continuation=%d %s", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-test","input":[{"type":"compaction","encrypted_content":"foreign-opaque"}]}`)))
			if w.Code != 400 || len(a.calls) != 2 {
				t.Fatalf("foreign=%d calls=%v", w.Code, a.calls)
			}
		})
	}
}

func TestGrokUnknownSubscriptionNeverUsesCodex(t *testing.T) {
	s := &Server{Config: &Config{}, CodexNativeHTTP: zaiForbiddenCodexHTTP{t}}
	w := httptest.NewRecorder()
	s.handleLegacyCodexResponsesRoute(w, httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"grok-unknown","input":"hello"}`)))
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestGrokRejectsEncryptedToolArguments(t *testing.T) {
	_, _, err := normaliseGrokRequest([]byte(`{"model":"grok-test","input":[{"type":"function_call","name":"f","call_id":"c","arguments":"{}","encrypted_function_args":["opaque"]}]}`))
	if err == nil {
		t.Fatal("silently discarded encrypted tool arguments")
	}
}

func TestGrokToolSelectionRejectsUnknownOrConflictingTools(t *testing.T) {
	for _, body := range []string{
		`{"model":"grok-test","tools":[{"type":"function","name":"known"}],"tool_choice":{"type":"function","name":"unknown"},"input":"hello"}`,
		`{"model":"grok-test","tools":[{"type":"function","name":"same"},{"type":"function","name":"same"}],"input":"hello"}`,
		`{"model":"grok-test","tools":[{"type":"unsupported_hosted_tool"}],"input":"hello"}`,
		`{"model":"grok-test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"web_search"}]}],"input":"hello"}`,
	} {
		if _, _, err := normaliseGrokRequest([]byte(body)); err == nil {
			t.Fatalf("accepted invalid tool selection: %s", body)
		}
	}
}

func TestGrokCustomToolStreamPreservesInput(t *testing.T) {
	encoded, codec, err := normaliseGrokRequest([]byte(`{"model":"grok-test","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		}
	}
	_ = json.Unmarshal(encoded, &request)
	added := fmt.Sprintf(`{"type":"response.output_item.added","item":{"id":"item-1","type":"function_call","call_id":"call-1","name":%q,"arguments":""}}`, request.Tools[0].Name)
	data, err := codec.normaliseResponse([]byte(added))
	if err != nil || !strings.Contains(string(data), `"custom_tool_call"`) {
		t.Fatalf("added=%s err=%v", data, err)
	}
	data, err = codec.normaliseResponse([]byte(`{"type":"response.function_call_arguments.delta","item_id":"item-1","delta":"{\"input\":"}`))
	if err != nil || len(data) != 0 {
		t.Fatalf("partial JSON leaked: %s err=%v", data, err)
	}
	data, err = codec.normaliseResponse([]byte(`{"type":"response.function_call_arguments.done","item_id":"item-1","arguments":"{\"input\":\"patch\\ntext\"}"}`))
	if err != nil || !strings.Contains(string(data), `"response.custom_tool_call_input.delta"`) || !strings.Contains(string(data), `patch\ntext`) {
		t.Fatalf("input=%s err=%v", data, err)
	}
}

func TestGrokStateIdentityCollisionFailsClosed(t *testing.T) {
	var bindings grokStateBindings
	output := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}
	bindings.record(grokBoundState{Account: "a", Model: "grok-test"}, "same-id", output)
	bindings.record(grokBoundState{Account: "b", Model: "grok-test"}, "same-id", output)
	for _, body := range []string{`{"previous_response_id":"same-id"}`, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`} {
		if _, err := bindings.requestState([]byte(body), "grok-test"); err == nil {
			t.Fatalf("accepted conflicting account binding: %s", body)
		}
	}
}

func TestGrokCompactionRestoresNativeReasoning(t *testing.T) {
	a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{}}
	native := json.RawMessage(`{"type":"reasoning","encrypted_content":"native-cipher","summary":[],"id":"native-reasoning"}`)
	s := grokTestServer(a)
	s.grokBindings.record(grokBoundState{Account: "a", Model: "grok-test"}, "native-response", []json.RawMessage{native})
	a.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(string(body), "native-cipher") || strings.Contains(string(body), "internal_chat_message_metadata_passthrough") || strings.Contains(string(body), `"status":"completed"`) {
			t.Errorf("native reasoning was not restored before summary: %s", body)
		}
		writeZAICompactionTestSSE(w, "summary", []json.RawMessage{json.RawMessage(zaiTestSummaryMessage)})
	}
	body := `{"model":"grok-test","input":[{"type":"reasoning","encrypted_content":"native-cipher","summary":[],"id":"native-reasoning","status":"completed","internal_chat_message_metadata_passthrough":"codex-bookkeeping"},{"type":"compaction_trigger"}]}`
	result, err := s.grokResponse(context.Background(), nil, []byte(body), false)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Compact == nil || len(a.calls) != 1 {
		t.Fatalf("summary calls=%v compact=%v", a.calls, result.Compact)
	}
}

func TestGrokCompactionRejectsInvalidSummary(t *testing.T) {
	for _, output := range []string{`{"type":"function_call","name":"run","call_id":"call-1","arguments":"{}"}`, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`} {
		t.Run(output, func(t *testing.T) {
			a := &grokTestAuthority{accounts: []grok.Account{{ID: "a", UserID: "user-a"}}, used: map[grok.AccountID]float64{}}
			a.respond = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				writeZAICompactionTestSSE(w, "summary", []json.RawMessage{json.RawMessage(output)})
			}
			s := grokTestServer(a)
			_, err := s.grokResponse(context.Background(), nil, []byte(`{"model":"grok-test","input":[{"role":"user","content":"task"},{"type":"compaction_trigger"}]}`), false)
			if err == nil || len(a.calls) != 1 {
				t.Fatalf("err=%v calls=%v", err, a.calls)
			}
		})
	}
}
