package proxy

import (
	"bytes"
	"compress/gzip"
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
		if errors.Is(err, codex.ErrCredentialInventoryDegraded) {
			closeResponse(rejected)
			return nil, CandidateAttempt{}, CodexPinnedAccepted, false, codex.ErrCredentialInventoryDegraded
		}
		if rejected != nil {
			return rejected, CandidateAttempt{}, CodexPinnedAuthFailure, true, nil
		}
		return nil, CandidateAttempt{}, CodexPinnedAccepted, false, err
	}
	var last CandidateAttempt
	refreshAttempt := cloneCandidateAttempt(plan.refreshAttempt)
	try := func(attempt CandidateAttempt) (*http.Response, CodexPinnedFailure, bool, error) {
		response, actual, dispatched, err := executeCodexAttempt(r.Executor, ctx, choice, attempt, req, func(actual CandidateAttempt) {
			closeResponse(rejected)
			rejected = nil
			last = actual
			updateCandidateAttempt(refreshAttempt, actual)
			observeCodexAttempt(ctx, choice, actual)
		})
		if err != nil {
			if errors.Is(err, codex.ErrCredentialInventoryDegraded) {
				closeResponse(rejected)
				rejected = nil
				return nil, CodexPinnedAccepted, false, codex.ErrCredentialInventoryDegraded
			}
			if !dispatched && rejected != nil {
				return rejected, CodexPinnedAuthFailure, true, nil
			}
			if !dispatched {
				last = actual
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
	if refreshAttempt != nil && r.Refresher != nil {
		ref, revision, refreshErr := r.Refresher.RefreshReference(ctx, refreshAttempt.Candidate, refreshAttempt.Revision)
		var attempt CandidateAttempt
		if refreshErr == nil {
			attempt, refreshErr = candidateAttemptWithRefreshedRevision(*refreshAttempt, ref, revision)
		}
		if refreshErr == nil {
			attempt.Ordinal = len(plan.Attempts) + 1
			response, failure, terminal, err := try(attempt)
			if err != nil {
				closeResponse(rejected)
				return nil, last, failure, false, err
			}
			return response, last, failure, terminal, nil
		}
		if errors.Is(refreshErr, codex.ErrCredentialInventoryDegraded) {
			closeResponse(rejected)
			return nil, *refreshAttempt, CodexPinnedAccepted, false, codex.ErrCredentialInventoryDegraded
		}
		if !errors.Is(refreshErr, codex.ErrRefreshIneligible) && !errors.Is(refreshErr, codex.ErrRefreshUnavailable) && !errors.Is(refreshErr, codex.ErrStaleRevision) {
			if rejected != nil {
				return rejected, last, CodexPinnedAuthFailure, true, nil
			}
			return nil, *refreshAttempt, CodexPinnedAccepted, false, fmt.Errorf("refresh Codex credential candidate: %w", refreshErr)
		}
	}
	return rejected, last, CodexPinnedAuthFailure, false, nil
}

// Do executes a request with same-identity 401 recovery, one eligible refresh,
// and account failover only for exact pre-admission hard quota rejection.
func (r *CodexRequestRouter) Do(ctx context.Context, requirements CodexRouteRequirements, req *http.Request) (*http.Response, RouteChoice, CandidateAttempt, error) {
	return r.do(ctx, requirements, req, nil)
}

func (r *CodexRequestRouter) do(ctx context.Context, requirements CodexRouteRequirements, req *http.Request, initialExclusions []codex.SelectionExclusion) (*http.Response, RouteChoice, CandidateAttempt, error) {
	if r == nil || r.Scope == nil || r.Executor == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("Codex request router unavailable")
	}
	excluded := append([]codex.SelectionExclusion(nil), initialExclusions...)
	initialExclusionCount := len(excluded)
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
			if errors.Is(err, codex.ErrCredentialInventoryDegraded) {
				closeResponse(rejected)
				return nil, rejectedChoice, rejectedAttempt, codex.ErrCredentialInventoryDegraded
			}
			if rejected != nil {
				return rejected, rejectedChoice, rejectedAttempt, nil
			}
			return nil, RouteChoice{}, CandidateAttempt{}, err
		}
		noteRouteAccount(ctx, redactedAccountHint("codex", string(plan.Choice.AccountKey)), len(excluded) > initialExclusionCount)

		accountHardLimited := false
		refreshAttempt := cloneCandidateAttempt(plan.refreshAttempt)
		for _, attempt := range plan.Attempts {
			resp, actual, dispatched, err := executeCodexAttempt(r.Executor, ctx, plan.Choice, attempt, req, func(actual CandidateAttempt) {
				closeResponse(rejected)
				rejected = nil
				updateCandidateAttempt(refreshAttempt, actual)
				observeCodexAttempt(ctx, plan.Choice, actual)
			})
			if err != nil {
				if errors.Is(err, codex.ErrCredentialInventoryDegraded) {
					closeResponse(rejected)
					return nil, plan.Choice, actual, codex.ErrCredentialInventoryDegraded
				}
				if !dispatched && rejected != nil {
					return rejected, rejectedChoice, rejectedAttempt, nil
				}
				closeResponse(rejected)
				return nil, plan.Choice, actual, err
			}
			failure, err := r.classifyAttemptResponse(plan.Choice, resp)
			if err != nil {
				closeResponse(resp)
				closeResponse(rejected)
				return nil, plan.Choice, actual, err
			}
			switch failure {
			case CodexPinnedAuthFailure:
				replaceRejected(resp, plan.Choice, actual)
				continue
			case CodexPinnedHardLimit:
				replaceRejected(resp, plan.Choice, actual)
				accountHardLimited = true
			case CodexPinnedAccepted:
				closeResponse(rejected)
				return resp, plan.Choice, actual, nil
			}
			if accountHardLimited {
				break
			}
		}

		if !accountHardLimited && refreshAttempt != nil && r.Refresher != nil {
			refreshedRef, refreshedRevision, refreshErr := r.Refresher.RefreshReference(ctx, refreshAttempt.Candidate, refreshAttempt.Revision)
			var refreshed CandidateAttempt
			if refreshErr == nil {
				refreshed, refreshErr = candidateAttemptWithRefreshedRevision(*refreshAttempt, refreshedRef, refreshedRevision)
			}
			switch {
			case refreshErr == nil:
				refreshed.Ordinal = len(plan.Attempts) + 1
				resp, actual, dispatched, err := executeCodexAttempt(r.Executor, ctx, plan.Choice, refreshed, req, func(actual CandidateAttempt) {
					closeResponse(rejected)
					rejected = nil
					observeCodexAttempt(ctx, plan.Choice, actual)
				})
				if err != nil {
					if errors.Is(err, codex.ErrCredentialInventoryDegraded) {
						closeResponse(rejected)
						return nil, plan.Choice, actual, codex.ErrCredentialInventoryDegraded
					}
					if !dispatched && rejected != nil {
						return rejected, rejectedChoice, rejectedAttempt, nil
					}
					closeResponse(rejected)
					return nil, plan.Choice, actual, err
				}
				failure, err := r.classifyAttemptResponse(plan.Choice, resp)
				if err != nil {
					closeResponse(resp)
					closeResponse(rejected)
					return nil, plan.Choice, actual, err
				}
				switch failure {
				case CodexPinnedAuthFailure:
					replaceRejected(resp, plan.Choice, actual)
				case CodexPinnedHardLimit:
					replaceRejected(resp, plan.Choice, actual)
					accountHardLimited = true
				case CodexPinnedAccepted:
					closeResponse(rejected)
					return resp, plan.Choice, actual, nil
				}
			case errors.Is(refreshErr, codex.ErrCredentialInventoryDegraded):
				closeResponse(rejected)
				return nil, plan.Choice, *refreshAttempt, codex.ErrCredentialInventoryDegraded
			case errors.Is(refreshErr, codex.ErrRefreshIneligible), errors.Is(refreshErr, codex.ErrRefreshUnavailable), errors.Is(refreshErr, codex.ErrStaleRevision):
				// Same-identity recovery is exhausted; preserve the rejection.
			default:
				if rejected != nil {
					return rejected, rejectedChoice, rejectedAttempt, nil
				}
				return nil, plan.Choice, *refreshAttempt, fmt.Errorf("refresh Codex credential candidate: %w", refreshErr)
			}
		}

		if !accountHardLimited && rejected != nil {
			return rejected, rejectedChoice, rejectedAttempt, nil
		}
		excluded = append(excluded, codex.SelectionExclusion{AccountKey: plan.Choice.AccountKey})
	}
}

