package proxy

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseRouteSnapshotReturnsDetachedGenerationFencedRouteState(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)

	target := codexLeaseRuntimeTestPlan("target", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "target-a", Kind: CodexAttemptSlotDirect}})
	targetHandle, err := runtimeLease.BeginRequest(target)
	if err != nil {
		t.Fatal(err)
	}
	targetHandle, err = targetHandle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	targetHandle, err = targetHandle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}

	prepared := codexLeaseRuntimeTestPlan("prepared", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "prepared-a", Kind: CodexAttemptSlotDirect}})
	prepared.Key.Lane.Thread = "prepared-thread"
	if _, err := runtimeLease.BeginRequest(prepared); err != nil {
		t.Fatal(err)
	}

	dispatched := codexLeaseRuntimeTestPlan("dispatched", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "dispatched-b", Kind: CodexAttemptSlotDirect}})
	dispatched.Key.Lane.Thread = "dispatched-thread"
	dispatchedHandle, err := runtimeLease.BeginRequest(dispatched)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchedHandle.MarkDispatched(); err != nil {
		t.Fatal(err)
	}

	abandoned := codexLeaseRuntimeTestPlan("abandoned", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-c", CandidateID: "abandoned-c", Kind: CodexAttemptSlotDirect}})
	abandoned.Key.Lane.Thread = "abandoned-thread"
	abandonedHandle, err := runtimeLease.BeginRequest(abandoned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := abandonedHandle.AbandonBeforeDispatch(); err != nil {
		t.Fatal(err)
	}

	accounts := []codex.AccountKey{"account-a", "account-b", "account-c"}
	owner := &countingCodexLeaseTestOwner{}
	coordinator.store.mu.Lock()
	coordinator.store.owner = owner
	coordinator.store.mu.Unlock()
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), target.Key, accounts, target.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BoundAccountKey != "account-a" || snapshot.AffinityAccountKey != "account-a" {
		t.Fatalf("route accounts = bound %q affinity %q, want account-a/account-a", snapshot.BoundAccountKey, snapshot.AffinityAccountKey)
	}
	if snapshot.AffinityCacheAdmittedAt != targetHandle.record.AdmittedAt {
		t.Fatalf("affinity cache admitted at = %v, want %v", snapshot.AffinityCacheAdmittedAt, targetHandle.record.AdmittedAt)
	}
	if snapshot.AffinityEffectiveModel != targetHandle.record.EffectiveModel {
		t.Fatalf("affinity effective model = %q, want %q", snapshot.AffinityEffectiveModel, targetHandle.record.EffectiveModel)
	}
	if snapshot.BoundIdentity != targetHandle.identity || snapshot.BoundRecordGeneration != targetHandle.record.RecordGeneration {
		t.Fatalf("bound fence = identity %#v record %d, want %#v/%d", snapshot.BoundIdentity, snapshot.BoundRecordGeneration, targetHandle.identity, targetHandle.record.RecordGeneration)
	}
	if snapshot.Classification != CodexRestoredLaneCurrent || snapshot.BoundChoice.AccountKey != "account-a" || snapshot.BoundChoice.EffectiveModel != targetHandle.record.EffectiveModel {
		t.Fatalf("bound disposition = %q choice %#v", snapshot.Classification, snapshot.BoundChoice)
	}
	wantProvisional := map[codex.AccountKey]int{"account-a": 1, "account-b": 1}
	if !reflect.DeepEqual(snapshot.Provisional, wantProvisional) {
		t.Fatalf("provisional = %#v, want %#v", snapshot.Provisional, wantProvisional)
	}
	if snapshot.JournalGeneration == 0 || snapshot.JournalGeneration != coordinator.store.Generation() {
		t.Fatalf("journal generation = %d, store = %d", snapshot.JournalGeneration, coordinator.store.Generation())
	}
	if owner.begins != 2 {
		t.Fatalf("owner operations = %d, want two independent store reads", owner.begins)
	}

	snapshot.Provisional["account-a"] = 99
	again, err := coordinator.LoadRouteSnapshot(context.Background(), target.Key, accounts, target.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Provisional, wantProvisional) {
		t.Fatalf("second provisional = %#v, want detached %#v", again.Provisional, wantProvisional)
	}
}

