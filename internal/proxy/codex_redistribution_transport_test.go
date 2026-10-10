//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// This catches retaining admitted account continuity after an explicit
// redistribution, or forwarding its displaced provider turn state to B.
func TestCodexRedistributionTransportHTTPMovesLatchedChatOnNextRequest(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "redistribute-http-session", ThreadID: "redistribute-http-thread", TurnID: "redistribute-http-turn", RequestKind: CodexRequestTurn}
	body := normalTransportGateHTTPBody(t, metadata)
	status, response := normalTransportGateHTTPCall(t, harness, body)
	if status != http.StatusOK {
		t.Fatalf("initial request = %d %q", status, response)
	}
	if receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http"); len(receipts) != 1 || receipts[0].accountID != "validation-upstream-a" {
		t.Fatalf("initial request did not establish A: %#v", receipts)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)

	staleState := http.Header{"X-Codex-Turn-State": {"state-validation-upstream-a"}}
	status, response = normalTransportGateHTTPCall(t, harness, body, staleState)
	if status != http.StatusOK {
		t.Fatalf("redistributed full request = %d %q, want 200", status, response)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 || receipts[1].accountID != "validation-upstream-b" || receipts[1].turnState != "" || receipts[1].payload != string(body) {
		t.Fatalf("redistribution receipts = %#v, want exactly A then B with unchanged full body and no stale turn state", receipts)
	}
	// Clients can keep presenting their old token after transparent migration.
	status, response = normalTransportGateHTTPCall(t, harness, body, staleState)
	if status != http.StatusOK {
		t.Fatalf("next request carrying displaced state = %d %q", status, response)
	}
	receipts = normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 3 || receipts[2].accountID != "validation-upstream-b" || receipts[2].turnState != "" {
		t.Fatalf("post-migration receipts = %#v, want B without stale turn state", receipts)
	}
	harness.backend.assertNoFailure(t)
}

// This catches sending a portable successor through an exhausted upstream,
// or closing the downstream socket instead of changing its upstream account.
func TestCodexRedistributionTransportWebSocketMovesPortableNextFrame(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	first := normalTransportGateWSFrame("redistribute-ws-portable-turn", "")
	if err := connection.WriteMessage(websocket.TextMessage, first); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, connection)
	if frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame"); len(frames) != 1 || frames[0].accountID != "validation-upstream-a" {
		t.Fatalf("initial frame did not establish A: %#v", frames)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)
	second := normalTransportGateWSFrame("redistribute-ws-portable-next-turn", "")
	if err := connection.WriteMessage(websocket.TextMessage, second); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, connection)
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 2 || frames[1].accountID != "validation-upstream-b" || frames[1].payload != string(second) {
		t.Fatalf("redistribution frames = %#v, want exactly A then unchanged full-create frame on B", frames)
	}
	harness.backend.assertNoFailure(t)
}

// This catches replaying an account-bound delta to either upstream after
// redistribution, rather than requesting full history before dispatch.
func TestCodexRedistributionTransportWebSocketDeltaResynchronisesBeforeDispatch(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSIdleHeldOpen)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("redistribute-ws-delta-turn", "")); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, connection)
	if frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame"); len(frames) != 1 || frames[0].accountID != "validation-upstream-a" {
		t.Fatalf("initial delta lane did not establish A: %#v", frames)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)
	delta := normalTransportGateWSFrame("redistribute-ws-delta-next-turn", `,"previous_response_id":"normal-transport-idle-held-one"`)
	if err := connection.WriteMessage(websocket.TextMessage, delta); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("redistribution delta = message type %d payload %q error %v, want pre-dispatch 1012", messageType, payload, err)
	}
	if frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame"); len(frames) != 1 {
		t.Fatalf("account-bound delta reached upstream: %#v", frames)
	}
	_ = connection.Close()

	retry := normalTransportGateWebSocket(t, harness)
	defer retry.Close()
	fullHistory := normalTransportGateWSFrame("redistribute-ws-delta-next-turn", "")
	fullHistory = bytes.Replace(fullHistory, []byte(`"input":[]`), []byte(`"input":[{"role":"user","content":[{"type":"input_text","text":"first turn"}]},{"role":"assistant","content":[{"type":"output_text","text":"first reply"}]},{"role":"user","content":[{"type":"input_text","text":"next turn"}]}]`), 1)
	if err := retry.WriteMessage(websocket.TextMessage, fullHistory); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, retry)
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 2 || frames[1].accountID != "validation-upstream-b" || frames[1].payload != string(fullHistory) {
		t.Fatalf("resynchronisation frames = %#v, want only initial A request and full history on B", frames)
	}
	// The held-open fixture records closure while awaiting its next delta.
	// Migration deliberately closes that idle upstream; all other errors fail.
	for {
		select {
		case failure := <-harness.backend.failures:
			if !strings.HasPrefix(failure.Error(), "provider WebSocket held-idle successor read:") {
				t.Fatal(failure)
			}
		default:
			return
		}
	}
}

