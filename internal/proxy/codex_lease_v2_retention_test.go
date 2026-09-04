package proxy

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2CompactUsesConfiguredExactRetentionAndPreservesSignedHorizon(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	lastObserved := time.Date(2026, 8, 9, 7, 2, 0, 0, time.UTC)
	now := lastObserved.Add(time.Hour)
	store.policy.Retention = time.Hour
	store.policy.Now = func() time.Time { return now }
	pinnedHorizon := store.v2.Cutover.LegacyQuarantineUntil

	beforeBoundary := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if err := store.Compact(now, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBoundary, store.journalBytes) || store.Generation() != beforeGeneration {
		t.Fatal("exact retention boundary compacted records or trusted caller retention")
	}

	now = now.Add(time.Nanosecond)
	beforeExpiry := findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(store.hash("session", "session"), store.hash("thread", "thread"), store.hash("namespace", CodexResponsesNamespace)),
		TurnDigest:    store.hash("turn", "current"),
		ModeEpoch:     9,
		Authoritative: true,
	})
	beforeLane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, beforeExpiry.Identity().LaneDigest)
	if err := store.Compact(time.Time{}, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if store.v2.Cutover.LegacyQuarantineUntil != pinnedHorizon {
		t.Fatal("configured retention compaction changed signed cutover horizon")
	}
	if store.Generation() != beforeGeneration+1 || len(store.v2.Lanes) != 1 || len(store.v2.Records) != 1 {
		t.Fatalf("first compaction = generation %d lanes %d records %d, want %d/1/1", store.Generation(), len(store.v2.Lanes), len(store.v2.Records), beforeGeneration+1)
	}
	remaining := store.v2.Records[0]
	if remaining.State != LeaseExpired || remaining.TurnHash != store.hash("turn", "current") || store.v2.Lanes[0].CurrentTurnHash != "" || store.v2.Lanes[0].LastTurnHash != remaining.TurnHash {
		t.Fatalf("first compaction remaining record/lane = %#v %#v", remaining, store.v2.Lanes[0])
	}
	if remaining.RecordGeneration != beforeExpiry.RecordGeneration+1 || remaining.LeaseGeneration != beforeExpiry.LeaseGeneration+1 || remaining.LaneGeneration != beforeLane.Generation+1 || store.v2.Lanes[0].Generation != beforeLane.Generation+1 {
		t.Fatalf("expiry generations = record %d lease %d record-lane %d lane %d; before %#v/%#v", remaining.RecordGeneration, remaining.LeaseGeneration, remaining.LaneGeneration, store.v2.Lanes[0].Generation, beforeExpiry, beforeLane)
	}
	wantAttempt := beforeExpiry.Attempts[0]
	switch wantAttempt.State {
	case CodexAttemptPrepared, CodexAttemptDispatched, CodexAttemptStreaming:
		wantAttempt.State = CodexAttemptIndeterminate
		wantAttempt.Revision++
		wantAttempt.LastObservedAt = now
	}
	if remaining.CreatedAt != beforeExpiry.CreatedAt || remaining.LastObservedAt != now || store.v2.Lanes[0].LastObservedAt != now || len(remaining.Attempts) != 1 || !reflect.DeepEqual(remaining.Attempts[0], wantAttempt) {
		t.Fatalf("expiry timestamps/attempt = now %v record %#v lane %#v", now, remaining, store.v2.Lanes[0])
	}
	restored, err := store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"},
		[]codex.AccountKey{"account-one"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}},
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := CodexJournalRecordIdentity{
		LaneDigest:    remaining.Identity().LaneDigest,
		TurnDigest:    remaining.PredecessorTurnHash,
		ModeEpoch:     remaining.PredecessorModeEpoch,
		Authoritative: remaining.PredecessorAuthoritative,
	}
	fence, err := restored.MutationFence(remaining.Identity(), predecessor)
	if err != nil {
		t.Fatal(err)
	}
	mutation := codexLeaseV2CASTestMutationRecord(remaining)
	mutation.NonMigratable = true
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour).Add(time.Nanosecond)
	if err := store.Compact(now, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if store.Generation() != beforeGeneration+3 || len(store.v2.Lanes) != 0 || len(store.v2.Records) != 0 {
		t.Fatalf("second compaction = generation %d lanes %d records %d, want %d/0/0", store.Generation(), len(store.v2.Lanes), len(store.v2.Records), beforeGeneration+3)
	}
}

