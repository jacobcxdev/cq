package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexPrewarmCyberCompactionResynchronisesBeforeAdoption(t *testing.T) {
	for _, capabilityOnly := range []bool{false, true} {
		name := "outside Cyber pool"
		if capabilityOnly {
			name = "Cyber capability removed"
		}
		t.Run(name, func(t *testing.T) {
			factory, coordinator, ctx := codexPrewarmCompactionFactory(t)
			if capabilityOnly {
				policy := RoutingPolicyV2{SchemaVersion: 2, RoutingGeneration: 7,
					Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Members: []codex.AccountKey{"ordinary", "cyber"}}}}
				factory.SessionPolicy = NewSessionPolicyResolver(make([]byte, 32), policy)
				factory.CyberEligibility = NewCyberEligibilityStore(&RoutingPolicyStore{current: &policy})
				factory.CyberEligibility.MarkIneligible("ordinary", "gpt-6.1-sol", "daybreak_blue")
			}
			metadata := CodexTurnMetadata{SessionID: "session-a", ThreadID: "thread-a", RequestKind: CodexRequestPrewarm}
			reservation, err := coordinator.prewarms.Create(metadata, "prewarm-correlation")
			if err != nil {
				t.Fatal(err)
			}
			reservation, err = coordinator.prewarms.BindSockets(reservation.Lane, "ordinary", 41, 43)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err = coordinator.prewarms.Ready(reservation.Lane, "prewarm-response", "prewarm-state")
			if err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), coordinator.store.journalBytes...)
			_, adoptionErr := factory.adoptWebSocketPrewarm(ctx, CodexHTTPRequestPlanInput{Encoded: codexPrewarmCompactionBody("compaction", "pre_turn", "prewarm-response", true)}, reservation,
				func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error { return nil })
			want := ErrSessionPolicyContinuity
			if capabilityOnly {
				want = ErrCapabilityRouteUnavailable
			}
			var planErr *CodexHTTPRequestPlanError
			if !errors.As(adoptionErr, &planErr) || planErr.Reason != codexRequestFailureReason(want) {
				t.Fatalf("incompatible adoption = %v, want %v", adoptionErr, want)
			}

			compaction := codexPrewarmCompactionBody("compaction", "pre_turn", "prewarm-response", true)
			pending, err := newCodexWSPendingFrameOwned(websocket.TextMessage, compaction)
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Release()
			controls := make(chan codexWSBrokerWrite, 1)
			downstream := &codexWSBrokerConnStub{controlWrites: controls}
			upstream := &codexWSBrokerConnStub{}
			active := &codexWSActiveUpstream{conn: upstream, account: "ordinary", generation: 43, prewarm: reservation}
			broker := &codexTerminatingWSBroker{config: codexTerminatingWSBrokerConfig{Plans: factory, DownstreamGeneration: 41}}
			if err := broker.serveFrame(ctx, downstream, pending, active); err != nil {
				t.Fatal(err)
			}
			if !upstream.isClosed() || len(upstream.writtenPayloads()) != 0 || active.account != "" {
				t.Fatal("incompatible prewarm retained or dispatched compaction")
			}
			if !bytes.Equal(before, coordinator.store.journalBytes) {
				t.Fatal("rejected adoption changed durable lease")
			}
			select {
			case close := <-controls:
				if close.messageType != websocket.CloseMessage || !bytes.Equal(close.payload, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "account unavailable")) {
					t.Fatalf("resynchronisation = %+v", close)
				}
			default:
				t.Fatal("missing full-history resynchronisation")
			}
			if _, err := coordinator.prewarms.Create(metadata, "replacement"); err != nil {
				t.Fatalf("reservation retained: %v", err)
			}
			// The client supplies full history after the close; CQ never drops an
			// arbitrary response anchor to manufacture a portable request.
			full := codexPrewarmCompactionBody("compaction", "pre_turn", "", true)
			prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: full})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Frozen.Release()
			if prepared.leaseHandle.AccountKey() != "cyber" {
				t.Fatal("full-history recovery missed Cyber account")
			}
		})
	}
}