func candidateAttemptWithRefreshedRevision(accepted CandidateAttempt, ref codex.CandidateRef, revision codex.Revision) (CandidateAttempt, error) {
	if ref != accepted.Candidate || revision == "" || revision == accepted.Revision {
		return CandidateAttempt{}, codex.ErrStaleRevision
	}
	accepted.Revision = revision
	return accepted, nil
}

func (r *CodexRequestRouter) classifyAttemptResponse(choice RouteChoice, response *http.Response) (CodexPinnedFailure, error) {
	if response == nil || response.Body == nil {
		return CodexPinnedAccepted, fmt.Errorf("Codex attempt returned an invalid response")
	}
	var capacity *codexRateLimitProducer
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		capacity = r.newRateLimitProducer(choice, true)
		if capacity != nil {
			_ = capacity.ObserveHeaders(response.Header)
		}
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return CodexPinnedAuthFailure, nil
	case http.StatusForbidden:
		body, complete, err := inspectAttemptResponseForClassification(response)
		if err != nil {
			return CodexPinnedAccepted, err
		}
		if !complete {
			return CodexPinnedAccepted, nil
		}
		wrapped, err := parseCodexHTTPError(body, response.StatusCode)
		if err != nil || !wrapped.AuthFailure || wrapped.ErrorType != "authentication_error" {
			return CodexPinnedAccepted, nil
		}
		return CodexPinnedAuthFailure, nil
	case http.StatusTooManyRequests:
		body, complete, err := inspectAttemptResponseForClassification(response)
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
		detachInspectedAttemptResponse(response)
		r.observeHardLimit(choice, response, capacity)
		return CodexPinnedHardLimit, nil
	default:
		return CodexPinnedAccepted, nil
	}
}

