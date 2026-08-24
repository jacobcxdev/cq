package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2AdmissionEvidenceStampedOnceWithStreamingCAS(t *testing.T) {
	t.Parallel()
	store, _, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "affinity-session", "affinity-thread", "first-turn")
	record.ModeEpoch = 9
	record.Authoritative = true
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, stored := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	if stored.EverAdmitted || stored.AdmissionJournalGeneration != 0 || !stored.AdmittedAt.IsZero() {
		t.Fatalf("provisional record gained admission evidence: %#v", stored)
	}

	*now = now.Add(1)
	dispatched := codexLeaseV2CASTestMutationRecord(stored)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: stored.Generation, Generation: stored.Attempts[0].Generation, Revision: stored.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if stored.EverAdmitted {
		t.Fatal("dispatched provisional record gained admission evidence")
	}

	*now = now.Add(1)
	streaming := codexLeaseV2CASTestMutationRecord(stored)
	streaming.State = LeaseBoundActive
	streaming.RoutingRefs = 1
	streaming.AttemptRefs = 1
	streaming.Attempts[0].State = CodexAttemptStreaming
	streamingFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	streamingFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: stored.Generation, Generation: stored.Attempts[0].Generation, Revision: stored.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{streamingFence}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
	if err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	admittedGeneration := fence.Journal
	admittedAt := *now
	if !stored.EverAdmitted || stored.AdmissionJournalGeneration != admittedGeneration || stored.AdmittedAt != admittedAt {
		t.Fatalf("first admission evidence = admitted %v generation %d at %v, want true/%d/%v", stored.EverAdmitted, stored.AdmissionJournalGeneration, stored.AdmittedAt, admittedGeneration, admittedAt)
	}
	lane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, record.Identity().LaneDigest)
	if lane.LastAdmittedAccountHash != stored.AccountHash || lane.LastAdmittedTurnHash != stored.TurnHash || lane.LastAdmittedModeEpoch != stored.ModeEpoch || !lane.LastAdmittedAuthoritative || lane.LastAdmissionJournalGeneration != admittedGeneration || lane.LastAdmittedAt != admittedAt {
		t.Fatalf("lane admission affinity = %#v", lane)
	}

	*now = now.Add(1)
	completed := codexLeaseV2CASTestMutationRecord(stored)
	completed.State = LeaseBoundQuiescent
	completed.RoutingRefs = 0
	completed.AttemptRefs = 0
	completed.Attempts[0].State = CodexAttemptProviderCompleted
	completedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	completedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: stored.Generation, Generation: stored.Attempts[0].Generation, Revision: stored.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{completedFence}
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{completed}}); err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	lane = findCodexLeaseV2CASTestLane(t, store.v2.Lanes, record.Identity().LaneDigest)
	if !stored.EverAdmitted || stored.AdmissionJournalGeneration != admittedGeneration || stored.AdmittedAt != admittedAt || lane.LastAdmissionJournalGeneration != admittedGeneration || lane.LastAdmittedAt != admittedAt {
		t.Fatalf("terminal update changed first-admission evidence: record %#v lane %#v", stored, lane)
	}
}

func TestCodexLeaseV2AffinityEligibilityKeepsShadowTurnExact(t *testing.T) {
	tests := []struct {
		name         string
		kind         CodexRequestKind
		phase        CodexCompactionPhase
		authority    bool
		wantEvidence bool
		wantAffinity bool
	}{
		{name: "turn", kind: CodexRequestTurn, authority: true, wantEvidence: true, wantAffinity: true},
		{name: "standalone compaction", kind: CodexRequestCompaction, phase: CodexCompactionStandalone, authority: true, wantEvidence: true, wantAffinity: true},
		{name: "pre-turn compaction", kind: CodexRequestCompaction, phase: CodexCompactionPreTurn, authority: true, wantEvidence: true, wantAffinity: true},
		{name: "shadow turn", kind: CodexRequestTurn, authority: false, wantEvidence: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, record, _ := openCodexLeaseV2AffinityVariantTestStore(t, test.kind, test.phase, test.authority)
			if record.EverAdmitted != test.wantEvidence {
				t.Fatalf("record evidence = %v, want %v: %#v", record.EverAdmitted, test.wantEvidence, record)
			}
			if test.wantEvidence && (record.AdmissionJournalGeneration != fence.Journal || record.AdmittedAt.IsZero()) {
				t.Fatalf("record evidence generation/time = %#v, post fence %#v", record, fence)
			}
			lane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, record.Identity().LaneDigest)
			if got := !codexLaneAffinityIsZero(lane); got != test.wantAffinity {
				t.Fatalf("lane affinity present = %v, want %v: %#v", got, test.wantAffinity, lane)
			}
		})
	}
}

