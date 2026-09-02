package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"syscall"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexHTTPAttemptDispatcher resolves one exact candidate, crosses the durable
// dispatch fence, then sends one already-frozen request. dispatched is true
// only after markDispatched succeeds and before RoundTrip begins.
type CodexHTTPAttemptDispatcher interface {
	DispatchFrozen(
		context.Context,
		RouteChoice,
		CandidateAttempt,
		*http.Request,
		func(CandidateAttempt) error,
	) (*http.Response, CandidateAttempt, bool, error)
}

// CodexHTTPAdmissionEvidence is validated provider response metadata that must
// be committed atomically with accepted-header admission.
type CodexHTTPAdmissionEvidence struct {
	TurnState    string
	HasTurnState bool
}

// CodexHTTPResponseEvidence is validated provider response metadata retained
// monotonically even when a later read or parse outcome is indeterminate.
type CodexHTTPResponseEvidence struct {
	ResponseAnchor    string
	HasResponseAnchor bool
	HasEncryptedState bool
}

// CodexHTTPCompletionEvidence is terminal response evidence plus the provider's
// sampling disposition. EndTurn describes this response, not whole-turn life.
type CodexHTTPCompletionEvidence struct {
	CodexHTTPResponseEvidence
	EndTurn bool
}

type codexHTTPTransportFailureReason string

const (
	codexHTTPTransportFailureUnknown          codexHTTPTransportFailureReason = "unknown"
	codexHTTPTransportFailureCancelled        codexHTTPTransportFailureReason = "cancelled"
	codexHTTPTransportFailureDeadline         codexHTTPTransportFailureReason = "deadline"
	codexHTTPTransportFailureTimeout          codexHTTPTransportFailureReason = "timeout"
	codexHTTPTransportFailureDNS              codexHTTPTransportFailureReason = "dns"
	codexHTTPTransportFailureConnect          codexHTTPTransportFailureReason = "connect"
	codexHTTPTransportFailureConnectionReset  codexHTTPTransportFailureReason = "connection_reset"
	codexHTTPTransportFailureBrokenPipe       codexHTTPTransportFailureReason = "broken_pipe"
	codexHTTPTransportFailureServerClosedIdle codexHTTPTransportFailureReason = "server_closed_idle"
	codexHTTPTransportFailureUnexpectedEOF    codexHTTPTransportFailureReason = "unexpected_eof"
	codexHTTPTransportFailureEOF              codexHTTPTransportFailureReason = "eof"
	codexHTTPTransportFailureTLS              codexHTTPTransportFailureReason = "tls"
	codexHTTPTransportFailureProtocol         codexHTTPTransportFailureReason = "protocol"
)

type codexHTTPTransportFacts struct {
	GotConn              bool
	ConnReused           bool
	ConnWasIdle          bool
	IdleMS               int64
	WroteRequest         bool
	WriteError           bool
	GotFirstResponseByte bool
}

type codexHTTPRoundTripError struct {
	reason codexHTTPTransportFailureReason
	facts  codexHTTPTransportFacts
	cause  error
}

func (failure *codexHTTPRoundTripError) Error() string { return "Codex HTTP round trip failed" }

func (failure *codexHTTPRoundTripError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

type codexHTTPTransportTrace struct {
	mu    sync.Mutex
	facts codexHTTPTransportFacts
}

func (trace *codexHTTPTransportTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			trace.mu.Lock()
			trace.facts.GotConn = true
			trace.facts.ConnReused = info.Reused
			trace.facts.ConnWasIdle = info.WasIdle
			trace.facts.IdleMS = boundedCodexHTTPIdleMilliseconds(info.IdleTime)
			trace.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			trace.mu.Lock()
			trace.facts.WroteRequest = trace.facts.WroteRequest || info.Err == nil
			trace.facts.WriteError = trace.facts.WriteError || info.Err != nil
			trace.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			trace.mu.Lock()
			trace.facts.GotFirstResponseByte = true
			trace.mu.Unlock()
		},
	}
}

func (trace *codexHTTPTransportTrace) snapshot() codexHTTPTransportFacts {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.facts
}

func boundedCodexHTTPIdleMilliseconds(idle time.Duration) int64 {
	const maximum = int64((10 * time.Minute) / time.Millisecond)
	milliseconds := idle.Milliseconds()
	if milliseconds < 0 {
		return 0
	}
	if milliseconds > maximum {
		return maximum
	}
	return milliseconds
}

