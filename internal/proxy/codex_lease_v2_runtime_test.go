package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseRuntimeHTTPAdmissionAndObserverDrain(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})

	prepared, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State() != LeaseProvisional || prepared.EverAdmitted() || prepared.AccountKey() != "account-a" || prepared.RequestGeneration() != 1 || prepared.AttemptGeneration() != 1 {
		t.Fatalf("prepared handle = state %v admitted %v account %q request %d attempt %d", prepared.State(), prepared.EverAdmitted(), prepared.AccountKey(), prepared.RequestGeneration(), prepared.AttemptGeneration())
	}
	assertCodexLeaseRuntimeRefs(t, prepared, 1, 0, 0, false)
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, dispatched, 1, 0, 0, false)
	observer, err := dispatched.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if !observer.EverAdmitted() || observer.State() != LeaseBoundActive {
		t.Fatalf("admitted observer = state %v admitted %v", observer.State(), observer.EverAdmitted())
	}
	assertCodexLeaseRuntimeRefs(t, observer, 1, 1, 1, false)
	completed, err := observer.ProviderCompleted(false)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State() != LeaseContinuationPending {
		t.Fatalf("completed state = %v, want continuation pending", completed.State())
	}
	assertCodexLeaseRuntimeRefs(t, completed, 0, 0, 1, false)

	next := plan
	next.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	beforeDrain := append([]byte(nil), store.journalBytes...)
	if _, err := runtime.BeginRequest(next); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("BeginRequest before observer drain = %T %v, want concurrent turn", err, err)
	}
	if !bytes.Equal(beforeDrain, store.journalBytes) {
		t.Fatal("blocked BeginRequest changed journal")
	}
	drained, err := completed.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if drained.State() != LeaseContinuationPending {
		t.Fatalf("drained state = %v", drained.State())
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)
	crossAccount := plan
	crossAccount.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-cross-account", Kind: CodexAttemptSlotDirect}}
	beforeCrossAccount := append([]byte(nil), store.journalBytes...)
	if _, err := runtime.BeginRequest(crossAccount); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("post-admission cross-account request = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(beforeCrossAccount, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("post-admission cross-account request changed authority: poison %v", store.poisoned)
	}
	second, err := runtime.BeginRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State() != LeaseBoundActive || !second.EverAdmitted() || second.AccountKey() != "account-a" || second.RequestGeneration() != 2 {
		t.Fatalf("second request = state %v admitted %v account %q generation %d", second.State(), second.EverAdmitted(), second.AccountKey(), second.RequestGeneration())
	}
	assertCodexLeaseRuntimeRefs(t, second, 1, 0, 0, false)
}

func TestCodexLeaseRuntimeAbandonsPreparedRequestAfterCancellation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	requestContext, cancelRequest := context.WithCancel(context.Background())
	prepared, err := runtimeLease.BeginRequestContext(requestContext, plan)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()

	cancelledContext, cancelCleanup := context.WithCancel(context.Background())
	cancelCleanup()
	beforeCancelled := append([]byte(nil), store.journalBytes...)
	beforeCancelledGeneration := store.Generation()
	if _, err := prepared.AbandonBeforeDispatchContext(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled abandonment = %T %v, want context canceled", err, err)
	}
	if store.Generation() != beforeCancelledGeneration || !bytes.Equal(beforeCancelled, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	abandoned, err := prepared.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State() != LeaseProvisional || abandoned.EverAdmitted() || codexLeaseCurrentAttemptState(abandoned.record) != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("abandoned request = %#v", abandoned.record)
	}
	assertCodexLeaseRuntimeRefs(t, abandoned, 0, 0, 0, true)

	beforeStale := append([]byte(nil), store.journalBytes...)
	beforeStaleGeneration := store.Generation()
	if _, err := prepared.AbandonBeforeDispatch(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale abandonment = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeStaleGeneration || !bytes.Equal(beforeStale, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	nextPlan := plan
	nextPlan.Accounts = []codex.AccountKey{"account-b"}
	nextPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	next, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	if next.RequestGeneration() != 2 || next.AccountKey() != "account-b" || next.State() != LeaseProvisional {
		t.Fatalf("request after abandonment = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeAbandonsLaterAdmittedRequest(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	admitted := completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)
	admissionJournalGeneration := admitted.record.AdmissionJournalGeneration
	admissionRequestGeneration := admitted.record.AdmissionRequestGeneration
	admittedAt := admitted.record.AdmittedAt

	nextPlan := plan
	nextPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}}
	prepared, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := prepared.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State() != LeaseOrphaned || !abandoned.EverAdmitted() || abandoned.AccountKey() != "account-a" || abandoned.RequestGeneration() != 2 || codexLeaseCurrentAttemptState(abandoned.record) != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("abandoned admitted request = %#v", abandoned.record)
	}
	if abandoned.record.AdmissionJournalGeneration != admissionJournalGeneration || abandoned.record.AdmissionRequestGeneration != admissionRequestGeneration || !abandoned.record.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("abandonment changed first-admission evidence: %#v", abandoned.record)
	}
	assertCodexLeaseRuntimeRefs(t, abandoned, 0, 0, 0, true)
	recovered, err := runtimeLease.BeginRequest(nextPlan)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RequestGeneration() != 3 || recovered.AccountKey() != "account-a" || !recovered.EverAdmitted() {
		t.Fatalf("request after admitted abandonment = %#v", recovered.record)
	}
}

