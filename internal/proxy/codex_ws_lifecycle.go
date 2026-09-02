package proxy

import (
	"context"
	"errors"
	"fmt"

	"github.com/gorilla/websocket"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexWebSocketAdmissionEvidence is bounded provider authority for one exact
// socket generation. ResponseCreated distinguishes an id-less valid
// response.created event from an unauthoritative transport upgrade.
type CodexWebSocketAdmissionEvidence struct {
	DownstreamGeneration uint64
	UpstreamGeneration   uint64
	TurnState            string
	HasTurnState         bool
	ResponseID           string
	ResponseCreated      bool
}

func (evidence CodexWebSocketAdmissionEvidence) validate() error {
	if evidence.DownstreamGeneration == 0 || evidence.UpstreamGeneration == 0 ||
		(!evidence.HasTurnState && !evidence.ResponseCreated) ||
		(!evidence.ResponseCreated && evidence.ResponseID != "") {
		return fmt.Errorf("%w: invalid WebSocket admission evidence", ErrCodexLeaseInvalidMutation)
	}
	return nil
}

type codexWSLifecycleResult struct {
	Kind                          CodexSSEEventKind
	DefinitePreAdmissionRejection bool
	HardUsageLimit                bool
	AuthFailure                   bool
	Terminal                      bool
}

// codexWSLifecycle is durable authority for one exact upstream generation.
// Broker owns serial access and replaces lifecycle only after old generation
// reader has stopped.
type codexWSLifecycle struct {
	handle               *CodexLeaseRequestHandle
	receipt              *codexTurnReceiptHandle
	downstreamGeneration uint64
	upstreamGeneration   uint64
	turnAdmitted         bool
	attemptAdmitted      bool
}

func newCodexWSLifecycle(handle *CodexLeaseRequestHandle, downstreamGeneration, upstreamGeneration uint64, receipts ...*codexTurnReceiptHandle) (*codexWSLifecycle, error) {
	if handle == nil || downstreamGeneration == 0 || upstreamGeneration == 0 {
		return nil, fmt.Errorf("%w: incomplete WebSocket lifecycle", ErrCodexLeaseInvalidMutation)
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || (current.State != CodexAttemptDispatched && current.State != CodexAttemptStreaming) {
		return nil, ErrCodexLeaseTransition
	}
	var receipt *codexTurnReceiptHandle
	if len(receipts) != 0 {
		receipt = receipts[0]
	}
	return &codexWSLifecycle{
		handle:               handle,
		receipt:              receipt,
		downstreamGeneration: downstreamGeneration,
		upstreamGeneration:   upstreamGeneration,
		turnAdmitted:         handle.record.EverAdmitted,
		attemptAdmitted:      current.State == CodexAttemptStreaming,
	}, nil
}

func (lifecycle *codexWSLifecycle) ObserveUpstreamUpgrade(ctx context.Context, upstreamGeneration uint64, turnState string) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	if turnState == "" {
		return nil
	}
	return lifecycle.admit(ctx, CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: lifecycle.downstreamGeneration,
		UpstreamGeneration:   lifecycle.upstreamGeneration,
		TurnState:            turnState,
		HasTurnState:         true,
	})
}

