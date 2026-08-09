package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexAttemptResponseLimit = 1 << 20

// CodexRequestRouter orchestrates bounded pre-admission recovery around an
// explicit, single-attempt executor.
type CodexRequestRouter struct {
	Scope     CodexRequestScoper
	Executor  ExplicitAccountExecutor
	Refresher codex.CredentialReferenceRefresher
	Capacity  *CodexCapacityLedger
	Now       func() time.Time
}

type CodexPinnedFailure uint8

const (
	CodexPinnedAccepted CodexPinnedFailure = iota
	CodexPinnedAuthFailure
	CodexPinnedHardLimit
)

// Plan exposes secret-free route planning for WebSocket attempts.
func (r *CodexRequestRouter) Plan(ctx context.Context, requirements CodexRouteRequirements, accepted codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	if r == nil || r.Scope == nil {
		return CodexRequestPlan{}, fmt.Errorf("Codex request router unavailable")
	}
	return r.Scope.Plan(ctx, requirements, accepted, exclude...)
}

func (r *CodexRequestRouter) AccountKeys(ctx context.Context) ([]codex.AccountKey, error) {
	scope, ok := r.Scope.(*CodexRequestScope)
	if !ok {
		return nil, fmt.Errorf("Codex request scope cannot enumerate accounts")
	}
	return scope.AccountKeys(ctx)
}

func (r *CodexRequestRouter) DoPinned(ctx context.Context, choice RouteChoice, req *http.Request) (*http.Response, CandidateAttempt, CodexPinnedFailure, error) {
	response, attempt, failure, _, err := r.doPinned(ctx, choice, req, nil)
	return response, attempt, failure, err
}

func (r *CodexRequestRouter) doPinned(ctx context.Context, choice RouteChoice, req *http.Request, rejected *http.Response) (*http.Response, CandidateAttempt, CodexPinnedFailure, bool, error) {
	if r == nil || r.Scope == nil || r.Executor == nil {
		return nil, CandidateAttempt{}, CodexPinnedAccepted, false, fmt.Errorf("Codex request router unavailable")
	}
	scoper, ok := r.Scope.(codexChoiceScoper)
	if !ok {
		return nil, CandidateAttempt{}, CodexPinnedAccepted, false, fmt.Errorf("Codex request scope does not support fixed choices")
	}
	plan, err := scoper.PlanChoice(ctx, choice, "")
	if err != nil {
		if rejected != nil {
			return rejected, CandidateAttempt{}, CodexPinnedAuthFailure, true, nil
		}
		return nil, CandidateAttempt{}, CodexPinnedAccepted, false, err
	}
	var last CandidateAttempt
	try := func(attempt CandidateAttempt) (*http.Response, CodexPinnedFailure, bool, error) {
		response, dispatched, err := executeCodexAttempt(r.Executor, ctx, choice, attempt, req, func() {
			closeResponse(rejected)
			rejected = nil
			last = attempt
			observeCodexAttempt(ctx, choice, attempt)
		})
		if err != nil {
			if !dispatched && rejected != nil {
				return rejected, CodexPinnedAuthFailure, true, nil
			}
			return nil, CodexPinnedAccepted, false, err
		}
		failure, err := r.classifyAttemptResponse(choice, response)
		if err != nil {
			closeResponse(response)
			return nil, CodexPinnedAccepted, false, err
		}
		return response, failure, false, nil
	}
	for _, attempt := range plan.Attempts {
		response, failure, terminal, err := try(attempt)
		if err != nil {
			closeResponse(rejected)
			return nil, last, failure, false, err
		}
		if terminal {
			return response, last, failure, true, nil
		}
		if failure != CodexPinnedAuthFailure {
			closeResponse(rejected)
			return response, last, failure, false, err
		}
		closeResponse(rejected)
		rejected = response
	}
	if plan.refreshAttempt != nil && r.Refresher != nil {
		ref, revision, refreshErr := r.Refresher.RefreshReference(ctx, plan.refreshAttempt.Candidate, plan.refreshAttempt.Revision)
		if refreshErr == nil {
			attempt := *plan.refreshAttempt
			attempt.Candidate = ref
			attempt.Revision = revision
			attempt.Ordinal = len(plan.Attempts) + 1
			response, failure, terminal, err := try(attempt)
			if err != nil {
				closeResponse(rejected)
				return nil, last, failure, false, err
			}
			return response, last, failure, terminal, nil
		}
		if !errors.Is(refreshErr, codex.ErrRefreshIneligible) && !errors.Is(refreshErr, codex.ErrRefreshUnavailable) && !errors.Is(refreshErr, codex.ErrStaleRevision) {
			if rejected != nil {
				return rejected, last, CodexPinnedAuthFailure, true, nil
			}
			return nil, *plan.refreshAttempt, CodexPinnedAccepted, false, fmt.Errorf("refresh Codex credential candidate: %w", refreshErr)
		}
	}
	return rejected, last, CodexPinnedAuthFailure, false, nil
}

