package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCodexHTTPRequestPlanFactoryRejectsQueuedFutureTurnState(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	plan.Key.Lane.Session = "session"
	plan.Key.Lane.Thread = "thread"
	plan.RequestedModel = "gpt-5"
	plan.EffectiveModel = "gpt-5"

	predecessor, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{
		TurnState: "private-state-a", HasTurnState: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Routes = coordinator
	factory.Authority = plan.Authority
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = withRuntimeCallerAuthority(ctx, RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
	})
	ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")

	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	result := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
			Headers: http.Header{"X-Codex-Turn-State": {"private-state-b"}},
		})
		result <- buildResult{prepared: prepared, err: buildErr}
	}()

	var completedEarly *buildResult
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	waiting := time.NewTicker(time.Millisecond)
	defer waiting.Stop()
waitForIngress:
	for !codexHTTPRequestPlanPlanningGateHeld(runtime, plan.Key.Lane) {
		select {
		case got := <-result:
			completedEarly = &got
			break waitForIngress
		case <-waiting.C:
		case <-deadline.C:
			t.Fatal("future-state request neither queued nor rejected at ingress")
		}
	}

	predecessor, err = predecessor.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{
		TurnState: "private-state-b", HasTurnState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = predecessor.Drain(); err != nil {
		t.Fatal(err)
	}

	var got buildResult
	if completedEarly != nil {
		got = *completedEarly
	} else {
		select {
		case got = <-result:
		case <-ctx.Done():
			t.Fatal("future-state request did not finish after predecessor drained")
		}
	}
	if got.err == nil {
		if got.prepared.Lifecycle != nil {
			_, _ = got.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background())
		}
		if got.prepared.Frozen != nil {
			got.prepared.Frozen.Release()
		}
		t.Fatal("turn state invalid at ingress became valid while waiting")
	}
	var planErr *CodexHTTPRequestPlanError
	if !errors.As(got.err, &planErr) ||
		planErr.Code != CodexHTTPRequestPlanBegin ||
		planErr.Reason != CodexRequestFailureReason(codexContinuityTurnStateMismatch) {
		t.Fatalf("future-state error = %#v, want begin_request/turn_state_mismatch", got.err)
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesLatchedTurnStateAcrossDrain(t *testing.T) {
	t.Parallel()

	fixture := newCodexTerminalIngressContinuityFixture(t)
	inventoryEntered := make(chan struct{})
	inventoryRelease := make(chan struct{})
	fixture.factory.Inventory = &codexHTTPRequestPlanBlockingInventory{
		inner:   fixture.factory.Inventory,
		entered: inventoryEntered,
		release: inventoryRelease,
	}
	ctx := codexIngressContinuityCallerContext(t)
	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	result := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := fixture.factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
			Headers: http.Header{"X-Codex-Turn-State": {"private-state-a"}},
		})
		result <- buildResult{prepared: prepared, err: buildErr}
	}()

	select {
	case <-inventoryEntered:
	case <-ctx.Done():
		t.Fatal("terminal successor did not reach credential inventory")
	}
	if _, err := fixture.predecessor.Drain(); err != nil {
		t.Fatal(err)
	}
	close(inventoryRelease)

	select {
	case got := <-result:
		if got.err != nil {
			var planErr *CodexHTTPRequestPlanError
			if errors.As(got.err, &planErr) {
				t.Fatalf("terminal ingress continuation = stage %s reason %s, want success", planErr.Code, planErr.Reason)
			}
			t.Fatalf("terminal ingress continuation = %T %v, want success", got.err, got.err)
		}
		if got.prepared.leaseHandle == nil || got.prepared.leaseHandle.RequestGeneration() != 3 {
			t.Fatalf("terminal ingress continuation generation = %v, want 3", got.prepared.leaseHandle)
		}
		if _, err := got.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		got.prepared.Frozen.Release()
	case <-ctx.Done():
		t.Fatal("terminal ingress continuation did not finish after drain")
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesLatchedTurnStateFirstSeenAfterDrain(t *testing.T) {
	t.Parallel()

	fixture := newCodexTerminalIngressContinuityFixture(t)
	if _, err := fixture.predecessor.Drain(); err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.factory.Build(codexIngressContinuityCallerContext(t), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		Headers: http.Header{"X-Codex-Turn-State": {"private-state-a"}},
	})
	if err != nil {
		t.Fatalf("latched turn-state continuation = %T %v, want success", err, err)
	}
	if _, err := prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared.Frozen.Release()
}

