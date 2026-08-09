package proxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2BeginAccountRemovalReturnsStableEmptySummary(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "unbound-account")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if guard == nil {
		t.Fatal("BeginAccountRemoval returned a nil guard")
	}
	if summary.JournalGeneration != coordinator.Store().Generation() || summary.BoundCount != 0 || summary != (CodexBoundAuthoritySummary{JournalGeneration: coordinator.Store().Generation()}) {
		t.Fatalf("empty account summary = %#v", summary)
	}
}

func TestCodexLeaseV2RemovalDoesNotClassifyLegacyPrewarmAsAuthenticatedAdoption(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	manager := coordinator.leases.ForMode(9, true)
	account := codex.AccountKey("legacy-prewarm-account")
	key := testCodexRemovalLeaseKey("legacy-prewarm-thread", "adopted-turn")
	if _, err := manager.adoptPrewarm(key, CodexPrewarmReservation{
		AccountKey:       account,
		SocketGeneration: 41,
		ResponseAnchor:   "legacy-prewarm-response",
	}); err != nil {
		t.Fatal(err)
	}

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if summary.BoundCount != 1 || summary.BoundQuiescent != 1 || summary.AdoptedPrewarm != 0 {
		t.Fatalf("legacy prewarm summary = %#v, want conservative quiescent authority without authenticated adoption", summary)
	}
}

func TestCodexLeaseV2RemovalBlocksSameAccountAdmissionAndRevalidatesPending(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	manager := coordinator.leases.ForMode(9, true)
	accountA := codex.AccountKey("account-a")
	accountB := codex.AccountKey("account-b")
	keyA := testCodexRemovalLeaseKey("thread-a", "turn-a")
	keyB := testCodexRemovalLeaseKey("thread-b", "turn-b")
	acquireCodexRemovalProvisional(t, manager, keyA, accountA)
	acquireCodexRemovalProvisional(t, manager, keyB, accountB)

	removal, summary, err := coordinator.BeginAccountRemoval(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("provisional account summary = %#v, want no bound authority", summary)
	}

	pendingErr := errors.New("account removal pending")
	var checkedA atomic.Int32
	var persistedA atomic.Int32
	resultA := make(chan error, 1)
	go func() {
		_, err := manager.AdmitContext(context.Background(), keyA, accountA, 11, func(context.Context, codex.AccountKey) error {
			checkedA.Add(1)
			return pendingErr
		}, func([]CodexTurnLease) error {
			persistedA.Add(1)
			return nil
		})
		resultA <- err
	}()
	waitCodexAccountGateRefs(t, manager.accountGates, accountA, 2)
	if lease, found := manager.Get(keyA); !found || lease.State != LeaseProvisional {
		t.Fatalf("blocked admission state = %#v found=%v", lease, found)
	}
	if checkedA.Load() != 0 || persistedA.Load() != 0 {
		t.Fatal("blocked admission revalidated or persisted before removal released")
	}

	resultB := make(chan error, 1)
	go func() {
		_, err := manager.AdmitContext(context.Background(), keyB, accountB, 12, nil, func([]CodexTurnLease) error { return nil })
		resultB <- err
	}()
	select {
	case err := <-resultB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("different-account admission blocked behind removal")
	}

	removal.Release()
	removal.Release()
	select {
	case err := <-resultA:
		if !errors.Is(err, pendingErr) {
			t.Fatalf("post-removal admission error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("same-account admission did not resume for pending revalidation")
	}
	if checkedA.Load() != 1 || persistedA.Load() != 0 {
		t.Fatalf("post-removal calls = check %d persist %d, want 1/0", checkedA.Load(), persistedA.Load())
	}
	if lease, found := manager.Get(keyA); !found || lease.State != LeaseProvisional {
		t.Fatalf("rejected post-removal admission state = %#v found=%v", lease, found)
	}
}

func TestCodexLeaseV2RemovalWaitsForAdmissionPersistenceFailureCleanup(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	manager := coordinator.leases.ForMode(9, true)
	account := codex.AccountKey("account-admitting")
	key := testCodexRemovalLeaseKey("thread-admitting", "turn-admitting")
	acquireCodexRemovalProvisional(t, manager, key, account)

	persistEntered := make(chan struct{})
	persistRelease := make(chan struct{})
	persistErr := errors.New("injected persistence failure")
	admitResult := make(chan error, 1)
	go func() {
		_, err := manager.AdmitContext(context.Background(), key, account, 21, nil, func([]CodexTurnLease) error {
			close(persistEntered)
			<-persistRelease
			return persistErr
		})
		admitResult <- err
	}()
	<-persistEntered

	type removalResult struct {
		guard   CodexAccountRemovalGuard
		summary CodexBoundAuthoritySummary
		err     error
	}
	removed := make(chan removalResult, 1)
	go func() {
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), account)
		removed <- removalResult{guard: guard, summary: summary, err: err}
	}()
	waitCodexAccountGateRefs(t, manager.accountGates, account, 2)
	select {
	case result := <-removed:
		if result.guard != nil {
			result.guard.Release()
		}
		t.Fatal("removal acquired before admission persistence finished")
	default:
	}

	close(persistRelease)
	if err := <-admitResult; !errors.Is(err, persistErr) {
		t.Fatalf("admission persistence error = %T %v", err, err)
	}
	result := <-removed
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.guard.Release()
	if result.summary.BoundCount != 1 || result.summary.BoundActive != 1 {
		t.Fatalf("post-failure removal summary = %#v", result.summary)
	}
	if lease, found := manager.Get(key); !found || lease.State != LeaseBoundActive || !lease.NonMigratable {
		t.Fatalf("post-failure bound lease = %#v found=%v", lease, found)
	}
}

