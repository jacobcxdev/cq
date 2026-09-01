package proxy

import (
	"context"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseAffinityInvalidationPreservesActiveRequestAndSurvivesRestart(t *testing.T) {
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{
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

	next := codexLeaseRuntimeTestPlan("turn-2", plan.Slots)
	before, err := coordinator.LoadRouteSnapshot(context.Background(), next.Key, next.Accounts, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if before.AffinityAccountKey != "account-a" || before.AffinityRequiresAccount {
		t.Fatalf("portable affinity before invalidation = account %q required %v", before.AffinityAccountKey, before.AffinityRequiresAccount)
	}

	result, err := coordinator.InvalidateTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.InvalidatedLeases != 1 || result.JournalGeneration <= before.JournalGeneration {
		t.Fatalf("invalidation result = %#v, previous generation %d", result, before.JournalGeneration)
	}
	after, err := coordinator.LoadRouteSnapshot(context.Background(), next.Key, next.Accounts, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !after.AffinityPresent || after.AffinityAccountKey != "" || after.AffinityRequiresAccount {
		t.Fatalf("portable affinity after invalidation = present %v account %q required %v", after.AffinityPresent, after.AffinityAccountKey, after.AffinityRequiresAccount)
	}
	preferred, required, err := codexHTTPRequestTaskAffinityAccounts(after, CodexProtocolRequest{}, *now)
	if err != nil || preferred != "" || required != "" {
		t.Fatalf("post-invalidation affinity choice = preferred %q required %q error %v", preferred, required, err)
	}

	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatalf("active request completion after invalidation: %v", err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatalf("active request drain after invalidation: %v", err)
	}

	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	restored, err := reopened.LoadRouteSnapshot(context.Background(), next.Key, []codex.AccountKey{"account-a", "account-b"}, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AffinityAccountKey != "" || restored.AffinityRequiresAccount {
		t.Fatalf("restored affinity = account %q required %v", restored.AffinityAccountKey, restored.AffinityRequiresAccount)
	}
}

func TestCodexLeaseAffinityInvalidationPreservesRequiredContinuity(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
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
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{
			ResponseAnchor: "response-1", HasResponseAnchor: true, HasEncryptedState: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Drain(); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.InvalidateTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.InvalidatedLeases != 0 {
		t.Fatalf("required continuity invalidated leases = %d, want 0", result.InvalidatedLeases)
	}

	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AffinityAccountKey != "account-a" || !snapshot.AffinityRequiresAccount {
		t.Fatalf("required affinity after invalidation = account %q required %v", snapshot.AffinityAccountKey, snapshot.AffinityRequiresAccount)
	}
}