func TestCodexLeaseV2CompactHonoursConfiguredBoundaries(t *testing.T) {
	for _, retention := range []time.Duration{24 * time.Hour, 48 * time.Hour, 30 * 24 * time.Hour} {
		retention := retention
		t.Run(retention.String(), func(t *testing.T) {
			coordinator := openCodexLeaseV2LaneTestCoordinator(t)
			store := coordinator.Store()
			now := time.Date(2026, 8, 9, 7, 2, 0, 0, time.UTC).Add(retention)
			store.policy.Retention = retention
			store.policy.Now = func() time.Time { return now }
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			if err := store.Compact(time.Time{}, time.Nanosecond); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, store.journalBytes) || store.Generation() != beforeGeneration {
				t.Fatal("exact configured retention boundary changed durable authority")
			}
			now = now.Add(time.Nanosecond)
			if err := store.Compact(time.Now(), 365*24*time.Hour); err != nil {
				t.Fatal(err)
			}
			if store.Generation() != beforeGeneration+1 || len(store.v2.Records) != 1 || store.v2.Records[0].State != LeaseExpired {
				t.Fatalf("post-boundary compaction = generation %d records %#v", store.Generation(), store.v2.Records)
			}
		})
	}
}

func TestCodexLeaseV2CommitPiggybacksPeriodicRetentionSweep(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	failed := reservingCodexLeaseV2CASTestRecord(store, "expired-session", "expired-thread", "expired-turn")
	fence, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal: store.Generation(), TouchedRecords: []CodexLeaseRecordFence{{Record: failed.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(failed), UpsertRecords: []CodexJournalRecordV2{failed}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	failedMutation := codexLeaseV2CASTestMutationRecord(stored)
	failedMutation.State = LeaseFailedUnadmitted
	failedMutation.SocketLineageExtinct = true
	lane := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, failed.Identity().LaneDigest))
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity())}
	if _, err := store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: []CodexJournalRecordV2{failedMutation}}); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(store.policy.Retention + time.Second)
	fresh := reservingCodexLeaseV2CASTestRecord(store, "fresh-session", "fresh-thread", "fresh-turn")
	if _, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal: store.Generation(), TouchedRecords: []CodexLeaseRecordFence{{Record: fresh.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(fresh), UpsertRecords: []CodexJournalRecordV2{fresh}}); err != nil {
		t.Fatal(err)
	}
	if len(store.v2.Records) != 1 || store.v2.Records[0].Identity() != fresh.Identity() || len(store.v2.Lanes) != 1 {
		t.Fatalf("piggyback retention left records=%#v lanes=%#v", store.v2.Records, store.v2.Lanes)
	}
}

func TestCodexLeaseV2CompactPreservesCompletedLegacyCutoverEvidence(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	retention := time.Hour
	_, legacy := writeCodexLeaseV1Fixture(t, fsys)
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy:      CodexLeasePolicy{Retention: retention, Now: func() time.Time { return now }},
		Modes:       CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, &cutoverTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	store := coordinator.Store()
	now = now.Add(retention)
	if _, err := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); err != nil {
		t.Fatal(err)
	}
	pinned := store.v2.Cutover
	archiveBefore := append([]byte(nil), legacy...)

	record := reservingCodexLeaseV2CASTestRecord(store, "session", "thread", "turn")
	record.ModeEpoch = 6
	record.SocketLineageExtinct = true
	if _, err := store.CommitLane(
		CodexLeaseGenerationFence{Journal: 9, TouchedRecords: []CodexLeaseRecordFence{{Record: record.Identity()}}},
		CodexLaneMutation{Lane: codexLeaseV2CASTestLane(record), UpsertRecords: []CodexJournalRecordV2{record}},
	); err != nil {
		t.Fatal(err)
	}
	now = now.Add(retention).Add(time.Nanosecond)
	if err := store.Compact(time.Time{}, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	archiveAfter, err := fsys.ReadFile("/state/" + store.legacyArchive)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.v2.Cutover, pinned) || !bytes.Equal(archiveAfter, archiveBefore) || store.Generation() != 11 {
		t.Fatalf("compaction changed completed legacy evidence: generation=%d cutover=%#v", store.Generation(), store.v2.Cutover)
	}
}