func TestCodexLeaseV2RejectsMidTurnFirstAdmissionWithoutWrite(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "mid-turn-session", "mid-turn-thread", "mid-turn")
	record.ModeEpoch = 9
	record.Authoritative = true
	record.RequestKind = CodexRequestCompaction
	record.CompactionPhase = CodexCompactionMidTurn
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	*now = now.Add(time.Second)
	dispatched := codexLeaseV2CASTestMutationRecord(record)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	var err error
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = now.Add(time.Second)
	streaming := codexLeaseV2CASTestMutationRecord(record)
	streaming.State = LeaseBoundActive
	streaming.RoutingRefs = 1
	streaming.AttemptRefs = 1
	streaming.Attempts[0].State = CodexAttemptStreaming
	streamingFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	streamingFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{streamingFence}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("mid-turn first admission error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("mid-turn first admission changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2SuccessorAdmissionReplacesAffinityButFailureDoesNot(t *testing.T) {
	store, fence, first, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	firstAdmissionGeneration := first.AdmissionJournalGeneration
	firstAdmittedAt := first.AdmittedAt
	*now = now.Add(1)
	fence, first = commitCodexLeaseV2AffinityState(t, store, fence, first, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)

	*now = now.Add(1)
	fence, successor := appendCodexLeaseV2AffinitySuccessor(t, store, fence, first, "successful-successor")
	*now = now.Add(1)
	fence, successor = commitCodexLeaseV2AffinityState(t, store, fence, successor, LeaseProvisional, CodexAttemptDispatched, false)
	*now = now.Add(1)
	fence, successor = commitCodexLeaseV2AffinityState(t, store, fence, successor, LeaseBoundActive, CodexAttemptStreaming, true)
	lane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, successor.Identity().LaneDigest)
	if lane.LastAdmittedTurnHash != successor.TurnHash || lane.LastAdmissionJournalGeneration != fence.Journal || successor.AdmissionJournalGeneration != fence.Journal || successor.AdmittedAt != *now || lane.LastCacheAdmittedAt != *now || lane.LastCacheEffectiveModel != successor.EffectiveModel {
		t.Fatalf("successor admission did not replace affinity: fence %#v record %#v lane %#v", fence, successor, lane)
	}
	cacheAdmittedAt := lane.LastCacheAdmittedAt
	cacheEffectiveModel := lane.LastCacheEffectiveModel
	retainedFirst := findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.Identity())
	if retainedFirst.AdmissionJournalGeneration != firstAdmissionGeneration || retainedFirst.AdmittedAt != firstAdmittedAt {
		t.Fatalf("successor admission changed predecessor evidence: %#v", retainedFirst)
	}

	*now = now.Add(1)
	fence, successor = commitCodexLeaseV2AffinityState(t, store, fence, successor, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)
	*now = now.Add(1)
	fence, failed := appendCodexLeaseV2AffinitySuccessor(t, store, fence, successor, "failed-successor")
	*now = now.Add(1)
	fence, failed = commitCodexLeaseV2AffinityState(t, store, fence, failed, LeaseProvisional, CodexAttemptDispatched, false)
	*now = now.Add(1)
	_, failed = commitCodexLeaseV2AffinityState(t, store, fence, failed, LeaseFailedUnadmitted, CodexAttemptProviderFailed, false)
	lane = findCodexLeaseV2CASTestLane(t, store.v2.Lanes, failed.Identity().LaneDigest)
	if failed.EverAdmitted || lane.LastAdmittedTurnHash != successor.TurnHash || lane.LastAdmissionJournalGeneration != successor.AdmissionJournalGeneration || lane.LastAdmittedAt != successor.AdmittedAt || lane.LastCacheAdmittedAt != cacheAdmittedAt || lane.LastCacheEffectiveModel != cacheEffectiveModel {
		t.Fatalf("failed successor changed affinity: failed %#v successor %#v lane %#v", failed, successor, lane)
	}
}

func TestCodexLeaseV2RejectsLateHistoricalFirstAdmissionWithoutWrite(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	historical := provisionalCodexLeaseV2CASTestRecord(store, "affinity-helper-session", "affinity-helper-thread", "historical-turn")
	historical.ModeEpoch = 9
	historical.Authoritative = true
	historical.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, historical := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, historical)
	*now = now.Add(1)
	dispatched := codexLeaseV2CASTestMutationRecord(historical)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, historical.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: historical.Generation, Generation: historical.Attempts[0].Generation, Revision: historical.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	historical = findCodexLeaseV2CASTestRecord(t, store.v2.Records, historical.Identity())
	*now = now.Add(1)
	fence, historical = commitCodexLeaseV2AffinityState(t, store, fence, historical, LeaseFailedUnadmitted, CodexAttemptProviderFailed, false)
	*now = now.Add(1)
	fence, _ = appendCodexLeaseV2AffinitySuccessor(t, store, fence, historical, "current-turn")

	*now = now.Add(1)
	late := codexLeaseV2CASTestMutationRecord(historical)
	late.State = LeaseBoundActive
	late.RoutingRefs = 1
	late.AttemptRefs = 1
	late.Attempts[0].State = CodexAttemptStreaming
	lateFence := codexLeaseV2CASTestRecordFence(store.v2.Records, historical.Identity())
	lateFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: historical.Generation, Generation: historical.Attempts[0].Generation, Revision: historical.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{lateFence}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{late}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("late historical admission error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) {
		t.Fatal("late historical admission changed durable authority")
	}
}

