package proxy

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTurnLeaseManagerRevokeClearsSharedAliases(t *testing.T) {
	manager := NewCodexTurnLeaseManager(8, false, nil)
	alias := manager.ForMode(9, true)
	key := testCodexLeaseKey("lifecycle-thread", "lifecycle-turn")
	choice := RouteChoice{
		AccountKey:      "account-sensitive",
		RequestedModel:  "requested-sensitive",
		EffectiveModel:  "effective-sensitive",
		RequiredBuckets: []CapacityBucket{"bucket-sensitive-a", "bucket-sensitive-b"},
	}
	lease, err := alias.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
		return choice, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := alias.SetTurnState(key, "turn-state-sensitive"); err != nil {
		t.Fatal(err)
	}
	if err := alias.SetResponseAnchor(key, "response-sensitive", true); err != nil {
		t.Fatal(err)
	}
	if _, err := alias.Admit(key, lease.AccountKey, 41, nil); err != nil {
		t.Fatal(err)
	}

	managed := manager.leases[key]
	buckets := managed.lease.Choice.RequiredBuckets
	manager.revoke()
	manager.revoke()

	if !manager.lifecycle.closed || manager.lifecycle != alias.lifecycle || manager.mu != alias.mu || manager.accountGates != alias.accountGates {
		t.Fatal("mode aliases do not share one closed lifecycle core")
	}
	if len(manager.current) != 0 || len(manager.leases) != 0 {
		t.Fatalf("revoked authority retained: current=%d leases=%d", len(manager.current), len(manager.leases))
	}
	if !reflect.DeepEqual(*managed, codexManagedLease{}) {
		t.Fatalf("revoked managed lease was not zeroed: %#v", *managed)
	}
	for index, bucket := range buckets {
		if bucket != "" {
			t.Fatalf("revoked nested bucket %d retained %q", index, bucket)
		}
	}

	var selected atomic.Bool
	if got, err := alias.Acquire(context.Background(), key, func(context.Context) (codex.AccountKey, error) {
		selected.Store(true)
		return "account-after-revoke", nil
	}); !reflect.DeepEqual(got, CodexTurnLease{}) || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("revoked Acquire = (%#v, %T %v)", got, err, err)
	}
	if selected.Load() {
		t.Fatal("revoked Acquire invoked its selector")
	}
	assertCodexTurnLeaseManagerClosedOperations(t, alias, key)

	alias.Restore([]CodexTurnLease{{
		Key:           key,
		State:         LeaseBoundActive,
		AccountKey:    "restored-sensitive",
		ModeEpoch:     9,
		Authoritative: true,
	}})
	alias.Compact(0)
	if got := alias.Snapshot(); len(got) != 0 {
		t.Fatalf("revoked Restore/Compact repopulated snapshot: %#v", got)
	}
	alias.accountGates.mu.Lock()
	gateCount := len(alias.accountGates.entries)
	alias.accountGates.mu.Unlock()
	if gateCount != 0 {
		t.Fatalf("revoked Restore populated %d account gates", gateCount)
	}

	closedView := alias.ForMode(10, true)
	if closedView.lifecycle != manager.lifecycle || closedView.mu != manager.mu || closedView.accountGates != manager.accountGates {
		t.Fatal("post-revoke ForMode did not retain the closed shared core")
	}
	if epoch, authoritative := closedView.Mode(); epoch != 0 || authoritative {
		t.Fatalf("closed mode = (%d, %t), want (0, false)", epoch, authoritative)
	}
	if _, err := closedView.Acquire(context.Background(), testCodexLeaseKey("closed-view", "turn"), fixedCodexAccount("account")); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed view Acquire error = %T %v", err, err)
	}

	var nilManager *CodexTurnLeaseManager
	nilView := nilManager.ForMode(11, true)
	if epoch, authoritative := nilView.Mode(); epoch != 0 || authoritative {
		t.Fatalf("nil-derived mode = (%d, %t), want (0, false)", epoch, authoritative)
	}
	if _, err := nilView.Acquire(context.Background(), testCodexLeaseKey("nil-view", "turn"), fixedCodexAccount("account")); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("nil-derived Acquire error = %T %v", err, err)
	}

	standalone := NewCodexTurnLeaseManager(12, true, nil)
	if _, err := standalone.Acquire(context.Background(), testCodexLeaseKey("standalone", "turn"), fixedCodexAccount("account-open")); err != nil {
		t.Fatalf("independent standalone manager was revoked: %v", err)
	}
}