func newCodexHTTPRoundTripError(cause error, facts codexHTTPTransportFacts) error {
	return &codexHTTPRoundTripError{
		reason: classifyCodexHTTPTransportFailure(cause, facts),
		facts:  facts,
		cause:  cause,
	}
}

func classifyCodexHTTPTransportFailure(cause error, facts codexHTTPTransportFacts) codexHTTPTransportFailureReason {
	if errors.Is(cause, context.Canceled) {
		return codexHTTPTransportFailureCancelled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return codexHTTPTransportFailureDeadline
	}
	var dnsErr *net.DNSError
	if errors.As(cause, &dnsErr) {
		return codexHTTPTransportFailureDNS
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(cause, &timeoutErr) && timeoutErr.Timeout() {
		return codexHTTPTransportFailureTimeout
	}
	var netErr *net.OpError
	if errors.As(cause, &netErr) && (netErr.Op == "dial" || netErr.Op == "connect") {
		return codexHTTPTransportFailureConnect
	}
	if errors.Is(cause, syscall.ECONNRESET) {
		return codexHTTPTransportFailureConnectionReset
	}
	if errors.Is(cause, syscall.EPIPE) {
		return codexHTTPTransportFailureBrokenPipe
	}
	if facts.GotConn && facts.ConnReused && facts.ConnWasIdle && !facts.WroteRequest && !facts.WriteError {
		return codexHTTPTransportFailureServerClosedIdle
	}
	if errors.Is(cause, io.ErrUnexpectedEOF) {
		return codexHTTPTransportFailureUnexpectedEOF
	}
	if errors.Is(cause, io.EOF) {
		return codexHTTPTransportFailureEOF
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(cause, &tlsErr) {
		return codexHTTPTransportFailureTLS
	}
	var protocolErr *http.ProtocolError
	if errors.As(cause, &protocolErr) {
		return codexHTTPTransportFailureProtocol
	}
	return codexHTTPTransportFailureUnknown
}

// DispatchFrozen resolves one exact credential and lets the durable lifecycle
// cross its dispatch fence immediately before RoundTrip.
func (executor *CodexAttemptExecutor) DispatchFrozen(
	ctx context.Context,
	choice RouteChoice,
	attempt CandidateAttempt,
	request *http.Request,
	markDispatched func(CandidateAttempt) error,
) (*http.Response, CandidateAttempt, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	material, actual, err := executor.resolveAttempt(ctx, choice, attempt)
	if err != nil {
		return nil, actual, false, err
	}
	if request == nil {
		return nil, actual, false, errors.New("Codex request is nil")
	}
	out, err := cloneCodexTransportRequest(request.WithContext(ctx))
	if err != nil {
		return nil, actual, false, err
	}
	releaseBody := true
	defer func() {
		if releaseBody && out.Body != nil {
			_ = out.Body.Close()
		}
	}()
	applyCodexTransportCredentials(out, material)
	if markDispatched == nil {
		return nil, actual, false, errors.New("Codex dispatch fence unavailable")
	}
	if err := markDispatched(actual); err != nil {
		return nil, actual, false, err
	}
	transportTrace := &codexHTTPTransportTrace{}
	out = out.WithContext(httptrace.WithClientTrace(out.Context(), transportTrace.clientTrace()))
	response, err := executor.Transport.inner().RoundTrip(out)
	if err != nil {
		err = newCodexHTTPRoundTripError(err, transportTrace.snapshot())
	}
	if err == nil {
		releaseBody = false
	}
	return response, actual, true, err
}

// CodexHTTPRequestLifecycle is the narrow immutable-handle boundary required
// by one HTTP request session. Each successful mutation returns its latest
// durable handle for the next mutation or response observer.
type CodexHTTPRequestLifecycle interface {
	EverAdmitted() bool
	AccountKey() codex.AccountKey
	MarkDispatchedContext(context.Context) (CodexHTTPRequestLifecycle, error)
	RejectAndPrepareContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error)
	RecordAccountUnavailableContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error)
	RecordQuotaExhaustedContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error)
	CompleteAccountUnavailableCycleContext(context.Context) (CodexHTTPRequestLifecycle, error)
	AbandonBeforeDispatchContext(context.Context) (CodexHTTPRequestLifecycle, error)
	FinishRejected() (CodexHTTPRequestLifecycle, error)
	IndeterminateContext(context.Context, CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error)
	Drain() (CodexHTTPRequestLifecycle, error)
	AdmitHTTP2xxContext(context.Context, CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error)
	ProviderCompleted(CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error)
	ProviderFailed(CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error)
}

