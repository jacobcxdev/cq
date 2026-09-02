package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2CommitLaneMergesAndRejectsStaleWithoutWriting(t *testing.T) {
	t.Parallel()
	store, fsys, now := openCodexLeaseV2CASTestStore(t)

	recordA := reservingCodexLeaseV2CASTestRecord(store, "session", "thread-a", "turn-a")
	fenceA, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        1,
		TouchedRecords: []CodexLeaseRecordFence{{Record: recordA.Identity()}},
	}, CodexLaneMutation{
		Lane:          codexLeaseV2CASTestLane(recordA),
		UpsertRecords: []CodexJournalRecordV2{recordA},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordB := reservingCodexLeaseV2CASTestRecord(store, "session", "thread-b", "turn-b")
	fenceB, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        fenceA.Journal,
		TouchedRecords: []CodexLeaseRecordFence{{Record: recordB.Identity()}},
	}, CodexLaneMutation{
		Lane:          codexLeaseV2CASTestLane(recordB),
		UpsertRecords: []CodexJournalRecordV2{recordB},
	})
	if err != nil {
		t.Fatal(err)
	}

	beforeStale, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitLane(fenceA, CodexLaneMutation{
		UpsertRecords: []CodexJournalRecordV2{recordA},
	})
	if !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale CommitLane error = %T %v, want ErrCodexLeaseStaleMutation", err, err)
	}
	afterStale, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterStale, beforeStale) {
		t.Fatal("stale CommitLane changed durable bytes")
	}

	*now = now.Add(time.Second)
	rebuiltA := fenceA
	rebuiltA.Journal = fenceB.Journal
	rebuiltA.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, recordA.Identity())}
	postA, err := store.CommitLane(rebuiltA, CodexLaneMutation{
		UpsertRecords: []CodexJournalRecordV2{recordA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if postA.Journal != fenceB.Journal+1 || postA.Lane != fenceA.Lane {
		t.Fatalf("post fence journal/lane = %d/%d, want %d/%d", postA.Journal, postA.Lane, fenceB.Journal+1, fenceA.Lane)
	}

	envelope := readCodexLeaseV2CASTestEnvelope(t, fsys)
	storedA := findCodexLeaseV2CASTestRecord(t, envelope.Records, recordA.Identity())
	storedB := findCodexLeaseV2CASTestRecord(t, envelope.Records, recordB.Identity())
	if storedA.RecordGeneration != 2 || storedA.LeaseGeneration != 1 || storedA.LaneGeneration != 1 {
		t.Fatalf("A generations = record %d lease %d lane %d, want 2/1/1", storedA.RecordGeneration, storedA.LeaseGeneration, storedA.LaneGeneration)
	}
	if storedB.RecordGeneration != 1 || storedB.LeaseGeneration != 1 || storedB.LaneGeneration != 1 {
		t.Fatalf("unmentioned B generations changed: record %d lease %d lane %d", storedB.RecordGeneration, storedB.LeaseGeneration, storedB.LaneGeneration)
	}
	if storedB.CreatedAt != storedB.LastObservedAt || storedB.LastObservedAt != now.Add(-time.Second) {
		t.Fatalf("unmentioned B timestamps changed: created=%v observed=%v", storedB.CreatedAt, storedB.LastObservedAt)
	}
}

func TestCodexLeaseV2CommitLaneOwnsAttemptGenerationsAndLimit(t *testing.T) {
	t.Parallel()
	store, fsys, now := openCodexLeaseV2CASTestStore(t)
	record := provisionalCodexLeaseV2CASTestRecord(store, "session", "thread", "turn")
	record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, stored := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
	if stored.CurrentAttemptGeneration != 1 || len(stored.Attempts) != 1 || stored.Attempts[0].Generation != 1 || stored.Attempts[0].Revision != 1 {
		t.Fatalf("prepared attempt = current %d rows %#v", stored.CurrentAttemptGeneration, stored.Attempts)
	}

	*now = now.Add(time.Second)
	dispatched := codexLeaseV2CASTestMutationRecord(stored)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	fence.TouchedRecords = []CodexLeaseRecordFence{{
		Record:            stored.Identity(),
		Revision:          stored.RecordGeneration,
		Lease:             stored.LeaseGeneration,
		RequestGeneration: stored.Generation,
		CurrentAttempt:    stored.CurrentAttemptGeneration,
		TouchedAttempts: []CodexAttemptFence{{
			RequestGeneration: stored.Generation,
			Generation:        1,
			Revision:          1,
		}},
	}}
	fence, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{dispatched}})
	if err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if stored.Attempts[0].Generation != 1 || stored.Attempts[0].Revision != 2 || stored.Attempts[0].State != CodexAttemptDispatched || stored.LeaseGeneration != 2 {
		t.Fatalf("dispatched attempt/generation = %#v lease=%d", stored.Attempts[0], stored.LeaseGeneration)
	}

	*now = now.Add(time.Second)
	rollover := codexLeaseV2CASTestMutationRecord(stored)
	rollover.CurrentAttemptGeneration = 0
	rollover.Attempts[0].State = CodexAttemptProviderFailed
	rollover.Attempts = append(rollover.Attempts, CodexJournalAttempt{Slot: 2, State: CodexAttemptPrepared})
	fence.TouchedRecords = []CodexLeaseRecordFence{{
		Record:            stored.Identity(),
		Revision:          stored.RecordGeneration,
		Lease:             stored.LeaseGeneration,
		RequestGeneration: stored.Generation,
		CurrentAttempt:    stored.CurrentAttemptGeneration,
		TouchedAttempts: []CodexAttemptFence{
			{RequestGeneration: stored.Generation, Generation: 1, Revision: 2},
			{RequestGeneration: stored.Generation, Generation: 0, Revision: 0},
		},
	}}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{rollover}})
	if err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if stored.CurrentAttemptGeneration != 2 || len(stored.Attempts) != 2 {
		t.Fatalf("rollover current/rows = %d/%d, want 2/2", stored.CurrentAttemptGeneration, len(stored.Attempts))
	}
	if stored.Attempts[0].Generation != 1 || stored.Attempts[0].Revision != 3 || stored.Attempts[0].State != CodexAttemptProviderFailed {
		t.Fatalf("terminal attempt = %#v", stored.Attempts[0])
	}
	if stored.Attempts[1].Generation != 2 || stored.Attempts[1].Revision != 1 || stored.Attempts[1].State != CodexAttemptPrepared {
		t.Fatalf("replacement attempt = %#v", stored.Attempts[1])
	}

	beforeLimit, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	tooMany := codexLeaseV2CASTestMutationRecord(stored)
	tooMany.CurrentAttemptGeneration = 0
	tooMany.Attempts = append(tooMany.Attempts, CodexJournalAttempt{Slot: 2, State: CodexAttemptPrepared})
	fence.TouchedRecords = []CodexLeaseRecordFence{{
		Record:            stored.Identity(),
		Revision:          stored.RecordGeneration,
		Lease:             stored.LeaseGeneration,
		RequestGeneration: stored.Generation,
		CurrentAttempt:    stored.CurrentAttemptGeneration,
		TouchedAttempts: []CodexAttemptFence{{
			RequestGeneration: stored.Generation,
			Generation:        0,
			Revision:          0,
		}},
	}}
	_, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{tooMany}})
	if !errors.Is(err, ErrCodexLeaseAttemptLimit) {
		t.Fatalf("attempt-limit error = %T %v, want ErrCodexLeaseAttemptLimit", err, err)
	}
	afterLimit, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterLimit, beforeLimit) {
		t.Fatal("attempt-limit rejection changed durable bytes")
	}
}

