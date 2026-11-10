package proxy

import (
	"bytes"
	"context"
	"errors"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"testing"
)

func TestCodexLeaseRepeatedAdmissionDoesNotPersist(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			admit := func(h *CodexLeaseRequestHandle) (*CodexLeaseRequestHandle, error) {
				if transport == "http" {
					return h.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "state", HasTurnState: true})
				}
				return h.AdmitWebSocketContext(context.Background(), CodexWebSocketAdmissionEvidence{TurnState: "state", HasTurnState: true, DownstreamGeneration: 41, UpstreamGeneration: 43})
			}
			handle, err = admit(handle)
			if err != nil {
				t.Fatal(err)
			}
			store := coordinator.Store()
			before := append([]byte(nil), store.journalBytes...)
			generation := store.Generation()
			written := countCodexDeltaWrites(store)
			repeated := handle
			for range 100 {
				repeated, err = admit(repeated)
				if err != nil {
					t.Fatal(err)
				}
			}
			if *written != 0 || generation != store.Generation() || !bytes.Equal(before, store.journalBytes) {
				t.Fatal("repeated admission rewrote durable journal")
			}
			completed, err := repeated.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := admit(handle); !errors.Is(err, ErrCodexLeaseStaleMutation) {
				t.Fatalf("stale repeated admission = %v", err)
			}
			if _, err := completed.Drain(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexLeaseRepeatedAdmissionRechecksGenerationAfterAccountValidation(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	armed := false
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		if !armed {
			return nil
		}
		armed = false
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.commitV2Locked(store.v2.Generation, *store.v2)
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
	armed = true
	if _, err := handle.AdmitHTTP2xx(); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("admission acknowledged superseded journal generation: %v", err)
	}
}

func TestCodexLeaseIdleInspectionDoesNotPersist(t *testing.T) {
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("idle-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	completeCodexLeaseRuntimeTurn(t, runtimeLease, plan)
	store := coordinator.Store()
	before := fsysFileBytes(t, fsys, "/state/leases.json")
	generation := store.Generation()
	written := countCodexDeltaWrites(store)
	for range 100 {
		if _, err := store.loadLaneSnapshot(plan.Key, plan.Accounts, plan.Authority); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthorityEvidence(); err != nil {
			t.Fatal(err)
		}
	}
	disk := fsysFileBytes(t, fsys, "/state/leases.json")
	if *written != 0 || generation != store.Generation() || !bytes.Equal(before, disk) {
		t.Fatal("idle authority inspection rewrote journal")
	}
}
