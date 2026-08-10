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
	if _, err := targetHandle.AdmitHTTP2xx(); err != nil {
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
	if owner.begins.Load() < 6 {
		t.Fatalf("owner operations = %d, want generation retry", owner.begins.Load())
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
}

func newBlockingCodexLeaseRouteSnapshotOwner() *blockingCodexLeaseRouteSnapshotOwner {
	return &blockingCodexLeaseRouteSnapshotOwner{
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (owner *blockingCodexLeaseRouteSnapshotOwner) AssertOwner() error { return nil }

func (owner *blockingCodexLeaseRouteSnapshotOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	if owner.begins.Add(1) == 2 {
		close(owner.secondStarted)
		<-owner.releaseSecond
	}
	return &codex.CredentialOwnerOperation{}, nil
}

func (owner *blockingCodexLeaseRouteSnapshotOwner) release() {
	owner.releaseOnce.Do(func() { close(owner.releaseSecond) })
}