func TestCodexLeaseV2CommitLaneRejectsNonTerminalAttemptRolloverWithoutWriting(t *testing.T) {
	store, fence, record, _ := openDispatchedCodexLeaseV2AffinityTestStore(t)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	rollover := codexLeaseV2CASTestMutationRecord(record)
	rollover.CurrentAttemptGeneration = 0
	rollover.Attempts = append(rollover.Attempts, CodexJournalAttempt{Slot: 2, State: CodexAttemptPrepared})
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: 0}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}

	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{rollover}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("non-terminal attempt rollover error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("non-terminal rollover changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneRejectsFirstTurnStateWithoutCurrentLatch(t *testing.T) {
	store, fence, _, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
	streaming.HasTurnState = true
	streaming.TurnStateHash = store.hash("turn-state", "private-provider-state")
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("unlatched first turn state error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("unlatched first turn state changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneRunsStrictValidationBeforeWriting(t *testing.T) {
	store, fsys, _ := openCodexLeaseV2CASTestStore(t)
	record := reservingCodexLeaseV2CASTestRecord(store, "memory-session", "memory-thread", "memory-turn")
	record.RequestKind = CodexRequestMemory
	before, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := store.Generation()
	_, err = store.CommitLane(
		CodexLeaseGenerationFence{Journal: beforeGeneration, TouchedRecords: []CodexLeaseRecordFence{{Record: record.Identity()}}},
		CodexLaneMutation{Lane: codexLeaseV2CASTestLane(record), UpsertRecords: []CodexJournalRecordV2{record}},
	)
	if !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("strict prewrite error = %T %v", err, err)
	}
	after, readErr := fsys.ReadFile("/state/leases.json")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(after, before) || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("strict-invalid mutation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneFreezesRouteChoiceWithoutNewAttempt(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2, *CodexLeaseStore)
	}{
		{name: "requested model", mutate: func(record *CodexJournalRecordV2, store *CodexLeaseStore) {
			record.RequestedModelHash = store.hash("requested-model", "different-request")
		}},
		{name: "effective model", mutate: func(record *CodexJournalRecordV2, _ *CodexLeaseStore) {
			record.EffectiveModel = "different-effective-model"
		}},
		{name: "required buckets", mutate: func(record *CodexJournalRecordV2, _ *CodexLeaseStore) {
			record.RequiredBuckets = []CapacityBucket{CapacityBucketBase, CapacityBucket("model:different")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, record, _ := openDispatchedCodexLeaseV2AffinityTestStore(t)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			mutation := codexLeaseV2CASTestMutationRecord(record)
			test.mutate(&mutation, store)
			fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())}
			if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("route-choice mutation error = %T %v", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
				t.Fatalf("route-choice mutation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CommitLaneRequiresCleanReservingRecordFirst(t *testing.T) {
	tests := []struct {
		name   string
		record func(*CodexLeaseStore) CodexJournalRecordV2
	}{
		{name: "provisional", record: func(store *CodexLeaseStore) CodexJournalRecordV2 {
			record := provisionalCodexLeaseV2CASTestRecord(store, "direct-session", "direct-thread", "direct-provisional")
			record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
			return record
		}},
		{name: "admitted", record: func(store *CodexLeaseStore) CodexJournalRecordV2 {
			record := provisionalCodexLeaseV2CASTestRecord(store, "direct-session", "direct-thread", "direct-admitted")
			record.State = LeaseBoundActive
			record.RoutingRefs = 1
			record.AttemptRefs = 1
			record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptStreaming}}
			return record
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fsys, _ := openCodexLeaseV2CASTestStore(t)
			record := test.record(store)
			before, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CommitLane(CodexLeaseGenerationFence{
				Journal: store.Generation(),
				TouchedRecords: []CodexLeaseRecordFence{{
					Record:          record.Identity(),
					TouchedAttempts: []CodexAttemptFence{{Generation: 0}},
				}},
			}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(record), UpsertRecords: []CodexJournalRecordV2{record}})
			if !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("direct new %s record error = %T %v", test.name, err, err)
			}
			after, readErr := fsys.ReadFile("/state/leases.json")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) || store.Generation() != 1 || store.poisoned != nil {
				t.Fatalf("direct new %s record changed authority: generation %d poison %v", test.name, store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CommitLaneFreezesProtocolWithinRequestGeneration(t *testing.T) {
	tests := []struct {
		name    string
		initial func(*CodexJournalRecordV2)
		mutate  func(*CodexJournalRecordV2)
	}{
		{name: "request kind", mutate: func(record *CodexJournalRecordV2) {
			record.RequestKind = CodexRequestCompaction
			record.CompactionPhase = CodexCompactionStandalone
		}},
		{name: "compaction phase", initial: func(record *CodexJournalRecordV2) {
			record.RequestKind = CodexRequestCompaction
			record.CompactionPhase = CodexCompactionStandalone
		}, mutate: func(record *CodexJournalRecordV2) {
			record.CompactionPhase = CodexCompactionPreTurn
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fsys, _ := openCodexLeaseV2CASTestStore(t)
			record := provisionalCodexLeaseV2CASTestRecord(store, "protocol-session", "protocol-thread", test.name)
			if test.initial != nil {
				test.initial(&record)
			}
			record.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
			fence, stored := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, record)
			mutation := codexLeaseV2CASTestMutationRecord(stored)
			test.mutate(&mutation)
			fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, stored.Identity())}
			before, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}})
			if !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("request protocol mutation error = %T %v", err, err)
			}
			after, readErr := fsys.ReadFile("/state/leases.json")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) || store.poisoned != nil {
				t.Fatalf("request protocol mutation changed authority: poison %v", store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CommitLaneRejectsRolloverOntoConsumedSlotWithoutWriting(t *testing.T) {
	store, fence, record, _ := openDispatchedCodexLeaseV2AffinityTestStore(t)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	rollover := codexLeaseV2CASTestMutationRecord(record)
	rollover.CurrentAttemptGeneration = 0
	rollover.Attempts[0].State = CodexAttemptProviderFailed
	rollover.Attempts = append(rollover.Attempts, CodexJournalAttempt{Slot: 1, State: CodexAttemptPrepared})
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{
		{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision},
		{RequestGeneration: record.Generation, Generation: 0},
	}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}

	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{rollover}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("consumed-slot rollover error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("consumed-slot rollover changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneRejectsSuccessorWhilePredecessorWorkIsLive(t *testing.T) {
	store, fence, predecessor, _ := openDispatchedCodexLeaseV2AffinityTestStore(t)
	successor := reservingCodexLeaseV2CASTestRecord(store, "affinity-failure-session", "affinity-failure-thread", "concurrent-successor")
	successor.ModeEpoch = predecessor.ModeEpoch
	successor.Authoritative = predecessor.Authoritative
	successor.PredecessorTurnHash = predecessor.TurnHash
	successor.PredecessorModeEpoch = predecessor.ModeEpoch
	successor.PredecessorAuthoritative = predecessor.Authoritative
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
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	if _, err := store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: []CodexJournalRecordV2{successor}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("live-predecessor successor error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("live-predecessor successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneRejectsSuccessorWhileResponseObserverIsLive(t *testing.T) {
	store, fence, predecessor, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	*now = now.Add(time.Second)
	fence, predecessor = commitCodexLeaseV2AffinityState(t, store, fence, predecessor, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)
	*now = now.Add(time.Second)
	observed := codexLeaseV2CASTestMutationRecord(predecessor)
	observed.SocketLineageExtinct = false
	observed.ResponseObserverRefs = 1
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, predecessor.Identity())}
	var err error
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{observed}})
	if err != nil {
		t.Fatal(err)
	}
	predecessor = findCodexLeaseV2CASTestRecord(t, store.v2.Records, predecessor.Identity())

	successor := reservingCodexLeaseV2CASTestRecord(store, "affinity-helper-session", "affinity-helper-thread", "observer-successor")
	successor.ModeEpoch = predecessor.ModeEpoch
	successor.Authoritative = predecessor.Authoritative
	successor.PredecessorTurnHash = predecessor.TurnHash
	successor.PredecessorModeEpoch = predecessor.ModeEpoch
	successor.PredecessorAuthoritative = predecessor.Authoritative
	lane := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, predecessor.Identity().LaneDigest))
	lane.CurrentTurnHash = successor.TurnHash
	lane.CurrentModeEpoch = successor.ModeEpoch
	lane.CurrentAuthoritative = successor.Authoritative
	lane.LastTurnHash = successor.TurnHash
	lane.LastModeEpoch = successor.ModeEpoch
	lane.LastAuthoritative = successor.Authoritative
	superseded := codexLeaseV2CASTestMutationRecord(predecessor)
	superseded.State = LeaseSuperseded
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, predecessor.Identity()),
		{Record: successor.Identity()},
	}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: []CodexJournalRecordV2{superseded, successor}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("live response-observer successor error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("live response-observer successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneRejectsHistoricalHeadResurrection(t *testing.T) {
	store, _, _ := openCodexLeaseV2CASTestStore(t)
	first := reservingCodexLeaseV2CASTestRecord(store, "head-session", "head-thread", "first")
	fence, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        store.Generation(),
		TouchedRecords: []CodexLeaseRecordFence{{Record: first.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(first), UpsertRecords: []CodexJournalRecordV2{first}})
	if err != nil {
		t.Fatal(err)
	}
	first = findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.Identity())
	failedFirst := codexLeaseV2CASTestMutationRecord(first)
	failedFirst.State = LeaseFailedUnadmitted
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, first.Identity())}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{failedFirst}})
	if err != nil {
		t.Fatal(err)
	}
	first = findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.Identity())

	second := reservingCodexLeaseV2CASTestRecord(store, "head-session", "head-thread", "second")
	second.PredecessorTurnHash = first.TurnHash
	second.PredecessorModeEpoch = first.ModeEpoch
	second.PredecessorAuthoritative = first.Authoritative
	lane := codexLeaseV2CASTestLane(second)
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, first.Identity()),
		{Record: second.Identity()},
	}
	fence, err = store.CommitLane(fence, CodexLaneMutation{Lane: lane, UpsertRecords: []CodexJournalRecordV2{second}})
	if err != nil {
		t.Fatal(err)
	}
	second = findCodexLeaseV2CASTestRecord(t, store.v2.Records, second.Identity())
	failedSecond := codexLeaseV2CASTestMutationRecord(second)
	failedSecond.State = LeaseFailedUnadmitted
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, second.Identity()),
		codexLeaseV2CASTestRecordFence(store.v2.Records, first.Identity()),
	}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{failedSecond}})
	if err != nil {
		t.Fatal(err)
	}

	resurrected := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, second.Identity().LaneDigest))
	resurrected.CurrentTurnHash = first.TurnHash
	resurrected.CurrentModeEpoch = first.ModeEpoch
	resurrected.CurrentAuthoritative = first.Authoritative
	resurrected.LastTurnHash = first.TurnHash
	resurrected.LastModeEpoch = first.ModeEpoch
	resurrected.LastAuthoritative = first.Authoritative
	fence.TouchedRecords = nil
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{Lane: &resurrected}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("historical head resurrection error = %T %v", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("historical head resurrection changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2AdmittedRecordCanPersistNextPreparedDispatchedAndStreamingRequest(t *testing.T) {
	store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	admissionGeneration := record.AdmissionJournalGeneration
	admissionRequestGeneration := record.AdmissionRequestGeneration
	admittedAt := record.AdmittedAt
	*now = now.Add(time.Second)
	fence, record = commitCodexLeaseV2AffinityState(t, store, fence, record, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)

	*now = now.Add(time.Second)
	priorRequest := cloneCodexCurrentRequest(record.CodexCurrentRequest)
	fence, record = beginNextCodexLeaseV2CASTestRequest(t, store, fence, record, "next-request")
	if record.State != LeaseBoundActive || record.Generation != priorRequest.Generation+1 || len(record.Attempts) != 1 || record.Attempts[0].Generation != 1 || record.Attempts[0].State != CodexAttemptPrepared || record.CurrentAttemptGeneration != 1 {
		t.Fatalf("prepared resampling state = %#v", record)
	}
	if reflect.DeepEqual(record.CodexCurrentRequest, priorRequest) || record.AttemptEnvelope.PlanDigest == priorRequest.AttemptEnvelope.PlanDigest {
		t.Fatalf("BeginRequest retained prior request snapshot: prior %#v next %#v", priorRequest, record.CodexCurrentRequest)
	}

	for _, state := range []CodexAttemptState{CodexAttemptDispatched, CodexAttemptStreaming} {
		*now = now.Add(time.Second)
		mutation := codexLeaseV2CASTestMutationRecord(record)
		mutation.Attempts[0].State = state
		recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
		recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
		fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
		var err error
		fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}})
		if err != nil {
			t.Fatal(err)
		}
		record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
		if record.Attempts[0].State != state {
			t.Fatalf("resampling attempt state = %v, want %v", record.Attempts[0].State, state)
		}
	}
	*now = now.Add(time.Second)
	fence, record = commitCodexLeaseV2AffinityState(t, store, fence, record, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)
	*now = now.Add(time.Second)
	_, record = beginNextCodexLeaseV2CASTestRequest(t, store, fence, record, "third-request")
	if record.Generation != 3 || len(record.Attempts) != 1 || record.Attempts[0].Generation != 1 || record.Attempts[0].State != CodexAttemptPrepared {
		t.Fatalf("third bounded request = %#v", record.CodexCurrentRequest)
	}
	if !record.EverAdmitted || record.AdmissionJournalGeneration != admissionGeneration || record.AdmissionRequestGeneration != admissionRequestGeneration || record.AdmittedAt != admittedAt {
		t.Fatalf("resampling changed first-admission evidence: %#v", record)
	}
}

