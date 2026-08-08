package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
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

	observationSequence atomic.Uint64
}

// Plan exposes secret-free route planning for WebSocket attempts.
func (r *CodexRequestRouter) Plan(ctx context.Context, requirements CodexRouteRequirements, accepted codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	if r == nil || r.Scope == nil {
		return CodexRequestPlan{}, fmt.Errorf("Codex request router unavailable")
	}
	return r.Scope.Plan(ctx, requirements, accepted, exclude...)
}

// Do executes a request with same-identity 401 recovery, one eligible refresh,
// and account failover only for pre-admission 401 or hard quota rejection.
func (r *CodexRequestRouter) Do(ctx context.Context, requirements CodexRouteRequirements, req *http.Request) (*http.Response, RouteChoice, CandidateAttempt, error) {
	if r == nil || r.Scope == nil || r.Executor == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("Codex request router unavailable")
	}
	var excluded []codex.SelectionExclusion
	var hardFallback *http.Response
	var lastChoice RouteChoice
	var lastAttempt CandidateAttempt
	hadUnauthorized := false

	for {
		plan, err := r.Scope.Plan(ctx, requirements, "", excluded...)
		if err != nil {
			if hardFallback != nil {
				return hardFallback, lastChoice, lastAttempt, nil
			}
			if hadUnauthorized {
				return nil, lastChoice, lastAttempt, fmt.Errorf("Codex token rejected and no alternate account available")
			}
			return nil, RouteChoice{}, CandidateAttempt{}, err
		}
		lastChoice = plan.Choice
		noteRouteAccount(ctx, redactedAccountHint("codex", string(plan.Choice.AccountKey)), len(excluded) > 0)

		accountHardLimited := false
		for _, attempt := range plan.Attempts {
			lastAttempt = attempt
			resp, err := r.Executor.Do(ctx, plan.Choice, attempt, req)
			if err != nil {
				return nil, plan.Choice, attempt, err
			}
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				hadUnauthorized = true
				closeResponse(resp)
				continue
			case http.StatusTooManyRequests:
				buffered, body, err := bufferAttemptResponse(resp)
				if err != nil {
					return nil, plan.Choice, attempt, err
				}
				if !isHardExhaustion(body) {
					return buffered, plan.Choice, attempt, nil
				}
				r.observeHardLimit(plan.Choice, buffered)
				if hardFallback != nil {
					closeResponse(hardFallback)
				}
				hardFallback = buffered
				accountHardLimited = true
			default:
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
				lastAttempt = refreshed
				resp, err := r.Executor.Do(ctx, plan.Choice, refreshed, req)
				if err != nil {
					return nil, plan.Choice, refreshed, err
				}
				switch resp.StatusCode {
				case http.StatusUnauthorized:
					hadUnauthorized = true
					closeResponse(resp)
				case http.StatusTooManyRequests:
					buffered, body, err := bufferAttemptResponse(resp)
					if err != nil {
						return nil, plan.Choice, refreshed, err
					}
					if !isHardExhaustion(body) {
						return buffered, plan.Choice, refreshed, nil
					}
					r.observeHardLimit(plan.Choice, buffered)
					if hardFallback != nil {
						closeResponse(hardFallback)
					}
					hardFallback = buffered
				default:
					return resp, plan.Choice, refreshed, nil
				}
			case errors.Is(refreshErr, codex.ErrRefreshIneligible), errors.Is(refreshErr, codex.ErrRefreshUnavailable), errors.Is(refreshErr, codex.ErrStaleRevision):
				// Another candidate or identity remains safe to try.
			default:
				return nil, plan.Choice, *plan.refreshAttempt, fmt.Errorf("refresh Codex credential candidate: %w", refreshErr)
			}
		}

		excluded = append(excluded, codex.SelectionExclusion{AccountKey: plan.Choice.AccountKey})
	}
}

func (r *CodexRequestRouter) observeHardLimit(choice RouteChoice, response *http.Response) {
	if r.Capacity == nil {
		return
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	resetAt := retryAfterReset(now, response.Header.Get("Retry-After"))
	sequence := r.observationSequence.Add(1)
	for _, bucket := range choice.RequiredBuckets {
		r.Capacity.Observe(CapacityFact{
			AccountKey:   choice.AccountKey,
			Bucket:       bucket,
			RemainingPct: 0,
			Source:       CapacitySourceHardLimit,
			Sequence:     sequence,
			ObservedAt:   now,
			ResetAt:      resetAt,
			Confidence:   CapacityConfidenceAuthoritative,
		})
	}
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

func bufferAttemptResponse(response *http.Response) (*http.Response, []byte, error) {
	if response == nil || response.Body == nil {
		return nil, nil, fmt.Errorf("Codex attempt returned an invalid response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, codexAttemptResponseLimit+1))
	response.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read Codex attempt response: %w", err)
	}
	if len(body) > codexAttemptResponseLimit {
		return nil, nil, fmt.Errorf("Codex attempt response exceeds 1 MiB")
	}
	buffered := makeBufferedResponse(response, body)
	buffered.Body = io.NopCloser(bytes.NewReader(body))
	return buffered, body, nil
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
