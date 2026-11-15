package dots

import (
	"errors"
	"strings"
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

func TestCloudHarnessMalformedUserGoalResultRetainedOwnedPause(t *testing.T) {
	for _, name := range []string{"missing_goal", "wrong_status", "wrong_objective", "wrong_budget", "missing_clear_ack", "invalid_clear_ack"} {
		t.Run(name, func(t *testing.T) {
			h, store, id, _, goal := cloudHarnessGoalFixture(t)
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"threadId": id, "status": "active", "origin": "user"}
			method := "thread/goal/set"
			if name == "wrong_objective" {
				params["objective"] = "Requested user objective"
			}
			if name == "wrong_budget" {
				params["tokenBudget"] = 1000
			}
			if strings.Contains(name, "clear_ack") {
				method = "thread/goal/clear"
				delete(params, "status")
			}
			request := cloudMessage{"id": cloudHarnessRaw(13), "method": cloudHarnessRaw(method), "params": cloudHarnessRaw(params)}
			if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
				t.Fatal(err)
			}
			// The native response claims success but made no goal change.
			result := map[string]any{}
			if name == "wrong_status" {
				result["goal"] = goal
			}
			if name == "wrong_objective" || name == "wrong_budget" {
				reported := map[string]any{}
				for key, value := range goal {
					reported[key] = value
				}
				reported["status"] = "active"
				result["goal"] = reported
			}
			if name == "invalid_clear_ack" {
				result["cleared"] = "yes"
			}
			h.observe(cloudHarnessRaw(map[string]any{"id": 13, "result": result}))
			receipt, err := store.Load(id)
			if err != nil || receipt.GoalHold == nil || goal["status"] != "paused" {
				t.Fatalf("unverified user result discarded CQ's owned pause: goal=%v receipt=%#v err=%v", goal["status"], receipt, err)
			}
			h.threads[id].active, h.threads[id].activeTurnID = false, ""
			if err := h.finishGoalHold(id, true); err != nil {
				t.Fatal(err)
			}
			receipt, err = store.Load(id)
			if err != nil || receipt.GoalHold != nil || goal["status"] != "active" {
				t.Fatalf("unverified user result prevented verified recovery: goal=%v receipt=%#v err=%v", goal["status"], receipt, err)
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

func TestCloudHarnessGoalAccountingBeforeOwnPauseRestores(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	base := h.b.local.input
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/goal/set" && cloudHarnessString(cloudHarnessObject(request["params"]), "status") == "paused" {
			// Native accounts the current active goal before applying an external pause.
			accounted := map[string]any{}
			for key, value := range goal {
				accounted[key] = value
			}
			accounted["tokensUsed"] = 33
			h.observe(cloudHarnessRaw(map[string]any{"method": "thread/goal/updated", "params": map[string]any{"threadId": id, "turnId": "current", "goal": accounted}}))
		}
		return base.Write(raw)
	}}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	if goal["status"] != "paused" || !h.threads[id].active {
		t.Fatalf("current turn did not retain its pause: %v", goal["status"])
	}
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "active" {
		t.Fatalf("accounting notification blocked owned restore: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
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

type cloudHarnessFailingHoldStore struct {
	cloudHarnessReceiptStore
	fail bool
}

func (s *cloudHarnessFailingHoldStore) Save(receipt harness.Receipt) error {
	if s.fail && receipt.GoalHold != nil {
		return errors.New("fixture goal hold write failure")
	}
	return s.cloudHarnessReceiptStore.Save(receipt)
}

func TestCloudHarnessGoalHoldWriteFailurePreventsNativePause(t *testing.T) {
	h, store, id, methods, goal := cloudHarnessGoalFixture(t)
	failing := &cloudHarnessFailingHoldStore{cloudHarnessReceiptStore: store, fail: true}
	h.b.deps.harnessReceipts = failing
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("failed hold journal was accepted")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "active" || strings.Contains(strings.Join(*methods, ","), "thread/goal/set") {
		t.Fatalf("native pause ran without durable intent: goal=%v marker=%#v methods=%v err=%v", goal["status"], receipt.GoalHold, *methods, err)
	}
	failing.fail = false
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold == nil || goal["status"] != "paused" {
		t.Fatalf("hold journal did not retry: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
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

func TestCloudHarnessGoalRestoreReceiptFailureRetried(t *testing.T) {
	h, store, id, methods, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	failing := &cloudHarnessFailingGoalStore{cloudHarnessReceiptStore: store, fail: true}
	h.b.deps.harnessReceipts = failing
	if err := h.finishGoalHold(id, true); err == nil {
		t.Fatal("accepted native restore hid receipt failure")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold == nil || goal["status"] != "active" || strings.Count(strings.Join(*methods, ","), "thread/goal/set") != 2 {
		t.Fatalf("accepted restore lost durable marker: goal=%v marker=%#v methods=%v err=%v", goal["status"], receipt.GoalHold, *methods, err)
	}
	failing.fail = false
	if err := h.finishGoalHold(id, true); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "active" || strings.Count(strings.Join(*methods, ","), "thread/goal/set") != 2 {
		t.Fatalf("restore receipt retry repeated activation: goal=%v marker=%#v methods=%v err=%v", goal["status"], receipt.GoalHold, *methods, err)
	}
}

func TestCloudHarnessAmbiguousHoldClearedByExplicitUserResume(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	base := h.b.local.input
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/goal/set" {
			goal["status"], goal["updatedAt"] = "paused", 3
			return 0, errors.New("fixture lost reply after native pause")
		}
		return base.Write(raw)
	}}
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("unknown native pause result was accepted")
	}
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("ambiguous pause was reported as restored")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold == nil || receipt.State != "partial" || !strings.Contains(strings.Join(receipt.Issues, ","), "native_goal_restore_ambiguous") {
		t.Fatalf("ambiguous pause lacks durable partial receipt: %#v %v", receipt, err)
	}
	objective, budget := goal["objective"], goal["tokenBudget"]
	request := cloudMessage{"id": cloudHarnessRaw(11), "method": cloudHarnessRaw("thread/goal/set"), "params": cloudHarnessRaw(map[string]any{"threadId": id, "status": "active", "origin": "user"})}
	if _, err := h.prepare(request, cloudHarnessRaw(request)); err != nil {
		t.Fatal(err)
	}
	goal["status"], goal["updatedAt"] = "active", 4
	h.observe(cloudHarnessRaw(map[string]any{"id": 11, "result": map[string]any{"goal": goal}}))
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold != nil || strings.Contains(strings.Join(receipt.Issues, ","), "native_goal_restore_ambiguous") || goal["objective"] != objective || goal["tokenBudget"] != budget {
		t.Fatalf("explicit user resume left ambiguous ownership: goal=%#v receipt=%#v err=%v", goal, receipt, err)
	}
}
