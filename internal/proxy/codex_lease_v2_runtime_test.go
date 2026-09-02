package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseRuntimePersistsPrivateHTTPEvidence(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	turnState := "private-turn-state"
	responseAnchor := "private-response-anchor"

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: turnState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	if !handle.record.HasTurnState || handle.record.TurnStateHash != store.hash("turn-state", turnState) {
		t.Fatalf("admission evidence = %#v", handle.record)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: responseAnchor, HasResponseAnchor: true, HasEncryptedState: true},
		EndTurn:                   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseContinuationPending || !handle.record.HasResponseAnchor || handle.record.CorrelationHash != store.hash("correlation", responseAnchor) || !handle.record.HasEncryptedState {
		t.Fatalf("completion evidence = %#v", handle.record)
	}
	if bytes.Contains(store.journalBytes, []byte(turnState)) || bytes.Contains(store.journalBytes, []byte(responseAnchor)) {
		t.Fatal("raw HTTP evidence entered the journal")
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}

	matching := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}})
	matching.Evidence = CodexLeaseRequestEvidence{PreviousResponseID: responseAnchor, TurnState: turnState, HasTurnState: true, HasEncryptedState: true}
	for _, test := range []struct {
		name    string
		mutate  func(*CodexLeaseRequestPlan)
		private string
	}{
		{name: "previous response mismatch", private: "wrong-response-anchor", mutate: func(plan *CodexLeaseRequestPlan) { plan.Evidence.PreviousResponseID = "wrong-response-anchor" }},
		{name: "turn state mismatch", private: "wrong-private-state", mutate: func(plan *CodexLeaseRequestPlan) { plan.Evidence.TurnState = "wrong-private-state" }},
		{name: "missing turn state", mutate: func(plan *CodexLeaseRequestPlan) { plan.Evidence.TurnState = ""; plan.Evidence.HasTurnState = false }},
		{name: "account mismatch", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
			plan.Slots[0].AccountKey = "account-b"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := matching
			invalid.Accounts = append([]codex.AccountKey(nil), matching.Accounts...)
			invalid.Slots = append([]CodexLeaseAttemptSlotPlan(nil), matching.Slots...)
			test.mutate(&invalid)
			before := append([]byte(nil), store.journalBytes...)
			_, beginErr := runtimeLease.BeginRequest(invalid)
			if !errors.Is(beginErr, ErrCodexContinuity) {
				t.Fatalf("BeginRequest = %T %v, want continuity error", beginErr, beginErr)
			}
			if !bytes.Equal(before, store.journalBytes) {
				t.Fatal("rejected continuity evidence changed journal")
			}
			if test.private != "" && strings.Contains(beginErr.Error(), test.private) {
				t.Fatal("continuity error exposed raw evidence")
			}
		})
	}

	authenticatedRetry := matching
	authenticatedRetry.Evidence.TurnState = ""
	authenticatedRetry.Evidence.HasTurnState = false
	authenticatedRetry.RequiresAccountContinuity = true
	authenticatedRetry.authenticatedCallerContinuity = true
	authenticatedRetry.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity: handle.identity, AccountKey: "account-a", RecordGeneration: handle.record.RecordGeneration,
	}
	resumed, err := runtimeLease.BeginRequest(authenticatedRetry)
	if err != nil {
		t.Fatalf("authenticated retry without turn state = %T %v", err, err)
	}
	if resumed.AccountKey() != "account-a" || resumed.identity != handle.identity {
		t.Fatalf("authenticated retry authority = account %q identity %#v, want account-a %#v", resumed.AccountKey(), resumed.identity, handle.identity)
	}
	if _, err := resumed.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
	mismatch := matching
	mismatch.Evidence.TurnState = "wrong-private-state"
	mismatch.RequiresAccountContinuity = true
	if _, err := runtimeLease.BeginRequest(mismatch); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("unauthenticated retry with mismatched turn state = %T %v, want continuity error", err, err)
	} else {
		var continuityErr *codexContinuityError
		if !errors.As(err, &continuityErr) || continuityErr.reason != codexContinuityTurnStateMismatch {
			t.Fatalf("authenticated retry mismatch reason = %T %v, want turn-state mismatch", err, err)
		}
	}

	next, err := runtimeLease.BeginRequest(matching)
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	nextState := "next-response-state"
	next, err = next.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: nextState, HasTurnState: true})
	if err != nil {
		t.Fatalf("later admission state = %T %v", err, err)
	}
	if !next.record.HasTurnState || next.record.TurnStateHash != store.hash("turn-state", turnState) {
		t.Fatalf("latched admission state = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeLatchesInitialTurnStateWhileStreaming(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	const initialState = "private-initial-turn-state"
	const streamedState = "private-streamed-turn-state"

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: initialState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, handle, 1, 1, 1, false)
	requestGeneration := handle.RequestGeneration()
	attemptGeneration := handle.AttemptGeneration()

	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: streamedState, HasTurnState: true})
	if err != nil {
		t.Fatalf("streamed turn-state admission = %T %v", err, err)
	}
	if handle.RequestGeneration() != requestGeneration || handle.AttemptGeneration() != attemptGeneration || codexLeaseCurrentAttemptState(handle.record) != CodexAttemptStreaming {
		t.Fatalf("streamed admission changed request identity: request %d attempt %d state %v", handle.RequestGeneration(), handle.AttemptGeneration(), codexLeaseCurrentAttemptState(handle.record))
	}
	if !handle.record.HasTurnState || handle.record.TurnStateHash != store.hash("turn-state", initialState) {
		t.Fatalf("latched turn state = %#v", handle.record)
	}
	assertCodexLeaseRuntimeRefs(t, handle, 1, 1, 1, false)
	if bytes.Contains(store.journalBytes, []byte(initialState)) || bytes.Contains(store.journalBytes, []byte(streamedState)) {
		t.Fatal("raw turn state entered journal")
	}

	completed, err := handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	beforeRejectedAdmission := append([]byte(nil), store.journalBytes...)
	if _, err := completed.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "private-late-state", HasTurnState: true}); !errors.Is(err, ErrCodexLeaseTransition) {
		t.Fatalf("terminal streamed admission = %T %v, want transition error", err, err)
	}
	if !bytes.Equal(beforeRejectedAdmission, store.journalBytes) {
		t.Fatal("rejected terminal admission changed journal")
	}
	completed, err = completed.Drain()
	if err != nil {
		t.Fatal(err)
	}

	nextPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect,
	}})
	nextPlan.Evidence = CodexLeaseRequestEvidence{TurnState: initialState, HasTurnState: true}
	next, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatalf("follow-up with initial turn state = %T %v", err, err)
	}
	if _, err := next.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLeaseRuntimeLatchesFirstTurnStateAcrossRequests(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	const firstState = "private-first-turn-state"
	const laterState = "private-later-turn-state"

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: firstState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.Drain()
	if err != nil {
		t.Fatal(err)
	}

	plan.Evidence = CodexLeaseRequestEvidence{TurnState: firstState, HasTurnState: true}
	second, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: laterState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := second.record.TurnStateHash, store.hash("turn-state", firstState); got != want {
		t.Fatalf("latched turn state hash = %q, want first state hash %q", got, want)
	}
	second, err = second.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.Drain()
	if err != nil {
		t.Fatal(err)
	}

	third, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatalf("same-turn follow-up with latched state = %T %v, want success", err, err)
	}
	if _, err := third.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLeaseRuntimeRelatchesAuthenticatedClientStateAfterLegacyRotation(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	const clientState = "private-client-turn-state"
	const rotatedState = "private-provider-rotated-state"

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: clientState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	legacy := cloneCodexLeaseV2Envelope(*store.v2)
	found := false
	for index := range legacy.Records {
		if legacy.Records[index].Identity() != first.identity {
			continue
		}
		legacy.Records[index].TurnStateHash = store.hash("turn-state", rotatedState)
		legacy.Records[index].TurnStateLatchCurrent = false
		found = true
		break
	}
	if !found {
		t.Fatalf("legacy record %#v not found", first.identity)
	}
	payload, err := store.marshalV2Envelope(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	restartedCoordinator := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	store = restartedCoordinator.Store()
	restarted := newCodexLeaseRuntimeTest(t, restartedCoordinator)
	plan.Evidence = CodexLeaseRequestEvidence{TurnState: clientState, HasTurnState: true}
	plan.RequiresAccountContinuity = true
	plan.authenticatedCallerContinuity = true
	next, err := restarted.BeginRequest(plan)
	if err != nil {
		t.Fatalf("authenticated continuation after legacy rotation = %T %v, want success", err, err)
	}
	if got, want := next.record.TurnStateHash, store.hash("turn-state", clientState); got != want {
		t.Fatalf("relatched turn state hash = %q, want client state hash %q", got, want)
	}
	next, err = next.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: rotatedState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.Drain(); err != nil {
		t.Fatal(err)
	}

	if err := restartedCoordinator.Close(); err != nil {
		t.Fatal(err)
	}
	restartedCoordinator = reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	restarted = newCodexLeaseRuntimeTest(t, restartedCoordinator)
	plan.Evidence.TurnState = "private-stale-client-state"
	if _, err := restarted.BeginRequest(plan); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("second authenticated turn-state mismatch = %T %v, want continuity error", err, err)
	}
}

func TestCodexLeaseRuntimeRebindsSocketGenerationForNewRequest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: 41,
		UpstreamGeneration:   51,
		TurnState:            "turn-state-a",
		HasTurnState:         true,
		ResponseID:           "response-a",
		ResponseCreated:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-a", HasResponseAnchor: true},
		EndTurn:                   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.Drain()
	if err != nil {
		t.Fatal(err)
	}

	nextPlan := plan
	nextPlan.Evidence = CodexLeaseRequestEvidence{
		PreviousResponseID: "response-a",
		TurnState:          "turn-state-a",
		HasTurnState:       true,
	}
	next, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: 42,
		UpstreamGeneration:   1,
		ResponseID:           "response-b",
		ResponseCreated:      true,
	})
	if err != nil {
		t.Fatalf("new request socket admission: %v", err)
	}
	if next.record.DownstreamSocketGeneration != 42 || next.record.UpstreamSocketGeneration != 1 {
		t.Fatalf("new request socket generations = %d/%d, want 42/1", next.record.DownstreamSocketGeneration, next.record.UpstreamSocketGeneration)
	}
}

func TestCodexLeaseRuntimeAdoptsAuthenticatedCallerAfterShadowTurn(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)

	shadowPlan := codexLeaseRuntimeTestPlan("shadow-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "shadow-candidate", Kind: CodexAttemptSlotDirect,
	}})
	shadowPlan.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 8}
	shadowPlan.RequiresAccountContinuity = true
	shadow, err := runtimeLease.BeginRequest(shadowPlan)
	if err != nil {
		t.Fatal(err)
	}
	shadow, err = shadow.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	shadow, err = shadow.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	shadow, err = shadow.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{
			ResponseAnchor: "shadow-response", HasResponseAnchor: true, HasEncryptedState: true,
		},
		EndTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	shadow, err = shadow.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if !shadow.EverAdmitted() || shadow.record.Authoritative || !shadow.record.NonMigratable {
		t.Fatalf("shadow predecessor = %#v", shadow.record)
	}
	for _, lane := range store.v2.Lanes {
		if lane.SessionHash == shadow.record.SessionHash && lane.ThreadHash == shadow.record.ThreadHash && lane.LastAdmittedAccountHash != "" {
			t.Fatalf("shadow predecessor unexpectedly installed admitted affinity: %#v", lane)
		}
	}

	normalPlan := codexLeaseRuntimeTestPlan("normal-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-b", CandidateID: "caller-candidate", Kind: CodexAttemptSlotDirect,
	}})
	normalPlan.Evidence = CodexLeaseRequestEvidence{PreviousResponseID: "rescue-response", HasEncryptedState: true}
	normalPlan.RequiresAccountContinuity = true
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(normalPlan); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("unproved shadow adoption = %T %v, want continuity error", err, err)
	} else {
		var continuityErr *codexContinuityError
		if !errors.As(err, &continuityErr) || continuityErr.reason != codexContinuityPreviousResponseMismatch {
			t.Fatalf("unproved shadow adoption reason = %T %v, want previous response mismatch", err, err)
		}
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("rejected shadow adoption changed journal")
	}

	normalPlan.authenticatedCallerContinuity = true
	adopted, err := runtimeLease.BeginRequest(normalPlan)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.newTurn || adopted.AccountKey() != "account-b" || !adopted.record.Authoritative || !adopted.record.NonMigratable || !adopted.record.HasEncryptedState {
		t.Fatalf("adopted caller continuation = %#v", adopted.record)
	}
	if _, err := adopted.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLeaseRuntimeDoesNotAdoptAuthenticatedCallerOverAuthoritativeTurn(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)

	predecessorPlan := codexLeaseRuntimeTestPlan("authoritative-turn", []CodexLeaseAttemptSlotPlan{{
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
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{
			ResponseAnchor: "authoritative-response", HasResponseAnchor: true, HasEncryptedState: true,
		},
		EndTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := predecessor.Drain(); err != nil {
		t.Fatal(err)
	}

	successor := codexLeaseRuntimeTestPlan("successor-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect,
	}})
	successor.Evidence = CodexLeaseRequestEvidence{PreviousResponseID: "different-response", HasEncryptedState: true}
	successor.RequiresAccountContinuity = true
	successor.authenticatedCallerContinuity = true
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(successor); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("authoritative caller override = %T %v, want continuity error", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("rejected authoritative caller override changed journal")
	}
}

