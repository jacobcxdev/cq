package dots

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"

	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudHarnessGoal struct {
	ThreadID  string `json:"threadId"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Budget    *int64 `json:"tokenBudget"`
}

func (goal cloudHarnessGoal) fingerprint() string {
	goal.Status, goal.UpdatedAt = "", 0
	sum := sha256.Sum256(cloudHarnessRaw(goal))
	return hex.EncodeToString(sum[:])
}

func (h *cloudHarness) readGoal(id string) (*cloudHarnessGoal, error) {
	result, err := h.call("thread/goal/get", map[string]string{"threadId": id})
	if err != nil {
		return nil, err
	}
	return cloudHarnessGoalResult(id, result)
}

func cloudHarnessGoalResult(id string, result cloudMessage) (*cloudHarnessGoal, error) {
	var goal *cloudHarnessGoal
	if raw := result["goal"]; len(raw) == 0 {
		return nil, nil
	} else if json.Unmarshal(raw, &goal) != nil || goal != nil && (goal.ThreadID != id || goal.Objective == "" || goal.CreatedAt <= 0 || goal.UpdatedAt <= 0) {
		return nil, errors.New("native goal state unavailable")
	}
	return goal, nil
}

// The existing per-chat lease also owns recovery of a durable continuation hold.
func (h *cloudHarness) loadGoalHold(id string) error {
	h.mu.Lock()
	thread := h.threads[id]
	needed := thread != nil && thread.owned && !thread.goalHoldLoaded
	h.mu.Unlock()
	if !needed || h.b.deps.harnessReceipts == nil {
		return nil
	}
	receipt, err := h.b.deps.harnessReceipts.Load(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if receipt.GoalHold != nil {
		return h.acquire(id)
	}
	h.mu.Lock()
	if thread = h.threads[id]; thread != nil {
		thread.goalHoldLoaded = true
	}
	h.mu.Unlock()
	return nil
}

func (h *cloudHarness) saveGoalHold(id string, hold *harness.GoalHold) error {
	h.receipts.Lock()
	defer h.receipts.Unlock()
	receipt, err := h.b.deps.harnessReceipts.Load(id)
	if err != nil {
		return errors.New("harness goal recovery receipt unavailable")
	}
	receipt.GoalHold = hold
	if hold == nil {
		receipt.Issues = slices.DeleteFunc(receipt.Issues, func(issue string) bool { return issue == "native_goal_resume_pending" })
		if receipt.State == "partial" && len(receipt.Issues) == 0 && len(receipt.Unsupported) == 0 {
			receipt.State = "applied"
		}
	}
	if err := h.b.deps.harnessReceipts.Save(receipt); err != nil {
		return errors.New("harness goal recovery receipt could not be saved")
	}
	h.mu.Lock()
	if thread := h.threads[id]; thread != nil {
		thread.goalHold = hold
		thread.goalHoldLoaded = true
		thread.receiptState = receipt.State
		if hold == nil {
			thread.goalHoldExternal = false
		}
	}
	h.mu.Unlock()
	return nil
}

func (h *cloudHarness) holdGoal(id, attempt string) error {
	h.mu.Lock()
	thread := h.threads[id]
	needed := thread != nil && thread.goalHold == nil && thread.goalHoldAttempt != attempt
	h.mu.Unlock()
	if !needed || h.b.deps.harnessReceipts == nil {
		return nil
	}
	goal, err := h.readGoal(id)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	if current := h.threads[id]; current != nil {
		current.goalHoldAttempt = attempt
		if goal != nil {
			current.goalCreatedAt = goal.CreatedAt
		}
	}
	h.mu.Unlock()
	if goal == nil || goal.Status != "active" {
		return nil
	}
	marker := &harness.GoalHold{Fingerprint: goal.fingerprint()}
	// Persist intent first: a broker crash after the RPC must not strand the goal.
	if err := h.saveGoalHold(id, marker); err != nil {
		h.mu.Lock()
		if current := h.threads[id]; current != nil {
			current.goalHoldAttempt = ""
		}
		h.mu.Unlock()
		return err
	}
	result, err := h.call("thread/goal/set", map[string]string{"threadId": id, "status": "paused", "origin": "automatic"})
	if err != nil {
		return errors.New("native goal continuation hold unavailable")
	}
	paused, err := cloudHarnessGoalResult(id, result)
	if err != nil || paused == nil || paused.Status != "paused" || paused.fingerprint() != marker.Fingerprint {
		return errors.New("native goal continuation hold unverified")
	}
	marker = &harness.GoalHold{Fingerprint: marker.Fingerprint, UpdatedAt: paused.UpdatedAt}
	return h.saveGoalHold(id, marker)
}

func (h *cloudHarness) finishGoalHold(id string, force bool) error {
	h.mu.Lock()
	thread := h.threads[id]
	if thread == nil || thread.goalHold == nil || thread.lease == nil || thread.pendingTurns > 0 || thread.pendingSettings > 0 || !force && thread.active {
		h.mu.Unlock()
		return nil
	}
	marker, external := *thread.goalHold, thread.goalHoldExternal
	h.mu.Unlock()
	if marker.Yield || external {
		return h.clearGoalHold(id)
	}
	if marker.UpdatedAt <= 0 {
		if err := h.clearGoalHold(id); err != nil {
			return err
		}
		h.mu.Lock()
		if thread := h.threads[id]; thread != nil {
			thread.goalHoldAttempt = ""
		}
		h.mu.Unlock()
		return nil
	}
	goal, err := h.readGoal(id)
	if err != nil {
		return errors.New("native goal restoration unavailable")
	}
	if goal != nil && goal.Status == "paused" && goal.fingerprint() == marker.Fingerprint && goal.UpdatedAt == marker.UpdatedAt {
		result, err := h.call("thread/goal/set", map[string]string{"threadId": id, "status": "active", "origin": "automatic"})
		if err != nil {
			return errors.New("native goal restoration failed")
		}
		resumed, err := cloudHarnessGoalResult(id, result)
		if err != nil || resumed == nil || resumed.Status != "active" || resumed.fingerprint() != marker.Fingerprint {
			return errors.New("native goal restoration unverified")
		}
	}
	return h.clearGoalHold(id)
}

func (h *cloudHarness) clearGoalHold(id string) error {
	if err := h.saveGoalHold(id, nil); err != nil {
		return err
	}
	h.mu.Lock()
	thread := h.threads[id]
	selection, state := thread.selection, thread.receiptState
	h.mu.Unlock()
	if selection.Source != "" {
		h.event(id, selection, state)
	}
	return nil
}

// h.op serialises user mutations with CQ's own read/restore sequence.
func (h *cloudHarness) beginGoalYield(id string, stop bool) error {
	h.mu.Lock()
	thread := h.threads[id]
	var marker *harness.GoalHold
	if thread != nil && thread.goalHold != nil {
		copy := *thread.goalHold
		copy.Yield = copy.Yield || stop
		marker = &copy
	}
	h.mu.Unlock()
	if marker == nil {
		h.mu.Lock()
		if thread != nil {
			thread.goalHoldAttempt = ""
		}
		h.mu.Unlock()
		return nil
	}
	return h.saveGoalHold(id, marker)
}

func (h *cloudHarness) completeGoalYield(id string, success bool) error {
	h.mu.Lock()
	thread := h.threads[id]
	var marker *harness.GoalHold
	if thread != nil && thread.goalHold != nil {
		copy := *thread.goalHold
		copy.Yield = false
		marker = &copy
	}
	h.mu.Unlock()
	if marker == nil {
		return nil
	}
	if success {
		return h.saveGoalHold(id, nil)
	}
	return h.saveGoalHold(id, marker)
}