func TestCodexLeaseV2CompactRequiresHealthyGuardedOwner(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	store.policy.Retention = time.Nanosecond
	store.policy.Now = func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	originalOwner := store.owner
	ownerErr := errors.New("owner operation revoked")
	store.mu.Lock()
	store.owner = &cutoverTestOwner{beginErr: ownerErr}
	store.mu.Unlock()
	if err := store.Compact(time.Time{}, 0); !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, ownerErr) {
		t.Fatalf("revoked-owner compaction error = %T %v", err, err)
	}
	store.mu.Lock()
	store.owner = originalOwner
	store.mu.Unlock()
	if !bytes.Equal(before, store.journalBytes) || store.Generation() != beforeGeneration {
		t.Fatal("revoked-owner compaction changed durable authority")
	}

	store.mu.Lock()
	store.poisoned = errors.New("indeterminate prior commit")
	store.mu.Unlock()
	if err := store.Compact(time.Time{}, 0); !errors.Is(err, ErrCodexLeaseStorePoisoned) {
		t.Fatalf("poisoned compaction error = %T %v", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.Generation() != beforeGeneration {
		t.Fatal("poisoned compaction changed durable authority")
	}
}

func TestCodexLeaseV2CompactCommitOutcomesPreserveInMemoryAuthority(t *testing.T) {
	tests := []struct {
		name         string
		fsys         *failingDurableFS
		wantOutcome  fsutil.CommitOutcome
		wantPoisoned bool
	}{
		{name: "not committed", fsys: &failingDurableFS{failWrite: true}, wantOutcome: fsutil.CommitNotCommitted},
		{name: "indeterminate", fsys: &failingDurableFS{failSyncDir: true}, wantOutcome: fsutil.CommitIndeterminate, wantPoisoned: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := openCodexLeaseV2LaneTestCoordinator(t)
			store := coordinator.Store()
			store.policy.Retention = time.Nanosecond
			store.policy.Now = func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }
			beforeBytes := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			beforeEnvelope := cloneCodexLeaseV2Envelope(*store.v2)
			store.directory = &failingSecureDirectory{SecureDirectory: store.directory, fsys: test.fsys}

			err := store.Compact(time.Time{}, 0)
			if err == nil || fsutil.AtomicWriteOutcome(err) != test.wantOutcome {
				t.Fatalf("compact outcome = %v error %v, want %v", fsutil.AtomicWriteOutcome(err), err, test.wantOutcome)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, beforeBytes) || !reflect.DeepEqual(*store.v2, beforeEnvelope) {
				t.Fatalf("failed compaction published memory: generation %d envelope %#v", store.Generation(), store.v2)
			}
			if got := store.poisoned != nil; got != test.wantPoisoned {
				t.Fatalf("poisoned = %v, want %v: %v", got, test.wantPoisoned, store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CompactRetainsLiveReferencesPastBoundary(t *testing.T) {
	tests := []struct {
		name                 string
		routingRefs          int
		attemptRefs          int
		responseObserverRefs int
		lineageExtinct       bool
	}{
		{name: "routing reference", routingRefs: 1, lineageExtinct: false},
		{name: "attempt reference", attemptRefs: 1, lineageExtinct: false},
		{name: "response observer reference", responseObserverRefs: 1, lineageExtinct: false},
		{name: "live socket lineage", lineageExtinct: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := openCodexLeaseV2LaneTestCoordinator(t)
			store := coordinator.Store()
			currentIdentity := CodexJournalRecordIdentity{
				LaneDigest:    codexJournalLaneDigest(store.hash("session", "session"), store.hash("thread", "thread"), store.hash("namespace", CodexResponsesNamespace)),
				TurnDigest:    store.hash("turn", "current"),
				ModeEpoch:     9,
				Authoritative: true,
			}
			restored, err := store.LoadLane(
				LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"},
				[]codex.AccountKey{"account-one"},
				CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}},
			)
			if err != nil {
				t.Fatal(err)
			}
			current := findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, currentIdentity)
			mutation := codexLeaseV2CASTestMutationRecord(current)
			mutation.RoutingRefs = test.routingRefs
			mutation.AttemptRefs = test.attemptRefs
			mutation.ResponseObserverRefs = test.responseObserverRefs
			mutation.SocketLineageExtinct = test.lineageExtinct
			fence, err := restored.MutationFence(currentIdentity, CodexJournalRecordIdentity{
				LaneDigest:    currentIdentity.LaneDigest,
				TurnDigest:    current.PredecessorTurnHash,
				ModeEpoch:     current.PredecessorModeEpoch,
				Authoritative: current.PredecessorAuthoritative,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); err != nil {
				t.Fatal(err)
			}
			store.policy.Retention = time.Hour
			store.policy.Now = func() time.Time { return time.Date(2026, 8, 10, 7, 2, 0, 0, time.UTC) }
			if err := store.Compact(time.Time{}, time.Nanosecond); err != nil {
				t.Fatal(err)
			}
			retained := findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, currentIdentity)
			if store.Generation() != restored.Fence.Journal+2 || retained.State != LeaseProvisional || retained.RoutingRefs != test.routingRefs || retained.AttemptRefs != test.attemptRefs || retained.ResponseObserverRefs != test.responseObserverRefs || retained.SocketLineageExtinct != test.lineageExtinct {
				t.Fatalf("compaction did not retain %s: generation=%d record=%#v", test.name, store.Generation(), retained)
			}
		})
	}
}

func TestCodexLeaseV2RetentionActionRetainsEachLivePredicate(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	base := CodexJournalRecordV2{
		State:                LeaseProvisional,
		SocketLineageExtinct: true,
		LastObservedAt:       now.Add(-2 * time.Hour),
	}
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2)
	}{
		{name: "routing reference", mutate: func(record *CodexJournalRecordV2) { record.RoutingRefs = 1 }},
		{name: "attempt reference", mutate: func(record *CodexJournalRecordV2) { record.AttemptRefs = 1 }},
		{name: "response observer reference", mutate: func(record *CodexJournalRecordV2) { record.ResponseObserverRefs = 1 }},
		{name: "socket lineage", mutate: func(record *CodexJournalRecordV2) { record.SocketLineageExtinct = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := base
			test.mutate(&record)
			if action := codexLeaseRetentionActionFor(record, now, time.Hour); action != codexLeaseRetentionKeep {
				t.Fatalf("retention action = %v, want keep for %s", action, test.name)
			}
		})
	}
}