func TestCodexLeaseRuntimeRejectsAuthenticatedCallerAdoptionWithoutEvidence(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	plan.RequiresAccountContinuity = true
	plan.authenticatedCallerContinuity = true

	if _, err := runtimeLease.BeginRequest(plan); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("caller adoption without evidence = %T %v, want invalid mutation", err, err)
	}
}

func TestCodexLeaseRuntimePersistsOpaqueDispatchPermitDigest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	permitDigest := strings.Repeat("d", 64)
	plan := codexLeaseRuntimeTestPlan("permit-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	plan.DispatchPermitDigest = permitDigest

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := store.hash("dispatch-permit", permitDigest)
	if handle.record.DispatchPermitDigest != want || bytes.Contains(store.journalBytes, []byte(permitDigest)) {
		t.Fatalf("persisted permit digest = %q, want opaque store digest", handle.record.DispatchPermitDigest)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatalf("MarkDispatched = %v", err)
	}
	invalid := codexLeaseRuntimeTestPlan("invalid-permit-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect,
	}})
	invalid.DispatchPermitDigest = "invalid"
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(invalid); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("invalid permit digest = %v, want invalid mutation", err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("invalid permit digest changed journal")
	}
}

func TestCodexLeaseRuntimeRetriesPinnedAccountCandidate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("pinned-retry-turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-a", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	plan.RequiresAccountContinuity = true

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatalf("same-account retry = %v", err)
	}
	if handle.AccountKey() != "account-a" || !handle.record.NonMigratable || handle.record.Attempts[0].State != CodexAttemptProviderFailed || handle.record.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("same-account retry handle = %#v", handle.record)
	}
}

func TestCodexLeaseRuntimeDoesNotInheritAccountContinuityFromEncryptedResponseState(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect,
	}})
	first, err := runtimeLease.BeginRequest(firstPlan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	portablePlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-2a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-2b", Kind: CodexAttemptSlotDirect},
	})
	portablePlan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	second, err := runtimeLease.BeginRequest(portablePlan)
	if err != nil {
		t.Fatal(err)
	}
	if second.record.NonMigratable || second.record.HasEncryptedState {
		t.Fatalf("inherited continuity = non-migratable %v current encrypted %v, want false/false", second.record.NonMigratable, second.record.HasEncryptedState)
	}
}

func TestCodexLeaseRuntimeMarksOnlyFirstRequestOfTurnAsNew(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !first.newTurn {
		t.Fatal("first durable request was not classified as a new turn")
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	second, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if second.newTurn {
		t.Fatal("later request on the same durable turn was classified as a new turn")
	}
}

func TestCodexLeaseRuntimeRetainsResponseEvidenceForEveryAdmittedTerminal(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"completed", "failed", "indeterminate"} {
		terminal := terminal
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{})
			if err != nil {
				t.Fatal(err)
			}
			anchor := "private-" + terminal + "-anchor"
			responseEvidence := CodexHTTPResponseEvidence{ResponseAnchor: anchor, HasResponseAnchor: true, HasEncryptedState: true}
			switch terminal {
			case "completed":
				handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: responseEvidence, EndTurn: true})
			case "failed":
				handle, err = handle.ProviderFailed(responseEvidence)
			case "indeterminate":
				handle, err = handle.IndeterminateContext(context.Background(), responseEvidence)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !handle.record.HasResponseAnchor || handle.record.CorrelationHash != store.hash("correlation", anchor) || !handle.record.HasEncryptedState || bytes.Contains(store.journalBytes, []byte(anchor)) {
				t.Fatalf("terminal evidence = %#v", handle.record)
			}
		})
	}
}

func TestCodexLeaseHTTPRequestLifecycleAdapter(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := NewCodexHTTPRequestLifecycle(handle)
	if lifecycle == nil || lifecycle.AccountKey() != "account-a" {
		t.Fatalf("adapter = %#v", lifecycle)
	}
	lifecycle, err = lifecycle.MarkDispatchedContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err = lifecycle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "adapter-state", HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err = lifecycle.ProviderFailed(CodexHTTPResponseEvidence{ResponseAnchor: "adapter-anchor", HasResponseAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	adapted := lifecycle.(*codexLeaseHTTPRequestLifecycle)
	if !adapted.handle.record.HasTurnState || !adapted.handle.record.HasResponseAnchor {
		t.Fatalf("adapted evidence = %#v", adapted.handle.record)
	}
}

func TestCodexLeaseRuntimeBeginReturnsCommittedCleanupHandle(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	owner.failAt.Store(owner.begins.Load() + 5)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil || handle == nil {
		t.Fatalf("BeginRequest = %#v, %v, want committed cleanup handle", handle, err)
	}
	assertCodexLeaseRuntimeRefs(t, handle, 1, 0, 0, false)
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("BeginRequest performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	owner.failAt.Store(0)
	handle, err = handle.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, handle, 0, 0, 0, true)
}

func TestCodexLeaseRuntimeMarkReturnsCommittedHandleWithoutPostCommitOwnerOperation(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	owner.failAt.Store(owner.begins.Load() + 3)

	dispatched, err := prepared.MarkDispatched()
	if err != nil || dispatched == nil {
		t.Fatalf("MarkDispatched = %#v, %v, want committed handle", dispatched, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("MarkDispatched performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	if state := codexLeaseCurrentAttemptState(dispatched.record); state != CodexAttemptDispatched {
		t.Fatalf("MarkDispatched attempt = %v, want dispatched", state)
	}
	assertCodexLeaseRuntimeRefs(t, dispatched, 1, 0, 0, false)
}

func TestCodexLeaseRuntimeAdmitReturnsCommittedHandleWithoutPostCommitOwnerOperation(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	owner.failAt.Store(owner.begins.Load() + 3)

	admitted, err := dispatched.AdmitHTTP2xx()
	if err != nil || admitted == nil {
		t.Fatalf("AdmitHTTP2xx = %#v, %v, want committed handle", admitted, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("AdmitHTTP2xx performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	if state := codexLeaseCurrentAttemptState(admitted.record); state != CodexAttemptStreaming || !admitted.record.EverAdmitted {
		t.Fatalf("AdmitHTTP2xx record = %#v, attempt=%v; want admitted streaming", admitted.record, state)
	}
	assertCodexLeaseRuntimeRefs(t, admitted, 1, 1, 1, false)
}

func TestCodexLeaseRuntimeRetryReturnsCommittedHandleWithoutPostCommitOwnerOperation(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	owner.failAt.Store(owner.begins.Load() + 3)

	retried, err := dispatched.RejectAndPrepare(2)
	if err != nil || retried == nil {
		t.Fatalf("RejectAndPrepare = %#v, %v, want committed handle", retried, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("RejectAndPrepare performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	if retried.AccountKey() != "account-b" || retried.AttemptGeneration() != 2 || len(retried.record.Attempts) != 2 || retried.record.Attempts[0].State != CodexAttemptProviderFailed || retried.record.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("retry committed handle = %#v", retried)
	}
	recordFence, found := codexLeaseRuntimeRecordFence(&retried.fence, retried.identity)
	if !found || len(recordFence.TouchedAttempts) != 1 || recordFence.TouchedAttempts[0].Generation != retried.AttemptGeneration() {
		t.Fatalf("retry mutation fence = %#v, want current attempt only", retried.fence)
	}
	oldRevision := retried.record.Attempts[0].Revision
	owner.failAt.Store(0)
	resent, err := retried.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if resent.record.Attempts[0].Revision != oldRevision {
		t.Fatalf("resent retry changed terminal attempt revision: got %d want %d", resent.record.Attempts[0].Revision, oldRevision)
	}
}

func TestCodexLeaseRuntimeFinishRejectedReturnsCommittedLastHandleWithoutPostCommitOwnerOperation(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	owner.failAt.Store(owner.begins.Load() + 3)

	failed, err := dispatched.FinishRejected()
	if err != nil || failed == nil {
		t.Fatalf("FinishRejected = %#v, %v, want committed handle", failed, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("FinishRejected performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	if failed.State() != LeaseFailedUnadmitted || codexLeaseCurrentAttemptState(failed.record) != CodexAttemptProviderFailed || !failed.fence.Current.IsZero() || failed.fence.Last != failed.identity {
		t.Fatalf("failed committed handle = %#v, fence=%#v", failed, failed.fence)
	}
	assertCodexLeaseRuntimeRefs(t, failed, 0, 0, 0, true)
}

func TestCodexLeaseRuntimeRetriesRestartableFailedLaneHead(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	failed, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}

	retry, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if retry.identity != failed.identity || retry.RequestGeneration() != 2 || retry.AttemptGeneration() != 1 || retry.State() != LeaseProvisional {
		t.Fatalf("failed-head retry = identity %#v record %#v", retry.identity, retry.record)
	}
	if _, err := failed.MarkDispatched(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("superseded failed handle = %T %v, want stale mutation", err, err)
	}
}

func TestCodexLeaseRuntimeFailedHeadRestartRacesSuccessorReservation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	failedPlan := codexLeaseRuntimeTestPlan("failed-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "failed-a", Kind: CodexAttemptSlotDirect}})
	failedPlan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	failed, err := runtimeLease.BeginRequest(failedPlan)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}

	successorPlan := codexLeaseRuntimeTestPlan("successor-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "successor-b", Kind: CodexAttemptSlotDirect}})
	successorPlan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	type result struct {
		name   string
		handle *CodexLeaseRequestHandle
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		handle, beginErr := runtimeLease.BeginRequest(failedPlan)
		results <- result{name: "restart", handle: handle, err: beginErr}
	}()
	go func() {
		<-start
		handle, beginErr := runtimeLease.BeginRequest(successorPlan)
		results <- result{name: "successor", handle: handle, err: beginErr}
	}()
	close(start)
	first := <-results
	second := <-results
	winners := 0
	var winner result
	for _, candidate := range []result{first, second} {
		if candidate.err == nil {
			winners++
			winner = candidate
			continue
		}
		if !errors.Is(candidate.err, ErrCodexConcurrentTurn) && !errors.Is(candidate.err, ErrCodexStaleTurn) && !errors.Is(candidate.err, ErrCodexLeaseStaleMutation) {
			t.Fatalf("%s loser = %T %v, want stale or concurrent", candidate.name, candidate.err, candidate.err)
		}
	}
	if winners != 1 {
		t.Fatalf("race results = %#v / %#v, want exactly one winner", first, second)
	}

	lane := findCodexLeaseV2CASTestLane(t, coordinator.store.v2.Lanes, failed.identity.LaneDigest)
	failedStored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, failed.identity)
	if winner.name == "restart" {
		if lane.CurrentTurnHash != failed.identity.TurnDigest || lane.LastTurnHash != failed.identity.TurnDigest || winner.handle.identity != failed.identity || failedStored.State != LeaseProvisional || failedStored.Generation != 2 {
			t.Fatalf("restart winner authority = lane %#v failed %#v handle %#v", lane, failedStored, winner.handle.record)
		}
		return
	}
	if lane.CurrentTurnHash != winner.handle.identity.TurnDigest || lane.LastTurnHash != winner.handle.identity.TurnDigest || winner.handle.identity.TurnDigest == failed.identity.TurnDigest || failedStored.State != LeaseFailedUnadmitted {
		t.Fatalf("successor winner authority = lane %#v failed %#v handle %#v", lane, failedStored, winner.handle.record)
	}
	if _, err := runtimeLease.BeginRequest(failedPlan); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("failed identity resurrected after successor = %T %v, want stale turn", err, err)
	}
}

func TestCodexLeaseRestartableFailedHeadRequiresDefinitiveAccountRejection(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	failed, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	failed, err = failed.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		state CodexAttemptState
		want  bool
	}{
		{state: CodexAttemptProviderFailed, want: true},
		{state: CodexAttemptAccountUnavailable, want: true},
		{state: CodexAttemptIndeterminate},
		{state: CodexAttemptProviderCompleted},
		{state: CodexAttemptAbandonedBeforeDispatch},
	} {
		record := cloneCodexJournalRecordV2(failed.record)
		record.Attempts[len(record.Attempts)-1].State = test.state
		if got := codexLeaseRestartableFailedHead(record); got != test.want {
			t.Fatalf("restartable failed head with %v = %t, want %t", test.state, got, test.want)
		}
	}
}

func TestCodexLeaseRuntimeRetriesAfterAllFrozenAccountsBecomeUnavailable(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseFailedUnadmitted || len(handle.record.Attempts) != 2 || handle.record.Attempts[0].State != CodexAttemptAccountUnavailable || handle.record.Attempts[1].State != CodexAttemptAccountUnavailable {
		t.Fatalf("terminal unavailable request = %#v", handle.record)
	}

	retryPlan := plan
	retryPlan.Accounts = []codex.AccountKey{"account-b"}
	retryPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b-fresh", Kind: CodexAttemptSlotDirect}}
	retryPlan.InitialSlot = 1
	retry, err := runtimeLease.BeginRequest(retryPlan)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AccountKey() != "account-b" || retry.RequestGeneration() != 2 || retry.AttemptGeneration() != 1 || retry.record.AccountHash != coordinator.store.hash("account", "account-b") {
		t.Fatalf("fresh request after terminal unavailability = %#v", retry.record)
	}
}

func TestCodexLeaseRuntimeRetriesUnavailableFailedSuccessorOnAnotherAccount(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	seed := codexLeaseRuntimeTestPlan("seed-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "seed-a", Kind: CodexAttemptSlotDirect}})
	seedHandle, err := runtimeLease.BeginRequest(seed)
	if err != nil {
		t.Fatal(err)
	}
	seedHandle, err = seedHandle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	seedHandle, err = seedHandle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	seedHandle, err = seedHandle.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedHandle.Drain(); err != nil {
		t.Fatal(err)
	}

	current := codexLeaseRuntimeTestPlan("current-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "current-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(current)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseFailedUnadmitted || handle.record.EverAdmitted || codexLeaseCurrentAttemptState(handle.record) != CodexAttemptAccountUnavailable {
		t.Fatalf("failed successor = %#v", handle.record)
	}

	retry := current
	retry.Accounts = []codex.AccountKey{"account-a", "account-b"}
	retry.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "retry-b", Kind: CodexAttemptSlotDirect}}
	retryHandle, err := runtimeLease.BeginRequest(retry)
	if err != nil {
		t.Fatal(err)
	}
	if retryHandle.identity != handle.identity || retryHandle.AccountKey() != "account-b" || retryHandle.RequestGeneration() != 2 || retryHandle.record.AccountHash != coordinator.store.hash("account", "account-b") {
		t.Fatalf("failed successor retry = %#v", retryHandle.record)
	}
}

func TestCodexLeaseRuntimeReplacesKnownUnavailableAccountBeforeDispatch(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if handle.AccountKey() != "account-b" || len(handle.record.Attempts) != 2 || handle.record.Attempts[0].State != CodexAttemptAccountUnavailable || handle.record.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("known-unavailable replacement = account %q record %#v", handle.AccountKey(), handle.record)
	}
}

func TestCodexLeaseRuntimeQuotaExhaustionPersistsAcrossSuccessorTurns(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordQuotaExhaustedContext(context.Background(), 2)
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
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatal(err)
	}

	successor := codexLeaseRuntimeTestPlan("turn-b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "successor-a", Kind: CodexAttemptSlotDirect}})
	successor.Accounts = []codex.AccountKey{"account-a", "account-b"}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), successor.Key, successor.Accounts, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("successor quota exhaustion = %#v, want account-a", snapshot.QuotaExhaustedAccountKeys)
	}
	before := append([]byte(nil), coordinator.store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(successor); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("successor exhausted-account BeginRequest = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, coordinator.store.journalBytes) {
		t.Fatal("rejected exhausted-account successor changed authority")
	}
}

func TestCodexLeaseRuntimeQuotaExhaustionProbeClearsOnlyOnCompletion(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.RecordQuotaExhaustedContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}

	probe := plan
	probe.Slots = []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a-stale", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-a", CandidateID: "candidate-a-refresh", Kind: CodexAttemptSlotEligibleManagedRefresh},
	}
	probe.QuotaExhaustionProbe = true
	handle, err = runtimeLease.BeginRequest(probe)
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
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("quota exhaustion after admission = %#v, want account-a until terminal completion", snapshot.QuotaExhaustedAccountKeys)
	}
	if _, err := handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.QuotaExhaustedAccountKeys) != 0 {
		t.Fatalf("quota exhaustion after proven recovery = %#v, want empty", snapshot.QuotaExhaustedAccountKeys)
	}
}

