package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestLegacyCodexWebSocketRecoversFromUpstreamDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name         string
		terminal     string
		clientCloses bool
		wantStatus   int
		wantStage    string
	}{
		{name: "upstream closes before completion", wantStatus: http.StatusUpgradeRequired, wantStage: "upstream_read"},
		{name: "upstream closes after completion", terminal: `{"type":"response.completed","response":{"id":"synthetic"}}`, wantStatus: http.StatusSwitchingProtocols, wantStage: "upstream_read"},
		{name: "upstream sends a quota error", terminal: `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, wantStatus: http.StatusSwitchingProtocols, wantStage: "upstream_read"},
		{name: "downstream closes first", clientCloses: true, wantStatus: http.StatusSwitchingProtocols, wantStage: "downstream_read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamReady := make(chan struct{})
			upgrader := websocket.Upgrader{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Error(err)
					return
				}
				close(upstreamReady)
				if tc.terminal != "" {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(tc.terminal)); err != nil {
						t.Error(err)
					}
				}
				if tc.clientCloses {
					_, _, _ = conn.ReadMessage()
				}
			}))
			defer upstream.Close()

			tracePath := filepath.Join(t.TempDir(), "route.jsonl")
			trace, err := OpenDiagnosticsWriter(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			defer trace.Close()
			plan := requestPlan("one", "candidate")
			attempt := plan.Attempts[0]
			resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
				attempt.Revision: testExactCredentialMaterial(attempt.Identity, "selected"),
			}}
			server := &Server{
				Config:                 &Config{ClaudeUpstream: "https://claude.test", CodexUpstream: upstream.URL, LocalToken: "local-token"},
				CodexRequests:          &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}},
				CodexWebSocketExecutor: NewCodexWebSocketAttemptExecutor(nil, resolver),
				Diag:                   trace,
			}
			handler, err := server.handler()
			if err != nil {
				t.Fatal(err)
			}
			handlerDone := make(chan struct{}, 3)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { handlerDone <- struct{}{} }()
				handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()

			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			endpoint := "ws" + strings.TrimPrefix(proxy.URL, "http") + legacyCodexResponsesPath
			headers := http.Header{"Session_id": {"affected-session"}}
			client, _, err := dialer.Dial(endpoint, headers)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := []byte(`{"type":"response.create","model":"gpt-5.4","input":"synthetic","client_metadata":{"x-codex-turn-metadata":{"session_id":"affected-session","thread_id":"affected-thread","turn_id":"turn","request_kind":"turn"}}}`)
			if err := client.WriteMessage(websocket.TextMessage, request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-upstreamReady:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not receive the request")
			}
			if tc.clientCloses {
				_ = client.Close()
			} else {
				_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
				if tc.terminal != "" {
					if _, body, err := client.ReadMessage(); err != nil || string(body) != tc.terminal {
						t.Fatalf("terminal response = %s, err = %v", body, err)
					}
				}
				var closed *websocket.CloseError
				if _, _, err := client.ReadMessage(); !errors.As(err, &closed) {
					t.Fatalf("WebSocket close = %v", err)
				}
				if tc.wantStatus == http.StatusUpgradeRequired && closed.Code != websocket.CloseInternalServerErr {
					t.Errorf("close code = %d, want a close frame from CQ", closed.Code)
				}
			}
			select {
			case <-handlerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("proxy did not close the relay")
			}
			retry, response, err := dialer.Dial(endpoint, headers)
			if retry != nil {
				_ = retry.Close()
			}
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if response == nil || response.StatusCode != tc.wantStatus {
				t.Errorf("retry status = %v, err = %v, want %d", response, err, tc.wantStatus)
			}
			other, response, err := dialer.Dial(endpoint, http.Header{"Session_id": {"other-session"}})
			if err != nil {
				t.Fatalf("other session upgrade failed: %v", err)
			}
			_ = other.Close()
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Errorf("other session status = %d", response.StatusCode)
			}
			data, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			var sawRelayEnd bool
			for _, line := range strings.Split(string(data), "\n") {
				var event CodexTraceEvent
				if json.Unmarshal([]byte(line), &event) == nil && event.Phase == "relay_terminal" && event.Stage == tc.wantStage && event.CloseCode == websocket.CloseAbnormalClosure {
					sawRelayEnd = true
					if want := hashPrefix("codex-thread", "affected-thread"); event.ThreadKey != want {
						t.Errorf("relay trace thread = %q, want %q", event.ThreadKey, want)
					}
					if event.Retry != (tc.wantStatus == http.StatusUpgradeRequired) {
						t.Errorf("relay trace HTTP fallback = %v, retry status = %d", event.Retry, tc.wantStatus)
					}
				}
			}
			if !sawRelayEnd {
				t.Errorf("missing trace for first %s close", tc.wantStage)
			}
		})
	}
}
