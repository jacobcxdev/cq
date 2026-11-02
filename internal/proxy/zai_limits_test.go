package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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
