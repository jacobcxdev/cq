package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestZCodeContextOverflowRetriesShorterReplay(t *testing.T) {
	const marker = "latest user request"
	oldText := strings.Repeat("x", 6<<20)
	fields := `"model":"glm-5.3-flash","input":[` +
		`{"role":"developer","content":"Keep this instruction."},` +
		`{"role":"user","content":"` + oldText + `"},` +
		`{"role":"user","content":"` + marker + `"},` +
		`{"type":"function_call","name":"lookup","call_id":"latest-call","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"latest-call","output":"latest result"}]`
	for _, test := range []struct {
		name, code string
		wantStatus int
		wantCalls  int
		overflow   bool
	}{
		{name: "prompt too long", code: `"1261"`, wantStatus: http.StatusOK, wantCalls: 2},
		{name: "unclassified rejection", code: `"1210"`, wantStatus: http.StatusBadRequest, wantCalls: 1},
		{name: "retry still exceeds context", code: `1261`, wantStatus: http.StatusBadRequest, wantCalls: 2, overflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			gateway := &zcode.Gateway{Version: zcode.ClientVersion, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				var projected struct {
					Messages []json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&projected); err != nil {
					t.Fatalf("decode projected replay: %v", err)
				}
				if calls == 1 {
					if r.ContentLength < int64(len(oldText)) {
						t.Fatal("first request omitted history before provider rejection")
					}
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"code":` + test.code + `,"message":"private upstream message"}}`)),
					}, nil
				}
				encoded, err := json.Marshal(projected)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{marker, "tool_use", "tool_result", "latest-call", "latest result"} {
					if !strings.Contains(string(encoded), want) {
						t.Errorf("shorter replay lost %q", want)
					}
				}
				if r.ContentLength >= int64(len(oldText)) {
					t.Errorf("retry body still contains oversized history: %d bytes", r.ContentLength)
				}
				if test.overflow {
					return makeResponse(http.StatusBadRequest, `{"error":{"code":"1261","message":"private upstream message"}}`), nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"answer","type":"message","role":"assistant","model":"glm-5.3-flash","content":[{"type":"text","text":"continued"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)),
				}, nil
			})}}
			server := &Server{
				Config: &Config{}, Catalog: zaiTestCatalog(), ZAIGateway: gateway,
				ZAIAccounts: func() ([]zaiprov.Account, error) {
					return []zaiprov.Account{{
						Transport: "zcode-start",
						OAuth:     &zcode.LoginSession{ID: "fixture", ZCodeJWT: "fake"},
					}}, nil
				},
			}
			request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader("{"+fields+"}"))
			response := httptest.NewRecorder()
			server.handleCodexResponsesRoute(response, request)
			if response.Code != test.wantStatus || calls != test.wantCalls {
				t.Fatalf("status=%d calls=%d, want %d and %d", response.Code, calls, test.wantStatus, test.wantCalls)
			}
			if strings.Contains(response.Body.String(), "private upstream message") {
				t.Fatal("upstream error body escaped the gateway")
			}
			if test.overflow && !strings.Contains(response.Body.String(), "context_length_exceeded") {
				t.Fatal("request did not report the provider's context overflow to Codex")
			}
			if test.wantStatus == http.StatusOK && !strings.Contains(response.Body.String(), "continued") {
				t.Fatal("retry did not return the completed turn")
			}
		})
	}
}

func TestZAIBoundsReplayWithLargeRequestEnvelope(t *testing.T) {
	old := strings.Repeat("x", (9<<20)-(256<<10))
	instructions := strings.Repeat("y", 1536<<10)
	fields := `"model":"glm-5.3-flash","instructions":"` + instructions +
		`","input":[{"role":"developer","content":"preamble"},` +
		`{"role":"user","content":"` + old + `"},` +
		`{"role":"user","content":"latest user request"},` +
		`{"type":"function_call","call_id":"latest-call","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"latest-call","output":"latest result"}]`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBody {
			t.Errorf("upstream body = %d bytes", r.ContentLength)
		}
		var got struct {
			Instructions string
			Input        []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if got.Instructions != instructions || len(got.Input) != 5 ||
			!strings.Contains(string(got.Input[0]), "preamble") ||
			!strings.Contains(string(got.Input[1]), "Earlier conversation omitted") ||
			!strings.Contains(string(got.Input[2]), "latest user request") ||
			!strings.Contains(string(got.Input[3]), "latest-call") ||
			!strings.Contains(string(got.Input[4]), "latest result") {
			t.Errorf("upstream lost request fields or current turn, input items = %d", len(got.Input))
		}
		writeZAITestSSE(w, "bounded-replay", nil)
	}))
	defer upstream.Close()
	server := &Server{
		Config: &Config{ClaudeUpstream: upstream.URL}, Catalog: zaiTestCatalog(), ZAIUpstream: upstream.URL,
		ZAIAPIKey: func() (string, error) { return "test-key", nil },
	}

	t.Run("websocket", func(t *testing.T) {
		client := openZAITestWebSocket(t, server, nil)
		if err := client.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create",`+fields+`}`)); err != nil {
			t.Fatal(err)
		}
		if got := readZAITestCompletion(t, client); got.Response.ID != "bounded-replay" {
			t.Fatalf("response id = %q", got.Response.ID)
		}
	})
	t.Run("http", func(t *testing.T) {
		body := `{"stream":true,` + fields + `}`
		request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.handleCodexResponsesRoute(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "bounded-replay") {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
	})
	t.Run("http continuation", func(t *testing.T) {
		body := `{"stream":true,"previous_response_id":"previous",` + fields + `}`
		request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.handleCodexResponsesRoute(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
	})
}

func TestZAIWebSocketRejectsOversizedNormalisedBody(t *testing.T) {
	const prefix = `{"model":"glm-5.3-flash","stream":true,"input":[{"type":"agent_message","content":[{"type":"input_text","text":"`
	const suffix = `"}]}]}`
	body := []byte(prefix + strings.Repeat("x", maxRequestBody-1-len(prefix)-len(suffix)) + suffix)
	if len(body) != maxRequestBody-1 {
		t.Fatalf("original body size = %d", len(body))
	}
	normalised, err := normaliseZAIRequest(body)
	if err != nil || len(normalised) <= maxRequestBody {
		t.Fatalf("test body did not grow beyond limit: bytes=%d, error=%v", len(normalised), err)
	}
	var upstreamCalls, credentialCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		writeZAITestSSE(w, "should-not-run", nil)
	}))
	defer upstream.Close()
	server := &Server{
		ZAIUpstream: upstream.URL,
		ZAIAPIKey: func() (string, error) {
			credentialCalls.Add(1)
			return "test-key", nil
		},
	}
	finished := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		_, _, err = server.streamZAIWebSocketResponse(r.Context(), connection, r.Header, body)
		finished <- err
	}))
	defer listener.Close()
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(listener.URL, "http"), nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Type != "error" || event.Status != http.StatusRequestEntityTooLarge || event.Error.Type != "invalid_request_error" || event.Error.Message != "Z.ai request body exceeds 10 MiB" {
		t.Errorf("oversized request response = %s, error=%v", payload, err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Error("oversized request returned no error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("oversized request did not finish")
	}
	if got := credentialCalls.Load(); got != 0 {
		t.Errorf("oversized request read credentials %d times", got)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("oversized request reached upstream %d times", got)
	}
}