func TestCodexLeaseV2ConcurrentFirstAdmissionHasOneCASWinner(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "affinity-race-session", "affinity-race-thread", "affinity-race-turn")
	record.ModeEpoch = 9
	record.Authoritative = true
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	*now = now.Add(1)
	dispatched := codexLeaseV2CASTestMutationRecord(record)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = now.Add(1)
	streaming := codexLeaseV2CASTestMutationRecord(record)
	streaming.State = LeaseBoundActive
	streaming.RoutingRefs = 1
	streaming.AttemptRefs = 1
	streaming.Attempts[0].State = CodexAttemptStreaming
	streamingFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	streamingFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{streamingFence}

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	winners := 0
	stale := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrCodexLeaseStaleMutation):
			stale++
		default:
			t.Fatalf("concurrent admission error = %T %v", err, err)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("concurrent admission results = winners %d stale %d", winners, stale)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	lane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, record.Identity().LaneDigest)
	if !stored.EverAdmitted || stored.AdmissionJournalGeneration != store.Generation() || lane.LastAdmissionJournalGeneration != store.Generation() {
		t.Fatalf("concurrent winner evidence = generation %d record %#v lane %#v", store.Generation(), stored, lane)
	}
}

func TestCodexLeaseV2AdmissionCommitOutcomesDoNotPublishFalseEvidence(t *testing.T) {
	t.Run("not committed", func(t *testing.T) {
		store, fence, record, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
		before := append([]byte(nil), store.journalBytes...)
		beforeGeneration := store.Generation()
		store.directory = &failingSecureDirectory{
			SecureDirectory: store.directory,
			fsys:            &failingDurableFS{failWrite: true},
		}
		if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); err == nil || fsutil.AtomicWriteOutcome(err) != fsutil.CommitNotCommitted {
			t.Fatalf("write failure outcome = %v (%v)", fsutil.AtomicWriteOutcome(err), err)
		}
		stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
		if stored.EverAdmitted || store.Generation() != beforeGeneration || store.poisoned != nil || !bytes.Equal(store.journalBytes, before) {
			t.Fatalf("not-committed admission published evidence: generation %d poison %v record %#v", store.Generation(), store.poisoned, stored)
		}
	})

	t.Run("indeterminate", func(t *testing.T) {
		store, fence, record, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
		before := append([]byte(nil), store.journalBytes...)
		beforeGeneration := store.Generation()
		store.directory = &failingSecureDirectory{
			SecureDirectory: store.directory,
			fsys:            &failingDurableFS{failSyncDir: true},
		}
		if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); err == nil || fsutil.AtomicWriteOutcome(err) != fsutil.CommitIndeterminate {
			t.Fatalf("directory sync failure outcome = %v (%v)", fsutil.AtomicWriteOutcome(err), err)
		}
		stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
		if stored.EverAdmitted || store.Generation() != beforeGeneration || store.poisoned == nil || !bytes.Equal(store.journalBytes, before) {
			t.Fatalf("indeterminate admission published in-memory evidence: generation %d poison %v record %#v", store.Generation(), store.poisoned, stored)
		}
		if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); !errors.Is(err, ErrCodexLeaseStorePoisoned) {
			t.Fatalf("poisoned retry error = %T %v", err, err)
		}
	})
}

func TestCodexLeaseV2OwnerCloseWaitsForAdmissionCommit(t *testing.T) {
	credentialFS := fsutil.NewMemFS()
	managed, err := codex.NewManagedStore(credentialFS)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "cq-state")
	credentialCoordinator, err := codex.NewCredentialCoordinator(managed, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	controlDirectory, err := os.MkdirTemp(tempRoot, "cqlease-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlDirectory) })
	control, err := codex.OpenCredentialControlPrepared(context.Background(), filepath.Join(controlDirectory, "credential.sock"), credentialCoordinator, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = control.Close()
		}
	})

	store, fence, record, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
	store.mu.Lock()
	store.owner = control
	store.mu.Unlock()
	entered := make(chan struct{})
	release := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
	})
	store.directory = &blockingCodexLeaseAffinityDirectory{
		SecureDirectory: store.directory,
		entered:         entered,
		release:         release,
	}
	commitResult := make(chan error, 1)
	go func() {
		_, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
		commitResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission commit did not reach blocked durable write")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- control.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for control.AssertOwner() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(control.AssertOwner(), codex.ErrCredentialOwnerRevoked) {
		t.Fatal("owner close did not enter closing state")
	}
	select {
	case err := <-closeResult:
		t.Fatalf("owner close finished before guarded commit: %v", err)
	default:
	}
	select {
	case err := <-commitResult:
		t.Fatalf("guarded commit finished before write release: %v", err)
	default:
	}
	release <- struct{}{}
	if err := <-commitResult; err != nil {
		t.Fatal(err)
	}
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
	closed = true
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if !stored.EverAdmitted || stored.AdmissionJournalGeneration != store.Generation() {
		t.Fatalf("guarded admission evidence = %#v generation %d", stored, store.Generation())
	}
}

