package proxy

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2CloseClearsOwnedBuffersAndBlocksPublicOperations(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writeCodexLeaseV1Fixture(t, fsys)
	coordinator, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.Store()
	manager := coordinator.leases.ForMode(4, true)
	leaseKey := testCodexLeaseKey("close-thread", "close-turn")
	lease, err := manager.AcquireRoute(context.Background(), leaseKey, func(context.Context) (RouteChoice, error) {
		return RouteChoice{AccountKey: "close-account", RequiredBuckets: []CapacityBucket{CapacityBucketBase}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	managed := manager.leases[leaseKey]
	buckets := managed.lease.Choice.RequiredBuckets
	key := store.key
	journal := store.journalBytes
	archive := store.legacyArchiveBytes
	store.mu.Lock()
	store.records = []CodexJournalRecord{{SessionHash: "legacy-session", AccountHash: "legacy-account"}}
	store.v2.Lanes = []CodexJournalLane{{SessionHash: "lane-session", ThreadHash: "lane-thread", NamespaceHash: "lane-namespace"}}
	store.v2.Records = []CodexJournalRecordV2{{
		SessionHash: "record-session",
		AccountHash: "record-account",
		CodexCurrentRequest: CodexCurrentRequest{
			RequiredBuckets: []CapacityBucket{CapacityBucketBase},
			AttemptEnvelope: CodexAttemptEnvelope{Slots: []CodexAttemptSlot{{AccountHash: "slot-account", CandidateHash: "slot-candidate"}}},
			Attempts:        []CodexJournalAttempt{{Generation: 1, State: CodexAttemptPrepared}},
		},
	}}
	legacyRecords := store.records
	v2 := store.v2
	lanes := store.v2.Lanes
	records := store.v2.Records
	authoritativeEpochs := store.v2.Cutover.AuthoritativeModeEpochs
	shadowEpochs := store.v2.Cutover.ShadowModeEpochs
	modes := store.modes.RecognisedAuthoritativeEpochs
	requiredBuckets := store.v2.Records[0].RequiredBuckets
	slots := store.v2.Records[0].AttemptEnvelope.Slots
	attempts := store.v2.Records[0].Attempts
	store.mu.Unlock()
	generation := store.Generation()
	if len(key) == 0 || len(journal) == 0 || len(archive) == 0 {
		t.Fatal("open store does not own expected key/journal/archive buffers")
	}
	if _, exportedClose := any(store).(interface{ Close() error }); exportedClose {
		t.Fatal("coordinator-owned durable store exposes an independent Close authority")
	}

	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	if store.key != nil || store.journalBytes != nil || store.legacyArchiveBytes != nil {
		t.Fatalf("closed store retained owned buffers: key=%d journal=%d archive=%d", len(store.key), len(store.journalBytes), len(store.legacyArchiveBytes))
	}
	if !allZeroCodexLeaseTestBytes(key) || !allZeroCodexLeaseTestBytes(journal) || !allZeroCodexLeaseTestBytes(archive) {
		t.Fatal("closed store did not clear owned buffer backing arrays")
	}
	if store.Generation() != generation {
		t.Fatalf("closed diagnostic generation = %d, want %d", store.Generation(), generation)
	}
	if store.v2 != nil || store.records != nil || store.modes.RecognisedAuthoritativeEpochs != nil || store.owner != nil || store.fs != nil || store.inspector != nil || store.policy.Retention != 0 || store.policy.Now != nil {
		t.Fatalf("closed store retained parsed authority or capabilities: v2=%p records=%d modes=%v owner=%T fs=%T inspector=%T policy=%#v", store.v2, len(store.records), store.modes.RecognisedAuthoritativeEpochs, store.owner, store.fs, store.inspector, store.policy)
	}
	if !reflect.DeepEqual(*v2, codexLeaseJournalEnvelopeV2{}) || !reflect.DeepEqual(lanes[0], CodexJournalLane{}) || !reflect.DeepEqual(records[0], CodexJournalRecordV2{}) || !reflect.DeepEqual(legacyRecords[0], CodexJournalRecord{}) || !allZeroCodexLeaseTestUint64s(authoritativeEpochs) || !allZeroCodexLeaseTestUint64s(shadowEpochs) || !allZeroCodexLeaseTestUint64s(modes) || !allZeroCodexLeaseTestBuckets(requiredBuckets) || !allZeroCodexLeaseTestSlots(slots) || !allZeroCodexLeaseTestAttempts(attempts) {
		t.Fatal("closed store did not clear parsed authority backing arrays")
	}
	if got := coordinator.Store(); got != store {
		t.Fatalf("closed coordinator store alias = %p, want exact opened store %p", got, store)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("closed coordinator retained manager authority: %#v", got)
	}
	if !reflect.DeepEqual(*managed, codexManagedLease{}) || !allZeroCodexLeaseTestBuckets(buckets) {
		t.Fatalf("closed coordinator retained managed lease: %#v buckets=%v", *managed, buckets)
	}
	if _, found := manager.Get(leaseKey); found {
		t.Fatal("closed manager Get found cleared authority")
	}
	if _, err := manager.Acquire(context.Background(), leaseKey, fixedCodexAccount(lease.AccountKey)); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed manager Acquire error = %T %v", err, err)
	}
	manager.Restore([]CodexTurnLease{lease})
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("closed manager Restore repopulated authority: %#v", got)
	}
	if epoch, authoritative := manager.ForMode(5, true).Mode(); epoch != 0 || authoritative {
		t.Fatalf("closed mode alias = (%d, %t), want unavailable", epoch, authoritative)
	}
	if _, err := NewCodexPrewarmManager(manager, nil).Adopt(testCodexLeaseKey("closed-prewarm", "turn"), "correlation"); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed prewarm adoption error = %T %v", err, err)
	}
	coordinatorType := reflect.TypeOf(CodexContinuityCoordinator{})
	if _, found := coordinatorType.FieldByName("Store"); found {
		t.Fatal("coordinator exposes a replaceable Store field")
	}
	if _, found := coordinatorType.FieldByName("Leases"); found {
		t.Fatal("coordinator exposes a replaceable lease-manager field")
	}

	if _, _, found := store.Lookup(testCodexLeaseKey("thread", "turn"), nil); found {
		t.Fatal("closed store legacy lookup succeeded")
	}
	if err := store.CommitCurrentLeases(nil); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed legacy commit error = %T %v, want writer unavailable", err, err)
	}
	if err := store.Compact(testCodexContinuityOptions(fsys).Policy.Now(), testCodexContinuityOptions(fsys).Policy.Retention); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed compact error = %T %v, want writer unavailable", err, err)
	}
	if _, err := store.LoadLane(testCodexLeaseKey("thread", "turn"), nil, CodexLeaseAuthorityPolicy{ModeEpoch: 4, Authoritative: true}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed LoadLane error = %T %v, want writer unavailable", err, err)
	}
	if _, err := store.AuthorityEvidence(); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed evidence error = %T %v, want writer unavailable", err, err)
	}
	if guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account"); guard != nil || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed removal result = guard %T error %T %v", guard, err, err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("repeated close error = %v", err)
	}
}

