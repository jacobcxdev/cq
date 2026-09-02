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
	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
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
		Capacity:             capacity,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	ctx := withCodexTrace(context.Background(), diagnostics, nil, CodexTraceStart{Transport: "websocket"})
	ctx = withCodexWSFrameObservationSink(ctx, func(*routeDiagnostics) { accepted++ })
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
	if got := capacity.Capacity("account-a", CapacityBucketForModel("gpt-5.6-sol")); got.State != CapacityZero || got.Source != CapacitySourceHardLimit || !got.ResetAt.IsZero() {
		t.Fatalf("A hard-limit capacity = %+v", got)
	}
	gotReceipt, found := receiptStore.lookup([]byte("session-a"), []byte("turn-a"))
	if !found || gotReceipt.State != CodexTurnReceiptCompleted || gotReceipt.ActualAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("rotated receipt = (%+v, %v)", gotReceipt, found)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	traceEvents := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "account_unavailable" && event.Reason == "capacity_exhausted" && event.AccountHint == codexTraceAccountHint("account-a")
	}, "WebSocket 429 exhaustion")
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "failover" && event.AccountHint == codexTraceAccountHint("account-b") && event.Failover
	}, "WebSocket account failover")
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "lease_transition" && event.Stage == "quota_exhausted" && event.Outcome == "before" && event.Lease != nil && event.Lease.RequestGeneration != 0
	}, "WebSocket lease state before quota transition")
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "lease_transition" && event.Stage == "quota_exhausted" && event.Outcome == "after" && event.Lease != nil && event.Lease.AttemptGeneration != 0
	}, "WebSocket lease state after quota transition")
}

func TestCodexTerminatingWSBrokerRotatesLaterPortableRequestAfterHard429(t *testing.T) {
	t.Parallel()
	testCodexTerminatingWSBrokerRotatesLaterPortableRequest(t, codexWSBrokerHard429())
}

func TestCodexTerminatingWSBrokerRotatesLaterPortableRequestAfterAuthFailure(t *testing.T) {
	t.Parallel()
	testCodexTerminatingWSBrokerRotatesLaterPortableRequest(t, []byte(`{"type":"error","status":401,"error":{"type":"authentication_error"}}`))
}