func TestCodexLeaseV2LoadLaneReturnsResolvedOrOpaqueUnresolvedAffinity(t *testing.T) {
	store, _, admitted, _ := openAdmittedCodexLeaseV2AffinityTestStore(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}
	successor := LeaseKey{
		Lane: LaneKey{Session: "affinity-helper-session", Thread: "affinity-helper-thread", Namespace: CodexResponsesNamespace},
		Turn: "successor-turn",
	}

	resolved, err := store.LoadLane(successor, []codex.AccountKey{"account"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := admitted.Identity()
	if resolved.Affinity == nil || !resolved.Affinity.Resolved || resolved.Affinity.AccountKey != "account" || resolved.Affinity.Source != wantSource || resolved.Affinity.AdmissionJournalGeneration != admitted.AdmissionJournalGeneration || resolved.Affinity.AdmittedAt != admitted.AdmittedAt {
		t.Fatalf("resolved affinity = %#v", resolved.Affinity)
	}
	resolved.Affinity.AccountKey = "mutated"
	resolved.Affinity.Source.TurnDigest = "mutated"
	reloaded, err := store.LoadLane(successor, []codex.AccountKey{"account"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Affinity == nil || reloaded.Affinity.AccountKey != "account" || reloaded.Affinity.Source != wantSource {
		t.Fatalf("affinity result aliases store state: %#v", reloaded.Affinity)
	}

	unresolved, err := store.LoadLane(successor, nil, policy)
	if err != nil {
		t.Fatalf("unavailable soft affinity blocked successor load: %v", err)
	}
	if unresolved.Affinity == nil || unresolved.Affinity.Resolved || unresolved.Affinity.AccountKey != "" || unresolved.Affinity.Source != wantSource {
		t.Fatalf("unresolved affinity = %#v", unresolved.Affinity)
	}
	display := fmt.Sprintf("%#v", unresolved.Affinity)
	if strings.Contains(display, admitted.AccountHash) || strings.Contains(display, "account") {
		t.Fatalf("unresolved affinity exposed persisted or raw account identity: %s", display)
	}

	exact := successor
	exact.Turn = "affinity-helper-turn"
	if _, err := store.LoadLane(exact, nil, policy); !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("unresolved exact continuation error = %T %v", err, err)
	}
}

func TestCodexLeaseV2RestartNormalisationPreservesAdmissionEvidenceAndAffinity(t *testing.T) {
	store, _, admitted, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	fsys, ok := store.fs.(*fsutil.MemFS)
	if !ok {
		t.Fatalf("lease test filesystem = %T, want *fsutil.MemFS", store.fs)
	}
	beforeGeneration := store.Generation()
	beforeEvidenceGeneration := admitted.AdmissionJournalGeneration
	beforeAdmittedAt := admitted.AdmittedAt
	beforeIdentity := admitted.Identity()
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)

	reopened, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
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
	t.Cleanup(func() { _ = reopened.Close() })

	restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, beforeIdentity)
	if reopened.Store().Generation() != beforeGeneration+1 || restored.State != LeaseOrphaned || !restored.SocketLineageExtinct || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.Attempts[0].State != CodexAttemptIndeterminate {
		t.Fatalf("restart normalisation = generation %d record %#v", reopened.Store().Generation(), restored)
	}
	if !restored.EverAdmitted || restored.AdmissionJournalGeneration != beforeEvidenceGeneration || restored.AdmittedAt != beforeAdmittedAt {
		t.Fatalf("restart changed first-admission evidence: before %d/%v after %#v", beforeEvidenceGeneration, beforeAdmittedAt, restored)
	}
	lane := findCodexLeaseV2CASTestLane(t, reopened.Store().v2.Lanes, beforeIdentity.LaneDigest)
	if lane.LastAdmittedAccountHash != restored.AccountHash || lane.LastAdmittedTurnHash != restored.TurnHash || lane.LastAdmissionJournalGeneration != beforeEvidenceGeneration || lane.LastAdmittedAt != beforeAdmittedAt {
		t.Fatalf("restart changed lane affinity: %#v", lane)
	}
	successor := LeaseKey{
		Lane: LaneKey{Session: "affinity-helper-session", Thread: "affinity-helper-thread", Namespace: CodexResponsesNamespace},
		Turn: "restart-successor",
	}
	loaded, err := reopened.Store().LoadLane(successor, []codex.AccountKey{"account"}, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true})
	if err != nil || loaded.Affinity == nil || !loaded.Affinity.Resolved || loaded.Affinity.AccountKey != "account" || loaded.Affinity.Source != beforeIdentity {
		t.Fatalf("restart affinity hint = %#v error %v", loaded.Affinity, err)
	}
}