func TestCodexLeaseV2ClosedStoreDoesNotAcquireOwnerAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writeCodexLeaseV1Fixture(t, fsys)
	owner := &countingCodexLeaseTestOwner{}
	coordinator, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), owner)
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.Store()
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	wantAsserts, wantBegins := owner.asserts, owner.begins

	if _, err := store.CommitLane(CodexLeaseGenerationFence{}, CodexLaneMutation{}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed CommitLane error = %T %v", err, err)
	}
	if _, err := store.LoadLane(testCodexLeaseKey("closed-owner", "turn"), nil, CodexLeaseAuthorityPolicy{ModeEpoch: 4, Authoritative: true}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed LoadLane error = %T %v", err, err)
	}
	if _, err := store.CompleteLegacyCutover(store.Generation(), CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed CompleteLegacyCutover error = %T %v", err, err)
	}
	if _, err := store.AuthorityEvidence(); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed AuthorityEvidence error = %T %v", err, err)
	}
	if err := store.Compact(time.Now(), time.Hour); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("closed Compact error = %T %v", err, err)
	}
	if owner.asserts != wantAsserts || owner.begins != wantBegins {
		t.Fatalf("closed store acquired owner authority: asserts %d->%d begins %d->%d", wantAsserts, owner.asserts, wantBegins, owner.begins)
	}
}

