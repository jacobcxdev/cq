package dots

import (
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessHeldGoalUntilConfigurationApplied(t *testing.T) {
	h, store, id, methods := cloudHarnessFailureFixture(t)
	thread := h.threads[id]
	thread.active, thread.activeTurnID = true, "current"
	goal := map[string]any{"threadId": id, "objective": "private goal objective", "status": "active", "createdAt": 1, "updatedAt": 2, "tokenBudget": nil, "tokensUsed": 10, "timeUsedSeconds": 1}
	base := h.b.local.input
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() != "thread/goal/get" && request.method() != "thread/goal/set" {
			if goal["status"] != "paused" && request.method() == "thread/unsubscribe" {
				t.Fatal("configuration reload raced automatic continuation")
			}
			return base.Write(raw)
		}
		*methods = append(*methods, request.method())
		if request.method() == "thread/goal/set" {
			params := cloudHarnessObject(request["params"])
			if len(params["objective"]) != 0 || len(params["tokenBudget"]) != 0 || cloudHarnessString(params, "origin") != "automatic" {
				t.Fatal("hold changed goal intent or budget")
			}
			if cloudHarnessString(params, "status") == "active" && !thread.minimal {
				t.Fatal("goal resumed before verified application")
			}
			goal["status"] = cloudHarnessString(params, "status")
			goal["updatedAt"] = 3
		}
		h.calls[string(request["id"])] <- cloudMessage{"result": cloudHarnessRaw(map[string]any{"goal": goal})}
		return len(raw), nil
	}}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	if goal["status"] != "paused" || !thread.active || strings.Contains(strings.Join(*methods, ","), "thread/unsubscribe") {
		t.Fatalf("pending reload did not hold continuation safely: goal=%v active=%v methods=%v", goal["status"], thread.active, *methods)
	}
	receipt, err := store.Load(id)
	if err != nil || !strings.Contains(string(cloudHarnessRaw(receipt)), "goal_hold") || strings.Contains(string(cloudHarnessRaw(receipt)), "private goal objective") {
		t.Fatalf("hold recovery receipt unavailable or leaked objective: %v", err)
	}
	thread.active, thread.activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	if goal["status"] != "active" || !thread.minimal || thread.receiptState == "pending" || thread.receiptState == "failed" {
		t.Fatalf("goal not restored after application: goal=%v receipt=%s", goal["status"], thread.receiptState)
	}
	receipt, err = store.Load(id)
	if err != nil || strings.Contains(string(cloudHarnessRaw(receipt)), "goal_hold") || strings.Contains(strings.Join(receipt.Issues, ","), "native_goal_resume_pending") {
		t.Fatalf("completed hold retained recovery marker: %v", err)
	}
}

func cloudHarnessGoalFixture(t *testing.T) (*cloudHarness, harness.Receipts, string, *[]string, map[string]any) {
	t.Helper()
	h, store, id, methods := cloudHarnessFailureFixture(t)
	h.threads[id].active, h.threads[id].activeTurnID = true, "current"
	h.threads[id].pluginsKnown = true
	goal := map[string]any{"threadId": id, "objective": "private objective", "status": "active", "createdAt": 1, "updatedAt": 2, "tokenBudget": nil}
	base := h.b.local.input
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() != "thread/goal/get" && request.method() != "thread/goal/set" {
			return base.Write(raw)
		}
		*methods = append(*methods, request.method())
		if request.method() == "thread/goal/set" {
			goal["status"] = cloudHarnessString(cloudHarnessObject(request["params"]), "status")
			goal["updatedAt"] = 3
		}
		h.calls[string(request["id"])] <- cloudMessage{"result": cloudHarnessRaw(map[string]any{"goal": goal})}
		return len(raw), nil
	}}
	return h, store, id, methods, goal
}

func TestCloudHarnessGoalHoldRespectedExistingGoalState(t *testing.T) {
	for _, name := range []string{"paused", "complete", "ephemeral"} {
		t.Run(name, func(t *testing.T) {
			h, store, id, methods, goal := cloudHarnessGoalFixture(t)
			switch name {
			case "ephemeral":
				h.threads[id].ephemeral = true
			default:
				goal["status"] = name
			}
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			receipt, err := store.Load(id)
			if err != nil || receipt.GoalHold != nil || strings.Contains(strings.Join(*methods, ","), "thread/goal/set") {
				t.Fatalf("goal was held unexpectedly: methods=%v err=%v", *methods, err)
			}
		})
	}
}