func TestCodexLeaseV2RetentionKeepsOpaqueAffinityUntilFinalLaneExpires(t *testing.T) {
	store, fence, admitted, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	completed := codexLeaseV2CASTestMutationRecord(admitted)
	completed.State = LeaseBoundQuiescent
	completed.RoutingRefs = 0
	completed.AttemptRefs = 0
	completed.SocketLineageExtinct = true
	completed.Attempts[0].State = CodexAttemptProviderCompleted
	completedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, admitted.Identity())
	completedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: admitted.Generation, Generation: admitted.Attempts[0].Generation, Revision: admitted.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{completedFence}
	var err error
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{completed}})
	if err != nil {
		t.Fatal(err)
	}
	admitted = findCodexLeaseV2CASTestRecord(t, store.v2.Records, admitted.Identity())

	*now = now.Add(2 * time.Hour)
	successor := reservingCodexLeaseV2CASTestRecord(store, "affinity-helper-session", "affinity-helper-thread", "retention-anchor")
	successor.ModeEpoch = 9
	successor.Authoritative = true
	successor.SocketLineageExtinct = true
	successor.PredecessorTurnHash = admitted.TurnHash
	successor.PredecessorModeEpoch = admitted.ModeEpoch
	successor.PredecessorAuthoritative = admitted.Authoritative
	lane := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, admitted.Identity().LaneDigest))
	lane.CurrentTurnHash = successor.TurnHash
	lane.CurrentModeEpoch = successor.ModeEpoch
	lane.CurrentAuthoritative = successor.Authoritative
	lane.LastTurnHash = successor.TurnHash
	lane.LastModeEpoch = successor.ModeEpoch
	lane.LastAuthoritative = successor.Authoritative
	superseded := codexLeaseV2CASTestMutationRecord(admitted)
	superseded.State = LeaseSuperseded
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, admitted.Identity()),
		{Record: successor.Identity()},
	}
	fence, err = store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: []CodexJournalRecordV2{superseded, successor}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Minute)
	storedSuccessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, successor.Identity())
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, storedSuccessor.Identity()),
		codexLeaseV2CASTestRecordFence(store.v2.Records, admitted.Identity()),
	}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{codexLeaseV2CASTestMutationRecord(storedSuccessor)}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30*time.Minute + time.Nanosecond)

	store.policy.Retention = time.Hour
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	if len(store.v2.Records) != 1 || store.v2.Records[0].Identity() != successor.Identity() {
		t.Fatalf("source-pruned records = %#v", store.v2.Records)
	}
	retainedLane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, successor.Identity().LaneDigest)
	if codexLaneAffinityIsZero(retainedLane) || retainedLane.LastAdmittedTurnHash != admitted.TurnHash || retainedLane.LastAdmissionJournalGeneration != admitted.AdmissionJournalGeneration {
		t.Fatalf("source pruning lost signed affinity: %#v", retainedLane)
	}
	key := LeaseKey{Lane: LaneKey{Session: "affinity-helper-session", Thread: "affinity-helper-thread", Namespace: CodexResponsesNamespace}, Turn: "after-prune"}
	restored, err := store.LoadLane(key, nil, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true})
	if err != nil || restored.Affinity == nil || restored.Affinity.Resolved || restored.Affinity.AccountKey != "" || restored.Affinity.Source != admitted.Identity() {
		t.Fatalf("pruned-source unresolved affinity = %#v error %v", restored.Affinity, err)
	}
	resolved, err := store.LoadLane(key, []codex.AccountKey{"account"}, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true})
	if err != nil || resolved.Affinity == nil || !resolved.Affinity.Resolved || resolved.Affinity.AccountKey != "account" {
		t.Fatalf("pruned-source resolved affinity = %#v error %v", resolved.Affinity, err)
	}
	leases := NewCodexTurnLeaseManager(9, true, store.policy.Now)
	coordinator := &CodexContinuityCoordinator{store: store, leases: leases}
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	requiresAccount, err := runtimeLease.validateRequestContinuity(
		resolved,
		codexLeaseRuntimeRequestIdentity(resolved),
		"account",
		CodexLeaseRequestEvidence{HasEncryptedState: true},
		false,
	)
	if err != nil || !requiresAccount {
		t.Fatalf("pruned-source encrypted affinity = required %v error %v", requiresAccount, err)
	}
	currentKey := key
	currentKey.Turn = "retention-anchor"
	current, err := store.LoadLane(currentKey, []codex.AccountKey{"account"}, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true})
	if err != nil || current.Classification != CodexRestoredLaneCurrent || current.Affinity == nil || !current.Affinity.Resolved {
		t.Fatalf("pruned-source current affinity = %#v error %v", current, err)
	}
	requiresAccount, err = runtimeLease.validateRequestContinuity(
		current,
		codexLeaseRuntimeRequestIdentity(current),
		"account",
		CodexLeaseRequestEvidence{HasEncryptedState: true},
		false,
	)
	if err != nil || !requiresAccount {
		t.Fatalf("pruned-source current encrypted affinity = required %v error %v", requiresAccount, err)
	}
	if _, err := runtimeLease.validateRequestContinuity(resolved, codexLeaseRuntimeRequestIdentity(resolved), "other", CodexLeaseRequestEvidence{HasEncryptedState: true}, false); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("pruned-source account mismatch = %v, want continuity error", err)
	}
	if _, err := runtimeLease.validateRequestContinuity(restored, codexLeaseRuntimeRequestIdentity(restored), "account", CodexLeaseRequestEvidence{HasEncryptedState: true}, false); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("unresolved pruned-source affinity = %v, want continuity error", err)
	}
	removal, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}
	removal.Release()
	if summary.BoundCount != 0 || summary.AdoptedPrewarm != 0 {
		t.Fatalf("soft affinity counted as durable bound authority: %#v", summary)
	}

	*now = now.Add(store.policy.Retention + time.Nanosecond)
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(store.policy.Retention + time.Nanosecond)
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	if len(store.v2.Lanes) != 0 || len(store.v2.Records) != 0 {
		t.Fatalf("final retention left affinity without lane: lanes %#v records %#v", store.v2.Lanes, store.v2.Records)
	}
	restored, err = store.LoadLane(key, nil, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true})
	if err != nil || restored.Affinity != nil {
		t.Fatalf("expired lane returned affinity %#v error %v", restored.Affinity, err)
	}
}