func TestCodexLeaseV2CoordinatorCloseDrainsOwnerAcquisition(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writeCodexLeaseV1Fixture(t, fsys)
	owner := &blockingCodexLeaseCloseOwner{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), owner)
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.Store()
	owner.block.Store(true)
	operation := make(chan error, 1)
	go func() {
		_, err := store.AuthorityEvidence()
		operation <- err
	}()
	select {
	case <-owner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owner acquisition did not block")
	}

	closed := make(chan error, 1)
	go func() { closed <- coordinator.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("coordinator Close crossed an active owner acquisition: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(owner.release)
	select {
	case err := <-operation:
		if err != nil {
			t.Fatalf("pre-close owner operation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner operation did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator Close did not drain owner acquisition")
	}
	wantAsserts, wantBegins := owner.asserts.Load(), owner.begins.Load()
	if _, err := store.AuthorityEvidence(); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("post-close evidence error = %T %v", err, err)
	}
	if owner.asserts.Load() != wantAsserts || owner.begins.Load() != wantBegins {
		t.Fatal("post-close store re-entered owner authority")
	}
}

func TestCodexLeaseV2CoordinatorCloseDuringSelectorCannotResurrectAuthority(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	manager := coordinator.leases.ForMode(9, true)
	key := testCodexLeaseKey("close-selector", "turn")
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := manager.AcquireRoute(context.Background(), key, func(context.Context) (RouteChoice, error) {
			close(entered)
			<-release
			return RouteChoice{AccountKey: "selector-account", RequiredBuckets: []CapacityBucket{CapacityBucketBase}}, nil
		})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("selector did not block")
	}

	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	if !store.closed {
		t.Fatal("coordinator returned from Close before closing its exact store")
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("selector result after coordinator Close = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selector did not finish after release")
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("selector resurrected authority after coordinator Close: %#v", got)
	}
}

func TestCodexLeaseV2CoordinatorCloseWaitsForAdmissionPersistence(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	manager := coordinator.leases.ForMode(9, true)
	key := testCodexLeaseKey("close-persist", "turn")
	account := codex.AccountKey("persist-account")
	if _, err := manager.Acquire(context.Background(), key, fixedCodexAccount(account)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	admitted := make(chan error, 1)
	var persistCalls atomic.Int32
	go func() {
		_, err := manager.AdmitContext(context.Background(), key, account, 31, nil, func([]CodexTurnLease) error {
			persistCalls.Add(1)
			close(entered)
			<-release
			return nil
		})
		admitted <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission persistence did not block")
	}

	closed := make(chan error, 1)
	go func() { closed <- coordinator.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("coordinator Close crossed active persistence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if store.closed {
		t.Fatal("store closed before active persistence completed")
	}
	close(release)
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("pre-close admitted persistence error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission persistence did not complete")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator Close did not finish after persistence")
	}
	if !store.closed || len(manager.Snapshot()) != 0 {
		t.Fatalf("coordinator close boundary = store closed %t manager %#v", store.closed, manager.Snapshot())
	}
	if _, err := manager.Admit(key, account, 32, func([]CodexTurnLease) error {
		persistCalls.Add(1)
		return nil
	}); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("post-close Admit error = %T %v", err, err)
	}
	if persistCalls.Load() != 1 {
		t.Fatalf("post-close persistence calls = %d, want 1", persistCalls.Load())
	}
}

func TestCodexLeaseV2CoordinatorCloseWinsAgainstRemovalWaitingForAccountGate(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	manager := coordinator.leases.ForMode(9, true)
	account := codex.AccountKey("removal-close-account")
	blocking, err := manager.accountGates.acquire(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), account)
		if guard != nil {
			guard.Release()
		}
		result <- err
	}()
	waitCodexAccountGateRefs(t, manager.accountGates, account, 2)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("post-close removal error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal did not finish when coordinator Close revoked the account gates")
	}
	blocking.Release()
	manager.accountGates.mu.Lock()
	gateCount := len(manager.accountGates.entries)
	manager.accountGates.mu.Unlock()
	if gateCount != 0 {
		t.Fatalf("closed coordinator retained %d account gate identities", gateCount)
	}
}

func allZeroCodexLeaseTestBytes(value []byte) bool {
	return bytes.Equal(value, make([]byte, len(value)))
}

func allZeroCodexLeaseTestBuckets(value []CapacityBucket) bool {
	for _, bucket := range value {
		if bucket != "" {
			return false
		}
	}
	return true
}

func allZeroCodexLeaseTestUint64s(value []uint64) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func allZeroCodexLeaseTestSlots(value []CodexAttemptSlot) bool {
	for _, item := range value {
		if !reflect.DeepEqual(item, CodexAttemptSlot{}) {
			return false
		}
	}
	return true
}

func allZeroCodexLeaseTestAttempts(value []CodexJournalAttempt) bool {
	for _, item := range value {
		if !reflect.DeepEqual(item, CodexJournalAttempt{}) {
			return false
		}
	}
	return true
}

type blockingCodexLeaseCloseOwner struct {
	block   atomic.Bool
	asserts atomic.Int32
	begins  atomic.Int32
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (owner *blockingCodexLeaseCloseOwner) AssertOwner() error {
	owner.asserts.Add(1)
	return nil
}

func (owner *blockingCodexLeaseCloseOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	owner.begins.Add(1)
	if owner.block.Load() {
		owner.once.Do(func() { close(owner.entered) })
		<-owner.release
	}
	return &codex.CredentialOwnerOperation{}, nil
}