func TestCodexTurnLeaseManagerRevokeDuringSelectorDoesNotResurrect(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	key := testCodexLeaseKey("selector-revoke", "turn")
	selectorEntered := make(chan struct{})
	selectorRelease := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := manager.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
			close(selectorEntered)
			<-selectorRelease
			return RouteChoice{
				AccountKey:      "selected-sensitive",
				RequestedModel:  "requested-sensitive",
				EffectiveModel:  "effective-sensitive",
				RequiredBuckets: []CapacityBucket{"bucket-sensitive"},
			}, nil
		})
		result <- err
	}()
	select {
	case <-selectorEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("selector did not block")
	}

	manager.mu.Lock()
	managed := manager.leases[key]
	ready := managed.ready
	manager.mu.Unlock()
	manager.revoke()
	select {
	case <-ready:
	default:
		t.Fatal("revoke did not wake the reserving ready channel")
	}
	close(selectorRelease)
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("post-revoke selector result = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selector owner did not resolve after revoke")
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("selector resurrected authority after revoke: %#v", got)
	}
	if !reflect.DeepEqual(*managed, codexManagedLease{}) {
		t.Fatalf("selector repopulated cleared managed lease: %#v", *managed)
	}
}

func TestCodexTurnLeaseManagerRevokeDuringAdmissionRevalidationWins(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	key := testCodexLeaseKey("revalidation-revoke", "turn")
	lease, err := manager.Acquire(context.Background(), key, fixedCodexAccount("account-sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	revalidationEntered := make(chan struct{})
	revalidationRelease := make(chan struct{})
	revalidationErr := errors.New("stale revalidation result")
	var persisted atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, err := manager.AdmitContext(context.Background(), key, lease.AccountKey, 51, func(context.Context, codex.AccountKey) error {
			close(revalidationEntered)
			<-revalidationRelease
			return revalidationErr
		}, func([]CodexTurnLease) error {
			persisted.Add(1)
			return nil
		})
		result <- err
	}()
	select {
	case <-revalidationEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission revalidation did not block")
	}

	manager.revoke()
	close(revalidationRelease)
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("post-revoke revalidation result = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission revalidation did not resolve after revoke")
	}
	if persisted.Load() != 0 {
		t.Fatalf("post-revoke admission persisted %d times", persisted.Load())
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("revalidation resurrected authority after revoke: %#v", got)
	}
}

func TestCodexTurnLeaseManagerRevokeWhileAdmissionWaitsForGate(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	key := testCodexLeaseKey("gate-revoke", "turn")
	account := codex.AccountKey("account-sensitive")
	if _, err := manager.Acquire(context.Background(), key, fixedCodexAccount(account)); err != nil {
		t.Fatal(err)
	}
	blockingGuard, err := manager.accountGates.acquire(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	var revalidated atomic.Int32
	var persisted atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, err := manager.AdmitContext(context.Background(), key, account, 61, func(context.Context, codex.AccountKey) error {
			revalidated.Add(1)
			return nil
		}, func([]CodexTurnLease) error {
			persisted.Add(1)
			return nil
		})
		result <- err
	}()
	waitCodexTurnLeaseManagerGateRefs(t, manager.accountGates, account, 2)

	manager.revoke()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("post-revoke gated admission result = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gated admission did not resolve when revoke closed the account gates")
	}
	blockingGuard.Release()
	if revalidated.Load() != 0 || persisted.Load() != 0 {
		t.Fatalf("post-revoke gated admission callbacks = revalidate %d persist %d", revalidated.Load(), persisted.Load())
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("gated admission resurrected authority after revoke: %#v", got)
	}
	manager.accountGates.mu.Lock()
	gateCount := len(manager.accountGates.entries)
	manager.accountGates.mu.Unlock()
	if gateCount != 0 {
		t.Fatalf("revoked account gates retained %d identities", gateCount)
	}
}