func TestCodexLeaseV2AdmissionEvidenceIsStoreOwnedAndImmutable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2)
	}{
		{name: "caller supplied evidence", mutate: func(record *CodexJournalRecordV2) {
			record.EverAdmitted = true
			record.AdmissionJournalGeneration = 99
			record.AdmittedAt = record.Attempts[0].LastObservedAt
		}},
		{name: "account after admission", mutate: func(record *CodexJournalRecordV2) {
			record.AccountHash = "different-but-invalid-before-write"
		}},
		{name: "request kind after admission", mutate: func(record *CodexJournalRecordV2) {
			record.RequestKind = CodexRequestCompaction
			record.CompactionPhase = CodexCompactionStandalone
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, stored, _ := openAdmittedCodexLeaseV2AffinityTestStore(t)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			mutation := codexLeaseV2CASTestMutationRecord(stored)
			test.mutate(&mutation)
			fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())}
			if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("mutate admitted evidence error = %T %v", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) {
				t.Fatal("invalid admitted mutation changed durable authority")
			}
		})
	}

	t.Run("caller supplied lane affinity", func(t *testing.T) {
		store, fence, stored, _ := openAdmittedCodexLeaseV2AffinityTestStore(t)
		before := append([]byte(nil), store.journalBytes...)
		beforeGeneration := store.Generation()
		lane := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, stored.Identity().LaneDigest))
		lane.LastAdmittedAccountHash = stored.AccountHash
		lane.LastAdmittedTurnHash = stored.TurnHash
		lane.LastAdmittedModeEpoch = stored.ModeEpoch
		lane.LastAdmittedAuthoritative = true
		lane.LastAdmissionJournalGeneration = stored.AdmissionJournalGeneration
		lane.LastAdmittedAt = stored.AdmittedAt
		fence.TouchedRecords = nil
		if _, err := store.CommitLane(fence, CodexLaneMutation{Lane: &lane}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
			t.Fatalf("caller lane affinity error = %T %v", err, err)
		}
		if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) {
			t.Fatal("caller lane affinity changed durable authority")
		}
	})
}

func TestCodexLeaseV2SchemaRejectsInvalidAdmissionEvidence(t *testing.T) {
	store, base := codexLeaseV2AdmittedSchemaFixture(t)
	cases := []struct {
		name   string
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "record flag missing", mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].EverAdmitted = false }},
		{name: "record generation missing", mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AdmissionJournalGeneration = 0 }},
		{name: "record generation before cutover", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AdmissionJournalGeneration = value.Cutover.CompletionGeneration
		}},
		{name: "record generation after journal", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AdmissionJournalGeneration = value.Generation + 1
		}},
		{name: "record time before creation", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AdmittedAt = value.Records[0].CreatedAt.Add(-1)
		}},
		{name: "record time after observation", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AdmittedAt = value.Records[0].LastObservedAt.Add(1)
		}},
		{name: "provisional evidence", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].State = LeaseProvisional
			value.Records[0].Attempts[0].State = CodexAttemptPrepared
		}},
		{name: "raw prewarm evidence", mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AdmissionRequestKind = CodexRequestPrewarm }},
		{name: "lane affinity missing", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			clearCodexLeaseLaneAffinity(&value.Lanes[0])
		}},
		{name: "lane account missing", mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastAdmittedAccountHash = "" }},
		{name: "lane source changed", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastAdmittedTurnHash = store.hash("turn", "absent-source")
		}},
		{name: "lane generation mismatch", mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastAdmissionJournalGeneration++ }},
		{name: "lane time mismatch", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastAdmittedAt = value.Lanes[0].LastAdmittedAt.Add(1)
		}},
		{name: "cache time before first admission", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastCacheAdmittedAt = value.Lanes[0].LastAdmittedAt.Add(-1)
		}},
		{name: "cache time after observation", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastCacheAdmittedAt = value.Lanes[0].LastObservedAt.Add(1)
		}},
		{name: "cache model without time", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastCacheAdmittedAt = time.Time{}
		}},
		{name: "cache time without model", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastCacheEffectiveModel = ""
		}},
		{name: "newer admitted history than lane affinity", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation++
			newer := value.Records[0]
			newer.TurnHash = store.hash("turn", "newer-admitted-history")
			newer.RecordGeneration++
			newer.LeaseGeneration++
			newer.State = LeaseBoundQuiescent
			newer.RoutingRefs = 0
			newer.AttemptRefs = 0
			newer.SocketLineageExtinct = true
			newer.AdmissionJournalGeneration++
			newer.Attempts[0].State = CodexAttemptProviderCompleted
			newer.Attempts[0].Revision++
			value.Records = append(value.Records, newer)
			sort.Slice(value.Records, func(left, right int) bool {
				return codexJournalRecordLess(value.Records[left], value.Records[right])
			})
		}},
	}
	tests := make([]struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}, len(cases))
	for index, test := range cases {
		tests[index].name = test.name
		tests[index].base = base
		tests[index].mutate = test.mutate
	}
	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaBindsFirstAdmissionEvidenceToItsRequest(t *testing.T) {
	store, base := codexLeaseV2AdmittedSchemaFixture(t)

	t.Run("same generation kind and phase mismatch", func(t *testing.T) {
		value := codexLeaseV2CloneSchemaFixture(t, base)
		value.Records[0].AdmissionRequestKind = CodexRequestCompaction
		value.Records[0].AdmissionCompactionPhase = CodexCompactionStandalone
		codexLeaseV2SignSchemaFixture(t, store, &value)

		reopened := &CodexLeaseStore{key: append([]byte(nil), store.key...)}
		if err := reopened.loadV2Locked(codexLeaseV2SchemaJSON(t, value)); !errors.Is(err, ErrCodexLeaseTrustLost) {
			t.Fatalf("reopen mismatched first-admission evidence error = %T %v, want trust lost", err, err)
		}
	})

	t.Run("later request preserves original admission evidence", func(t *testing.T) {
		value := codexLeaseV2CloneSchemaFixture(t, base)
		record := &value.Records[0]
		record.Generation++
		record.RequestKind = CodexRequestCompaction
		record.CompactionPhase = CodexCompactionMidTurn
		codexLeaseV2SignSchemaFixture(t, store, &value)
		if err := store.validateV2Envelope(value); err != nil {
			t.Fatalf("validate later request with retained first-admission evidence: %v", err)
		}
	})
}