type codexLeaseHTTPRequestLifecycle struct {
	handle *CodexLeaseRequestHandle
}

var _ CodexHTTPRequestLifecycle = (*codexLeaseHTTPRequestLifecycle)(nil)

// NewCodexHTTPRequestLifecycle adapts one durable immutable lease handle to the
// HTTP session boundary while preserving the fresh handle after each mutation.
func NewCodexHTTPRequestLifecycle(handle *CodexLeaseRequestHandle) CodexHTTPRequestLifecycle {
	if handle == nil {
		return nil
	}
	return &codexLeaseHTTPRequestLifecycle{handle: handle}
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) EverAdmitted() bool {
	return lifecycle != nil && lifecycle.handle.EverAdmitted()
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) AccountKey() codex.AccountKey {
	if lifecycle == nil {
		return ""
	}
	return lifecycle.handle.AccountKey()
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) next(handle *CodexLeaseRequestHandle, err error) (CodexHTTPRequestLifecycle, error) {
	if err != nil {
		return nil, err
	}
	return NewCodexHTTPRequestLifecycle(handle), nil
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) MarkDispatchedContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "mark_dispatched", lifecycle.handle)
	handle, err := lifecycle.handle.MarkDispatchedContext(ctx)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) RejectAndPrepareContext(ctx context.Context, slot uint32) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "reject_and_prepare", lifecycle.handle)
	handle, err := lifecycle.handle.RejectAndPrepareContext(ctx, slot)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) RecordAccountUnavailableContext(ctx context.Context, replacementSlot uint32) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "account_unavailable", lifecycle.handle)
	handle, err := lifecycle.handle.RecordAccountUnavailableContext(ctx, replacementSlot)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) RecordQuotaExhaustedContext(ctx context.Context, replacementSlot uint32) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "quota_exhausted", lifecycle.handle)
	handle, err := lifecycle.handle.RecordQuotaExhaustedContext(ctx, replacementSlot)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) CompleteAccountUnavailableCycleContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "complete_account_unavailable", lifecycle.handle)
	handle, err := lifecycle.handle.CompleteAccountUnavailableCycleContext(ctx)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) AbandonBeforeDispatchContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "abandon_before_dispatch", lifecycle.handle)
	handle, err := lifecycle.handle.AbandonBeforeDispatchContext(ctx)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	handle, err := lifecycle.handle.FinishRejected()
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) IndeterminateContext(ctx context.Context, evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "indeterminate", lifecycle.handle)
	handle, err := lifecycle.handle.IndeterminateContext(ctx, evidence)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) Drain() (CodexHTTPRequestLifecycle, error) {
	handle, err := lifecycle.handle.Drain()
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) AdmitHTTP2xxContext(ctx context.Context, evidence CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	finishTrace := beginCodexTraceLeaseTransition(ctx, "admit_http", lifecycle.handle)
	handle, err := lifecycle.handle.AdmitHTTP2xxContext(ctx, evidence)
	finishTrace(handle, err)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	handle, err := lifecycle.handle.ProviderCompleted(evidence)
	return lifecycle.next(handle, err)
}

func (lifecycle *codexLeaseHTTPRequestLifecycle) ProviderFailed(evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	handle, err := lifecycle.handle.ProviderFailed(evidence)
	return lifecycle.next(handle, err)
}

// CodexHTTPRequestSession owns bounded retry and response retention for one
// frozen native Responses request.
type CodexHTTPRequestSession struct {
	Executor  CodexHTTPAttemptDispatcher
	Refresher codex.CredentialReferenceRefresher
	Capacity  *CodexCapacityLedger
}

// CodexHTTPRequestSessionResult transfers response ownership and the latest
// durable lifecycle handle to the relay or response observer.
type CodexHTTPRequestSessionResult struct {
	Response  *http.Response
	Choice    RouteChoice
	Attempt   CandidateAttempt
	Lifecycle CodexHTTPRequestLifecycle
}