func TestCodexLeaseRuntimeProviderFailureRetainsObserverUntilDrain(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	var reject atomic.Bool
	var revalidations atomic.Int32
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		revalidations.Add(1)
		if reject.Load() {
			return errors.New("account is no longer routable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	observer, err := handle.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(true)
	beforeRevalidations := revalidations.Load()
	failed, err := observer.ProviderFailed()
	if err != nil {
		t.Fatal(err)
	}
	if failed.State() != LeaseBoundQuiescent || !failed.EverAdmitted() || failed.AccountKey() != "account-a" || codexLeaseCurrentAttemptState(failed.record) != CodexAttemptProviderFailed {
		t.Fatalf("provider failure = %#v", failed.record)
	}
	if revalidations.Load() != beforeRevalidations {
		t.Fatalf("terminal provider failure revalidated account: %d -> %d", beforeRevalidations, revalidations.Load())
	}
	assertCodexLeaseRuntimeRefs(t, failed, 0, 0, 1, false)

	beforeStale := append([]byte(nil), store.journalBytes...)
	beforeStaleGeneration := store.Generation()
	if _, err := observer.ProviderFailed(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale provider failure = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeStaleGeneration || !bytes.Equal(beforeStale, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale provider failure changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	drained, err := failed.Drain()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)
}

func TestCodexLeaseRuntimeCreatesSuccessorAndNeverResurrectsPredecessor(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	firstPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}})
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, firstPlan)
	firstIdentity := first.identity

	secondPlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}})
	second, err := runtimeLease.BeginRequest(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	if second.State() != LeaseProvisional || second.EverAdmitted() || second.RequestGeneration() != 1 || second.AccountKey() != "account-a" {
		t.Fatalf("successor request = %#v", second.record)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, firstIdentity)
	if predecessor.State != LeaseSuperseded || predecessor.RoutingRefs != 0 || predecessor.AttemptRefs != 0 || predecessor.ResponseObserverRefs != 0 {
		t.Fatalf("superseded predecessor = %#v", predecessor)
	}
	if second.record.PredecessorTurnHash != firstIdentity.TurnDigest || second.record.PredecessorModeEpoch != firstIdentity.ModeEpoch || second.record.PredecessorAuthoritative != firstIdentity.Authoritative || second.record.PredecessorGeneration != predecessor.RecordGeneration {
		t.Fatalf("successor predecessor fence = %#v, want %#v generation %d", second.record, firstIdentity, predecessor.RecordGeneration)
	}
	if store.v2.Lanes[0].CurrentTurnHash != second.identity.TurnDigest || store.v2.Lanes[0].LastTurnHash != second.identity.TurnDigest {
		t.Fatalf("successor lane pointers = %#v", store.v2.Lanes[0])
	}
	for _, fence := range second.fence.TouchedRecords {
		if fence.Record == firstIdentity && len(fence.TouchedAttempts) != 0 {
			t.Fatalf("successor handle touched predecessor attempts: %#v", fence.TouchedAttempts)
		}
	}
	beforeHistorical := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(firstPlan); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("historical predecessor request = %T %v, want stale turn", err, err)
	}
	if !bytes.Equal(beforeHistorical, store.journalBytes) {
		t.Fatal("historical predecessor request changed journal")
	}

	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	failed, err := second.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if failed.State() != LeaseFailedUnadmitted {
		t.Fatalf("failed successor = %#v", failed.record)
	}
	if store.v2.Lanes[0].CurrentTurnHash != "" || store.v2.Lanes[0].LastTurnHash != failed.identity.TurnDigest {
		t.Fatalf("failed successor lane pointers = %#v", store.v2.Lanes[0])
	}
	thirdPlan := codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}})
	third, err := runtimeLease.BeginRequest(thirdPlan)
	if err != nil {
		t.Fatal(err)
	}
	if third.record.PredecessorTurnHash != failed.identity.TurnDigest || third.record.PredecessorTurnHash == firstIdentity.TurnDigest {
		t.Fatalf("third turn resurrected the wrong predecessor: %#v", third.record)
	}
}

func TestCodexLeaseRuntimeRejectsSuccessorUntilPredecessorDrains(t *testing.T) {
	tests := []struct {
		name    string
		advance func(*testing.T, *CodexLeaseRequestHandle) *CodexLeaseRequestHandle
	}{
		{name: "prepared", advance: func(_ *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle { return handle }},
		{name: "observer remains", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			next, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			next, err = next.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			next, err = next.ProviderCompleted(true)
			if err != nil {
				t.Fatal(err)
			}
			return next
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			first, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			_ = test.advance(t, first)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			_, err = runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
			if !errors.Is(err, ErrCodexConcurrentTurn) {
				t.Fatalf("successor before drain = %T %v, want concurrent turn", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("blocked successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeConcurrentSuccessorsHaveOneWinner(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	beforeGeneration := store.Generation()
	plans := []CodexLeaseRequestPlan{
		codexLeaseRuntimeTestPlan("turn-2a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2a", Kind: CodexAttemptSlotDirect}}),
		codexLeaseRuntimeTestPlan("turn-2b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2b", Kind: CodexAttemptSlotDirect}}),
	}
	start := make(chan struct{})
	results := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, len(plans))
	for _, plan := range plans {
		plan := plan
		go func() {
			<-start
			handle, err := runtimeLease.BeginRequest(plan)
			results <- struct {
				handle *CodexLeaseRequestHandle
				err    error
			}{handle: handle, err: err}
		}()
	}
	close(start)
	var winner *CodexLeaseRequestHandle
	for range plans {
		result := <-results
		if result.err == nil {
			if winner != nil {
				t.Fatal("both concurrent successors committed")
			}
			winner = result.handle
			continue
		}
		if !errors.Is(result.err, ErrCodexConcurrentTurn) && !errors.Is(result.err, ErrCodexLeaseStaleMutation) {
			t.Fatalf("losing successor error = %T %v", result.err, result.err)
		}
	}
	if winner == nil {
		t.Fatal("concurrent successors had no winner")
	}
	if store.Generation() != beforeGeneration+2 {
		t.Fatalf("concurrent successors advanced generation %d -> %d, want two commits", beforeGeneration, store.Generation())
	}
	if store.v2.Lanes[0].CurrentTurnHash != winner.identity.TurnDigest || store.v2.Lanes[0].LastTurnHash != winner.identity.TurnDigest {
		t.Fatalf("concurrent successor lane = %#v, winner %#v", store.v2.Lanes[0], winner.identity)
	}
}

func TestCodexLeaseRuntimeSuccessorAfterRestart(t *testing.T) {
	t.Parallel()
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
	second, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, first.identity)
	if predecessor.State != LeaseSuperseded || second.record.PredecessorTurnHash != first.identity.TurnDigest || second.record.PredecessorGeneration != predecessor.RecordGeneration {
		t.Fatalf("post-restart successor = predecessor %#v successor %#v", predecessor, second.record)
	}
}

