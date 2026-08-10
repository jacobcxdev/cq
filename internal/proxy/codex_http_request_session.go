package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	applyCodexTransportCredentials(out, material)
	if markDispatched == nil {
		if out.Body != nil {
			_ = out.Body.Close()
		}
		return nil, actual, false, errors.New("Codex dispatch fence unavailable")
	}
	if err := markDispatched(actual); err != nil {
		if out.Body != nil {
			_ = out.Body.Close()
		}
		return nil, actual, false, err
	}
	response, err := executor.Transport.inner().RoundTrip(out)
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
	AbandonBeforeDispatchContext(context.Context) (CodexHTTPRequestLifecycle, error)
	FinishRejected() (CodexHTTPRequestLifecycle, error)
	IndeterminateContext(context.Context) (CodexHTTPRequestLifecycle, error)
	Drain() (CodexHTTPRequestLifecycle, error)
	AdmitHTTP2xxContext(context.Context, CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error)
	ProviderCompleted(bool) (CodexHTTPRequestLifecycle, error)
	ProviderFailed() (CodexHTTPRequestLifecycle, error)
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
	accounts := plan.Accounts()
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
	accounts := plan.Accounts()
	if len(accounts) == 0 {
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
		attempts := account.Attempts()
		directAttempts := len(attempts)
		refreshPlanned, hasRefresh := account.RefreshAttempt()
		refreshConsidered := false
		for attemptIndex := 0; attemptIndex < len(attempts); attemptIndex++ {
			attempt := attempts[attemptIndex]
			result.Choice = choice
			result.Attempt = attempt
			if result.Lifecycle.AccountKey() != choice.AccountKey {
				return session.abandonPrepared(ctx, result, errors.New("Codex HTTP lifecycle account does not match frozen attempt"))
			}
			replay, request, err := codexHTTPRequestFromFrozen(ctx, template, frozen)
			if err != nil {
				return session.abandonPrepared(ctx, result, err)
			}
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
				return nil
			})
			replay.Release()
			result.Attempt = actual
			if err != nil {
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
			result.Response = response
			if response.StatusCode >= 200 && response.StatusCode < 300 {
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
				continue
			}
			if (authRejected || hardRejected) && !result.Lifecycle.EverAdmitted() && accountIndex+1 < len(accounts) {
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
				next, retryErr := result.Lifecycle.RejectAndPrepareContext(ctx, accountSlots[accountIndex+1].direct[0])
				if retryErr != nil {
					return session.finishIndeterminate(ctx, result, retryErr)
				}
				result.Lifecycle = next
				continue accountsLoop
			}
			if (authRejected || hardRejected) && defaultRetained {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				next, finishErr := result.Lifecycle.FinishRejected()
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
			if (authRejected || hardRejected) && plan.TerminalError() != nil {
				discardCodexHTTPRequestResponse(ctx, response)
				result.Response = nil
				next, finishErr := result.Lifecycle.FinishRejected()
				if finishErr == nil {
					result.Lifecycle = next
				}
				return result, errors.Join(plan.TerminalError(), finishErr)
			}
			next, finishErr := result.Lifecycle.FinishRejected()
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

type codexHTTPRequestAccountSlots struct {
	direct  []uint32
	refresh uint32
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
	next, err := result.Lifecycle.IndeterminateContext(context.WithoutCancel(ctx))
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