func TestCloudHarnessGoalHoldRecoveredAndRespectedUserChanges(t *testing.T) {
	for _, change := range []string{"none", "intent-only", "objective", "paused-again", "complete"} {
		t.Run(change, func(t *testing.T) {
			h, store, id, _, goal := cloudHarnessGoalFixture(t)
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			if change == "intent-only" {
				receipt, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				receipt.GoalHold.UpdatedAt = 0
				if err := store.Save(receipt); err != nil {
					t.Fatal(err)
				}
			}
			h.release(id)
			thread := h.threads[id]
			thread.goalHold, thread.goalHoldLoaded = nil, false
			thread.active, thread.activeTurnID = false, ""
			switch change {
			case "objective":
				goal["objective"] = "user edited objective"
			case "paused-again":
				goal["updatedAt"] = 4
			case "complete":
				goal["status"] = "complete"
			}
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			want := "paused"
			if change == "none" {
				want = "active"
			} else if change == "complete" {
				want = "complete"
			}
			receipt, err := store.Load(id)
			if err != nil || receipt.GoalHold != nil || goal["status"] != want {
				t.Fatalf("goal recovery ignored ownership: goal=%v want=%s receipt=%#v err=%v", goal["status"], want, receipt.GoalHold, err)
			}
		})
	}
}

func TestCloudHarnessGoalHoldRestoredAfterReloadFailure(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	base := h.b.local.input
	failed := false
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/resume" && !failed {
			failed = true
			h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32000, "message": "fixture rejection"})}
			return len(raw), nil
		}
		return base.Write(raw)
	}}
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("reload failure disappeared")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold != nil || receipt.State != "failed" || goal["status"] != "active" {
		t.Fatalf("reload failure stranded goal: status=%v receipt=%#v err=%v", goal["status"], receipt, err)
	}
}

func TestCloudHarnessGoalHoldReadFailureRetried(t *testing.T) {
	h, store, id, methods, goal := cloudHarnessGoalFixture(t)
	base := h.b.local.input
	failed := false
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/goal/get" && !failed {
			failed = true
			h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32000, "message": "fixture rejection"})}
			return len(raw), nil
		}
		return base.Write(raw)
	}}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	if !failed || goal["status"] != "active" {
		t.Fatalf("failed read changed goal: failed=%v status=%v", failed, goal["status"])
	}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold == nil || goal["status"] != "paused" || strings.Count(strings.Join(*methods, ","), "thread/goal/get") < 1 {
		t.Fatalf("transient read was not retried: goal=%v marker=%#v methods=%v err=%v", goal["status"], receipt.GoalHold, *methods, err)
	}
}

func TestCloudHarnessGoalHoldPauseRejectionRetried(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	base := h.b.local.input
	rejected := false
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/goal/set" && !rejected {
			rejected = true
			h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32000, "message": "fixture rejection"})}
			return len(raw), nil
		}
		return base.Write(raw)
	}}
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("native pause rejection disappeared")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "active" {
		t.Fatalf("rejected pause left a hold: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold == nil || goal["status"] != "paused" {
		t.Fatalf("rejected pause not retried: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
}

func TestCloudHarnessGoalHoldSameIntentPausePreserved(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	goal["updatedAt"] = 4 // User or agent paused the same goal again.
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "paused" {
		t.Fatalf("same-intent pause was overridden: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
}

func TestCloudHarnessGoalHoldRestoreFailureRetried(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	base := h.b.local.input
	failed := false
	h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		if request.method() == "thread/goal/set" && cloudHarnessString(cloudHarnessObject(request["params"]), "status") == "active" && !failed {
			failed = true
			h.calls[string(request["id"])] <- cloudMessage{"result": cloudHarnessRaw(map[string]any{"goal": goal})}
			return len(raw), nil
		}
		return base.Write(raw)
	}}
	h.threads[id].active, h.threads[id].activeTurnID = false, ""
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("unverified native restore was accepted")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold == nil || goal["status"] != "paused" {
		t.Fatalf("failed restore lost recovery marker: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold != nil || goal["status"] != "active" {
		t.Fatalf("failed restore not retried: goal=%v marker=%#v err=%v", goal["status"], receipt.GoalHold, err)
	}
}

func TestCloudHarnessGoalYieldDurableUntilNativeResult(t *testing.T) {
	h, store, id, _, goal := cloudHarnessGoalFixture(t)
	if err := h.reconcile(id, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.beginGoalYield(id, true); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.GoalHold == nil || !receipt.GoalHold.Yield {
		t.Fatalf("pre-forward yield not durable: marker=%#v err=%v", receipt.GoalHold, err)
	}
	if err := h.completeGoalYield(id, false); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold == nil || receipt.GoalHold.Yield {
		t.Fatalf("failed mutation discarded hold: marker=%#v err=%v", receipt.GoalHold, err)
	}
	if err := h.beginGoalYield(id, true); err != nil {
		t.Fatal(err)
	}
	goal["updatedAt"] = 4
	if err := h.completeGoalYield(id, true); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.GoalHold != nil {
		t.Fatalf("successful mutation retained CQ hold: marker=%#v err=%v", receipt.GoalHold, err)
	}
}