func TestCodexLeaseRouteSnapshotSuppressesUnavailableBoundAccount(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "account-a", Kind: CodexAttemptSlotDirect,
	}})
	initial, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	initial, err = initial.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	initial, err = initial.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	initial, err = initial.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Drain(); err != nil {
		t.Fatal(err)
	}

	incremental := plan
	incremental.RequiresAccountContinuity = true
	incremental.Evidence.HasEncryptedState = true
	handle, err := runtimeLease.BeginRequest(incremental)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.RecordQuotaExhaustedContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseBoundQuiescent || codexLeaseCurrentAttemptState(handle.record) != CodexAttemptAccountUnavailable || !handle.EverAdmitted() {
		t.Fatalf("terminal unavailable request = %#v", handle.record)
	}

	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BoundAccountKey != "" || !snapshot.BoundIdentity.IsZero() || snapshot.BoundRecordGeneration != 0 || !reflect.DeepEqual(snapshot.BoundChoice, RouteChoice{}) || snapshot.AffinityAccountKey != "" || snapshot.AffinityRequiresAccount {
		t.Fatalf("unavailable route remained bound: %#v", snapshot)
	}
	if !reflect.DeepEqual(snapshot.UnavailableAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("unavailable accounts = %#v, want account-a", snapshot.UnavailableAccountKeys)
	}
	if !reflect.DeepEqual(snapshot.QuotaExhaustedAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("quota-exhausted accounts = %#v, want account-a", snapshot.QuotaExhaustedAccountKeys)
	}
	snapshot.UnavailableAccountKeys[0] = "mutated"
	again, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.UnavailableAccountKeys, []codex.AccountKey{"account-a"}) {
		t.Fatalf("second unavailable accounts = %#v, want detached account-a", again.UnavailableAccountKeys)
	}
}

func TestCodexLeaseRouteSnapshotRefreshesAffinityFromLatestAdmittedRequest(t *testing.T) {
	t.Parallel()

	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "first-a", Kind: CodexAttemptSlotDirect,
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
	firstAdmissionGeneration := first.record.AdmissionJournalGeneration
	firstAdmissionRequestGeneration := first.record.AdmissionRequestGeneration
	firstAdmissionRequestKind := first.record.AdmissionRequestKind
	firstAdmissionCompactionPhase := first.record.AdmissionCompactionPhase
	firstAdmittedAt := first.record.AdmittedAt
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.Drain()
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(40 * time.Minute)
	secondPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "second-a", Kind: CodexAttemptSlotDirect,
	}})
	secondPlan.RequestedModel = "latest-requested-model"
	secondPlan.EffectiveModel = "gpt-latest-cache-model"
	second, err := runtimeLease.BeginRequest(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	latestCacheAdmission := *now
	if second.record.AdmissionJournalGeneration != firstAdmissionGeneration || second.record.AdmissionRequestGeneration != firstAdmissionRequestGeneration || second.record.AdmissionRequestKind != firstAdmissionRequestKind || second.record.AdmissionCompactionPhase != firstAdmissionCompactionPhase || second.record.AdmittedAt != firstAdmittedAt {
		t.Fatalf("immutable first admission changed: %#v", second.record)
	}
	second, err = second.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.Drain()
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Minute)
	rejectedPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "rejected-a", Kind: CodexAttemptSlotDirect,
	}})
	rejectedPlan.EffectiveModel = "gpt-rejected-model"
	rejected, err := runtimeLease.BeginRequest(rejectedPlan)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err = rejected.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	rejected, err = rejected.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator = reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)

	thirdPlan := codexLeaseRuntimeTestPlan("successor-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "third-a", Kind: CodexAttemptSlotDirect,
	}})
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), thirdPlan.Key, thirdPlan.Accounts, thirdPlan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AffinityAccountKey != "account-a" || snapshot.AffinityCacheAdmittedAt != latestCacheAdmission || snapshot.AffinityEffectiveModel != second.record.EffectiveModel {
		t.Fatalf("latest affinity = account %q cache admitted %v model %q, want account-a/%v/%q", snapshot.AffinityAccountKey, snapshot.AffinityCacheAdmittedAt, snapshot.AffinityEffectiveModel, latestCacheAdmission, second.record.EffectiveModel)
	}
}