func TestCodexLeaseRuntimeReservationCrashRequiresFreshSuccessor(t *testing.T) {
	t.Run("unseen reservation", func(t *testing.T) {
		coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		interrupted := codexLeaseRuntimeTestPlan("turn-interrupted", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-interrupted", Kind: CodexAttemptSlotDirect}})
		release, err := runtimeLease.beginAccountMutation(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := runtimeLease.store.LoadLane(interrupted.Key, interrupted.Accounts, interrupted.Authority)
		if err == nil {
			err = runtimeLease.reserveUnseen(restored)
		}
		release()
		if err != nil {
			t.Fatal(err)
		}
		interruptedIdentity := restored.RequestedIdentity
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
		reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
		reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
		if _, err := reopenedRuntime.BeginRequest(interrupted); !errors.Is(err, ErrCodexStaleTurn) {
			t.Fatalf("interrupted turn retry = %T %v, want stale turn", err, err)
		}
		fresh, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-fresh", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-fresh", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.record.PredecessorTurnHash != interruptedIdentity.TurnDigest {
			t.Fatalf("fresh successor skipped failed reservation: %#v", fresh.record)
		}
	})

	t.Run("successor reservation", func(t *testing.T) {
		coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
		interrupted := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}})
		release, err := runtimeLease.beginAccountMutation(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := runtimeLease.store.LoadLane(interrupted.Key, interrupted.Accounts, interrupted.Authority)
		if err == nil {
			err = runtimeLease.reserveSuccessor(context.Background(), "account-a", restored)
		}
		release()
		if err != nil {
			t.Fatal(err)
		}
		interruptedIdentity := restored.RequestedIdentity
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
		reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
		reopenedRuntime := newCodexLeaseRuntimeTest(t, reopened)
		if _, err := reopenedRuntime.BeginRequest(interrupted); !errors.Is(err, ErrCodexStaleTurn) {
			t.Fatalf("interrupted successor retry = %T %v, want stale turn", err, err)
		}
		fresh, err := reopenedRuntime.BeginRequest(codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.record.PredecessorTurnHash != interruptedIdentity.TurnDigest || fresh.record.PredecessorTurnHash == first.identity.TurnDigest {
			t.Fatalf("fresh successor resurrected pre-crash predecessor: %#v", fresh.record)
		}
	})
}

func TestCodexLeaseRuntimeSuccessorSurvivesPredecessorRetention(t *testing.T) {
	t.Parallel()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	second, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(25 * time.Hour)
	if err := store.Compact(time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	for _, record := range store.v2.Records {
		if record.Identity() == first.identity {
			t.Fatalf("retention kept collectible predecessor: %#v", record)
		}
	}
	dispatched, err := second.MarkDispatched()
	if err != nil {
		t.Fatalf("retained predecessor fence staled live successor: %v", err)
	}
	if dispatched.State() != LeaseProvisional || codexLeaseCurrentAttemptState(dispatched.record) != CodexAttemptDispatched || dispatched.record.PredecessorGeneration != second.record.PredecessorGeneration {
		t.Fatalf("successor after predecessor retention = %#v", dispatched.record)
	}
}

func TestCodexLeaseRuntimeRejectedSuccessorAuthorityDoesNotSupersede(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	reject := false
	rejected := errors.New("successor account unavailable")
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		if reject {
			return rejected
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	reject = true
	_, err = runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if !errors.Is(err, rejected) {
		t.Fatalf("rejected successor = %T %v, want callback error", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected successor changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.identity)
	if predecessor.State != LeaseBoundQuiescent || store.v2.Lanes[0].CurrentTurnHash != first.identity.TurnDigest {
		t.Fatalf("rejected successor superseded predecessor: %#v lane %#v", predecessor, store.v2.Lanes[0])
	}
}

func TestCodexLeaseRuntimeFinalRejectionAfterAdmissionPreservesAuthority(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})

	first, err := runtimeLease.BeginRequest(plan)
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
	first, err = first.ProviderCompleted(false)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.Drain()
	if err != nil {
		t.Fatal(err)
	}
	admissionGeneration := first.record.AdmissionJournalGeneration
	admissionRequest := first.record.AdmissionRequestGeneration
	admittedAt := first.record.AdmittedAt

	secondPlan := plan
	secondPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-second", Kind: CodexAttemptSlotDirect}}
	second, err := runtimeLease.BeginRequest(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := second.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State() != LeaseBoundQuiescent || !terminal.EverAdmitted() || terminal.AccountKey() != "account-a" || terminal.RequestGeneration() != 2 || codexLeaseCurrentAttemptState(terminal.record) != CodexAttemptProviderFailed {
		t.Fatalf("admitted final rejection = %#v", terminal.record)
	}
	if terminal.record.AdmissionJournalGeneration != admissionGeneration || terminal.record.AdmissionRequestGeneration != admissionRequest || !terminal.record.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("admission evidence changed after later rejection: %#v", terminal.record)
	}
	assertCodexLeaseRuntimeRefs(t, terminal, 0, 0, 0, true)

	crossAccount := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-cross", Kind: CodexAttemptSlotDirect}})
	crossAccount.Accounts = append(crossAccount.Accounts, "account-a")
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(crossAccount); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("cross-account request after admitted rejection = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cross-account request after rejection changed authority: poison %v", store.poisoned)
	}

	thirdPlan := plan
	thirdPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-third", Kind: CodexAttemptSlotDirect}}
	third, err := runtimeLease.BeginRequest(thirdPlan)
	if err != nil {
		t.Fatal(err)
	}
	if third.RequestGeneration() != 3 || third.State() != LeaseBoundActive || !third.EverAdmitted() || third.AccountKey() != "account-a" {
		t.Fatalf("request after admitted final rejection = %#v", third.record)
	}
}

func TestCodexLeaseRuntimeRejectsUnsupportedFirstRequestWithoutWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexLeaseRequestPlan)
		want   error
	}{
		{name: "raw prewarm", mutate: func(plan *CodexLeaseRequestPlan) { plan.RequestKind = CodexRequestPrewarm }, want: ErrCodexLeaseInvalidMutation},
		{name: "memory", mutate: func(plan *CodexLeaseRequestPlan) { plan.RequestKind = CodexRequestMemory }, want: ErrCodexLeaseInvalidMutation},
		{name: "mid-turn compaction before admission", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.RequestKind = CodexRequestCompaction
			plan.CompactionPhase = CodexCompactionMidTurn
		}, want: ErrCodexLeaseAuthorityMismatch},
		{name: "missing effective-model bucket", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.EffectiveModel = codexSparkModel
			plan.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
		}, want: ErrCodexLeaseInvalidMutation},
		{name: "untrimmed effective model", mutate: func(plan *CodexLeaseRequestPlan) {
			plan.EffectiveModel = " gpt-effective "
		}, want: ErrCodexLeaseInvalidMutation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			test.mutate(&plan)
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			if _, err := runtimeLease.BeginRequest(plan); !errors.Is(err, test.want) {
				t.Fatalf("BeginRequest error = %T %v, want %v", err, err, test.want)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected request changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeRequiresFreshAccountAuthorityBeforeWrites(t *testing.T) {
	if runtimeLease, err := NewCodexLeaseRuntime(nil, func(context.Context, codex.AccountKey) error { return nil }); runtimeLease != nil || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("nil coordinator constructor = (%#v, %T %v)", runtimeLease, err, err)
	}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	if runtimeLease, err := NewCodexLeaseRuntime(coordinator, nil); runtimeLease != nil || !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("nil revalidator constructor = (%#v, %T %v)", runtimeLease, err, err)
	}

	rejected := errors.New("account authority rejected")
	tests := []struct {
		name        string
		wantAccount codex.AccountKey
		prepare     func(*testing.T, *CodexLeaseRuntime) func() error
	}{
		{name: "begin request", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			return func() error {
				_, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
				return err
			}
		}},
		{name: "mark dispatched", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.MarkDispatched(); return err }
		}},
		{name: "prepare retry", wantAccount: "account-b", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
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
			return func() error { _, err := handle.RejectAndPrepare(2); return err }
		}},
		{name: "admit 2xx", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.AdmitHTTP2xx(); return err }
		}},
		{name: "pre-admission indeterminate", wantAccount: "account-a", prepare: func(t *testing.T, runtimeLease *CodexLeaseRuntime) func() error {
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := handle.Indeterminate(); return err }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			reject := false
			var revalidated codex.AccountKey
			runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(_ context.Context, account codex.AccountKey) error {
				revalidated = account
				if reject {
					return fmt.Errorf("removed account %s: %w", account, rejected)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			invoke := test.prepare(t, runtimeLease)
			revalidated = ""
			reject = true
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			err = invoke()
			if !errors.Is(err, rejected) {
				t.Fatalf("rejected operation error = %T %v, want callback error", err, err)
			}
			if strings.Contains(err.Error(), string(test.wantAccount)) {
				t.Fatalf("revalidation error exposed account identity: %v", err)
			}
			if revalidated != test.wantAccount {
				t.Fatalf("revalidated account = %q, want %q", revalidated, test.wantAccount)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected operation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseRuntimeTerminalEvidenceDoesNotRequireRoutability(t *testing.T) {
	rejected := errors.New("account is no longer routable")
	t.Run("final rejection", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		beforeCalls := calls.Load()
		reject.Store(true)
		terminal, err := handle.FinishRejected()
		if err != nil {
			t.Fatal(err)
		}
		if terminal.State() != LeaseFailedUnadmitted || calls.Load() != beforeCalls {
			t.Fatalf("final rejection = state %v callbacks %d -> %d", terminal.State(), beforeCalls, calls.Load())
		}
	})

	t.Run("provider completion and drain", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
		beforeCalls := calls.Load()
		reject.Store(true)
		handle, err = handle.ProviderCompleted(true)
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.Drain()
		if err != nil {
			t.Fatal(err)
		}
		if handle.State() != LeaseBoundQuiescent || calls.Load() != beforeCalls {
			t.Fatalf("terminal drain = state %v callbacks %d -> %d", handle.State(), beforeCalls, calls.Load())
		}
	})

	t.Run("admitted indeterminate", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		var reject atomic.Bool
		var calls atomic.Int32
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			calls.Add(1)
			if reject.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
		guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Release()
		if summary.BoundCount != 1 {
			t.Fatalf("admitted removal summary = %+v", summary)
		}
		beforeCalls := calls.Load()
		reject.Store(true)
		uncertain, err := handle.Indeterminate()
		if err != nil {
			t.Fatal(err)
		}
		if uncertain.State() != LeaseOrphaned || calls.Load() != beforeCalls {
			t.Fatalf("admitted indeterminate = state %v callbacks %d -> %d", uncertain.State(), beforeCalls, calls.Load())
		}
	})
}

func TestCodexLeaseRuntimeMidTurnCompactionRequiresAdmittedTurn(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	request, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.ProviderCompleted(false)
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.Drain()
	if err != nil {
		t.Fatal(err)
	}

	compaction := plan
	compaction.RequestKind = CodexRequestCompaction
	compaction.CompactionPhase = CodexCompactionMidTurn
	compaction.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-compaction", Kind: CodexAttemptSlotDirect}}
	next, err := runtimeLease.BeginRequest(compaction)
	if err != nil {
		t.Fatal(err)
	}
	if next.RequestGeneration() != 2 || next.State() != LeaseBoundActive || !next.EverAdmitted() || next.record.RequestKind != CodexRequestCompaction || next.record.CompactionPhase != CodexCompactionMidTurn {
		t.Fatalf("mid-turn compaction request = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeAdmissionUsesAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("pre-admission removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := dispatched.AdmitHTTP2xxContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gated admission error = %T %v, want deadline", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("blocked admission changed authority: poison %v", store.poisoned)
	}
	guard.Release()
	admitted, err := dispatched.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.EverAdmitted() || admitted.State() != LeaseBoundActive {
		t.Fatalf("released admission = %#v", admitted.record)
	}
}

func TestCodexLeaseRuntimeBeginAndDispatchUseAccountRemovalGate(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
		if _, err := runtimeLease.BeginRequestContext(ctx, plan); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gated begin = %T %v, want deadline", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) {
			t.Fatal("gated begin changed journal")
		}
		guard.Release()
		if _, err := runtimeLease.BeginRequest(plan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("dispatch", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := handle.MarkDispatchedContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gated dispatch = %T %v, want deadline", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) {
			t.Fatal("gated dispatch changed journal")
		}
		guard.Release()
		if _, err := handle.MarkDispatched(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCodexLeaseRuntimeCancellationReleasesGateWhilePersistenceBusy(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	coordinator.leases.lifecycle.persistence.Lock()
	locked := true
	defer func() {
		if locked {
			coordinator.leases.lifecycle.persistence.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runtimeLease.BeginRequestContext(ctx, codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		result <- err
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 1)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled persistence waiter = %T %v, want context canceled", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled persistence waiter retained the account gate")
	}
	coordinator.leases.accountGates.mu.Lock()
	_, retained := coordinator.leases.accountGates.entries["account-a"]
	coordinator.leases.accountGates.mu.Unlock()
	if retained {
		t.Fatal("cancelled persistence waiter retained its account identity")
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled persistence waiter changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	coordinator.leases.lifecycle.persistence.Unlock()
	locked = false
}

func TestCodexLeaseRuntimeAdmittedIndeterminateHonoursCancellationWhilePersistenceBusy(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
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
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	coordinator.leases.lifecycle.persistence.Lock()
	locked := true
	defer func() {
		if locked {
			coordinator.leases.lifecycle.persistence.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := handle.IndeterminateContext(ctx)
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admitted indeterminate = %T %v, want context canceled", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted indeterminate ignored cancellation while persistence was busy")
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cancelled admitted indeterminate changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
	coordinator.leases.lifecycle.persistence.Unlock()
	locked = false
}

func TestCodexLeaseRuntimeRevalidatesAfterWaitingForRemoval(t *testing.T) {
	rejected := errors.New("account removal completed")
	t.Run("admission", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		var removed atomic.Bool
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
			if removed.Load() {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		result := make(chan error, 1)
		go func() { _, transitionErr := handle.AdmitHTTP2xx(); result <- transitionErr }()
		waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 2)
		removed.Store(true)
		guard.Release()
		if err := <-result; !errors.Is(err, rejected) {
			t.Fatalf("admission after removal = %T %v, want callback error", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
			t.Fatalf("rejected admission changed authority: poison %v", store.poisoned)
		}
	})

	t.Run("retry", func(t *testing.T) {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		store := coordinator.Store()
		var removed atomic.Bool
		runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(_ context.Context, account codex.AccountKey) error {
			if removed.Load() && account == "account-b" {
				return rejected
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
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
		guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), store.journalBytes...)
		result := make(chan error, 1)
		go func() { _, transitionErr := handle.RejectAndPrepare(2); result <- transitionErr }()
		waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
		removed.Store(true)
		guard.Release()
		if err := <-result; !errors.Is(err, rejected) {
			t.Fatalf("retry after removal = %T %v, want callback error", err, err)
		}
		if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
			t.Fatalf("rejected retry changed authority: poison %v", store.poisoned)
		}
	})
}

func TestCodexLeaseRuntimeDetachesPlanAndPersistsNoRawIdentity(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("private-turn-runtime", []CodexLeaseAttemptSlotPlan{{
		AccountKey:  "private-account-runtime",
		CandidateID: "private-candidate-runtime",
		Kind:        CodexAttemptSlotDirect,
	}})
	plan.Key.Lane.Session = "private-session-runtime"
	plan.Key.Lane.Thread = "private-thread-runtime"
	plan.RequestedModel = "private-requested-model-runtime"
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Accounts[0] = "mutated-account"
	plan.RequiredBuckets[0] = CapacityBucket("mutated-bucket")
	plan.Slots[0].AccountKey = "mutated-slot-account"
	plan.Slots[0].CandidateID = "mutated-candidate"
	if prepared.AccountKey() != "private-account-runtime" || prepared.slotAccounts[0] != "private-account-runtime" || prepared.record.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("runtime plan aliases caller memory: account %q slots %#v buckets %#v", prepared.AccountKey(), prepared.slotAccounts, prepared.record.RequiredBuckets)
	}
	for _, raw := range []string{
		"private-session-runtime",
		"private-thread-runtime",
		"private-turn-runtime",
		"private-account-runtime",
		"private-candidate-runtime",
		"private-requested-model-runtime",
	} {
		if bytes.Contains(store.journalBytes, []byte(raw)) {
			t.Fatalf("journal persisted raw private value %q", raw)
		}
	}
}

func TestCodexLeaseV2CASNarrowsProvisionalUncertainty(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2)
		valid  bool
	}{
		{name: "exact dispatched after-image", valid: true},
		{name: "routing ref remains", mutate: func(record *CodexJournalRecordV2) { record.RoutingRefs = 1 }},
		{name: "attempt ref remains", mutate: func(record *CodexJournalRecordV2) { record.AttemptRefs = 1 }},
		{name: "observer ownership missing", mutate: func(record *CodexJournalRecordV2) { record.ResponseObserverRefs = 0 }},
		{name: "lineage prematurely extinct", mutate: func(record *CodexJournalRecordV2) { record.SocketLineageExtinct = true }},
		{name: "attempt is definitely failed", mutate: func(record *CodexJournalRecordV2) { record.Attempts[0].State = CodexAttemptProviderFailed }},
		{name: "account changes", mutate: func(record *CodexJournalRecordV2) { record.AccountHash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }},
		{name: "encrypted state appears", mutate: func(record *CodexJournalRecordV2) { record.HasEncryptedState = true }},
		{name: "socket generation changes", mutate: func(record *CodexJournalRecordV2) { record.UpstreamSocketGeneration++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			prepared, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			dispatched, err := prepared.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			desired := codexLeaseRuntimeMutationRecord(dispatched.record)
			desired.State = LeaseOrphaned
			desired.RoutingRefs = 0
			desired.AttemptRefs = 0
			desired.ResponseObserverRefs = 1
			desired.Attempts[0].State = CodexAttemptIndeterminate
			if test.mutate != nil {
				test.mutate(&desired)
			}
			before := append([]byte(nil), store.journalBytes...)
			beforeGeneration := store.Generation()
			_, err = store.CommitLane(dispatched.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}})
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, dispatched.identity)
				if stored.State != LeaseOrphaned || !stored.NonMigratable || codexLeaseCurrentAttemptState(stored) != CodexAttemptIndeterminate {
					t.Fatalf("exact uncertainty = %#v", stored)
				}
				return
			}
			if !errors.Is(err, ErrCodexLeaseInvalidMutation) {
				t.Fatalf("narrow uncertainty error = %T %v, want invalid mutation", err, err)
			}
			if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
				t.Fatalf("rejected uncertainty changed authority: generation %d poison %v", store.Generation(), store.poisoned)
			}
		})
	}
}