func TestExpireCodexLeaseRecordPreservesSocketGenerationsAndClearsEveryReference(t *testing.T) {
	createdAt := time.Date(2026, 8, 9, 7, 1, 0, 0, time.UTC)
	observedAt := createdAt.Add(time.Minute)
	now := observedAt.Add(2 * time.Hour)
	record := CodexJournalRecordV2{
		RecordGeneration:           11,
		LeaseGeneration:            7,
		DownstreamSocketGeneration: 41,
		UpstreamSocketGeneration:   73,
		State:                      LeaseBoundActive,
		CodexCurrentRequest: CodexCurrentRequest{
			Generation:           5,
			RequestKind:          CodexRequestTurn,
			RequestedModelHash:   "request-hash",
			EffectiveModel:       "gpt-effective",
			RequiredBuckets:      []CapacityBucket{CapacityBucketBase},
			RoutingRefs:          2,
			AttemptRefs:          3,
			ResponseObserverRefs: 4,
			Attempts: []CodexJournalAttempt{{
				Generation:     1,
				Revision:       6,
				Slot:           1,
				State:          CodexAttemptStreaming,
				CreatedAt:      createdAt,
				LastObservedAt: observedAt,
			}},
		},
		CreatedAt:      createdAt,
		LastObservedAt: observedAt,
	}
	beforeRequest := cloneCodexCurrentRequest(record.CodexCurrentRequest)

	if err := expireCodexLeaseRecord(&record, now); err != nil {
		t.Fatal(err)
	}

	if record.DownstreamSocketGeneration != 41 || record.UpstreamSocketGeneration != 73 || !record.SocketLineageExtinct {
		t.Fatalf("expired socket lineage = downstream %d upstream %d extinct %v, want 41/73/true", record.DownstreamSocketGeneration, record.UpstreamSocketGeneration, record.SocketLineageExtinct)
	}
	if record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 {
		t.Fatalf("expired references = routing %d attempt %d observer %d, want all zero", record.RoutingRefs, record.AttemptRefs, record.ResponseObserverRefs)
	}
	if record.RecordGeneration != 12 || record.LeaseGeneration != 8 || record.State != LeaseExpired || record.CreatedAt != createdAt || record.LastObservedAt != now {
		t.Fatalf("expired lifecycle = %#v", record)
	}
	wantRequest := beforeRequest
	wantRequest.RoutingRefs = 0
	wantRequest.AttemptRefs = 0
	wantRequest.ResponseObserverRefs = 0
	wantRequest.Attempts[0].State = CodexAttemptIndeterminate
	wantRequest.Attempts[0].Revision++
	wantRequest.Attempts[0].LastObservedAt = now
	if !record.NonMigratable {
		t.Fatal("expired indeterminate request remained migratable")
	}
	if !reflect.DeepEqual(record.CodexCurrentRequest, wantRequest) {
		t.Fatalf("expired current request = %#v, want %#v", record.CodexCurrentRequest, wantRequest)
	}
}