// This catches treating redistribution as applicable only to retries of the
// completed turn, leaving a new turn pinned to the exhausted account.
func TestCodexRedistributionTransportHTTPMovesNextTurn(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "redistribute-next-session", ThreadID: "redistribute-next-thread", TurnID: "redistribute-first-turn", RequestKind: CodexRequestTurn}
	if status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata)); status != http.StatusOK {
		t.Fatalf("initial turn = %d %q", status, body)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)
	metadata.TurnID = "redistribute-next-turn"
	fullBody := normalTransportGateHTTPBody(t, metadata)
	status, body := normalTransportGateHTTPCall(t, harness, fullBody, http.Header{"X-Codex-Turn-State": {"state-validation-upstream-a"}})
	if status != http.StatusOK {
		t.Fatalf("next turn = %d %q, want 200", status, body)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[1].accountID != "validation-upstream-b" || receipts[1].turnState != "" || receipts[1].payload != string(fullBody) {
		t.Fatalf("next turn receipts = %#v, want A then new turn once on B without A turn state", receipts)
	}
	harness.backend.assertNoFailure(t)
}

// This catches consuming redistribution without a positive alternative, or
// discarding the marker before a subsequently reset account becomes usable.
func TestCodexRedistributionTransportHTTPDefersWithoutPositiveAlternative(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "redistribute-pending-session", ThreadID: "redistribute-pending-thread", TurnID: "redistribute-pending-turn", RequestKind: CodexRequestTurn}
	body := normalTransportGateHTTPBody(t, metadata)
	if status, response := normalTransportGateHTTPCall(t, harness, body); status != http.StatusOK {
		t.Fatalf("initial request = %d %q", status, response)
	}
	redistributionTransportQuota(t, harness, 0)
	redistributionTransportSchedule(t, harness)
	staleState := http.Header{"X-Codex-Turn-State": {"state-validation-upstream-a"}}
	if status, response := normalTransportGateHTTPCall(t, harness, body, staleState); status != http.StatusOK {
		t.Fatalf("pending request without positive quota = %d %q, want old account 200", status, response)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 || receipts[1].accountID != "validation-upstream-a" || receipts[1].turnState != "state-validation-upstream-a" {
		t.Fatalf("deferred receipts = %#v, want old upstream with valid state", receipts)
	}
	snapshot := redistributionTransportSnapshot(t, harness, metadata)
	if snapshot.RedistributionGeneration == 0 || snapshot.BoundAccountKey != codexInstalledHTTPValidationAccountA {
		t.Fatalf("redistribution prematurely consumed: %+v", snapshot)
	}
	redistributionTransportResetQuota(t, harness)
	metadata.TurnID = "redistribute-pending-next-turn"
	if status, response := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata), staleState); status != http.StatusOK {
		t.Fatalf("pending request after positive quota = %d %q", status, response)
	}
	receipts = normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 3 || receipts[2].accountID != "validation-upstream-b" || receipts[2].turnState != "" {
		t.Fatalf("pending migration receipts = %#v, want B after reset without rescheduling", receipts)
	}
	harness.backend.assertNoFailure(t)
}