func TestCodexLeaseV2CASRejectsUncertaintyWithHistoricalAttemptFence(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
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
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	desired.State = LeaseOrphaned
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 1
	desired.Attempts[1].State = CodexAttemptIndeterminate
	fence := cloneCodexLeaseGenerationFence(handle.fence)
	recordFence, ok := codexLeaseRuntimeRecordFence(&fence, handle.identity)
	if !ok {
		t.Fatal("current record fence is absent")
	}
	recordFence.TouchedAttempts = append(recordFence.TouchedAttempts, CodexAttemptFence{
		RequestGeneration: handle.record.Generation,
		Generation:        handle.record.Attempts[0].Generation,
		Revision:          handle.record.Attempts[0].Revision,
	})
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("uncertainty with historical attempt fence = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected uncertainty changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CASRejectsAbandonmentThatRegressesCurrentAttempt(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
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
	handle, err = handle.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	desired.Attempts[1].State = CodexAttemptAbandonedBeforeDispatch
	desired.CurrentAttemptGeneration = desired.Attempts[0].Generation
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(handle.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) || !strings.Contains(err.Error(), "prepared abandonment lacks an exact terminal after-image") {
		t.Fatalf("regressed abandonment = %T %v, want exact abandonment rejection", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("rejected abandonment changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseV2CASRequiresSoleNarrowTerminalMutation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	first := completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect}}))
	current, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	current, err = current.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	desired := codexLeaseRuntimeMutationRecord(current.record)
	desired.State = LeaseOrphaned
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 1
	desired.Attempts[0].State = CodexAttemptIndeterminate
	predecessor := findCodexLeaseV2CASTestRecord(t, store.v2.Records, first.identity)
	predecessorNoOp := codexLeaseRuntimeMutationRecord(predecessor)
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := store.CommitLane(current.fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{predecessorNoOp, desired}}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("bundled narrow terminal mutation = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("bundled narrow mutation changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseRuntimeStaleRequestFenceDoesNotWrite(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	prepared, err := runtime.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	if err != nil {
		t.Fatal(err)
	}
	stale := prepared
	if _, err := prepared.MarkDispatched(); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	if _, err := stale.MarkDispatched(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale request fence error = %T %v, want stale mutation", err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale request fence changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}
}

func TestCodexLeaseRuntimeUnrelatedLaneCommitDoesNotStaleHandle(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	planA := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	preparedA, err := runtimeLease.BeginRequest(planA)
	if err != nil {
		t.Fatal(err)
	}
	planB := codexLeaseRuntimeTestPlan("turn-b", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}})
	planB.Key.Lane.Thread = "runtime-thread-b"
	if _, err := runtimeLease.BeginRequest(planB); err != nil {
		t.Fatal(err)
	}
	dispatchedA, err := preparedA.MarkDispatched()
	if err != nil {
		t.Fatalf("unrelated lane commit staled handle: %v", err)
	}
	if dispatchedA.record.Attempts[0].State != CodexAttemptDispatched {
		t.Fatalf("dispatched attempt = %#v", dispatchedA.record.Attempts[0])
	}
}

func TestCodexLeaseRuntimeRejectAndPrepareUsesNextAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	beforeReused := append([]byte(nil), store.journalBytes...)
	beforeReusedGeneration := store.Generation()
	if _, err := dispatched.RejectAndPrepare(1); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("reused-slot retry error = %T %v, want invalid mutation", err, err)
	}
	if store.Generation() != beforeReusedGeneration || !bytes.Equal(beforeReused, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("reused-slot retry changed authority: generation %d poison %v", store.Generation(), store.poisoned)
	}

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("unadmitted removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := dispatched.RejectAndPrepareContext(ctx, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gated retry error = %T %v, want deadline", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("blocked retry changed authority: poison %v", store.poisoned)
	}
	guard.Release()

	retry, err := dispatched.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AccountKey() != "account-b" || retry.AttemptGeneration() != 2 || retry.record.Attempts[0].State != CodexAttemptProviderFailed || retry.record.Attempts[1].State != CodexAttemptPrepared || retry.EverAdmitted() || retry.record.NonMigratable {
		t.Fatalf("retry handle = account %q request %#v", retry.AccountKey(), retry.record.CodexCurrentRequest)
	}
	assertCodexLeaseRuntimeRefs(t, retry, 1, 0, 0, false)

	before = append([]byte(nil), store.journalBytes...)
	if _, err := retry.RejectAndPrepare(0); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("zero-slot retry error = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("zero-slot retry changed journal")
	}
	retryDispatched, err := retry.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := retryDispatched.FinishRejected()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State() != LeaseFailedUnadmitted || terminal.EverAdmitted() || !terminal.record.SocketLineageExtinct || store.v2.Lanes[0].CurrentTurnHash != "" {
		t.Fatalf("terminal rejection = state %v record %#v lane %#v", terminal.State(), terminal.record, store.v2.Lanes[0])
	}

	before = append([]byte(nil), store.journalBytes...)
	if _, err := dispatched.Indeterminate(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale-head callback error = %T %v, want stale mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("stale-head callback changed authority: poison %v", store.poisoned)
	}
}

func TestCodexLeaseRuntimeRetrySerialisesWithConcurrentAccountRemoval(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	result := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, 1)
	go func() {
		handle, retryErr := dispatched.RejectAndPrepare(2)
		result <- struct {
			handle *CodexLeaseRequestHandle
			err    error
		}{handle: handle, err: retryErr}
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
	select {
	case outcome := <-result:
		t.Fatalf("retry escaped held removal gate: handle %#v error %v", outcome.handle, outcome.err)
	default:
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked concurrent retry changed journal")
	}
	guard.Release()
	outcome := <-result
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.handle.AccountKey() != "account-b" || outcome.handle.AttemptGeneration() != 2 {
		t.Fatalf("released concurrent retry = %#v", outcome.handle.record)
	}
}

func TestCodexLeaseRuntimeCloseWakesRetryBehindRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	before := fsysFileBytes(t, fsys, "/state/leases.json")
	result := make(chan error, 1)
	go func() {
		_, retryErr := dispatched.RejectAndPrepare(2)
		result <- retryErr
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-b", 2)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
			t.Fatalf("retry after Close = %T %v, want writer unavailable", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry remained blocked after Close")
	}
	if !bytes.Equal(before, fsysFileBytes(t, fsys, "/state/leases.json")) {
		t.Fatal("Close-woken retry changed durable journal")
	}
	coordinator.leases.accountGates.mu.Lock()
	remaining := len(coordinator.leases.accountGates.entries)
	coordinator.leases.accountGates.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("closed account gate set retained %d identities", remaining)
	}
	guard.Release()
}

func TestCodexLeaseRuntimeIndeterminateUsesCurrentAccountRemovalGate(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if summary.BoundCount != 0 {
		t.Fatalf("pre-admission removal summary = %+v", summary)
	}
	before := append([]byte(nil), store.journalBytes...)
	result := make(chan struct {
		handle *CodexLeaseRequestHandle
		err    error
	}, 1)
	go func() {
		handle, transitionErr := dispatched.Indeterminate()
		result <- struct {
			handle *CodexLeaseRequestHandle
			err    error
		}{handle: handle, err: transitionErr}
	}()
	waitCodexLeaseRuntimeGateRefs(t, coordinator.leases.accountGates, "account-a", 2)
	select {
	case outcome := <-result:
		t.Fatalf("indeterminate escaped held removal gate: handle %#v error %v", outcome.handle, outcome.err)
	default:
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked indeterminate changed journal")
	}
	guard.Release()
	outcome := <-result
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.handle.State() != LeaseOrphaned || !outcome.handle.record.NonMigratable {
		t.Fatalf("released indeterminate = %#v", outcome.handle.record)
	}
}

func TestCodexLeaseRuntimeIndeterminatePinsAccountAndDrains(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	prepared, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := prepared.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	uncertain, err := dispatched.Indeterminate()
	if err != nil {
		t.Fatal(err)
	}
	if uncertain.State() != LeaseOrphaned || uncertain.EverAdmitted() || !uncertain.record.NonMigratable || uncertain.record.Attempts[0].State != CodexAttemptIndeterminate {
		t.Fatalf("uncertain request = %#v", uncertain.record)
	}
	assertCodexLeaseRuntimeRefs(t, uncertain, 0, 0, 1, false)
	before := append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(plan); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("request before uncertain drain = %T %v, want concurrent", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("blocked uncertain request changed journal")
	}
	drained, err := uncertain.Drain()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexLeaseRuntimeRefs(t, drained, 0, 0, 0, true)

	other := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b-next", Kind: CodexAttemptSlotDirect}})
	other.Accounts = append(other.Accounts, "account-a")
	before = append([]byte(nil), store.journalBytes...)
	if _, err := runtimeLease.BeginRequest(other); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("cross-account uncertain request = %T %v, want invalid mutation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) || store.poisoned != nil {
		t.Fatalf("cross-account uncertain request changed authority: poison %v", store.poisoned)
	}
	same := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a-next", Kind: CodexAttemptSlotDirect}})
	next, err := runtimeLease.BeginRequest(same)
	if err != nil {
		t.Fatal(err)
	}
	if next.AccountKey() != "account-a" || next.RequestGeneration() != 2 || next.State() != LeaseProvisional || !next.record.NonMigratable {
		t.Fatalf("same-account uncertain recovery = %#v", next.record)
	}
}

func TestCodexLeaseRuntimeRestartNormalisesAndFencesOldHandles(t *testing.T) {
	tests := []struct {
		name           string
		advance        func(*testing.T, *CodexLeaseRequestHandle) *CodexLeaseRequestHandle
		wantLease      LeaseState
		wantAttempt    CodexAttemptState
		wantAdmitted   bool
		wantPinned     bool
		requestAccount codex.AccountKey
	}{
		{name: "prepared", advance: func(_ *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle { return handle }, wantLease: LeaseProvisional, wantAttempt: CodexAttemptAbandonedBeforeDispatch, requestAccount: "account-b"},
		{name: "dispatched", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			next, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			return next
		}, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantPinned: true, requestAccount: "account-a"},
		{name: "streaming", advance: func(t *testing.T, handle *CodexLeaseRequestHandle) *CodexLeaseRequestHandle {
			t.Helper()
			dispatched, err := handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			next, err := dispatched.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			return next
		}, wantLease: LeaseOrphaned, wantAttempt: CodexAttemptIndeterminate, wantAdmitted: true, wantPinned: true, requestAccount: "account-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
				{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
				{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
			})
			prepared, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			oldHandle := test.advance(t, prepared)
			identity := oldHandle.identity
			beforeClose := append([]byte(nil), coordinator.Store().journalBytes...)
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := oldHandle.Indeterminate(); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
				t.Fatalf("old handle after Close = %T %v, want writer unavailable", err, err)
			}
			if !bytes.Equal(beforeClose, fsysFileBytes(t, fsys, "/state/leases.json")) {
				t.Fatal("closed old handle changed journal")
			}
			*now = now.Add(time.Minute)
			reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
			restored := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
			if restored.State != test.wantLease || codexLeaseCurrentAttemptState(restored) != test.wantAttempt || restored.EverAdmitted != test.wantAdmitted || restored.NonMigratable != test.wantPinned || !restored.SocketLineageExtinct || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.ResponseObserverRefs != 0 {
				t.Fatalf("restart state = %#v", restored)
			}
			nextPlan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: test.requestAccount, CandidateID: "candidate-next", Kind: CodexAttemptSlotDirect}})
			next, err := newCodexLeaseRuntimeTest(t, reopened).BeginRequest(nextPlan)
			if err != nil {
				t.Fatal(err)
			}
			if next.AccountKey() != test.requestAccount || next.RequestGeneration() != 2 || next.EverAdmitted() != test.wantAdmitted {
				t.Fatalf("post-restart request = %#v", next.record)
			}
		})
	}
}