func TestCodexLeaseV2BeginRequestRequiresTerminalDrainedPriorRequest(t *testing.T) {
	tests := []struct {
		name                 string
		state                LeaseState
		attempt              CodexAttemptState
		routingRefs          int
		attemptRefs          int
		responseObserverRefs int
	}{
		{name: "nonterminal attempt", state: LeaseBoundActive, attempt: CodexAttemptStreaming},
		{name: "routing ref", state: LeaseBoundQuiescent, attempt: CodexAttemptProviderCompleted, routingRefs: 1},
		{name: "attempt ref", state: LeaseBoundQuiescent, attempt: CodexAttemptProviderCompleted, attemptRefs: 1},
		{name: "response observer ref", state: LeaseBoundQuiescent, attempt: CodexAttemptProviderCompleted, responseObserverRefs: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
			*now = now.Add(time.Second)
			prior := codexLeaseV2CASTestMutationRecord(record)
			prior.State = test.state
			prior.Attempts[0].State = test.attempt
			prior.RoutingRefs = test.routingRefs
			prior.AttemptRefs = test.attemptRefs
			prior.ResponseObserverRefs = test.responseObserverRefs
			prior.SocketLineageExtinct = test.routingRefs == 0 && test.attemptRefs == 0 && test.responseObserverRefs == 0
			priorFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
			if prior.Attempts[0].State != record.Attempts[0].State {
				priorFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
			}
			fence.TouchedRecords = []CodexLeaseRecordFence{priorFence}
			var err error
			fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{prior}})
			if err != nil {
				t.Fatal(err)
			}
			record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
			mutation := nextCodexLeaseV2CASTestRequestMutation(store, record, test.name)
			recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
			recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
			fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
			identity := record.Identity()
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			if _, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("BeginRequest error = %T %v, want invalid mutation", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
				t.Fatalf("rejected BeginRequest changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2GenericAttemptAppendCannotReplaceTerminalRequest(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2)
	}{
		{
			name: "completed admitted request",
			setup: func(t *testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2) {
				store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
				*now = now.Add(time.Second)
				fence, record = commitCodexLeaseV2AffinityState(t, store, fence, record, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)
				return store, fence, record
			},
		},
		{
			name: "indeterminate admitted request",
			setup: func(t *testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2) {
				store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
				*now = now.Add(time.Second)
				mutation := codexLeaseV2CASTestMutationRecord(record)
				mutation.State = LeaseOrphaned
				mutation.NonMigratable = true
				mutation.SocketLineageExtinct = true
				mutation.RoutingRefs = 0
				mutation.AttemptRefs = 0
				mutation.ResponseObserverRefs = 0
				mutation.Attempts[0].State = CodexAttemptIndeterminate
				recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
				recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
				fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
				var err error
				fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}})
				if err != nil {
					t.Fatal(err)
				}
				return store, fence, findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
			},
		},
		{
			name: "abandoned never-admitted request",
			setup: func(t *testing.T) (*CodexLeaseStore, CodexLeaseGenerationFence, CodexJournalRecordV2) {
				store, fsys, now := openCodexLeaseV2CASTestStore(t)
				desired := provisionalCodexLeaseV2CASTestRecord(store, "abandoned-session", "abandoned-thread", "abandoned-turn")
				desired.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
				_, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, desired)
				identity := record.Identity()
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
				restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
				lane := findCodexLeaseV2CASTestLane(t, reopened.Store().v2.Lanes, identity.LaneDigest)
				return reopened.Store(), CodexLeaseGenerationFence{
					Journal: reopened.Store().Generation(),
					Lane:    lane.Generation,
					Current: identity,
					Last:    identity,
				}, restored
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, record := test.setup(t)
			oldRequestGeneration := record.Generation
			mutation := codexLeaseV2CASTestMutationRecord(record)
			mutation.State = LeaseBoundActive
			if !record.EverAdmitted {
				mutation.State = LeaseProvisional
			}
			mutation.SocketLineageExtinct = false
			mutation.RoutingRefs = 1
			mutation.CurrentAttemptGeneration = 0
			mutation.Attempts = append(mutation.Attempts, CodexJournalAttempt{Slot: 2, State: CodexAttemptPrepared})
			recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
			recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: 0}}
			fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("generic terminal-request append error = %T %v, want invalid mutation", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
				t.Fatalf("generic terminal-request append changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}

			begin := nextCodexLeaseV2CASTestRequestMutation(store, record, test.name)
			if !record.EverAdmitted {
				begin.State = LeaseProvisional
			}
			recordFence = codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
			recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
			fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
			identity := record.Identity()
			if _, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{begin}}); err != nil {
				t.Fatalf("typed BeginRequest error = %T %v", err, err)
			}
			next := findCodexLeaseV2CASTestRecord(t, store.v2.Records, identity)
			if next.Generation != oldRequestGeneration+1 || len(next.Attempts) != 1 || next.Attempts[0].Generation != 1 || next.Attempts[0].State != CodexAttemptPrepared {
				t.Fatalf("typed BeginRequest result = %#v", next.CodexCurrentRequest)
			}
		})
	}
}

