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

	current, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, next.Accounts, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if current.BoundAccountKey != "account-a" || current.BoundRequiresAccount || !current.AffinityInvalidated {
		t.Fatalf("portable current binding after invalidation = account %q required %v invalidated %v", current.BoundAccountKey, current.BoundRequiresAccount, current.AffinityInvalidated)
	}
	current = codexHTTPRequestDetachInvalidatedPortableRoute(current, CodexProtocolRequest{}, nil)
	if current.BoundAccountKey != "" {
		t.Fatalf("portable current route after invalidation = account %q", current.BoundAccountKey)
	}
	migrated := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect,
	}})
	migrated.Accounts = []codex.AccountKey{"account-a", "account-b"}
	migratedHandle, err := runtimeLease.BeginRequest(migrated)
	if err != nil {
		t.Fatalf("portable current request after invalidation: %v", err)
	}
	if migratedHandle.AccountKey() != "account-b" {
		t.Fatalf("portable current account after invalidation = %q, want account-b", migratedHandle.AccountKey())
	}
	migratedHandle, err = migratedHandle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	migratedHandle, err = migratedHandle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	migratedHandle, err = migratedHandle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migratedHandle.Drain(); err != nil {
		t.Fatal(err)
	}
	admitted, err := coordinator.LoadRouteSnapshot(context.Background(), next.Key, next.Accounts, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.AffinityAccountKey != "account-b" || admitted.BoundAccountKey != "" || admitted.AffinityInvalidated {
		t.Fatalf("admitted affinity = account %q bound %q invalidated %v", admitted.AffinityAccountKey, admitted.BoundAccountKey, admitted.AffinityInvalidated)
	}

	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	restored, err := reopened.LoadRouteSnapshot(context.Background(), next.Key, []codex.AccountKey{"account-a", "account-b"}, next.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AffinityAccountKey != "account-b" || restored.BoundAccountKey != "" || restored.AffinityRequiresAccount || restored.AffinityInvalidated {
		t.Fatalf("restored affinity = account %q bound %q required %v invalidated %v", restored.AffinityAccountKey, restored.BoundAccountKey, restored.AffinityRequiresAccount, restored.AffinityInvalidated)
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
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "turn-state-1", HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-1", HasResponseAnchor: true},
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
	if snapshot.AffinityAccountKey != "account-a" || !snapshot.AffinityRequiresAccount || snapshot.AffinityInvalidated {
		t.Fatalf("required affinity after invalidation = account %q required %v invalidated %v", snapshot.AffinityAccountKey, snapshot.AffinityRequiresAccount, snapshot.AffinityInvalidated)
	}
}

func TestCodexLeaseAffinityInvalidationReleasesLegacyEncryptedBinding(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	plan.RequiresAccountContinuity = true
	plan.Evidence.HasEncryptedState = true
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
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if handle, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}
	if !handle.record.NonMigratable || !handle.record.HasEncryptedState {
		t.Fatalf("legacy encrypted binding = non-migratable %t encrypted %t, want true/true", handle.record.NonMigratable, handle.record.HasEncryptedState)
	}

	result, err := coordinator.InvalidateTaskAffinities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.InvalidatedLeases != 1 {
		t.Fatalf("invalidated legacy encrypted leases = %d, want 1", result.InvalidatedLeases)
	}

	migrated := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect,
	}})
	migrated.Accounts = []codex.AccountKey{"account-a", "account-b"}
	migrated.Evidence.HasEncryptedState = true
	migratedHandle, err := runtimeLease.BeginRequest(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if migratedHandle.AccountKey() != "account-b" || migratedHandle.record.NonMigratable {
		t.Fatalf("migrated legacy encrypted binding = account %q non-migratable %t, want account-b/false", migratedHandle.AccountKey(), migratedHandle.record.NonMigratable)
	}
}
