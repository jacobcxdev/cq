package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTerminatingWSBrokerRotatesPortableFrameBeforeAdmission(t *testing.T) {
	t.Parallel()
	receiptStore, err := NewCodexTurnReceiptStore(strings.NewReader(strings.Repeat("r", 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	receiptValue := testCodexTurnReceipt()
	receiptValue.Transport = CodexTurnReceiptTransportWebSocket
	receipt := receiptStore.register([]byte("session-a"), []byte("turn-a"), receiptValue)
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		receipt: receipt,
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
	}
	frame := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-a","thread_id":"thread-a","turn_id":"turn-a","request_kind":"turn"}},"input":[]}`)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {upstreamA},
		"account-b": {upstreamB},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	ctx := withCodexWSFrameObservationSink(context.Background(), func(*routeDiagnostics) { accepted++ })
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("accepted frame observations = %d, want 1 across rotation", accepted)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := upstreamA.writtenPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], frame) {
		t.Fatalf("A writes = %#v", got)
	}
	if got := upstreamB.writtenPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], frame) {
		t.Fatalf("B writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"response-b"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`),
	}) {
		t.Fatalf("downstream writes = %#v", got)
	}
	if !upstreamA.closed {
		t.Fatal("replaced upstream was not closed")
	}
	gotReceipt, found := receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if !found || gotReceipt.State != CodexTurnReceiptCompleted || gotReceipt.ActualAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("rotated receipt = (%+v, %v)", gotReceipt, found)
	}
}

func TestCodexTerminatingWSBrokerReleasesDispatchedRequestAfterUpgradeAuthorityFailure(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}}}
	upstream := &codexWSBrokerConnStub{}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{
			conn: upstream,
			response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{
				"X-Codex-Turn-State": {"state-a", "state-b"},
			}},
		}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err == nil {
		t.Fatal("invalid upstream authority succeeded")
	}

	next, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-a", planner.slots))
	if err != nil {
		t.Fatalf("request after broker failure: %v", err)
	}
	if _, err := next.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexTerminatingWSBrokerReleasesDispatchedRequestAfterUpgradeAdmissionFailure(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}}}
	upstream := &codexWSBrokerConnStub{}
	dialer := &codexWSBrokerDialerStub{
		onDial: cancel,
		outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
			"account-a": {{
				conn: upstream,
				response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{
					"X-Codex-Turn-State": {"state-a"},
				}},
			}},
		},
	}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(ctx, downstream); !errors.Is(err, context.Canceled) {
		t.Fatalf("upgrade admission failure = %v, want cancellation", err)
	}

	assertCodexWSBrokerLaneReleased(t, runtimeLease, planner.slots)
}

func TestCodexTerminatingWSBrokerReleasesDispatchedRequestAfterFrameAdmissionFailure(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.created","response":{"id":"` + strings.Repeat("x", codexTurnIDMaxBytes+1) + `"}}`),
	}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("frame admission failure = %v, want invalid mutation", err)
	}

	assertCodexWSBrokerLaneReleased(t, runtimeLease, planner.slots)
}

func TestCodexTerminatingWSBrokerReleasesDispatchedRequestAfterDownstreamWriteFailure(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	downstream := &codexWSBrokerConnStub{
		reads:    []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}},
		writeErr: errors.New("downstream unavailable"),
	}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.created","response":{"id":"response-a"}}`),
	}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err == nil || !strings.Contains(err.Error(), "downstream WebSocket write failed") {
		t.Fatalf("downstream write failure = %v", err)
	}

	assertCodexWSBrokerLaneReleased(t, runtimeLease, planner.slots)
}

func TestCodexTerminatingWSBrokerDrainsTerminalRequestAfterDownstreamWriteFailure(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	downstream := &codexWSBrokerConnStub{
		reads:       []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}},
		writeErr:    errors.New("downstream unavailable"),
		failWriteAt: 2,
	}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err == nil || !strings.Contains(err.Error(), "downstream WebSocket write failed") {
		t.Fatalf("downstream write failure = %v", err)
	}

	assertCodexWSBrokerLaneReleased(t, runtimeLease, planner.slots)
}

func TestCodexTerminatingWSBrokerRetainsLaneUntilFailedUpstreamCloses(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
		}},
	}
	downstream := &codexWSBrokerConnStub{
		reads:    []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}},
		writeErr: errors.New("downstream unavailable"),
	}
	upstream := newCodexWSBrokerCloseBlockingConn([]codexWSBrokerRead{{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.created","response":{"id":"response-a"}}`),
	}})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(upstream.closeRelease) }) }
	defer releaseClose()
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- broker.Serve(context.Background(), downstream) }()
	select {
	case <-upstream.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("failed upstream close did not start")
	}

	next, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-a", planner.slots))
	if err == nil {
		_, _ = next.AbandonBeforeDispatch()
	}
	if !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("request while failed upstream closes = %v, want concurrent turn", err)
	}

	releaseClose()
	select {
	case err := <-serveDone:
		if err == nil || !strings.Contains(err.Error(), "downstream WebSocket write failed") {
			t.Fatalf("downstream write failure = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not finish after failed upstream closed")
	}
	assertCodexWSBrokerLaneReleased(t, runtimeLease, planner.slots)
}