// CodexHTTPAttemptSlotPlan is one raw-free bridge entry for the durable lease
// request envelope. Index is one-based and stable for the frozen plan.
type CodexHTTPAttemptSlotPlan struct {
	Index       uint32
	AccountKey  codex.AccountKey
	CandidateID codex.CandidateID
	Kind        CodexAttemptSlotKind
}

// CodexHTTPAttemptSlots projects the exact direct and eligible-refresh order
// consumed by Do into durable request-envelope slots.
func CodexHTTPAttemptSlots(plan CodexFrozenDispatchPlan) []CodexHTTPAttemptSlotPlan {
	accounts, _ := codexHTTPRequestDispatchAccounts(plan)
	accountSlots := codexHTTPRequestAccountSlotMap(accounts)
	var slots []CodexHTTPAttemptSlotPlan
	for accountIndex, account := range accounts {
		for attemptIndex, attempt := range account.Attempts() {
			slots = append(slots, CodexHTTPAttemptSlotPlan{
				Index:       accountSlots[accountIndex].direct[attemptIndex],
				AccountKey:  attempt.AccountKey,
				CandidateID: attempt.Candidate.CandidateID,
				Kind:        CodexAttemptSlotDirect,
			})
		}
		if refresh, ok := account.RefreshAttempt(); ok {
			slots = append(slots, CodexHTTPAttemptSlotPlan{
				Index:       accountSlots[accountIndex].refresh,
				AccountKey:  refresh.AccountKey,
				CandidateID: refresh.Candidate.CandidateID,
				Kind:        CodexAttemptSlotEligibleManagedRefresh,
			})
		}
	}
	return slots
}