func TestCodexLeaseV2AccountGateCancelsWithoutABAAndKeepsAccountsIndependent(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	accountA := codex.AccountKey("account-a")
	accountB := codex.AccountKey("account-b")
	first, _, err := coordinator.BeginAccountRemoval(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		guard, _, err := coordinator.BeginAccountRemoval(ctx, accountA)
		if guard != nil {
			guard.Release()
		}
		cancelled <- err
	}()
	waitCodexAccountGateRefs(t, coordinator.leases.accountGates, accountA, 2)
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled removal error = %T %v", err, err)
	}

	other, _, err := coordinator.BeginAccountRemoval(context.Background(), accountB)
	if err != nil {
		t.Fatal(err)
	}
	other.Release()
	first.Release()
	first.Release()
	waitCodexAccountGateAbsent(t, coordinator.leases.accountGates, accountA)

	third, _, err := coordinator.BeginAccountRemoval(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	third.Release()
	waitCodexAccountGateAbsent(t, coordinator.leases.accountGates, accountA)
}

func TestCodexLeaseV2ForModeSharesGateAndSerialisesSameAccountPersistence(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(9, true, nil)
	view := manager.ForMode(8, true)
	if view.accountGates != manager.accountGates {
		t.Fatal("ForMode did not share the exact account gate set")
	}
	account := codex.AccountKey("shared-account")
	keyA := testCodexRemovalLeaseKey("thread-a", "turn-a")
	keyB := testCodexRemovalLeaseKey("thread-b", "turn-b")
	acquireCodexRemovalProvisional(t, manager, keyA, account)
	acquireCodexRemovalProvisional(t, view, keyB, account)

	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	var concurrent atomic.Int32
	var maximum atomic.Int32
	persist := func([]CodexTurnLease) error {
		active := concurrent.Add(1)
		for {
			previous := maximum.Load()
			if active <= previous || maximum.CompareAndSwap(previous, active) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		concurrent.Add(-1)
		return nil
	}
	results := make(chan error, 2)
	go func() {
		_, err := manager.AdmitContext(context.Background(), keyA, account, 31, nil, persist)
		results <- err
	}()
	go func() {
		_, err := view.AdmitContext(context.Background(), keyB, account, 32, nil, persist)
		results <- err
	}()
	<-entered
	waitCodexAccountGateRefs(t, manager.accountGates, account, 2)
	select {
	case <-entered:
		t.Fatal("same-account persistence callbacks overlapped")
	default:
	}
	release <- struct{}{}
	<-entered
	release <- struct{}{}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent same-account persistence = %d, want 1", maximum.Load())
	}
}