func TestCodexTerminatingWSBrokerRetriesApplicationAuthOnRemainingSameAccountCandidate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	attemptA1 := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-a-1", Ordinal: 1}
	attemptA2 := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-2"}, Revision: "revision-a-2", Ordinal: 2}
	attemptB := CandidateAttempt{AccountKey: "account-b", Candidate: codex.CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"}, Revision: "revision-b", Ordinal: 1}
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a-1", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-a", CandidateID: "candidate-a-2", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
		frozenAccounts: []CodexFrozenDispatchAccount{
			{choice: RouteChoice{AccountKey: "account-a", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attemptA1, attemptA2}},
			{choice: RouteChoice{AccountKey: "account-b", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attemptB}},
		},
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	firstA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"error","status":401,"error":{"type":"authentication_error"}}`)}}}
	secondA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {firstA, secondA},
		"account-b": {&codexWSBrokerConnStub{}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-a"}) {
		t.Fatalf("dial accounts = %#v, want same account credential retry", got)
	}
	if len(dialer.attempts) != 2 {
		t.Fatalf("dial attempts = %#v", dialer.attempts)
	}
	if got := []codex.CandidateID{dialer.attempts[0].Candidate.CandidateID, dialer.attempts[1].Candidate.CandidateID}; !reflect.DeepEqual(got, []codex.CandidateID{"candidate-a-1", "candidate-a-2"}) {
		t.Fatalf("dial candidates = %#v", got)
	}
	if got := firstA.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{frame}) {
		t.Fatalf("first A writes = %#v", got)
	}
	if got := secondA.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{frame}) {
		t.Fatalf("second A writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"response-a"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`),
	}) {
		t.Fatalf("downstream writes = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerRefreshesAfterApplicationAuthBeforeAccountRotation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	direct := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"}, Revision: "revision-stale", Source: codex.SourceManaged, Ordinal: 1}
	refresh := direct
	attemptB := CandidateAttempt{AccountKey: "account-b", Candidate: codex.CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"}, Revision: "revision-b", Ordinal: 1}
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotEligibleManagedRefresh},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
		frozenAccounts: []CodexFrozenDispatchAccount{
			{choice: RouteChoice{AccountKey: "account-a", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{direct}, refreshAttempt: &refresh},
			{choice: RouteChoice{AccountKey: "account-b", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attemptB}},
		},
	}
	refresher := &codexHTTPSessionRefresher{
		wantRef: direct.Candidate, wantRevision: direct.Revision,
		ref: direct.Candidate, revision: "revision-fresh",
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	firstA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"error","status":401,"error":{"type":"authentication_error"}}`)}}}
	refreshedA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {firstA, refreshedA},
		"account-b": {&codexWSBrokerConnStub{}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, Refresher: refresher, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-a"}) {
		t.Fatalf("dial accounts = %#v, want refreshed same account", got)
	}
	if len(dialer.attempts) != 2 || dialer.attempts[1].Revision != "revision-fresh" || dialer.attempts[1].Ordinal != 2 {
		t.Fatalf("dial attempts = %#v", dialer.attempts)
	}
}

func TestCodexTerminatingWSBrokerExhaustsSameAccountApplicationAuthBeforeRotation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	attemptA1 := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-a-1", Ordinal: 1}
	attemptA2 := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-2"}, Revision: "revision-a-2", Ordinal: 2}
	refresh := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-refresh"}, Revision: "revision-stale", Source: codex.SourceManaged, Ordinal: 3}
	attemptB := CandidateAttempt{AccountKey: "account-b", Candidate: codex.CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"}, Revision: "revision-b", Ordinal: 1}
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a-1", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-a", CandidateID: "candidate-a-2", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-a", CandidateID: "candidate-a-refresh", Kind: CodexAttemptSlotEligibleManagedRefresh},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
		frozenAccounts: []CodexFrozenDispatchAccount{
			{choice: RouteChoice{AccountKey: "account-a", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attemptA1, attemptA2}, refreshAttempt: &refresh},
			{choice: RouteChoice{AccountKey: "account-b", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attemptB}},
		},
	}
	refresher := &codexHTTPSessionRefresher{
		wantRef: refresh.Candidate, wantRevision: refresh.Revision,
		ref: refresh.Candidate, revision: "revision-fresh",
	}
	request := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: request}, {err: io.EOF}}}
	auth := func() *codexWSBrokerConnStub {
		return &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"error","status":401,"error":{"type":"authentication_error"}}`)}}}
	}
	created := []byte(`{"type":"response.created","response":{"id":"response-b"}}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: created},
		{messageType: websocket.TextMessage, payload: completed},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {auth(), auth(), auth()},
		"account-b": {upstreamB},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, Refresher: refresher, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-a", "account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v, want all A credentials before B", got)
	}
	if len(dialer.attempts) != 4 || dialer.attempts[2].Candidate.CandidateID != "candidate-a-refresh" || dialer.attempts[2].Revision != "revision-fresh" {
		t.Fatalf("dial attempts = %#v", dialer.attempts)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{created, completed}) {
		t.Fatalf("downstream writes = %#v, want only healthy B outcome", got)
	}
}

func TestCodexTerminatingWSBrokerDoesNotRotateApplicationPolicy403(t *testing.T) {
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
	policy := []byte(`{"type":"error","status":403,"error":{"type":"policy_violation","code":"safety_denial"}}`)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: policy}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {upstreamA},
		"account-b": {&codexWSBrokerConnStub{}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v, want no policy failover", got)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{policy}) {
		t.Fatalf("downstream writes = %#v, want exact policy rejection", got)
	}
	snapshot, err := coordinator.LoadRouteSnapshot(
		context.Background(),
		LeaseKey{Lane: LaneKey{Session: "runtime-session", Thread: "runtime-thread", Namespace: CodexResponsesNamespace}, Turn: "turn-a"},
		[]codex.AccountKey{"account-a", "account-b"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.UnavailableAccountKeys) != 0 || len(snapshot.QuotaExhaustedAccountKeys) != 0 {
		t.Fatalf("policy rejection changed account availability: %+v", snapshot)
	}
}

func TestCodexTerminatingWSBrokerRotatesResponseFailedHardUsageLimitBeforeAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		failed []byte
	}{
		{name: "usage limit type", failed: []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"usage exhausted"}}}`)},
		{name: "insufficient quota code", failed: []byte(`{"type":"response.failed","response":{"error":{"code":"insufficient_quota","message":"quota exhausted"}}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			planner := &codexWSBrokerPlannerStub{
				runtime: newCodexLeaseRuntimeTest(t, coordinator),
				slots: []CodexLeaseAttemptSlotPlan{
					{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
					{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
				},
			}
			request := codexTerminatingWSFrame("turn-a", "")
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: request}, {err: io.EOF}}}
			upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: test.failed}}}
			created := []byte(`{"type":"response.created","response":{"id":"response-b"}}`)
			completed := []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)
			upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
				{messageType: websocket.TextMessage, payload: created},
				{messageType: websocket.TextMessage, payload: completed},
			}}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
				"account-a": {upstreamA},
				"account-b": {upstreamB},
			}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Serve(context.Background(), downstream); err != nil {
				t.Fatal(err)
			}
			if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
				t.Fatalf("dial accounts = %#v, want hard-limit rotation", got)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{created, completed}) {
				t.Fatalf("downstream writes = %#v, want healthy fallback only", got)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerDoesNotRotateResponseFailedPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		failed []byte
	}{
		{name: "policy type", failed: []byte(`{"type":"response.failed","response":{"error":{"type":"policy_violation","code":"safety_denial"}}}`)},
		{name: "rate limit code", failed: []byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`)},
		{name: "cyber policy code", failed: []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy"}}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
			diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
			if traceErr != nil {
				t.Fatal(traceErr)
			}
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			planner := &codexWSBrokerPlannerStub{
				runtime: newCodexLeaseRuntimeTest(t, coordinator),
				slots: []CodexLeaseAttemptSlotPlan{
					{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
					{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
				},
			}
			request := codexTerminatingWSFrame("turn-a", "")
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: request}, {err: io.EOF}}}
			upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: test.failed}}}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
				"account-a": {upstreamA},
				"account-b": {&codexWSBrokerConnStub{}},
			}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			ctx := withCodexTrace(context.Background(), diagnostics, nil, CodexTraceStart{Transport: "websocket"})
			if err := broker.Serve(ctx, downstream); err != nil {
				t.Fatal(err)
			}
			if err := diagnostics.Close(); err != nil {
				t.Fatal(err)
			}
			traceEvents := readCodexTraceEvents(t, tracePath)
			requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
				return event.Phase == "terminal" && event.Outcome == "error" && event.EventName == string(CodexSSEError)
			}, "WebSocket provider failure terminal outcome")
			for _, event := range traceEvents {
				if event.Phase == "terminal" && event.Outcome == "success" {
					t.Fatalf("provider failure traced as terminal success: %+v", event)
				}
			}
			if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
				t.Fatalf("dial accounts = %#v, want no policy failover", got)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{test.failed}) {
				t.Fatalf("downstream writes = %#v, want exact policy outcome", got)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerDoesNotReplayAdmittedAccountUnavailableOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		frame      []byte
		wantReason string
	}{
		{name: "401", frame: []byte(`{"type":"error","status":401,"error":{"type":"authentication_error"}}`), wantReason: "auth_rejected"},
		{name: "auth-shaped 403", frame: []byte(`{"type":"error","status":403,"error":{"type":"authentication_error"}}`), wantReason: "auth_rejected"},
		{name: "hard 429", frame: codexWSBrokerHard429(), wantReason: "capacity_exhausted"},
		{name: "response.failed hard limit", frame: []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"usage exhausted"}}}`), wantReason: "capacity_exhausted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
			diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
			if traceErr != nil {
				t.Fatal(traceErr)
			}
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			planner := &codexWSBrokerPlannerStub{
				runtime: newCodexLeaseRuntimeTest(t, coordinator),
				slots: []CodexLeaseAttemptSlotPlan{
					{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
					{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
				},
			}
			request := codexTerminatingWSFrame("turn-a", "")
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: request}, {err: io.EOF}}}
			created := []byte(`{"type":"response.created","response":{"id":"response-a"}}`)
			upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
				{messageType: websocket.TextMessage, payload: created},
				{messageType: websocket.TextMessage, payload: test.frame},
			}}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
				"account-a": {upstreamA},
				"account-b": {&codexWSBrokerConnStub{}},
			}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			ctx := withCodexTrace(context.Background(), diagnostics, nil, CodexTraceStart{Transport: "websocket"})
			if err := broker.Serve(ctx, downstream); err != nil {
				t.Fatal(err)
			}
			if err := diagnostics.Close(); err != nil {
				t.Fatal(err)
			}
			traceEvents := readCodexTraceEvents(t, tracePath)
			requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
				return event.Phase == "account_unavailable" && event.Outcome == "observed" && event.Reason == test.wantReason && event.AccountHint != ""
			}, "WebSocket provider account-unavailable frame")
			requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
				return event.Phase == "terminal" && event.Outcome == "error" && event.Reason == test.wantReason && event.EventName == string(CodexSSEError) && event.AccountHint == codexTraceAccountHint("account-a")
			}, "WebSocket account-unavailable terminal outcome")
			for _, event := range traceEvents {
				if event.Phase == "terminal" && event.Outcome == "success" {
					t.Fatalf("account-unavailable frame traced as terminal success: %+v", event)
				}
			}
			if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
				t.Fatalf("dial accounts = %#v, admitted outcome must not replay", got)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{created, test.frame}) {
				t.Fatalf("downstream writes = %#v, want exact admitted outcome", got)
			}
			resolved, err := coordinator.store.LoadLane(
				LeaseKey{Lane: LaneKey{Session: "runtime-session", Thread: "runtime-thread", Namespace: CodexResponsesNamespace}, Turn: "turn-a"},
				[]codex.AccountKey{"account-a", "account-b"},
				CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Classification != CodexRestoredLaneCurrent || len(resolved.ResolvedRecords) != 1 {
				t.Fatalf("resolved lane = %#v", resolved)
			}
			record := resolved.ResolvedRecords[0].Record
			if record.State != LeaseBoundQuiescent || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
				t.Fatalf("admitted terminal cleanup = %#v", record)
			}
		})
	}
}

func testCodexTerminatingWSBrokerRotatesLaterPortableRequest(t *testing.T, unavailable []byte) {
	t.Helper()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
	}
	first := codexTerminatingWSFrame("turn-a", "")
	second := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: first},
		{messageType: websocket.TextMessage, payload: second},
		{err: io.EOF},
	}}
	upstreamA := &codexWSBrokerConnStub{readGateAfter: 2, readGateReleaseAtWrite: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: unavailable},
	}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {upstreamA},
		"account-b": {upstreamB},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := upstreamA.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{first, second}) {
		t.Fatalf("A writes = %#v", got)
	}
	if got := upstreamB.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{second}) {
		t.Fatalf("B writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"response-a"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"response-a"}}`),
		[]byte(`{"type":"response.created","response":{"id":"response-b"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`),
	}) {
		t.Fatalf("downstream writes = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerRotatesLaterPortableHandshakeHard429(t *testing.T) {
	t.Parallel()
	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	payloadPath := filepath.Join(t.TempDir(), "payloads.jsonl")
	diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
	if traceErr != nil {
		t.Fatal(traceErr)
	}
	payloads, payloadErr := OpenPayloadWriter(payloadPath)
	if payloadErr != nil {
		t.Fatal(payloadErr)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexWSBrokerContinuationRuntime(t, coordinator, "turn-a", "response-a")
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"120"}}}, body: []byte(`{"error":{"type":"usage_limit_reached"}}`), err: errors.New("rejected")}},
		"account-b": {{conn: upstreamB, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, Capacity: capacity, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), diagnostics, payloads, CodexTraceStart{Transport: "websocket"})
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatal(err)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := upstreamB.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{frame}) {
		t.Fatalf("B writes = %#v", got)
	}
	if got := capacity.Capacity("account-a", CapacityBucketForModel("gpt-5.6-sol")); got.State != CapacityZero || got.Source != CapacitySourceHardLimit || !got.ResetAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("A handshake hard-limit capacity = %+v", got)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	if err := payloads.Close(); err != nil {
		t.Fatal(err)
	}
	traceEvents := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "upstream_dial" && event.Outcome == "error" && event.UpstreamStatus == http.StatusTooManyRequests && event.ErrorClass == "capacity_exhausted"
	}, "WebSocket 429 handshake")
	payloadEvents := readCodexPayloadEvents(t, payloadPath)
	var handshake *PayloadEvent
	for index := range payloadEvents {
		if payloadEvents[index].Direction == "upstream_handshake_response" {
			handshake = &payloadEvents[index]
			break
		}
	}
	if handshake == nil {
		t.Fatalf("payload events = %+v, want rejected handshake", payloadEvents)
	}
	wantBody := `{"error":{"type":"usage_limit_reached"}}`
	if handshake.StatusCode != http.StatusTooManyRequests || handshake.AccountHint != codexTraceAccountHint("account-a") || handshake.Attempt != 1 || !handshake.Complete || string(handshake.Body) != wantBody {
		t.Fatalf("handshake payload = %+v, want account-a attempt 1 status 429 body %s", *handshake, wantBody)
	}
}