func TestCodexLeaseV2CompactLatchesIndeterminateNonMigratableAcrossReopen(t *testing.T) {
	store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	mutation := codexLeaseV2CASTestMutationRecord(record)
	mutation.SocketLineageExtinct = true
	mutation.RoutingRefs = 0
	mutation.AttemptRefs = 0
	mutation.ResponseObserverRefs = 0
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())}
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = record.LastObservedAt.Add(2 * time.Hour)
	store.policy.Retention = time.Hour
	beforeGeneration := store.Generation()
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	expired := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if store.Generation() != beforeGeneration+1 || expired.State != LeaseExpired || codexLeaseCurrentAttemptState(expired) != CodexAttemptIndeterminate || !expired.NonMigratable {
		t.Fatalf("compacted live request = generation %d record %#v", store.Generation(), expired)
	}

	fsys := store.fs
	keyPath := store.keyPath
	journalPath := store.path
	policy := store.policy
	modes := cloneCodexModeSnapshot(store.modes)
	journal := append([]byte(nil), store.journalBytes...)
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     keyPath,
		JournalPath: journalPath,
		Policy:      policy,
		Modes:       modes,
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, record.Identity())
	if reopened.Store().Generation() != beforeGeneration+1 || !bytes.Equal(reopened.Store().journalBytes, journal) || restored.State != LeaseExpired || codexLeaseCurrentAttemptState(restored) != CodexAttemptIndeterminate || !restored.NonMigratable {
		t.Fatalf("reopened compacted request = generation %d record %#v", reopened.Store().Generation(), restored)
	}
}