func TestCodexLeaseV2RemovalBlocksAuthoritativeRestore(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	manager := coordinator.leases.ForMode(9, true)
	account := codex.AccountKey("restored-account")
	key := testCodexRemovalLeaseKey("restored-thread", "restored-turn")
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		manager.Restore([]CodexTurnLease{{
			Key:           key,
			State:         LeaseBoundQuiescent,
			AccountKey:    account,
			ModeEpoch:     9,
			Authoritative: true,
			LastSeen:      time.Now(),
		}})
		close(done)
	}()
	waitCodexAccountGateRefs(t, manager.accountGates, account, 2)
	if _, found := manager.Get(key); found {
		t.Fatal("restore published bound authority while removal held the account gate")
	}
	guard.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restore did not resume after removal released")
	}
	if lease, found := manager.Get(key); !found || lease.State != LeaseOrphaned {
		t.Fatalf("restored lease = %#v found=%v", lease, found)
	}
}

func TestCodexLeaseV2RemovalSummarisesAuthenticatedUnhydratedAuthority(t *testing.T) {
	t.Parallel()
	coordinator, _ := openCodexLeaseV2RemovalTestCoordinator(t)
	store := coordinator.Store()
	account := codex.AccountKey("indexed-account")

	type fixture struct {
		thread        string
		state         LeaseState
		authoritative bool
		requestKind   CodexRequestKind
		account       codex.AccountKey
	}
	fixtures := []fixture{
		{thread: "active", state: LeaseBoundActive, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "continuation", state: LeaseContinuationPending, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "quiescent", state: LeaseBoundQuiescent, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "orphaned", state: LeaseOrphaned, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "shadow-active", state: LeaseBoundActive, authoritative: false, requestKind: CodexRequestTurn, account: account},
		{thread: "prewarm-without-handoff", state: LeaseProvisional, authoritative: true, requestKind: CodexRequestPrewarm, account: account},
		{thread: "provisional", state: LeaseProvisional, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "reserving", state: LeaseReserving, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "failed", state: LeaseFailedUnadmitted, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "superseded", state: LeaseSuperseded, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "expired", state: LeaseExpired, authoritative: true, requestKind: CodexRequestTurn, account: account},
		{thread: "other-account", state: LeaseBoundActive, authoritative: true, requestKind: CodexRequestTurn, account: "other-account"},
	}

	store.mu.Lock()
	next := cloneCodexLeaseV2Envelope(*store.v2)
	for _, fixture := range fixtures {
		lane, record := codexLeaseV2RemovalRecord(store, fixture.thread, fixture.state, fixture.authoritative, fixture.requestKind, fixture.account)
		next.Lanes = append(next.Lanes, lane)
		next.Records = append(next.Records, record)
	}
	// The mutable current request kind cannot erase immutable turn-level
	// admission authority. This represents an admitted turn whose later bounded
	// request happens to be prewarm-shaped; Task 10B still owns adopted-prewarm
	// classification for a distinct sentinel handoff.
	admittedPrewarmLane, admittedPrewarm := codexLeaseV2RemovalRecord(store, "admitted-then-prewarm", LeaseBoundActive, true, CodexRequestPrewarm, account)
	admittedPrewarm.EverAdmitted = true
	admittedPrewarm.AdmissionJournalGeneration = store.v2.Generation + 1
	admittedPrewarm.Generation = 2
	admittedPrewarm.AdmissionRequestGeneration = 1
	admittedPrewarm.AdmissionRequestKind = CodexRequestTurn
	admittedPrewarm.AdmittedAt = admittedPrewarm.Attempts[0].CreatedAt
	admittedPrewarmLane.LastAdmittedAccountHash = admittedPrewarm.AccountHash
	admittedPrewarmLane.LastAdmittedTurnHash = admittedPrewarm.TurnHash
	admittedPrewarmLane.LastAdmittedModeEpoch = admittedPrewarm.ModeEpoch
	admittedPrewarmLane.LastAdmittedAuthoritative = true
	admittedPrewarmLane.LastAdmissionJournalGeneration = admittedPrewarm.AdmissionJournalGeneration
	admittedPrewarmLane.LastAdmittedAt = admittedPrewarm.AdmittedAt
	next.Lanes = append(next.Lanes, admittedPrewarmLane)
	next.Records = append(next.Records, admittedPrewarm)
	// A retained lane may remember affinity for an admitted record that has
	// already been pruned. Neither that affinity nor the unrelated current
	// request is authenticated bound authority for the target account.
	affinityLane, affinityRecord := codexLeaseV2RemovalRecord(store, "affinity-only", LeaseProvisional, true, CodexRequestTurn, "affinity-current-account")
	affinityLane.LastAdmittedAccountHash = store.hash("account", string(account))
	affinityLane.LastAdmittedTurnHash = store.hash("turn", "pruned-admitted-affinity-only")
	affinityLane.LastAdmittedModeEpoch = 9
	affinityLane.LastAdmittedAuthoritative = true
	affinityLane.LastAdmissionJournalGeneration = store.v2.Generation + 1
	affinityLane.LastAdmittedAt = affinityLane.LastObservedAt.Add(-time.Minute)
	next.Lanes = append(next.Lanes, affinityLane)
	next.Records = append(next.Records, affinityRecord)
	err := store.commitV2Locked(store.v2.Generation, next)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	// Live restored authority replaces, rather than duplicates, the matching
	// durable active record.
	coordinator.leases.Restore([]CodexTurnLease{{
		Key:           testCodexRemovalLeaseKey("active", "turn-active"),
		State:         LeaseBoundActive,
		AccountKey:    account,
		ModeEpoch:     9,
		Authoritative: true,
		LastSeen:      time.Now(),
	}})
	// A newer live provisional view is not allowed to erase the still-counted
	// authenticated durable quiescent record.
	acquireCodexRemovalProvisional(t, coordinator.leases, testCodexRemovalLeaseKey("quiescent", "turn-quiescent"), account)

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	want := CodexBoundAuthoritySummary{
		JournalGeneration:   store.Generation(),
		BoundCount:          5,
		BoundActive:         1,
		ContinuationPending: 1,
		BoundQuiescent:      1,
		OrphanedOrRestored:  2,
	}
	if summary != want {
		t.Fatalf("authenticated account summary = %#v, want %#v", summary, want)
	}
}

