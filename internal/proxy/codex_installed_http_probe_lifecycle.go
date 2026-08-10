package proxy

import (
	"context"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// codexInstalledHTTPProbedLifecycle observes only successful mutations of the
// concrete v2 lifecycle. It does not alter mutation order or error handling.
type codexInstalledHTTPProbedLifecycle struct {
	inner CodexHTTPRequestLifecycle
	trace *codexInstalledHTTPGateTrace
}

func codexInstalledHTTPV2Generations(lifecycle CodexHTTPRequestLifecycle) (uint64, uint64, bool) {
	lease, ok := lifecycle.(*codexLeaseHTTPRequestLifecycle)
	if !ok || lease == nil || lease.handle == nil {
		return 0, 0, false
	}
	requestGeneration := lease.handle.RequestGeneration()
	attemptGeneration := lease.handle.AttemptGeneration()
	return requestGeneration, attemptGeneration, requestGeneration != 0 && attemptGeneration != 0
}

func codexInstalledHTTPV2NewTurn(lifecycle CodexHTTPRequestLifecycle) bool {
	lease, ok := lifecycle.(*codexLeaseHTTPRequestLifecycle)
	return ok && lease != nil && lease.handle != nil && lease.handle.newTurn
}

func (trace *codexInstalledHTTPGateTrace) wrapLifecycle(lifecycle CodexHTTPRequestLifecycle) CodexHTTPRequestLifecycle {
	if trace == nil || lifecycle == nil {
		return lifecycle
	}
	requestGeneration, attemptGeneration, durable := codexInstalledHTTPV2Generations(lifecycle)
	trace.mu.Lock()
	if durable {
		trace.lifecycle.begin = true
		trace.lifecycle.requestGen = requestGeneration
		trace.lifecycle.attemptGen = attemptGeneration
	}
	trace.mu.Unlock()
	return &codexInstalledHTTPProbedLifecycle{inner: lifecycle, trace: trace}
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) EverAdmitted() bool {
	return lifecycle != nil && lifecycle.inner != nil && lifecycle.inner.EverAdmitted()
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) AccountKey() codex.AccountKey {
	if lifecycle == nil || lifecycle.inner == nil {
		return ""
	}
	return lifecycle.inner.AccountKey()
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) next(next CodexHTTPRequestLifecycle, err error, event codexInstalledHTTPTerminalKind, dispatched, drained bool) (CodexHTTPRequestLifecycle, error) {
	if err != nil || next == nil {
		if lifecycle != nil && lifecycle.trace != nil {
			lifecycle.trace.mu.Lock()
			lifecycle.trace.lifecycle.invalid = true
			lifecycle.trace.mu.Unlock()
		}
		return next, err
	}
	requestGeneration, attemptGeneration, durable := codexInstalledHTTPV2Generations(next)
	if lifecycle == nil || lifecycle.trace == nil {
		return next, nil
	}
	trace := lifecycle.trace
	trace.mu.Lock()
	if !durable || requestGeneration < trace.lifecycle.requestGen || attemptGeneration < trace.lifecycle.attemptGen ||
		(event != 0 && trace.lifecycle.terminal != 0) || (drained && trace.lifecycle.terminal == 0) {
		trace.lifecycle.invalid = true
	} else {
		trace.lifecycle.requestGen = requestGeneration
		trace.lifecycle.attemptGen = attemptGeneration
		trace.lifecycle.dispatched = trace.lifecycle.dispatched || dispatched
		if event != 0 {
			trace.lifecycle.terminal = event
		}
		trace.lifecycle.drained = trace.lifecycle.drained || drained
	}
	trace.mu.Unlock()
	return &codexInstalledHTTPProbedLifecycle{inner: next, trace: trace}, nil
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) MarkDispatchedContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.MarkDispatchedContext(ctx)
	return lifecycle.next(next, err, 0, true, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) RejectAndPrepareContext(ctx context.Context, slot uint32) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.RejectAndPrepareContext(ctx, slot)
	return lifecycle.next(next, err, 0, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) AbandonBeforeDispatchContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.AbandonBeforeDispatchContext(ctx)
	return lifecycle.next(next, err, codexInstalledHTTPTerminalIndeterminate, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.FinishRejected()
	return lifecycle.next(next, err, codexInstalledHTTPTerminalRejected, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) IndeterminateContext(ctx context.Context, evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.IndeterminateContext(ctx, evidence)
	return lifecycle.next(next, err, codexInstalledHTTPTerminalIndeterminate, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) Drain() (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.Drain()
	return lifecycle.next(next, err, 0, false, true)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) AdmitHTTP2xxContext(ctx context.Context, evidence CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.AdmitHTTP2xxContext(ctx, evidence)
	return lifecycle.next(next, err, 0, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.ProviderCompleted(evidence)
	return lifecycle.next(next, err, codexInstalledHTTPTerminalCompleted, false, false)
}

func (lifecycle *codexInstalledHTTPProbedLifecycle) ProviderFailed(evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.ProviderFailed(evidence)
	return lifecycle.next(next, err, codexInstalledHTTPTerminalProviderFailed, false, false)
}
