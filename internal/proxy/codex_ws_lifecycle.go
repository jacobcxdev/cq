package proxy

import (
	"context"
	"fmt"
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
			return result, lifecycle.indeterminate(ctx, "completion preceded admission")
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
		if !lifecycle.turnAdmitted && !lifecycle.attemptAdmitted && observation.Error.Found {
			result.DefinitePreAdmissionRejection = true
			result.HardUsageLimit = observation.Error.HardUsageLimit
			result.AuthFailure = observation.Error.AuthFailure
			return result, nil
		}
		if !lifecycle.attemptAdmitted {
			return result, lifecycle.indeterminate(ctx, "error authority was ambiguous")
		}
		failed, err := lifecycle.handle.ProviderFailed(CodexHTTPResponseEvidence{HasEncryptedState: observation.HasEncryptedState})
		if err != nil {
			return result, err
		}
		lifecycle.handle = failed
		lifecycle.receipt.terminal(CodexTurnReceiptFailed)
		result.Terminal = true
	case CodexSSEMalformed, CodexSSEUnknown:
		return result, lifecycle.indeterminate(ctx, "upstream event was malformed or unknown")
	case CodexSSEDelta:
		if !lifecycle.attemptAdmitted {
			return result, lifecycle.indeterminate(ctx, "delta preceded admission")
		}
	}
	return result, nil
}

func (lifecycle *codexWSLifecycle) RejectAndPrepare(ctx context.Context, upstreamGeneration uint64, nextSlot uint32) error {
	if err := lifecycle.validateGeneration(upstreamGeneration); err != nil {
		return err
	}
	if lifecycle.turnAdmitted || lifecycle.attemptAdmitted {
		return ErrCodexLeaseTransition
	}
	handle, err := lifecycle.handle.RejectAndPrepareContext(ctx, nextSlot)
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	return nil
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
	handle, err := lifecycle.handle.MarkDispatchedContext(ctx)
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

func (lifecycle *codexWSLifecycle) admit(ctx context.Context, evidence CodexWebSocketAdmissionEvidence) error {
	if lifecycle == nil || lifecycle.handle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	handle, err := lifecycle.handle.AdmitWebSocketContext(ctx, evidence)
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
	handle, err := lifecycle.handle.IndeterminateContext(ctx, CodexHTTPResponseEvidence{})
	if err != nil {
		return err
	}
	lifecycle.handle = handle
	lifecycle.receipt.terminal(CodexTurnReceiptIndeterminate)
	return fmt.Errorf("%w: %s", ErrCodexWSInvalidFrame, reason)
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