func TestCodexLeaseV2RemovalFailsClosedOnUntrustedAuthority(t *testing.T) {
	t.Run("legacy quarantine", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		now := time.Date(2026, 8, 9, 4, 5, 6, 0, time.UTC)
		writeCodexLeaseV1Fixture(t, fsys)
		coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
			FS:          fsys,
			KeyPath:     "/state/leases.key",
			JournalPath: "/state/leases.json",
			Policy:      CodexLeasePolicy{Retention: time.Hour, Now: func() time.Time { return now }},
			Modes:       CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
		}, &cutoverTestOwner{})
		if err != nil {
			t.Fatal(err)
		}
		defer coordinator.Close()
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLegacyQuarantine) {
			t.Fatalf("legacy-quarantine result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("empty account", func(t *testing.T) {
		coordinator := openCodexLeaseV2LaneTestCoordinator(t)
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
			t.Fatalf("empty-account result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("revoked owner releases account gate", func(t *testing.T) {
		coordinator := openCodexLeaseV2LaneTestCoordinator(t)
		store := coordinator.Store()
		originalOwner := store.owner
		ownerErr := errors.New("owner revoked")
		store.owner = &cutoverTestOwner{beginErr: ownerErr}
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, ownerErr) {
			t.Fatalf("revoked-owner result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
		store.owner = originalOwner
		guard, _, err = coordinator.BeginAccountRemoval(context.Background(), "account")
		if err != nil {
			t.Fatalf("account gate leaked after owner failure: %v", err)
		}
		guard.Release()
	})

	t.Run("poison", func(t *testing.T) {
		coordinator := openCodexLeaseV2LaneTestCoordinator(t)
		coordinator.Store().mu.Lock()
		coordinator.Store().poisoned = errors.New("indeterminate commit")
		coordinator.Store().mu.Unlock()
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseStorePoisoned) {
			t.Fatalf("poisoned result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("unrecognised authoritative epoch", func(t *testing.T) {
		coordinator, _ := openCodexLeaseV2RemovalTestCoordinator(t)
		store := coordinator.Store()
		store.mu.Lock()
		next := cloneCodexLeaseV2Envelope(*store.v2)
		lane, record := codexLeaseV2RemovalRecord(store, "unrecognised", LeaseOrphaned, true, CodexRequestTurn, "account")
		record.ModeEpoch = 10
		lane.CurrentModeEpoch = 10
		lane.LastModeEpoch = 10
		next.Lanes = append(next.Lanes, lane)
		next.Records = append(next.Records, record)
		err := store.commitV2Locked(store.v2.Generation, next)
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
			t.Fatalf("unrecognised-mode result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("replaced journal", func(t *testing.T) {
		coordinator, fsys := openCodexLeaseV2RemovalTestCoordinator(t)
		if err := fsys.WriteFile("/state/leases.json", []byte(`{"version":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseTrustLost) {
			t.Fatalf("replaced-journal result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		coordinator := openCodexLeaseV2LaneTestCoordinator(t)
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("closed result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})

	t.Run("nil coordinator", func(t *testing.T) {
		var coordinator *CodexContinuityCoordinator
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
		if guard != nil || summary != (CodexBoundAuthoritySummary{}) || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("nil result = guard %T summary %#v error %T %v", guard, summary, err, err)
		}
	})
}

func acquireCodexRemovalProvisional(t *testing.T, manager *CodexTurnLeaseManager, key LeaseKey, account codex.AccountKey) {
	t.Helper()
	lease, err := manager.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
		return RouteChoice{AccountKey: account}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.State != LeaseProvisional || lease.AccountKey != account {
		t.Fatalf("provisional lease = %#v", lease)
	}
}

func testCodexRemovalLeaseKey(thread, turn string) LeaseKey {
	return LeaseKey{Lane: LaneKey{Session: "removal-session", Thread: thread, Namespace: CodexResponsesNamespace}, Turn: turn}
}

func openCodexLeaseV2RemovalTestCoordinator(t *testing.T) (*CodexContinuityCoordinator, *fsutil.MemFS) {
	t.Helper()
	store, fsys, _ := openCodexLeaseV2CASTestStore(t)
	store.mu.Lock()
	store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}}
	store.mu.Unlock()
	leases := NewCodexTurnLeaseManager(9, true, store.policy.Now)
	return &CodexContinuityCoordinator{store: store, leases: leases}, fsys
}

func codexLeaseV2RemovalRecord(store *CodexLeaseStore, thread string, state LeaseState, authoritative bool, requestKind CodexRequestKind, account codex.AccountKey) (CodexJournalLane, CodexJournalRecordV2) {
	now := store.policy.Now().UTC()
	createdAt := now.Add(-2 * time.Minute)
	attemptAt := now.Add(-time.Minute)
	sessionHash := store.hash("session", "removal-session")
	threadHash := store.hash("thread", thread)
	namespaceHash := store.hash("namespace", CodexResponsesNamespace)
	turnHash := store.hash("turn", "turn-"+thread)
	record := CodexJournalRecordV2{
		SessionHash:          sessionHash,
		ThreadHash:           threadHash,
		NamespaceHash:        namespaceHash,
		TurnHash:             turnHash,
		RecordGeneration:     1,
		LaneGeneration:       1,
		LeaseGeneration:      1,
		ModeEpoch:            9,
		State:                state,
		ProtocolSchema:       CurrentCodexLeaseSchema,
		Authoritative:        authoritative,
		SocketLineageExtinct: state == LeaseOrphaned || state == LeaseReserving || state == LeaseFailedUnadmitted || state == LeaseSuperseded || state == LeaseExpired,
		NonMigratable:        state == LeaseOrphaned,
		CreatedAt:            createdAt,
		LastObservedAt:       now,
	}
	if state != LeaseReserving {
		record.Generation = 1
		record.RequestKind = requestKind
		accountHash := store.hash("account", string(account))
		slots := []CodexAttemptSlot{{Index: 1, AccountHash: accountHash, CandidateHash: store.hash("candidate", "candidate-"+thread), Kind: CodexAttemptSlotDirect}}
		record.AccountHash = accountHash
		record.RequestedModelHash = store.hash("requested-model", "gpt-requested")
		record.EffectiveModel = "gpt-effective"
		record.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
		record.CurrentAttemptGeneration = 1
		record.AttemptEnvelope = CodexAttemptEnvelope{
			PolicyVersion: CodexLeaseAttemptPolicyVersion,
			PlanDigest:    codexLeaseAttemptPlanDigest(store.key, slots),
			AttemptLimit:  1,
			Slots:         slots,
		}
		attemptState := CodexAttemptProviderFailed
		switch state {
		case LeaseProvisional:
			attemptState = CodexAttemptPrepared
		case LeaseBoundActive:
			attemptState = CodexAttemptStreaming
		case LeaseContinuationPending, LeaseBoundQuiescent:
			attemptState = CodexAttemptProviderCompleted
		case LeaseOrphaned:
			attemptState = CodexAttemptIndeterminate
		}
		record.Attempts = []CodexJournalAttempt{{
			Generation:     1,
			Revision:       1,
			Slot:           1,
			State:          attemptState,
			CreatedAt:      attemptAt,
			LastObservedAt: now,
		}}
	}
	if state == LeaseBoundActive {
		record.RoutingRefs = 1
		record.AttemptRefs = 1
	}
	lane := CodexJournalLane{
		SessionHash:       sessionHash,
		ThreadHash:        threadHash,
		NamespaceHash:     namespaceHash,
		Generation:        1,
		LastTurnHash:      turnHash,
		LastModeEpoch:     9,
		LastAuthoritative: authoritative,
		LastObservedAt:    now,
	}
	if state != LeaseFailedUnadmitted && state != LeaseSuperseded && state != LeaseExpired {
		lane.CurrentTurnHash = turnHash
		lane.CurrentModeEpoch = 9
		lane.CurrentAuthoritative = authoritative
	}
	if authoritative && requestKind != CodexRequestPrewarm && (state == LeaseBoundActive || state == LeaseContinuationPending || state == LeaseBoundQuiescent) {
		record.EverAdmitted = true
		record.AdmissionJournalGeneration = store.v2.Generation + 1
		record.AdmissionRequestGeneration = record.Generation
		record.AdmissionRequestKind = record.RequestKind
		record.AdmissionCompactionPhase = record.CompactionPhase
		record.AdmittedAt = attemptAt
		lane.LastAdmittedAccountHash = record.AccountHash
		lane.LastAdmittedTurnHash = record.TurnHash
		lane.LastAdmittedModeEpoch = record.ModeEpoch
		lane.LastAdmittedAuthoritative = true
		lane.LastAdmissionJournalGeneration = record.AdmissionJournalGeneration
		lane.LastAdmittedAt = record.AdmittedAt
	}
	return lane, record
}

func waitCodexAccountGateRefs(t *testing.T, set *codexAccountGateSet, account codex.AccountKey, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		set.mu.Lock()
		entry := set.entries[account]
		got := 0
		if entry != nil {
			got = entry.refs
		}
		set.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("account gate refs did not reach %d", want)
}

func waitCodexAccountGateAbsent(t *testing.T, set *codexAccountGateSet, account codex.AccountKey) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		set.mu.Lock()
		_, found := set.entries[account]
		set.mu.Unlock()
		if !found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("account gate entry was not released")
}
