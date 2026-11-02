package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const zaiTestSummaryMessage = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Keep fixture-marker."}]}`

func TestCQCompactionRestoredBeforeCodexHTTPWithoutZAI(t *testing.T) {
	item, err := sealZAICompaction("owner-token", "Keep fixture-marker.")
	if err != nil {
		t.Fatal(err)
	}
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
			capture := &zaiCapturingCodexHTTP{}
			s := &Server{Config: &Config{LocalToken: "owner-token"}, CodexNativeHTTP: capture}
			body := []byte(`{"model":"gpt-test","input":[` + string(item) + `]}`)
			if compressed {
				body = encodeCodexZstd(t, body)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			if compressed {
				r.Header.Set("Content-Encoding", "zstd")
			}
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, r)
			if w.Code != http.StatusNoContent || !strings.Contains(capture.body, "fixture-marker") || strings.Contains(capture.body, zaiCompactionPrefix) || capture.encoding != "" {
				t.Fatalf("CQ state not restored before Codex dispatch: status=%d", w.Code)
			}
		})
	}
}

func TestZAICompactionRejectsUnfinishedSummary(t *testing.T) {
	for _, response := range []string{
		`{"id":"r","output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"incomplete","output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"failed","output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"completed","error":{},"output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"completed","incomplete_details":{},"output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"failed","status":"completed","output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"completed","output":[],"output":[` + zaiTestSummaryMessage + `]}`,
		`{"id":"r","status":"completed","output":[{"type":"message","role":"user","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`,
		`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial","text":"summary"}]}]}`,
	} {
		t.Run(fmt.Sprintf("case-%d", len(response)), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
			}))
			defer upstream.Close()
			s := &Server{Config: &Config{LocalToken: "private-proxy-token"}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "test-key", nil }, ZAIUpstream: upstream.URL}
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"glm-5.3-flash","input":[{"type":"compaction_trigger"}]}`)))
			if w.Code != http.StatusBadGateway || bytes.Contains(w.Body.Bytes(), []byte(zaiCompactionPrefix)) {
				t.Fatalf("unfinished summary produced compact state: status=%d", w.Code)
			}
		})
	}
}

func TestZAICompactionTriggerHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeZAICompactionTestSSE(w, "summary-response", []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Retain fixture-marker and finish the pending check."}]}`)})
			}))
			defer upstream.Close()
			s := &Server{Config: &Config{LocalToken: "private-proxy-token"}, Catalog: zaiTestCatalog(), ZAIAPIKey: func() (string, error) { return "test-key", nil }, ZAIUpstream: upstream.URL}
			body := fmt.Sprintf(`{"model":"glm-5.3-flash","stream":%t,"input":[{"role":"user","content":"Remember fixture-marker."},{"type":"compaction_trigger"}]}`, stream)
			w := httptest.NewRecorder()
			s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Fatalf("compact status = %d", w.Code)
			}
			data := w.Body.Bytes()
			if stream {
				events, err := NewCodexSSEParser(1 << 20).Feed(data)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type == "response.completed" {
						var envelope struct {
							Response json.RawMessage `json:"response"`
						}
						if err := json.Unmarshal(event.Data, &envelope); err != nil {
							t.Fatal(err)
						}
						data = envelope.Response
					}
				}
			}
			var result struct {
				Output []struct {
					Type      string `json:"type"`
					Encrypted string `json:"encrypted_content"`
				} `json:"output"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Output) != 1 || result.Output[0].Type != "compaction" || result.Output[0].Encrypted == "" {
				t.Fatalf("expected one actual compaction item, got %d ordinary output items", len(result.Output))
			}
		})
	}
}

func writeZAICompactionTestSSE(w http.ResponseWriter, id string, output []json.RawMessage) {
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output}})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
