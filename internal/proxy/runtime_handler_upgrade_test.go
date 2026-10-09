package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestRuntimeHandlerUpgradeWithoutWebSocketBroker(t *testing.T) {
	for _, test := range []struct {
		name   string
		broker CodexWebSocketRoutingHandler
		want   error
	}{
		{name: "passthrough"},
		{name: "unknown broker", broker: &codexWebSocketRoutingHandlerStub{}, want: ErrRuntimeUpgradeUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{Config: &Config{ClaudeUpstream: "https://example.test"}, CodexWebSocketBroker: test.broker}
			handler, err := server.RuntimeHandler()
			if err != nil {
				t.Fatal(err)
			}
			worker := handler.(RuntimeUpgradeWorker)
			if err := worker.PrepareUpgrade(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("PrepareUpgrade = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRuntimeHandlerUpgradeWaitsForPassthroughWebSocketClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			kind, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if err := connection.WriteMessage(kind, body); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	server := &Server{
		Config: &Config{ClaudeUpstream: upstream.URL, CodexUpstream: upstream.URL},
		CodexUpgradeTransport: &legacyCodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "test-codex-token"}},
			Inner:    http.DefaultTransport,
		},
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
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := connection.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, body, err := connection.ReadMessage(); err != nil || string(body) != "ping" {
		t.Fatalf("passthrough echo = %q, %v", body, err)
	}
	if err := worker.PrepareUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := worker.AwaitUpgradeQuiescence(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active passthrough socket quiescence = %v", err)
	}
	connection.Close()
	wait, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	if err := worker.AwaitUpgradeQuiescence(wait); err != nil {
		t.Fatalf("closed passthrough socket quiescence = %v", err)
	}
	if err := worker.ResumeUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
}