func TestCodexLeaseRuntimeQuotaExhaustionSurvivesRetrySuccessorAndRestart(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	plan.Accounts = []codex.AccountKey{"account-a", "account-b", "account-c"}
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordQuotaExhaustedContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.RecordQuotaExhaustedContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	runtimeLease = newCodexLeaseRuntimeTest(t, reopened)
	snapshot, err := reopened.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("reopened quota exhaustion = %#v, want account-a/account-b", snapshot.QuotaExhaustedAccountKeys)
	}
	retry := plan
	retry.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-c", CandidateID: "candidate-c", Kind: CodexAttemptSlotDirect}}
	retry.InitialSlot = 1
	handle, err = runtimeLease.BeginRequest(retry)
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
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatal(err)
	}

	successor := codexLeaseRuntimeTestPlan("turn-b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "successor-a", Kind: CodexAttemptSlotDirect}})
	successor.Accounts = plan.Accounts
	before := append([]byte(nil), reopened.store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(successor); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("successor exhausted-account BeginRequest = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, reopened.store.journalBytes) {
		t.Fatal("rejected successor exhausted-account request changed authority")
	}
	snapshot, err = reopened.LoadRouteSnapshot(context.Background(), successor.Key, successor.Accounts, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("successor quota exhaustion = %#v, want account-a/account-b", snapshot.QuotaExhaustedAccountKeys)
	}
}

func TestCodexLeaseRuntimeRejectsStaleAccountUnavailableCallback(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	stale, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	stale, err = stale.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	current, err := stale.RecordAccountUnavailableContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	wantUnavailable := append([]string(nil), coordinator.store.v2.Lanes[0].RequestUnavailableAccountHashes...)
	before := append([]byte(nil), coordinator.store.journalBytes...)
	beforeGeneration := coordinator.store.Generation()
	for _, replacementSlot := range []uint32{0, 2} {
		if _, err := stale.RecordAccountUnavailableContext(context.Background(), replacementSlot); !errors.Is(err, ErrCodexLeaseStaleMutation) {
			t.Fatalf("stale account-unavailable slot %d = %T %v, want stale mutation", replacementSlot, err, err)
		}
		if coordinator.store.Generation() != beforeGeneration || !bytes.Equal(before, coordinator.store.journalBytes) || !reflect.DeepEqual(coordinator.store.v2.Lanes[0].RequestUnavailableAccountHashes, wantUnavailable) || coordinator.store.poisoned != nil {
			t.Fatalf("stale account-unavailable slot %d changed authority: generation %d hashes %#v poison %v", replacementSlot, coordinator.store.Generation(), coordinator.store.v2.Lanes[0].RequestUnavailableAccountHashes, coordinator.store.poisoned)
		}
	}
	stored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, current.identity)
	if stored.RecordGeneration != current.record.RecordGeneration || stored.CurrentAttemptGeneration != current.record.CurrentAttemptGeneration || len(stored.Attempts) != 2 || stored.Attempts[0].State != CodexAttemptAccountUnavailable || stored.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("stale callback changed replacement = %#v, want %#v", stored, current.record)
	}
}

func TestCodexLeaseRuntimeRejectsStaleQuotaExhaustedCallback(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	stale, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	stale, err = stale.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	current, err := stale.RecordQuotaExhaustedContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	wantHashes := append([]string(nil), coordinator.store.v2.Lanes[0].QuotaExhaustedAccountHashes...)
	before := append([]byte(nil), coordinator.store.journalBytes...)
	beforeGeneration := coordinator.store.Generation()
	for _, replacementSlot := range []uint32{0, 2} {
		if _, err := stale.RecordQuotaExhaustedContext(context.Background(), replacementSlot); !errors.Is(err, ErrCodexLeaseStaleMutation) {
			t.Fatalf("stale quota-exhausted slot %d = %T %v, want stale mutation", replacementSlot, err, err)
		}
		if coordinator.store.Generation() != beforeGeneration || !bytes.Equal(before, coordinator.store.journalBytes) || !reflect.DeepEqual(coordinator.store.v2.Lanes[0].QuotaExhaustedAccountHashes, wantHashes) || coordinator.store.poisoned != nil {
			t.Fatalf("stale quota-exhausted slot %d changed authority: generation %d hashes %#v poison %v", replacementSlot, coordinator.store.Generation(), coordinator.store.v2.Lanes[0].QuotaExhaustedAccountHashes, coordinator.store.poisoned)
		}
	}
	stored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, current.identity)
	if stored.RecordGeneration != current.record.RecordGeneration || stored.CurrentAttemptGeneration != current.record.CurrentAttemptGeneration {
		t.Fatalf("stale quota callback changed replacement = %#v, want %#v", stored, current.record)
	}
}

func TestCodexLeaseRuntimeFullCreateRebindsAfterNonPortableAccountUnavailable(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "initial-a", Kind: CodexAttemptSlotDirect}})
	initialHandle, err := runtimeLease.BeginRequest(initial)
	if err != nil {
		t.Fatal(err)
	}
	initialHandle, err = initialHandle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	initialHandle, err = initialHandle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	initialHandle, err = initialHandle.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-a", HasResponseAnchor: true},
		EndTurn:                   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	initialHandle, err = initialHandle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	initialAffinityTime := initialHandle.record.AdmittedAt

	incremental := initial
	incremental.RequiresAccountContinuity = true
	incremental.Evidence.PreviousResponseID = "response-a"
	handle, err := runtimeLease.BeginRequest(incremental)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !handle.EverAdmitted() || !handle.record.NonMigratable || handle.State() != LeaseBoundQuiescent {
		t.Fatalf("terminal non-portable request = %#v", handle.record)
	}

	fresh := initial
	fresh.Accounts = []codex.AccountKey{"account-b"}
	fresh.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "fresh-b", Kind: CodexAttemptSlotDirect}}
	fresh.InitialSlot = 1
	rebound, err := runtimeLease.BeginRequest(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.AccountKey() != "account-b" || rebound.record.NonMigratable || rebound.RequestGeneration() != 3 || rebound.record.AccountHash != coordinator.store.hash("account", "account-a") {
		t.Fatalf("fresh full-create pending binding = %#v", rebound.record)
	}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), fresh.Key, []codex.AccountKey{"account-a", "account-b"}, fresh.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BoundAccountKey != "account-b" || snapshot.AffinityAccountKey != "account-a" || !snapshot.AffinityCacheAdmittedAt.Equal(initialAffinityTime) {
		t.Fatalf("pre-admission rebound snapshot = %#v, want bound B with historical affinity A", snapshot)
	}
}

func TestCodexLeaseRuntimeAccountUnavailableAccumulatesOnlyWithinTurn(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	accounts := []codex.AccountKey{"account-a", "account-b", "account-c"}
	planFor := func(turn string, account codex.AccountKey) CodexLeaseRequestPlan {
		plan := codexLeaseRuntimeTestPlan(turn, []CodexLeaseAttemptSlotPlan{{
			AccountKey: account, CandidateID: "candidate-" + string(account), Kind: CodexAttemptSlotDirect,
		}})
		plan.Accounts = accounts
		return plan
	}
	rejectAfterAdmission := func(plan CodexLeaseRequestPlan) {
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
		if _, err := handle.RecordAccountUnavailableContext(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}

	first := planFor("turn-a", "account-a")
	rejectAfterAdmission(first)
	second := planFor("turn-a", "account-b")
	rejectAfterAdmission(second)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator = reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	runtimeLease = newCodexLeaseRuntimeTest(t, coordinator)
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), first.Key, accounts, first.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.UnavailableAccountKeys, []codex.AccountKey{"account-a", "account-b"}) || len(snapshot.QuotaExhaustedAccountKeys) != 0 {
		t.Fatalf("same-turn unavailable accounts = %#v quota %#v, want A/B generic only", snapshot.UnavailableAccountKeys, snapshot.QuotaExhaustedAccountKeys)
	}

	third := planFor("turn-a", "account-c")
	completed, err := runtimeLease.BeginRequest(third)
	if err != nil {
		stored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, codexLaneTupleIdentity(coordinator.store.v2.Lanes[0], true))
		t.Fatalf("C BeginRequest after reopened A/B unavailability = %v; stored %#v", err, stored)
	}
	completed, err = completed.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	completed, err = completed.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	completed, err = completed.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	completed, err = completed.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if completed.AccountKey() != "account-c" {
		t.Fatalf("third same-turn account = %q, want account-c", completed.AccountKey())
	}
	successor := planFor("turn-b", "account-a")
	snapshot, err = coordinator.LoadRouteSnapshot(context.Background(), successor.Key, accounts, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.UnavailableAccountKeys) != 0 || len(snapshot.QuotaExhaustedAccountKeys) != 0 {
		t.Fatalf("successor inherited generic unavailability: unavailable %#v quota %#v", snapshot.UnavailableAccountKeys, snapshot.QuotaExhaustedAccountKeys)
	}
}