// Do executes a request with same-identity 401 recovery, one eligible refresh,
// and account failover only for pre-admission 401 or hard quota rejection.
func (r *CodexRequestRouter) Do(ctx context.Context, requirements CodexRouteRequirements, req *http.Request) (*http.Response, RouteChoice, CandidateAttempt, error) {
	if r == nil || r.Scope == nil || r.Executor == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("Codex request router unavailable")
	}
	var excluded []codex.SelectionExclusion
	var rejected *http.Response
	var rejectedChoice RouteChoice
	var rejectedAttempt CandidateAttempt
	replaceRejected := func(response *http.Response, choice RouteChoice, attempt CandidateAttempt) {
		closeResponse(rejected)
		rejected = response
		rejectedChoice = choice
		rejectedAttempt = attempt
	}

	for {
		plan, err := r.Scope.Plan(ctx, requirements, "", excluded...)
		if err != nil {
			if rejected != nil {
				return rejected, rejectedChoice, rejectedAttempt, nil
			}
			return nil, RouteChoice{}, CandidateAttempt{}, err
		}
		noteRouteAccount(ctx, redactedAccountHint("codex", string(plan.Choice.AccountKey)), len(excluded) > 0)

		accountHardLimited := false
		for _, attempt := range plan.Attempts {
			resp, dispatched, err := executeCodexAttempt(r.Executor, ctx, plan.Choice, attempt, req, func() {
				closeResponse(rejected)
				rejected = nil
				observeCodexAttempt(ctx, plan.Choice, attempt)
			})
			if err != nil {
				if !dispatched && rejected != nil {
					return rejected, rejectedChoice, rejectedAttempt, nil
				}
				closeResponse(rejected)
				return nil, plan.Choice, attempt, err
			}
			failure, err := r.classifyAttemptResponse(plan.Choice, resp)
			if err != nil {
				closeResponse(resp)
				closeResponse(rejected)
				return nil, plan.Choice, attempt, err
			}
			switch failure {
			case CodexPinnedAuthFailure:
				replaceRejected(resp, plan.Choice, attempt)
				continue
			case CodexPinnedHardLimit:
				replaceRejected(resp, plan.Choice, attempt)
				accountHardLimited = true
			case CodexPinnedAccepted:
				closeResponse(rejected)
				return resp, plan.Choice, attempt, nil
			}
			if accountHardLimited {
				break
			}
		}

		if !accountHardLimited && plan.refreshAttempt != nil && r.Refresher != nil {
			refreshedRef, refreshedRevision, refreshErr := r.Refresher.RefreshReference(ctx, plan.refreshAttempt.Candidate, plan.refreshAttempt.Revision)
			switch {
			case refreshErr == nil:
				refreshed := *plan.refreshAttempt
				refreshed.Candidate = refreshedRef
				refreshed.Revision = refreshedRevision
				refreshed.Ordinal = len(plan.Attempts) + 1
				resp, dispatched, err := executeCodexAttempt(r.Executor, ctx, plan.Choice, refreshed, req, func() {
					closeResponse(rejected)
					rejected = nil
					observeCodexAttempt(ctx, plan.Choice, refreshed)
				})
				if err != nil {
					if !dispatched && rejected != nil {
						return rejected, rejectedChoice, rejectedAttempt, nil
					}
					closeResponse(rejected)
					return nil, plan.Choice, refreshed, err
				}
				failure, err := r.classifyAttemptResponse(plan.Choice, resp)
				if err != nil {
					closeResponse(resp)
					closeResponse(rejected)
					return nil, plan.Choice, refreshed, err
				}
				switch failure {
				case CodexPinnedAuthFailure:
					replaceRejected(resp, plan.Choice, refreshed)
				case CodexPinnedHardLimit:
					replaceRejected(resp, plan.Choice, refreshed)
					accountHardLimited = true
				case CodexPinnedAccepted:
					closeResponse(rejected)
					return resp, plan.Choice, refreshed, nil
				}
			case errors.Is(refreshErr, codex.ErrRefreshIneligible), errors.Is(refreshErr, codex.ErrRefreshUnavailable), errors.Is(refreshErr, codex.ErrStaleRevision):
				// Another candidate or identity remains safe to try.
			default:
				if rejected != nil {
					return rejected, rejectedChoice, rejectedAttempt, nil
				}
				return nil, plan.Choice, *plan.refreshAttempt, fmt.Errorf("refresh Codex credential candidate: %w", refreshErr)
			}
		}

		excluded = append(excluded, codex.SelectionExclusion{AccountKey: plan.Choice.AccountKey})
	}
}

