package dots

import (
	"errors"

	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudHarnessRPCError struct {
	reason string
	code   *int
}

func (failure *cloudHarnessRPCError) Error() string {
	switch failure.reason {
	case "rpc_rejected":
		return "native harness request rejected"
	case "rpc_timeout":
		return "native harness request timed out"
	default:
		return "native harness request failed"
	}
}

type cloudHarnessSubscriptionError struct{ cause, rollback error }

func (failure *cloudHarnessSubscriptionError) Error() string {
	return "native harness could not release this chat subscription"
}
func (failure *cloudHarnessSubscriptionError) Unwrap() error { return failure.cause }

func cloudHarnessFailureReason(err error) (string, *int) {
	var native *cloudHarnessRPCError
	if errors.As(err, &native) {
		return native.reason, native.code
	}
	return "internal", nil
}

func (h *cloudHarness) recordFailure(id string, selection harness.Selection, stage string, err error, rollback, rollbackStage string, rollbackErr error) {
	reason, code := cloudHarnessFailureReason(err)
	failure := &harness.ApplicationFailure{Stage: stage, Reason: reason, NativeCode: code, Rollback: rollback}
	if rollbackErr != nil {
		failure.RollbackStage = rollbackStage
		failure.RollbackReason, failure.RollbackNativeCode = cloudHarnessFailureReason(rollbackErr)
	}
	h.mu.Lock()
	if thread := h.threads[id]; thread != nil {
		thread.failure = failure
	}
	h.mu.Unlock()
	_ = h.record(id, selection, "failed")
}