func (lifecycle *codexWSLifecycle) ObserveFrame(ctx context.Context, upstreamGeneration uint64, frame []byte) (codexWSLifecycleResult, error) {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return codexWSLifecycleResult{}, err
	}
	observation := classifyCodexSSEData(frame)
	result := codexWSLifecycleResult{Kind: observation.Kind}
	switch observation.Kind {
	case CodexSSECreated:
		if err := lifecycle.admit(ctx, CodexWebSocketAdmissionEvidence{
			DownstreamGeneration: lifecycle.downstreamGeneration,
			UpstreamGeneration:   lifecycle.upstreamGeneration,
			ResponseID:           observation.ResponseID,
			ResponseCreated:      true,
		}); err != nil {
			return result, err
		}
	case CodexSSEMetadata:
		if observation.TurnState != "" {
			if err := lifecycle.admit(ctx, CodexWebSocketAdmissionEvidence{
				DownstreamGeneration: lifecycle.downstreamGeneration,
				UpstreamGeneration:   lifecycle.upstreamGeneration,
				TurnState:            observation.TurnState,
				HasTurnState:         true,
			}); err != nil {
				return result, err
			}
		}
	case CodexSSECompleted:
		if !lifecycle.attemptAdmitted {
			return result, lifecycle.indeterminateFrame(ctx, frame, codexWSInvalidFrameCompletionOrder, "completion preceded admission")
		}
		completed, err := lifecycle.handle.ProviderCompleted(CodexHTTPCompletionEvidence{
			CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{
				ResponseAnchor:    observation.ResponseID,
				HasResponseAnchor: observation.ResponseID != "",
				HasEncryptedState: observation.HasEncryptedState,
			},
			EndTurn: observation.EndTurn != nil && *observation.EndTurn,
		})
		if err != nil {
			return result, err
		}
		lifecycle.handle = completed
		lifecycle.receipt.terminal(CodexTurnReceiptCompleted)
		result.Terminal = true
	case CodexSSEError:
		if observation.Error.Found {
			result.HardUsageLimit = observation.Error.HardUsageLimit
			result.AuthFailure = observation.Error.AuthFailure
			if !lifecycle.attemptAdmitted {
				result.DefinitePreAdmissionRejection = true
				return result, nil
			}
			if result.HardUsageLimit || result.AuthFailure {
				return result, nil
			}
		}
		if !lifecycle.attemptAdmitted {
			return result, lifecycle.indeterminateFrame(ctx, frame, codexWSInvalidFrameErrorOrder, "error authority was ambiguous")
		}
		failed, err := lifecycle.handle.ProviderFailed(CodexHTTPResponseEvidence{HasEncryptedState: observation.HasEncryptedState})
		if err != nil {
			return result, err
		}
		lifecycle.handle = failed
		lifecycle.receipt.terminal(CodexTurnReceiptFailed)
		result.Terminal = true
	case CodexSSEMalformed, CodexSSEUnknown:
		detail := codexWSInvalidFrameMalformedEvent
		if observation.Kind == CodexSSEUnknown {
			detail = codexWSInvalidFrameUnknownEvent
		}
		return result, lifecycle.indeterminateFrame(ctx, frame, detail, "upstream event was malformed or unknown")
	case CodexSSEDelta:
		if !lifecycle.attemptAdmitted {
			return result, lifecycle.indeterminateFrame(ctx, frame, codexWSInvalidFrameDeltaOrder, "delta preceded admission")
		}
	}
	return result, nil
}

func (lifecycle *codexWSLifecycle) RejectAndPrepare(ctx context.Context, upstreamGeneration uint64, nextSlot uint32) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	if lifecycle.attemptAdmitted {
		return ErrCodexLeaseTransition
	}
	finishTrace := beginCodexTraceLeaseTransition(ctx, "reject_and_prepare", lifecycle.handle)
	handle, err := lifecycle.handle.RejectAndPrepareContext(ctx, nextSlot)
	finishTrace(handle, err)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	return nil
}

func (lifecycle *codexWSLifecycle) RecordAccountUnavailable(ctx context.Context, upstreamGeneration uint64, replacementSlot uint32) error {
	return lifecycle.recordAccountUnavailable(ctx, upstreamGeneration, replacementSlot, false)
}

func (lifecycle *codexWSLifecycle) RecordQuotaExhausted(ctx context.Context, upstreamGeneration uint64, replacementSlot uint32) error {
	return lifecycle.recordAccountUnavailable(ctx, upstreamGeneration, replacementSlot, true)
}

func (lifecycle *codexWSLifecycle) CompleteAccountUnavailableCycle(ctx context.Context, upstreamGeneration uint64) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	finishTrace := beginCodexTraceLeaseTransition(ctx, "complete_account_unavailable", lifecycle.handle)
	handle, err := lifecycle.handle.CompleteAccountUnavailableCycleContext(ctx)
	finishTrace(handle, err)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	return nil
}

func (lifecycle *codexWSLifecycle) recordAccountUnavailable(ctx context.Context, upstreamGeneration uint64, replacementSlot uint32, quotaExhausted bool) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	if lifecycle.attemptAdmitted && replacementSlot != 0 {
		return ErrCodexLeaseTransition
	}
	var (
		handle *CodexLeaseRequestHandle
		err    error
	)
	stage := "account_unavailable"
	if quotaExhausted {
		stage = "quota_exhausted"
	}
	finishTrace := beginCodexTraceLeaseTransition(ctx, stage, lifecycle.handle)
	if quotaExhausted {
		handle, err = lifecycle.handle.RecordQuotaExhaustedContext(ctx, replacementSlot)
	} else {
		handle, err = lifecycle.handle.RecordAccountUnavailableContext(ctx, replacementSlot)
	}
	finishTrace(handle, err)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	if replacementSlot == 0 {
		terminal := CodexTurnReceiptRejected
		if lifecycle.attemptAdmitted {
			terminal = CodexTurnReceiptFailed
		}
		lifecycle.receipt.terminal(terminal)
	}
	return nil
}

func (lifecycle *codexWSLifecycle) replacementSlot(account codex.AccountKey) (uint32, error) {
	if lifecycle == nil || lifecycle.handle == nil || account == "" {
		return 0, ErrCodexLeaseWriterUnavailable
	}
	for index, slotAccount := range lifecycle.handle.slotAccounts {
		if slotAccount != account {
			continue
		}
		slot := uint32(index + 1)
		used := false
		for _, attempt := range lifecycle.handle.record.Attempts {
			if attempt.Slot == slot {
				used = true
				break
			}
		}
		if !used {
			return slot, nil
		}
	}
	return 0, fmt.Errorf("%w: replacement account slot is unavailable", ErrCodexLeaseAuthorityMismatch)
}