func TestCodexLeaseV2BeginRequestRequiresCurrentLaneHead(t *testing.T) {
	store, _, _ := openCodexLeaseV2CASTestStore(t)
	lane, historical := codexLeaseV2RemovalRecord(store, "historical-head", LeaseOrphaned, true, CodexRequestTurn, "account")
	current := cloneCodexJournalRecordV2(historical)
	current.TurnHash = store.hash("turn", "current-head")
	current.State = LeaseProvisional
	current.NonMigratable = false
	current.SocketLineageExtinct = false
	current.Attempts[0].State = CodexAttemptPrepared
	lane.CurrentTurnHash = current.TurnHash
	lane.CurrentModeEpoch = current.ModeEpoch
	lane.CurrentAuthoritative = current.Authoritative
	lane.LastTurnHash = current.TurnHash
	lane.LastModeEpoch = current.ModeEpoch
	lane.LastAuthoritative = current.Authoritative

	store.mu.Lock()
	next := cloneCodexLeaseV2Envelope(*store.v2)
	next.Lanes = []CodexJournalLane{lane}
	next.Records = []CodexJournalRecordV2{historical, current}
	err := store.commitV2Locked(store.v2.Generation, next)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	historical = findCodexLeaseV2CASTestRecord(t, store.v2.Records, historical.Identity())
	current = findCodexLeaseV2CASTestRecord(t, store.v2.Records, current.Identity())
	mutation := nextCodexLeaseV2CASTestRequestMutation(store, historical, "historical-request")
	mutation.State = LeaseProvisional
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, historical.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	fence := CodexLeaseGenerationFence{
		Journal:        store.Generation(),
		Lane:           lane.Generation,
		Current:        current.Identity(),
		Last:           current.Identity(),
		TouchedRecords: []CodexLeaseRecordFence{recordFence},
	}
	identity := historical.Identity()
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("historical BeginRequest error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("historical BeginRequest changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2ConcurrentBeginRequestHasOneWinnerAndOldRequestStaysStale(t *testing.T) {
	store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
	*now = now.Add(time.Second)
	fence, record = commitCodexLeaseV2AffinityState(t, store, fence, record, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)
	oldRecord := cloneCodexJournalRecordV2(record)
	oldRecordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	mutation := nextCodexLeaseV2CASTestRequestMutation(store, record, "concurrent-next")
	beginFence := oldRecordFence
	beginFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	fence.TouchedRecords = []CodexLeaseRecordFence{beginFence}
	identity := record.Identity()

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}})
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
			t.Fatalf("concurrent BeginRequest error = %T %v", err, err)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("concurrent BeginRequest outcomes = winners %d stale %d", winners, stale)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, identity)
	if stored.Generation != oldRecord.Generation+1 || len(stored.Attempts) != 1 || stored.Attempts[0].Generation != 1 {
		t.Fatalf("winning request snapshot = %#v", stored.CodexCurrentRequest)
	}

	lateFence := CodexLeaseGenerationFence{
		Journal: store.Generation(),
		Lane:    fence.Lane,
		Current: fence.Current,
		Last:    fence.Last,
		TouchedRecords: []CodexLeaseRecordFence{{
			Record:            oldRecordFence.Record,
			Revision:          oldRecordFence.Revision,
			Lease:             oldRecordFence.Lease,
			RequestGeneration: oldRecordFence.RequestGeneration,
			CurrentAttempt:    oldRecordFence.CurrentAttempt,
		}},
	}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(lateFence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{codexLeaseV2CASTestMutationRecord(oldRecord)}}); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("late old-request callback error = %T %v, want stale", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("late old-request callback changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2RestartNormalisesEachLaterAttemptFence(t *testing.T) {
	tests := []struct {
		name         string
		attemptState CodexAttemptState
		wantLease    LeaseState
		wantAttempt  CodexAttemptState
		wantRevision uint64
	}{
		{name: "prepared is abandoned before dispatch", attemptState: CodexAttemptPrepared, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptAbandonedBeforeDispatch, wantRevision: 1},
		{name: "dispatched becomes uncertain", attemptState: CodexAttemptDispatched, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantRevision: 1},
		{name: "streaming becomes uncertain", attemptState: CodexAttemptStreaming, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantRevision: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, fence, record, now := openAdmittedCodexLeaseV2AffinityTestStore(t)
			*now = now.Add(time.Second)
			fence, record = commitCodexLeaseV2AffinityState(t, store, fence, record, LeaseBoundQuiescent, CodexAttemptProviderCompleted, false)

			*now = now.Add(time.Second)
			fence, record = beginNextCodexLeaseV2CASTestRequest(t, store, fence, record, "restart-request")
			var err error
			for _, state := range []CodexAttemptState{CodexAttemptDispatched, CodexAttemptStreaming} {
				if test.attemptState < state {
					break
				}
				*now = now.Add(time.Second)
				mutation := codexLeaseV2CASTestMutationRecord(record)
				mutation.Attempts[0].State = state
				recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
				recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
				fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
				fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutation}})
				if err != nil {
					t.Fatal(err)
				}
				record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
			}

			fsys, ok := store.fs.(*fsutil.MemFS)
			if !ok {
				t.Fatalf("lease test filesystem = %T, want *fsutil.MemFS", store.fs)
			}
			beforeGeneration := store.Generation()
			beforeAttemptRevision := record.Attempts[0].Revision
			beforeRequestGeneration := record.Generation
			beforeAdmissionGeneration := record.AdmissionJournalGeneration
			beforeAdmittedAt := record.AdmittedAt
			identity := record.Identity()
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

			restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
			wantRevision := beforeAttemptRevision + test.wantRevision
			if reopened.Store().Generation() != beforeGeneration+1 || restored.State != test.wantLease || restored.Generation != beforeRequestGeneration || len(restored.Attempts) != 1 || restored.Attempts[0].State != test.wantAttempt || restored.Attempts[0].Revision != wantRevision || !restored.SocketLineageExtinct || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.ResponseObserverRefs != 0 {
				t.Fatalf("restart at %v = generation %d record %#v", test.attemptState, reopened.Store().Generation(), restored)
			}
			if !restored.EverAdmitted || restored.AdmissionJournalGeneration != beforeAdmissionGeneration || restored.AdmittedAt != beforeAdmittedAt {
				t.Fatalf("restart at %v changed first-admission evidence: %#v", test.attemptState, restored)
			}
		})
	}
}

