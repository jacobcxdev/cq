package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type rescueWebSocketDialerFunc func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)

func (dial rescueWebSocketDialerFunc) DialContext(ctx context.Context, target string, header http.Header) (*websocket.Conn, *http.Response, error) {
	return dial(ctx, target, header)
}

func TestRescueWebSocketRelaysOneConnectionWithoutNormalRuntime(t *testing.T) {
	var upstreamConnections atomic.Int32
	var upstreamAuthorization atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
	upstreamURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	relay := &RescueRelay{
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		Budget: NewRescueBudget(time.Now, [sha256.Size]byte{6}),
		DialWS: rescueWebSocketDialerFunc(func(ctx context.Context, target string, header http.Header) (*websocket.Conn, *http.Response, error) {
			if target != "wss://chatgpt.com/backend-api/codex/responses" {
				t.Fatalf("upstream target = %q", target)
			}
			return (&websocket.Dialer{
				HandshakeTimeout:  time.Second,
				EnableCompression: true,
				Subprotocols:      []string{"responses"},
			}).DialContext(ctx, upstreamURL, header)
		}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		request.Host = "127.0.0.1:29280"
		relay.ServeHTTP(writer, request)
	}))
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
	client, response, err := (&websocket.Dialer{
		HandshakeTimeout:  time.Second,
		EnableCompression: true,
		Subprotocols:      []string{"responses"},
	}).Dial(dialTarget, header)
	if err != nil {
		if response != nil {
			t.Fatalf("dial: %v (status %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	defer client.Close()
	if client.Subprotocol() != "responses" {
		t.Fatalf("subprotocol = %q", client.Subprotocol())
	}
	if response.Header.Get("Sec-WebSocket-Extensions") == "" {
		t.Fatal("compression was not negotiated")
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	messageType, message, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || string(message) != "hello" {
		t.Fatalf("message = %d %q", messageType, message)
	}
	if upstreamConnections.Load() != 1 {
		t.Fatalf("upstream connections = %d", upstreamConnections.Load())
	}
	if got, _ := upstreamAuthorization.Load().(string); got != "Bearer opaque-websocket-token" {
		t.Fatalf("upstream authorization = %q", got)
	}
}

func TestRescueBudgetPreservesOwnerWebSocketCapacity(t *testing.T) {
	budget := NewRescueBudget(time.Now, [sha256.Size]byte{7})
	releases := make([]func(), 0, 4)
	for index := 0; index < 3; index++ {
		release, err := budget.AcquireWebSocket(context.Background(), RescueIngressUnverified, []byte("bearer-"+string(rune('a'+index))))
		if err != nil {
			t.Fatalf("unverified %d: %v", index, err)
		}
		releases = append(releases, release)
	}
	if release, err := budget.AcquireWebSocket(context.Background(), RescueIngressUnverified, []byte("bearer-over")); !errors.Is(err, ErrRescueCapacity) || release != nil {
		t.Fatalf("fourth unverified = release:%v err:%v", release != nil, err)
	}
	release, err := budget.AcquireWebSocket(context.Background(), RescueIngressOwnerPermitted, nil)
	if err != nil {
		t.Fatal(err)
	}
	releases = append(releases, release)
	if release, err := budget.AcquireWebSocket(context.Background(), RescueIngressOwnerPermitted, nil); !errors.Is(err, ErrRescueCapacity) || release != nil {
		t.Fatalf("fifth total = release:%v err:%v", release != nil, err)
	}
	for _, release := range releases {
		release()
	}
}