func TestCodexTerminatingWSBrokerSurfacesHard429OnceAfterAllAccountsExhausted(t *testing.T) {
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
	upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {upstreamA},
		"account-b": {upstreamB},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{codexWSBrokerHard429()}) {
		t.Fatalf("downstream writes = %#v, want one exact hard limit", got)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerRefreshesRefreshOnlyAccountBeforeHandshake(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	stale := CandidateAttempt{
		AccountKey: "account-a",
		Candidate:  codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"},
		Revision:   "revision-stale",
		Ordinal:    1,
	}
	planner := &codexWSBrokerPlannerStub{
		runtime:     newCodexLeaseRuntimeTest(t, coordinator),
		slots:       []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotEligibleManagedRefresh}},
		refreshOnly: map[codex.AccountKey]CandidateAttempt{"account-a": stale},
	}
	refresher := &codexHTTPSessionRefresher{
		wantRef: stale.Candidate, wantRevision: stale.Revision,
		ref: stale.Candidate, revision: "revision-fresh",
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, Refresher: refresher,
		UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	if len(dialer.attempts) != 1 || dialer.attempts[0].Revision != "revision-fresh" {
		t.Fatalf("dial attempts = %#v, want only refreshed revision", dialer.attempts)
	}
}

func TestCodexTerminatingWSBrokerSkipsRefreshOnlyAccountWhenRefreshFails(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	stale := CandidateAttempt{
		AccountKey: "account-a",
		Candidate:  codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"},
		Revision:   "revision-stale",
		Ordinal:    1,
	}
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotEligibleManagedRefresh},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
		refreshOnly: map[codex.AccountKey]CandidateAttempt{"account-a": stale},
	}
	refresher := &codexHTTPSessionRefresher{
		wantRef: stale.Candidate, wantRevision: stale.Revision,
		err: codex.ErrStaleRevision,
	}
	frame := codexTerminatingWSFrame("turn-a", "")
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-b": {upstreamB}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, Refresher: refresher,
		UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(context.Background(), downstream); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-b"}) {
		t.Fatalf("dial accounts = %#v, want stale account skipped", got)
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
	prewarmUpstream := &codexWSBrokerConnStub{readGateAfter: 3, readGateReleaseAtWrite: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
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
	firstUpstream := &codexWSBrokerConnStub{readGateAfter: 2, readGateReleaseAtWrite: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-a"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-a","end_turn":true}}`)},
	}}
	secondUpstream := &codexWSBrokerConnStub{readGateAfter: 2, readGateReleaseAtWrite: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-b","end_turn":true}}`)},
	}}
	staleUpstream := &codexWSBrokerConnStub{readGateAfter: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
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
	dial := broker.connect(context.Background(), handle, receipt, account, active, false)
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

func TestCodexTerminatingWSBrokerDoesNotReuseDifferentCredentialProvenance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		firstAttempts []CandidateAttempt
		firstOutcomes []codexWSBrokerDialOutcome
		nextAttempt   CandidateAttempt
		anchored      bool
	}{
		{
			name: "different candidate",
			firstAttempts: []CandidateAttempt{
				{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-1", Ordinal: 1},
				{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-2"}, Revision: "revision-2", Ordinal: 2},
			},
			firstOutcomes: []codexWSBrokerDialOutcome{
				{response: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)}, body: []byte(`{"error":{"type":"authentication_error"}}`), err: errors.New("unauthorised")},
				{conn: &codexWSBrokerConnStub{}, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}},
			},
			nextAttempt: CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-1", Ordinal: 1},
			anchored:    true,
		},
		{
			name: "stale revision",
			firstAttempts: []CandidateAttempt{
				{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-stale", Ordinal: 1},
			},
			firstOutcomes: []codexWSBrokerDialOutcome{
				{
					conn:     &codexWSBrokerConnStub{},
					response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)},
					actual:   CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-fresh", Ordinal: 1},
				},
			},
			nextAttempt: CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a-1"}, Revision: "revision-stale", Ordinal: 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			firstSlots := make([]CodexLeaseAttemptSlotPlan, 0, len(test.firstAttempts))
			for _, attempt := range test.firstAttempts {
				firstSlots = append(firstSlots, CodexLeaseAttemptSlotPlan{AccountKey: attempt.AccountKey, CandidateID: string(attempt.Candidate.CandidateID), Kind: CodexAttemptSlotDirect})
			}
			firstCoordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			firstHandle, err := newCodexLeaseRuntimeTest(t, firstCoordinator).BeginRequest(codexLeaseRuntimeTestPlan("turn-first", firstSlots))
			if err != nil {
				t.Fatal(err)
			}
			nextCoordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			nextHandle, err := newCodexLeaseRuntimeTest(t, nextCoordinator).BeginRequest(codexLeaseRuntimeTestPlan("turn-next", []CodexLeaseAttemptSlotPlan{{AccountKey: test.nextAttempt.AccountKey, CandidateID: string(test.nextAttempt.Candidate.CandidateID), Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			receiptStore, err := NewCodexTurnReceiptStore(strings.NewReader(strings.Repeat("e", 32)), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			receiptValue := testCodexTurnReceipt()
			receiptValue.Transport = CodexTurnReceiptTransportWebSocket
			receipt := receiptStore.register([]byte("session-a"), []byte("turn-a"), receiptValue)
			redial := &codexWSBrokerConnStub{}
			dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
				"account-a": append(append([]codexWSBrokerDialOutcome(nil), test.firstOutcomes...), codexWSBrokerDialOutcome{conn: redial, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}}),
			}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: &codexWSBrokerPlannerStub{}, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			active := &codexWSActiveUpstream{}
			first := broker.connect(context.Background(), firstHandle, receipt, CodexFrozenDispatchAccount{choice: RouteChoice{AccountKey: "account-a"}, attempts: test.firstAttempts}, active, false)
			if first.err != nil || first.lifecycle == nil || active.conn == nil {
				t.Fatalf("first dial = %#v, active = %#v", first, active)
			}
			firstConnection := active.conn

			next := broker.connect(context.Background(), nextHandle, receipt, CodexFrozenDispatchAccount{choice: RouteChoice{AccountKey: "account-a"}, attempts: []CandidateAttempt{test.nextAttempt}}, active, test.anchored)
			if next.err != nil || next.lifecycle == nil {
				t.Fatalf("next dial = %#v", next)
			}
			if active.conn != redial || active.conn == firstConnection {
				t.Fatalf("active connection = %p, want redial %p instead of stale %p", active.conn, redial, firstConnection)
			}
			if !firstConnection.(*codexWSBrokerConnStub).isClosed() {
				t.Fatal("mismatched credential connection was not closed before redial")
			}
			if got, want := dialer.attempts[len(dialer.attempts)-1], test.nextAttempt; got != want {
				t.Fatalf("redial attempt = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerPayloadDiagnosticsCaptureHiddenPrewarmFailureBeforeFailover(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloads, err := OpenPayloadWriter(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	hardLimit := codexWSBrokerHard429()
	downstream := &codexWSBrokerConnStub{}
	active := &codexWSActiveUpstream{
		conn:       &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: hardLimit}}},
		account:    "account-a",
		generation: 1,
	}
	broker := &codexTerminatingWSBroker{}
	account := CodexFrozenDispatchAccount{choice: RouteChoice{AccountKey: "account-a"}}
	ctx := withCodexTrace(context.Background(), nil, payloads, CodexTraceStart{Transport: "websocket"})
	rotate, err := broker.readPrewarmResponse(ctx, ctx, downstream, nil, account, true, active)
	if err != nil {
		t.Fatal(err)
	}
	if !rotate {
		t.Fatal("hidden prewarm capacity failure did not request failover")
	}
	if err := payloads.Close(); err != nil {
		t.Fatal(err)
	}
	events := readCodexPayloadEvents(t, payloadPath)
	if len(events) != 1 {
		t.Fatalf("payload events = %d, want hidden prewarm failure", len(events))
	}
	event := events[0]
	if event.EventType != "codex_payload" || event.TraceID == "" || event.ConnectionID == "" || event.Transport != "websocket" {
		t.Fatalf("payload trace context = %+v", event)
	}
	if event.Direction != "upstream_response" || event.AccountHint != codexTraceAccountHint("account-a") || event.FrameType != "text" || !event.Complete {
		t.Fatalf("payload event = %+v", event)
	}
	if string(event.Body) != string(hardLimit) {
		t.Fatalf("payload body = %s, want %s", event.Body, hardLimit)
	}
	if got := downstream.writtenPayloads(); len(got) != 0 {
		t.Fatalf("hidden failure was relayed downstream: %#v", got)
	}
}

func TestCodexTerminatingWSBrokerPrewarmTracksActualCredentialProvenance(t *testing.T) {
	t.Parallel()
	planned := CandidateAttempt{
		AccountKey: "account-a",
		Candidate:  codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"},
		Revision:   "revision-stale",
		Ordinal:    1,
	}
	actual := planned
	actual.Revision = "revision-fresh"
	firstConnection := &codexWSBrokerConnStub{}
	redial := &codexWSBrokerConnStub{}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {
			{conn: firstConnection, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}, actual: actual},
			{conn: redial, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}},
		},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: &codexWSBrokerPlannerStub{}, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	account := CodexFrozenDispatchAccount{choice: RouteChoice{AccountKey: "account-a"}, attempts: []CandidateAttempt{planned}}
	reservation := CodexPrewarmReservation{Correlation: "prewarm-a", State: CodexPrewarmCreating, Generation: 1}
	active := &codexWSActiveUpstream{prewarm: reservation}
	if dial := broker.connectPrewarm(context.Background(), account, active); dial.err != nil || active.conn != firstConnection {
		t.Fatalf("first prewarm dial = %#v, active = %#v", dial, active)
	}
	if active.attempt != actual {
		t.Fatalf("active prewarm attempt = %+v, want actual %+v", active.attempt, actual)
	}

	if dial := broker.connectPrewarm(context.Background(), account, active); dial.err != nil || active.conn != redial {
		t.Fatalf("redial = %#v, active = %#v", dial, active)
	}
	if !firstConnection.isClosed() {
		t.Fatal("mismatched prewarm credential connection was not closed before redial")
	}
	if active.prewarm != reservation {
		t.Fatalf("prewarm reservation = %+v, want preserved %+v", active.prewarm, reservation)
	}
	if got := dialer.attempts; len(got) != 2 || got[1] != planned {
		t.Fatalf("prewarm attempts = %+v, want stale plan redial", got)
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
	prewarmUpstream := &codexWSBrokerConnStub{readGateAfter: 2, readGateReleaseAtWrite: 2, readGate: make(chan struct{}), reads: []codexWSBrokerRead{
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

func TestCodexTerminatingWSBrokerRoutesSuccessorAfterAbandonedPredecessor(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planner := codexHTTPRequestPlanTestFactory(runtimeLease)
	planner.Routes = coordinator
	planner.TransportKind = "websocket"
	planner.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}

	predecessorPlan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	predecessorPlan.Key.Lane = LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}
	predecessorPlan.Authority = planner.Authority
	predecessorPlan.RequestedModel = "gpt-5"
	predecessorPlan.EffectiveModel = "gpt-5"
	predecessor, err := runtimeLease.BeginRequest(predecessorPlan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := predecessor.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}

	successorFrame := bytes.Replace(
		frozenRequestBody("gpt-5", CodexRequestTurn, "successor"),
		[]byte(`"turn_id":"turn"`),
		[]byte(`"turn_id":"turn-b"`),
		1,
	)
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: successorFrame},
		{err: io.EOF},
	}}
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"turn-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"turn-b","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account": {upstream},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatalf("successor WebSocket route after abandoned predecessor = %T %v", err, err)
	}
	if got := upstream.writtenPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], successorFrame) {
		t.Fatalf("successor upstream writes = %#v", got)
	}
	if got := downstream.writtenPayloads(); len(got) != 2 {
		t.Fatalf("successor downstream writes = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerFailsOpenWhenPrewarmResponseStalls(t *testing.T) {
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
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: prewarm},
		{err: io.EOF},
	}}
	upstream := newCodexWSBrokerBlockingConn()
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
		"account-a": {upstream},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
		UpstreamURL:          "wss://example.invalid/responses",
		DownstreamGeneration: 41,
		PrewarmTimeout:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- broker.Serve(context.Background(), downstream) }()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = upstream.Close()
		<-serveDone
		t.Fatal("stalled upstream prewarm blocked broker")
	}
	const wantFrame = `{"type":"error","status":504,"error":{"type":"api_error"}}`
	if writes := downstream.writtenPayloads(); len(writes) != 1 || string(writes[0]) != wantFrame {
		t.Fatalf("fail-open writes = %q, want %q", writes, wantFrame)
	}
	if planner.cancelCalls != 1 {
		t.Fatalf("prewarm cancellations = %d, want 1", planner.cancelCalls)
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

func TestCodexTerminatingWSBrokerSurfacesCurrentAdmissionFailureWithoutReplay(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlanner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		},
	}
	frame := codexTerminatingWSFrame("turn-admitted", "")
	closeFrames := make(chan codexWSBrokerWrite, 1)
	firstDownstream := &codexWSBrokerConnStub{
		reads:         []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}},
		controlWrites: closeFrames,
	}
	upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{}}`)},
		{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()},
	}}
	firstDialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstreamA}}}
	firstBroker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: firstPlanner, Upstream: firstDialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstBroker.Serve(context.Background(), firstDownstream); err != nil {
		t.Fatal(err)
	}
	if got := firstDialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("dial accounts = %#v", got)
	}
	if got := firstDownstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{
		[]byte(`{"type":"response.created","response":{}}`),
		codexWSBrokerHard429(),
	}) {
		t.Fatalf("admitted failure downstream writes = %#v, want exact provider outcome", got)
	}
	select {
	case got := <-closeFrames:
		t.Fatalf("admitted failure requested unsafe replay with close %#v", got)
	default:
	}

	secondDownstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	secondDialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-b": {upstreamB}}}
	secondBroker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: &codexWSBrokerPlannerStub{
			runtime:  runtimeLease,
			accounts: []codex.AccountKey{"account-a", "account-b"},
			slots:    []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}},
		},
		Upstream: secondDialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondBroker.Serve(context.Background(), secondDownstream); err != nil {
		t.Fatal(err)
	}
	if got := secondDialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-b"}) {
		t.Fatalf("reset accounts = %#v, want B", got)
	}
}

func TestCodexTerminatingWSBrokerHidesIncrementalHard429AndRequiresFullCreate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexWSBrokerContinuationRuntime(t, coordinator, "turn-a", "response-a")
	incremental := codexTerminatingWSFrame("turn-a", `,"previous_response_id":"response-a"`)
	closeFrames := make(chan codexWSBrokerWrite, 1)
	firstDownstream := &codexWSBrokerConnStub{
		reads:         []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: incremental}},
		controlWrites: closeFrames,
	}
	upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
	firstBroker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: &codexWSBrokerPlannerStub{
			runtime:         runtimeLease,
			slots:           []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
			resetCandidates: []codex.AccountKey{"account-b"},
		},
		Upstream:    &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstreamA}}},
		UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstBroker.Serve(context.Background(), firstDownstream); err != nil {
		t.Fatalf("incremental hard limit = %T %v, want clean retryable close", err, err)
	}
	if got := firstDownstream.writtenPayloads(); len(got) != 0 {
		t.Fatalf("incremental hard limit leaked downstream: %#v", got)
	}
	select {
	case got := <-closeFrames:
		want := codexWSBrokerWrite{messageType: websocket.CloseMessage, payload: websocket.FormatCloseMessage(websocket.CloseServiceRestart, "account unavailable")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("incremental hard limit close = %#v, want %#v", got, want)
		}
	default:
		t.Fatal("incremental hard limit did not close downstream for retry")
	}
	if !upstreamA.closed {
		t.Fatal("exhausted incremental upstream was not closed")
	}

	fullCreate := codexTerminatingWSFrame("turn-a", "")
	secondDownstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: fullCreate}, {err: io.EOF}}}
	upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-b"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)},
	}}
	dialerB := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-b": {upstreamB}}}
	secondBroker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: &codexWSBrokerPlannerStub{
			runtime:  runtimeLease,
			accounts: []codex.AccountKey{"account-a", "account-b"},
			slots:    []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}},
		},
		Upstream: dialerB, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondBroker.Serve(context.Background(), secondDownstream); err != nil {
		t.Fatal(err)
	}
	if got := dialerB.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-b"}) {
		t.Fatalf("full-create accounts = %#v", got)
	}
	if got := upstreamB.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{fullCreate}) {
		t.Fatalf("full-create B writes = %#v", got)
	}
	if got := secondDownstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"response-b"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`),
	}) {
		t.Fatalf("full-create downstream writes = %#v", got)
	}
}