func assertCodexWSBrokerLaneReleased(t *testing.T, runtimeLease *CodexLeaseRuntime, slots []CodexLeaseAttemptSlotPlan) {
	t.Helper()
	next, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-a", slots))
	if err != nil {
		t.Fatalf("request after broker failure: %v", err)
	}
	if _, err := next.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexTerminatingWSBrokerKeepsInstalledPrewarmConnection(t *testing.T) {
	t.Parallel()
	receiptStore, err := NewCodexTurnReceiptStore(strings.NewReader(strings.Repeat("p", 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	receiptValue := testCodexTurnReceipt()
	receiptValue.Transport = CodexTurnReceiptTransportWebSocket
	receipt := receiptStore.register([]byte("session-a"), []byte("turn-a"), receiptValue)
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		receipt: receipt,
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey:  "account-a",
			CandidateID: "candidate-a",
			Kind:        CodexAttemptSlotDirect,
		}},
	}
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	turn := codexTerminatingWSFrame("turn-a", `,"previous_response_id":"prewarm-a"`)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: prewarm},
		{messageType: websocket.TextMessage, payload: turn},
		{err: io.EOF},
	}}
	prewarmUpstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"prewarm-state"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"turn-state"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {prewarmUpstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		Headers:              http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"session-a","thread_id":"thread-a","turn_id":"","request_kind":"prewarm"}`}},
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	var accepted []codexObservationFields
	ctx := withCodexWSFrameObservationSink(context.Background(), func(diagnostics *routeDiagnostics) {
		event := RouteEvent{}
		event.applyRouteDiagnostics(diagnostics)
		accepted = append(accepted, codexObservationFields{RequestKind: event.RequestKind, RequestLineage: event.RequestLineage})
	})
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 || accepted[0].RequestKind != "prewarm" || accepted[0].RequestLineage != "previous_response_id_absent" ||
		accepted[1].RequestKind != "turn" || accepted[1].RequestLineage != "previous_response_id_present" {
		t.Fatalf("accepted frame observations = %+v, want prewarm then continued turn", accepted)
	}
	if planner.prewarmCalls != 1 || planner.adoptionCalls != 1 || planner.buildCalls != 0 {
		t.Fatalf("planner calls = prewarm %d adoption %d build %d, want 1/1/0", planner.prewarmCalls, planner.adoptionCalls, planner.buildCalls)
	}
	if planner.prewarmHeaders.Get(codexTurnMetadataKey) != "" || planner.buildHeaders.Get(codexTurnMetadataKey) != "" {
		t.Fatalf("connection metadata reached per-frame planner: prewarm=%v build=%v", planner.prewarmHeaders, planner.buildHeaders)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := prewarmUpstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{prewarm, turn}) {
		t.Fatalf("prewarm upstream writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); len(got) != 6 {
		t.Fatalf("downstream writes = %#v", got)
	}
	restored, err := coordinator.store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session-a", Thread: "thread-a", Namespace: CodexResponsesNamespace}, Turn: "turn-a"},
		[]codex.AccountKey{"account-a"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.ResolvedRecords) != 1 || !restored.ResolvedRecords[0].Record.AdoptedPrewarm || restored.ResolvedRecords[0].Record.UpstreamSocketGeneration != 1 {
		t.Fatalf("durable prewarm adoption = %#v", restored.ResolvedRecords)
	}
	if record := restored.ResolvedRecords[0].Record; !record.HasTurnState || record.TurnStateHash != coordinator.store.hash("turn-state", "turn-state") {
		t.Fatalf("turn state = (%t, %q), want real-turn authority", record.HasTurnState, record.TurnStateHash)
	}
	gotReceipt, found := receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if !found || gotReceipt.State != CodexTurnReceiptCompleted || gotReceipt.ActualAccountHint != redactedAccountHint("codex", "account-a") {
		t.Fatalf("reconnected receipt = (%+v, %v)", gotReceipt, found)
	}
}

func TestCodexTerminatingWSBrokerResumesAfterCompletedClientDisconnect(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	plannerA := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey:  "account-a",
			CandidateID: "candidate-a",
			Kind:        CodexAttemptSlotDirect,
		}},
	}
	plannerB := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey:  "account-b",
			CandidateID: "candidate-b",
			Kind:        CodexAttemptSlotDirect,
		}},
	}
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	firstUpstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
	}}
	secondUpstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-b","end_turn":true}}`)},
	}}
	staleUpstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-c"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-c"}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {firstUpstream, staleUpstream},
		"account-b": {secondUpstream},
	}}
	serve := func(planner *codexWSBrokerPlannerStub, downstreamGeneration uint64, turn, anchor string) error {
		broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
			Plans:                planner,
			Upstream:             dialer,
			UpstreamURL:          "wss://example.invalid/responses",
			DownstreamGeneration: downstreamGeneration,
		})
		if err != nil {
			return err
		}
		downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
			{messageType: websocket.TextMessage, payload: prewarm},
			{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame(turn, `,"previous_response_id":"`+anchor+`"`)},
			{err: &websocket.CloseError{Code: websocket.CloseNormalClosure}},
		}}
		return broker.Serve(context.Background(), downstream)
	}

	if err := serve(plannerA, 41, "turn-a", "prewarm-a"); err != nil {
		t.Fatalf("initial broker: %v", err)
	}
	completed, err := coordinator.store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session-a", Thread: "thread-a", Namespace: CodexResponsesNamespace}, Turn: "turn-a"},
		[]codex.AccountKey{"account-a", "account-b"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Classification != CodexRestoredLaneCurrent || len(completed.ResolvedRecords) != 1 {
		t.Fatalf("completed lane = %#v", completed)
	}
	completedRecord := completed.ResolvedRecords[0].Record
	if completedRecord.State != LeaseBoundQuiescent || completedRecord.RoutingRefs != 0 || completedRecord.AttemptRefs != 0 || completedRecord.ResponseObserverRefs != 0 || !completedRecord.SocketLineageExtinct {
		t.Fatalf("completed turn retained live ownership = %#v", completedRecord)
	}
	if err := serve(plannerB, 42, "turn-b", "prewarm-b"); err != nil {
		t.Fatalf("resumed broker after clean client disconnect: %v", err)
	}
	if err := serve(plannerA, 43, "turn-b", "prewarm-c"); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("same-turn resumed adoption error = %v, want %v", err, ErrCodexStaleTurn)
	}
	if plannerA.prewarmCalls != 2 || plannerA.adoptionCalls != 2 || plannerA.buildCalls != 0 ||
		plannerB.prewarmCalls != 1 || plannerB.adoptionCalls != 1 || plannerB.buildCalls != 0 {
		t.Fatalf("planner calls = A prewarm/adoption/build %d/%d/%d, B %d/%d/%d", plannerA.prewarmCalls, plannerA.adoptionCalls, plannerA.buildCalls, plannerB.prewarmCalls, plannerB.adoptionCalls, plannerB.buildCalls)
	}
	restored, err := coordinator.store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session-a", Thread: "thread-a", Namespace: CodexResponsesNamespace}, Turn: "turn-b"},
		[]codex.AccountKey{"account-a", "account-b"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Classification != CodexRestoredLaneCurrent || len(restored.ResolvedRecords) != 2 {
		t.Fatalf("resumed lane = %#v", restored)
	}
	for _, resolved := range restored.ResolvedRecords {
		switch resolved.Record.TurnHash {
		case coordinator.store.hash("turn", "turn-a"):
			attempt, found := codexLeaseAttemptByGeneration(resolved.Record.Attempts, resolved.Record.CurrentAttemptGeneration)
			if resolved.AccountKey != "account-a" || !found || attempt.State != CodexAttemptProviderCompleted || resolved.Record.State != LeaseSuperseded ||
				resolved.Record.RecordGeneration != completedRecord.RecordGeneration+1 || resolved.Record.LeaseGeneration != completedRecord.LeaseGeneration+1 ||
				resolved.Record.LaneGeneration != restored.Lane.Generation || resolved.Record.RoutingRefs != 0 || resolved.Record.AttemptRefs != 0 || resolved.Record.ResponseObserverRefs != 0 || !resolved.Record.SocketLineageExtinct {
				t.Fatalf("completed predecessor = %#v, account %q, attempt %#v", resolved.Record, resolved.AccountKey, attempt)
			}
		case coordinator.store.hash("turn", "turn-b"):
			if resolved.AccountKey != "account-b" || resolved.Record.State != LeaseBoundQuiescent || resolved.Record.PredecessorGeneration != completedRecord.RecordGeneration+1 || resolved.Record.RoutingRefs != 0 || resolved.Record.AttemptRefs != 0 || resolved.Record.ResponseObserverRefs != 0 {
				t.Fatalf("resumed turn state = %#v, want drained bound quiescent", resolved.Record)
			}
		}
	}
}

func TestCodexTerminatingWSBrokerDoesNotInventActualBeforeDispatch(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	receiptStore, err := NewCodexTurnReceiptStore(strings.NewReader(strings.Repeat("d", 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	receiptValue := testCodexTurnReceipt()
	receiptValue.Transport = CodexTurnReceiptTransportWebSocket
	receipt := receiptStore.register([]byte("session-a"), []byte("turn-a"), receiptValue)
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{err: errors.New("resolution failed"), skipDispatch: true}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: &codexWSBrokerPlannerStub{}, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	account := CodexFrozenDispatchAccount{
		choice:   RouteChoice{AccountKey: "account-a"},
		attempts: []CandidateAttempt{{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"}}},
	}
	active := &codexWSActiveUpstream{}
	dial := broker.connect(context.Background(), handle, receipt, account, active)
	if dial.lifecycle == nil || dial.err == nil {
		t.Fatalf("dial = %#v", dial)
	}
	got, found := receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if !found || got.State != CodexTurnReceiptPlanned || got.ActualAccountHint != "" {
		t.Fatalf("pre-dispatch receipt = (%+v, %v)", got, found)
	}
	_ = dial.lifecycle.Indeterminate(context.Background(), dial.lifecycle.upstreamGeneration)
	got, _ = receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if got.State != CodexTurnReceiptIndeterminate || got.ActualAccountHint != "" {
		t.Fatalf("indeterminate receipt = %+v", got)
	}
}

func TestCodexTerminatingWSBrokerRejectedFrameDoesNotEmitObservation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{runtime: newCodexLeaseRuntimeTest(t, coordinator)}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: &codexWSBrokerDialerStub{}, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	var accepted []codexObservationFields
	ctx := withCodexWSFrameObservationSink(context.Background(), func(diagnostics *routeDiagnostics) {
		event := RouteEvent{}
		event.applyRouteDiagnostics(diagnostics)
		accepted = append(accepted, codexObservationFields{RequestKind: event.RequestKind})
	})
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","private":"prompt"}`)}}}
	if err := broker.Serve(ctx, downstream); err == nil {
		t.Fatal("rejected response frame succeeded")
	}
	if len(accepted) != 0 {
		t.Fatalf("rejected frame observations = %+v, want none", accepted)
	}
}

func TestCodexTerminatingWSBrokerRoutesTurnWithoutPrewarmAdoption(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey:  "account-a",
			CandidateID: "candidate-a",
			Kind:        CodexAttemptSlotDirect,
		}},
	}
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	turn := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: prewarm},
		{messageType: websocket.TextMessage, payload: turn},
		{err: io.EOF},
	}}
	prewarmUpstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {prewarmUpstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if planner.prewarmCalls != 1 || planner.adoptionCalls != 0 || planner.cancelCalls != 1 || planner.buildCalls != 1 {
		t.Fatalf("planner calls = prewarm %d adoption %d cancel %d build %d, want 1/0/1/1", planner.prewarmCalls, planner.adoptionCalls, planner.cancelCalls, planner.buildCalls)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v, want one persistent connection", got)
	}
	if got := prewarmUpstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{prewarm, turn}) {
		t.Fatalf("prewarm upstream writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); len(got) != 4 {
		t.Fatalf("downstream writes = %#v", got)
	}
}