// Do executes one frozen plan. It always releases frozen request ownership.
func (session *CodexHTTPRequestSession) Do(
	ctx context.Context,
	template *http.Request,
	plan CodexFrozenDispatchPlan,
	frozen *CodexFrozenRequest,
	lifecycle CodexHTTPRequestLifecycle,
) (result CodexHTTPRequestSessionResult, returnErr error) {
	result.Lifecycle = lifecycle
	if frozen != nil {
		defer frozen.Release()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if session == nil || session.Executor == nil || template == nil || template.URL == nil || frozen == nil || lifecycle == nil {
		return result, errors.New("Codex HTTP request session unavailable")
	}
	accounts, ordinaryAccountCount := codexHTTPRequestDispatchAccounts(plan)
	if ordinaryAccountCount == 0 {
		return session.abandonPrepared(ctx, result, plan.TerminalError())
	}
	frozenChoice, err := frozen.Choice()
	if err != nil {
		return session.abandonPrepared(ctx, result, err)
	}
	firstChoice := accounts[0].Choice()
	if frozenChoice.AccountKey != firstChoice.AccountKey || !codexRoutePolicyCompatibleWithFrozen(frozenChoice, firstChoice) {
		return session.abandonPrepared(ctx, result, errors.New("Codex frozen request does not match dispatch plan"))
	}
	var retention CodexRejectedResponseRetention
	defer retention.Release()
	var retainedChoice RouteChoice
	var retainedAttempt CandidateAttempt
	defaultRetained := false

	accountSlots := codexHTTPRequestAccountSlotMap(accounts)
accountsLoop:
	for accountIndex, account := range accounts {
		choice := account.Choice()
		emitCodexTrace(ctx, CodexTraceEvent{
			Phase: "account", Outcome: "considered", AccountHint: codexTraceAccountHint(choice.AccountKey),
			Attempt: accountIndex + 1, Failover: accountIndex > 0,
		})
		attempts := account.Attempts()
		decisionRecorded := false
		directAttempts := len(attempts)
		refreshPlanned, hasRefresh := account.RefreshAttempt()
		refreshConsidered := false
		if len(attempts) == 0 && hasRefresh {
			emitCodexTrace(ctx, CodexTraceEvent{Phase: "credential_refresh", Outcome: "started", AccountHint: codexTraceAccountHint(choice.AccountKey)})
			refreshConsidered = true
			result.Choice = choice
			result.Attempt = refreshPlanned
			var refreshErr error
			if session.Refresher == nil {
				refreshErr = codex.ErrRefreshUnavailable
			} else {
				ref, revision, err := session.Refresher.RefreshReference(ctx, refreshPlanned.Candidate, refreshPlanned.Revision)
				if err != nil {
					refreshErr = err
				} else {
					refreshed, validationErr := candidateAttemptWithRefreshedRevision(refreshPlanned, ref, revision)
					if validationErr != nil {
						refreshErr = validationErr
					} else {
						refreshed.Ordinal = 1
						attempts = append(attempts, refreshed)
					}
				}
			}
			if refreshErr != nil {
				emitCodexTrace(ctx, CodexTraceEvent{Phase: "credential_refresh", Outcome: "error", AccountHint: codexTraceAccountHint(choice.AccountKey), Reason: string(codexRequestFailureReason(refreshErr))})
				nextAccountIndex, hasReplacement := codexHTTPRequestNextUnavailableAccount(
					accountIndex,
					ordinaryAccountCount,
					len(accounts),
					result.Lifecycle.EverAdmitted(),
				)
				replacementSlot := uint32(0)
				if hasReplacement {
					replacementSlot = codexHTTPRequestFirstAccountSlot(accountSlots[nextAccountIndex])
				}
				if !hasReplacement && !codexHTTPRequestCanRecordAccountUnavailable(plan, result.Lifecycle) {
					return session.abandonPrepared(ctx, result, refreshErr)
				}
				next, unavailableErr := codexHTTPRequestRecordAccountUnavailable(ctx, result.Lifecycle, replacementSlot, false)
				if unavailableErr != nil {
					return session.finishIndeterminate(ctx, result, errors.Join(refreshErr, unavailableErr))
				}
				result.Lifecycle = next
				if hasReplacement {
					emitCodexTrace(ctx, CodexTraceEvent{Phase: "failover", Outcome: "scheduled", AccountHint: codexTraceAccountHint(accounts[nextAccountIndex].Choice().AccountKey), Reason: "credential_refresh_failed", Retry: true, Failover: true})
					continue accountsLoop
				}
				return result, refreshErr
			}
		}
		for attemptIndex := 0; attemptIndex < len(attempts); attemptIndex++ {
			attempt := attempts[attemptIndex]
			emitCodexTrace(ctx, CodexTraceEvent{
				Phase: "attempt", Stage: "prepare", Outcome: "started", AccountHint: codexTraceAccountHint(choice.AccountKey),
				Attempt: attempt.Ordinal, Retry: attemptIndex > 0 || accountIndex > 0, Failover: accountIndex > 0,
			})
			result.Choice = choice
			result.Attempt = attempt
			if result.Lifecycle.AccountKey() != choice.AccountKey {
				return session.abandonPrepared(ctx, result, errors.New("Codex HTTP lifecycle account does not match frozen attempt"))
			}
			replay, request, err := codexHTTPRequestFromFrozen(ctx, template, frozen)
			if err != nil {
				return session.abandonPrepared(ctx, result, err)
			}
			probeAttempt := codexInstalledHTTPTraceFromContext(ctx).prepareAttempt(replay, uint32(accountIndex+1))
			marked := false
			response, actual, dispatched, err := session.Executor.DispatchFrozen(ctx, choice, attempt, request, func(CandidateAttempt) error {
				if marked {
					return errors.New("Codex HTTP attempt crossed dispatch fence twice")
				}
				next, markErr := result.Lifecycle.MarkDispatchedContext(ctx)
				if markErr != nil {
					return markErr
				}
				result.Lifecycle = next
				marked = true
				emitCodexTrace(ctx, CodexTraceEvent{
					Phase: "attempt", Stage: "dispatch", Outcome: "dispatched", AccountHint: codexTraceAccountHint(choice.AccountKey),
					Attempt: attempt.Ordinal, Retry: attemptIndex > 0 || accountIndex > 0, Failover: accountIndex > 0,
				})
				if !decisionRecorded {
					codexProcessRuntimeObservability.recordDecision(ctx, account.decision)
					decisionRecorded = true
				}
				return nil
			})
			probeAttempt.dispatched(dispatched && marked)
			replay.Release()
			result.Attempt = actual
			if err != nil {
				emitCodexTrace(ctx, CodexTraceEvent{
					Phase: "attempt", Stage: "round_trip", Outcome: "error", AccountHint: codexTraceAccountHint(choice.AccountKey),
					Attempt: actual.Ordinal, Reason: codexTraceErrorReason(err), Retry: attemptIndex > 0 || accountIndex > 0, Failover: accountIndex > 0,
				})
				discardCodexHTTPRequestResponse(ctx, response)
				if dispatched || marked {
					return session.finishIndeterminate(ctx, result, err)
				}
				return session.abandonPrepared(ctx, result, err)
			}
			if !dispatched || !marked || response == nil || response.Body == nil {
				discardCodexHTTPRequestResponse(ctx, response)
				if dispatched || marked {
					return session.finishIndeterminate(ctx, result, errors.New("Codex HTTP attempt returned an invalid response"))
				}
				return session.abandonPrepared(ctx, result, errors.New("Codex HTTP attempt did not cross dispatch fence"))
			}
			captureCodexHTTPAttemptPayload(ctx, template, response, choice, actual)
			result.Response = response
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				emitCodexTrace(ctx, CodexTraceEvent{
					Phase: "attempt", Stage: "response", Outcome: "accepted", AccountHint: codexTraceAccountHint(choice.AccountKey),
					Attempt: actual.Ordinal, UpstreamStatus: response.StatusCode, Retry: attemptIndex > 0 || accountIndex > 0, Failover: accountIndex > 0,
				})
				turnState, hasTurnState, evidenceErr := ParseCodexTurnStateHeader(response.Header)
				next, admitErr := result.Lifecycle.AdmitHTTP2xxContext(ctx, CodexHTTPAdmissionEvidence{
					TurnState:    turnState,
					HasTurnState: hasTurnState,
				})
				if admitErr == nil {
					result.Lifecycle = next
				}
				if evidenceErr != nil || admitErr != nil {
					discardCodexHTTPRequestResponse(ctx, response)
					result.Response = nil
					return session.finishIndeterminate(ctx, result, errors.Join(evidenceErr, admitErr))
				}
				probeAttempt.admitted()
				return result, nil
			}
			failure, classifyErr := (&CodexRequestRouter{Capacity: session.Capacity}).classifyAttemptResponse(choice, response)
			if classifyErr != nil {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				next, finishErr := result.Lifecycle.FinishRejected()
				if finishErr == nil {
					result.Lifecycle = next
				}
				return result, errors.Join(classifyErr, finishErr)
			}
			authRejected := failure == CodexPinnedAuthFailure
			hardRejected := failure == CodexPinnedHardLimit
			rejectReason := "upstream_rejected"
			if authRejected {
				rejectReason = "auth_rejected"
			} else if hardRejected {
				rejectReason = "capacity_exhausted"
			}
			emitCodexTrace(ctx, CodexTraceEvent{
				Phase: "attempt", Stage: "response", Outcome: "rejected", AccountHint: codexTraceAccountHint(choice.AccountKey),
				Attempt: actual.Ordinal, UpstreamStatus: response.StatusCode, Reason: rejectReason,
				Retry: authRejected || hardRejected, Failover: accountIndex > 0,
			})
			rejectKind := codexInstalledHTTPRejectOther
			if authRejected {
				rejectKind = codexInstalledHTTPRejectAuth
			} else if hardRejected {
				rejectKind = codexInstalledHTTPRejectExactHard429
			}
			probeAttempt.rejected(rejectKind)
			if authRejected && attemptIndex+1 == len(attempts) && hasRefresh && !refreshConsidered && session.Refresher != nil {
				refreshConsidered = true
				ref, revision, refreshErr := session.Refresher.RefreshReference(ctx, refreshPlanned.Candidate, refreshPlanned.Revision)
				if refreshErr == nil {
					refreshed, validationErr := candidateAttemptWithRefreshedRevision(refreshPlanned, ref, revision)
					if validationErr == nil {
						refreshed.Ordinal = len(attempts) + 1
						attempts = append(attempts, refreshed)
					} else {
						refreshErr = validationErr
					}
				}
				if refreshErr != nil && !errors.Is(refreshErr, codex.ErrRefreshIneligible) && !errors.Is(refreshErr, codex.ErrRefreshUnavailable) && !errors.Is(refreshErr, codex.ErrStaleRevision) {
					discardCodexHTTPRequestResponse(ctx, response)
					result.Response = nil
					next, finishErr := result.Lifecycle.FinishRejected()
					if finishErr == nil {
						result.Lifecycle = next
					}
					return result, errors.Join(refreshErr, finishErr)
				}
			}
			if authRejected && attemptIndex+1 < len(attempts) {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				var nextSlot uint32
				if attemptIndex+1 >= directAttempts {
					nextSlot = accountSlots[accountIndex].refresh
				} else {
					nextSlot = accountSlots[accountIndex].direct[attemptIndex+1]
				}
				next, retryErr := result.Lifecycle.RejectAndPrepareContext(ctx, nextSlot)
				if retryErr != nil {
					return session.finishIndeterminate(ctx, result, retryErr)
				}
				result.Lifecycle = next
				emitCodexTrace(ctx, CodexTraceEvent{Phase: "retry", Outcome: "credential", AccountHint: codexTraceAccountHint(choice.AccountKey), Attempt: attempts[attemptIndex+1].Ordinal, Reason: "auth_rejected", Retry: true})
				continue
			}
			if accountUnavailable := authRejected || hardRejected; accountUnavailable {
				nextAccountIndex, hasReplacement := codexHTTPRequestNextUnavailableAccount(
					accountIndex,
					ordinaryAccountCount,
					len(accounts),
					result.Lifecycle.EverAdmitted(),
				)
				if hasReplacement {
					nextChoice := accounts[nextAccountIndex].Choice()
					if account.IsDefault() {
						retainErr := retention.Reject(ctx, response, true)
						if retainErr != nil {
							next, finishErr := result.Lifecycle.FinishRejected()
							if finishErr == nil {
								result.Lifecycle = next
							}
							var rejectedErr *CodexRejectedResponseError
							if errors.As(retainErr, &rejectedErr) && rejectedErr.Code == CodexRejectedResponseBodyTooLarge {
								return result, finishErr
							}
							result.Response = nil
							return result, errors.Join(retainErr, finishErr)
						}
						retainedChoice = choice
						retainedAttempt = actual
						defaultRetained = true
					} else {
						discardCodexHTTPRequestResponse(ctx, response)
					}
					result.Response = nil
					next, retryErr := codexHTTPRequestRecordAccountUnavailable(
						ctx,
						result.Lifecycle,
						codexHTTPRequestFirstAccountSlot(accountSlots[nextAccountIndex]),
						hardRejected,
					)
					if retryErr != nil {
						return session.finishIndeterminate(ctx, result, retryErr)
					}
					result.Lifecycle = next
					emitCodexTrace(ctx, CodexTraceEvent{
						Phase: "failover", Outcome: "account", AccountHint: codexTraceAccountHint(nextChoice.AccountKey),
						Reason: rejectReason, Retry: true, Failover: true,
					})
					continue accountsLoop
				}
			}
			if (authRejected || hardRejected) && defaultRetained {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				next, finishErr := codexHTTPRequestRecordAccountUnavailable(ctx, result.Lifecycle, 0, hardRejected)
				if finishErr != nil {
					return result, finishErr
				}
				result.Lifecycle = next
				retained, retainErr := retention.Exhausted()
				if retainErr != nil {
					return result, retainErr
				}
				result.Response = retained
				result.Choice = retainedChoice
				result.Attempt = retainedAttempt
				return result, nil
			}
			if (authRejected || hardRejected) && plan.TerminalError() != nil && codexHTTPRequestCanRecordAccountUnavailable(plan, result.Lifecycle) {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				next, finishErr := codexHTTPRequestRecordAccountUnavailable(ctx, result.Lifecycle, 0, hardRejected)
				if finishErr == nil {
					result.Lifecycle = next
				}
				return result, errors.Join(plan.TerminalError(), finishErr)
			}
			var next CodexHTTPRequestLifecycle
			var finishErr error
			if (authRejected || hardRejected) && codexHTTPRequestCanRecordAccountUnavailable(plan, result.Lifecycle) {
				next, finishErr = codexHTTPRequestRecordAccountUnavailable(ctx, result.Lifecycle, 0, hardRejected)
			} else {
				next, finishErr = result.Lifecycle.FinishRejected()
			}
			if finishErr != nil {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				return result, finishErr
			}
			result.Lifecycle = next
			return result, nil
		}
	}
	return session.abandonPrepared(ctx, result, fmt.Errorf("%w: frozen plan has no attempts", plan.TerminalError()))
}

