package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCodexLeaseRuntimeWebSocketAdmissionPersistsExactSocketGeneration(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("ws-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})

	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: 41,
		UpstreamGeneration:   43,
		ResponseID:           "response-a",
		ResponseCreated:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if handle.State() != LeaseBoundActive || !handle.record.EverAdmitted {
		t.Fatalf("admitted state = %s ever=%v, want bound-active/true", handle.State(), handle.record.EverAdmitted)
	}
	if handle.record.DownstreamSocketGeneration != 41 || handle.record.UpstreamSocketGeneration != 43 || handle.record.SocketLineageExtinct {
		t.Fatalf("socket lineage = %d/%d extinct=%v, want 41/43/false", handle.record.DownstreamSocketGeneration, handle.record.UpstreamSocketGeneration, handle.record.SocketLineageExtinct)
	}
	if !handle.record.HasResponseAnchor {
		t.Fatal("response.created anchor was not persisted")
	}
}

func TestCodexWSLifecycleDoesNotAdmitStatelessUpstreamUpgrade(t *testing.T) {
	t.Parallel()
	lifecycle, store := newCodexWSLifecycleTest(t, []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	before := append([]byte(nil), store.journalBytes...)
	if err := lifecycle.ObserveUpstreamUpgrade(context.Background(), 43, ""); err != nil {
		t.Fatal(err)
	}
	if lifecycle.attemptAdmitted || lifecycle.handle.record.EverAdmitted {
		t.Fatal("stateless upstream 101 admitted request")
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("stateless upstream 101 changed journal")
	}
}

func TestCodexWSLifecycleAdmitsStatefulUpgradeOrCreatedEvent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		observe func(*codexWSLifecycle) error
	}{
		{name: "stateful upgrade", observe: func(lifecycle *codexWSLifecycle) error {
			return lifecycle.ObserveUpstreamUpgrade(context.Background(), 43, "state-a")
		}},
		{name: "response created", observe: func(lifecycle *codexWSLifecycle) error {
			_, err := lifecycle.ObserveFrame(context.Background(), 43, []byte(`{"type":"response.created","response":{"id":"response-a"}}`))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, _ := newCodexWSLifecycleTest(t, []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			if err := test.observe(lifecycle); err != nil {
				t.Fatal(err)
			}
			if !lifecycle.attemptAdmitted || !lifecycle.handle.record.EverAdmitted || lifecycle.handle.State() != LeaseBoundActive {
				t.Fatalf("admission = attempt %v ever %v state %s", lifecycle.attemptAdmitted, lifecycle.handle.record.EverAdmitted, lifecycle.handle.State())
			}
			if lifecycle.handle.record.DownstreamSocketGeneration != 41 || lifecycle.handle.record.UpstreamSocketGeneration != 43 {
				t.Fatalf("socket generation = %d/%d", lifecycle.handle.record.DownstreamSocketGeneration, lifecycle.handle.record.UpstreamSocketGeneration)
			}
		})
	}
}

func TestCodexWSLifecycleRejectsStaleGenerationWithoutMutation(t *testing.T) {
	t.Parallel()
	lifecycle, store := newCodexWSLifecycleTest(t, []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	before := append([]byte(nil), store.journalBytes...)
	if _, err := lifecycle.ObserveFrame(context.Background(), 44, []byte(`{"type":"response.created","response":{}}`)); !errors.Is(err, ErrCodexWSStaleGeneration) {
		t.Fatalf("stale event = %T %v, want stale generation", err, err)
	}
	if !bytes.Equal(before, store.journalBytes) {
		t.Fatal("stale generation changed journal")
	}
}

func TestCodexWSLifecycleHard429RotatesOnlyBeforeAdmission(t *testing.T) {
	t.Parallel()
	slots := []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	}
	lifecycle, _ := newCodexWSLifecycleTest(t, slots)
	result, err := lifecycle.ObserveFrame(context.Background(), 43, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.DefinitePreAdmissionRejection || !result.HardUsageLimit {
		t.Fatalf("pre-admission result = %#v", result)
	}
	if err := lifecycle.RejectAndPrepare(context.Background(), 43, 2); err != nil {
		t.Fatal(err)
	}
	if lifecycle.handle.AccountKey() != "account-b" || lifecycle.handle.record.State != LeaseProvisional {
		t.Fatalf("replacement = account %q state %s", lifecycle.handle.AccountKey(), lifecycle.handle.record.State)
	}

	admitted, store := newCodexWSLifecycleTest(t, slots)
	if _, err := admitted.ObserveFrame(context.Background(), 43, []byte(`{"type":"response.created","response":{}}`)); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	result, err = admitted.ObserveFrame(context.Background(), 43, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.DefinitePreAdmissionRejection {
		t.Fatal("admitted hard 429 became rotation authority")
	}
	if admitted.handle.AccountKey() != "account-a" || !admitted.handle.record.EverAdmitted {
		t.Fatal("admitted hard 429 changed account authority")
	}
	if bytes.Equal(before, store.journalBytes) {
		t.Fatal("admitted provider failure was not persisted")
	}
}

func TestCodexWSLifecycleMalformedEventBecomesIndeterminate(t *testing.T) {
	t.Parallel()
	lifecycle, _ := newCodexWSLifecycleTest(t, []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	if _, err := lifecycle.ObserveFrame(context.Background(), 43, []byte(`{"type":`)); err == nil {
		t.Fatal("malformed event returned nil error")
	}
	if lifecycle.handle.record.State != LeaseOrphaned || !lifecycle.handle.record.NonMigratable {
		t.Fatalf("malformed after-image = state %s non-migratable %v", lifecycle.handle.record.State, lifecycle.handle.record.NonMigratable)
	}
}

func newCodexWSLifecycleTest(t *testing.T, slots []CodexLeaseAttemptSlotPlan) (*codexWSLifecycle, *CodexLeaseStore) {
	t.Helper()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("ws-turn", slots))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := newCodexWSLifecycle(handle, 41, 43)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle, coordinator.Store()
}

func TestCodexLeaseRuntimeWebSocketAdmissionRejectsIncompleteOrChangedGeneration(t *testing.T) {
	t.Parallel()
	for _, evidence := range []CodexWebSocketAdmissionEvidence{
		{UpstreamGeneration: 43, ResponseID: "response-a", ResponseCreated: true},
		{DownstreamGeneration: 41, ResponseID: "response-a", ResponseCreated: true},
	} {
		coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
		runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
		plan := codexLeaseRuntimeTestPlan("ws-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
		handle, err := runtimeLease.BeginRequest(plan)
		if err != nil {
			t.Fatal(err)
		}
		handle, err = handle.MarkDispatched()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.AdmitWebSocketContext(context.Background(), evidence); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
			t.Fatalf("incomplete socket evidence = %T %v, want invalid mutation", err, err)
		}
	}
}