func TestServerCodexWebSocketEnforceUsesTerminatingBroker(t *testing.T) {
	t.Parallel()
	broker := &codexWebSocketRoutingHandlerStub{}
	server := &Server{
		Config: &Config{
			ClaudeUpstream: "https://claude.test",
			CodexUpstream:  "https://codex.test",
			LocalToken:     "local-token",
		},
		CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
			Configured:         CodexRoutingEnforce,
			Effective:          CodexRoutingEnforce,
			ModeEpoch:          41,
			AuthoritativeEpoch: 41,
		}},
		CodexWebSocketBroker: broker,
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	header := make(http.Header)
	header.Set("X-Test-Authority", "bound")
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxyServer.URL, "http")+legacyCodexResponsesPath, header)
	if err != nil {
		t.Fatalf("downstream WebSocket upgrade: %v", err)
	}
	defer client.Close()
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("downstream upgrade response = %v, want 101", response)
	}
	request := codexTerminatingWSFrame("turn-a", "")
	if err := client.WriteMessage(websocket.TextMessage, request); err != nil {
		t.Fatal(err)
	}
	messageType, reply, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || string(reply) != `{"type":"response.completed"}` {
		t.Fatalf("broker reply = (%d, %q)", messageType, reply)
	}
	if got := broker.header.Get("X-Test-Authority"); got != "bound" {
		t.Fatalf("broker header = %q", got)
	}
}

func TestServerCodexWebSocketEnforceTracesSafeBrokerFailure(t *testing.T) {
	for _, test := range []struct {
		name         string
		brokerErr    error
		wantDecision string
		wantReason   string
	}{
		{
			name:         "plan failure",
			brokerErr:    fmt.Errorf("private wrapped cause: %w", newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexLeaseAuthorityMismatch)),
			wantDecision: "plan_failed",
			wantReason:   "lease_authority_mismatch",
		},
		{
			name:         "unknown broker failure",
			brokerErr:    errors.New("private broker failure token-should-not-leak"),
			wantDecision: "broker_failed",
			wantReason:   "unknown",
		},
		{
			name:         "lease transition",
			brokerErr:    ErrCodexLeaseTransition,
			wantDecision: "broker_failed",
			wantReason:   "lease_transition",
		},
		{
			name:         "concurrent turn",
			brokerErr:    ErrCodexConcurrentTurn,
			wantDecision: "broker_failed",
			wantReason:   "concurrent_turn",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			diagnostics, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = diagnostics.Close() })
			server := &Server{
				Config: &Config{ClaudeUpstream: "https://claude.test", CodexUpstream: "https://codex.test", LocalToken: "local-token"},
				CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
					Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 41, AuthoritativeEpoch: 41,
				}},
				CodexWebSocketBroker: &codexWebSocketFailingHandlerStub{err: test.brokerErr},
				Diag:                 diagnostics,
			}
			handler, err := server.handler()
			if err != nil {
				t.Fatal(err)
			}
			proxyServer := httptest.NewServer(handler)
			t.Cleanup(proxyServer.Close)

			header := make(http.Header)
			header.Set("X-Codex-Window-Id", "private-window-id")
			client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxyServer.URL, "http")+legacyCodexResponsesPath, header)
			if err != nil {
				t.Fatalf("downstream WebSocket upgrade: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("downstream upgrade response = %v, want 101", response)
			}
			if err := client.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame("turn-private", "")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseInternalServerErr) {
				t.Fatalf("downstream close = %v, want 1011", err)
			}

			events := waitForDiagnosticsEvents(t, path, 1)
			if len(events) != 1 {
				t.Fatalf("diagnostics events = %d, want 1", len(events))
			}
			event := events[0]
			if event.RouteKind != "codex_websocket_broker" || event.StatusCode != http.StatusSwitchingProtocols || event.Decision != test.wantDecision || event.Reason != test.wantReason {
				t.Fatalf("broker event = %+v, want decision=%q reason=%q", event, test.wantDecision, test.wantReason)
			}
			if event.SessionKey != "codex-window:83b0f854cdc5" || event.SessionSource != "x-codex-window-id" {
				t.Fatalf("session correlation = %q/%q", event.SessionKey, event.SessionSource)
			}
			assertDiagnosticsLogDoesNotContain(t, path, "private-window-id")
			assertDiagnosticsLogDoesNotContain(t, path, "private wrapped cause")
			assertDiagnosticsLogDoesNotContain(t, path, "token-should-not-leak")
		})
	}
}

func TestServerCodexWebSocketEnforcePersistsEachAcceptedFrameSeparately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostics.Close() })
	server := &Server{
		Config: &Config{ClaudeUpstream: "https://claude.test", CodexUpstream: "https://codex.test", LocalToken: "local-token"},
		CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
			Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 41, AuthoritativeEpoch: 41,
		}},
		CodexWebSocketBroker: &codexWebSocketFrameSinkHandlerStub{},
		Diag:                 diagnostics,
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxyServer.URL, "http")+legacyCodexResponsesPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range [][]byte{
		codexTerminatingWSFrame("turn-a", ""),
		codexTerminatingWSFrame("turn-b", `,"previous_response_id":"private-id"`),
	} {
		if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := client.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	events := waitForDiagnosticsEvents(t, path, 3)
	var frameEvents []RouteEvent
	for _, event := range events {
		if event.RouteKind == "codex_websocket_frame" {
			frameEvents = append(frameEvents, event)
		}
	}
	if len(frameEvents) != 2 || frameEvents[0].RequestLineage != "previous_response_id_absent" || frameEvents[1].RequestLineage != "previous_response_id_present" {
		t.Fatalf("frame events = %+v, want two independent snapshots", frameEvents)
	}
}

func TestServerCodexWebSocketEnforceRejectsMissingBrokerBeforeUpgrade(t *testing.T) {
	t.Parallel()
	server := &Server{
		Config: &Config{
			ClaudeUpstream: "https://claude.test",
			CodexUpstream:  "https://codex.test",
			LocalToken:     "local-token",
		},
		CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
			Configured:         CodexRoutingEnforce,
			Effective:          CodexRoutingEnforce,
			ModeEpoch:          41,
			AuthoritativeEpoch: 41,
		}},
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxyServer.URL, "http")+legacyCodexResponsesPath, nil)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("missing broker upgraded downstream")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing broker result = response %v error %v", response, err)
	}
}

type codexWebSocketRoutingHandlerStub struct {
	header http.Header
}

type codexWebSocketFailingHandlerStub struct {
	err error
}

type codexWebSocketFrameSinkHandlerStub struct{}

func (*codexWebSocketFrameSinkHandlerStub) Serve(ctx context.Context, connection *websocket.Conn, _ http.Header) error {
	for range 2 {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		pending, err := newCodexWSPendingFrame(messageType, payload)
		if err != nil {
			return err
		}
		emitAcceptedCodexWSFrameObservation(ctx, pending.diagnostics)
		pending.Release()
	}
	return connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
}

func (handler *codexWebSocketRoutingHandlerStub) Serve(_ context.Context, connection *websocket.Conn, header http.Header) error {
	handler.header = header.Clone()
	if _, _, err := connection.ReadMessage(); err != nil {
		return err
	}
	return connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
}