func TestCodexTerminatingWSBrokerSurfacesNonPortableHard429WhenNoResetAccountRemains(t *testing.T) {
	for _, test := range []struct {
		name      string
		handshake bool
	}{
		{name: "application frame"},
		{name: "handshake", handshake: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexWSBrokerContinuationRuntime(t, coordinator, "turn-a", "response-a")
			incremental := codexTerminatingWSFrame("turn-a", `,"previous_response_id":"response-a"`)
			closeFrames := make(chan codexWSBrokerWrite, 1)
			downstream := &codexWSBrokerConnStub{
				reads:         []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: incremental}},
				controlWrites: closeFrames,
			}
			dialer := &codexWSBrokerDialerStub{}
			if test.handshake {
				dialer.outcomes = map[codex.AccountKey][]codexWSBrokerDialOutcome{
					"account-a": {{
						response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)},
						body:     []byte(`{"error":{"type":"usage_limit_reached"}}`),
						err:      errors.New("rejected"),
					}},
				}
			} else {
				dialer.connections = map[codex.AccountKey][]websocketRelayConn{
					"account-a": {&codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}},
				}
			}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
				Plans: &codexWSBrokerPlannerStub{
					runtime: runtimeLease,
					slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
				},
				Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Serve(context.Background(), downstream); err != nil {
				t.Fatal(err)
			}
			want := codexWSBrokerHard429()
			if test.handshake {
				want = []byte(`{"error":{"type":"usage_limit_reached"},"status":429,"type":"error"}`)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{want}) {
				t.Fatalf("all-depleted downstream writes = %#v, want exact final hard limit", got)
			}
			select {
			case got := <-closeFrames:
				if reflect.DeepEqual(got.payload, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "account unavailable")) {
					t.Fatalf("all-depleted request entered retry loop with close %#v", got)
				}
			default:
			}
		})
	}
}

