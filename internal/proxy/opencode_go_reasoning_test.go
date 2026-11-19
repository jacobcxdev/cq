package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestOpenCodeGoSequentialReasoningCompletesBeforeNextItem(t *testing.T) {
	const reasoning = `{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"First complete sentence. Then finish."}]}`
	const tool = `{"type":"function_call","id":"fc","call_id":"call-1","name":"echo","arguments":"{}"}`
	const output = `{"type":"function_call_output","call_id":"call-1","output":"ok"}`
	const input = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	const body = `"model":"glm-5.3","input":[` + input + `],"stream":true,"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}`
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"go-1","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","sequence_number":2,"item_id":"rs","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","sequence_number":3,"item_id":"rs","output_index":0,"summary_index":0,"delta":"First complete sentence"}`,
		`{"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs","output_index":0,"summary_index":0,"delta":". Then finish."}`,
		`{"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":` + tool + `}`,
		`{"type":"response.reasoning_summary_text.done","sequence_number":6,"item_id":"rs","output_index":0,"summary_index":0,"text":"First complete sentence. Then finish."}`,
		`{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":` + reasoning + `}`,
		`{"type":"response.completed","sequence_number":8,"response":{"id":"go-1","output":[` + reasoning + `,` + tool + `]}}`,
	}
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			catalog := openCodeGoTestCatalog().Snapshot()
			entry := openCodeGoTestEntry("glm-5.3")
			entry.SharedBudget.NativeAPI = "chat-completions"
			catalog.SharedBudgetModels["glm-5.3"] = []modelregistry.Entry{entry}
			s := &Server{
				Config: &Config{}, Catalog: modelregistry.NewCatalog(catalog),
				SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
					var request struct {
						Input         []json.RawMessage
						StreamOptions json.RawMessage `json:"stream_options"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					if len(request.StreamOptions) > 0 {
						t.Error("Go received client-only stream_options")
					}
					w := httptest.NewRecorder()
					w.Header().Set("Content-Type", "text/event-stream")
					if len(request.Input) > 1 {
						if n := len(request.Input); n < 3 || string(request.Input[n-3]) != reasoning || string(request.Input[n-2]) != tool || string(request.Input[n-1]) != output {
							t.Error("tool continuation changed provider reasoning or call history")
						}
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"go-2\",\"output\":[]}}\n\n")
					} else {
						for _, event := range events {
							_, _ = io.WriteString(w, "data: "+event+"\n\n")
						}
					}
					return w.Result(), nil
				}),
			}
			var received [][]byte
			switch transport {
			case "http":
				w := httptest.NewRecorder()
				s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader("{"+body+"}")))
				if w.Code != http.StatusOK {
					t.Fatalf("HTTP status = %d", w.Code)
				}
				observations, err := ParseCodexSSE(w.Body.Bytes(), zaiMaxSSEEventBytes)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range observations {
					received = append(received, event.Data)
				}
				var completed struct {
					Response struct{ Output []json.RawMessage }
				}
				if err := json.Unmarshal(received[len(received)-1], &completed); err != nil || len(completed.Response.Output) != 2 {
					t.Fatal("missing completed tool history")
				}
				continuation := `{"model":"glm-5.3","stream":true,"input":[` + input + "," +
					string(completed.Response.Output[0]) + "," + string(completed.Response.Output[1]) + "," + output + "]}"
				followup := httptest.NewRecorder()
				s.handleCodexResponsesRoute(followup, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(continuation)))
				if followup.Code != http.StatusOK || !strings.Contains(followup.Body.String(), `"id":"go-2"`) {
					t.Fatal("HTTP tool continuation did not complete")
				}
			case "websocket":
				conn := openZAITestWebSocket(t, s, nil)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create",`+body+"}")); err != nil {
					t.Fatal(err)
				}
				for {
					_, frame, err := conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					received = append(received, frame)
					if strings.Contains(string(frame), `"response.completed"`) {
						break
					}
				}
				continuation := `{"type":"response.create","model":"glm-5.3","previous_response_id":"go-1","input":[` + output + `]}`
				if err := conn.WriteMessage(websocket.TextMessage, []byte(continuation)); err != nil {
					t.Fatal(err)
				}
				if got := readZAITestCompletion(t, conn).Response.ID; got != "go-2" {
					t.Fatalf("continuation response = %q", got)
				}
			}
			var completedSections []string
			var nextItemStarted bool
			for index, data := range received {
				var event struct {
					Type         string
					Text         string
					Sequence     int `json:"sequence_number"`
					SummaryIndex int `json:"summary_index"`
					Item         struct{ Type string }
				}
				if err := json.Unmarshal(data, &event); err != nil {
					t.Fatal(err)
				}
				if event.Sequence != index {
					t.Fatalf("sequence number = %d at event %d", event.Sequence, index)
				}
				if event.Type == "response.reasoning_summary_text.done" {
					if nextItemStarted || event.SummaryIndex != len(completedSections) {
						t.Fatal("summary section completed late or out of order")
					}
					completedSections = append(completedSections, event.Text)
				}
				if event.Type == "response.output_item.added" && event.Item.Type == "function_call" {
					nextItemStarted = true
				}
			}
			if len(completedSections) != 2 || strings.Join(completedSections, "") != "First complete sentence. Then finish." {
				t.Fatalf("completed summary sections before tool start = %q", completedSections)
			}
			if got := string(received[len(received)-1]); !strings.Contains(got, reasoning) {
				t.Fatal("final response changed provider reasoning")
			}
		})
	}
}

func TestOpenCodeGoSequentialReasoningLeavesOtherStreamsAlone(t *testing.T) {
	const stream = "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"summary_index\":0,\"item_id\":\"rs\",\"delta\":\"First complete sentence. \"}\n\n"
	for _, tc := range []struct {
		name, nativeAPI, options string
		otherAccount             bool
	}{
		{"ordinary delivery", "chat-completions", "", false},
		{"native Responses", "responses", `,"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}`, false},
		{"another account translates", "responses", `,"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}`, true},
		{"explicit suppression", "chat-completions", `,"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},"reasoning":{"summary":"none"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := openCodeGoTestCatalog().Snapshot()
			entry := openCodeGoTestEntry("glm-5.3")
			entry.SharedBudget.NativeAPI = tc.nativeAPI
			snapshot.SharedBudgetModels["glm-5.3"] = []modelregistry.Entry{entry}
			if tc.otherAccount {
				entry.SharedBudget.CredentialScope = (SharedBudgetCredential{Bearer: "go-key"}).binding()
				other := openCodeGoTestEntry("glm-5.3")
				other.SharedBudget.NativeAPI = "chat-completions"
				other.SharedBudget.CredentialScope = (SharedBudgetCredential{Bearer: "other-key"}).binding()
				snapshot.SharedBudgetModels["glm-5.3"] = []modelregistry.Entry{other, entry}
			}
			s := &Server{
				Config: &Config{}, Catalog: modelregistry.NewCatalog(snapshot),
				SharedBudgetRoutes: openCodeGoTestRoutes(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(stream)),
					}, nil
				}),
			}
			if tc.otherAccount {
				route := s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo]
				route.RequireCatalogueScope = true
				s.SharedBudgetRoutes[modelregistry.ProviderOpenCodeGo] = route
			}
			w := httptest.NewRecorder()
			body := `{"model":"glm-5.3","input":[{"type":"message","role":"user","content":[]}],"stream":true` + tc.options + "}"
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)))
			if w.Code != http.StatusOK || w.Body.String() != stream {
				t.Fatal("unrequested sequential summary conversion")
			}
		})
	}
}