func (handler *codexWebSocketFailingHandlerStub) Serve(_ context.Context, connection *websocket.Conn, _ http.Header) error {
	if _, _, err := connection.ReadMessage(); err != nil {
		return err
	}
	return handler.err
}

func TestCodexTerminatingWSBrokerNeverRotatesNonPortableOrAdmittedTurn(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		frame      []byte
		reads      []codexWSBrokerRead
		seedAnchor string
	}{
		{
			name:       "incremental request",
			frame:      codexTerminatingWSFrame("turn-incremental", `,"previous_response_id":"response-old"`),
			reads:      []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}},
			seedAnchor: "response-old",
		},
		{
			name:  "admitted request",
			frame: codexTerminatingWSFrame("turn-admitted", ""),
			reads: []codexWSBrokerRead{
				{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{}}`)},
				{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			slots := []CodexLeaseAttemptSlotPlan{
				{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
				{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
			}
			if test.seedAnchor != "" {
				slots = slots[:1]
				predecessor, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-predecessor", slots))
				if err != nil {
					t.Fatal(err)
				}
				predecessor, err = predecessor.MarkDispatched()
				if err != nil {
					t.Fatal(err)
				}
				predecessor, err = predecessor.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{})
				if err != nil {
					t.Fatal(err)
				}
				predecessor, err = predecessor.ProviderCompleted(CodexHTTPCompletionEvidence{
					CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: test.seedAnchor, HasResponseAnchor: true},
				})
				if err != nil {
					t.Fatal(err)
				}
				if predecessor, err = predecessor.Drain(); err != nil {
					t.Fatal(err)
				}
			}
			planner := &codexWSBrokerPlannerStub{
				runtime: runtimeLease,
				slots:   slots,
			}
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: test.frame}, {err: io.EOF}}}
			upstream := &codexWSBrokerConnStub{reads: test.reads}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Serve(context.Background(), downstream); err != nil {
				t.Fatal(err)
			}
			if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
				t.Fatalf("dial accounts = %#v", got)
			}
			writes := downstream.writtenPayloads()
			if len(writes) == 0 || !reflect.DeepEqual(writes[len(writes)-1], codexWSBrokerHard429()) {
				t.Fatalf("terminal writes = %#v", writes)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerReusesAccountBoundUpstreamForSuccessor(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	first := codexTerminatingWSFrame("turn-a", "")
	second := codexTerminatingWSFrame("turn-b", `,"previous_response_id":"response-a"`)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: first},
		{messageType: websocket.TextMessage, payload: second},
		{err: io.EOF},
	}}
	upstream := newCodexWSBrokerSuccessorConn([]codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	})
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v, want one persistent upstream", got)
	}
	if got := upstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{first, second}) {
		t.Fatalf("upstream writes = %#v", got)
	}
}

func TestServeCodexWSIdleKeepalivePingsUpstream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	controls := make(chan codexWSBrokerWrite, 1)
	upstream := &codexWSBrokerConnStub{controlWrites: controls}
	done := make(chan error, 1)
	go func() {
		done <- serveCodexWSIdleKeepalive(ctx, upstream, ticks)
	}()
	ticks <- time.Now()
	select {
	case control := <-controls:
		if control.messageType != websocket.PingMessage || len(control.payload) != 0 {
			t.Fatalf("control = %#v", control)
		}
	case <-time.After(time.Second):
		t.Fatal("idle upstream did not receive keepalive ping")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("keepalive did not stop with context")
	}
}

func TestCodexWSUpstreamReaderAnswersIdlePing(t *testing.T) {
	t.Parallel()
	pong := make(chan string, 1)
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		conn.SetPongHandler(func(payload string) error {
			pong <- payload
			return nil
		})
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_, _, _ = conn.ReadMessage()
		}()
		if err := conn.WriteControl(websocket.PingMessage, []byte("idle"), time.Now().Add(time.Second)); err != nil {
			serverDone <- err
			return
		}
		select {
		case payload := <-pong:
			if payload != "idle" {
				serverDone <- fmt.Errorf("pong payload = %q", payload)
				return
			}
			serverDone <- nil
		case <-time.After(time.Second):
			serverDone <- errors.New("idle upstream ping was not answered")
		}
	}))
	defer server.Close()

	upstream, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	active := codexWSActiveUpstream{conn: upstream}
	startCodexWSUpstreamReader(context.Background(), &active)
	defer closeCodexWSActiveUpstream(&active)
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle upstream ping test timed out")
	}
}

func TestCodexWSIdleUpstreamDropsLateMaintenanceFrame(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`,
		`{"type":"keepalive"}`,
		`{"type":"responsesapi.websocket_timing"}`,
	} {
		readFrames := make(chan codexWSUpstreamRead, 1)
		readFrames <- codexWSUpstreamRead{messageType: websocket.TextMessage, payload: []byte(payload)}
		active := &codexWSActiveUpstream{
			conn:       &codexWSBrokerConnStub{},
			readFrames: readFrames,
		}

		if err := codexWSIdleUpstreamError(active); err != nil {
			t.Fatalf("idle maintenance frame %q = %v, want dropped", payload, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := readCodexWSActiveMessage(ctx, active); !errors.Is(err, context.Canceled) {
			t.Fatalf("idle maintenance frame %q remained queued: %v", payload, err)
		}
	}
}

func TestCodexWSIdleUpstreamRejectsLateResponseFrame(t *testing.T) {
	t.Parallel()
	readFrames := make(chan codexWSUpstreamRead, 2)
	readFrames <- codexWSUpstreamRead{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"keepalive"}`),
	}
	readFrames <- codexWSUpstreamRead{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.output_text.delta","delta":"late"}`),
	}
	active := &codexWSActiveUpstream{
		conn:       &codexWSBrokerConnStub{},
		readFrames: readFrames,
	}

	if err := codexWSIdleUpstreamError(active); !errors.Is(err, ErrCodexWSInvalidFrame) {
		t.Fatalf("idle response frame = %v, want %v", err, ErrCodexWSInvalidFrame)
	}
}

func TestCodexTerminatingWSBrokerRejectsKnownClosedIdleUpstreamBeforeDispatch(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             &codexWSBrokerDialerStub{},
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, codexTerminatingWSFrame("turn-b", `,"previous_response_id":"response-a"`))
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	readDone := make(chan struct{})
	close(readDone)
	readFrames := make(chan codexWSUpstreamRead, 1)
	readFrames <- codexWSUpstreamRead{err: io.EOF}
	close(readFrames)
	upstream := &codexWSBrokerConnStub{}
	active := codexWSActiveUpstream{
		conn:       upstream,
		account:    "account-a",
		generation: 1,
		readCancel: func() {},
		readFrames: readFrames,
		readDone:   readDone,
	}
	defer closeCodexWSActiveUpstream(&active)

	err = broker.serveFrame(context.Background(), &codexWSBrokerConnStub{}, pending, &active)
	if err == nil {
		t.Fatal("known-closed idle upstream was reused")
	}
	failure := classifyCodexWebSocketFailure(err)
	if failure.Stage != "upstream_idle" || failure.Reason != "upstream_closed" {
		t.Fatalf("idle upstream failure = %s/%s, want upstream_idle/upstream_closed", failure.Stage, failure.Reason)
	}
	if planner.buildCalls != 0 {
		t.Fatalf("planner builds = %d, want no successor dispatch", planner.buildCalls)
	}
	if writes := upstream.writtenPayloads(); len(writes) != 0 {
		t.Fatalf("known-closed upstream writes = %#v", writes)
	}
}