func (lifecycle *codexWSLifecycle) FinishRejected(upstreamGeneration uint64) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	if lifecycle.attemptAdmitted {
		return ErrCodexLeaseTransition
	}
	handle, err := lifecycle.handle.FinishRejected()
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	lifecycle.receipt.terminal(CodexTurnReceiptRejected)
	return nil
}

func (lifecycle *codexWSLifecycle) Indeterminate(ctx context.Context, upstreamGeneration uint64) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	return lifecycle.indeterminate(ctx, "upstream outcome was ambiguous")
}

func (lifecycle *codexWSLifecycle) MarkReplacementDispatched(ctx context.Context, upstreamGeneration uint64) error {
	if lifecycle == nil || upstreamGeneration == 0 || upstreamGeneration == lifecycle.upstreamGeneration {
		return ErrCodexWSStaleGeneration
	}
	finishTrace := beginCodexTraceLeaseTransition(ctx, "mark_replacement_dispatched", lifecycle.handle)
	handle, err := lifecycle.handle.MarkDispatchedContext(ctx)
	finishTrace(handle, err)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	lifecycle.upstreamGeneration = upstreamGeneration
	lifecycle.attemptAdmitted = false
	return nil
}

func (lifecycle *codexWSLifecycle) Drain() error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	handle, err := lifecycle.handle.Drain()
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	return nil
}

// cleanupAfterBrokerExit releases every request reference after an abnormal
// post-dispatch broker exit. It also handles terminal requests whose final
// downstream write failed before the response observer could drain.
func (lifecycle *codexWSLifecycle) cleanupAfterBrokerExit(ctx context.Context) error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if lifecycle.handle.record.RoutingRefs == 0 && lifecycle.handle.record.AttemptRefs == 0 && lifecycle.handle.record.ResponseObserverRefs == 0 {
		return nil
	}
	current, ok := codexLeaseAttemptByGeneration(lifecycle.handle.record.Attempts, lifecycle.handle.record.CurrentAttemptGeneration)
	if !ok {
		return ErrCodexLeaseTransition
	}
	if current.State == CodexAttemptDispatched || current.State == CodexAttemptStreaming {
		if ctx == nil {
			ctx = context.Background()
		}
		handle, err := lifecycle.handle.IndeterminateContext(context.WithoutCancel(ctx), CodexHTTPResponseEvidence{})
		if err != nil {
			return err
		}
		lifecycle.handle = handle
		lifecycle.receipt.terminal(CodexTurnReceiptIndeterminate)
	} else if !codexLeaseAttemptTerminalForRequest(current.State) {
		return ErrCodexLeaseTransition
	}
	if lifecycle.handle.record.ResponseObserverRefs > 0 {
		handle, err := lifecycle.handle.Drain()
		if err != nil {
			return err
		}
		lifecycle.handle = handle
	}
	if lifecycle.handle.record.RoutingRefs != 0 || lifecycle.handle.record.AttemptRefs != 0 || lifecycle.handle.record.ResponseObserverRefs != 0 {
		return ErrCodexLeaseTransition
	}
	return nil
}

func (lifecycle *codexWSLifecycle) admit(ctx context.Context, evidence CodexWebSocketAdmissionEvidence) error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	finishTrace := beginCodexTraceLeaseTransition(ctx, "admit_websocket", lifecycle.handle)
	handle, err := lifecycle.handle.AdmitWebSocketContext(ctx, evidence)
	finishTrace(handle, err)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	lifecycle.turnAdmitted = handle.record.EverAdmitted
	lifecycle.attemptAdmitted = true
	return nil
}

func (lifecycle *codexWSLifecycle) indeterminate(ctx context.Context, reason string) error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	handle, err := lifecycle.handle.IndeterminateContext(context.WithoutCancel(ctx), CodexHTTPResponseEvidence{})
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	lifecycle.receipt.terminal(CodexTurnReceiptIndeterminate)
	return fmt.Errorf("%w: %s", ErrCodexWSInvalidFrame, reason)
}

func (lifecycle *codexWSLifecycle) indeterminateFrame(ctx context.Context, frame []byte, detail codexWSInvalidFrameDetail, reason string) error {
	err := lifecycle.indeterminate(ctx, reason)
	if !errors.Is(err, ErrCodexWSInvalidFrame) {
		return err
	}
	return newCodexWSInvalidFrameErrorWithDetail(codexWSInvalidFrameUpstreamResponse, websocket.TextMessage, frame, detail, err)
}

func (lifecycle *codexWSLifecycle) validateGeneration(upstreamGeneration uint64) error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if upstreamGeneration == 0 || upstreamGeneration != lifecycle.upstreamGeneration {
		return ErrCodexWSStaleGeneration
	}
	return nil
}