func TestCodexLeaseV2NeverAdmittedIndeterminateRestartCanBeginSameAccountRequest(t *testing.T) {
	store, _, dispatched, _ := openDispatchedCodexLeaseV2AffinityTestStore(t)
	fsys, ok := store.fs.(*fsutil.MemFS)
	if !ok {
		t.Fatalf("lease test filesystem = %T, want *fsutil.MemFS", store.fs)
	}
	identity := dispatched.Identity()
	priorRequestGeneration := dispatched.Generation
	restartNow := store.policy.Now().UTC().Add(time.Minute)
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return restartNow },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}},
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
	if restored.EverAdmitted || restored.State != LeaseOrphaned || !restored.NonMigratable || restored.Generation != priorRequestGeneration || codexLeaseCurrentAttemptState(restored) != CodexAttemptIndeterminate || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.ResponseObserverRefs != 0 {
		t.Fatalf("restarted never-admitted request = %#v", restored)
	}
	lane := findCodexLeaseV2CASTestLane(t, reopened.Store().v2.Lanes, identity.LaneDigest)
	restoredFence := codexLeaseV2CASTestRecordFence(reopened.Store().v2.Records, identity)
	restoredFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	fence := CodexLeaseGenerationFence{
		Journal:        reopened.Store().Generation(),
		Lane:           lane.Generation,
		Current:        identity,
		Last:           identity,
		TouchedRecords: []CodexLeaseRecordFence{restoredFence},
	}
	mutation := nextCodexLeaseV2CASTestRequestMutation(reopened.Store(), restored, "same-account-recovery")
	mutation.State = LeaseProvisional
	post, err := reopened.Store().CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}})
	if err != nil {
		t.Fatal(err)
	}

	next := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
	if next.State != LeaseProvisional || !next.NonMigratable || next.EverAdmitted || next.Generation != priorRequestGeneration+1 || len(next.Attempts) != 1 || next.Attempts[0].Generation != 1 || next.Attempts[0].Revision != 1 || next.Attempts[0].State != CodexAttemptPrepared || !constantTimeCodexLeaseDigestEqual(next.AccountHash, restored.AccountHash) {
		t.Fatalf("same-account recovery request = %#v", next)
	}

	lateFence := post
	lateFence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence([]CodexJournalRecordV2{restored}, identity)}
	before := append([]byte(nil), reopened.Store().journalBytes...)
	beforeGeneration := reopened.Store().Generation()
	if _, err := reopened.Store().CommitLane(lateFence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{codexLeaseV2CASTestMutationRecord(restored)}}); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("late prior-request callback error = %T %v, want stale", err, err)
	}
	if reopened.Store().Generation() != beforeGeneration || !bytes.Equal(reopened.Store().journalBytes, before) || reopened.Store().poisoned != nil {
		t.Fatalf("late prior-request callback changed authority: generation %d poison %v", reopened.Store().Generation(), reopened.Store().poisoned)
	}
}