func TestCodexTerminatingWSBrokerReusesAccountBoundUpstreamForSuccessor(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerRevisionRotatingPlanner{inner: &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}, revisions: []codex.Revision{"revision-before-tool", "revision-after-tool"}}
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

type codexWSBrokerRevisionRotatingPlanner struct {
	inner     CodexNativeHTTPRequestPlanner
	revisions []codex.Revision
	calls     int
}

func (planner *codexWSBrokerRevisionRotatingPlanner) Build(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	prepared, err := planner.inner.Build(ctx, input)
	if err != nil {
		return CodexPreparedHTTPRequest{}, err
	}
	if len(planner.revisions) == 0 {
		return prepared, nil
	}
	revision := planner.revisions[min(planner.calls, len(planner.revisions)-1)]
	planner.calls++
	for accountIndex := range prepared.Dispatch.accounts {
		for attemptIndex := range prepared.Dispatch.accounts[accountIndex].attempts {
			prepared.Dispatch.accounts[accountIndex].attempts[attemptIndex].Revision = revision
		}
	}
	return prepared, nil
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

func TestCodexTerminatingWSBrokerReconnectsKnownClosedIdleUpstreamBeforeDispatch(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	predecessorPlan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	predecessor, err := runtimeLease.BeginRequest(predecessorPlan)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-a", HasResponseAnchor: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if predecessor, err = predecessor.Drain(); err != nil {
		t.Fatal(err)
	}
	planner := &codexWSBrokerPlannerStub{
		runtime: runtimeLease,
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	dialer := &codexWSBrokerDialerStub{}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                planner,
		Upstream:             dialer,
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

	closeFrames := make(chan codexWSBrokerWrite, 1)
	downstream := &codexWSBrokerConnStub{controlWrites: closeFrames}
	if err := broker.serveFrame(context.Background(), downstream, pending, &active); err != nil {
		t.Fatal(err)
	}
	if planner.buildCalls != 0 {
		t.Fatalf("planner builds = %d, want full-create resynchronisation before planning", planner.buildCalls)
	}
	if writes := upstream.writtenPayloads(); len(writes) != 0 {
		t.Fatalf("known-closed upstream writes = %#v", writes)
	}
	if !upstream.closed {
		t.Fatal("known-closed upstream was not retired")
	}
	if got := dialer.accounts; len(got) != 0 {
		t.Fatalf("reconnect accounts = %#v, want none before full create", got)
	}
	select {
	case got := <-closeFrames:
		want := codexWSBrokerWrite{messageType: websocket.CloseMessage, payload: websocket.FormatCloseMessage(websocket.CloseServiceRestart, "account unavailable")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resynchronisation close = %#v, want %#v", got, want)
		}
	default:
		t.Fatal("known-closed anchored successor did not request full-create resynchronisation")
	}
}

func TestCodexTerminatingWSBrokerPreservesHandledHandshakeFailureWithoutLoggingBody(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
	if traceErr != nil {
		t.Fatal(traceErr)
	}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}, {err: io.EOF}}}
	dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{
		"account-a": {{
			response: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)},
			body:     []byte(`{"error":{"type":"authentication_error","message":"re-authenticate before retry","code":"token_expired"}}`),
			err:      errors.New("private dial error"),
		}},
	}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	ctx, routeDiagnostics := withRouteDiagnostics(context.Background())
	ctx = withCodexTrace(ctx, diagnostics, nil, CodexTraceStart{Transport: "websocket"})
	stderr := captureStderr(t, func() {
		if err := broker.Serve(ctx, downstream); err != nil {
			t.Fatal(err)
		}
	})
	writes := downstream.writtenPayloads()
	const wantFrame = `{"error":{"type":"authentication_error","message":"re-authenticate before retry","code":"token_expired"},"status":401,"type":"error"}`
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
	for _, private := range []string{"re-authenticate before retry", "token_expired", "private dial error"} {
		if strings.Contains(stderr, private) {
			t.Fatalf("provider handshake detail escaped into trace: trace=%q frame=%q", stderr, writes[0])
		}
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	traceEvents := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "upstream_dial" && event.Outcome == "error" && event.UpstreamStatus == http.StatusUnauthorized && event.ErrorClass == "auth_rejected"
	}, "WebSocket 401 handshake")
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
		frame, status := canonicalCodexWSHandshakeError(&http.Response{StatusCode: test.status}, test.wrapped, nil)
		if status != test.status || string(frame) != fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":"%s"}}`, test.status, test.kind) {
			t.Fatalf("status %d projection = %d/%s", test.status, status, frame)
		}
		if strings.Contains(string(frame), "private") {
			t.Fatalf("status %d projection leaked private detail: %s", test.status, frame)
		}
	}
}