func codexLeaseRuntimeTestPlan(turn string, slots []CodexLeaseAttemptSlotPlan) CodexLeaseRequestPlan {
	accounts := make([]codex.AccountKey, 0, len(slots))
	seen := make(map[codex.AccountKey]struct{}, len(slots))
	for _, slot := range slots {
		if _, ok := seen[slot.AccountKey]; ok {
			continue
		}
		seen[slot.AccountKey] = struct{}{}
		accounts = append(accounts, slot.AccountKey)
	}
	return CodexLeaseRequestPlan{
		Key: LeaseKey{
			Lane: LaneKey{Session: "runtime-session", Thread: "runtime-thread", Namespace: CodexResponsesNamespace},
			Turn: turn,
		},
		Accounts:       accounts,
		Authority:      CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		RequestKind:    CodexRequestTurn,
		RequestedModel: "requested-model-private",
		EffectiveModel: "gpt-effective",
		RequiredBuckets: []CapacityBucket{
			CapacityBucketBase,
		},
		Slots:       slots,
		InitialSlot: 1,
	}
}

func newCodexLeaseRuntimeTest(t *testing.T, coordinator *CodexContinuityCoordinator) *CodexLeaseRuntime {
	t.Helper()
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return runtimeLease
}

func completeCodexLeaseRuntimeTurn(t *testing.T, runtimeLease *CodexLeaseRuntime, plan CodexLeaseRequestPlan) *CodexLeaseRequestHandle {
	t.Helper()
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
	handle, err = handle.ProviderCompleted(true)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func assertCodexLeaseRuntimeRefs(t *testing.T, handle *CodexLeaseRequestHandle, routing, attempts, observers int, extinct bool) {
	t.Helper()
	if handle.record.RoutingRefs != routing || handle.record.AttemptRefs != attempts || handle.record.ResponseObserverRefs != observers || handle.record.SocketLineageExtinct != extinct {
		t.Fatalf("runtime refs = routing %d attempts %d observers %d extinct %v, want %d/%d/%d/%v", handle.record.RoutingRefs, handle.record.AttemptRefs, handle.record.ResponseObserverRefs, handle.record.SocketLineageExtinct, routing, attempts, observers, extinct)
	}
}

func openCodexLeaseRuntimeTestCoordinator(t *testing.T) (*CodexContinuityCoordinator, *fsutil.MemFS, *time.Time) {
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
		Version:     codexLeaseJournalVersionV2,
		HashVersion: codexLeaseHashVersion,
		Generation:  1,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   3,
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
	return coordinator, fsys, &now
}

func reopenCodexLeaseRuntimeTestCoordinator(t *testing.T, fsys *fsutil.MemFS, now *time.Time) *CodexContinuityCoordinator {
	t.Helper()
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
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
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}

func fsysFileBytes(t *testing.T, fsys *fsutil.MemFS, path string) []byte {
	t.Helper()
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func waitCodexLeaseRuntimeGateRefs(t *testing.T, gates *codexAccountGateSet, account codex.AccountKey, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gates.mu.Lock()
		entry := gates.entries[account]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		gates.mu.Unlock()
		if refs == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("account gate %q did not reach %d references", account, want)
}