func TestCodexLeaseRuntimePendingAccountUnavailableRebindSurvivesRestart(t *testing.T) {
	for _, test := range []struct {
		name       string
		dispatched bool
	}{
		{name: "prepared"},
		{name: "dispatched", dispatched: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "a", Kind: CodexAttemptSlotDirect}})
			plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
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
			handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			replacementPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "b", Kind: CodexAttemptSlotDirect}})
			replacementPlan.Accounts = plan.Accounts
			replacement, err := runtimeLease.BeginRequest(replacementPlan)
			if err != nil {
				t.Fatal(err)
			}
			if test.dispatched {
				replacement, err = replacement.MarkDispatched()
				if err != nil {
					t.Fatal(err)
				}
			}
			identity := replacement.identity
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			coordinator = reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
			restored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, identity)
			if restored.AccountHash != coordinator.store.hash("account", "account-a") || !restored.EverAdmitted || restored.State != LeaseOrphaned {
				t.Fatalf("reopened pending rebind = %#v", restored)
			}
			runtimeLease = newCodexLeaseRuntimeTest(t, coordinator)
			if test.dispatched {
				if codexLeaseCurrentAttemptState(restored) != CodexAttemptIndeterminate || !restored.NonMigratable {
					t.Fatalf("reopened dispatched rebind = %#v", restored)
				}
				before := append([]byte(nil), coordinator.store.journalBytes...)
				if _, err := runtimeLease.BeginRequest(replacementPlan); !errors.Is(err, ErrCodexContinuity) {
					t.Fatalf("indeterminate pending rebind retry = %T %v, want fail-closed continuity", err, err)
				}
				if !bytes.Equal(before, coordinator.store.journalBytes) {
					t.Fatal("blocked indeterminate pending rebind changed journal")
				}
				return
			}
			if codexLeaseCurrentAttemptState(restored) != CodexAttemptAbandonedBeforeDispatch || restored.NonMigratable {
				t.Fatalf("reopened prepared rebind = %#v", restored)
			}
			resumed, err := runtimeLease.BeginRequest(replacementPlan)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.AccountKey() != "account-b" || resumed.record.AccountHash != coordinator.store.hash("account", "account-a") || !resumed.EverAdmitted() {
				t.Fatalf("resumed pending rebind = %#v", resumed.record)
			}
		})
	}
}

func TestCodexLeaseRuntimeCompletesAccountUnavailableCycle(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	accounts := []codex.AccountKey{"account-a", "account-b"}
	terminal := func(account codex.AccountKey, quota bool) *CodexLeaseRequestHandle {
		plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: account, CandidateID: string(account), Kind: CodexAttemptSlotDirect}})
		plan.Accounts = accounts
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
		if quota {
			handle, err = handle.RecordQuotaExhaustedContext(context.Background(), 0)
		} else {
			handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		return handle
	}
	terminal("account-a", true)
	final := terminal("account-b", false)
	cleared, err := final.CompleteAccountUnavailableCycleContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), final.key, accounts, final.authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.UnavailableAccountKeys, []codex.AccountKey{"account-a"}) || !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("completed unavailable cycle = generic %#v quota %#v", snapshot.UnavailableAccountKeys, snapshot.QuotaExhaustedAccountKeys)
	}
	before := append([]byte(nil), coordinator.store.journalBytes...)
	if _, err := final.CompleteAccountUnavailableCycle(); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("repeated unavailable cycle completion = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, coordinator.store.journalBytes) || cleared == nil {
		t.Fatal("stale unavailable cycle completion changed journal")
	}
}

func TestCodexLeaseCASRequiresUnavailableProvenanceForFullCreateRebind(t *testing.T) {
	t.Parallel()

	commitBegin := func(t *testing.T, coordinator *CodexContinuityCoordinator, runtimeLease *CodexLeaseRuntime, handle *CodexLeaseRequestHandle, plan CodexLeaseRequestPlan, accountHash string) error {
		t.Helper()
		desired := codexLeaseRuntimeMutationRecord(handle.record)
		desired.State = LeaseBoundActive
		desired.AccountHash = accountHash
		desired.CodexCurrentRequest = runtimeLease.requestAfterImage(plan)
		desired.NonMigratable = false
		desired.DownstreamSocketGeneration = 0
		desired.UpstreamSocketGeneration = 0
		desired.SocketLineageExtinct = false
		fence := cloneCodexLeaseGenerationFence(handle.fence)
		recordFence, found := codexLeaseRuntimeRecordFence(&fence, handle.identity)
		if !found {
			t.Fatal("current record fence is absent")
		}
		recordFence.TouchedAttempts = []CodexAttemptFence{{}}
		identity := handle.identity
		_, err := coordinator.store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{desired}})
		return err
	}

	t.Run("ordinary admitted request cannot rebind", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "initial-a", Kind: CodexAttemptSlotDirect}})
		completed := completeCodexLeaseRuntimeTurn(t, runtimeLease, initial)
		foreign := initial
		foreign.Accounts = []codex.AccountKey{"account-a", "account-b"}
		foreign.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "foreign-b", Kind: CodexAttemptSlotDirect}}
		before := append([]byte(nil), coordinator.store.journalBytes...)
		beforeGeneration := coordinator.store.Generation()
		if err := commitBegin(t, coordinator, runtimeLease, completed, foreign, completed.record.AccountHash); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
			t.Fatalf("ordinary foreign-account BeginRequest = %T %v, want invalid mutation", err, err)
		}
		if coordinator.store.Generation() != beforeGeneration || !bytes.Equal(before, coordinator.store.journalBytes) || coordinator.store.poisoned != nil {
			t.Fatalf("rejected ordinary rebind changed authority: generation %d poison %v", coordinator.store.Generation(), coordinator.store.poisoned)
		}
	})

	t.Run("unavailable reset cannot rotate binding before admission", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "initial-a", Kind: CodexAttemptSlotDirect}})
		completeCodexLeaseRuntimeTurn(t, runtimeLease, initial)
		incremental := initial
		incremental.RequiresAccountContinuity = true
		handle, err := runtimeLease.BeginRequest(incremental)
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		foreign := initial
		foreign.Accounts = []codex.AccountKey{"account-a", "account-b", "account-c"}
		foreign.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "foreign-b", Kind: CodexAttemptSlotDirect}}
		before := append([]byte(nil), coordinator.store.journalBytes...)
		beforeGeneration := coordinator.store.Generation()
		if err := commitBegin(t, coordinator, runtimeLease, handle, foreign, coordinator.store.hash("account", "account-c")); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
			t.Fatalf("pre-admission binding rotation = %T %v, want invalid mutation", err, err)
		}
		if coordinator.store.Generation() != beforeGeneration || !bytes.Equal(before, coordinator.store.journalBytes) || coordinator.store.poisoned != nil {
			t.Fatalf("rejected pre-admission rotation changed authority: generation %d poison %v", coordinator.store.Generation(), coordinator.store.poisoned)
		}
	})
}

func TestCodexLeaseRuntimeRetriesReboundAccountCandidateBeforeAdmission(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "initial-a", Kind: CodexAttemptSlotDirect}})
	initialHandle := completeCodexLeaseRuntimeTurn(t, runtimeLease, initial)
	initialAdmissionGeneration := initialHandle.record.AdmissionJournalGeneration
	initialAdmissionRequest := initialHandle.record.AdmissionRequestGeneration
	initialAdmittedAt := initialHandle.record.AdmittedAt

	request := initial
	request.Accounts = []codex.AccountKey{"account-a", "account-b"}
	request.Slots = []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b-stale", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b-current", Kind: CodexAttemptSlotEligibleManagedRefresh},
	}
	handle, err := runtimeLease.BeginRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(3)
	if err != nil {
		t.Fatal(err)
	}
	if handle.AccountKey() != "account-b" || handle.record.AccountHash != coordinator.store.hash("account", "account-a") || len(handle.record.Attempts) != 3 || handle.record.Attempts[0].State != CodexAttemptAccountUnavailable || handle.record.Attempts[1].State != CodexAttemptProviderFailed || handle.record.Attempts[2].State != CodexAttemptPrepared {
		t.Fatalf("rebound account retry = account %q record %#v", handle.AccountKey(), handle.record)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if handle.record.AccountHash != coordinator.store.hash("account", "account-b") || handle.record.AdmissionJournalGeneration != initialAdmissionGeneration || handle.record.AdmissionRequestGeneration != initialAdmissionRequest || !handle.record.AdmittedAt.Equal(initialAdmittedAt) {
		t.Fatalf("rebound retry admission = %#v", handle.record)
	}
}

func TestCodexLeaseRuntimeFirstAdmissionAfterCrossAccountRetry(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(2)
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
	if handle.AccountKey() != "account-b" || handle.record.AccountHash != coordinator.store.hash("account", "account-b") || !handle.record.EverAdmitted || handle.record.AdmissionRequestGeneration != handle.record.Generation {
		t.Fatalf("first admission after cross-account retry = %#v", handle.record)
	}
}

func TestCodexLeaseRuntimeTerminalAndDrainReturnCommittedHandlesWithoutPostCommitOwnerOperations(t *testing.T) {
	t.Parallel()
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("post-commit owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := dispatched.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	owner.failAt.Store(owner.begins.Load() + 3)

	terminal, err := admitted.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil || terminal == nil {
		t.Fatalf("ProviderCompleted = %#v, %v, want committed handle", terminal, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("ProviderCompleted performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	if terminal.State() != LeaseBoundQuiescent || codexLeaseCurrentAttemptState(terminal.record) != CodexAttemptProviderCompleted {
		t.Fatalf("terminal committed handle = %#v", terminal)
	}
	assertCodexLeaseRuntimeRefs(t, terminal, 0, 0, 1, false)
	owner.failAt.Store(owner.begins.Load() + 3)

	drained, err := terminal.Drain()
	if err != nil || drained == nil {
		t.Fatalf("Drain = %#v, %v, want committed handle", drained, err)
	}
	if owner.begins.Load() >= owner.failAt.Load() {
		t.Fatalf("Drain performed a post-commit owner operation: begins %d fail-at %d", owner.begins.Load(), owner.failAt.Load())
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)
}

func TestCodexLeaseCommitCapturesInstalledRequestBeforeCompact(t *testing.T) {
	t.Parallel()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	store := coordinator.Store()
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptDispatched
		}
	}
	captureEntered := make(chan struct{})
	releaseCapture := make(chan struct{})
	commitDone := make(chan error, 1)
	var captureErr error
	go func() {
		_, commitErr := store.commitLane(handle.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}, func(_ CodexLeaseGenerationFence, installed codexLeaseJournalEnvelopeV2) {
			if len(installed.Records) == 0 || codexLeaseCurrentAttemptState(installed.Records[0]) != CodexAttemptDispatched {
				captureErr = errors.New("capture did not observe installed dispatched request")
			}
			close(captureEntered)
			<-releaseCapture
		})
		commitDone <- commitErr
	}()
	<-captureEntered
	compactDone := make(chan error, 1)
	go func() { compactDone <- store.Compact(*now, DefaultCodexLeaseRetention) }()
	select {
	case compactErr := <-compactDone:
		t.Fatalf("Compact crossed installed capture: %v", compactErr)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCapture)
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	if err := <-compactDone; err != nil {
		t.Fatal(err)
	}
}

func TestCodexLeaseRuntimeHTTPAdmissionAndObserverDrain(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})

	prepared, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State() != LeaseProvisional || prepared.EverAdmitted() || prepared.AccountKey() != "account-a" || prepared.RequestGeneration() != 1 || prepared.AttemptGeneration() != 1 {
		t.Fatalf("prepared handle = state %v admitted %v account %q request %d attempt %d", prepared.State(), prepared.EverAdmitted(), prepared.AccountKey(), prepared.RequestGeneration(), prepared.AttemptGeneration())
	}
	assertCodexLeaseRuntimeRefs(t, prepared, 1, 0, 0, false)
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, dispatched, 1, 0, 0, false)
	observer, err := dispatched.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if !observer.EverAdmitted() || observer.State() != LeaseBoundActive {
		t.Fatalf("admitted observer = state %v admitted %v", observer.State(), observer.EverAdmitted())
	}
	assertCodexLeaseRuntimeRefs(t, observer, 1, 1, 1, false)
	completed, err := observer.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State() != LeaseContinuationPending {
		t.Fatalf("completed state = %v, want continuation pending", completed.State())
	}
	assertCodexLeaseRuntimeRefs(t, completed, 0, 0, 1, false)

	next := plan
	next.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	beforeDrain := append([]byte(nil), store.journalBytes...)
	if _, err := runtime.BeginRequest(next); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("BeginRequest before observer drain = %T %v, want concurrent turn", err, err)
	}
	if !bytes.Equal(beforeDrain, store.journalBytes) {
		t.Fatal("blocked BeginRequest changed journal")
	}
	drained, err := completed.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if drained.State() != LeaseContinuationPending {
		t.Fatalf("drained state = %v", drained.State())
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)
	crossAccount := plan
	crossAccount.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-cross-account", Kind: CodexAttemptSlotDirect}}
	beforeCrossAccount := append([]byte(nil), store.journalBytes...)
	if _, err := runtime.BeginRequest(crossAccount); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("post-admission cross-account request = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(beforeCrossAccount, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("post-admission cross-account request changed authority: poison %v", store.poisoned)
	}
	second, err := runtime.BeginRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State() != LeaseBoundActive || !second.EverAdmitted() || second.AccountKey() != "account-a" || second.RequestGeneration() != 2 {
		t.Fatalf("second request = state %v admitted %v account %q generation %d", second.State(), second.EverAdmitted(), second.AccountKey(), second.RequestGeneration())
	}
	assertCodexLeaseRuntimeRefs(t, second, 1, 0, 0, false)
}

