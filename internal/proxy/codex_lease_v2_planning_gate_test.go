package proxy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexLeaseRuntimePlanningGateCancelledWaiterReleasesReference(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})

	releaseOwner, err := runtimeLease.AcquireRequestPlanningContext(context.Background(), plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOwner()

	type acquisition struct {
		release func()
		err     error
	}
	result := make(chan acquisition, 1)
	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	go func() {
		release, acquireErr := runtimeLease.AcquireRequestPlanningContext(waiterContext, plan.Key, plan.Accounts, plan.Authority)
		result <- acquisition{release: release, err: acquireErr}
	}()

	waitCodexLeasePlanningGateState(t, runtimeLease, plan.Key.Lane, 2, true)
	cancelWaiter()

	select {
	case got := <-result:
		if got.release != nil {
			got.release()
			t.Fatal("cancelled waiter acquired the planning gate")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancelled waiter error = %T %v, want context cancellation", got.err, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled planning waiter did not return")
	}

	waitCodexLeasePlanningGateState(t, runtimeLease, plan.Key.Lane, 1, true)
	releaseOwner()
	waitCodexLeasePlanningGateState(t, runtimeLease, plan.Key.Lane, 0, false)

	reacquireContext, cancelReacquire := context.WithTimeout(context.Background(), time.Second)
	defer cancelReacquire()
	release, err := runtimeLease.AcquireRequestPlanningContext(reacquireContext, plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatalf("planning gate after cancelled waiter = %T %v", err, err)
	}
	release()
	waitCodexLeasePlanningGateState(t, runtimeLease, plan.Key.Lane, 0, false)
}

func TestCodexLeaseRuntimePlanningGateQueuesSuccessorBeginsSerially(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect,
	}}))
	successorPlans := []CodexLeaseRequestPlan{
		codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect}}),
		codexLeaseRuntimeTestPlan("turn-3", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-3", Kind: CodexAttemptSlotDirect}}),
	}
	acquireKey := successorPlans[0].Key

	testContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releaseOwner, err := runtimeLease.AcquireRequestPlanningContext(testContext, acquireKey, successorPlans[0].Accounts, successorPlans[0].Authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOwner()

	type beginResult struct {
		order    int32
		identity CodexJournalRecordIdentity
		err      error
	}
	results := make(chan beginResult, 2)
	firstBegan := make(chan struct{})
	allowFirstDrain := make(chan struct{})
	var beginOrder atomic.Int32

	worker := func() {
		release, acquireErr := runtimeLease.AcquireRequestPlanningContext(testContext, acquireKey, successorPlans[0].Accounts, successorPlans[0].Authority)
		if acquireErr != nil {
			results <- beginResult{err: acquireErr}
			return
		}
		order := beginOrder.Add(1)
		if order < 1 || order > int32(len(successorPlans)) {
			release()
			results <- beginResult{order: order, err: fmt.Errorf("unexpected begin order %d", order)}
			return
		}
		handle, beginErr := runtimeLease.BeginRequestContext(testContext, successorPlans[order-1])
		release()
		if beginErr != nil {
			results <- beginResult{order: order, err: beginErr}
			return
		}
		identity := handle.identity
		if order == 1 {
			close(firstBegan)
			select {
			case <-allowFirstDrain:
			case <-testContext.Done():
				results <- beginResult{order: order, identity: identity, err: testContext.Err()}
				return
			}
		}
		if completeErr := completeCodexLeasePlanningGateRequest(handle); completeErr != nil {
			results <- beginResult{order: order, identity: identity, err: completeErr}
			return
		}
		results <- beginResult{order: order, identity: identity}
	}
	go worker()
	go worker()

	waitCodexLeasePlanningGateState(t, runtimeLease, acquireKey.Lane, 3, true)
	releaseOwner()

	select {
	case <-firstBegan:
	case got := <-results:
		t.Fatalf("queued successor failed before first Begin completed: order %d error %v", got.order, got.err)
	case <-testContext.Done():
		t.Fatal("first queued successor did not begin")
	}
	waitCodexLeasePlanningGateState(t, runtimeLease, acquireKey.Lane, 1, true)
	if got := beginOrder.Load(); got != 1 {
		t.Fatalf("successor begins before first drain = %d, want 1", got)
	}
	close(allowFirstDrain)

	byOrder := make(map[int32]beginResult, len(successorPlans))
	for len(byOrder) < len(successorPlans) {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("queued successor %d = %T %v", got.order, got.err, got.err)
			}
			if _, duplicate := byOrder[got.order]; duplicate {
				t.Fatalf("duplicate successor order %d", got.order)
			}
			byOrder[got.order] = got
		case <-testContext.Done():
			t.Fatalf("queued successor begins deadlocked after %d completions", len(byOrder))
		}
	}
	if byOrder[1].identity == (CodexJournalRecordIdentity{}) || byOrder[2].identity == (CodexJournalRecordIdentity{}) || byOrder[1].identity == byOrder[2].identity {
		t.Fatalf("successor identities = %#v and %#v, want distinct durable requests", byOrder[1].identity, byOrder[2].identity)
	}
	waitCodexLeasePlanningGateState(t, runtimeLease, acquireKey.Lane, 0, false)
}