func TestCodexHTTPRequestPlanFactoryRejectsArbitraryTerminalIngressTurnState(t *testing.T) {
	t.Parallel()

	fixture := newCodexTerminalIngressContinuityFixture(t)
	inventoryEntered := make(chan struct{})
	inventoryRelease := make(chan struct{})
	fixture.factory.Inventory = &codexHTTPRequestPlanBlockingInventory{
		inner:   fixture.factory.Inventory,
		entered: inventoryEntered,
		release: inventoryRelease,
	}
	ctx := codexIngressContinuityCallerContext(t)
	result := make(chan error, 1)
	go func() {
		prepared, buildErr := fixture.factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
			Headers: http.Header{"X-Codex-Turn-State": {"private-state-x"}},
		})
		if buildErr == nil {
			if prepared.Lifecycle != nil {
				_, _ = prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background())
			}
			if prepared.Frozen != nil {
				prepared.Frozen.Release()
			}
		}
		result <- buildErr
	}()

	select {
	case <-inventoryEntered:
	case <-ctx.Done():
		t.Fatal("arbitrary-state request did not reach credential inventory")
	}
	if _, err := fixture.predecessor.Drain(); err != nil {
		t.Fatal(err)
	}
	close(inventoryRelease)
	select {
	case err := <-result:
		assertCodexIngressContinuityMismatch(t, err)
	case <-ctx.Done():
		t.Fatal("arbitrary-state request did not finish after drain")
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesLatchedTurnStateAfterRuntimeRestart(t *testing.T) {
	t.Parallel()

	fixture := newCodexTerminalIngressContinuityFixture(t)
	fixture.factory.Runtime = newCodexLeaseRuntimeTest(t, fixture.coordinator)
	inventoryEntered := make(chan struct{})
	inventoryRelease := make(chan struct{})
	fixture.factory.Inventory = &codexHTTPRequestPlanBlockingInventory{
		inner:   fixture.factory.Inventory,
		entered: inventoryEntered,
		release: inventoryRelease,
	}
	ctx := codexIngressContinuityCallerContext(t)
	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	result := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := fixture.factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
			Headers: http.Header{"X-Codex-Turn-State": {"private-state-a"}},
		})
		result <- buildResult{prepared: prepared, err: buildErr}
	}()

	select {
	case <-inventoryEntered:
	case <-ctx.Done():
		t.Fatal("restarted successor did not reach credential inventory")
	}
	if _, err := fixture.predecessor.Drain(); err != nil {
		t.Fatal(err)
	}
	close(inventoryRelease)
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("latched turn-state continuation after restart = %T %v, want success", got.err, got.err)
		}
		if _, err := got.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		got.prepared.Frozen.Release()
	case <-ctx.Done():
		t.Fatal("restarted successor did not finish after drain")
	}
}

type codexTerminalIngressContinuityFixture struct {
	predecessor *CodexLeaseRequestHandle
	factory     *CodexHTTPRequestPlanFactory
	coordinator *CodexContinuityCoordinator
}

func newCodexTerminalIngressContinuityFixture(t *testing.T) codexTerminalIngressContinuityFixture {
	t.Helper()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	})
	plan.Key.Lane.Session = "session"
	plan.Key.Lane.Thread = "thread"
	plan.RequestedModel = "gpt-5"
	plan.EffectiveModel = "gpt-5"

	seed, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	seed, err = seed.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	seed, err = seed.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{
		TurnState: "private-state-a", HasTurnState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seed, err = seed.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Drain(); err != nil {
		t.Fatal(err)
	}

	plan.Evidence = CodexLeaseRequestEvidence{TurnState: "private-state-a", HasTurnState: true}
	predecessor, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.RejectAndPrepare(2)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{
		TurnState: "private-state-b", HasTurnState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.Indeterminate()
	if err != nil {
		t.Fatal(err)
	}

	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Routes = coordinator
	factory.Authority = plan.Authority
	return codexTerminalIngressContinuityFixture{predecessor: predecessor, factory: factory, coordinator: coordinator}
}

func codexIngressContinuityCallerContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	ctx = withRuntimeCallerAuthority(ctx, RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
	})
	return withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")
}

