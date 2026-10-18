package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTerminatingWSBrokerReplansAuthenticatedPortableQuotaRejection(t *testing.T) {
	for _, handshake := range []bool{false, true} {
		name := "application"
		if handshake {
			name = "handshake"
		}
		t.Run(name, func(t *testing.T) {
			coordinator, seed, factory, ctx := codexWSQuotaReplanFixture(t)
			frame := codexTerminatingWSFrame("turn-a", "")
			created := []byte(`{"type":"response.created","response":{"id":"response-b"}}`)
			completed := []byte(`{"type":"response.completed","response":{"id":"response-b","end_turn":true}}`)
			upstreamA := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
			upstreamB := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: created}, {messageType: websocket.TextMessage, payload: completed}}}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstreamA}, "account-b": {upstreamB}}}
			if handshake {
				dialer.outcomes = map[codex.AccountKey][]codexWSBrokerDialOutcome{"account-a": {{response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}, body: codexWSBrokerHard429(), err: errors.New("quota rejected")}}}
			}
			downstream := codexWSQuotaReplanServe(t, ctx, factory, dialer, frame)
			if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b"}) {
				t.Fatalf("dial accounts = %v, want bound account then replacement", got)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{created, completed}) {
				t.Fatalf("downstream writes = %q, want only replacement response", got)
			}
			downstream.mu.Lock()
			controls := downstream.controlWrites
			downstream.mu.Unlock()
			if len(controls) != 0 {
				t.Fatal("transparent quota failover sent downstream control frame")
			}
			if got := upstreamB.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{frame}) {
				t.Fatalf("replacement request = %q, want exact original frame", got)
			}
			if !handshake {
				if got := upstreamA.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{frame}) {
					t.Fatalf("bound request = %q", got)
				}
			}
			snapshot, err := coordinator.LoadRouteSnapshot(ctx, seed.Key, []codex.AccountKey{"account-a", "account-b"}, seed.Authority)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.BoundAccountKey != "account-b" || !snapshot.BoundRequestCompleted {
				t.Fatalf("replacement durable binding = %#v", snapshot)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerBoundsAuthenticatedPortableQuotaReplans(t *testing.T) {
	_, _, factory, ctx := codexWSQuotaReplanFixture(t)
	inventory := factory.Inventory.(*codexHTTPRequestPlanTestInventory)
	inventory.inventory.Accounts = append(inventory.inventory.Accounts, frozenDispatchTestLogicalAccount("account-c", frozenDispatchCandidate("account-c", "candidate-c", "revision-c", codex.SourceExternal, false, factory.Now().Add(time.Hour))))
	frame := codexTerminatingWSFrame("turn-a", "")
	rejected := func() *codexWSBrokerConnStub {
		return &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
	}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {rejected()}, "account-b": {rejected()}, "account-c": {rejected()}}}
	downstream := codexWSQuotaReplanServe(t, ctx, factory, dialer, frame)
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a", "account-b", "account-c"}) {
		t.Fatalf("exhausted pool attempts = %v, want each account once", got)
	}
	if got := downstream.writtenPayloads(); len(got) != 1 || !bytes.Contains(got[0], []byte("usage_limit_reached")) {
		t.Fatalf("exhausted pool replies = %q, want one final quota error", got)
	}
}

func TestCodexTerminatingWSBrokerDoesNotReplanAuthenticatedQuotaAfterAdmission(t *testing.T) {
	_, _, factory, ctx := codexWSQuotaReplanFixture(t)
	frame := codexTerminatingWSFrame("turn-a", "")
	created := []byte(`{"type":"response.created","response":{"id":"response-a-current"}}`)
	upstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: created}, {messageType: websocket.TextMessage, payload: codexWSBrokerHard429()}}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}}
	downstream := codexWSQuotaReplanServe(t, ctx, factory, dialer, frame)
	if got := dialer.accounts; !reflect.DeepEqual(got, []codex.AccountKey{"account-a"}) {
		t.Fatalf("admitted request replayed: %v", got)
	}
	if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{created, codexWSBrokerHard429()}) {
		t.Fatalf("admitted response writes = %q", got)
	}
}

func codexWSQuotaReplanFixture(t *testing.T) (*CodexContinuityCoordinator, CodexLeaseRequestPlan, *CodexHTTPRequestPlanFactory, context.Context) {
	t.Helper()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	seed := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	seed.Key.Lane.Session = "session-a"
	seed.Key.Lane.Thread = "thread-a"
	seed.RequestedModel = "gpt-5.6-sol"
	seed.EffectiveModel = "gpt-5.6-sol"
	handle, err := runtimeLease.BeginRequest(seed)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{DownstreamGeneration: 1, UpstreamGeneration: 1, HasTurnState: true, TurnState: "state-a"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatal(err)
	}
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceExternal, false, now.Add(time.Hour))),
	}}
	factory := &CodexHTTPRequestPlanFactory{Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory}, Routes: coordinator, Runtime: runtimeLease, DefaultAccountKey: "account-a", Authority: seed.Authority, Now: func() time.Time { return *now }, TransportKind: "websocket"}
	ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerCodex})
	ctx = withRuntimeCallerIdentity(ctx, "account-a\x00candidate-a\x00revision-a")
	return coordinator, seed, factory, ctx
}

func codexWSQuotaReplanServe(t *testing.T, ctx context.Context, factory *CodexHTTPRequestPlanFactory, dialer *codexWSBrokerDialerStub, frame []byte) *codexWSBrokerConnStub {
	t.Helper()
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: frame}, {err: io.EOF}}, controlWrites: make(chan codexWSBrokerWrite, 16)}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: factory, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatal(err)
	}
	return downstream
}