func TestCodexTerminatingWSBrokerReportsHandledHandshakeFailure(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}, {err: io.EOF}}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{
			response: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)},
			body:     []byte(`{"error":{"type":"authentication_error","message":"private upstream payload"}}`),
			err:      errors.New("private dial error"),
		}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	ctx, routeDiagnostics := withRouteDiagnostics(context.Background())
	stderr := captureStderr(t, func() {
		if err := broker.Serve(ctx, downstream); err != nil {
			t.Fatal(err)
		}
	})
	writes := downstream.writtenPayloads()
	const wantFrame = `{"type":"error","status":401,"error":{"type":"authentication_error"}}`
	if len(writes) != 1 || string(writes[0]) != wantFrame {
		t.Fatalf("translated writes = %q, want %q", writes, wantFrame)
	}
	const wantTrace = "cq: Codex route trace transport=websocket event=broker_failed stage=upstream_handshake reason=upstream_rejected response_present=true upstream_status=401 auth_failure=true hard_limit=false\n"
	if stderr != wantTrace {
		t.Fatalf("trace = %q, want %q", stderr, wantTrace)
	}
	event := RouteEvent{}
	event.applyRouteDiagnostics(routeDiagnostics)
	if event.Decision != "broker_failed" || event.Reason != "upstream_rejected" {
		t.Fatalf("diagnostics = %+v, want broker_failed/upstream_rejected", event)
	}
	for _, private := range []string{"private upstream payload", "private dial error"} {
		if strings.Contains(stderr, private) || strings.Contains(string(writes[0]), private) {
			t.Fatalf("private handshake detail escaped: trace=%q frame=%q", stderr, writes[0])
		}
	}
}

func TestCodexTerminatingWSBrokerReportsHandledPrewarmTransportFailure(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: prewarm}, {err: io.EOF}}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{err: errors.New("private prewarm dial error")}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	ctx, routeDiagnostics := withRouteDiagnostics(context.Background())
	stderr := captureStderr(t, func() {
		if err := broker.Serve(ctx, downstream); err != nil {
			t.Fatal(err)
		}
	})
	writes := downstream.writtenPayloads()
	const wantFrame = `{"type":"error","status":502,"error":{"type":"api_error"}}`
	if len(writes) != 1 || string(writes[0]) != wantFrame {
		t.Fatalf("translated writes = %q, want %q", writes, wantFrame)
	}
	const wantTrace = "cq: Codex route trace transport=websocket event=broker_failed stage=upstream_handshake reason=upstream_outcome_indeterminate response_present=false upstream_status=0 auth_failure=false hard_limit=false\n"
	if stderr != wantTrace {
		t.Fatalf("trace = %q, want %q", stderr, wantTrace)
	}
	event := RouteEvent{}
	event.applyRouteDiagnostics(routeDiagnostics)
	if event.Decision != "broker_failed" || event.Reason != "upstream_outcome_indeterminate" {
		t.Fatalf("diagnostics = %+v, want broker_failed/upstream_outcome_indeterminate", event)
	}
	if strings.Contains(stderr, "private prewarm dial error") || strings.Contains(string(writes[0]), "private prewarm dial error") {
		t.Fatalf("private prewarm detail escaped: trace=%q frame=%q", stderr, writes[0])
	}
}

func TestCanonicalCodexWSHandshakeErrorPreservesSupportedStatus(t *testing.T) {
	for _, test := range []struct {
		status  int
		wrapped CodexWrappedError
		kind    string
	}{
		{status: http.StatusUnauthorized, wrapped: CodexWrappedError{Found: true, AuthFailure: true}, kind: "authentication_error"},
		{status: http.StatusForbidden, wrapped: CodexWrappedError{Found: true, AuthFailure: true}, kind: "authentication_error"},
		{status: http.StatusTooManyRequests, wrapped: CodexWrappedError{Found: true, HardUsageLimit: true}, kind: "usage_limit_reached"},
		{status: http.StatusUpgradeRequired, wrapped: CodexWrappedError{Found: true}, kind: "api_error"},
	} {
		frame, status := canonicalCodexWSHandshakeError(&http.Response{StatusCode: test.status}, test.wrapped)
		if status != test.status || string(frame) != fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":"%s"}}`, test.status, test.kind) {
			t.Fatalf("status %d projection = %d/%s", test.status, status, frame)
		}
		if strings.Contains(string(frame), "private") {
			t.Fatalf("status %d projection leaked private detail: %s", test.status, frame)
		}
	}
}

func TestCodexTerminatingWSBrokerCancellationUnblocksUpstreamRead(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}}}
	upstream := newCodexWSBrokerBlockingConn()
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Serve(ctx, downstream) }()
	select {
	case <-upstream.started:
	case <-time.After(time.Second):
		t.Fatal("broker did not enter upstream read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve cancellation = %T %v", err, err)
		}
	case <-time.After(time.Second):
		_ = upstream.Close()
		<-done
		t.Fatal("cancellation did not unblock upstream read")
	}
}

func TestCodexTerminatingWSBrokerPeerCloseCancelsBlockedPlanning(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	slots := []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}}
	lane := LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}
	predecessorPlan := codexLeaseRuntimeTestPlan("turn", slots)
	predecessorPlan.Key.Lane = lane
	predecessorPlan.RequestedModel = "gpt-5"
	predecessorPlan.EffectiveModel = "gpt-5"
	turn, err := runtimeLease.BeginRequest(predecessorPlan)
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if turn, err = turn.Drain(); err != nil {
		t.Fatal(err)
	}
	midTurnPlan := predecessorPlan
	midTurnPlan.RequestKind = CodexRequestCompaction
	midTurnPlan.CompactionPhase = CodexCompactionMidTurn
	predecessor, err := runtimeLease.BeginRequest(midTurnPlan)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	predecessorLifecycle, err := newCodexWSLifecycle(predecessor, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := predecessorLifecycle.ObserveUpstreamUpgrade(context.Background(), 1, "private-ws-state"); err != nil {
		t.Fatal(err)
	}

	planner := codexHTTPRequestPlanTestFactory(runtimeLease)
	planner.Routes = coordinator
	planner.Authority = predecessorPlan.Authority
	planner.TransportKind = "websocket"
	body := bytes.Replace(
		frozenRequestBody("gpt-5", CodexRequestCompaction, "private-body"),
		[]byte(`"compaction":"standalone_turn"`),
		[]byte(`"compaction":"pre_turn"`),
		1,
	)
	caller := RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
	}
	withCaller := func(ctx context.Context) context.Context {
		return withRuntimeCallerIdentity(withRuntimeCallerAuthority(ctx, caller), "account\x00candidate\x00revision")
	}
	dialer := &codexWSBrokerDialerStub{}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	cancelBroker := make(chan context.CancelFunc, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downstream, upgradeErr := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if upgradeErr != nil {
			serveDone <- upgradeErr
			return
		}
		defer downstream.Close()
		brokerContext, cancel := context.WithCancel(withCaller(request.Context()))
		cancelBroker <- cancel
		defer cancel()
		serveDone <- broker.Serve(brokerContext, downstream)
	}))
	t.Cleanup(server.Close)

	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		_ = client.Close()
		t.Fatalf("downstream upgrade response = %v, want 101", response)
	}
	cancel := <-cancelBroker
	t.Cleanup(cancel)
	if err := client.WriteMessage(websocket.TextMessage, body); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}

	waitCodexWSBrokerPlanningGateState(t, runtimeLease, lane, 1, true)
	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	waiterContext, cancelWaiter := context.WithTimeout(withCaller(context.Background()), 2*time.Second)
	defer cancelWaiter()
	waiterResult := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := planner.Build(waiterContext, CodexHTTPRequestPlanInput{Encoded: body})
		waiterResult <- buildResult{prepared: prepared, err: buildErr}
	}()
	waitCodexWSBrokerPlanningGateState(t, runtimeLease, lane, 2, true)

	if err := client.Close(); err != nil {
		cancel()
		t.Fatal(err)
	}

	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			t.Fatalf("broker after peer close = %T %v, want clean return", serveErr, serveErr)
		}
	case <-time.After(time.Second):
		cancel()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
		}
		t.Fatal("peer close did not cancel blocked WebSocket planning")
	}
	waitCodexWSBrokerPlanningGateState(t, runtimeLease, lane, 1, true)
	if len(dialer.accounts) != 0 {
		t.Fatalf("peer-closed planner dispatched upstream accounts %#v", dialer.accounts)
	}
	if _, err := predecessorLifecycle.ObserveFrame(
		context.Background(),
		1,
		[]byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":false}}`),
	); err != nil {
		t.Fatal(err)
	}
	if err := predecessorLifecycle.Drain(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-waiterResult:
		if result.err != nil {
			var planErr *CodexHTTPRequestPlanError
			if errors.As(result.err, &planErr) {
				t.Fatalf("queued successor after predecessor drain = %T %v (%s/%s)", result.err, result.err, planErr.Code, planErr.Reason)
			}
			t.Fatalf("queued successor after predecessor drain = %T %v", result.err, result.err)
		}
		if result.prepared.Frozen == nil || result.prepared.Lifecycle == nil || result.prepared.leaseHandle == nil {
			t.Fatalf("queued successor ownership = %#v", result.prepared)
		}
		if result.prepared.leaseHandle.RequestGeneration() != 3 {
			t.Fatalf("queued successor generation = %d, want 3", result.prepared.leaseHandle.RequestGeneration())
		}
		if _, err := result.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		result.prepared.Frozen.Release()
	case <-waiterContext.Done():
		t.Fatal("queued successor did not continue after predecessor drain")
	}
	waitCodexWSBrokerPlanningGateState(t, runtimeLease, lane, 0, false)
}