func assertCodexIngressContinuityMismatch(t *testing.T, err error) {
	t.Helper()
	var planErr *CodexHTTPRequestPlanError
	if !errors.As(err, &planErr) ||
		planErr.Code != CodexHTTPRequestPlanBegin ||
		planErr.Reason != CodexRequestFailureReason(codexContinuityTurnStateMismatch) {
		t.Fatalf("continuity error = %#v, want begin_request/turn_state_mismatch", err)
	}
}

func TestCodexLeaseIngressContinuityClaimIsOneShotAndScoped(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	otherCoordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	otherRuntime := newCodexLeaseRuntimeTest(t, otherCoordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	evidence := CodexLeaseRequestEvidence{
		PreviousResponseID: "response-a",
		TurnState:          "private-state-a",
		HasTurnState:       true,
		HasEncryptedState:  true,
	}
	binding := codexLeaseIngressContinuityBinding{
		owner:               runtime,
		key:                 plan.Key,
		authority:           cloneCodexLeaseAuthorityPolicy(plan.Authority),
		turnStateHash:       runtime.store.hash("turn-state", evidence.TurnState),
		correlationHash:     runtime.store.hash("correlation", evidence.PreviousResponseID),
		hasTurnState:        true,
		hasPreviousResponse: true,
		hasEncryptedState:   true,
	}
	newClaim := func() *codexLeaseIngressContinuityClaim {
		return newCodexLeaseIngressContinuityClaim(runtime, &binding)
	}

	claim := newClaim()
	if got, err := claim.consume(runtime, plan.Key, plan.Authority, evidence); err != nil || got == nil {
		t.Fatalf("first claim consume = %#v, %v", got, err)
	}
	if _, err := claim.consume(runtime, plan.Key, plan.Authority, evidence); !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("replayed claim error = %T %v, want authority mismatch", err, err)
	}

	released := newClaim()
	released.release()
	if _, err := released.consume(runtime, plan.Key, plan.Authority, evidence); !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("released claim error = %T %v, want authority mismatch", err, err)
	}

	wrongKey := plan.Key
	wrongKey.Lane.Thread = "other-thread"
	wrongAuthority := cloneCodexLeaseAuthorityPolicy(plan.Authority)
	wrongAuthority.ModeEpoch++
	wrongEvidence := evidence
	wrongEvidence.TurnState = "private-state-b"
	wrongPreviousResponse := evidence
	wrongPreviousResponse.PreviousResponseID = "response-b"
	wrongEncryptedState := evidence
	wrongEncryptedState.HasEncryptedState = false
	for name, attempt := range map[string]func(*codexLeaseIngressContinuityClaim) error{
		"runtime": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(otherRuntime, plan.Key, plan.Authority, evidence)
			return err
		},
		"key": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(runtime, wrongKey, plan.Authority, evidence)
			return err
		},
		"authority": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(runtime, plan.Key, wrongAuthority, evidence)
			return err
		},
		"evidence": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(runtime, plan.Key, plan.Authority, wrongEvidence)
			return err
		},
		"previous response": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(runtime, plan.Key, plan.Authority, wrongPreviousResponse)
			return err
		},
		"encrypted state": func(claim *codexLeaseIngressContinuityClaim) error {
			_, err := claim.consume(runtime, plan.Key, plan.Authority, wrongEncryptedState)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := attempt(newClaim()); !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
				t.Fatalf("scoped claim error = %T %v, want authority mismatch", err, err)
			}
		})
	}
}