func TestCodexWSDialErrorClassifiesBoundedRepresentationsAndAuthAuthority(t *testing.T) {
	hardLimit := []byte(`{"error":{"type":"usage_limit_reached","message":"quota exhausted"}}`)
	authFailure := []byte(`{"error":{"type":"authentication_error"}}`)
	policyFailure := []byte(`{"error":{"type":"permission_denied","code":"safety_policy"}}`)
	tests := []struct {
		name      string
		status    int
		encoding  string
		body      []byte
		wantBody  []byte
		wantAuth  bool
		wantHard  bool
		wantFound bool
		wantErr   bool
	}{
		{name: "gzip hard limit", status: http.StatusTooManyRequests, encoding: "gzip", body: gzipCodexAttemptBody(t, hardLimit), wantBody: hardLimit, wantHard: true, wantFound: true},
		{name: "zstd hard limit", status: http.StatusTooManyRequests, encoding: "zstd", body: zstdCodexAttemptBody(t, hardLimit), wantBody: hardLimit, wantHard: true, wantFound: true},
		{name: "authentication forbidden", status: http.StatusForbidden, body: authFailure, wantBody: authFailure, wantAuth: true, wantFound: true},
		{name: "policy forbidden", status: http.StatusForbidden, body: policyFailure, wantBody: policyFailure, wantFound: true},
		{name: "unauthorized malformed body", status: http.StatusUnauthorized, body: []byte(`{`), wantAuth: true, wantFound: true},
		{name: "malformed gzip hard limit", status: http.StatusTooManyRequests, encoding: "gzip", body: []byte("not gzip")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			if test.encoding != "" {
				header.Set("Content-Encoding", test.encoding)
			}
			wrapped, providerBody, err := codexWSDialError(&http.Response{StatusCode: test.status, Header: header}, test.body)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, want error=%t", err, test.wantErr)
			}
			if wrapped.Found != test.wantFound || wrapped.AuthFailure != test.wantAuth || wrapped.HardUsageLimit != test.wantHard {
				t.Fatalf("wrapped = %#v", wrapped)
			}
			if !bytes.Equal(providerBody, test.wantBody) {
				t.Fatalf("provider body = %q, want %q", providerBody, test.wantBody)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerCancellationUnblocksUpstreamRead(t *testing.T) {
	t.Parallel()
	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
	if traceErr != nil {
		t.Fatal(traceErr)
	}
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
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := withCodexTrace(baseCtx, diagnostics, nil, CodexTraceStart{Transport: "websocket"})
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
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	traceEvents := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "upstream_read" && event.Outcome == "error" && event.Reason == "context_canceled"
	}, "WebSocket cancelled read")
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

func TestCodexTerminatingWSBrokerBackpressuresBurstApplicationFrames(t *testing.T) {
	t.Parallel()

	planner := &codexWSBrokerContextBlockingPlanner{started: make(chan struct{})}
	dialer := &codexWSBrokerDialerStub{}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	t.Cleanup(cancelServe)
	serveDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downstream, upgradeErr := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if upgradeErr != nil {
			serveDone <- upgradeErr
			return
		}
		defer downstream.Close()
		serveDone <- broker.Serve(serveCtx, downstream)
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
		t.Fatalf("burst application frames stopped broker while planning: %T %v", serveErr, serveErr)
	case <-time.After(100 * time.Millisecond):
	}
	cancelServe()
	select {
	case serveErr := <-serveDone:
		if !errors.Is(serveErr, context.Canceled) {
			t.Fatalf("cancelled burst broker = %T %v, want context cancellation", serveErr, serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled burst broker did not stop")
	}
	if len(dialer.accounts) != 0 {
		t.Fatalf("backpressured frames dispatched upstream accounts %#v", dialer.accounts)
	}
}

func TestCodexWSDownstreamReaderSerializesBurstFrames(t *testing.T) {
	t.Parallel()
	first := []byte("private-first-frame")
	second := []byte("private-second-frame")
	third := []byte("private-third-frame")
	conn := newCodexWSBrokerBurstConn(first, second, third)
	ctx, cancel := context.WithCancel(context.Background())
	reader := startCodexWSDownstreamReader(ctx, cancel, conn)
	t.Cleanup(reader.close)

	conn.waitForRead(t, 1)
	conn.waitForRead(t, 2)
	conn.assertReadBlocked(t, 3)
	messageType, payload, err := reader.read(context.Background(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || !bytes.Equal(payload, first) {
		t.Fatalf("first burst frame = (%d, %q), want text %q", messageType, payload, first)
	}
	conn.waitForRead(t, 3)
	for index, want := range [][]byte{second, third} {
		messageType, payload, err = reader.read(context.Background(), ctx)
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.TextMessage || !bytes.Equal(payload, want) {
			t.Fatalf("burst frame %d = (%d, %q), want text %q", index+2, messageType, payload, want)
		}
	}
}

func TestCodexWSDownstreamReaderClearsQueuedAndBlockedPayloadsOnCancel(t *testing.T) {
	t.Parallel()
	queued := []byte("private-queued-frame")
	blocked := []byte("private-blocked-frame")
	conn := newCodexWSBrokerBurstConn(queued, blocked)
	ctx, cancel := context.WithCancel(context.Background())
	reader := startCodexWSDownstreamReader(ctx, cancel, conn)
	conn.waitForRead(t, 1)
	conn.waitForRead(t, 2)
	reader.close()
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("cancelled downstream reader did not stop")
	}
	if !bytes.Equal(queued, make([]byte, len(queued))) {
		t.Fatalf("queued payload retained private bytes: %q", queued)
	}
	if !bytes.Equal(blocked, make([]byte, len(blocked))) {
		t.Fatalf("blocked payload retained private bytes: %q", blocked)
	}
}

func TestCodexTerminatingWSBrokerClassifiesUpstreamCloseBeforeCompletion(t *testing.T) {
	t.Parallel()
	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, traceErr := OpenDiagnosticsWriter(tracePath)
	if traceErr != nil {
		t.Fatal(traceErr)
	}
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

	ctx := withCodexTrace(context.Background(), diagnostics, nil, CodexTraceStart{Transport: "websocket"})
	err = broker.Serve(ctx, downstream)
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
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	traceEvents := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, traceEvents, func(event CodexTraceEvent) bool {
		return event.Phase == "terminal" && event.Stage == "upstream_read" && event.Reason == "upstream_outcome_indeterminate"
	}, "WebSocket close before completion")
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
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots: []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a-1", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-a", CandidateID: "candidate-a-2", Kind: CodexAttemptSlotDirect},
		},
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

func newCodexWSBrokerContinuationRuntime(t *testing.T, coordinator *CodexContinuityCoordinator, turn, responseAnchor string) *CodexLeaseRuntime {
	t.Helper()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan(turn, []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: responseAnchor, HasResponseAnchor: true},
		EndTurn:                   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatal(err)
	}
	return runtimeLease
}

func codexWSBrokerHard429() []byte {
	return []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
}

type codexWSBrokerPlannerStub struct {
	runtime         *CodexLeaseRuntime
	receipt         *codexTurnReceiptHandle
	accounts        []codex.AccountKey
	slots           []CodexLeaseAttemptSlotPlan
	frozenAccounts  []CodexFrozenDispatchAccount
	resetCandidates []codex.AccountKey
	attempts        map[codex.AccountKey]int
	refreshOnly     map[codex.AccountKey]CandidateAttempt
	prewarmCalls    int
	adoptionCalls   int
	cancelCalls     int
	buildCalls      int
	prewarmHeaders  http.Header
	buildHeaders    http.Header
	adoptionFrozen  *CodexFrozenRequest
}

type codexWSBrokerContextBlockingPlanner struct {
	started     chan struct{}
	startedOnce sync.Once
}

type codexWSBrokerBurstConn struct {
	payloads [][]byte
	reads    chan int
	stop     chan struct{}
	stopOnce sync.Once
	index    int
}

func newCodexWSBrokerBurstConn(payloads ...[]byte) *codexWSBrokerBurstConn {
	return &codexWSBrokerBurstConn{
		payloads: payloads,
		reads:    make(chan int, len(payloads)),
		stop:     make(chan struct{}),
	}
}

func (conn *codexWSBrokerBurstConn) ReadMessage() (int, []byte, error) {
	if conn.index >= len(conn.payloads) {
		<-conn.stop
		return 0, nil, io.EOF
	}
	payload := conn.payloads[conn.index]
	conn.index++
	conn.reads <- conn.index
	return websocket.TextMessage, payload, nil
}

func (conn *codexWSBrokerBurstConn) waitForRead(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-conn.reads:
		if got != want {
			t.Fatalf("burst read = %d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("burst read %d did not start", want)
	}
}

func (conn *codexWSBrokerBurstConn) assertReadBlocked(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-conn.reads:
		t.Fatalf("burst read advanced to %d before queued frame %d was consumed", got, want-1)
	case <-time.After(50 * time.Millisecond):
	}
}

func (*codexWSBrokerBurstConn) WriteMessage(int, []byte) error            { return nil }
func (*codexWSBrokerBurstConn) WriteControl(int, []byte, time.Time) error { return nil }
func (conn *codexWSBrokerBurstConn) SetReadDeadline(time.Time) error {
	conn.stopOnce.Do(func() { close(conn.stop) })
	return nil
}
func (*codexWSBrokerBurstConn) SetWriteDeadline(time.Time) error { return nil }
func (*codexWSBrokerBurstConn) Close() error                     { return nil }

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
	if len(planner.accounts) != 0 {
		plan.Accounts = append([]codex.AccountKey(nil), planner.accounts...)
	}
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
		refreshAttempt, refreshOnly := planner.refreshOnly[slot.AccountKey]
		attemptCount := max(1, planner.attempts[slot.AccountKey])
		attempts := make([]CandidateAttempt, 0, attemptCount)
		if !refreshOnly {
			for ordinal := 1; ordinal <= attemptCount; ordinal++ {
				attempts = append(attempts, CandidateAttempt{AccountKey: slot.AccountKey, Candidate: codex.CandidateRef{AccountKey: slot.AccountKey, CandidateID: codex.CandidateID(slot.CandidateID)}, Revision: codex.Revision("revision-" + string(rune('0'+ordinal))), Ordinal: ordinal})
			}
		}
		account := CodexFrozenDispatchAccount{
			choice:   RouteChoice{AccountKey: slot.AccountKey, RequestedModel: request.Model, EffectiveModel: request.Model, RequiredBuckets: []CapacityBucket{CapacityBucketBase}},
			attempts: attempts,
		}
		if refreshOnly {
			account.refreshAttempt = &refreshAttempt
		}
		accounts = append(accounts, account)
	}
	if planner.frozenAccounts != nil {
		accounts = cloneCodexFrozenDispatchAccounts(planner.frozenAccounts)
	}
	return CodexPreparedHTTPRequest{
		Dispatch: CodexFrozenDispatchPlan{
			accounts: accounts, status: CodexRoutePlanReady,
			accountUnavailableResetCandidates: append([]codex.AccountKey(nil), planner.resetCandidates...),
		},
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
	mu                     sync.Mutex
	reads                  []codexWSBrokerRead
	readCount              int
	readGateAfter          int
	readGateReleaseAtWrite int
	readGate               chan struct{}
	writes                 []codexWSBrokerWrite
	writeErr               error
	writeCalls             int
	failWriteAt            int
	controlWrites          chan<- codexWSBrokerWrite
	closed                 bool
	roleKnown              bool
	downstream             bool
	downstreamReadBlocked  bool
	downstreamReadWake     chan struct{}
	readInterrupted        bool
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
	for conn.readGate != nil && conn.readCount >= conn.readGateAfter && !conn.closed && !conn.readInterrupted {
		gate := conn.readGate
		conn.mu.Unlock()
		<-gate
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
	conn.readCount++
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
	if conn.readGate != nil && conn.readGateReleaseAtWrite > 0 && conn.writeCalls >= conn.readGateReleaseAtWrite {
		close(conn.readGate)
		conn.readGate = nil
	}
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
	if messageType == websocket.CloseMessage {
		conn.releaseDownstreamReadLocked()
	}
	return nil
}

func (conn *codexWSBrokerConnStub) SetReadDeadline(time.Time) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.readInterrupted = true
	conn.releaseReadGateLocked()
	conn.releaseDownstreamReadLocked()
	return nil
}
func (conn *codexWSBrokerConnStub) SetWriteDeadline(time.Time) error { return nil }
func (conn *codexWSBrokerConnStub) Close() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.closed = true
	conn.releaseReadGateLocked()
	conn.releaseDownstreamReadLocked()
	return nil
}

func (conn *codexWSBrokerConnStub) releaseReadGateLocked() {
	if conn.readGate == nil {
		return
	}
	close(conn.readGate)
	conn.readGate = nil
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

func (conn *codexWSBrokerConnStub) isClosed() bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.closed
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
	attempts    []CandidateAttempt
}

type codexWSBrokerDialOutcome struct {
	conn         websocketRelayConn
	response     *http.Response
	body         []byte
	actual       CandidateAttempt
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

func (conn *codexWSBrokerSuccessorConn) Close() error {
	conn.gateOnce.Do(func() { close(conn.gate) })
	return conn.codexWSBrokerConnStub.Close()
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
	dialer.attempts = append(dialer.attempts, attempt)
	if dialer.onDial != nil {
		dialer.onDial()
	}
	if outcomes := dialer.outcomes[choice.AccountKey]; len(outcomes) != 0 {
		outcome := outcomes[0]
		dialer.outcomes[choice.AccountKey] = outcomes[1:]
		actual := outcome.actual
		if actual.AccountKey == "" {
			actual = attempt
		}
		if onDispatch != nil && !outcome.skipDispatch {
			onDispatch(actual)
		}
		return outcome.conn, outcome.response, append([]byte(nil), outcome.body...), actual, outcome.err
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