func TestCodexLeaseV2LiveIndeterminateLatchesNonMigratableAndRejectsAnotherAccount(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	desired := provisionalCodexLeaseV2CASTestRecord(store, "uncertain-session", "uncertain-thread", "uncertain-turn")
	desired.Authoritative = false
	desired.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, desired)
	*now = now.Add(time.Second)
	dispatched := codexLeaseV2CASTestMutationRecord(record)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
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
	recordFence = codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
	if err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	*now = now.Add(time.Second)
	uncertain := codexLeaseV2CASTestMutationRecord(record)
	uncertain.State = LeaseOrphaned
	uncertain.SocketLineageExtinct = true
	uncertain.RoutingRefs = 0
	uncertain.AttemptRefs = 0
	uncertain.ResponseObserverRefs = 0
	uncertain.Attempts[0].State = CodexAttemptIndeterminate
	recordFence = codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{uncertain}})
	if err != nil {
		t.Fatal(err)
	}
	record = findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if !record.NonMigratable || !record.EverAdmitted || record.State != LeaseOrphaned || codexLeaseCurrentAttemptState(record) != CodexAttemptIndeterminate {
		t.Fatalf("live uncertain record = %#v", record)
	}
	lane := findCodexLeaseV2CASTestLane(t, store.v2.Lanes, record.Identity().LaneDigest)
	if !codexLaneAffinityIsZero(lane) {
		t.Fatalf("shadow uncertainty supplied cross-turn affinity: %#v", lane)
	}

	otherAccount := store.hash("account", "other-account")
	begin := nextCodexLeaseV2CASTestRequestMutation(store, record, "other-account-request")
	begin.State = LeaseProvisional
	begin.AccountHash = otherAccount
	begin.AttemptEnvelope.Slots[0].AccountHash = otherAccount
	begin.AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(store.key, begin.AttemptEnvelope.Slots)
	recordFence = codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	identity := record.Identity()
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{begin}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("cross-account uncertain BeginRequest error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("cross-account uncertain BeginRequest changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2GenericPrewarmCannotCreateAuthoritativeBoundLease(t *testing.T) {
	store, _, now := openCodexLeaseV2CASTestStore(t)
	desired := provisionalCodexLeaseV2CASTestRecord(store, "prewarm-session", "prewarm-thread", "prewarm-turn")
	desired.RequestKind = CodexRequestPrewarm
	desired.Attempts = []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}}
	fence, record := commitNewProvisionalCodexLeaseV2CASTestRecord(t, store, desired)
	*now = now.Add(time.Second)
	dispatched := codexLeaseV2CASTestMutationRecord(record)
	dispatched.Attempts[0].State = CodexAttemptDispatched
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
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
	recordFence = codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: record.Generation, Generation: record.Attempts[0].Generation, Revision: record.Attempts[0].Revision}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("generic prewarm admission error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || store.poisoned != nil {
		t.Fatalf("generic prewarm admission changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLanePreservesTypedPostRenameUncertainty(t *testing.T) {
	store, fence, record, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
	beforeGeneration := store.Generation()
	store.directory = &failCodexLeaseV2PostSyncReadDirectory{
		SecureDirectory: store.directory,
		journalName:     store.journalName,
	}

	_, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
	if !errors.Is(err, ErrCodexLeaseStorePoisoned) || !errors.Is(err, fsutil.ErrCommitIndeterminate) {
		t.Fatalf("post-rename verification error = %T %v, want poisoned indeterminate", err, err)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if store.Generation() != beforeGeneration || stored.EverAdmitted || store.poisoned == nil {
		t.Fatalf("post-rename uncertainty published memory state: generation %d poison %v record %#v", store.Generation(), store.poisoned, stored)
	}
}

func TestCodexLeaseV2CommitLanePoisonsOnPreReplaceTrustLoss(t *testing.T) {
	store, fence, record, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
	beforeGeneration := store.Generation()
	before := append([]byte(nil), store.journalBytes...)
	store.directory = &failCodexLeaseV2PreReplaceReadDirectory{
		SecureDirectory: store.directory,
		journalName:     store.journalName,
	}

	_, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}})
	if !errors.Is(err, ErrCodexLeaseTrustLost) || !errors.Is(err, fsutil.ErrCommitNotCommitted) {
		t.Fatalf("pre-replace trust loss = %T %v, want typed not-committed trust loss", err, err)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, record.Identity())
	if store.Generation() != beforeGeneration || stored.EverAdmitted || store.poisoned == nil || !bytes.Equal(store.journalBytes, before) {
		t.Fatalf("pre-replace trust loss published memory state: generation %d poison %v record %#v", store.Generation(), store.poisoned, stored)
	}
	if _, err := store.LoadLane(testCodexLeaseKey("thread", "turn"), nil, CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}); !errors.Is(err, ErrCodexLeaseStorePoisoned) {
		t.Fatalf("poisoned store LoadLane error = %T %v, want poisoned", err, err)
	}
}

func TestCodexLeaseV2CommitLaneAvoidsRedundantJournalRevalidation(t *testing.T) {
	store, fence, _, streaming := openDispatchedCodexLeaseV2AffinityTestStore(t)
	directory := &countCodexLeaseV2JournalReadsDirectory{
		SecureDirectory: store.directory,
		journalName:     store.journalName,
	}
	store.directory = directory

	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{streaming}}); err != nil {
		t.Fatal(err)
	}
	if directory.journalReads != 7 {
		t.Fatalf("journal reads = %d, want one pre-replace journal proof", directory.journalReads)
	}
}

func TestCodexLeaseV2CommitLaneRejectsUnrepresentedAuthoritativeEpochWithoutWriting(t *testing.T) {
	store, fsys, _ := openCodexLeaseV2CASTestStore(t)
	store.mu.Lock()
	store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{}}
	store.mu.Unlock()
	record := reservingCodexLeaseV2CASTestRecord(store, "unrepresented-session", "unrepresented-thread", "unrepresented-turn")
	before, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := store.Generation()
	_, err = store.CommitLane(CodexLeaseGenerationFence{
		Journal:        beforeGeneration,
		TouchedRecords: []CodexLeaseRecordFence{{Record: record.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(record), UpsertRecords: []CodexJournalRecordV2{record}})
	if !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("unrepresented authoritative mutation error = %T %v, want authority mismatch", err, err)
	}
	after, readErr := fsys.ReadFile("/state/leases.json")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(after, before) || store.poisoned != nil {
		t.Fatalf("unrepresented authoritative mutation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CommitLaneClearsFailedHeadAndRetainsLast(t *testing.T) {
	t.Parallel()
	store, _, now := openCodexLeaseV2CASTestStore(t)
	failed := reservingCodexLeaseV2CASTestRecord(store, "session", "thread", "failed-turn")
	fence, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        1,
		TouchedRecords: []CodexLeaseRecordFence{{Record: failed.Identity()}},
	}, CodexLaneMutation{
		Lane:          codexLeaseV2CASTestLane(failed),
		UpsertRecords: []CodexJournalRecordV2{failed},
	})
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(time.Second)
	storedFailed := findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	failedMutation := codexLeaseV2CASTestMutationRecord(storedFailed)
	failedMutation.State = LeaseFailedUnadmitted
	laneAfterFailure := codexLeaseV2CASTestMutationLane(findCodexLeaseV2CASTestLane(t, store.v2.Lanes, failed.Identity().LaneDigest))
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity())}
	fence, err = store.CommitLane(fence, CodexLaneMutation{
		Lane:          &laneAfterFailure,
		UpsertRecords: []CodexJournalRecordV2{failedMutation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fence.Current.IsZero() || fence.Last != failed.Identity() || fence.Lane != 2 {
		t.Fatalf("failed head fence = current %#v last %#v lane %d", fence.Current, fence.Last, fence.Lane)
	}
	storedFailed = findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	if storedFailed.State != LeaseFailedUnadmitted {
		t.Fatalf("failed state = %v", storedFailed.State)
	}

	*now = now.Add(time.Second)
	successor := reservingCodexLeaseV2CASTestRecord(store, "session", "thread", "successor")
	successor.PredecessorTurnHash = storedFailed.TurnHash
	successor.PredecessorModeEpoch = storedFailed.ModeEpoch
	successor.PredecessorAuthoritative = storedFailed.Authoritative
	laneForSuccessor := codexLeaseV2CASTestLane(successor)
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity()),
		{Record: successor.Identity()},
	}
	post, err := store.CommitLane(fence, CodexLaneMutation{
		Lane:          laneForSuccessor,
		UpsertRecords: []CodexJournalRecordV2{successor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if post.Current != successor.Identity() || post.Last != successor.Identity() || post.Lane != 3 {
		t.Fatalf("successor fence = current %#v last %#v lane %d", post.Current, post.Last, post.Lane)
	}
	retainedFailed := findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	storedSuccessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, successor.Identity())
	if retainedFailed.State != LeaseFailedUnadmitted || retainedFailed.RecordGeneration != storedFailed.RecordGeneration {
		t.Fatalf("failed tombstone was mutated: before %#v after %#v", storedFailed, retainedFailed)
	}
	if storedSuccessor.PredecessorGeneration != retainedFailed.RecordGeneration {
		t.Fatalf("successor predecessor generation = %d, want %d", storedSuccessor.PredecessorGeneration, retainedFailed.RecordGeneration)
	}
}

func TestCodexLeaseV2CommitLaneCannotDeleteFreshHistoricalAuthority(t *testing.T) {
	store, fsys, now := openCodexLeaseV2CASTestStore(t)
	failed := reservingCodexLeaseV2CASTestRecord(store, "delete-session", "delete-thread", "failed")
	fence, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        store.Generation(),
		TouchedRecords: []CodexLeaseRecordFence{{Record: failed.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(failed), UpsertRecords: []CodexJournalRecordV2{failed}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	storedFailed := findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	failedAfter := codexLeaseV2CASTestMutationRecord(storedFailed)
	failedAfter.State = LeaseFailedUnadmitted
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity())}
	fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{failedAfter}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	storedFailed = findCodexLeaseV2CASTestRecord(t, store.v2.Records, failed.Identity())
	successor := reservingCodexLeaseV2CASTestRecord(store, "delete-session", "delete-thread", "successor")
	successor.PredecessorTurnHash = storedFailed.TurnHash
	successor.PredecessorModeEpoch = storedFailed.ModeEpoch
	successor.PredecessorAuthoritative = storedFailed.Authoritative
	fence.TouchedRecords = []CodexLeaseRecordFence{
		codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity()),
		{Record: successor.Identity()},
	}
	fence, err = store.CommitLane(fence, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(successor), UpsertRecords: []CodexJournalRecordV2{successor}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	fence.TouchedRecords = []CodexLeaseRecordFence{codexLeaseV2CASTestRecordFence(store.v2.Records, failed.Identity())}
	if _, err := store.CommitLane(fence, CodexLaneMutation{DeleteRecords: []CodexJournalRecordIdentity{failed.Identity()}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("fresh historical deletion error = %T %v, want ErrCodexLeaseInvalidMutation", err, err)
	}
	after, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || store.poisoned != nil {
		t.Fatalf("fresh historical deletion changed authority: poison %v", store.poisoned)
	}
}

func openCodexLeaseV2CASTestStore(t *testing.T) (*CodexLeaseStore, *fsutil.MemFS, *time.Time) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, codexLeaseHMACKeyBytes)
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	cutoverAt := now.Add(-time.Hour)
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV3,
		HashVersion: codexLeaseHashVersion,
		Generation:  1,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   4,
			State:                CodexLeaseCutoverComplete,
			At:                   cutoverAt,
			JournalGeneration:    1,
			CompletedAt:          cutoverAt,
			CompletionGeneration: 1,
			NoLegacyAuthority:    true,
		},
		Lanes:   []CodexJournalLane{},
		Records: []CodexJournalRecordV2{},
	}
	payload := codexLeaseV2CASTestEnvelopePayload(t, key, envelope)
	if err := fsys.WriteFile("/state/leases.json", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}},
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator.Store(), fsys, &now
}