func TestCodexTerminatingWSBrokerRejectsSecondQueuedApplicationFrame(t *testing.T) {
	t.Parallel()

	planner := &codexWSBrokerContextBlockingPlanner{started: make(chan struct{})}
	dialer := &codexWSBrokerDialerStub{}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downstream, upgradeErr := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if upgradeErr != nil {
			serveDone <- upgradeErr
			return
		}
		defer downstream.Close()
		serveDone <- broker.Serve(request.Context(), downstream)
	}))
	t.Cleanup(server.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame("turn-a", "")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-planner.started:
	case <-time.After(time.Second):
		t.Fatal("first frame did not enter planning")
	}
	for _, turn := range []string{"turn-b", "turn-c"} {
		if err := client.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame(turn, "")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case serveErr := <-serveDone:
		if !errors.Is(serveErr, ErrCodexWSInvalidFrame) {
			t.Fatalf("second queued application frame = %T %v, want invalid frame", serveErr, serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("second queued application frame did not stop broker")
	}
	if len(dialer.accounts) != 0 {
		t.Fatalf("queued frame overflow dispatched upstream accounts %#v", dialer.accounts)
	}
}

func TestCodexWSDownstreamReaderClearsQueuedAndOverflowPayloads(t *testing.T) {
	t.Parallel()
	queued := []byte("private-queued-frame")
	overflow := []byte("private-overflow-frame")
	conn := &codexWSBrokerPayloadConn{payloads: [][]byte{queued, overflow}}
	ctx, cancel := context.WithCancel(context.Background())
	reader := startCodexWSDownstreamReader(ctx, cancel, conn)
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		reader.close()
		t.Fatal("downstream overflow reader did not stop")
	}
	reader.close()
	if !bytes.Equal(queued, make([]byte, len(queued))) {
		t.Fatalf("queued payload retained private bytes: %q", queued)
	}
	if !bytes.Equal(overflow, make([]byte, len(overflow))) {
		t.Fatalf("overflow payload retained private bytes: %q", overflow)
	}
}

func TestCodexTerminatingWSBrokerClassifiesUpstreamCloseBeforeCompletion(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{err: errors.New("private upstream read token-should-not-leak")}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}

	err = broker.Serve(context.Background(), downstream)
	if err == nil {
		t.Fatal("upstream close before completion succeeded")
	}
	failure := classifyCodexWebSocketFailure(err)
	if failure.Stage != "upstream_read" || failure.Reason != "upstream_outcome_indeterminate" {
		t.Fatalf("upstream close failure = %s/%s, want upstream_read/upstream_outcome_indeterminate", failure.Stage, failure.Reason)
	}
	if strings.Contains(err.Error(), "token-should-not-leak") {
		t.Fatalf("broker error leaked private upstream text: %q", err)
	}
}

func TestCodexTerminatingWSBrokerTreatsNormalDownstreamCloseAfterCompletedTurnAsClean(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")},
		{err: &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "done"}},
	}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}

	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatalf("normal downstream close after completed turn = %v, want clean return", err)
	}
	if writes := downstream.writtenPayloads(); len(writes) != 2 {
		t.Fatalf("completed turn writes = %#v, want created and completed", writes)
	}
}

func TestCodexTerminatingWSBrokerTreatsAbnormalDownstreamCloseAfterCompletedTurnAsClean(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")},
		{err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"}},
	}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}

	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatalf("abnormal downstream close after completed turn = %v, want clean return", err)
	}
	if writes := downstream.writtenPayloads(); len(writes) != 2 {
		t.Fatalf("completed turn writes = %#v, want created and completed", writes)
	}
}

func TestCodexTerminatingWSBrokerClassifiesAbnormalDownstreamReadWithoutPrivateData(t *testing.T) {
	t.Parallel()
	broker := &codexTerminatingWSBroker{}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{err: errors.New("private downstream read token-should-not-leak")}}}

	err := broker.Serve(context.Background(), downstream)
	if err == nil {
		t.Fatal("abnormal downstream read succeeded")
	}
	failure := classifyCodexWebSocketFailure(err)
	if failure.Stage != "downstream_read" || failure.Reason != "downstream_read_failed" {
		t.Fatalf("downstream read failure = %s/%s, want downstream_read/downstream_read_failed", failure.Stage, failure.Reason)
	}
	if strings.Contains(err.Error(), "token-should-not-leak") {
		t.Fatalf("broker error leaked private downstream text: %q", err)
	}
}

func TestCodexTerminatingWSBrokerExhaustsSameAccountAuthBeforeRotation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime:  newCodexLeaseRuntimeTest(t, coordinator),
		slots:    []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
		attempts: map[codex.AccountKey]int{"account-a": 2},
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)}, body: []byte(`{"error":{"type":"authentication_error"}}`), err: errors.New("unauthorised")},
			{conn: upstream, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}},
		},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	ctx := withCodexWSFrameObservationSink(context.Background(), func(*routeDiagnostics) { accepted++ })
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("accepted frame observations = %d, want 1 across candidate retry", accepted)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-a"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := upstream.writtenPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], frame) {
		t.Fatalf("successful candidate writes = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerRotatesHandshakeHard429BeforeAdmission(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}, body: []byte(`{"error":{"type":"usage_limit_reached"}}`), err: errors.New("rejected")}},
		"account-b": {{conn: upstreamB, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := upstreamB.writtenPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], frame) {
		t.Fatalf("B writes = %#v", got)
	}
}

func codexTerminatingWSFrame(turn, extra string) []byte {
	return []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-a","thread_id":"thread-a","turn_id":"` + turn + `","request_kind":"turn"}},"input":[]` + extra + `}`)
}

func codexWSBrokerHard429() []byte {
	return []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
}

type codexWSBrokerPlannerStub struct {
	runtime        *CodexLeaseRuntime
	receipt        *codexTurnReceiptHandle
	slots          []CodexLeaseAttemptSlotPlan
	attempts       map[codex.AccountKey]int
	prewarmCalls   int
	adoptionCalls  int
	cancelCalls    int
	buildCalls     int
	prewarmHeaders http.Header
	buildHeaders   http.Header
	adoptionFrozen *CodexFrozenRequest
}

type codexWSBrokerContextBlockingPlanner struct {
	started     chan struct{}
	startedOnce sync.Once
}

type codexWSBrokerPayloadConn struct {
	payloads [][]byte
	index    int
}

func (conn *codexWSBrokerPayloadConn) ReadMessage() (int, []byte, error) {
	if conn.index >= len(conn.payloads) {
		return 0, nil, io.EOF
	}
	payload := conn.payloads[conn.index]
	conn.index++
	return websocket.TextMessage, payload, nil
}

func (*codexWSBrokerPayloadConn) WriteMessage(int, []byte) error            { return nil }
func (*codexWSBrokerPayloadConn) WriteControl(int, []byte, time.Time) error { return nil }
func (*codexWSBrokerPayloadConn) SetReadDeadline(time.Time) error           { return nil }
func (*codexWSBrokerPayloadConn) SetWriteDeadline(time.Time) error          { return nil }
func (*codexWSBrokerPayloadConn) Close() error                              { return nil }