func TestCodexPrewarmCompactionCyberTurn(t *testing.T) {
	for _, withState := range []bool{false, true} {
		name := "without turn state"
		if withState {
			name = "with turn state"
		}
		t.Run(name, func(t *testing.T) {
			factory, coordinator, ctx := codexPrewarmCompactionFactory(t)
			prewarm := codexPrewarmCompactionBody("prewarm", "", "", false)
			dispatch, err := factory.planWebSocketPrewarm(ctx, CodexHTTPRequestPlanInput{Encoded: prewarm})
			if err != nil || len(dispatch.Accounts()) == 0 || dispatch.Accounts()[0].Choice().AccountKey != "ordinary" {
				t.Fatalf("ordinary prewarm = %+v, %v", dispatch.Accounts(), err)
			}
			reservation, err := factory.beginWebSocketPrewarm(CodexHTTPRequestPlanInput{Encoded: prewarm})
			if err != nil {
				t.Fatal(err)
			}
			reservation, err = factory.bindWebSocketPrewarm(reservation, "ordinary", 41, 43)
			if err != nil {
				t.Fatal(err)
			}
			state := ""
			if withState {
				state = "prewarm-state"
			}
			reservation, err = factory.readyWebSocketPrewarm(reservation, "prewarm-response", state)
			if err != nil {
				t.Fatal(err)
			}
			compaction := codexPrewarmCompactionBody("compaction", "pre_turn", "prewarm-response", false)
			prepared, err := factory.adoptWebSocketPrewarm(ctx, CodexHTTPRequestPlanInput{Encoded: compaction}, reservation,
				func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Frozen.Release()
			handle, err := prepared.leaseHandle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.AdmitWebSocketContext(ctx, CodexWebSocketAdmissionEvidence{
				DownstreamGeneration: 41, UpstreamGeneration: 43, ResponseCreated: true, ResponseID: "compaction-response",
				TurnState: state, HasTurnState: withState,
			})
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{
				CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "compaction-response", HasResponseAnchor: true, HasEncryptedState: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			// A terminal event is insufficient while its observer still owns the lane.
			undrained, err := coordinator.LoadRouteSnapshot(ctx, NewCodexLeaseKey(CodexTurnMetadata{SessionID: "session-a", ThreadID: "thread-a", TurnID: "turn-a"}), []codex.AccountKey{"ordinary", "cyber"}, factory.Authority)
			if err != nil || undrained.BoundPreTurnCompleted {
				t.Fatalf("undrained compaction released account authority: %+v, %v", undrained, err)
			}
			handle, err = handle.Drain()
			if err != nil {
				t.Fatal(err)
			}
			admission := handle.record.AdmissionJournalGeneration
			for _, input := range []CodexHTTPRequestPlanInput{
				{Encoded: codexPrewarmCompactionBody("turn", "", "compaction-response", true)},
				{Encoded: codexPrewarmCompactionBody("turn", "", "", true), Headers: http.Header{"X-Codex-Turn-State": {"prewarm-state"}}},
			} {
				before := append([]byte(nil), coordinator.store.journalBytes...)
				if _, err := factory.Build(ctx, input); err == nil {
					t.Fatal("hard continuation crossed Cyber policy boundary")
				}
				if !bytes.Equal(before, coordinator.store.journalBytes) {
					t.Fatal("rejected hard continuation changed lease")
				}
			}
			// Codex sends a full create after pre-turn compaction, with the same
			// turn ID and a portable encrypted compaction item. Cyber first appears here.
			turn := codexPrewarmCompactionBody("turn", "", "", true)
			retry, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: turn})
			if err != nil {
				t.Fatalf("Cyber turn after completed pre-turn compaction: %v", err)
			}
			defer retry.Frozen.Release()
			if retry.leaseHandle.AccountKey() != "cyber" || retry.leaseHandle.record.AdoptedPrewarm {
				t.Fatalf("Cyber turn retained ordinary prewarm: %+v", retry.leaseHandle.record)
			}
			if retry.leaseHandle.record.AdmissionJournalGeneration != admission {
				t.Fatal("planning replaced prior admission evidence")
			}
			next, err := retry.leaseHandle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			next, err = next.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			if next.record.AccountHash != coordinator.store.hash("account", "cyber") || next.record.HasTurnState || next.record.RequestKind != CodexRequestTurn || next.record.AdmissionJournalGeneration != admission {
				t.Fatalf("Cyber admission retained compaction authority: %+v", next.record)
			}
			next, err = next.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = next.Drain(); err != nil {
				t.Fatal(err)
			}
			// Once a real turn has been admitted, the compaction exception is spent.
			continued, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: codexPrewarmCompactionBody("turn", "", "", false)})
			if err != nil {
				t.Fatal(err)
			}
			defer continued.Frozen.Release()
			if continued.leaseHandle.AccountKey() != "cyber" {
				t.Fatal("real turn lost its account binding")
			}
			if _, err := continued.leaseHandle.AbandonBeforeDispatch(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexTerminatingWSBrokerPrewarmCompactionCyberTurn(t *testing.T) {
	factory, coordinator, ctx := codexPrewarmCompactionFactory(t)
	factory.TransportKind = "websocket"
	ctx = withRuntimeCallerAuthority(ctx, RuntimeCallerAuthorityV1{Domain: NormalCallerCodex, SubjectID: "codex", ConsumptionDigest: strings.Repeat("b", 64)})
	ctx = withRuntimeCallerIdentity(ctx, "ordinary\x00ordinary-candidate\x00ordinary-revision")
	prewarm := codexPrewarmCompactionBody("prewarm", "", "", false)
	compaction := codexPrewarmCompactionBody("compaction", "pre_turn", "prewarm-response", false)
	turn := codexPrewarmCompactionBody("turn", "", "", true)
	ordinary := newCodexWSBrokerSuccessorConn([]codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"prewarm-response"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"prewarm-response"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"compaction-response"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"compaction-response","output":[{"type":"compaction","encrypted_content":"portable-compaction"}]}}`)},
	})
	cyber := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"cyber-response"}}`)},
		{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"cyber-response","end_turn":true}}`)},
	}}
	dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"ordinary": {ordinary}, "cyber": {cyber}}}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: factory, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
		CyberEligibility: factory.CyberEligibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	active := &codexWSActiveUpstream{}
	defer closeCodexWSActiveUpstream(active)
	controls := make(chan codexWSBrokerWrite, 4)
	downstream := &codexWSBrokerConnStub{controlWrites: controls}
	for _, body := range [][]byte{prewarm, compaction, turn} {
		pending, err := newCodexWSPendingFrameOwned(websocket.TextMessage, bytes.Clone(body))
		if err != nil {
			t.Fatal(err)
		}
		err = broker.serveFrame(ctx, downstream, pending, active)
		pending.Release()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(dialer.accounts, []codex.AccountKey{"ordinary", "cyber"}) {
		t.Fatalf("dial accounts = %v", dialer.accounts)
	}
	if got := ordinary.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{prewarm, compaction}) {
		t.Fatalf("ordinary upstream writes = %q", got)
	}
	if got := cyber.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{turn}) {
		t.Fatalf("Cyber upstream writes = %q", got)
	}
	if len(downstream.writtenPayloads()) != 6 || len(controls) != 0 {
		t.Fatal("sequence disconnected before response.completed")
	}
	snapshot, err := coordinator.LoadRouteSnapshot(ctx, NewCodexLeaseKey(CodexTurnMetadata{SessionID: "session-a", ThreadID: "thread-a", TurnID: "turn-a"}), []codex.AccountKey{"ordinary", "cyber"}, factory.Authority)
	if err != nil || snapshot.BoundAccountKey != "cyber" || !snapshot.BoundRequestCompleted {
		t.Fatalf("completed Cyber turn = %+v, %v", snapshot, err)
	}
}

func TestCodexCyberTurnKeepsOtherAdmittedRequestsBound(t *testing.T) {
	for _, test := range []struct {
		name, kind, phase string
		failed            bool
	}{
		{name: "failed pre-turn compaction", kind: "compaction", phase: "pre_turn", failed: true},
		{name: "standalone compaction", kind: "compaction", phase: "standalone_turn"},
		{name: "real turn", kind: "turn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory, coordinator, ctx := codexPrewarmCompactionFactory(t)
			prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: codexPrewarmCompactionBody(test.kind, test.phase, "", false)})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Frozen.Release()
			handle, err := prepared.leaseHandle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			if test.failed {
				handle, err = handle.ProviderFailed(CodexHTTPResponseEvidence{})
			} else {
				handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = handle.Drain(); err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), coordinator.store.journalBytes...)
			_, err = factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: codexPrewarmCompactionBody("turn", "", "", true)})
			var planErr *CodexHTTPRequestPlanError
			if !errors.As(err, &planErr) || planErr.Reason != CodexRequestFailureSessionPolicyContinuity {
				t.Fatalf("admitted account moved: %v", err)
			}
			if !bytes.Equal(before, coordinator.store.journalBytes) {
				t.Fatal("rejected Cyber switch changed durable authority")
			}
		})
	}
}

func codexPrewarmCompactionFactory(t *testing.T) (*CodexHTTPRequestPlanFactory, *CodexContinuityCoordinator, context.Context) {
	t.Helper()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	policy := RoutingPolicyV2{
		SchemaVersion: 2, RoutingGeneration: 7,
		Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []codex.AccountKey{"cyber"}}},
	}
	eligibility := NewCyberEligibilityStore(&RoutingPolicyStore{current: &policy})
	eligibility.ReplaceAccount("ordinary", map[string]map[string]CyberAccessState{"gpt-6.1-sol": {"daybreak_blue": CyberAccessIneligible}})
	eligibility.ReplaceAccount("cyber", map[string]map[string]CyberAccessState{"gpt-6.1-sol": {"daybreak_blue": CyberAccessEligible}})
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("ordinary", frozenDispatchCandidate("ordinary", "ordinary-candidate", "ordinary-revision", codex.SourceSystem, false, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("cyber", frozenDispatchCandidate("cyber", "cyber-candidate", "cyber-revision", codex.SourceSystem, false, now.Add(time.Hour))),
		}}},
		Routes: coordinator, Runtime: newCodexLeaseRuntimeTest(t, coordinator), DefaultAccountKey: "ordinary",
		Authority:     CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		SessionPolicy: NewSessionPolicyResolver(make([]byte, 32), policy), CyberEligibility: eligibility,
		DispatchPermits: &sessionPolicyPermitRecorder{}, Now: func() time.Time { return *now },
	}
	ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{
		Domain: NormalCallerLocal, SubjectID: "local", ConsumptionDigest: strings.Repeat("a", 64),
	})
	return factory, coordinator, ctx
}

func codexPrewarmCompactionBody(kind, phase, previous string, cyber bool) []byte {
	body := `{"type":"response.create","model":"gpt-6.1-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-a","thread_id":"thread-a","turn_id":"turn-a","request_kind":"` + kind + `"`
	if phase != "" {
		body += `,"compaction":"` + phase + `"`
	}
	body += `}},"input":[{"type":"compaction","encrypted_content":"portable-compaction"}]`
	if kind == "prewarm" {
		body = strings.Replace(body, `"turn_id":"turn-a"`, `"turn_id":""`, 1)
		body += `,"generate":false`
	}
	if previous != "" {
		body += `,"previous_response_id":"` + previous + `"`
	}
	if cyber {
		body += `,"access_programs":{"cyber":"daybreak_blue"}`
	}
	return []byte(body + `}`)
}