func TestCodexLeaseRuntimeRetriesSameAccountAfterAdmission(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-initial", Kind: CodexAttemptSlotDirect,
	}})

	handle, err := runtimeLease.BeginRequest(initial)
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
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	admissionJournalGeneration := handle.record.AdmissionJournalGeneration
	admissionRequestGeneration := handle.record.AdmissionRequestGeneration
	admittedAt := handle.record.AdmittedAt

	continuation := initial
	continuation.Slots = []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-stale", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-a", CandidateID: "candidate-current", Kind: CodexAttemptSlotDirect},
	}
	handle, err = runtimeLease.BeginRequest(continuation)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatalf("same-account retry after admission = %T %v", err, err)
	}
	if handle.State() != LeaseBoundActive || !handle.EverAdmitted() || handle.AccountKey() != "account-a" || handle.RequestGeneration() != 2 || handle.AttemptGeneration() != 2 {
		t.Fatalf("retry handle = state %v admitted %t account %q request %d attempt %d", handle.State(), handle.EverAdmitted(), handle.AccountKey(), handle.RequestGeneration(), handle.AttemptGeneration())
	}
	if len(handle.record.Attempts) != 2 || handle.record.Attempts[0].State != CodexAttemptProviderFailed || handle.record.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("retry attempts = %#v", handle.record.Attempts)
	}
	if handle.record.AdmissionJournalGeneration != admissionJournalGeneration || handle.record.AdmissionRequestGeneration != admissionRequestGeneration || handle.record.AdmittedAt != admittedAt {
		t.Fatalf("retry changed first-admission authority: %#v", handle.record)
	}
	assertCodexLeaseRuntimeRefs(t, handle, 1, 0, 0, false)

	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseContinuationPending || !handle.EverAdmitted() || handle.record.AdmissionJournalGeneration != admissionJournalGeneration || handle.record.AdmissionRequestGeneration != admissionRequestGeneration || handle.record.AdmittedAt != admittedAt {
		t.Fatalf("completed retry changed admission authority: %#v", handle.record)
	}
}

func TestCodexLeaseRuntimeAdmittedPortableRequestFreezesHardLimitFallback(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-initial", Kind: CodexAttemptSlotDirect,
	}})
	completeCodexLeaseRuntimeTurn(t, runtimeLease, initial)

	request := initial
	request.Accounts = []codex.AccountKey{"account-a", "account-b"}
	request.Slots = []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-bound", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-fallback", Kind: CodexAttemptSlotDirect},
	}
	handle, err := runtimeLease.BeginRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if handle.AccountKey() != "account-a" || handle.AttemptGeneration() != 1 || len(handle.record.AttemptEnvelope.Slots) != 2 {
		t.Fatalf("portable fallback request = account %q attempt %d envelope %#v", handle.AccountKey(), handle.AttemptGeneration(), handle.record.AttemptEnvelope)
	}
}

func TestCodexLeaseRuntimeAccountUnavailableRebindsAdmittedPortableRequest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	initial := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-initial", Kind: CodexAttemptSlotDirect,
	}})
	initialHandle := completeCodexLeaseRuntimeTurn(t, runtimeLease, initial)
	initialAdmissionGeneration := initialHandle.record.AdmissionJournalGeneration
	initialAdmissionRequest := initialHandle.record.AdmissionRequestGeneration
	initialAdmittedAt := initialHandle.record.AdmittedAt

	request := initial
	request.Accounts = []codex.AccountKey{"account-a", "account-b"}
	request.Slots = []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-bound", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-fallback", Kind: CodexAttemptSlotDirect},
	}
	handle, err := runtimeLease.BeginRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if handle.AccountKey() != "account-b" || handle.AttemptGeneration() != 2 || len(handle.record.Attempts) != 2 || handle.record.Attempts[0].State != CodexAttemptAccountUnavailable || handle.record.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("account-unavailable replacement = account %q request %#v", handle.AccountKey(), handle.record.CodexCurrentRequest)
	}
	if handle.record.AccountHash != coordinator.store.hash("account", "account-a") || handle.record.AdmissionJournalGeneration != initialAdmissionGeneration || handle.record.AdmissionRequestGeneration != initialAdmissionRequest || !handle.record.AdmittedAt.Equal(initialAdmittedAt) {
		t.Fatalf("prepared replacement changed admitted binding: %#v", handle.record)
	}

	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if handle.AccountKey() != "account-b" || handle.record.AccountHash != coordinator.store.hash("account", "account-b") || handle.record.AdmissionJournalGeneration != initialAdmissionGeneration || handle.record.AdmissionRequestGeneration != initialAdmissionRequest || !handle.record.AdmittedAt.Equal(initialAdmittedAt) {
		t.Fatalf("replacement admission did not install account-b binding: %#v", handle.record)
	}
	lane := coordinator.store.v2.Lanes[0]
	if lane.LastAdmittedAccountHash != handle.record.AccountHash || lane.LastAdmissionJournalGeneration != handle.record.AdmissionJournalGeneration || !lane.LastAdmittedAt.Equal(handle.record.AdmittedAt) {
		t.Fatalf("replacement lane affinity = %#v, record %#v", lane, handle.record)
	}
}

func TestCodexLeaseRuntimeStreamingAccountUnavailableRequiresReconnect(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
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
	admissionGeneration := handle.record.AdmissionJournalGeneration
	admittedAt := handle.record.AdmittedAt
	before := append([]byte(nil), coordinator.store.journalBytes...)
	if _, err := handle.RecordAccountUnavailableContext(context.Background(), 2); !errors.Is(err, ErrCodexLeaseTransition) {
		t.Fatalf("streaming in-place replacement = %T %v, want transition error", err, err)
	}
	if !bytes.Equal(before, coordinator.store.journalBytes) {
		t.Fatal("rejected streaming replacement changed authority")
	}

	handle, err = handle.RecordAccountUnavailableContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseBoundQuiescent || codexLeaseCurrentAttemptState(handle.record) != CodexAttemptAccountUnavailable || !handle.record.SocketLineageExtinct || handle.record.RoutingRefs != 0 || handle.record.AttemptRefs != 0 || handle.record.ResponseObserverRefs != 0 {
		t.Fatalf("streaming account-unavailable terminal = %#v", handle.record)
	}
	if !handle.record.EverAdmitted || handle.record.AdmissionJournalGeneration != admissionGeneration || !handle.record.AdmittedAt.Equal(admittedAt) || handle.record.AccountHash != coordinator.store.hash("account", "account-a") {
		t.Fatalf("streaming account-unavailable changed admitted binding = %#v", handle.record)
	}
}

func TestCodexLeaseRuntimeResumesRetainedAuthoritativeTurnInObserveMode(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("retained-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "candidate-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	admitted := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)

	retained := codexLeaseRuntimeTestPlan("retained-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "candidate-retained",
		Kind:        CodexAttemptSlotDirect,
	}})
	retained.Authority = CodexLeaseAuthorityPolicy{
		ModeEpoch:                   10,
		RetainedAuthoritativeEpochs: []uint64{9},
	}
	retained.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity:         admitted.identity,
		AccountKey:       admitted.AccountKey(),
		RecordGeneration: admitted.record.RecordGeneration,
	}

	resumed, err := runtimeLease.BeginRequest(retained)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.AccountKey() != "account-a" || resumed.identity != admitted.identity || resumed.identity.ModeEpoch != 9 || !resumed.identity.Authoritative || resumed.RequestGeneration() != 2 || !resumed.EverAdmitted() {
		t.Fatalf("retained request = identity %#v account %q request %d admitted %t", resumed.identity, resumed.AccountKey(), resumed.RequestGeneration(), resumed.EverAdmitted())
	}
	if len(coordinator.Store().v2.Records) != 1 || coordinator.Store().v2.Records[0].Identity() != admitted.identity {
		t.Fatalf("retained request created shadow authority: %#v", coordinator.Store().v2.Records)
	}
}

func TestCodexLeaseRuntimeRejectsChangedExpectedBoundRecordBeforeMutation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("expected-bound", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "candidate-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	admitted := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)

	next := plan
	next.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	next.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity:         admitted.identity,
		AccountKey:       admitted.AccountKey(),
		RecordGeneration: admitted.record.RecordGeneration - 1,
	}
	before := append([]byte(nil), coordinator.store.journalBytes...)
	beforeGeneration := coordinator.store.Generation()
	if _, err := runtimeLease.BeginRequest(next); !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("BeginRequest error = %T %v, want authority mismatch", err, err)
	}
	if coordinator.store.Generation() != beforeGeneration || !bytes.Equal(coordinator.store.journalBytes, before) {
		t.Fatal("changed expected bound mutated journal")
	}
}

func TestCodexLeaseRuntimeKeepsExpectedBoundAcrossRequestBuckets(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("expected-bound-buckets", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "candidate-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.RequiredBuckets = []CapacityBucket{CapacityBucketBase, CapacityBucketForModel(codexSparkModel)}
	admitted := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)

	next := plan
	next.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
	next.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	next.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity:         admitted.identity,
		AccountKey:       admitted.AccountKey(),
		RecordGeneration: admitted.record.RecordGeneration,
	}

	resumed, err := runtimeLease.BeginRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.AccountKey() != "account-a" {
		t.Fatalf("resumed account = %q, want account-a", resumed.AccountKey())
	}
}

func TestCodexLeaseRuntimeAbandonsPreparedRequestAfterCancellation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	requestContext, cancelRequest := context.WithCancel(context.Background())
	prepared, err := runtimeLease.BeginRequestContext(requestContext, plan)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()

	cancelledContext, cancelCleanup := context.WithCancel(context.Background())
	cancelCleanup()
	beforeCancelled := append([]byte(nil), store.journalBytes...)
	beforeCancelledGeneration := store.Generation()
	if _, err := prepared.AbandonBeforeDispatchContext(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled abandonment = %T %v, want context canceled", err, err)
	}
	if store.Generation() != beforeCancelledGeneration || !bytes.Equal(beforeCancelled, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	abandoned, err := prepared.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State() != LeaseProvisional || abandoned.EverAdmitted() || codexLeaseCurrentAttemptState(abandoned.record) != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("abandoned request = %#v", abandoned.record)
	}
	assertCodexLeaseRuntimeRefs(t, abandoned, 0, 0, 0, true)

	beforeStale := append([]byte(nil), store.journalBytes...)
	beforeStaleGeneration := store.Generation()
	if _, err := prepared.AbandonBeforeDispatch(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale abandonment = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeStaleGeneration || !bytes.Equal(beforeStale, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	nextPlan := plan
	nextPlan.Accounts = []codex.AccountKey{"account-b"}
	nextPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	next, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	if next.RequestGeneration() != 2 || next.AccountKey() != "account-b" || next.State() != LeaseProvisional {
		t.Fatalf("request after abandonment = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeAbandonsLaterAdmittedRequest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	admitted := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)
	admissionJournalGeneration := admitted.record.AdmissionJournalGeneration
	admissionRequestGeneration := admitted.record.AdmissionRequestGeneration
	admittedAt := admitted.record.AdmittedAt

	nextPlan := plan
	nextPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	prepared, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := prepared.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State() != LeaseOrphaned || !abandoned.EverAdmitted() || abandoned.AccountKey() != "account-a" || abandoned.RequestGeneration() != 2 || codexLeaseCurrentAttemptState(abandoned.record) != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("abandoned admitted request = %#v", abandoned.record)
	}
	if abandoned.record.AdmissionJournalGeneration != admissionJournalGeneration || abandoned.record.AdmissionRequestGeneration != admissionRequestGeneration || !abandoned.record.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("abandonment changed first-admission evidence: %#v", abandoned.record)
	}
	assertCodexLeaseRuntimeRefs(t, abandoned, 0, 0, 0, true)
	recovered, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RequestGeneration() != 3 || recovered.AccountKey() != "account-a" || !recovered.EverAdmitted() {
		t.Fatalf("request after admitted abandonment = %#v", recovered.record)
	}
}

func TestCodexLeaseRuntimeProviderFailureRetainsObserverUntilDrain(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	var reject atomic.Bool
	var revalidations atomic.Int32
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		revalidations.Add(1)
		if reject.Load() {
			return errors.New("account is no longer routable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	observer, err := handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(true)
	beforeRevalidations := revalidations.Load()
	failed, err := observer.ProviderFailed(CodexHTTPResponseEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State() != LeaseBoundQuiescent || !failed.EverAdmitted() || failed.AccountKey() != "account-a" || codexLeaseCurrentAttemptState(failed.record) != CodexAttemptProviderFailed {
		t.Fatalf("provider failure = %#v", failed.record)
	}
	if revalidations.Load() != beforeRevalidations {
		t.Fatalf("terminal provider failure revalidated account: %d -> %d", beforeRevalidations, revalidations.Load())
	}
	assertCodexLeaseRuntimeRefs(t, failed, 0, 0, 1, false)

	beforeStale := append([]byte(nil), store.journalBytes...)
	beforeStaleGeneration := store.Generation()
	if _, err := observer.ProviderFailed(CodexHTTPResponseEvidence{}); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale provider failure = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeStaleGeneration || !bytes.Equal(beforeStale, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale provider failure changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	drained, err := failed.Drain()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)
}

func TestCodexLeaseRuntimeCreatesSuccessorAndNeverResurrectsPredecessor(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}})
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, firstPlan)
	firstIdentity := first.identity

	secondPlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}})
	second, err := runtimeLease.BeginRequest(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	if second.State() != LeaseProvisional || second.EverAdmitted() || second.RequestGeneration() != 1 || second.AccountKey() != "account-a" {
		t.Fatalf("successor request = %#v", second.record)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, firstIdentity)
	if predecessor.State != LeaseSuperseded || predecessor.RoutingRefs != 0 || predecessor.AttemptRefs != 0 || predecessor.ResponseObserverRefs != 0 {
		t.Fatalf("superseded predecessor = %#v", predecessor)
	}
	if second.record.PredecessorTurnHash != firstIdentity.TurnDigest || second.record.PredecessorModeEpoch != firstIdentity.ModeEpoch || second.record.PredecessorAuthoritative != firstIdentity.Authoritative || second.record.PredecessorGeneration != predecessor.RecordGeneration {
		t.Fatalf("successor predecessor fence = %#v, want %#v generation %d", second.record, firstIdentity, predecessor.RecordGeneration)
	}
	if store.v2.Lanes[0].CurrentTurnHash != second.identity.TurnDigest || store.v2.Lanes[0].LastTurnHash != second.identity.TurnDigest {
		t.Fatalf("successor lane pointers = %#v", store.v2.Lanes[0])
	}
	for _, fence := range second.fence.TouchedRecords {
		if fence.Record == firstIdentity && len(fence.TouchedAttempts) != 0 {
			t.Fatalf("successor handle touched predecessor attempts: %#v", fence.TouchedAttempts)
		}
	}
	beforeHistorical := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(firstPlan); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("historical predecessor request = %T %v, want stale turn", err, err)
	}
	if !bytes.Equal(beforeHistorical, store.journalBytes) {
		t.Fatal("historical predecessor request changed journal")
	}

	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	failed, err := second.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if failed.State() != LeaseFailedUnadmitted {
		t.Fatalf("failed successor = %#v", failed.record)
	}
	if store.v2.Lanes[0].CurrentTurnHash != "" || store.v2.Lanes[0].LastTurnHash != failed.identity.TurnDigest {
		t.Fatalf("failed successor lane pointers = %#v", store.v2.Lanes[0])
	}
	thirdPlan := codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}})
	third, err := runtimeLease.BeginRequest(thirdPlan)
	if err != nil {
		t.Fatal(err)
	}
	if third.record.PredecessorTurnHash != failed.identity.TurnDigest || third.record.PredecessorTurnHash == firstIdentity.TurnDigest {
		t.Fatalf("third turn resurrected the wrong predecessor: %#v", third.record)
	}
}

