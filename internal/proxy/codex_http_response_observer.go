package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

type codexHTTPResponseMode uint8

const (
	codexHTTPResponseModeSSE codexHTTPResponseMode = iota + 1
	codexHTTPResponseModeCompact
)

var (
	errCodexHTTPResponseMalformed           = errors.New("Codex accepted response is malformed")
	errCodexHTTPResponseNoTerminal          = errors.New("Codex accepted response has no typed terminal evidence")
	errCodexHTTPResponseConflictingTerminal = errors.New("Codex accepted response has conflicting terminal evidence")
	errCodexHTTPResponseConflictingAnchor   = errors.New("Codex accepted response has conflicting response anchors")
	errCodexHTTPResponseCompactTooLarge     = errors.New("Codex compact response exceeds limit")
	errCodexHTTPResponseInvalidMode         = errors.New("Codex accepted response mode is invalid")
	errCodexHTTPResponseUnavailable         = errors.New("Codex accepted response lifecycle is unavailable")
	errCodexHTTPResponseNilHandle           = errors.New("Codex accepted response lifecycle returned a nil handle")
)

type codexHTTPResponseOutcome uint8

const (
	codexHTTPResponseOutcomeUnknown codexHTTPResponseOutcome = iota
	codexHTTPResponseOutcomeCompleted
	codexHTTPResponseOutcomeFailed
)

// codexHTTPObservedResponseEvidence retains raw provider continuity values only
// until the lifecycle hashes them. It must never be logged or persisted here.
type codexHTTPObservedResponseEvidence struct {
	responseAnchor    string
	hasResponseAnchor bool
	hasEncryptedState bool
}

// codexHTTPResponseObserver delays one terminal mutation until the body has
// reached a clean EOF and has been closed. This lets transport uncertainty
// override terminal-looking bytes without changing the bytes relayed downstream.
type codexHTTPResponseObserver struct {
	mu         sync.Mutex
	finishOnce sync.Once

	cleanup   context.Context
	mode      codexHTTPResponseMode
	lifecycle CodexHTTPRequestLifecycle
	parser    *CodexSSEParser
	compact   []byte
	overflow  bool
	finished  bool

	outcome     codexHTTPResponseOutcome
	endTurn     bool
	evidence    codexHTTPObservedResponseEvidence
	evidenceErr error
	finishErr   error
}

func newCodexHTTPResponseObserver(ctx context.Context, mode codexHTTPResponseMode, lifecycle CodexHTTPRequestLifecycle) *codexHTTPResponseObserver {
	if ctx == nil {
		ctx = context.Background()
	}
	observer := &codexHTTPResponseObserver{
		cleanup:   context.WithoutCancel(ctx),
		mode:      mode,
		lifecycle: lifecycle,
	}
	if mode == codexHTTPResponseModeSSE {
		observer.parser = NewCodexSSEParser(0)
	}
	return observer
}

