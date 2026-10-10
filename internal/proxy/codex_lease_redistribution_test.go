package proxy

import (
	"context"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexRedistributionSchedulesLatchedChatWithoutInterruptingStream(t *testing.T) {
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "old-state", HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	// Missing scheduling is the regression: required continuity must still be queued.
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ScheduledLeases != 1 || result.DeferredLeases != 1 {
		t.Fatalf("result = %+v, want one scheduled and deferred", result)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "old-response", HasResponseAnchor: true}})
	if err != nil {
		t.Fatalf("scheduled redistribution interrupted stream: %v", err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	snapshot, err := reopened.LoadRouteSnapshot(context.Background(), plan.Key, []codex.AccountKey{"account-a", "account-b"}, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RedistributionGeneration == 0 || snapshot.BoundAccountKey != "account-a" {
		t.Fatalf("pending redistribution lost after restart: %+v", snapshot)
	}
}

func TestCodexRedistributionRebindsLatchedCurrentRequestOnlyAtAdmission(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "old-state", HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coordinator.RedistributeTaskAffinities(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "old-response", HasResponseAnchor: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	plan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	plan.RedistributionGeneration = snapshot.RedistributionGeneration
	moved, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatalf("drained full request did not migrate: %v", err)
	}
	snapshot, err = coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AffinityAccountKey != "account-a" || snapshot.RedistributionGeneration == 0 {
		t.Fatalf("binding switched before replacement admission: %+v", snapshot)
	}
	moved, err = moved.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "new-state", HasTurnState: true})
	if err != nil {
		t.Fatalf("replacement admission: %v", err)
	}
	moved, err = moved.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "new-response", HasResponseAnchor: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = moved.Drain(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BoundAccountKey != "account-b" || snapshot.RedistributionGeneration != 0 {
		t.Fatalf("replacement admission did not consume redistribution: %+v", snapshot)
	}
	stale, err := runtimeLease.reboundTurnState(context.Background(), plan.Key, plan.Authority, "old-state")
	if err != nil || !stale {
		t.Fatalf("displaced turn state recognised = %v, error %v", stale, err)
	}
}

func TestCodexRedistributionResetEventIsDurablyDeduplicated(t *testing.T) {
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
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
	if _, err = coordinator.RedistributeTaskAffinitiesForReset(context.Background(), "account-b", "event-1"); err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	result, err := reopened.RedistributeTaskAffinitiesForReset(context.Background(), "account-b", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.ScheduledLeases != 0 {
		t.Fatalf("duplicate reset scheduled %d leases", result.ScheduledLeases)
	}
}

func TestCodexRedistributionDuringReplacementKeepsNewerRequestPending(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
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
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	first, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	plan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	plan.RedistributionGeneration = first.JournalGeneration
	moved, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.JournalGeneration <= first.JournalGeneration {
		t.Fatal("new redistribution coalesced into already dispatched replacement")
	}
	moved, err = moved.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = moved.Drain(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RedistributionGeneration != second.JournalGeneration {
		t.Fatalf("new event consumed by old admission: %+v", snapshot)
	}
}

func TestCodexRedistributionSchedulesFirstDispatchedRequestBeforeAdmission(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ScheduledLeases != 1 {
		t.Fatalf("first active chat omitted: %+v", result)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatalf("original request disturbed: %v", err)
	}
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RedistributionGeneration == 0 {
		t.Fatal("old request admission consumed pending redistribution")
	}
}

func TestCodexRedistributionRejectsReplacementWhileOldResponseIsInFlight(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
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
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	plan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	plan.RedistributionGeneration = result.JournalGeneration
	if _, err = runtimeLease.BeginRequest(plan); err == nil {
		t.Fatal("replacement allowed before old response drained")
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatalf("rejected replacement disturbed stream: %v", err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRedistributionCASRejectsForgedRequestProvenance(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
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
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := handle.refreshMutationFence()
	if err != nil {
		t.Fatal(err)
	}
	forged := codexLeaseRuntimeMutationRecord(handle.record)
	forged.RedistributionGeneration = result.JournalGeneration
	if _, err = coordinator.store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{forged}}); err == nil {
		t.Fatal("CAS accepted redistribution provenance outside BeginRequest")
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatalf("rejected CAS changed request: %v", err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRedistributionPreservesPortableEncryptedReplay(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
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
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true}, EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	plan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	plan.Evidence.HasEncryptedState = true
	plan.RedistributionGeneration = result.JournalGeneration
	moved, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if moved.AccountKey() != "account-b" || !moved.record.HasEncryptedState {
		t.Fatalf("encrypted replay lost or binding not moved: %+v", moved.record)
	}
}

func TestCodexRedistributionAfterFirstRequestDefiniteFailureAllowsFreshCreate(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.FinishRejected(); err != nil {
		t.Fatal(err)
	}
	plan.Accounts = []codex.AccountKey{"account-a", "account-b"}
	plan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	plan.RedistributionGeneration = result.JournalGeneration
	moved, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatalf("pending redistribution blocked failed unadmitted retry: %v", err)
	}
	moved, err = moved.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if moved.AccountKey() != "account-b" {
		t.Fatalf("replacement account = %s", moved.AccountKey())
	}
}

func TestCodexRedistributionRetiresCompletedAdoptedPrewarmAtFullCreate(t *testing.T) {
	coordinator, _, _, _, adoption := prepareCodexPrewarmAdoptionTest(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	accounts := []codex.AccountKey{"account-raw", "account-b"}
	handle, err := runtimeLease.adoptWebSocketPrewarmContext(context.Background(), accounts, adoption)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{DownstreamGeneration: adoption.DownstreamSocketGeneration, UpstreamGeneration: adoption.UpstreamSocketGeneration, ResponseID: adoption.ResponseAnchor, ResponseCreated: true, TurnState: adoption.TurnState, HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RedistributeTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := codexLeaseRuntimeTestPlan("turn-raw", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}})
	plan.Key, plan.Authority, plan.Accounts = adoption.Key, adoption.Policy, accounts
	plan.RequestedModel, plan.EffectiveModel, plan.RequiredBuckets = adoption.Choice.RequestedModel, adoption.Choice.EffectiveModel, adoption.Choice.RequiredBuckets
	plan.RedistributionGeneration = result.JournalGeneration
	moved, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatalf("completed adopted request blocked full-history boundary: %v", err)
	}
	if moved.record.AdoptedPrewarm || moved.record.PrewarmAdoptionJournalGeneration != 0 {
		t.Fatal("replacement retained old prewarm anchor")
	}
	moved, err = moved.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	moved, err = moved.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if moved.AccountKey() != "account-b" {
		t.Fatalf("replacement account = %s", moved.AccountKey())
	}
}
