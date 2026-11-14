package dots

import (
	"errors"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessGoalMutationRetainedHoldUntilReply(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			h, store, id, _, goal := cloudHarnessGoalFixture(t)
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			request := cloudMessage{"id": cloudHarnessRaw(7), "method": cloudHarnessRaw("thread/goal/set"), "params": cloudHarnessRaw(map[string]any{"threadId": id, "status": "paused", "origin": "user"})}
			if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
				t.Fatal(err)
			}
			receipt, err := store.Load(id)
			if err != nil || receipt.GoalHold == nil || !receipt.GoalHold.Yield || h.threads[id].pendingSettings != 1 {
				t.Fatalf("user mutation discarded hold before its result: %#v, %v", receipt.GoalHold, err)
			}
			h.threads[id].active = false
			if err := h.finishGoalHold(id, true); err != nil || goal["status"] != "paused" {
				t.Fatalf("restoration raced pending mutation: %v", err)
			}
			reply := cloudMessage{"id": cloudHarnessRaw(7), "result": cloudHarnessRaw(map[string]any{"goal": goal})}
			if rejected {
				delete(reply, "result")
				reply["error"] = cloudHarnessRaw(map[string]any{"code": -32000, "message": "fixture rejection"})
			}
			h.observe(cloudHarnessRaw(reply))
			if h.threads[id].pendingSettings != 0 {
				t.Fatal("user mutation left pending counter")
			}
			if err := h.finishGoalHold(id, true); err != nil {
				t.Fatal(err)
			}
			want := "paused"
			if rejected {
				want = "active"
			}
			if goal["status"] != want {
				t.Fatalf("goal status %v, want %s", goal["status"], want)
			}
		})
	}
}

func TestCloudHarnessReleasedGoalBeforeLastSubscription(t *testing.T) {
	for _, method := range []string{"thread/unsubscribe", "thread/archive", "shutdown", "disconnected"} {
		t.Run(method, func(t *testing.T) {
			h, store, id, _, goal := cloudHarnessGoalFixture(t)
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			switch method {
			case "shutdown":
				h.restoreGoalHolds()
			case "disconnected":
				h.threads[id].owned = false
				if err := h.reconcile(id, "", ""); err != nil {
					t.Fatal(err)
				}
			default:
				request := cloudMessage{"id": cloudHarnessRaw(8), "method": cloudHarnessRaw(method), "params": cloudHarnessRaw(map[string]any{"threadId": id})}
				if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := store.Load(id)
			if err != nil || receipt.GoalHold != nil || goal["status"] != "active" {
				t.Fatalf("subscription release stranded hold: %v %#v %v", goal["status"], receipt.GoalHold, err)
			}
		})
	}
}

func TestCloudHarnessGoalToolPauseWonWithinSameSecond(t *testing.T) {
	h, _, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	h.observe(cloudHarnessRaw(map[string]any{"method": "thread/goal/updated", "params": map[string]any{"threadId": id, "turnId": "current", "goal": goal}}))
	h.threads[id].active = false
	if err := h.finishGoalHold(id, true); err != nil || goal["status"] != "paused" {
		t.Fatalf("CQ resumed explicit native goal-tool pause: %v", err)
	}
}

func TestCloudHarnessCancelledUnsentGoalMutationRestoredHold(t *testing.T) {
	h, _, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	request := cloudMessage{"id": cloudHarnessRaw(9), "method": cloudHarnessRaw("thread/goal/set"), "params": cloudHarnessRaw(map[string]any{"threadId": id, "status": "paused"})}
	if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
		t.Fatal(err)
	}
	gateway := &cloudHarnessGateway{h: h}
	gateway.cancelPending("stdio", request["id"])
	if h.threads[id].pendingSettings != 0 {
		t.Fatal("cancelled request left pending counter")
	}
	if err := h.finishGoalHold(id, true); err != nil || goal["status"] != "active" {
		t.Fatalf("unsent request stranded hold: %v", err)
	}
}

type cloudHarnessFailingGoalStore struct {
	cloudHarnessReceiptStore
	fail bool
}

func (s *cloudHarnessFailingGoalStore) Save(receipt harness.Receipt) error {
	if s.fail {
		return errors.New("fixture receipt failure")
	}
	return s.cloudHarnessReceiptStore.Save(receipt)
}

func TestCloudHarnessRetriedGoalHandoffJournalBeforeResuming(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	request := cloudMessage{"id": cloudHarnessRaw(10), "method": cloudHarnessRaw("thread/goal/set"), "params": cloudHarnessRaw(map[string]any{"threadId": id, "status": "paused"})}
	if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.prepare(request, cloudHarnessRaw(request)); err == nil {
		t.Fatal("overlapping goal mutation was admitted")
	}
	failing := &cloudHarnessFailingGoalStore{cloudHarnessReceiptStore: store, fail: true}
	h.b.deps.harnessReceipts = failing
	h.observe(cloudHarnessRaw(map[string]any{"id": 10, "error": map[string]any{"code": -32000, "message": "fixture rejection"}}))
	thread := h.threads[id]
	if thread.pendingSettings != 1 || thread.goalYieldResult == nil || !thread.goalMutationPending {
		t.Fatal("failed handoff journal released mutation guard")
	}
	if err := h.finishGoalHold(id, true); err != nil || goal["status"] != "paused" {
		t.Fatalf("failed handoff journal raced restoration: %v", err)
	}
	failing.fail = false
	thread.active, thread.activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || thread.pendingSettings != 0 || thread.goalMutationPending || thread.goalYieldResult != nil || goal["status"] != "active" || receipt.GoalHold != nil {
		t.Fatalf("handoff journal did not recover: status=%v receipt=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
}