func (r *CodexRequestRouter) classifyAttemptResponse(choice RouteChoice, response *http.Response) (CodexPinnedFailure, error) {
	if response == nil || response.Body == nil {
		return CodexPinnedAccepted, fmt.Errorf("Codex attempt returned an invalid response")
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return CodexPinnedAuthFailure, nil
	case http.StatusTooManyRequests:
		if response.Uncompressed || !codexAttemptResponseHasIdentityEncoding(response.Header) {
			return CodexPinnedAccepted, nil
		}
		body, complete, err := inspectAttemptResponse(response)
		if err != nil {
			return CodexPinnedAccepted, err
		}
		if !complete {
			return CodexPinnedAccepted, nil
		}
		wrapped, err := parseCodexHTTPError(body, response.StatusCode)
		if err != nil || !wrapped.HardUsageLimit {
			return CodexPinnedAccepted, nil
		}
		r.observeHardLimit(choice, response)
		return CodexPinnedHardLimit, nil
	default:
		return CodexPinnedAccepted, nil
	}
}

func executeCodexAttempt(executor ExplicitAccountExecutor, ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request, onDispatch func()) (*http.Response, bool, error) {
	dispatched := false
	markDispatched := func() {
		if dispatched {
			return
		}
		dispatched = true
		if onDispatch != nil {
			onDispatch()
		}
	}
	if aware, ok := executor.(explicitAccountDispatchExecutor); ok {
		response, err := aware.doOnDispatch(ctx, choice, attempt, req, markDispatched)
		return response, dispatched, err
	}
	markDispatched()
	response, err := executor.Do(ctx, choice, attempt, req)
	return response, dispatched, err
}

func codexAttemptResponseHasIdentityEncoding(header http.Header) bool {
	found := false
	var values []string
	for name, headerValues := range header {
		if !strings.EqualFold(name, "Content-Encoding") {
			continue
		}
		found = true
		values = append(values, headerValues...)
	}
	if !found {
		return true
	}
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}

func (r *CodexRequestRouter) observeHardLimit(choice RouteChoice, response *http.Response) {
	if r.Capacity == nil {
		return
	}
	attemptedModel := choice.EffectiveModel
	if attemptedModel == "" {
		attemptedModel = choice.RequestedModel
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	resetAt := retryAfterReset(now, response.Header.Get("Retry-After"))
	stream := r.Capacity.NewObservationStream()
	r.Capacity.Observe(stream.Stamp(CapacityFact{
		AccountKey:   choice.AccountKey,
		Bucket:       CapacityBucketForModel(attemptedModel),
		RemainingPct: 0,
		Source:       CapacitySourceHardLimit,
		ObservedAt:   now,
		ResetAt:      resetAt,
		Confidence:   CapacityConfidenceAuthoritative,
	}))
}

func retryAfterReset(now time.Time, value string) time.Time {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return parsed
	}
	return time.Time{}
}

func inspectAttemptResponse(response *http.Response) ([]byte, bool, error) {
	if response == nil || response.Body == nil {
		return nil, false, fmt.Errorf("Codex attempt returned an invalid response")
	}
	original := response.Body
	body, err := io.ReadAll(io.LimitReader(original, codexAttemptResponseLimit+1))
	if err != nil {
		return nil, false, fmt.Errorf("read Codex attempt response: %w", err)
	}
	if len(body) > codexAttemptResponseLimit {
		response.Body = &codexReplayBody{
			Reader: io.MultiReader(bytes.NewReader(body), original),
			Closer: original,
		}
		return nil, false, nil
	}
	_ = original.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	return body, true, nil
}

type codexReplayBody struct {
	io.Reader
	io.Closer
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
