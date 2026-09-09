//go:build !windows

package proxy

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestNormalProxyTransportWebSocketReserveRetriesWithinRequest(t *testing.T) {
	harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "reserve-ws-turn", RequestKind: CodexRequestTurn}
	status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
	if status != http.StatusOK {
		t.Fatalf("seed = %d %q", status, body)
	}
	now := time.Now()
	harness.httpPlanner.Capacity.ObserveQuotaSnapshot(codexInstalledHTTPValidationAccountA, QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 2, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err := harness.reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	frame := normalTransportGateWSFrame(metadata.TurnID, "")
	if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatal(err)
	}
	replies := normalTransportGateReadWSCompletion(t, connection)
	if bytes.Contains(bytes.Join(replies, nil), []byte("usage_limit_reached")) {
		t.Fatalf("reserve leaked: %q", replies)
	}
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 1 || frames[0].accountID != "validation-upstream-b" || frames[0].payload != string(frame) {
		t.Fatalf("frames = %#v, want only B", frames)
	}
	harness.backend.assertNoFailure(t)
}