func cloneCandidateAttempt(attempt *CandidateAttempt) *CandidateAttempt {
	if attempt == nil {
		return nil
	}
	copy := *attempt
	return &copy
}

func updateCandidateAttempt(planned *CandidateAttempt, actual CandidateAttempt) {
	if planned == nil || planned.AccountKey != actual.AccountKey ||
		planned.Candidate.AccountKey != actual.Candidate.AccountKey ||
		planned.Candidate.CandidateID != actual.Candidate.CandidateID ||
		planned.Source != actual.Source || planned.Identity != actual.Identity {
		return
	}
	*planned = actual
}

func executeCodexAttempt(executor ExplicitAccountExecutor, ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request, onDispatch func(CandidateAttempt)) (*http.Response, CandidateAttempt, bool, error) {
	dispatched := false
	actual := attempt
	markDispatched := func(resolved CandidateAttempt) {
		if dispatched {
			return
		}
		actual = resolved
		dispatched = true
		if onDispatch != nil {
			onDispatch(resolved)
		}
	}
	if aware, ok := executor.(explicitAccountDispatchExecutor); ok {
		response, resolved, err := aware.doOnDispatch(ctx, choice, attempt, req, markDispatched)
		if !dispatched {
			actual = resolved
		}
		return response, actual, dispatched, err
	}
	markDispatched(attempt)
	response, err := executor.Do(ctx, choice, attempt, req)
	return response, actual, dispatched, err
}

func codexAttemptResponseHasIdentityEncoding(header http.Header) bool {
	encoding, supported := codexAttemptResponseEncoding(header)
	return supported && encoding == "identity"
}

func codexAttemptResponseEncoding(header http.Header) (string, bool) {
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
		return "identity", true
	}
	if len(values) != 1 {
		return "", false
	}
	encoding := strings.ToLower(strings.TrimSpace(values[0]))
	switch encoding {
	case "identity", "gzip", "zstd":
		return encoding, true
	default:
		return "", false
	}
}

func (r *CodexRequestRouter) newRateLimitProducer(choice RouteChoice, liveEventsAuthoritative bool) *codexRateLimitProducer {
	if r == nil || r.Capacity == nil {
		return nil
	}
	now := r.Now
	if now == nil {
		now = r.Capacity.now
	}
	return newCodexRateLimitProducer(
		r.Capacity,
		r.Capacity.NewObservationStream(),
		choice.AccountKey,
		now,
		liveEventsAuthoritative,
	)
}

func (r *CodexRequestRouter) observeHardLimit(choice RouteChoice, response *http.Response, producer *codexRateLimitProducer) {
	if r == nil || response == nil {
		return
	}
	attemptedModel := choice.EffectiveModel
	if attemptedModel == "" {
		attemptedModel = choice.RequestedModel
	}
	if producer == nil {
		producer = r.newRateLimitProducer(choice, true)
	}
	if producer == nil {
		return
	}
	now := producer.now()
	resetAt := retryAfterReset(now, response.Header.Get("Retry-After"))
	producer.ObserveHardLimit(CapacityBucketForModel(attemptedModel), resetAt)
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
	response.Body = &codexReplayBody{
		Reader: bytes.NewReader(body),
		Closer: original,
	}
	return body, true, nil
}

func inspectAttemptResponseForClassification(response *http.Response) ([]byte, bool, error) {
	body, complete, err := inspectAttemptResponse(response)
	if err != nil || !complete {
		return body, complete, err
	}
	decoded, complete := decodeCodexErrorResponseBody(body, response.Header, response.Uncompressed)
	return decoded, complete, nil
}

func decodeCodexErrorResponseBody(body []byte, header http.Header, uncompressed bool) ([]byte, bool) {
	if len(body) > codexAttemptResponseLimit {
		return nil, false
	}
	if uncompressed {
		return body, true
	}
	encoding, supported := codexAttemptResponseEncoding(header)
	if !supported {
		return nil, false
	}
	switch encoding {
	case "identity":
		return body, true
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		decoded, readErr := io.ReadAll(io.LimitReader(reader, codexAttemptResponseLimit+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(decoded) > codexAttemptResponseLimit {
			return nil, false
		}
		return decoded, true
	case "zstd":
		decoded, err := DecodeCodexRequest(body, encoding, CodexZstdLimits{
			MaxEncodedBytes: codexAttemptResponseLimit,
			MaxDecodedBytes: codexAttemptResponseLimit,
		})
		if err != nil {
			return nil, false
		}
		return decoded.Decoded(), true
	default:
		return nil, false
	}
}

func detachInspectedAttemptResponse(response *http.Response) {
	if response == nil {
		return
	}
	replay, ok := response.Body.(*codexReplayBody)
	if !ok {
		return
	}
	_ = replay.Closer.Close()
	response.Body = io.NopCloser(replay.Reader)
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