type codexLeaseV2CASTestOwner struct{}

func (codexLeaseV2CASTestOwner) AssertOwner() error { return nil }

func (codexLeaseV2CASTestOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	return &codex.CredentialOwnerOperation{}, nil
}

func reservingCodexLeaseV2CASTestRecord(store *CodexLeaseStore, session, thread, turn string) CodexJournalRecordV2 {
	return CodexJournalRecordV2{
		SessionHash:    store.hash("session", session),
		ThreadHash:     store.hash("thread", thread),
		NamespaceHash:  store.hash("namespace", CodexResponsesNamespace),
		TurnHash:       store.hash("turn", turn),
		ModeEpoch:      9,
		State:          LeaseReserving,
		ProtocolSchema: CurrentCodexLeaseSchema,
		Authoritative:  true,
	}
}

func provisionalCodexLeaseV2CASTestRecord(store *CodexLeaseStore, session, thread, turn string) CodexJournalRecordV2 {
	record := reservingCodexLeaseV2CASTestRecord(store, session, thread, turn)
	record.State = LeaseProvisional
	record.RequestKind = CodexRequestTurn
	record.AccountHash = store.hash("account", "account")
	record.RequestedModelHash = store.hash("requested-model", "gpt-requested")
	record.EffectiveModel = "gpt-effective"
	record.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
	record.AttemptEnvelope = CodexAttemptEnvelope{
		PolicyVersion: CodexLeaseAttemptPolicyVersion,
		AttemptLimit:  2,
		Slots: []CodexAttemptSlot{
			{Index: 1, AccountHash: record.AccountHash, CandidateHash: store.hash("candidate", "candidate-one"), Kind: CodexAttemptSlotDirect},
			{Index: 2, AccountHash: record.AccountHash, CandidateHash: store.hash("candidate", "candidate-two"), Kind: CodexAttemptSlotDirect},
		},
	}
	record.AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(store.key, record.AttemptEnvelope.Slots)
	return record
}

func commitNewProvisionalCodexLeaseV2CASTestRecord(t *testing.T, store *CodexLeaseStore, desired CodexJournalRecordV2) (CodexLeaseGenerationFence, CodexJournalRecordV2) {
	t.Helper()
	reserving := cloneCodexJournalRecordV2(desired)
	reserving.State = LeaseReserving
	reserving.AccountHash = ""
	reserving.CodexCurrentRequest = CodexCurrentRequest{}
	fence, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        store.Generation(),
		TouchedRecords: []CodexLeaseRecordFence{{Record: reserving.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(reserving), UpsertRecords: []CodexJournalRecordV2{reserving}})
	if err != nil {
		t.Fatal(err)
	}
	mutation := codexLeaseV2CASTestMutationRecord(desired)
	mutation.Generation = 0
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, reserving.Identity())
	if len(desired.Attempts) != 0 {
		recordFence.TouchedAttempts = []CodexAttemptFence{{Generation: 0}}
	}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	identity := desired.Identity()
	fence, err = store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}})
	if err != nil {
		t.Fatal(err)
	}
	return fence, findCodexLeaseV2CASTestRecord(t, store.v2.Records, desired.Identity())
}

func beginNextCodexLeaseV2CASTestRequest(t *testing.T, store *CodexLeaseStore, fence CodexLeaseGenerationFence, record CodexJournalRecordV2, token string) (CodexLeaseGenerationFence, CodexJournalRecordV2) {
	t.Helper()
	mutation := nextCodexLeaseV2CASTestRequestMutation(store, record, token)
	recordFence := codexLeaseV2CASTestRecordFence(store.v2.Records, record.Identity())
	recordFence.TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
	fence.TouchedRecords = []CodexLeaseRecordFence{recordFence}
	identity := record.Identity()
	post, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}})
	if err != nil {
		t.Fatal(err)
	}
	return post, findCodexLeaseV2CASTestRecord(t, store.v2.Records, identity)
}