func TestCodexLeaseV2SchemaRestrictsLaterAdmittedRequestPlanToBoundAccount(t *testing.T) {
	store, base := codexLeaseV2AdmittedSchemaFixture(t)
	foreignAccountHash := store.hash("account", "foreign-later-request-account")

	t.Run("first admission request retains authorised alternate slots", func(t *testing.T) {
		value := codexLeaseV2CloneSchemaFixture(t, base)
		value.Records[0].AttemptEnvelope.Slots[1].AccountHash = foreignAccountHash
		codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		codexLeaseV2SignSchemaFixture(t, store, &value)
		if err := store.validateV2Envelope(value); err != nil {
			t.Fatalf("validate first-admission request with frozen alternate slot: %v", err)
		}
	})

	t.Run("later request rejects foreign account slot", func(t *testing.T) {
		value := codexLeaseV2CloneSchemaFixture(t, base)
		record := &value.Records[0]
		record.Generation++
		record.AttemptEnvelope.Slots[1].AccountHash = foreignAccountHash
		codexLeaseV2RefreshPlanDigest(t, store, record)
		codexLeaseV2SignSchemaFixture(t, store, &value)

		reopened := &CodexLeaseStore{key: append([]byte(nil), store.key...)}
		if err := reopened.loadV2Locked(codexLeaseV2SchemaJSON(t, value)); !errors.Is(err, ErrCodexLeaseTrustLost) {
			t.Fatalf("reopen later admitted request with foreign slot error = %T %v, want trust lost", err, err)
		}
	})
}

func openAdmittedCodexLeaseV2AffinityTestStore(t *testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2, *time.Time) {
	t.Helper()
	return openCodexLeaseV2AffinityVariantTestStore(t, CodexRequestTurn, "", true)
}

func openCodexLeaseV2AffinityVariantTestStore(t *testing.T, kind CodexRequestKind, phase CodexCompactionPhase, authoritative bool) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2, *time.Time) {
	t.Helper()
	store, _, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "affinity-helper-session", "affinity-helper-thread", "affinity-helper-turn")
	record.ModeEpoch = 9
	record.RequestKind = kind
	record.CompactionPhase = phase
	record.Authoritative = authoritative
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, stored := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	*now = now.Add(1)
	dispatched := codexLeaseV2CASTestMutationRecord(stored)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: stored.Generation, Generation: stored.Attempts[0].Generation, Revision: stored.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = now.Add(1)
	streaming := codexLeaseV2CASTestMutationRecord(stored)
	streaming.State = LeaseBoundActive
	streaming.RoutingRefs = 1
	streaming.AttemptRefs = 1
	streaming.Attempts[0].State = CodexAttemptStreaming
	streamingFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	streamingFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: stored.Generation, Generation: stored.Attempts[0].Generation, Revision: stored.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{streamingFence}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}}
	store.mu.Unlock()
	return store, fence, findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity()), now
}

func openDispatchedCodexLeaseV2AffinityTestStore(t *testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2, CodexJournalRecordV2) {
	t.Helper()
	store, _, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "affinity-failure-session", "affinity-failure-thread", "affinity-failure-turn")
	record.ModeEpoch = 9
	record.Authoritative = true
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	*now = now.Add(1)
	dispatched := codexLeaseV2CASTestMutationRecord(record)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	dispatchedFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	dispatchedFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{dispatchedFence}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = now.Add(1)
	streaming := codexLeaseV2CASTestMutationRecord(record)
	streaming.State = LeaseBoundActive
	streaming.RoutingRefs = 1
	streaming.AttemptRefs = 1
	streaming.Attempts[0].State = CodexAttemptStreaming
	streamingFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	streamingFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{streamingFence}
	return store, fence, record, streaming
}

func codexLeaseV2AdmittedSchemaFixture(t *testing.T) (*CodexLeaseStore, codexLeaseJournalEnvelopeV2) {
	t.Helper()
	store, envelope := codexLeaseV2SchemaFixture(t)
	envelope.Generation = 5
	envelope.Records[0].State = LeaseBoundActive
	envelope.Records[0].RecordGeneration = 4
	envelope.Records[0].LeaseGeneration = 3
	envelope.Records[0].Attempts[0].State = CodexAttemptStreaming
	envelope.Records[0].Attempts[0].Revision = 3
	codexLeaseV2SetSchemaAdmissionEvidence(&envelope, 4)
	return store, envelope
}