func TestCodexLeaseRuntimePlanningGateAllowsSuccessorAfterAbandonedPredecessor(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	predecessorPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect,
	}})
	predecessor, err := runtimeLease.BeginRequest(predecessorPlan)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.AbandonBeforeDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if predecessor.State() != LeaseProvisional || predecessor.EverAdmitted() ||
		codexLeaseCurrentAttemptState(predecessor.record) != CodexAttemptAbandonedBeforeDispatch ||
		predecessor.record.RoutingRefs != 0 || predecessor.record.AttemptRefs != 0 ||
		predecessor.record.ResponseObserverRefs != 0 || !predecessor.record.SocketLineageExtinct {
		t.Fatalf("abandoned predecessor = %#v", predecessor.record)
	}

	successorPlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	release, err := runtimeLease.AcquireRequestPlanningContext(ctx, successorPlan.Key, successorPlan.Accounts, successorPlan.Authority)
	if err != nil {
		t.Fatalf("successor planning after abandoned predecessor = %T %v", err, err)
	}
	defer release()

	successor, err := runtimeLease.BeginRequestContext(ctx, successorPlan)
	if err != nil {
		t.Fatalf("successor after abandoned predecessor = %T %v", err, err)
	}
	if successor.record.PredecessorTurnHash != predecessor.identity.TurnDigest {
		t.Fatalf("successor predecessor = %q, want %q", successor.record.PredecessorTurnHash, predecessor.identity.TurnDigest)
	}
}

func TestCodexLeaseRuntimePlanningGateTracesBlockedPredecessorState(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	predecessorPlan := codexLeaseRuntimeTestPlan("turn-1", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-1", Kind: CodexAttemptSlotDirect,
	}})
	if _, err := runtimeLease.BeginRequest(predecessorPlan); err != nil {
		t.Fatal(err)
	}
	successorPlan := codexLeaseRuntimeTestPlan("turn-2", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-2", Kind: CodexAttemptSlotDirect,
	}})

	tracePath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), diagnostics, nil, CodexTraceStart{Transport: "websocket"})
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := runtimeLease.AcquireRequestPlanningContext(ctx, successorPlan.Key, successorPlan.Accounts, successorPlan.Authority); !errors.Is(err, context.DeadlineExceeded) {
		_ = diagnostics.Close()
		t.Fatalf("blocked successor planning = %T %v, want deadline", err, err)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}

	events := readCodexTraceEvents(t, tracePath)
	requireCodexTraceEvent(t, events, func(event CodexTraceEvent) bool {
		return event.Phase == "planning_gate" && event.Outcome == "acquired"
	}, "planning gate acquisition")
	requireCodexTraceEvent(t, events, func(event CodexTraceEvent) bool {
		return event.Phase == "planning_wait" && event.Outcome == "blocked" &&
			strings.Contains(event.Reason, "classification=unseen state=provisional attempt=prepared")
	}, "blocked durable predecessor state")
	requireCodexTraceEvent(t, events, func(event CodexTraceEvent) bool {
		return event.Phase == "planning_wait" && event.Outcome == "error" && event.Reason == "deadline_exceeded"
	}, "planning wait deadline")
	blocked := 0
	for _, event := range events {
		if event.Phase == "planning_wait" && event.Outcome == "blocked" {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("blocked planning trace events = %d, want 1", blocked)
	}
}

func completeCodexLeasePlanningGateRequest(handle *CodexLeaseRequestHandle) error {
	var err error
	handle, err = handle.MarkDispatched()
	if err != nil {
		return fmt.Errorf("mark dispatched: %w", err)
	}
	handle, err = handle.AdmitHTTP2xx()
	if err != nil {
		return fmt.Errorf("admit HTTP 2xx: %w", err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		return fmt.Errorf("provider completed: %w", err)
	}
	if _, err = handle.Drain(); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return nil
}

func waitCodexLeasePlanningGateState(t *testing.T, runtimeLease *CodexLeaseRuntime, lane LaneKey, wantRefs int, wantHeld bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		refs, held := codexLeasePlanningGateState(runtimeLease, lane)
		if refs == wantRefs && held == wantHeld {
			return
		}
		runtime.Gosched()
	}
	refs, held := codexLeasePlanningGateState(runtimeLease, lane)
	t.Fatalf("planning gate state = refs %d held %v, want refs %d held %v", refs, held, wantRefs, wantHeld)
}

func codexLeasePlanningGateState(runtimeLease *CodexLeaseRuntime, lane LaneKey) (int, bool) {
	if runtimeLease == nil || runtimeLease.planningGates == nil {
		return 0, false
	}
	runtimeLease.planningGates.mu.Lock()
	defer runtimeLease.planningGates.mu.Unlock()
	entry := runtimeLease.planningGates.entries[lane]
	if entry == nil {
		return 0, false
	}
	return entry.refs, len(entry.token) == 0
}