func codexHTTPRequestCanRecordAccountUnavailable(plan CodexFrozenDispatchPlan, lifecycle CodexHTTPRequestLifecycle) bool {
	return lifecycle != nil && (!lifecycle.EverAdmitted() || plan.accountUnavailablePortable || len(plan.accountUnavailableFallbacks) != 0 || len(plan.accountUnavailableResetCandidates) != 0)
}

func codexHTTPRequestRecordAccountUnavailable(ctx context.Context, lifecycle CodexHTTPRequestLifecycle, replacementSlot uint32, quotaExhausted bool) (CodexHTTPRequestLifecycle, error) {
	if quotaExhausted {
		return lifecycle.RecordQuotaExhaustedContext(ctx, replacementSlot)
	}
	next, err := lifecycle.RecordAccountUnavailableContext(ctx, replacementSlot)
	if err != nil || replacementSlot != 0 || !lifecycle.EverAdmitted() {
		return next, err
	}
	return next.CompleteAccountUnavailableCycleContext(ctx)
}

func codexHTTPRequestDispatchAccounts(plan CodexFrozenDispatchPlan) ([]CodexFrozenDispatchAccount, int) {
	ordinary := plan.Accounts()
	fallbacks := plan.AccountUnavailableFallbacks()
	accounts := make([]CodexFrozenDispatchAccount, 0, len(ordinary)+len(fallbacks))
	accounts = append(accounts, ordinary...)
	accounts = append(accounts, fallbacks...)
	return accounts, len(ordinary)
}

