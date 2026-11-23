package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

const zaiTestSummaryMessage = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Keep fixture-marker."}]}`

func TestCQCompactionRestoredBeforeCodexHTTPWithoutZAI(t *testing.T) {
	item, err := sealZAICompaction("owner-token", "Keep fixture-marker.")
	if err != nil {
		t.Fatal(err)
	}
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
			capture := &cqCompactionCapturingHTTP{}
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

type cqCompactionCapturingHTTP struct{ body, encoding string }

func (capture *cqCompactionCapturingHTTP) TryServe(w http.ResponseWriter, r *http.Request, _ bool) (bool, string) {
	body, err := readCodexNativeHTTPRequest(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return true, ""
	}
	capture.body, capture.encoding = string(body), r.Header.Get("Content-Encoding")
	w.WriteHeader(http.StatusNoContent)
	return true, "gpt-test"
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

func TestZAICompactionBoundsOversizedReplay(t *testing.T) {
	var upstreamCalls int
	s := &Server{
		Config: &Config{LocalToken: "private-proxy-token"}, Catalog: zaiTestCatalog(),
		ZAIAPIKey: func() (string, error) { return "test-key", nil },
		ZAITransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			upstreamCalls++
			if r.ContentLength > maxRequestBody {
				t.Errorf("compaction request is oversized: %d bytes", r.ContentLength)
			}
			var request struct{ Input []json.RawMessage }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode compaction request: %v", err)
			}
			if len(request.Input) != 3 ||
				!bytes.Contains(request.Input[0], []byte("Earlier conversation omitted")) ||
				!bytes.Contains(request.Input[1], []byte("latest user request")) ||
				!bytes.Contains(request.Input[2], []byte(zaiCompactionPrompt)) {
				t.Fatalf("compaction lost its latest turn or instruction, got %d items", len(request.Input))
			}
			w := httptest.NewRecorder()
			writeZAICompactionTestSSE(w, "summary-response", []json.RawMessage{json.RawMessage(zaiTestSummaryMessage)})
			return w.Result(), nil
		}),
	}
	body := `{"model":"glm-5.3-flash","input":[{"role":"user","content":"` +
		strings.Repeat("x", 11<<20) +
		`"},{"role":"user","content":"latest user request"},{"type":"compaction_trigger"}]}`
	w := httptest.NewRecorder()
	s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)))
	if w.Code != http.StatusOK || upstreamCalls != 1 || !bytes.Contains(w.Body.Bytes(), []byte(zaiCompactionPrefix)) {
		t.Fatalf("status=%d calls=%d, want a saved compaction item", w.Code, upstreamCalls)
	}
}

func TestZAICompactionFallsBackToOpenCodeGo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		webSocket bool
		wantGo    int32
	}{
		{"HTTP quota rejection", http.StatusTooManyRequests, false, 1},
		{"WebSocket quota rejection", http.StatusTooManyRequests, true, 1},
		{"initial SSE quota rejection", http.StatusOK, false, 1},
		{"ambiguous upstream error", http.StatusBadGateway, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var zaiCalls, goCalls atomic.Int32
			s := &Server{
				Config:    &Config{ClaudeUpstream: "https://claude.test", LocalToken: "private-proxy-token"},
				Catalog:   openCodeGoTestCatalog(),
				ZAIAPIKey: func() (string, error) { return "zai-key", nil },
				ZAITransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					zaiCalls.Add(1)
					w := httptest.NewRecorder()
					if tc.status == http.StatusOK {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\n")
					} else {
						w.WriteHeader(tc.status)
					}
					return w.Result(), nil
				}),
				SharedBudgetRoutes: openCodeGoTestRoutes(func(r *http.Request) (*http.Response, error) {
					goCalls.Add(1)
					assertOpenCodeGoUpstream(t, r)
					var request struct {
						Input  json.RawMessage
						Stream bool
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream ||
						!bytes.Contains(request.Input, []byte(zaiCompactionPrompt)) || bytes.Contains(request.Input, []byte("compaction_trigger")) {
						t.Error("Go did not receive the compact summary request")
					}
					if tc.webSocket && !bytes.Contains(request.Input, []byte("Earlier compacted history.")) {
						t.Error("Go did not receive the restored CQ summary")
					}
					w := httptest.NewRecorder()
					writeZAICompactionTestSSE(w, "go-summary", []json.RawMessage{json.RawMessage(zaiTestSummaryMessage)})
					return w.Result(), nil
				}),
			}
			const request = `"model":"glm-5.3","input":[{"role":"user","content":"Remember fixture-marker."},{"type":"compaction_trigger"}]`
			if tc.webSocket {
				conn := openZAITestWebSocket(t, s, nil)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"glm-5.3","input":[],"generate":false}`)); err != nil {
					t.Fatal(err)
				}
				prewarm := readZAITestCompletion(t, conn).Response.ID
				sealed, err := sealZAICompaction(s.cqCompactionToken(), "Earlier compacted history.")
				if err != nil {
					t.Fatal(err)
				}
				frame := fmt.Sprintf(`{"type":"response.create","model":"glm-5.3","previous_response_id":%q,"input":[%s,{"type":"compaction_trigger"}]}`, prewarm, sealed)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					t.Fatal(err)
				}
				response := readZAITestCompletion(t, conn).Response
				if !strings.HasPrefix(response.ID, "cq_zai_compaction_") {
					t.Fatalf("missing compaction state in response %q", response.ID)
				}
			} else {
				w := httptest.NewRecorder()
				s.handleCodexResponsesRoute(w, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{`+request+`}`)))
				if tc.wantGo != 0 && (w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(zaiCompactionPrefix))) {
					t.Fatalf("missing compact state: status %d, response %s", w.Code, w.Body.String())
				}
				if tc.wantGo == 0 && w.Code != http.StatusBadGateway {
					t.Fatalf("unexpected status %d", w.Code)
				}
			}
			if zaiCalls.Load() != 1 || goCalls.Load() != tc.wantGo {
				t.Fatalf("Z.ai calls %d, Go calls %d, want 1 and %d", zaiCalls.Load(), goCalls.Load(), tc.wantGo)
			}
		})
	}
}

func writeZAICompactionTestSSE(w http.ResponseWriter, id string, output []json.RawMessage) {
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output}})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