func TestCodexLeaseV2CompactExpiryReopensWithExactCurrentRequestAndSocketGenerations(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	identity := CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(store.hash("session", "session"), store.hash("thread", "thread"), store.hash("namespace", CodexResponsesNamespace)),
		TurnDigest:    store.hash("turn", "current"),
		ModeEpoch:     9,
		Authoritative: true,
	}
	restored, err := store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"},
		[]codex.AccountKey{"account-one"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}},
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeSocketMutation := findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, identity)
	predecessor := CodexJournalRecordIdentity{
		LaneDigest:    identity.LaneDigest,
		TurnDigest:    beforeSocketMutation.PredecessorTurnHash,
		ModeEpoch:     beforeSocketMutation.PredecessorModeEpoch,
		Authoritative: beforeSocketMutation.PredecessorAuthoritative,
	}
	fence, err := restored.MutationFence(identity, predecessor)
	if err != nil {
		t.Fatal(err)
	}
	mutation := codexLeaseV2CASTestMutationRecord(beforeSocketMutation)
	mutation.DownstreamSocketGeneration = 41
	mutation.UpstreamSocketGeneration = 73
	mutation.SocketLineageExtinct = true
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); err != nil {
		t.Fatal(err)
	}

	beforeExpiry := findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, identity)
	beforeLane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, identity.LaneDigest)
	now := beforeExpiry.LastObservedAt.Add(time.Hour).Add(time.Nanosecond)
	store.policy.Retention = time.Hour
	store.policy.Now = func() time.Time { return now }
	beforeJournalGeneration := store.Generation()
	if err := store.Compact(time.Time{}, time.Nanosecond); err != nil {
		t.Fatal(err)
	}

	wantRecord := cloneCodexJournalRecordV2(beforeExpiry)
	wantRecord.State = LeaseExpired
	wantRecord.RecordGeneration++
	wantRecord.LeaseGeneration++
	wantRecord.LaneGeneration = beforeLane.Generation + 1
	wantRecord.SocketLineageExtinct = true
	wantRecord.RoutingRefs = 0
	wantRecord.AttemptRefs = 0
	wantRecord.ResponseObserverRefs = 0
	wantRecord.LastObservedAt = now
	switch wantRecord.Attempts[0].State {
	case CodexAttemptPrepared, CodexAttemptDispatched, CodexAttemptStreaming:
		wantRecord.Attempts[0].State = CodexAttemptIndeterminate
		wantRecord.Attempts[0].Revision++
		wantRecord.Attempts[0].LastObservedAt = now
	}
	wantLane := beforeLane
	wantLane.Generation++
	wantLane.CurrentTurnHash = ""
	wantLane.CurrentModeEpoch = 0
	wantLane.CurrentAuthoritative = false
	wantLane.LastObservedAt = now

	if store.Generation() != beforeJournalGeneration+1 || len(store.v2.Records) != 1 || len(store.v2.Lanes) != 1 {
		t.Fatalf("expiry shape = journal %d records %d lanes %d, want %d/1/1", store.Generation(), len(store.v2.Records), len(store.v2.Lanes), beforeJournalGeneration+1)
	}
	if !reflect.DeepEqual(store.v2.Records[0], wantRecord) || !reflect.DeepEqual(store.v2.Lanes[0], wantLane) {
		t.Fatalf("expiry after-image = record %#v lane %#v, want %#v %#v", store.v2.Records[0], store.v2.Lanes[0], wantRecord, wantLane)
	}
	if store.v2.Records[0].Generation != beforeExpiry.Generation || store.v2.Records[0].DownstreamSocketGeneration != 41 || store.v2.Records[0].UpstreamSocketGeneration != 73 {
		t.Fatalf("expiry lost bounded request or socket generations: %#v", store.v2.Records[0])
	}

	journalBytes := append([]byte(nil), store.journalBytes...)
	fsys := store.fs
	keyPath := store.keyPath
	journalPath := store.path
	policy := store.policy
	modes := cloneCodexModeSnapshot(store.modes)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     keyPath,
		JournalPath: journalPath,
		Policy:      policy,
		Modes:       modes,
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Store().Generation() != beforeJournalGeneration+1 || !bytes.Equal(reopened.Store().journalBytes, journalBytes) || len(reopened.Store().v2.Records) != 1 || !reflect.DeepEqual(reopened.Store().v2.Records[0], wantRecord) || len(reopened.Store().v2.Lanes) != 1 || !reflect.DeepEqual(reopened.Store().v2.Lanes[0], wantLane) {
		t.Fatalf("reopened expiry authority = generation %d records %#v lanes %#v", reopened.Store().Generation(), reopened.Store().v2.Records, reopened.Store().v2.Lanes)
	}
}