// Observe consumes a borrowed byte slice. It never retains or changes the
// relayed slice, and calls arriving after Finish are ignored.
func (observer *codexHTTPResponseObserver) Observe(chunk []byte) {
	if observer == nil || len(chunk) == 0 {
		return
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.finished {
		return
	}
	switch observer.mode {
	case codexHTTPResponseModeSSE:
		observer.observeSSELocked(chunk)
	case codexHTTPResponseModeCompact:
		if observer.overflow {
			return
		}
		var ok bool
		observer.compact, ok = appendCodexObservationBody(observer.compact, chunk, codexProtocolMaxBytes)
		if !ok {
			observer.compact = nil
			observer.overflow = true
		}
	}
}

func (observer *codexHTTPResponseObserver) observeSSELocked(chunk []byte) {
	if observer.parser == nil || observer.evidenceErr != nil {
		return
	}
	observations, err := observer.parser.Feed(chunk)
	for _, observation := range observations {
		observer.observeSSEEventLocked(observation)
		if observer.evidenceErr != nil {
			break
		}
	}
	if observer.evidenceErr == nil && err != nil {
		observer.evidenceErr = errors.Join(errCodexHTTPResponseMalformed, err)
	}
}

func (observer *codexHTTPResponseObserver) observeSSEEventLocked(observation CodexSSEObservation) {
	if observation.Kind == CodexSSEMalformed {
		observer.evidenceErr = errCodexHTTPResponseMalformed
		return
	}
	if observation.HasEncryptedState {
		observer.evidence.hasEncryptedState = true
	}
	if observation.ResponseID != "" {
		if len(observation.ResponseID) > codexTurnIDMaxBytes {
			observer.evidenceErr = errCodexHTTPResponseMalformed
			return
		}
		if observer.evidence.hasResponseAnchor && observer.evidence.responseAnchor != observation.ResponseID {
			observer.evidenceErr = errCodexHTTPResponseConflictingAnchor
			return
		}
		observer.evidence.responseAnchor = observation.ResponseID
		observer.evidence.hasResponseAnchor = true
	}
	switch observation.Kind {
	case CodexSSECompleted:
		endTurn := true
		if observation.EndTurn != nil {
			endTurn = *observation.EndTurn
		}
		observer.recordTerminalLocked(codexHTTPResponseOutcomeCompleted, endTurn)
	case CodexSSEError:
		observer.recordTerminalLocked(codexHTTPResponseOutcomeFailed, false)
	}
}

func (observer *codexHTTPResponseObserver) recordTerminalLocked(outcome codexHTTPResponseOutcome, endTurn bool) {
	if observer.outcome != codexHTTPResponseOutcomeUnknown {
		observer.evidenceErr = errCodexHTTPResponseConflictingTerminal
		return
	}
	observer.outcome = outcome
	observer.endTurn = endTurn
}

// Finish classifies the complete body once, records one terminal outcome, then
// drains exactly one response-observer reference. The first call wins.
func (observer *codexHTTPResponseObserver) Finish(cause error) error {
	if observer == nil {
		return errors.Join(cause, errCodexHTTPResponseUnavailable)
	}
	observer.finishOnce.Do(func() {
		observer.finish(cause)
	})
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.finishErr
}

func (observer *codexHTTPResponseObserver) finish(cause error) {
	observer.mu.Lock()
	observer.finished = true
	if cause == nil {
		observer.finaliseEvidenceLocked()
	}
	outcome := observer.outcome
	endTurn := observer.endTurn
	evidence := observer.evidence
	evidenceErr := observer.evidenceErr
	lifecycle := observer.lifecycle
	observer.compact = nil
	observer.parser = nil
	observer.evidence = codexHTTPObservedResponseEvidence{}
	observer.mu.Unlock()
	responseEvidence := CodexHTTPResponseEvidence{
		ResponseAnchor:    evidence.responseAnchor,
		HasResponseAnchor: evidence.hasResponseAnchor,
		HasEncryptedState: evidence.hasEncryptedState,
	}

	resultErr := errors.Join(cause, evidenceErr)
	if cause != nil || evidenceErr != nil {
		outcome = codexHTTPResponseOutcomeUnknown
	}
	if lifecycle == nil {
		observer.storeFinishError(errors.Join(resultErr, errCodexHTTPResponseUnavailable))
		return
	}

	var next CodexHTTPRequestLifecycle
	var terminalErr error
	switch outcome {
	case codexHTTPResponseOutcomeCompleted:
		next, terminalErr = lifecycle.ProviderCompleted(CodexHTTPCompletionEvidence{
			CodexHTTPResponseEvidence: responseEvidence,
			EndTurn:                   endTurn,
		})
	case codexHTTPResponseOutcomeFailed:
		next, terminalErr = lifecycle.ProviderFailed(responseEvidence)
	default:
		next, terminalErr = lifecycle.IndeterminateContext(observer.cleanup, responseEvidence)
	}
	if terminalErr == nil {
		if next == nil {
			terminalErr = errCodexHTTPResponseNilHandle
		} else {
			lifecycle = next
		}
	}
	_, drainErr := lifecycle.Drain()
	observer.storeFinishError(errors.Join(resultErr, terminalErr, drainErr))
}

func (observer *codexHTTPResponseObserver) finaliseEvidenceLocked() {
	switch observer.mode {
	case codexHTTPResponseModeSSE:
		if observer.evidenceErr == nil && observer.parser != nil {
			observations, err := observer.parser.Finish()
			for _, observation := range observations {
				observer.observeSSEEventLocked(observation)
				if observer.evidenceErr != nil {
					break
				}
			}
			if observer.evidenceErr == nil && err != nil {
				observer.evidenceErr = errors.Join(errCodexHTTPResponseMalformed, err)
			}
		}
		if observer.evidenceErr == nil && observer.outcome == codexHTTPResponseOutcomeUnknown {
			observer.evidenceErr = errCodexHTTPResponseNoTerminal
		}
	case codexHTTPResponseModeCompact:
		if observer.overflow {
			observer.evidenceErr = errCodexHTTPResponseCompactTooLarge
			return
		}
		compact, err := ParseCodexCompactResponse(observer.compact)
		if err != nil {
			observer.evidenceErr = errCodexHTTPResponseMalformed
			return
		}
		observer.evidence.hasEncryptedState = compact.HasEncryptedState
		if len(compact.ResponseID) > codexTurnIDMaxBytes {
			observer.evidenceErr = errCodexHTTPResponseMalformed
			return
		}
		if compact.ResponseID != "" {
			observer.evidence.responseAnchor = compact.ResponseID
			observer.evidence.hasResponseAnchor = true
		}
		observer.outcome = codexHTTPResponseOutcomeCompleted
		observer.endTurn = true
	default:
		observer.evidenceErr = errCodexHTTPResponseInvalidMode
	}
}

func (observer *codexHTTPResponseObserver) storeFinishError(err error) {
	observer.mu.Lock()
	observer.finishErr = err
	observer.mu.Unlock()
}

// relayCodexAcceptedHTTPResponse consumes response.Body. The body is closed
// before terminal persistence and Drain so durable socket extinction never
// precedes release of the live upstream response.
func relayCodexAcceptedHTTPResponse(ctx context.Context, writer http.ResponseWriter, response *http.Response, mode codexHTTPResponseMode, lifecycle CodexHTTPRequestLifecycle) error {
	if ctx == nil {
		ctx = context.Background()
	}
	observer := newCodexHTTPResponseObserver(ctx, mode, lifecycle)
	if response == nil {
		return observer.Finish(errors.New("Codex accepted response is nil"))
	}
	if response.Body == nil {
		return observer.Finish(errors.New("Codex accepted response body is nil"))
	}
	if writer == nil {
		closeErr := response.Body.Close()
		return observer.Finish(errors.Join(errors.New("Codex accepted response writer is nil"), closeErr))
	}
	for key, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	relayErr := relayCodexObservedHTTPBody(ctx, writer, response.Body, mode == codexHTTPResponseModeSSE, observer.Observe)
	closeErr := response.Body.Close()
	return observer.Finish(errors.Join(relayErr, closeErr, ctx.Err()))
}

func relayCodexObservedHTTPBody(ctx context.Context, writer http.ResponseWriter, body io.Reader, flush bool, observe func([]byte)) error {
	flusher, canFlush := writer.(http.Flusher)
	buffer := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := body.Read(buffer)
		if read > 0 {
			observe(buffer[:read])
			written, writeErr := writer.Write(buffer[:read])
			if writeErr == nil && written != read {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				return writeErr
			}
			if flush && canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
