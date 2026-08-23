package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

func TestCodexTerminatingWSBrokerForwardsInstalledPrewarmBeforeDurableTurn(t *testing.T) {
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
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
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
	if got := upstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{prewarm, turn}) {
		t.Fatalf("upstream writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); len(got) != 4 {
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
	if len(restored.ResolvedRecords) != 1 || !restored.ResolvedRecords[0].Record.AdoptedPrewarm {
		t.Fatalf("durable prewarm adoption = %#v", restored.ResolvedRecords)
	}
	gotReceipt, found := receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if !found || gotReceipt.State != CodexTurnReceiptCompleted || gotReceipt.ActualAccountHint != redactedAccountHint("codex", "account-a") {
		t.Fatalf("reused-socket receipt = (%+v, %v)", gotReceipt, found)
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
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
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
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if planner.prewarmCalls != 1 || planner.adoptionCalls != 0 || planner.cancelCalls != 1 || planner.buildCalls != 1 {
		t.Fatalf("planner calls = prewarm %d adoption %d cancel %d build %d, want 1/0/1/1", planner.prewarmCalls, planner.adoptionCalls, planner.cancelCalls, planner.buildCalls)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v, want prewarmed connection reuse", got)
	}
	if got := upstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{prewarm, turn}) {
		t.Fatalf("upstream writes = %#v", got)
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

func TestCodexTerminatingWSBrokerNeverRotatesNonPortableOrAdmittedTurn(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		frame []byte
		reads []codexWSBrokerRead
	}{
		{
			name:  "incremental request",
			frame: codexTerminatingWSFrame("turn-incremental", `,"previous_response_id":"response-old"`),
			reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}},
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
			planner := &codexWSBrokerPlannerStub{
				runtime: newCodexLeaseRuntimeTest(t, coordinator),
				slots: []CodexLeaseAttemptSlotPlan{
					{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
					{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
				},
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
	second := codexTerminatingWSFrame("turn-b", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: first},
		{messageType: websocket.TextMessage, payload: second},
		{err: io.EOF},
	}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
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

func TestCodexTerminatingWSBrokerTranslatesHandshakeFailureWithoutPrivateBody(t *testing.T) {
	t.Parallel()
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
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	writes := downstream.writtenPayloads()
	if len(writes) != 1 || string(writes[0]) != `{"type":"error","status":401,"error":{"type":"authentication_error"}}` {
		t.Fatalf("translated writes = %q", writes)
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
}

func (planner *codexWSBrokerPlannerStub) Build(_ context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	planner.buildCalls++
	planner.buildHeaders = input.Headers.Clone()
	request, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	handle, err := planner.runtime.BeginRequest(codexLeaseRuntimeTestPlan(request.Metadata.Metadata.TurnID, planner.slots))
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
	return CodexPreparedHTTPRequest{Dispatch: dispatch, Lifecycle: NewCodexHTTPRequestLifecycle(handle), leaseHandle: handle, receipt: planner.receipt}, nil
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
	mu     sync.Mutex
	reads  []codexWSBrokerRead
	writes []codexWSBrokerWrite
	closed bool
}

func (conn *codexWSBrokerConnStub) ReadMessage() (int, []byte, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.reads) == 0 {
		return 0, nil, io.EOF
	}
	read := conn.reads[0]
	conn.reads = conn.reads[1:]
	return read.messageType, append([]byte(nil), read.payload...), read.err
}

func (conn *codexWSBrokerConnStub) WriteMessage(messageType int, payload []byte) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return errors.New("closed")
	}
	conn.writes = append(conn.writes, codexWSBrokerWrite{messageType: messageType, payload: append([]byte(nil), payload...)})
	return nil
}

func (conn *codexWSBrokerConnStub) SetReadDeadline(time.Time) error  { return nil }
func (conn *codexWSBrokerConnStub) SetWriteDeadline(time.Time) error { return nil }
func (conn *codexWSBrokerConnStub) Close() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.closed = true
	return nil
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

type codexWSBrokerDialerStub struct {
	connections map[codex.AccountKey][]websocketRelayConn
	outcomes    map[codex.AccountKey][]codexWSBrokerDialOutcome
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

func newCodexWSBrokerBlockingConn() *codexWSBrokerBlockingConn {
	return &codexWSBrokerBlockingConn{started: make(chan struct{}), release: make(chan struct{})}
}

func (conn *codexWSBrokerBlockingConn) ReadMessage() (int, []byte, error) {
	conn.once.Do(func() { close(conn.started) })
	<-conn.release
	return 0, nil, context.Canceled
}

func (*codexWSBrokerBlockingConn) WriteMessage(int, []byte) error { return nil }
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