func TestCodexLeaseV2MidTurnCompactionAdmissionRefreshesTaskCacheAffinity(t *testing.T) {
	t.Parallel()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "turn-a", Kind: CodexAttemptSlotDirect,
	}})
	firstPlan.EffectiveModel = "gpt-cache-origin"
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
	firstCacheAdmittedAt := first.record.AdmittedAt
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if first, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(29 * time.Minute)
	compactionPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "compaction-a", Kind: CodexAttemptSlotDirect,
	}})
	compactionPlan.RequestKind = CodexRequestCompaction
	compactionPlan.CompactionPhase = CodexCompactionMidTurn
	compactionPlan.EffectiveModel = "gpt-5.6-sol"
	compaction, err := runtimeLease.BeginRequest(compactionPlan)
	if err != nil {
		t.Fatal(err)
	}
	compaction, err = compaction.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	compaction, err = compaction.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	compactCacheAdmittedAt := *now

	successor := codexLeaseRuntimeTestPlan("successor", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "successor-a", Kind: CodexAttemptSlotDirect,
	}})
	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), successor.Key, successor.Accounts, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AffinityCacheAdmittedAt != compactCacheAdmittedAt || snapshot.AffinityEffectiveModel != compactionPlan.EffectiveModel {
		t.Fatalf("mid-turn compaction cache affinity = %v/%q, want %v/%q (sampling was %v)", snapshot.AffinityCacheAdmittedAt, snapshot.AffinityEffectiveModel, compactCacheAdmittedAt, compactionPlan.EffectiveModel, firstCacheAdmittedAt)
	}
	compaction, err = compaction.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if compaction, err = compaction.Drain(); err != nil {
		t.Fatal(err)
	}

	*now = firstCacheAdmittedAt.Add(30 * time.Minute)
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
	}}
	capacity := NewCodexCapacityLedger(func() time.Time { return *now }, time.Hour)
	frozenDispatchObserveCapacity(t, capacity, "account-a", CapacityBucketBase, 10, *now)
	frozenDispatchObserveCapacity(t, capacity, "account-b", CapacityBucketBase, 90, *now)
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory}, Capacity: capacity, Routes: coordinator, Runtime: runtimeLease,
		DefaultAccountKey: "account-b", Authority: CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}, Now: func() time.Time { return *now },
	}
	encoded := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"runtime-session","thread_id":"runtime-thread","turn_id":"successor","request_kind":"turn"}},"input":[{"role":"user","content":"private"}]}`)
	prepared, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: encoded})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) == 0 || accounts[0].Choice().AccountKey != "account-a" || prepared.Dispatch.accounts[0].decision != codexRuntimeDecisionAffinityReuse {
		t.Fatalf("successor route = %#v decision %q, want account-a affinity reuse", accounts, prepared.Dispatch.accounts[0].decision)
	}
	if _, err := prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLeaseV2UnadmittedMidTurnCompactionCannotRefreshTaskCacheAffinity(t *testing.T) {
	t.Parallel()
	old := CodexJournalRecordV2{
		AccountHash: "account-digest", CodexCurrentRequest: CodexCurrentRequest{RequestKind: CodexRequestCompaction, CompactionPhase: CodexCompactionMidTurn},
	}
	result := old
	if codexLeaseCurrentRequestCacheRefreshEligible(old, result) {
		t.Fatal("unadmitted mid-turn compaction became cache-refresh eligible")
	}
	old.EverAdmitted = true
	result.EverAdmitted = true
	result.AccountHash = "other-account-digest"
	if codexLeaseCurrentRequestCacheRefreshEligible(old, result) {
		t.Fatal("cross-account mid-turn compaction became cache-refresh eligible")
	}
}

func TestCodexLeaseV2UnsuccessfulMidTurnCompactionDoesNotRefreshTaskCacheAffinity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		finish func(*CodexLeaseRequestHandle) (*CodexLeaseRequestHandle, error)
	}{
		{name: "rejected", finish: func(handle *CodexLeaseRequestHandle) (*CodexLeaseRequestHandle, error) {
			return handle.FinishRejected()
		}},
		{name: "ambiguous", finish: func(handle *CodexLeaseRequestHandle) (*CodexLeaseRequestHandle, error) { return handle.Indeterminate() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "sampling", Kind: CodexAttemptSlotDirect}})
			plan.EffectiveModel = "gpt-5.6-sol"
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			if handle, err = handle.MarkDispatched(); err != nil {
				t.Fatal(err)
			}
			if handle, err = handle.AdmitHTTP2xx(); err != nil {
				t.Fatal(err)
			}
			wantTime := handle.record.AdmittedAt
			if handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false}); err != nil {
				t.Fatal(err)
			}
			if handle, err = handle.Drain(); err != nil {
				t.Fatal(err)
			}

			*now = now.Add(29 * time.Minute)
			compactPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "compact", Kind: CodexAttemptSlotDirect}})
			compactPlan.RequestKind = CodexRequestCompaction
			compactPlan.CompactionPhase = CodexCompactionMidTurn
			compactPlan.EffectiveModel = "gpt-5.6-sol"
			compact, err := runtimeLease.BeginRequest(compactPlan)
			if err != nil {
				t.Fatal(err)
			}
			if compact, err = compact.MarkDispatched(); err != nil {
				t.Fatal(err)
			}
			if compact, err = test.finish(compact); err != nil {
				t.Fatal(err)
			}
			lane := coordinator.Store().v2.Lanes[0]
			if lane.LastCacheAdmittedAt != wantTime || lane.LastCacheEffectiveModel != plan.EffectiveModel {
				t.Fatalf("%s compact moved cache affinity to %v/%q, want %v/%q", test.name, lane.LastCacheAdmittedAt, lane.LastCacheEffectiveModel, wantTime, plan.EffectiveModel)
			}
		})
	}
}

func TestCodexLeaseV2FailedUnadmittedCompactionDoesNotCreateCacheAdmission(t *testing.T) {
	t.Parallel()
	old := CodexJournalRecordV2{
		AccountHash: "account-digest", CodexCurrentRequest: CodexCurrentRequest{
			Generation: 1, RequestKind: CodexRequestCompaction, CompactionPhase: CodexCompactionMidTurn, CurrentAttemptGeneration: 1,
			Attempts: []CodexJournalAttempt{{Generation: 1, State: CodexAttemptDispatched}},
		},
	}
	failed := old
	failed.State = LeaseFailedUnadmitted
	failed.Attempts = []CodexJournalAttempt{{Generation: 1, State: CodexAttemptProviderFailed}}
	if codexLeaseCacheAdmission(old, failed, true) {
		t.Fatal("failed-unadmitted compaction created cache admission")
	}
}

func TestCodexLeaseRouteSnapshotProjectsEncryptedAffinityWhenAccountIsUnavailable(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	predecessorPlan := codexLeaseRuntimeTestPlan("predecessor", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "predecessor-a", Kind: CodexAttemptSlotDirect,
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
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{HasEncryptedState: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if predecessor, err = predecessor.Drain(); err != nil {
		t.Fatal(err)
	}

	successor := codexLeaseRuntimeTestPlan("successor", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "successor-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "successor-b", Kind: CodexAttemptSlotDirect},
	})
	resolved, err := coordinator.LoadRouteSnapshot(context.Background(), successor.Key, successor.Accounts, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.AffinityPresent || resolved.AffinityAccountKey != "account-a" || !resolved.AffinityRequiresAccount || resolved.AffinityCacheAdmittedAt != predecessor.record.AdmittedAt || resolved.AffinityEffectiveModel != predecessor.record.EffectiveModel {
		t.Fatalf("resolved encrypted affinity = %#v", resolved)
	}

	unresolved, err := coordinator.LoadRouteSnapshot(context.Background(), successor.Key, []codex.AccountKey{"account-b"}, successor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !unresolved.AffinityPresent || unresolved.AffinityAccountKey != "" || !unresolved.AffinityRequiresAccount || unresolved.AffinityCacheAdmittedAt != predecessor.record.AdmittedAt || unresolved.AffinityEffectiveModel != predecessor.record.EffectiveModel {
		t.Fatalf("unresolved encrypted affinity = %#v", unresolved)
	}
}

func TestCodexLeaseRouteSnapshotBindsCurrentNonMigratableRequest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
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
	handle, err = handle.Indeterminate()
	if err != nil {
		t.Fatal(err)
	}
	if handle, err = handle.Drain(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !handle.record.NonMigratable || handle.record.EverAdmitted {
		t.Fatalf("test precondition = non-migratable %v admitted %v, want true/false", handle.record.NonMigratable, handle.record.EverAdmitted)
	}
	if snapshot.BoundAccountKey != "account-a" || snapshot.BoundRecordGeneration != handle.record.RecordGeneration || snapshot.BoundChoice.AccountKey != "account-a" || !snapshot.BoundRequiresAccount {
		t.Fatalf("non-migratable binding = account %q generation %d choice %#v", snapshot.BoundAccountKey, snapshot.BoundRecordGeneration, snapshot.BoundChoice)
	}
	wrongAccount := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-b", CandidateID: "candidate-wrong", Kind: CodexAttemptSlotDirect,
	}})
	wrongAccount.Accounts = []codex.AccountKey{"account-a", "account-b"}
	if _, err := runtimeLease.BeginRequest(wrongAccount); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("non-migratable account change = %v, want continuity error", err)
	}
	retry := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-retry", Kind: CodexAttemptSlotDirect,
	}})
	retry.ExpectedBound = &CodexLeaseBoundExpectation{
		Identity:         snapshot.BoundIdentity,
		AccountKey:       snapshot.BoundAccountKey,
		RecordGeneration: snapshot.BoundRecordGeneration,
	}
	if _, err := runtimeLease.BeginRequest(retry); err != nil {
		t.Fatalf("raw-state-free non-migratable retry: %v", err)
	}
}

func TestCodexLeaseRouteSnapshotBindsAdmittedShadowTurnWithoutAffinity(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("shadow-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "shadow-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 10}

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
	if !handle.EverAdmitted() {
		t.Fatal("shadow admission was not retained as an actual admission fact")
	}

	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BoundAccountKey != "account-a" {
		t.Fatalf("bound account = %q, want account-a", snapshot.BoundAccountKey)
	}
	if snapshot.BoundIdentity.Authoritative || snapshot.BoundIdentity.ModeEpoch != 10 || snapshot.BoundRecordGeneration == 0 {
		t.Fatalf("shadow bound fence = identity %#v record %d", snapshot.BoundIdentity, snapshot.BoundRecordGeneration)
	}
	if snapshot.AffinityAccountKey != "" {
		t.Fatalf("shadow account became cross-turn affinity: %q", snapshot.AffinityAccountKey)
	}
}

func TestCodexLeaseRouteSnapshotRestoresAdmittedShadowExactTurnWithoutCrossTurnAffinity(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("restored-shadow-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "restored-shadow-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	handle := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)
	if !handle.EverAdmitted() || handle.identity.Authoritative {
		t.Fatalf("completed shadow authority = identity %#v admitted %t", handle.identity, handle.EverAdmitted())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)

	exact, err := reopened.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Classification != CodexRestoredLaneCurrent || exact.BoundAccountKey != "account-a" {
		t.Fatalf("restored exact shadow = classification %q bound %q", exact.Classification, exact.BoundAccountKey)
	}
	if exact.BoundIdentity.Authoritative || exact.BoundIdentity.ModeEpoch != 10 || exact.BoundRecordGeneration == 0 {
		t.Fatalf("restored shadow fence = identity %#v record %d", exact.BoundIdentity, exact.BoundRecordGeneration)
	}
	if exact.AffinityAccountKey != "" {
		t.Fatalf("restored exact shadow became affinity: %q", exact.AffinityAccountKey)
	}

	otherKey := plan.Key
	otherKey.Turn = "other-shadow-turn"
	other, err := reopened.LoadRouteSnapshot(context.Background(), otherKey, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if other.Classification != CodexRestoredLaneUnseen || other.BoundAccountKey != "" || other.AffinityAccountKey != "" {
		t.Fatalf("cross-turn shadow state = classification %q bound %q affinity %q", other.Classification, other.BoundAccountKey, other.AffinityAccountKey)
	}
}

func TestCodexLeaseRouteSnapshotDoesNotPromoteAdmittedShadowAcrossPolicyEpoch(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("epoch-shadow-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "account-a",
		CandidateID: "epoch-shadow-a",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	handle := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)
	if !handle.EverAdmitted() || handle.identity.Authoritative {
		t.Fatalf("completed shadow authority = identity %#v admitted %t", handle.identity, handle.EverAdmitted())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)

	nextPolicy := CodexLeaseAuthorityPolicy{ModeEpoch: 11}
	beforeGeneration := reopened.Store().Generation()
	next, err := reopened.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, nextPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if next.Classification != CodexRestoredLaneUnseen || next.BoundAccountKey != "" || !next.BoundIdentity.IsZero() || next.BoundRecordGeneration != 0 || !reflect.DeepEqual(next.BoundChoice, RouteChoice{}) {
		t.Fatalf("next-epoch shadow state = classification %q bound %q identity %#v record %d choice %#v", next.Classification, next.BoundAccountKey, next.BoundIdentity, next.BoundRecordGeneration, next.BoundChoice)
	}
	if next.AffinityAccountKey != "" {
		t.Fatalf("next-epoch shadow became affinity: %q", next.AffinityAccountKey)
	}
	if reopened.Store().Generation() != beforeGeneration {
		t.Fatalf("next-epoch read changed journal generation from %d to %d", beforeGeneration, reopened.Store().Generation())
	}
	for _, record := range reopened.Store().v2.Records {
		if record.TurnHash == reopened.Store().hash("turn", plan.Key.Turn) && record.Authoritative {
			t.Fatalf("shadow record promoted across policy epoch: %#v", record.Identity())
		}
	}
}

func TestCodexLeaseRouteSnapshotDistinguishesHistoricalAuthority(t *testing.T) {
	tests := []struct {
		name          string
		firstPolicy   CodexLeaseAuthorityPolicy
		probePolicy   CodexLeaseAuthorityPolicy
		authoritative bool
	}{
		{name: "retained authoritative", firstPolicy: CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}, probePolicy: CodexLeaseAuthorityPolicy{ModeEpoch: 10, RetainedAuthoritativeEpochs: []uint64{9}}, authoritative: true},
		{name: "shadow", firstPolicy: CodexLeaseAuthorityPolicy{ModeEpoch: 10}, probePolicy: CodexLeaseAuthorityPolicy{ModeEpoch: 10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			first := codexLeaseRuntimeTestPlan("historical-first", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "first", Kind: CodexAttemptSlotDirect}})
			first.Authority = test.firstPolicy
			completeCodexLeaseRuntimeTurn(t, runtimeLease, first)
			successor := codexLeaseRuntimeTestPlan("historical-successor", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "successor", Kind: CodexAttemptSlotDirect}})
			successor.Authority = test.firstPolicy
			if _, err := runtimeLease.BeginRequest(successor); err != nil {
				t.Fatal(err)
			}

			snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), first.Key, first.Accounts, test.probePolicy)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Classification != CodexRestoredLaneHistorical || snapshot.HistoricalAuthoritative != test.authoritative {
				t.Fatalf("historical snapshot = classification %q authoritative %t", snapshot.Classification, snapshot.HistoricalAuthoritative)
			}
		})
	}
}

func TestCodexLeaseRouteSnapshotHonoursCancellationBeforePersistence(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	plan := codexLeaseRuntimeTestPlan("target", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "target-a", Kind: CodexAttemptSlotDirect}})
	before := coordinator.store.Generation()

	coordinator.leases.lifecycle.persistence.Lock()
	defer coordinator.leases.lifecycle.persistence.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := coordinator.LoadRouteSnapshot(ctx, plan.Key, plan.Accounts, plan.Authority)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadRouteSnapshot error = %T %v, want context canceled", err, err)
	}
	if !reflect.DeepEqual(snapshot, CodexLeaseRouteSnapshot{}) {
		t.Fatalf("cancelled snapshot = %#v, want zero", snapshot)
	}
	if coordinator.store.Generation() != before {
		t.Fatalf("cancelled snapshot changed generation from %d to %d", before, coordinator.store.Generation())
	}
}

func TestCodexLeaseRouteSnapshotRetriesGenerationDrift(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("target", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "target-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}

	owner := newBlockingCodexLeaseRouteSnapshotOwner()
	coordinator.store.mu.Lock()
	coordinator.store.owner = owner
	coordinator.store.mu.Unlock()
	t.Cleanup(owner.release)
	type result struct {
		snapshot CodexLeaseRouteSnapshot
		err      error
	}
	resultChannel := make(chan result, 1)
	go func() {
		snapshot, loadErr := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, plan.Authority)
		resultChannel <- result{snapshot: snapshot, err: loadErr}
	}()

	select {
	case <-owner.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("route snapshot did not reach its generation-verification read")
	}
	if _, err := prepared.transitionAttemptWithFence(prepared.fence, CodexAttemptPrepared, CodexAttemptDispatched, nil); err != nil {
		t.Fatal(err)
	}
	owner.release()

	select {
	case result := <-resultChannel:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.snapshot.JournalGeneration != coordinator.store.Generation() {
			t.Fatalf("snapshot generation = %d, store = %d", result.snapshot.JournalGeneration, coordinator.store.Generation())
		}
		if !reflect.DeepEqual(result.snapshot.Provisional, map[codex.AccountKey]int{"account-a": 1}) {
			t.Fatalf("provisional = %#v, want retried current generation", result.snapshot.Provisional)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route snapshot did not retry generation drift")
	}
	if owner.begins.Load() < 5 {
		t.Fatalf("owner operations = %d, want generation retry", owner.begins.Load())
	}
}

func TestCodexLeaseRouteSnapshotDetachesAuthorityPolicyAcrossGenerationRetry(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("target", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "target-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}

	retained := []uint64{9}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10, RetainedAuthoritativeEpochs: retained}
	owner := newBlockingCodexLeaseRouteSnapshotOwner()
	owner.mutateFirst = func() { retained[0] = 8 }
	coordinator.store.mu.Lock()
	coordinator.store.owner = owner
	coordinator.store.mu.Unlock()
	t.Cleanup(owner.release)
	type result struct {
		snapshot CodexLeaseRouteSnapshot
		err      error
	}
	resultChannel := make(chan result, 1)
	go func() {
		snapshot, loadErr := coordinator.LoadRouteSnapshot(context.Background(), plan.Key, plan.Accounts, policy)
		resultChannel <- result{snapshot: snapshot, err: loadErr}
	}()

	select {
	case <-owner.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("route snapshot did not reach its generation-verification read")
	}
	if _, err := prepared.transitionAttemptWithFence(prepared.fence, CodexAttemptPrepared, CodexAttemptDispatched, nil); err != nil {
		t.Fatal(err)
	}
	owner.release()

	select {
	case result := <-resultChannel:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.snapshot.JournalGeneration != coordinator.store.Generation() {
			t.Fatalf("snapshot generation = %d, store = %d", result.snapshot.JournalGeneration, coordinator.store.Generation())
		}
		if !reflect.DeepEqual(result.snapshot.Provisional, map[codex.AccountKey]int{"account-a": 1}) {
			t.Fatalf("provisional = %#v, want retried current generation", result.snapshot.Provisional)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route snapshot did not retry generation drift")
	}
	if retained[0] != 8 {
		t.Fatal("owner callback did not mutate caller policy")
	}
}

func TestCodexLeaseRouteSnapshotRejectsUnresolvedGlobalProvisionalAccount(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	active := codexLeaseRuntimeTestPlan("active", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "active-a", Kind: CodexAttemptSlotDirect}})
	if _, err := runtimeLease.BeginRequest(active); err != nil {
		t.Fatal(err)
	}
	target := codexLeaseRuntimeTestPlan("unseen", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "target-b", Kind: CodexAttemptSlotDirect}})
	target.Key.Lane.Thread = "unseen-thread"

	snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), target.Key, target.Accounts, target.Authority)
	if !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("LoadRouteSnapshot error = %T %v, want authority mismatch", err, err)
	}
	if !reflect.DeepEqual(snapshot, CodexLeaseRouteSnapshot{}) {
		t.Fatalf("unresolved snapshot = %#v, want zero", snapshot)
	}
}

type blockingCodexLeaseRouteSnapshotOwner struct {
	begins        atomic.Int32
	secondStarted chan struct{}
	releaseSecond chan struct{}
	releaseOnce   sync.Once
	mutateFirst   func()
}

func newBlockingCodexLeaseRouteSnapshotOwner() *blockingCodexLeaseRouteSnapshotOwner {
	return &blockingCodexLeaseRouteSnapshotOwner{
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (owner *blockingCodexLeaseRouteSnapshotOwner) AssertOwner() error { return nil }

func (owner *blockingCodexLeaseRouteSnapshotOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	count := owner.begins.Add(1)
	if count == 1 && owner.mutateFirst != nil {
		owner.mutateFirst()
	}
	if count == 2 {
		close(owner.secondStarted)
		<-owner.releaseSecond
	}
	return &codex.CredentialOwnerOperation{}, nil
}

func (owner *blockingCodexLeaseRouteSnapshotOwner) release() {
	owner.releaseOnce.Do(func() { close(owner.releaseSecond) })
}