func TestCodexTurnLeaseManagerRevokeWakesAuthoritativeRestoreWaitingForGate(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	account := codex.AccountKey("restore-account-sensitive")
	blockingGuard, err := manager.accountGates.acquire(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	restored := make(chan struct{})
	go func() {
		manager.Restore([]CodexTurnLease{{
			Key:           testCodexLeaseKey("restore-gate-revoke", "turn"),
			State:         LeaseBoundQuiescent,
			AccountKey:    account,
			ModeEpoch:     9,
			Authoritative: true,
		}})
		close(restored)
	}()
	waitCodexTurnLeaseManagerGateRefs(t, manager.accountGates, account, 2)

	manager.revoke()
	select {
	case <-restored:
	case <-time.After(2 * time.Second):
		t.Fatal("authoritative Restore did not resolve when revoke closed the account gates")
	}
	blockingGuard.Release()
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("gated Restore resurrected authority after revoke: %#v", got)
	}
	manager.accountGates.mu.Lock()
	gateCount := len(manager.accountGates.entries)
	manager.accountGates.mu.Unlock()
	if gateCount != 0 {
		t.Fatalf("revoked account gates retained %d identities", gateCount)
	}
}

func TestCodexTurnLeaseManagerRevokeWaitsForAdmissionPersistence(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	key := testCodexLeaseKey("persist-revoke", "turn")
	account := codex.AccountKey("account-sensitive")
	if _, err := manager.Acquire(context.Background(), key, fixedCodexAccount(account)); err != nil {
		t.Fatal(err)
	}
	persistEntered := make(chan struct{})
	persistRelease := make(chan struct{})
	var persistCalls atomic.Int32
	var persistAuthoritySnapshots atomic.Int32
	type admissionResult struct {
		lease CodexTurnLease
		err   error
	}
	admitted := make(chan admissionResult, 1)
	go func() {
		lease, err := manager.AdmitContext(context.Background(), key, account, 71, nil, func([]CodexTurnLease) error {
			persistCalls.Add(1)
			manager.Compact(DefaultCodexLeaseRetention)
			if got := manager.Snapshot(); len(got) == 1 && got[0].State == LeaseBoundActive {
				persistAuthoritySnapshots.Add(1)
			}
			close(persistEntered)
			<-persistRelease
			manager.Compact(DefaultCodexLeaseRetention)
			if got := manager.Snapshot(); len(got) == 1 && got[0].State == LeaseBoundActive {
				persistAuthoritySnapshots.Add(1)
			}
			return nil
		})
		admitted <- admissionResult{lease: lease, err: err}
	}()
	select {
	case <-persistEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission persistence did not block")
	}

	revokeReturned := make(chan struct{})
	go func() {
		manager.revoke()
		close(revokeReturned)
	}()
	select {
	case <-revokeReturned:
		t.Fatal("revoke crossed an in-flight persistence boundary")
	case <-time.After(20 * time.Millisecond):
	}
	close(persistRelease)
	select {
	case result := <-admitted:
		if result.err != nil || result.lease.State != LeaseBoundActive {
			t.Fatalf("pre-revoke admission result = (%#v, %T %v)", result.lease, result.err, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not finish after persistence release")
	}
	select {
	case <-revokeReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("revoke did not finish after persistence")
	}
	if persistCalls.Load() != 1 {
		t.Fatalf("pre-revoke persistence calls = %d, want 1", persistCalls.Load())
	}
	if persistAuthoritySnapshots.Load() != 2 {
		t.Fatalf("persistence authority snapshots = %d, want 2", persistAuthoritySnapshots.Load())
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("persisted admission survived revoke: %#v", got)
	}
	if _, err := manager.Admit(key, account, 72, func([]CodexTurnLease) error {
		persistCalls.Add(1)
		return nil
	}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("post-revoke Admit error = %T %v", err, err)
	}
	if persistCalls.Load() != 1 {
		t.Fatalf("post-revoke persistence calls = %d, want 1", persistCalls.Load())
	}
}

func TestCodexTurnLeaseManagerAdmissionPersistenceMayReenterManager(t *testing.T) {
	manager := NewCodexTurnLeaseManager(9, true, nil)
	key := testCodexLeaseKey("reentrant-persist", "turn")
	account := codex.AccountKey("account-sensitive")
	if _, err := manager.Acquire(context.Background(), key, fixedCodexAccount(account)); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.Admit(key, account, 81, func([]CodexTurnLease) error {
			manager.Compact(DefaultCodexLeaseRetention)
			if got := manager.Snapshot(); len(got) != 1 {
				return errors.New("reentrant snapshot lost live admission")
			}
			return nil
		})
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission persistence deadlocked while re-entering the manager")
	}
}

func TestCodexTurnLeaseManagerCopiesLeaseSliceBoundaries(t *testing.T) {
	manager := NewCodexTurnLeaseManager(31, true, nil)
	key := testCodexLeaseKey("copy-boundaries", "turn")
	account := codex.AccountKey("account-copy")
	originalBucket := CapacityBucket("bucket-original")
	selectorBuckets := []CapacityBucket{originalBucket}
	acquired, err := manager.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
		return RouteChoice{
			AccountKey:      account,
			RequestedModel:  "requested",
			EffectiveModel:  "effective",
			RequiredBuckets: selectorBuckets,
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	selectorBuckets[0] = "selector-input-mutated"
	if acquired.Choice.RequiredBuckets[0] != originalBucket {
		t.Fatalf("selector input mutated AcquireRoute result: %#v", acquired.Choice.RequiredBuckets)
	}
	acquired.Choice.RequiredBuckets[0] = "acquire-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, originalBucket)
	existing, err := manager.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
		t.Fatal("existing lease invoked its selector")
		return RouteChoice{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	existing.Choice.RequiredBuckets[0] = "existing-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, originalBucket)

	replacementBucket := CapacityBucket("bucket-replacement")
	replacementBuckets := []CapacityBucket{replacementBucket}
	replaced, err := manager.ReplaceProvisionalRoute(key, RouteChoice{
		AccountKey:      account,
		RequestedModel:  "requested-replacement",
		EffectiveModel:  "effective-replacement",
		RequiredBuckets: replacementBuckets,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacementBuckets[0] = "replacement-input-mutated"
	if replaced.Choice.RequiredBuckets[0] != replacementBucket {
		t.Fatalf("replacement input mutated result: %#v", replaced.Choice.RequiredBuckets)
	}
	replaced.Choice.RequiredBuckets[0] = "replacement-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	got, found := manager.Get(key)
	if !found {
		t.Fatal("Get did not find copied lease")
	}
	got.Choice.RequiredBuckets[0] = "get-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	snapshot := manager.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("Snapshot length = %d, want 1", len(snapshot))
	}
	snapshot[0].Choice.RequiredBuckets[0] = "snapshot-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	admitted, err := manager.Admit(key, account, 91, nil)
	if err != nil {
		t.Fatal(err)
	}
	admitted.Choice.RequiredBuckets[0] = "admit-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	failed, err := manager.ObserveProviderFailed(key)
	if err != nil {
		t.Fatal(err)
	}
	failed.Choice.RequiredBuckets[0] = "provider-failed-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	persistFailure := errors.New("persist failure")
	failedPersist, err := manager.Admit(key, account, 92, func([]CodexTurnLease) error {
		return persistFailure
	})
	if !errors.Is(err, persistFailure) {
		t.Fatalf("Admit persistence error = %T %v", err, err)
	}
	failedPersist.Choice.RequiredBuckets[0] = "failed-persist-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)
	if _, err := manager.ObserveProviderFailed(key); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Admit(key, account, 93, nil); err != nil {
		t.Fatal(err)
	}
	completed, err := manager.ObserveCompleted(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed.Choice.RequiredBuckets[0] = "completed-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)

	if _, err := manager.Admit(key, account, 94, nil); err != nil {
		t.Fatal(err)
	}
	indeterminate, err := manager.ObserveIndeterminate(key)
	if err != nil {
		t.Fatal(err)
	}
	indeterminate.Choice.RequiredBuckets[0] = "indeterminate-result-mutated"
	assertCodexTurnLeaseManagerBucket(t, manager, key, replacementBucket)
	detached, found := manager.Get(key)
	if !found {
		t.Fatal("Get did not find lease before revoke")
	}
	manager.revoke()
	if detached.Choice.RequiredBuckets[0] != replacementBucket {
		t.Fatalf("revoke mutated detached Get result: %#v", detached.Choice.RequiredBuckets)
	}

	restoredManager := NewCodexTurnLeaseManager(32, true, nil)
	restoredKey := testCodexLeaseKey("restore-copy-boundaries", "turn")
	restoreBucket := CapacityBucket("bucket-restored")
	restoreInput := []CodexTurnLease{{
		Key:           restoredKey,
		State:         LeaseOrphaned,
		AccountKey:    account,
		Choice:        RouteChoice{AccountKey: account, RequiredBuckets: []CapacityBucket{restoreBucket}},
		ModeEpoch:     32,
		Authoritative: true,
	}}
	restoredManager.Restore(restoreInput)
	restoreInput[0].Choice.RequiredBuckets[0] = "restore-input-mutated"
	assertCodexTurnLeaseManagerBucket(t, restoredManager, restoredKey, restoreBucket)
	restoredManager.revoke()
	if restoreInput[0].Choice.RequiredBuckets[0] != "restore-input-mutated" {
		t.Fatalf("revoke mutated Restore caller input: %#v", restoreInput[0].Choice.RequiredBuckets)
	}
}

func waitCodexTurnLeaseManagerGateRefs(t *testing.T, gates *codexAccountGateSet, account codex.AccountKey, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gates.mu.Lock()
		entry := gates.entries[account]
		got := 0
		if entry != nil {
			got = entry.refs
		}
		gates.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("account gate refs did not reach %d", want)
}

func assertCodexTurnLeaseManagerBucket(t *testing.T, manager *CodexTurnLeaseManager, key LeaseKey, want CapacityBucket) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || len(managed.lease.Choice.RequiredBuckets) != 1 || managed.lease.Choice.RequiredBuckets[0] != want {
		t.Fatalf("managed buckets = %#v, want [%q]", managed, want)
	}
}

func assertCodexTurnLeaseManagerClosedOperations(t *testing.T, manager *CodexTurnLeaseManager, key LeaseKey) {
	t.Helper()
	choice := RouteChoice{AccountKey: "account", RequestedModel: "requested", EffectiveModel: "effective", RequiredBuckets: []CapacityBucket{CapacityBucketBase}}
	assertWriterUnavailable := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("revoked %s error = %T %v", name, err, err)
		}
	}

	if got, err := manager.ReplaceProvisionalRoute(key, choice); !reflect.DeepEqual(got, CodexTurnLease{}) {
		t.Fatalf("revoked ReplaceProvisionalRoute result = %#v", got)
	} else {
		assertWriterUnavailable("ReplaceProvisionalRoute", err)
	}
	assertWriterUnavailable("ReleaseRouting", manager.ReleaseRouting(key))
	if got, err := manager.Admit(key, "account", 1, func([]CodexTurnLease) error {
		t.Fatal("revoked Admit invoked persistence")
		return nil
	}); !reflect.DeepEqual(got, CodexTurnLease{}) {
		t.Fatalf("revoked Admit result = %#v", got)
	} else {
		assertWriterUnavailable("Admit", err)
	}
	if got, err := manager.ObserveCompleted(key, nil); !reflect.DeepEqual(got, CodexTurnLease{}) {
		t.Fatalf("revoked ObserveCompleted result = %#v", got)
	} else {
		assertWriterUnavailable("ObserveCompleted", err)
	}
	if got, err := manager.ObserveIndeterminate(key); !reflect.DeepEqual(got, CodexTurnLease{}) {
		t.Fatalf("revoked ObserveIndeterminate result = %#v", got)
	} else {
		assertWriterUnavailable("ObserveIndeterminate", err)
	}
	if got, err := manager.ObserveProviderFailed(key); !reflect.DeepEqual(got, CodexTurnLease{}) {
		t.Fatalf("revoked ObserveProviderFailed result = %#v", got)
	} else {
		assertWriterUnavailable("ObserveProviderFailed", err)
	}
	assertWriterUnavailable("FailUnadmitted", manager.FailUnadmitted(key))
	assertWriterUnavailable("SetTurnState", manager.SetTurnState(key, "state"))
	assertWriterUnavailable("SetResponseAnchor", manager.SetResponseAnchor(key, "response", true))
	assertWriterUnavailable("MarkNonMigratable", manager.MarkNonMigratable(key))
	if got, found := manager.Get(key); !reflect.DeepEqual(got, CodexTurnLease{}) || found {
		t.Fatalf("revoked Get = (%#v, %t)", got, found)
	}
	if got, found, err := manager.ObservedRouteChoice(key); !reflect.DeepEqual(got, RouteChoice{}) || found {
		t.Fatalf("revoked ObservedRouteChoice = (%#v, %t, %v)", got, found, err)
	} else {
		assertWriterUnavailable("ObservedRouteChoice", err)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("revoked Snapshot = %#v", got)
	}
	if epoch, authoritative := manager.Mode(); epoch != 0 || authoritative {
		t.Fatalf("revoked Mode = (%d, %t)", epoch, authoritative)
	}
}