func nextCodexLeaseV2CASTestRequestMutation(store *CodexLeaseStore, record CodexJournalRecordV2, token string) CodexJournalRecordV2 {
	mutation := codexLeaseV2CASTestMutationRecord(record)
	mutation.State = LeaseBoundActive
	mutation.SocketLineageExtinct = false
	mutation.CodexCurrentRequest = CodexCurrentRequest{
		RequestKind:        CodexRequestTurn,
		RequestedModelHash: store.hash("requested-model", token),
		EffectiveModel:     "gpt-effective",
		RequiredBuckets:    []CapacityBucket{CapacityBucketBase},
		AttemptEnvelope: CodexAttemptEnvelope{
			PolicyVersion: CodexLeaseAttemptPolicyVersion,
			AttemptLimit:  1,
			Slots: []CodexAttemptSlot{{
				Index:         1,
				AccountHash:   record.AccountHash,
				CandidateHash: store.hash("candidate", token),
				Kind:          CodexAttemptSlotDirect,
			}},
		},
		RoutingRefs: 1,
		Attempts:    []CodexJournalAttempt{{Slot: 1, State: CodexAttemptPrepared}},
	}
	mutation.AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(store.key, mutation.AttemptEnvelope.Slots)
	return mutation
}

func codexLeaseV2CASTestLane(record CodexJournalRecordV2) *CodexJournalLane {
	return &CodexJournalLane{
		SessionHash:          record.SessionHash,
		ThreadHash:           record.ThreadHash,
		NamespaceHash:        record.NamespaceHash,
		CurrentTurnHash:      record.TurnHash,
		CurrentModeEpoch:     record.ModeEpoch,
		CurrentAuthoritative: record.Authoritative,
		LastTurnHash:         record.TurnHash,
		LastModeEpoch:        record.ModeEpoch,
		LastAuthoritative:    record.Authoritative,
	}
}

func codexLeaseV2CASTestMutationLane(lane CodexJournalLane) CodexJournalLane {
	lane.Generation = 0
	lane.LastObservedAt = time.Time{}
	lane.LastAdmittedAccountHash = ""
	lane.LastAdmittedTurnHash = ""
	lane.LastAdmittedModeEpoch = 0
	lane.LastAdmittedAuthoritative = false
	lane.LastAdmissionJournalGeneration = 0
	lane.LastAdmittedAt = time.Time{}
	lane.LastCacheAdmittedAt = time.Time{}
	lane.LastCacheEffectiveModel = ""
	return lane
}

func codexLeaseV2CASTestMutationRecord(record CodexJournalRecordV2) CodexJournalRecordV2 {
	record.RecordGeneration = 0
	record.LaneGeneration = 0
	record.PredecessorGeneration = 0
	record.LeaseGeneration = 0
	record.CreatedAt = time.Time{}
	record.LastObservedAt = time.Time{}
	record.EverAdmitted = false
	record.AdmissionJournalGeneration = 0
	record.AdmissionRequestGeneration = 0
	record.AdmissionRequestKind = ""
	record.AdmissionCompactionPhase = ""
	record.AdmittedAt = time.Time{}
	record.AttemptEnvelope.Slots = slices.Clone(record.AttemptEnvelope.Slots)
	record.RequiredBuckets = slices.Clone(record.RequiredBuckets)
	record.Attempts = slices.Clone(record.Attempts)
	for index := range record.Attempts {
		record.Attempts[index].Revision = 0
		record.Attempts[index].CreatedAt = time.Time{}
		record.Attempts[index].LastObservedAt = time.Time{}
	}
	return record
}

type failCodexLeaseV2PostSyncReadDirectory struct {
	fsutil.SecureDirectory
	journalName           string
	synced                bool
	journalReadsAfterSync int
}

func forwardCodexLeaseRenameChecked(directory fsutil.SecureDirectory, oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func forwardCodexLeaseRenameNoReplaceChecked(directory fsutil.SecureDirectory, oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func forwardCodexLeaseRemoveChecked(directory fsutil.SecureDirectory, name string, expected fsutil.SecureFileIdentity) error {
	return directory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *failCodexLeaseV2PostSyncReadDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *failCodexLeaseV2PostSyncReadDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameNoReplaceChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *failCodexLeaseV2PostSyncReadDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRemoveChecked(directory.SecureDirectory, name, expected)
}

type failCodexLeaseV2PreReplaceReadDirectory struct {
	fsutil.SecureDirectory
	journalName  string
	journalReads int
}

type countCodexLeaseV2JournalReadsDirectory struct {
	fsutil.SecureDirectory
	journalName  string
	journalReads int
}

func (directory *countCodexLeaseV2JournalReadsDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *countCodexLeaseV2JournalReadsDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameNoReplaceChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *countCodexLeaseV2JournalReadsDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRemoveChecked(directory.SecureDirectory, name, expected)
}

func (directory *countCodexLeaseV2JournalReadsDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if name == directory.journalName {
		directory.journalReads++
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func (directory *failCodexLeaseV2PreReplaceReadDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *failCodexLeaseV2PreReplaceReadDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRenameNoReplaceChecked(directory.SecureDirectory, oldName, newName, expected)
}

func (directory *failCodexLeaseV2PreReplaceReadDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return forwardCodexLeaseRemoveChecked(directory.SecureDirectory, name, expected)
}

func (directory *failCodexLeaseV2PreReplaceReadDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if name == directory.journalName {
		directory.journalReads++
		if directory.journalReads == 3 {
			return nil, errors.New("injected pre-replace journal trust loss")
		}
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func (directory *failCodexLeaseV2PostSyncReadDirectory) Sync() error {
	if err := directory.SecureDirectory.Sync(); err != nil {
		return err
	}
	directory.synced = true
	return nil
}

func (directory *failCodexLeaseV2PostSyncReadDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if directory.synced && name == directory.journalName {
		directory.journalReadsAfterSync++
		if directory.journalReadsAfterSync == 2 {
			return nil, errors.New("injected post-rename journal read failure")
		}
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func codexLeaseV2CASTestRecordFence(records []CodexJournalRecordV2, identity CodexJournalRecordIdentity) CodexLeaseRecordFence {
	for _, record := range records {
		if record.Identity() == identity {
			return CodexLeaseRecordFence{
				Record:            identity,
				Revision:          record.RecordGeneration,
				Lease:             record.LeaseGeneration,
				RequestGeneration: record.Generation,
				CurrentAttempt:    record.CurrentAttemptGeneration,
			}
		}
	}
	return CodexLeaseRecordFence{Record: identity}
}

func findCodexLeaseV2CASTestRecord(t *testing.T, records []CodexJournalRecordV2, identity CodexJournalRecordIdentity) CodexJournalRecordV2 {
	t.Helper()
	for _, record := range records {
		if record.Identity() == identity {
			return record
		}
	}
	t.Fatalf("record %#v not found", identity)
	return CodexJournalRecordV2{}
}

func findCodexLeaseV2CASTestLane(t *testing.T, lanes []CodexJournalLane, laneDigest string) CodexJournalLane {
	t.Helper()
	for _, lane := range lanes {
		if codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash) == laneDigest {
			return lane
		}
	}
	t.Fatalf("lane %q not found", laneDigest)
	return CodexJournalLane{}
}

func readCodexLeaseV2CASTestEnvelope(t *testing.T, fsys *fsutil.MemFS) codexLeaseJournalEnvelopeV2 {
	t.Helper()
	data, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope codexLeaseJournalEnvelopeV2
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func codexLeaseV2CASTestEnvelopePayload(t *testing.T, key []byte, envelope codexLeaseJournalEnvelopeV2) []byte {
	t.Helper()
	data, err := (&CodexLeaseStore{key: key}).marshalV2Envelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