// This catches forcing client resynchronisation while every replacement
// account still has no quota, unnecessarily breaking a usable delta stream.
func TestCodexRedistributionTransportWebSocketDeltaDefersWithoutPositiveAlternative(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSIdleHeldOpen)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("redistribute-ws-pending-turn", "")); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, connection)
	redistributionTransportQuota(t, harness, 0)
	redistributionTransportSchedule(t, harness)
	delta := normalTransportGateWSFrame("redistribute-ws-pending-next-turn", `,"previous_response_id":"normal-transport-idle-held-one"`)
	if err := connection.WriteMessage(websocket.TextMessage, delta); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, connection)
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-a" || frames[0].connection != frames[1].connection || frames[1].payload != string(delta) {
		t.Fatalf("deferred delta frames = %#v, want unchanged delta on existing A connection", frames)
	}
	metadata := CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "redistribute-ws-pending-next-turn", RequestKind: CodexRequestTurn}
	if snapshot := redistributionTransportSnapshot(t, harness, metadata); snapshot.RedistributionGeneration == 0 {
		t.Fatalf("delta without positive replacement consumed redistribution: %+v", snapshot)
	}
	harness.backend.assertNoFailure(t)
}

// This catches checking redistribution only when this downstream socket has
// an active upstream, allowing a reconnected client's first delta to dispatch.
func TestCodexRedistributionTransportWebSocketFirstReconnectedDeltaResynchronisesBeforeDispatch(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	initial := normalTransportGateWebSocket(t, harness)
	if err := initial.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("redistribute-reconnect-first-turn", "")); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, initial)
	_ = initial.Close()
	if frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame"); len(frames) != 1 || frames[0].accountID != "validation-upstream-a" {
		t.Fatalf("initial reconnect lane did not establish A: %#v", frames)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)

	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	delta := normalTransportGateWSFrame("redistribute-reconnect-next-turn", `,"previous_response_id":"normal-transport-websocket"`)
	if err := connection.WriteMessage(websocket.TextMessage, delta); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("first reconnected delta = message type %d payload %q error %v, want pre-dispatch 1012", messageType, payload, err)
	}
	receipts := harness.backend.snapshot()
	if frames := normalTransportGateReceipts(receipts, "websocket_frame"); len(frames) != 1 {
		t.Fatalf("first reconnected delta reached upstream: %#v", frames)
	}
	if handshakes := normalTransportGateReceipts(receipts, "websocket_handshake"); len(handshakes) != 1 {
		t.Fatalf("first reconnected delta opened upstream: %#v", handshakes)
	}
	_ = connection.Close()

	retry := normalTransportGateWebSocket(t, harness)
	defer retry.Close()
	fullHistory := normalTransportGateWSFrame("redistribute-reconnect-next-turn", "")
	fullHistory = bytes.Replace(fullHistory, []byte(`"input":[]`), []byte(`"input":[{"role":"user","content":[{"type":"input_text","text":"first turn"}]},{"role":"assistant","content":[{"type":"output_text","text":"first reply"}]},{"role":"user","content":[{"type":"input_text","text":"next turn"}]}]`), 1)
	if err := retry.WriteMessage(websocket.TextMessage, fullHistory); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, retry)
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 2 || frames[1].accountID != "validation-upstream-b" || frames[1].payload != string(fullHistory) {
		t.Fatalf("reconnected resynchronisation frames = %#v, want initial A then full history once on B", frames)
	}
	harness.backend.assertNoFailure(t)
}

