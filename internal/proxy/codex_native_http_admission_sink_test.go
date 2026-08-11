package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexCanaryLeaseRuntimeCreditsOnlyDurableFirstAdmission(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	canaryFS := fsutil.NewMemFS()
	recorder, err := StartCodexCanary(canaryFS, "/canary/state.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runtimeLease, err := NewCodexCanaryLeaseRuntime(
		coordinator,
		func(context.Context, codex.AccountKey) error { return nil },
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := codexLeaseRuntimeTestPlan("canary-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.AdmitHTTP2xx(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().AdmittedTurns; got != 1 {
		t.Fatalf("admitted turns = %d, want 1", got)
	}
	if runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
		t.Fatal("successful canary credit blocked promotion")
	}
}

func TestCodexCanaryLeaseRuntimeBlocksPromotionWhenCreditCannotPersist(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/canary/state.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	runtimeLease, err := NewCodexCanaryLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error { return nil }, recorder)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("failed-canary-credit", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := handle.AdmitHTTP2xx()
	if err != nil || admitted == nil || !admitted.EverAdmitted() {
		t.Fatalf("durable admission = %#v, %v", admitted, err)
	}
	if !runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
		t.Fatal("failed canary credit did not block promotion")
	}
}

func TestCodexNativeHTTPAdmissionSinkRunsAfterFirstDurableAuthoritativeCommit(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	sink := &codexNativeHTTPDurableAdmissionTestSink{
		store: coordinator.Store(),
		raw:   []string{"private-turn", "private-account", "private-candidate", "private-evidence"},
	}
	runtimeLease, err := newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
		coordinator,
		func(context.Context, codex.AccountKey) error { return nil },
		sink,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := codexLeaseRuntimeTestPlan("private-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "private-account", CandidateID: "private-candidate", Kind: CodexAttemptSlotDirect,
	}})

	first, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	first, err = first.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{
		TurnState: "private-evidence", HasTurnState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || !sink.durable || sink.leaked != "" {
		t.Fatalf("first admission sink = calls %d durable %t leaked %q", sink.calls, sink.durable, sink.leaked)
	}
	if runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
		t.Fatal("successful first-admission sink blocked promotion")
	}
	first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Drain(); err != nil {
		t.Fatal(err)
	}

	repeatedPlan := plan
	repeatedPlan.Evidence = CodexLeaseRequestEvidence{TurnState: "private-evidence", HasTurnState: true}
	repeated, err := runtimeLease.BeginRequest(repeatedPlan)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err = repeated.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repeated.AdmitHTTP2xx(); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("repeated admission sink calls = %d, want 1", sink.calls)
	}
}

func TestCodexNativeHTTPAdmissionSinkIgnoresShadowAdmission(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	sink := &codexNativeHTTPDurableAdmissionTestSink{store: coordinator.Store()}
	runtimeLease, err := newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
		coordinator,
		func(context.Context, codex.AccountKey) error { return nil },
		sink,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := codexLeaseRuntimeTestPlan("shadow-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "shadow-account", CandidateID: "shadow-candidate", Kind: CodexAttemptSlotDirect,
	}})
	plan.Authority.Authoritative = false
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.AdmitHTTP2xx(); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 0 || runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
		t.Fatalf("shadow admission sink = calls %d blocked %t", sink.calls, runtimeLease.nativeHTTPAdmissionPromotionBlocked())
	}
}

func TestCodexNativeHTTPAdmissionSinkIgnoresFailedCommit(t *testing.T) {
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	sink := &codexNativeHTTPDurableAdmissionTestSink{store: coordinator.Store()}
	runtimeLease, err := newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
		coordinator,
		func(context.Context, codex.AccountKey) error { return nil },
		sink,
	)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("failed-commit-turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "failed-commit-account", CandidateID: "failed-commit-candidate", Kind: CodexAttemptSlotDirect,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.Store()
	store.directory = &failingSecureDirectory{
		SecureDirectory: store.directory,
		fsys:            &failingDurableFS{MemFS: fsys, failWrite: true},
	}

	if _, err = handle.AdmitHTTP2xx(); err == nil {
		t.Fatal("failed durable admission returned nil error")
	}
	if sink.calls != 0 || runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
		t.Fatalf("failed commit sink = calls %d blocked %t", sink.calls, runtimeLease.nativeHTTPAdmissionPromotionBlocked())
	}
}

func TestCodexNativeHTTPAdmissionSinkFailureCannotUndoAdmission(t *testing.T) {
	for _, test := range []struct {
		name      string
		panicSink bool
		err       error
	}{
		{name: "error", err: errors.New("test sink failure")},
		{name: "panic", panicSink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			sink := &codexNativeHTTPDurableAdmissionTestSink{store: coordinator.Store(), err: test.err, panicSink: test.panicSink}
			runtimeLease, err := newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
				coordinator,
				func(context.Context, codex.AccountKey) error { return nil },
				sink,
			)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
				AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
			}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			admitted, err := handle.AdmitHTTP2xx()
			if err != nil {
				t.Fatalf("admission returned sink failure: %v", err)
			}
			if !admitted.EverAdmitted() || sink.calls != 1 || !runtimeLease.nativeHTTPAdmissionPromotionBlocked() {
				t.Fatalf("failed sink result = admitted %t calls %d blocked %t", admitted.EverAdmitted(), sink.calls, runtimeLease.nativeHTTPAdmissionPromotionBlocked())
			}
		})
	}
}

type codexNativeHTTPDurableAdmissionTestSink struct {
	store     *CodexLeaseStore
	raw       []string
	calls     int
	durable   bool
	leaked    string
	err       error
	panicSink bool
}

func (sink *codexNativeHTTPDurableAdmissionTestSink) observeCodexNativeHTTPFirstAdmission(observation codexNativeHTTPAdmissionObservation) error {
	sink.calls++
	encoded := fmt.Sprintf("%v", observation)
	for _, raw := range sink.raw {
		if strings.Contains(encoded, raw) {
			sink.leaked = raw
		}
	}
	if sink.store != nil {
		sink.store.mu.Lock()
		if sink.store.v2 != nil {
			for _, record := range sink.store.v2.Records {
				if record.EverAdmitted && record.Authoritative &&
					record.AdmissionRequestGeneration == observation.RequestGeneration &&
					record.CurrentAttemptGeneration == observation.AttemptGeneration &&
					record.AdmissionJournalGeneration <= sink.store.v2.Generation {
					sink.durable = true
					break
				}
			}
		}
		sink.store.mu.Unlock()
	}
	if sink.panicSink {
		panic("test sink panic")
	}
	return sink.err
}