func TestCodexLeaseRuntimeRetriesIndeterminateSuccessorAsFullCreate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}})
	first, err := runtimeLease.BeginRequest(firstPlan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-1", HasResponseAnchor: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	incrementalPlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}})
	incrementalPlan.RequiresAccountContinuity = true
	incrementalPlan.Evidence.PreviousResponseID = "response-1"
	incremental, err := runtimeLease.BeginRequest(incrementalPlan)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err = incremental.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	indeterminate, err := incremental.Indeterminate()
	if err != nil {
		t.Fatal(err)
	}
	if indeterminate, err = indeterminate.Drain(); err != nil {
		t.Fatal(err)
	}

	fullCreatePlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}})
	fullCreatePlan.Accounts = []codex.AccountKey{"account-a", "account-b", "account-c"}
	fullCreatePlan.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity:         indeterminate.identity,
		AccountKey:       "account-a",
		RecordGeneration: indeterminate.record.RecordGeneration,
	}
	fullCreatePlan.RequiresAccountContinuity = true
	fullCreatePlan.authenticatedCallerContinuity = true
	retry, err := runtimeLease.BeginRequest(fullCreatePlan)
	if err != nil {
		t.Fatal(err)
	}
	if retry.State() != LeaseProvisional || retry.AccountKey() != "account-a" || retry.RequestGeneration() != 2 || !retry.record.NonMigratable {
		t.Fatalf("full-create retry = %#v", retry.record)
	}
}

func TestCodexLeaseRuntimeCreatesThirdSuccessorAfterDrainedChain(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))

	third, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	if third.State() != LeaseProvisional || third.AccountKey() != "account-a" || third.record.PredecessorTurnHash == "" {
		t.Fatalf("third successor = %#v", third.record)
	}
}

func TestCodexLeaseRuntimeRejectsSuccessorUntilPredecessorDrains(t *testing.T) {
	tests := []struct {
		name    string
		advance func(*testing.T, *CodexLeaseRequestHandle) *CodexLeaseRequestHandle
	}{
		{name: "prepared", advance: func(_ *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle { return handle }},
		{name: "observer remains", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			next, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			next, err = next.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			next, err = next.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			if err != nil {
				t.Fatal(err)
			}
			return next
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			first, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			_ = test.advance(t, first)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			_, err = runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
			if !errors.Is(err, ErrCodexConcurrentTurn) {
				t.Fatalf("successor before drain = %T %v, want concurrent turn", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("blocked successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeConcurrentSuccessorsHaveOneWinner(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	beforeGeneration := store.Generation()
	plans := []CodexLeaseRequestPlan{
		codexLeaseRuntimeTestPlan("turn-2a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2a", Kind: CodexAttemptSlotDirect}}),
		codexLeaseRuntimeTestPlan("turn-2b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2b", Kind: CodexAttemptSlotDirect}}),
	}
	start := make(chan struct{})
	results := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, len(plans))
	for _, plan := range plans {
		plan := plan
		go func() {
			<-start
			handle, err := runtimeLease.BeginRequest(plan)
			results <- struct {
				handle *CodexLeaseRequestHandle
				err    error
			}{handle: handle, err: err}
		}()
	}
	close(start)
	var winner *CodexLeaseRequestHandle
	for range plans {
		result := <-results
		if result.err == nil {
			if winner != nil {
				t.Fatal("both concurrent successors committed")
			}
			winner = result.handle
			continue
		}
		if !errors.Is(result.err, ErrCodexConcurrentTurn) && !errors.Is(result.err, ErrCodexLeaseStaleMutation) {
			t.Fatalf("losing successor error = %T %v", result.err, result.err)
		}
	}
	if winner == nil {
		t.Fatal("concurrent successors had no winner")
	}
	if store.Generation() != beforeGeneration+2 {
		t.Fatalf("concurrent successors advanced generation %d -> %d, want two commits", beforeGeneration, store.Generation())
	}
	if store.v2.Lanes[0].CurrentTurnHash != winner.identity.TurnDigest || store.v2.Lanes[0].LastTurnHash != winner.identity.TurnDigest {
		t.Fatalf("concurrent successor lane = %#v, winner %#v", store.v2.Lanes[0], winner.identity)
	}
}

func TestCodexLeaseRuntimeSuccessorAfterRestart(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
	second, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, first.identity)
	if predecessor.State != LeaseSuperseded || second.record.PredecessorTurnHash != first.identity.TurnDigest || second.record.PredecessorGeneration != predecessor.RecordGeneration {
		t.Fatalf("post-restart successor = predecessor %#v successor %#v", predecessor, second.record)
	}
}

func TestCodexLeaseRuntimeReservationCrashRequiresFreshSuccessor(t *testing.T) {
	t.Run("unseen reservation", func(t *testing.T) {
		coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		interrupted := codexLeaseRuntimeTestPlan("turn-interrupted", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-interrupted", Kind: CodexAttemptSlotDirect}})
		release, err := runtimeLease.beginAccountMutation(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := runtimeLease.store.LoadLane(interrupted.Key, interrupted.Accounts, interrupted.Authority)
		if err == nil {
			err = runtimeLease.reserveUnseen(restored)
		}
		release()
		if err != nil {
			t.Fatal(err)
		}
		interruptedIdentity := restored.RequestedIdentity
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
		reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
		reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
		if _, err := reopenedRuntime.BeginRequest(interrupted); !errors.Is(err, ErrCodexStaleTurn) {
			t.Fatalf("interrupted turn retry = %T %v, want stale turn", err, err)
		}
		fresh, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-fresh", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-fresh", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.record.PredecessorTurnHash != interruptedIdentity.TurnDigest {
			t.Fatalf("fresh successor skipped failed reservation: %#v", fresh.record)
		}
	})

	t.Run("successor reservation", func(t *testing.T) {
		coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
		interrupted := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}})
		release, err := runtimeLease.beginAccountMutation(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := runtimeLease.store.LoadLane(interrupted.Key, interrupted.Accounts, interrupted.Authority)
		if err == nil {
			err = runtimeLease.reserveSuccessor(context.Background(), "account-a", restored)
		}
		release()
		if err != nil {
			t.Fatal(err)
		}
		interruptedIdentity := restored.RequestedIdentity
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
		reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
		reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
		if _, err := reopenedRuntime.BeginRequest(interrupted); !errors.Is(err, ErrCodexStaleTurn) {
			t.Fatalf("interrupted successor retry = %T %v, want stale turn", err, err)
		}
		fresh, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.record.PredecessorTurnHash != interruptedIdentity.TurnDigest || fresh.record.PredecessorTurnHash == first.identity.TurnDigest {
			t.Fatalf("fresh successor resurrected pre-crash predecessor: %#v", fresh.record)
		}
	})
}

func TestCodexLeaseRuntimeSuccessorSurvivesPredecessorRetention(t *testing.T) {
	t.Parallel()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	second, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(25 * time.Hour)
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	for _, record := range store.v2.Records {
		if record.Identity() == first.identity {
			t.Fatalf("retention kept collectible predecessor: %#v", record)
		}
	}
	dispatched, err := second.MarkDispatched()
	if err != nil {
		t.Fatalf("retained predecessor fence staled live successor: %v", err)
	}
	if dispatched.State() != LeaseProvisional || codexLeaseCurrentAttemptState(dispatched.record) != CodexAttemptDispatched || dispatched.record.PredecessorGeneration != second.record.PredecessorGeneration {
		t.Fatalf("successor after predecessor retention = %#v", dispatched.record)
	}
}