func (planner *codexWSBrokerContextBlockingPlanner) Build(ctx context.Context, _ CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	planner.startedOnce.Do(func() { close(planner.started) })
	<-ctx.Done()
	return CodexPreparedHTTPRequest{}, ctx.Err()
}

func (planner *codexWSBrokerPlannerStub) Build(_ context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	planner.buildCalls++
	planner.buildHeaders = input.Headers.Clone()
	request, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	plan := codexLeaseRuntimeTestPlan(request.Metadata.Metadata.TurnID, planner.slots)
	plan.Evidence = CodexLeaseRequestEvidence{
		PreviousResponseID: request.PreviousResponseID,
		TurnState:          request.TurnState,
		HasTurnState:       request.HasTurnState,
		HasEncryptedState:  request.HasEncryptedState,
	}
	handle, err := planner.runtime.BeginRequest(plan)
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	accounts := make([]CodexFrozenDispatchAccount, 0, len(planner.slots))
	for _, slot := range planner.slots {
		attemptCount := max(1, planner.attempts[slot.AccountKey])
		attempts := make([]CandidateAttempt, 0, attemptCount)
		for ordinal := 1; ordinal <= attemptCount; ordinal++ {
			attempts = append(attempts, CandidateAttempt{AccountKey: slot.AccountKey, Candidate: codex.CandidateRef{AccountKey: slot.AccountKey, CandidateID: codex.CandidateID(slot.CandidateID)}, Revision: codex.Revision("revision-" + string(rune('0'+ordinal))), Ordinal: ordinal})
		}
		accounts = append(accounts, CodexFrozenDispatchAccount{
			choice:   RouteChoice{AccountKey: slot.AccountKey, RequestedModel: request.Model, EffectiveModel: request.Model, RequiredBuckets: []CapacityBucket{CapacityBucketBase}},
			attempts: attempts,
		})
	}
	return CodexPreparedHTTPRequest{
		Dispatch:    CodexFrozenDispatchPlan{accounts: accounts, status: CodexRoutePlanReady},
		Lifecycle:   NewCodexHTTPRequestLifecycle(handle),
		leaseHandle: handle,
		receipt:     planner.receipt,
	}, nil
}

func (planner *codexWSBrokerPlannerStub) planWebSocketPrewarm(_ context.Context, input CodexHTTPRequestPlanInput) (CodexFrozenDispatchPlan, error) {
	planner.prewarmCalls++
	planner.prewarmHeaders = input.Headers.Clone()
	request, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexFrozenDispatchPlan{}, err
	}
	accounts := make([]CodexFrozenDispatchAccount, 0, len(planner.slots))
	for _, slot := range planner.slots {
		accounts = append(accounts, CodexFrozenDispatchAccount{
			choice:   RouteChoice{AccountKey: slot.AccountKey, RequestedModel: request.Model, EffectiveModel: request.Model, RequiredBuckets: []CapacityBucket{CapacityBucketBase}},
			attempts: []CandidateAttempt{{AccountKey: slot.AccountKey, Candidate: codex.CandidateRef{AccountKey: slot.AccountKey, CandidateID: codex.CandidateID(slot.CandidateID)}, Revision: "revision-1", Ordinal: 1}},
		})
	}
	return CodexFrozenDispatchPlan{accounts: accounts, status: CodexRoutePlanReady}, nil
}

func (planner *codexWSBrokerPlannerStub) beginWebSocketPrewarm(input CodexHTTPRequestPlanInput) (CodexPrewarmReservation, error) {
	request, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexPrewarmReservation{}, err
	}
	return planner.runtime.coordinator.prewarms.Create(request.Metadata.Metadata, "stub-prewarm-correlation")
}

func (planner *codexWSBrokerPlannerStub) bindWebSocketPrewarm(reservation CodexPrewarmReservation, account codex.AccountKey, downstreamGeneration, upstreamGeneration uint64) (CodexPrewarmReservation, error) {
	return planner.runtime.coordinator.prewarms.BindSockets(reservation.Lane, account, downstreamGeneration, upstreamGeneration)
}

func (planner *codexWSBrokerPlannerStub) readyWebSocketPrewarm(reservation CodexPrewarmReservation, responseAnchor, turnState string) (CodexPrewarmReservation, error) {
	return planner.runtime.coordinator.prewarms.Ready(reservation.Lane, responseAnchor, turnState)
}

func (planner *codexWSBrokerPlannerStub) cancelWebSocketPrewarm(reservation CodexPrewarmReservation) error {
	planner.cancelCalls++
	return planner.runtime.coordinator.prewarms.cancel(reservation.Lane, reservation.Correlation)
}

func (planner *codexWSBrokerPlannerStub) adoptWebSocketPrewarm(ctx context.Context, input CodexHTTPRequestPlanInput, reservation CodexPrewarmReservation, revalidate CodexPrewarmAdoptionRevalidator) (CodexPreparedHTTPRequest, error) {
	planner.adoptionCalls++
	request, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	var selected CodexLeaseAttemptSlotPlan
	for _, slot := range planner.slots {
		if slot.AccountKey == reservation.AccountKey {
			selected = slot
			break
		}
	}
	if selected.AccountKey == "" || request.PreviousResponseID != reservation.ResponseAnchor {
		return CodexPreparedHTTPRequest{}, ErrCodexContinuity
	}
	choice := RouteChoice{AccountKey: selected.AccountKey, RequestedModel: request.Model, EffectiveModel: request.Model, RequiredBuckets: []CapacityBucket{CapacityBucketBase}}
	handle, err := planner.runtime.adoptWebSocketPrewarmContext(ctx, []codex.AccountKey{selected.AccountKey}, CodexPrewarmAdoptionRequest{
		Key:                        NewCodexLeaseKey(request.Metadata.Metadata),
		Policy:                     CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		Choice:                     choice,
		AttemptSlots:               []CodexPrewarmAttemptSlot{{AccountKey: selected.AccountKey, CandidateID: codex.CandidateID(selected.CandidateID), Kind: selected.Kind}},
		RequestKind:                request.Metadata.Metadata.RequestKind,
		Correlation:                reservation.Correlation,
		ResponseAnchor:             reservation.ResponseAnchor,
		TurnState:                  reservation.TurnState,
		ReservationGeneration:      reservation.Generation,
		DownstreamSocketGeneration: reservation.DownstreamSocketGeneration,
		UpstreamSocketGeneration:   reservation.UpstreamSocketGeneration,
		Revalidate:                 revalidate,
	})
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	dispatch := CodexFrozenDispatchPlan{accounts: []CodexFrozenDispatchAccount{{
		choice:   choice,
		attempts: []CandidateAttempt{{AccountKey: selected.AccountKey, Candidate: codex.CandidateRef{AccountKey: selected.AccountKey, CandidateID: codex.CandidateID(selected.CandidateID)}, Revision: "revision-1", Ordinal: 1}},
	}}, status: CodexRoutePlanReady}
	lifecycle := wrapCodexTurnReceiptLifecycle(NewCodexHTTPRequestLifecycle(handle), planner.receipt)
	return CodexPreparedHTTPRequest{Dispatch: dispatch, Frozen: planner.adoptionFrozen, Lifecycle: lifecycle, leaseHandle: handle, receipt: planner.receipt}, nil
}

type codexWSBrokerRead struct {
	messageType int
	payload     []byte
	err         error
}

type codexWSBrokerWrite struct {
	messageType int
	payload     []byte
}

type codexWSBrokerConnStub struct {
	mu                    sync.Mutex
	reads                 []codexWSBrokerRead
	writes                []codexWSBrokerWrite
	writeErr              error
	writeCalls            int
	failWriteAt           int
	controlWrites         chan<- codexWSBrokerWrite
	closed                bool
	roleKnown             bool
	downstream            bool
	downstreamReadBlocked bool
	downstreamReadWake    chan struct{}
	readInterrupted       bool
}