func clearCodexLeaseLaneAffinity(lane *CodexJournalLane) {
	lane.LastAdmittedAccountHash = ""
	lane.LastAdmittedTurnHash = ""
	lane.LastAdmittedModeEpoch = 0
	lane.LastAdmittedAuthoritative = false
	lane.LastAdmissionJournalGeneration = 0
	lane.LastAdmittedAt = time.Time{}
	lane.LastCacheAdmittedAt = time.Time{}
	lane.LastCacheEffectiveModel = ""
}

func appendCodexLeaseV2AffinitySuccessor(t *testing.T, store *CodexLeaseStore, fence CodexLeaseGenerationFence, predecessor CodexJournalRecordV2, turn string) (CodexLeaseGenerationFence, CodexJournalRecordV2) {
	t.Helper()
	desired := provisionalCodexLeaseV2CASTestRecord(store, "affinity-helper-session", "affinity-helper-thread", turn)
	desired.ModeEpoch = 9
	desired.Authoritative = true
	desired.PredecessorTurnHash = predecessor.TurnHash
	desired.PredecessorModeEpoch = predecessor.ModeEpoch
	desired.PredecessorAuthoritative = predecessor.Authoritative
	desired.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	successor := cloneCodexJournalRecordV2(desired)
	successor.State = LeaseReserving
	successor.AccountHash = ""
	successor.CodexCurrentRequest = CodexCurrentRequest{}
	upserts := []CodexJournalRecordV2{successor}
	if predecessor.State != LeaseFailedUnadmitted {
		superseded := codexLeaseV2CASTestMutationRecord(predecessor)
		superseded.State = LeaseSuperseded
		upserts = append([]CodexJournalRecordV2{superseded}, upserts...)
	}
	lane := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, predecessor.Identity().LaneDigest))
	lane.CurrentTurnHash = successor.TurnHash
	lane.CurrentModeEpoch = successor.ModeEpoch
	lane.CurrentAuthoritative = successor.Authoritative
	lane.LastTurnHash = successor.TurnHash
	lane.LastModeEpoch = successor.ModeEpoch
	lane.LastAuthoritative = successor.Authoritative
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, predecessor.Identity()),
		{Record: successor.Identity()},
	}
	if predecessor.PredecessorTurnHash != "" {
		fence.TouchedRecords = append(fence.TouchedRecords, codexLeaseV2CASTestRecordFence(store.v2.Records, CodexJournalRecordIdentity{
			LaneDigest:    predecessor.Identity().LaneDigest,
			TurnDigest:    predecessor.PredecessorTurnHash,
			ModeEpoch:     predecessor.PredecessorModeEpoch,
			Authoritative: predecessor.PredecessorAuthoritative,
		}))
	}
	post, err := store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: upserts})
	if err != nil {
		t.Fatal(err)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, successor.Identity())
	mutation := codexLeaseV2CASTestMutationRecord(desired)
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	post.TouchedRecords = []CodexLeaseRecordFence{
		recordFence,
		codexLeaseV2CASTestRecordFence(store.v2.Records, predecessor.Identity()),
	}
	identity := stored.Identity()
	post, err = store.CommitLane(post, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}})
	if err != nil {
		t.Fatal(err)
	}
	return post, findCodexLeaseV2CASTestRecord(t, store.v2.Records, successor.Identity())
}

func commitCodexLeaseV2AffinityState(t *testing.T, store *CodexLeaseStore, fence CodexLeaseGenerationFence, record CodexJournalRecordV2, state LeaseState, attemptState CodexAttemptState, live bool) (CodexLeaseGenerationFence, CodexJournalRecordV2) {
	t.Helper()
	mutation := codexLeaseV2CASTestMutationRecord(record)
	mutation.State = state
	mutation.RoutingRefs = 0
	mutation.AttemptRefs = 0
	mutation.SocketLineageExtinct = record.SocketLineageExtinct
	if state == LeaseBoundQuiescent || state == LeaseFailedUnadmitted {
		mutation.SocketLineageExtinct = true
	}
	if live {
		mutation.SocketLineageExtinct = false
		mutation.RoutingRefs = 1
		mutation.AttemptRefs = 1
	}
	mutation.Attempts[0].State = attemptState
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	if record.PredecessorTurnHash != "" {
		predecessor := CodexJournalRecordIdentity{
			LaneDigest:    record.Identity().LaneDigest,
			TurnDigest:    record.PredecessorTurnHash,
			ModeEpoch:     record.PredecessorModeEpoch,
			Authoritative: record.PredecessorAuthoritative,
		}
		fence.TouchedRecords = append(fence.TouchedRecords, codexLeaseV2CASTestRecordFence(store.v2.Records, predecessor))
	}
	post, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}})
	if err != nil {
		t.Fatal(err)
	}
	return post, findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
}

type blockingCodexLeaseAffinityDirectory struct {
	fsutil.SecureDirectory
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (directory *blockingCodexLeaseAffinityDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &blockingCodexLeaseAffinityFile{DurableFile: file, directory: directory}, nil
}

type blockingCodexLeaseAffinityFile struct {
	fsutil.DurableFile
	directory *blockingCodexLeaseAffinityDirectory
}

func (file *blockingCodexLeaseAffinityFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *blockingCodexLeaseAffinityFile) Write(data []byte) (int, error) {
	file.directory.once.Do(func() { close(file.directory.entered) })
	<-file.directory.release
	return file.DurableFile.Write(data)
}