func TestCodexLeaseRuntimeRejectedSuccessorAuthorityDoesNotSupersede(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	reject := false
	rejected := errors.New("successor account unavailable")
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		if reject {
			return rejected
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	reject = true
	_, err = runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if !errors.Is(err, rejected) {
		t.Fatalf("rejected successor = %T %v, want callback error", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.identity)
	if predecessor.State != LeaseBoundQuiescent || store.v2.Lanes[0].CurrentTurnHash != first.identity.TurnDigest {
		t.Fatalf("rejected successor superseded predecessor: %#v lane %#v", predecessor, store.v2.Lanes[0])
	}
}

func TestCodexLeaseRuntimeFinalRejectionAfterAdmissionPreservesAuthority(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.Drain()
	if err != nil {
		t.Fatal(err)
	}
	admissionGeneration := first.record.AdmissionJournalGeneration
	admissionRequest := first.record.AdmissionRequestGeneration
	admittedAt := first.record.AdmittedAt

	secondPlan := plan
	secondPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-second", Kind: CodexAttemptSlotDirect}}
	second, err := runtimeLease.BeginRequest(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := second.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State() != LeaseBoundQuiescent || !terminal.EverAdmitted() || terminal.AccountKey() != "account-a" || terminal.RequestGeneration() != 2 || codexLeaseCurrentAttemptState(terminal.record) != CodexAttemptProviderFailed {
		t.Fatalf("admitted final rejection = %#v", terminal.record)
	}
	if terminal.record.AdmissionJournalGeneration != admissionGeneration || terminal.record.AdmissionRequestGeneration != admissionRequest || !terminal.record.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("admission evidence changed after later rejection: %#v", terminal.record)
	}
	assertCodexLeaseRuntimeRefs(t, terminal, 0, 0, 0, true)

	crossAccount := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-cross", Kind: CodexAttemptSlotDirect}})
	crossAccount.Accounts = append(crossAccount.Accounts, "account-a")
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(crossAccount); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("cross-account request after admitted rejection = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cross-account request after rejection changed authority: poison %v", store.poisoned)
	}

	thirdPlan := plan
	thirdPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-third", Kind: CodexAttemptSlotDirect}}
	third, err := runtimeLease.BeginRequest(thirdPlan)
	if err != nil {
		t.Fatal(err)
	}
	if third.RequestGeneration() != 3 || third.State() != LeaseBoundActive || !third.EverAdmitted() || third.AccountKey() != "account-a" {
		t.Fatalf("request after admitted final rejection = %#v", third.record)
	}
}

func TestCodexLeaseRuntimeRejectsUnsupportedFirstRequestWithoutWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexLeaseRequestPlan)
		want   error
	}{
		{name: "raw prewarm", mutate: func(plan *CodexLeaseRequestPlan) { plan.RequestKind = CodexRequestPrewarm }, want: ErrCodexLeaseInvalidMutation},
		{name: "memory", mutate: func(plan *CodexLeaseRequestPlan) { plan.RequestKind = CodexRequestMemory }, want: ErrCodexLeaseInvalidMutation},
		{name: "mid-turn compaction before admission", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.RequestKind = CodexRequestCompaction
			plan.CompactionPhase = CodexCompactionMidTurn
		}, want: ErrCodexLeaseAuthorityMismatch},
		{name: "missing effective-model bucket", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.EffectiveModel = codexSparkModel
			plan.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
		}, want: ErrCodexLeaseInvalidMutation},
		{name: "untrimmed effective model", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.EffectiveModel = " gpt-effective "
		}, want: ErrCodexLeaseInvalidMutation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			test.mutate(&plan)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			if _, err := runtimeLease.BeginRequest(plan); !errors.Is(err, test.want) {
				t.Fatalf("BeginRequest error = %T %v, want %v", err, err, test.want)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected request changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeRequiresFreshAccountAuthorityBeforeWrites(t *testing.T) {
	if runtimeLease, err := NewCodexLeaseRuntime(nil, func(context.Context, codex.AccountKey) error { return nil }); runtimeLease != nil || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("nil coordinator constructor = (%#v, %T %v)", runtimeLease, err, err)
	}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	if runtimeLease, err := NewCodexLeaseRuntime(coordinator, nil); runtimeLease != nil || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("nil revalidator constructor = (%#v, %T %v)", runtimeLease, err, err)
	}

	rejected := errors.New("account authority rejected")
	tests := []struct {
		name        string
		wantAccount codex.AccountKey
		prepare     func(*testing.T, *CodexLeaseRuntime) func() error
	}{
		{name: "begin request", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			return func() error {
				_, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
				return err
			}
		}},
		{name: "mark dispatched", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.MarkDispatched(); return err }
		}},
		{name: "prepare retry", wantAccount: "account-b", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
				{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
				{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
			})
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.RejectAndPrepare(2); return err }
		}},
		{name: "admit 2xx", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.AdmitHTTP2xx(); return err }
		}},
		{name: "pre-admission indeterminate", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.Indeterminate(); return err }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			reject := false
			var revalidated codex.AccountKey
			runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(_ context.Context, account codex.AccountKey) error {
				revalidated = account
				if reject {
					return fmt.Errorf("removed account %s: %w", account, rejected)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			invoke := test.prepare(t, runtimeLease)
			revalidated = ""
			reject = true
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			err = invoke()
			if !errors.Is(err, rejected) {
				t.Fatalf("rejected operation error = %T %v, want callback error", err, err)
			}
			if strings.Contains(err.Error(), string(test.wantAccount)) {
				t.Fatalf("revalidation error exposed account identity: %v", err)
			}
			if revalidated != test.wantAccount {
				t.Fatalf("revalidated account = %q, want %q", revalidated, test.wantAccount)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected operation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeTerminalEvidenceDoesNotRequireRoutability(t *testing.T) {
	rejected := errors.New("account is no longer routable")
	t.Run("final rejection", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		beforeCalls := calls.Load()
		reject.Store(true)
		terminal, err := handle.FinishRejected()
		if err != nil {
			t.Fatal(err)
		}
		if terminal.State() != LeaseFailedUnadmitted || calls.Load() != beforeCalls {
			t.Fatalf("final rejection = state %v callbacks %d -> %d", terminal.State(), beforeCalls, calls.Load())
		}
	})

	t.Run("provider completion and drain", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
		beforeCalls := calls.Load()
		reject.Store(true)
		handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.Drain()
		if err != nil {
			t.Fatal(err)
		}
		if handle.State() != LeaseBoundQuiescent || calls.Load() != beforeCalls {
			t.Fatalf("terminal drain = state %v callbacks %d -> %d", handle.State(), beforeCalls, calls.Load())
		}
	})

	t.Run("admitted indeterminate", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Release()
		if summary.BoundCount != 1 {
			t.Fatalf("admitted removal summary = %+v", summary)
		}
		beforeCalls := calls.Load()
		reject.Store(true)
		uncertain, err := handle.Indeterminate()
		if err != nil {
			t.Fatal(err)
		}
		if uncertain.State() != LeaseOrphaned || calls.Load() != beforeCalls {
			t.Fatalf("admitted indeterminate = state %v callbacks %d -> %d", uncertain.State(), beforeCalls, calls.Load())
		}
	})
}