// This catches filtering out a recovered account because an earlier turn
// durably exhausted it, requiring another failing request on A to recover B.
func TestCodexRedistributionTransportHTTPProbesRecoveredHistoricalAccount(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "redistribute-historical-session", ThreadID: "redistribute-historical-thread", TurnID: "redistribute-historical-predecessor", RequestKind: CodexRequestTurn}
	seedHTTPQuotaExclusion(t, harness, metadata, codexInstalledHTTPValidationAccountB)
	metadata.TurnID = "redistribute-historical-current"
	if status, response := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata)); status != http.StatusOK {
		t.Fatalf("initial admitted A = %d %q", status, response)
	}
	snapshot := redistributionTransportSnapshot(t, harness, metadata)
	if !containsCodexHTTPRequestAccountKey(snapshot.QuotaExhaustedAccountKeys, codexInstalledHTTPValidationAccountB) {
		t.Fatalf("fixture lost historical B quota fence: %+v", snapshot)
	}
	redistributionTransportResetQuota(t, harness)
	redistributionTransportSchedule(t, harness)
	snapshot = redistributionTransportSnapshot(t, harness, metadata)
	if !containsCodexHTTPRequestAccountKey(snapshot.QuotaExhaustedAccountKeys, codexInstalledHTTPValidationAccountB) {
		t.Fatalf("quota observation or scheduling prematurely cleared B fence: %+v", snapshot)
	}
	metadata.TurnID = "redistribute-historical-next"
	fullBody := normalTransportGateHTTPBody(t, metadata)
	status, response := normalTransportGateHTTPCall(t, harness, fullBody, http.Header{"X-Codex-Turn-State": {"state-validation-upstream-a"}})
	if status != http.StatusOK {
		t.Fatalf("recovered account migration = %d %q", status, response)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[1].accountID != "validation-upstream-b" || receipts[1].turnState != "" || receipts[1].payload != string(fullBody) {
		t.Fatalf("historical reset receipts = %#v, want A then one direct full request on B", receipts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot = redistributionTransportSnapshot(t, harness, metadata)
		if snapshot.BoundRequestCompleted || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !snapshot.BoundRequestCompleted || containsCodexHTTPRequestAccountKey(snapshot.QuotaExhaustedAccountKeys, codexInstalledHTTPValidationAccountB) || snapshot.RedistributionGeneration != 0 {
		t.Fatalf("successful B completion did not clear probe fence and redistribution: %+v", snapshot)
	}
	harness.backend.assertNoFailure(t)
}

func redistributionTransportSnapshot(t *testing.T, harness *normalTransportGateHarness, metadata CodexTurnMetadata) CodexLeaseRouteSnapshot {
	t.Helper()
	snapshot, err := harness.continuity.LoadRouteSnapshot(context.Background(), NewCodexLeaseKey(metadata), []codex.AccountKey{codexInstalledHTTPValidationAccountA, codexInstalledHTTPValidationAccountB, codexInstalledHTTPValidationDefault}, harness.httpPlanner.Authority)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func redistributionTransportResetQuota(t *testing.T, harness *normalTransportGateHarness) {
	t.Helper()
	redistributionTransportQuota(t, harness, 100)
}

func redistributionTransportQuota(t *testing.T, harness *normalTransportGateHarness, remainingB int) {
	t.Helper()
	now := time.Now()
	for account, remaining := range map[codex.AccountKey]int{
		codexInstalledHTTPValidationAccountA: 0,
		codexInstalledHTTPValidationAccountB: remainingB,
		codexInstalledHTTPValidationDefault:  0,
	} {
		stream := harness.httpPlanner.Capacity.NewObservationStream()
		fact := stream.Stamp(CapacityFact{AccountKey: account, Bucket: CapacityBucketBase, RemainingPct: remaining, Source: CapacitySourceLiveRateLimits, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative})
		if !harness.httpPlanner.Capacity.Observe(fact) {
			t.Fatalf("reset quota fixture rejected account %s", account)
		}
	}
}

func redistributionTransportSchedule(t *testing.T, harness *normalTransportGateHarness) {
	t.Helper()
	result, err := harness.continuity.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ScheduledLeases != 1 {
		t.Fatalf("scheduled redistribution = %+v, want one established chat", result)
	}
}