func codexHTTPRequestNextUnavailableAccount(current, ordinaryCount, total int, everAdmitted bool) (int, bool) {
	if current < ordinaryCount {
		if !everAdmitted && current+1 < ordinaryCount {
			return current + 1, true
		}
		if ordinaryCount < total {
			return ordinaryCount, true
		}
		return 0, false
	}
	if current+1 < total {
		return current + 1, true
	}
	return 0, false
}

type codexHTTPRequestAccountSlots struct {
	direct  []uint32
	refresh uint32
}

func codexHTTPRequestFirstAccountSlot(slots codexHTTPRequestAccountSlots) uint32 {
	if len(slots.direct) != 0 {
		return slots.direct[0]
	}
	return slots.refresh
}

func codexHTTPRequestAccountSlotMap(accounts []CodexFrozenDispatchAccount) []codexHTTPRequestAccountSlots {
	result := make([]codexHTTPRequestAccountSlots, len(accounts))
	next := uint32(1)
	for accountIndex, account := range accounts {
		attempts := account.Attempts()
		result[accountIndex].direct = make([]uint32, len(attempts))
		for attemptIndex := range attempts {
			result[accountIndex].direct[attemptIndex] = next
			next++
		}
		if _, ok := account.RefreshAttempt(); ok {
			result[accountIndex].refresh = next
			next++
		}
	}
	return result
}