func TestCodexLeaseRuntimeMidTurnCompactionRequiresAdmittedTurn(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	request, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.Drain()
	if err != nil {
		t.Fatal(err)
	}

	compaction := plan
	compaction.RequestKind = CodexRequestCompaction
	compaction.CompactionPhase = CodexCompactionMidTurn
	compaction.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-compaction", Kind: CodexAttemptSlotDirect}}
	next, err := runtimeLease.BeginRequest(compaction)
	if err != nil {
		t.Fatal(err)
	}
	if next.RequestGeneration() != 2 || next.State() != LeaseBoundActive || !next.EverAdmitted() || next.record.RequestKind != CodexRequestCompaction || next.record.CompactionPhase != CodexCompactionMidTurn {
		t.Fatalf("mid-turn compaction request = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeAdmissionUsesAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("pre-admission removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := dispatched.AdmitHTTP2xxContext(ctx, CodexHTTPAdmissionEvidence{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gated admission error = %T %v, want deadline", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("blocked admission changed authority: poison %v", store.poisoned)
	}
	guard.Release()
	admitted, err := dispatched.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.EverAdmitted() || admitted.State() != LeaseBoundActive {
		t.Fatalf("released admission = %#v", admitted.record)
	}
}

func TestCodexLeaseRuntimeBeginAndDispatchUseAccountRemovalGate(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
		if _, err := runtimeLease.BeginRequestContext(ctx, plan); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gated begin = %T %v, want deadline", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) {
			t.Fatal("gated begin changed journal")
		}
		guard.Release()
		if _, err := runtimeLease.BeginRequest(plan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("dispatch", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := handle.MarkDispatchedContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gated dispatch = %T %v, want deadline", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) {
			t.Fatal("gated dispatch changed journal")
		}
		guard.Release()
		if _, err := handle.MarkDispatched(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCodexLeaseRuntimeCancellationReleasesGateWhilePersistenceBusy(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	coordinator.leases.lifecycle.persistence.Lock()
	locked := true
	defer func() {
		if locked {
			coordinator.leases.lifecycle.persistence.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runtimeLease.BeginRequestContext(ctx, codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		result <- err
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 1)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled persistence waiter = %T %v, want context canceled", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled persistence waiter retained the account gate")
	}
	coordinator.leases.accountGates.mu.Lock()
	_, retained := coordinator.leases.accountGates.entries["account-a"]
	coordinator.leases.accountGates.mu.Unlock()
	if retained {
		t.Fatal("cancelled persistence waiter retained its account identity")
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled persistence waiter changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	coordinator.leases.lifecycle.persistence.Unlock()
	locked = false
}

func TestCodexLeaseRuntimeAdmittedIndeterminateHonoursCancellationWhilePersistenceBusy(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	coordinator.leases.lifecycle.persistence.Lock()
	locked := true
	defer func() {
		if locked {
			coordinator.leases.lifecycle.persistence.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := handle.IndeterminateContext(ctx, CodexHTTPResponseEvidence{})
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admitted indeterminate = %T %v, want context canceled", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted indeterminate ignored cancellation while persistence was busy")
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled admitted indeterminate changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	coordinator.leases.lifecycle.persistence.Unlock()
	locked = false
}

func TestCodexLeaseRuntimeRevalidatesAfterWaitingForRemoval(t *testing.T) {
	rejected := errors.New("account removal completed")
	t.Run("admission", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		var removed atomic.Bool
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			if removed.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		result := make(chan error, 1)
		go func() { _, transitionErr := handle.AdmitHTTP2xx(); result <- transitionErr }()
		waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 2)
		removed.Store(true)
		guard.Release()
		if err := <-result; !errors.Is(err, rejected) {
			t.Fatalf("admission after removal = %T %v, want callback error", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
			t.Fatalf("rejected admission changed authority: poison %v", store.poisoned)
		}
	})

	t.Run("retry", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		var removed atomic.Bool
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(_ context.Context, account codex.AccountKey) error {
			if removed.Load() && account == "account-b" {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
			{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
			{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
		})
		handle, err := runtimeLease.BeginRequest(plan)
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		result := make(chan error, 1)
		go func() { _, transitionErr := handle.RejectAndPrepare(2); result <- transitionErr }()
		waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
		removed.Store(true)
		guard.Release()
		if err := <-result; !errors.Is(err, rejected) {
			t.Fatalf("retry after removal = %T %v, want callback error", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
			t.Fatalf("rejected retry changed authority: poison %v", store.poisoned)
		}
	})
}

func TestCodexLeaseRuntimeDetachesPlanAndPersistsNoRawIdentity(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("private-turn-runtime", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "private-account-runtime",
		CandidateID: "private-candidate-runtime",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.Key.Lane.Session = "private-session-runtime"
	plan.Key.Lane.Thread = "private-thread-runtime"
	plan.RequestedModel = "private-requested-model-runtime"
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Accounts[0] = "mutated-account"
	plan.RequiredBuckets[0] = CapacityBucket("mutated-bucket")
	plan.Slots[0].AccountKey = "mutated-slot-account"
	plan.Slots[0].CandidateID = "mutated-candidate"
	if prepared.AccountKey() != "private-account-runtime" || prepared.slotAccounts[0] != "private-account-runtime" || prepared.record.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("runtime plan aliases caller memory: account %q slots %#v buckets %#v", prepared.AccountKey(), prepared.slotAccounts, prepared.record.RequiredBuckets)
	}
	for _, raw := range []string{
		"private-session-runtime",
		"private-thread-runtime",
		"private-turn-runtime",
		"private-account-runtime",
		"private-candidate-runtime",
		"private-requested-model-runtime",
	} {
		if bytes.Contains(store.journalBytes, []byte(raw)) {
			t.Fatalf("journal persisted raw private value %q", raw)
		}
	}
}

func TestCodexLeaseV2CASNarrowsProvisionalUncertainty(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2)
		valid  bool
	}{
		{name: "exact dispatched after-image", valid: true},
		{name: "routing ref remains", mutate: func(record *CodexJournalRecordV2) { record.RoutingRefs = 1 }},
		{name: "attempt ref remains", mutate: func(record *CodexJournalRecordV2) { record.AttemptRefs = 1 }},
		{name: "observer ownership missing", mutate: func(record *CodexJournalRecordV2) { record.ResponseObserverRefs = 0 }},
		{name: "lineage prematurely extinct", mutate: func(record *CodexJournalRecordV2) { record.SocketLineageExtinct = true }},
		{name: "attempt is definitely failed", mutate: func(record *CodexJournalRecordV2) { record.Attempts[0].State = CodexAttemptProviderFailed }},
		{name: "account changes", mutate: func(record *CodexJournalRecordV2) { record.AccountHash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }},
		{name: "encrypted state appears", mutate: func(record *CodexJournalRecordV2) { record.HasEncryptedState = true }},
		{name: "socket generation changes", mutate: func(record *CodexJournalRecordV2) { record.UpstreamSocketGeneration++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			prepared, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			dispatched, err := prepared.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			desired := codexLeaseRuntimeMutationRecord(dispatched.record)
			desired.State = LeaseOrphaned
			desired.RoutingRefs = 0
			desired.AttemptRefs = 0
			desired.ResponseObserverRefs = 1
			desired.Attempts[0].State = CodexAttemptIndeterminate
			if test.mutate != nil {
				test.mutate(&desired)
			}
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			_, err = store.CommitLane(dispatched.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}})
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, dispatched.identity)
				if stored.State != LeaseOrphaned || !stored.NonMigratable || codexLeaseCurrentAttemptState(stored) != CodexAttemptIndeterminate {
					t.Fatalf("exact uncertainty = %#v", stored)
				}
				return
			}
			if !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("narrow uncertainty error = %T %v, want invalid mutation", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected uncertainty changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CASRejectsUncertaintyWithHistoricalAttemptFence(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	desired.State = LeaseOrphaned
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 1
	desired.Attempts[1].State = CodexAttemptIndeterminate
	fence := cloneCodexLeaseGenerationFence(handle.fence)
	recordFence, ok := codexLeaseRuntimeRecordFence(&fence, handle.identity)
	if !ok {
		t.Fatal("current record fence is absent")
	}
	recordFence.TouchedAttempts = append(recordFence.TouchedAttempts, CodexAttemptFence{
		RequestGeneration: handle.record.Generation,
		Generation:        handle.record.Attempts[0].Generation,
		Revision:          handle.record.Attempts[0].Revision,
	})
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("uncertainty with historical attempt fence = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected uncertainty changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CASRejectsAbandonmentThatRegressesCurrentAttempt(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	desired.Attempts[1].State = CodexAttemptAbandonedBeforeDispatch
	desired.CurrentAttemptGeneration = desired.Attempts[0].Generation
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(handle.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) || !strings.Contains(err.Error(), "prepared abandonment lacks an exact terminal after-image") {
		t.Fatalf("regressed abandonment = %T %v, want exact abandonment rejection", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CASRequiresSoleNarrowTerminalMutation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	current, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	current, err = current.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(current.record)
	desired.State = LeaseOrphaned
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 1
	desired.Attempts[0].State = CodexAttemptIndeterminate
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.identity)
	predecessorNoOp := codexLeaseRuntimeMutationRecord(predecessor)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(current.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{predecessorNoOp, desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("bundled narrow terminal mutation = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("bundled narrow mutation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseRuntimeStaleRequestFenceDoesNotWrite(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	prepared, err := runtime.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	stale := prepared
	if _, err := prepared.MarkDispatched(); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := stale.MarkDispatched(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale request fence error = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale request fence changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseRuntimeUnrelatedLaneCommitDoesNotStaleHandle(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planA := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	preparedA, err := runtimeLease.BeginRequest(planA)
	if err != nil {
		t.Fatal(err)
	}
	planB := codexLeaseRuntimeTestPlan("turn-b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}})
	planB.Key.Lane.Thread = "runtime-thread-b"
	if _, err := runtimeLease.BeginRequest(planB); err != nil {
		t.Fatal(err)
	}
	dispatchedA, err := preparedA.MarkDispatched()
	if err != nil {
		t.Fatalf("unrelated lane commit staled handle: %v", err)
	}
	if dispatchedA.record.Attempts[0].State != CodexAttemptDispatched {
		t.Fatalf("dispatched attempt = %#v", dispatchedA.record.Attempts[0])
	}
}

func TestCodexLeaseRuntimeRejectAndPrepareUsesNextAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	beforeReused := append([]byte(nil), store.journalBytes...)
	beforeReusedGeneration := store.Generation()
	if _, err := dispatched.RejectAndPrepare(1); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("reused-slot retry error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeReusedGeneration || !bytes.Equal(beforeReused, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("reused-slot retry changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("unadmitted removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := dispatched.RejectAndPrepareContext(ctx, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gated retry error = %T %v, want deadline", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("blocked retry changed authority: poison %v", store.poisoned)
	}
	guard.Release()

	retry, err := dispatched.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AccountKey() != "account-b" || retry.AttemptGeneration() != 2 || retry.record.Attempts[0].State != CodexAttemptProviderFailed || retry.record.Attempts[1].State != CodexAttemptPrepared || retry.EverAdmitted() || retry.record.NonMigratable {
		t.Fatalf("retry handle = account %q request %#v", retry.AccountKey(), retry.record.CodexCurrentRequest)
	}
	assertCodexLeaseRuntimeRefs(t, retry, 1, 0, 0, false)

	before = append([]byte(nil), store.journalBytes...)
	if _, err := retry.RejectAndPrepare(0); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("zero-slot retry error = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("zero-slot retry changed journal")
	}
	retryDispatched, err := retry.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := retryDispatched.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State() != LeaseFailedUnadmitted || terminal.EverAdmitted() || !terminal.record.SocketLineageExtinct || store.v2.Lanes[0].CurrentTurnHash != "" {
		t.Fatalf("terminal rejection = state %v record %#v lane %#v", terminal.State(), terminal.record, store.v2.Lanes[0])
	}

	before = append([]byte(nil), store.journalBytes...)
	if _, err := dispatched.Indeterminate(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale-head callback error = %T %v, want stale mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale-head callback changed authority: poison %v", store.poisoned)
	}
}

func TestCodexLeaseRuntimeRetrySerialisesWithConcurrentAccountRemoval(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	result := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, 1)
	go func() {
		handle, retryErr := dispatched.RejectAndPrepare(2)
		result <- struct {
			handle *CodexLeaseRequestHandle
			err    error
		}{handle: handle, err: retryErr}
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
	select {
	case outcome := <-result:
		t.Fatalf("retry escaped held removal gate: handle %#v error %v", outcome.handle, outcome.err)
	default:
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked concurrent retry changed journal")
	}
	guard.Release()
	outcome := <-result
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.handle.AccountKey() != "account-b" || outcome.handle.AttemptGeneration() != 2 {
		t.Fatalf("released concurrent retry = %#v", outcome.handle.record)
	}
}

func TestCodexLeaseRuntimeCloseWakesRetryBehindRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	before := fsysFileBytes(t, fsys, "/state/leases.json")
	result := make(chan error, 1)
	go func() {
		_, retryErr := dispatched.RejectAndPrepare(2)
		result <- retryErr
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("retry after Close = %T %v, want writer unavailable", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry remained blocked after Close")
	}
	if !bytes.Equal(before, fsysFileBytes(t, fsys, "/state/leases.json")) {
		t.Fatal("Close-woken retry changed durable journal")
	}
	coordinator.leases.accountGates.mu.Lock()
	remaining := len(coordinator.leases.accountGates.entries)
	coordinator.leases.accountGates.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("closed account gate set retained %d identities", remaining)
	}
	guard.Release()
}

func TestCodexLeaseRuntimeIndeterminateUsesCurrentAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("pre-admission removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	result := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, 1)
	go func() {
		handle, transitionErr := dispatched.Indeterminate()
		result <- struct {
			handle *CodexLeaseRequestHandle
			err    error
		}{handle: handle, err: transitionErr}
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 2)
	select {
	case outcome := <-result:
		t.Fatalf("indeterminate escaped held removal gate: handle %#v error %v", outcome.handle, outcome.err)
	default:
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked indeterminate changed journal")
	}
	guard.Release()
	outcome := <-result
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.handle.State() != LeaseOrphaned || !outcome.handle.record.NonMigratable {
		t.Fatalf("released indeterminate = %#v", outcome.handle.record)
	}
}

func TestCodexLeaseRuntimeIndeterminatePinsAccountAndDrains(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	uncertain, err := dispatched.Indeterminate()
	if err != nil {
		t.Fatal(err)
	}
	if uncertain.State() != LeaseOrphaned || uncertain.EverAdmitted() || !uncertain.record.NonMigratable || uncertain.record.Attempts[0].State != CodexAttemptIndeterminate {
		t.Fatalf("uncertain request = %#v", uncertain.record)
	}
	assertCodexLeaseRuntimeRefs(t, uncertain, 0, 0, 1, false)
	same := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a-next", Kind: CodexAttemptSlotDirect}})
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(same); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("request before uncertain drain = %T %v, want concurrent", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked uncertain request changed journal")
	}
	drained, err := uncertain.Drain()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)

	other := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b-next", Kind: CodexAttemptSlotDirect}})
	other.Accounts = append(other.Accounts, "account-a")
	before = append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(other); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("cross-account uncertain request = %T %v, want continuity error", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cross-account uncertain request changed authority: poison %v", store.poisoned)
	}
	next, err := runtimeLease.BeginRequest(same)
	if err != nil {
		t.Fatal(err)
	}
	if next.AccountKey() != "account-a" || next.RequestGeneration() != 2 || next.State() != LeaseProvisional || !next.record.NonMigratable {
		t.Fatalf("same-account uncertain recovery = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeRestartNormalisesAndFencesOldHandles(t *testing.T) {
	tests := []struct {
		name           string
		advance        func(*testing.T, *CodexLeaseRequestHandle) *CodexLeaseRequestHandle
		wantLease      LeaseState
		wantAttempt    CodexAttemptState
		wantAdmitted   bool
		wantPinned     bool
		requestAccount codex.AccountKey
	}{
		{name: "prepared", advance: func(_ *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle { return handle }, wantLease: LeaseProvisional, wantAttempt: CodexAttemptAbandonedBeforeDispatch, requestAccount: "account-b"},
		{name: "dispatched", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			next, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return next
		}, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantPinned: true, requestAccount: "account-a"},
		{name: "streaming", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			dispatched, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			next, err := dispatched.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			return next
		}, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantAdmitted: true, wantPinned: true, requestAccount: "account-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
				{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
				{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
			})
			prepared, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			oldHandle := test.advance(t, prepared)
			identity := oldHandle.identity
			beforeClose := append([]byte(nil), coordinator.Store().journalBytes...)
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := oldHandle.Indeterminate(); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
				t.Fatalf("old handle after Close = %T %v, want writer unavailable", err, err)
			}
			if !bytes.Equal(beforeClose, fsysFileBytes(t, fsys, "/state/leases.json")) {
				t.Fatal("closed old handle changed journal")
			}
			*now = now.Add(time.Minute)
			reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
			restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
			if restored.State != test.wantLease || codexLeaseCurrentAttemptState(restored) != test.wantAttempt || restored.EverAdmitted != test.wantAdmitted || restored.NonMigratable != test.wantPinned || !restored.SocketLineageExtinct || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.ResponseObserverRefs != 0 {
				t.Fatalf("restart state = %#v", restored)
			}
			nextPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: test.requestAccount, CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}})
			next, err := newCodexLeaseRuntimeTest(t, reopened).BeginRequest(nextPlan)
			if err != nil {
				t.Fatal(err)
			}
			if next.AccountKey() != test.requestAccount || next.RequestGeneration() != 2 || next.EverAdmitted() != test.wantAdmitted {
				t.Fatalf("post-restart request = %#v", next.record)
			}
		})
	}
}

func codexLeaseRuntimeTestPlan(turn string, slots []CodexLeaseAttemptSlotPlan) CodexLeaseRequestPlan {
	accounts := make([]codex.AccountKey, 0, len(slots))
	seen := make(map[codex.AccountKey]struct{}, len(slots))
	for _, slot := range slots {
		if _, ok := seen[slot.AccountKey]; ok {
			continue
		}
		seen[slot.AccountKey] = struct{}{}
		accounts = append(accounts, slot.AccountKey)
	}
	return CodexLeaseRequestPlan{
		Key: LeaseKey{
			Lane: LaneKey{Session: "runtime-session", Thread: "runtime-thread", Namespace: CodexResponsesNamespace},
			Turn: turn,
		},
		Accounts:       accounts,
		Authority:      CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		RequestKind:    CodexRequestTurn,
		RequestedModel: "requested-model-private",
		EffectiveModel: "gpt-effective",
		RequiredBuckets: []CapacityBucket{
			CapacityBucketBase,
		},
		Slots:       slots,
		InitialSlot: 1,
	}
}

func newCodexLeaseRuntimeTest(t *testing.T, coordinator *CodexContinuityCoordinator) *CodexLeaseRuntime {
	t.Helper()
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return runtimeLease
}

func completeCodexLeaseRuntimeTurn(t *testing.T, runtimeLease *CodexLeaseRuntime, plan CodexLeaseRequestPlan) *CodexLeaseRequestHandle {
	t.Helper()
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
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func assertCodexLeaseRuntimeRefs(t *testing.T, handle *CodexLeaseRequestHandle, routing, attempts, observers int, extinct bool) {
	t.Helper()
	if handle.record.RoutingRefs != routing || handle.record.AttemptRefs != attempts || handle.record.ResponseObserverRefs != observers || handle.record.SocketLineageExtinct != extinct {
		t.Fatalf("runtime refs = routing %d attempts %d observers %d extinct %v, want %d/%d/%d/%v", handle.record.RoutingRefs, handle.record.AttemptRefs, handle.record.ResponseObserverRefs, handle.record.SocketLineageExtinct, routing, attempts, observers, extinct)
	}
}

func openCodexLeaseRuntimeTestCoordinator(t *testing.T) (*CodexContinuityCoordinator, *fsutil.MemFS, *time.Time) {
	return openCodexLeaseRuntimeTestCoordinatorWithOwner(t, codexLeaseV2CASTestOwner{})
}

func openCodexLeaseRuntimeTestCoordinatorWithOwner(t *testing.T, owner CodexLeaseWriterAuthority) (*CodexContinuityCoordinator, *fsutil.MemFS, *time.Time) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, codexLeaseHMACKeyBytes)
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	cutoverAt := now.Add(-time.Hour)
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV3,
		HashVersion: codexLeaseHashVersion,
		Generation:  1,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   4,
			State:                CodexLeaseCutoverComplete,
			At:                   cutoverAt,
			JournalGeneration:    1,
			CompletedAt:          cutoverAt,
			CompletionGeneration: 1,
			NoLegacyAuthority:    true,
		},
		Lanes:   []CodexJournalLane{},
		Records: []CodexJournalRecordV2{},
	}
	payload := codexLeaseV2CASTestEnvelopePayload(t, key, envelope)
	if err := fsys.WriteFile("/state/leases.json", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator, fsys, &now
}

type codexLeaseRuntimeFailingOwner struct {
	begins atomic.Int32
	failAt atomic.Int32
	err    error
}

func (*codexLeaseRuntimeFailingOwner) AssertOwner() error { return nil }

func (owner *codexLeaseRuntimeFailingOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	begin := owner.begins.Add(1)
	if failAt := owner.failAt.Load(); failAt != 0 && begin == failAt {
		return nil, owner.err
	}
	return &codex.CredentialOwnerOperation{}, nil
}

func reopenCodexLeaseRuntimeTestCoordinator(t *testing.T, fsys *fsutil.MemFS, now *time.Time) *CodexContinuityCoordinator {
	t.Helper()
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return *now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}},
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}

func fsysFileBytes(t *testing.T, fsys *fsutil.MemFS, path string) []byte {
	t.Helper()
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func waitCodexLeaseRuntimeGateRefs(t *testing.T, gates *codexAccountGateSet, account codex.AccountKey, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gates.mu.Lock()
		entry := gates.entries[account]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		gates.mu.Unlock()
		if refs == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("account gate %q did not reach %d references", account, want)
}
