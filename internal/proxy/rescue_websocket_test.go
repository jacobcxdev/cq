package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRescueWebSocketRelaysConcurrentConnectionsWithoutNormalRuntime(t *testing.T) {
	var upstreamConnections atomic.Int32
	var upstreamAuthorization atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q", request.URL.Path)
			return
		}
		upstreamConnections.Add(1)
		upstreamAuthorization.Store(request.Header.Get("Authorization"))
		connection, err := (&websocket.Upgrader{
			CheckOrigin:       func(*http.Request) bool { return true },
			EnableCompression: true,
			Subprotocols:      []string{"responses"},
		}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		messageType, message, err := connection.ReadMessage()
		if err == nil {
			_ = connection.WriteMessage(messageType, message)
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	relay := &RescueRelay{
		Origin: testRescueOrigin(t),
		Transport: rescueRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			clone := request.Clone(request.Context())
			target := *request.URL
			target.Scheme = upstreamURL.Scheme
			target.Host = upstreamURL.Host
			clone.URL = &target
			clone.Host = upstreamURL.Host
			return http.DefaultTransport.RoundTrip(clone)
		}),
	}
	server := httptest.NewServer(relay)
	defer server.Close()
	dialTarget := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	header := http.Header{
		"Authorization":       {"Bearer opaque-websocket-token"},
		"User-Agent":          {"codex/0.147.0 (darwin 25.0; arm64) Terminal"},
		"Originator":          {"codex"},
		"Version":             {"0.147.0"},
		"OpenAI-Beta":         {"responses_websockets=2026-02-06"},
		"Session-Id":          {"session"},
		"Thread-Id":           {"thread"},
		"X-Client-Request-Id": {"request"},
		"X-Codex-Window-Id":   {"window"},
	}
	clients := make([]*websocket.Conn, 0, 2)
	for index := 0; index < 2; index++ {
		client, response, err := (&websocket.Dialer{
			HandshakeTimeout:  time.Second,
			EnableCompression: true,
			Subprotocols:      []string{"responses"},
		}).Dial(dialTarget, header)
		if err != nil {
			if response != nil {
				t.Fatalf("dial %d: %v (status %d)", index+1, err, response.StatusCode)
			}
			t.Fatalf("dial %d: %v", index+1, err)
		}
		if response.Header.Get("Sec-WebSocket-Extensions") == "" {
			t.Fatalf("connection %d compression was not negotiated", index+1)
		}
		clients = append(clients, client)
		defer client.Close()
	}
	for index, client := range clients {
		if client.Subprotocol() != "responses" {
			t.Fatalf("connection %d subprotocol = %q", index+1, client.Subprotocol())
		}
		if err := client.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		messageType, message, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.TextMessage || string(message) != "hello" {
			t.Fatalf("connection %d message = %d %q", index+1, messageType, message)
		}
	}
	if upstreamConnections.Load() != 2 {
		t.Fatalf("upstream connections = %d", upstreamConnections.Load())
	}
	if got, _ := upstreamAuthorization.Load().(string); got != "Bearer opaque-websocket-token" {
		t.Fatalf("upstream authorization = %q", got)
	}
}