func (conn *codexWSBrokerConnStub) ReadMessage() (int, []byte, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.roleKnown {
		conn.roleKnown = true
		conn.downstream = conn.writeCalls == 0
	}
	for conn.downstreamReadBlocked && !conn.closed && !conn.readInterrupted {
		wake := conn.downstreamReadWake
		conn.mu.Unlock()
		<-wake
		conn.mu.Lock()
	}
	if conn.closed || conn.readInterrupted {
		return 0, nil, errors.New("closed")
	}
	if len(conn.reads) == 0 {
		return 0, nil, io.EOF
	}
	read := conn.reads[0]
	conn.reads = conn.reads[1:]
	if conn.downstream && read.err == nil {
		conn.downstreamReadBlocked = true
		conn.downstreamReadWake = make(chan struct{})
	}
	return read.messageType, append([]byte(nil), read.payload...), read.err
}

func (conn *codexWSBrokerConnStub) WriteMessage(messageType int, payload []byte) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return errors.New("closed")
	}
	conn.writeCalls++
	if conn.writeErr != nil && (conn.failWriteAt == 0 || conn.writeCalls == conn.failWriteAt) {
		return conn.writeErr
	}
	conn.writes = append(conn.writes, codexWSBrokerWrite{messageType: messageType, payload: append([]byte(nil), payload...)})
	if conn.downstream && codexWSBrokerDownstreamTerminalWrite(messageType, payload) {
		conn.releaseDownstreamReadLocked()
	}
	return nil
}

func (conn *codexWSBrokerConnStub) WriteControl(messageType int, payload []byte, _ time.Time) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return errors.New("closed")
	}
	if conn.controlWrites != nil {
		conn.controlWrites <- codexWSBrokerWrite{messageType: messageType, payload: append([]byte(nil), payload...)}
	}
	return nil
}

func (conn *codexWSBrokerConnStub) SetReadDeadline(time.Time) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.readInterrupted = true
	conn.releaseDownstreamReadLocked()
	return nil
}
func (conn *codexWSBrokerConnStub) SetWriteDeadline(time.Time) error { return nil }
func (conn *codexWSBrokerConnStub) Close() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.closed = true
	conn.releaseDownstreamReadLocked()
	return nil
}

func (conn *codexWSBrokerConnStub) releaseDownstreamReadLocked() {
	if !conn.downstreamReadBlocked {
		return
	}
	conn.downstreamReadBlocked = false
	close(conn.downstreamReadWake)
	conn.downstreamReadWake = nil
}

func codexWSBrokerDownstreamTerminalWrite(messageType int, payload []byte) bool {
	if messageType == websocket.CloseMessage {
		return true
	}
	if messageType != websocket.TextMessage {
		return false
	}
	switch classifyCodexSSEData(payload).Kind {
	case CodexSSECompleted, CodexSSEError:
		return true
	default:
		return false
	}
}

func (conn *codexWSBrokerConnStub) writtenPayloads() [][]byte {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	result := make([][]byte, 0, len(conn.writes))
	for _, write := range conn.writes {
		if write.messageType == websocket.TextMessage {
			result = append(result, append([]byte(nil), write.payload...))
		}
	}
	return result
}

func waitCodexWSBrokerPlanningGateState(t *testing.T, runtimeLease *CodexLeaseRuntime, lane LaneKey, wantRefs int, wantHeld bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runtimeLease.planningGates.mu.Lock()
		entry := runtimeLease.planningGates.entries[lane]
		refs := 0
		held := false
		if entry != nil {
			refs = entry.refs
			held = len(entry.token) == 0
		}
		runtimeLease.planningGates.mu.Unlock()
		if refs == wantRefs && held == wantHeld {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("WebSocket planning gate did not reach refs %d held %v", wantRefs, wantHeld)
}

type codexWSBrokerDialerStub struct {
	connections map[codex.AccountKey][]websocketRelayConn
	outcomes    map[codex.AccountKey][]codexWSBrokerDialOutcome
	onDial      func()
	accounts    []codex.AccountKey
}

type codexWSBrokerDialOutcome struct {
	conn         websocketRelayConn
	response     *http.Response
	body         []byte
	err          error
	skipDispatch bool
}

type codexWSBrokerBlockingConn struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type codexWSBrokerCloseBlockingConn struct {
	*codexWSBrokerConnStub
	closeStarted chan struct{}
	closeRelease chan struct{}
	closeOnce    sync.Once
}

func newCodexWSBrokerCloseBlockingConn(reads []codexWSBrokerRead) *codexWSBrokerCloseBlockingConn {
	return &codexWSBrokerCloseBlockingConn{
		codexWSBrokerConnStub: &codexWSBrokerConnStub{reads: reads},
		closeStarted:          make(chan struct{}),
		closeRelease:          make(chan struct{}),
	}
}

func (conn *codexWSBrokerCloseBlockingConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closeStarted) })
	<-conn.closeRelease
	return conn.codexWSBrokerConnStub.Close()
}

type codexWSBrokerSuccessorConn struct {
	*codexWSBrokerConnStub
	gate       chan struct{}
	gateOnce   sync.Once
	mu         sync.Mutex
	readCount  int
	writeCount int
}

func newCodexWSBrokerSuccessorConn(reads []codexWSBrokerRead) *codexWSBrokerSuccessorConn {
	return &codexWSBrokerSuccessorConn{
		codexWSBrokerConnStub: &codexWSBrokerConnStub{reads: reads},
		gate:                  make(chan struct{}),
	}
}

func (conn *codexWSBrokerSuccessorConn) ReadMessage() (int, []byte, error) {
	conn.mu.Lock()
	conn.readCount++
	readCount := conn.readCount
	conn.mu.Unlock()
	if readCount == 3 {
		<-conn.gate
	}
	return conn.codexWSBrokerConnStub.ReadMessage()
}

func (conn *codexWSBrokerSuccessorConn) WriteMessage(messageType int, payload []byte) error {
	if err := conn.codexWSBrokerConnStub.WriteMessage(messageType, payload); err != nil {
		return err
	}
	conn.mu.Lock()
	conn.writeCount++
	writeCount := conn.writeCount
	conn.mu.Unlock()
	if writeCount == 2 {
		conn.gateOnce.Do(func() { close(conn.gate) })
	}
	return nil
}

func newCodexWSBrokerBlockingConn() *codexWSBrokerBlockingConn {
	return &codexWSBrokerBlockingConn{started: make(chan struct{}), release: make(chan struct{})}
}

func (conn *codexWSBrokerBlockingConn) ReadMessage() (int, []byte, error) {
	conn.once.Do(func() { close(conn.started) })
	<-conn.release
	return 0, nil, context.Canceled
}

func (*codexWSBrokerBlockingConn) WriteMessage(int, []byte) error { return nil }
func (*codexWSBrokerBlockingConn) WriteControl(int, []byte, time.Time) error {
	return nil
}
func (conn *codexWSBrokerBlockingConn) SetReadDeadline(time.Time) error {
	select {
	case <-conn.release:
	default:
		close(conn.release)
	}
	return nil
}
func (*codexWSBrokerBlockingConn) SetWriteDeadline(time.Time) error { return nil }
func (conn *codexWSBrokerBlockingConn) Close() error {
	select {
	case <-conn.release:
	default:
		close(conn.release)
	}
	return nil
}

func (dialer *codexWSBrokerDialerStub) Dial(_ context.Context, choice RouteChoice, attempt CandidateAttempt, _ string, _ http.Header, onDispatch func(CandidateAttempt)) (websocketRelayConn, *http.Response, []byte, CandidateAttempt, error) {
	dialer.accounts = append(dialer.accounts, choice.AccountKey)
	if dialer.onDial != nil {
		dialer.onDial()
	}
	if outcomes := dialer.outcomes[choice.AccountKey]; len(outcomes) != 0 {
		outcome := outcomes[0]
		dialer.outcomes[choice.AccountKey] = outcomes[1:]
		if onDispatch != nil && !outcome.skipDispatch {
			onDispatch(attempt)
		}
		return outcome.conn, outcome.response, append([]byte(nil), outcome.body...), attempt, outcome.err
	}
	connections := dialer.connections[choice.AccountKey]
	if len(connections) == 0 {
		return nil, nil, nil, attempt, errors.New("no scripted connection")
	}
	dialer.connections[choice.AccountKey] = connections[1:]
	if onDispatch != nil {
		onDispatch(attempt)
	}
	return connections[0], &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}, nil, attempt, nil
}