func (session *CodexHTTPRequestSession) abandonPrepared(ctx context.Context, result CodexHTTPRequestSessionResult, cause error) (CodexHTTPRequestSessionResult, error) {
	cleanup := context.WithoutCancel(ctx)
	next, err := result.Lifecycle.AbandonBeforeDispatchContext(cleanup)
	if err == nil {
		result.Lifecycle = next
	}
	if cause == nil {
		cause = errors.New("Codex HTTP request has no dispatchable route")
	}
	return result, errors.Join(cause, err)
}

func (session *CodexHTTPRequestSession) finishIndeterminate(ctx context.Context, result CodexHTTPRequestSessionResult, cause error) (CodexHTTPRequestSessionResult, error) {
	next, err := result.Lifecycle.IndeterminateContext(context.WithoutCancel(ctx), CodexHTTPResponseEvidence{})
	if err != nil {
		return result, errors.Join(cause, err)
	}
	result.Lifecycle = next
	next, err = result.Lifecycle.Drain()
	if err != nil {
		return result, errors.Join(cause, err)
	}
	result.Lifecycle = next
	return result, cause
}

func codexHTTPRequestFromFrozen(ctx context.Context, template *http.Request, frozen *CodexFrozenRequest) (*CodexRequestReplay, *http.Request, error) {
	replay, err := frozen.Replay()
	if err != nil {
		return nil, nil, err
	}
	header, err := replay.Header()
	if err != nil {
		replay.Release()
		return nil, nil, err
	}
	contentLength, err := replay.ContentLength()
	if err != nil {
		replay.Release()
		return nil, nil, err
	}
	request := template.Clone(ctx)
	request.Header = header
	request.Body = http.NoBody
	request.GetBody = replay.GetBody
	request.ContentLength = contentLength
	return replay, request, nil
}

func discardCodexHTTPRequestResponse(ctx context.Context, response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	drainAndCloseCodexRejectedResponse(ctx, response)
}
