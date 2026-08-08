package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type queuedRequestScope struct {
	plans []CodexRequestPlan
	calls int
}

func (s *queuedRequestScope) Plan(_ context.Context, _ CodexRouteRequirements, _ codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	s.calls++
	index := len(exclude)
	if index >= len(s.plans) {
		return CodexRequestPlan{}, errors.New("no account")
	}
	return s.plans[index], nil
}

type attemptResult struct {
	status int
	body   string
	err    error
}

type queuedAttemptExecutor struct {
	results map[codex.CandidateID][]attemptResult
	calls   []codex.CandidateID
}

func (e *queuedAttemptExecutor) Do(_ context.Context, _ RouteChoice, attempt CandidateAttempt, _ *http.Request) (*http.Response, error) {
	id := attempt.Candidate.CandidateID
	e.calls = append(e.calls, id)
	queue := e.results[id]
	if len(queue) == 0 {
		return nil, errors.New("unexpected attempt")
	}
	result := queue[0]
	e.results[id] = queue[1:]
	if result.err != nil {
		return nil, result.err
	}
	return &http.Response{StatusCode: result.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result.body))}, nil
}

type fakeReferenceRefresher struct {
	ref      codex.CandidateRef
	revision codex.Revision
	err      error
	calls    int
}

func (r *fakeReferenceRefresher) RefreshReference(context.Context, codex.CandidateRef, codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	r.calls++
	return r.ref, r.revision, r.err
}

func requestPlan(account codex.AccountKey, ids ...codex.CandidateID) CodexRequestPlan {
	plan := CodexRequestPlan{Choice: RouteChoice{
		AccountKey: account, RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4",
		RequiredBuckets: []CapacityBucket{CapacityBucketForModel("gpt-5.4")},
	}}
	for index, id := range ids {
		plan.Attempts = append(plan.Attempts, CandidateAttempt{
			AccountKey: account,
			Candidate:  codex.CandidateRef{AccountKey: account, CandidateID: id},
			Revision:   codex.Revision("revision-" + id),
			Ordinal:    index + 1,
		})
	}
	return plan
}

func TestCodexRequestRouterUsesSameIdentityCandidatesBeforeFailover(t *testing.T) {
	first := requestPlan("one", "stale", "fresh")
	second := requestPlan("two", "other")
	scope := &queuedRequestScope{plans: []CodexRequestPlan{first, second}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"stale": {{status: http.StatusUnauthorized}},
		"fresh": {{status: http.StatusOK}},
	}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor}
	response, choice, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if choice.AccountKey != "one" || scope.calls != 1 || strings.Join(candidateStrings(executor.calls), ",") != "stale,fresh" {
		t.Fatalf("choice=%q scope calls=%d attempts=%v", choice.AccountKey, scope.calls, executor.calls)
	}
}

func TestCodexRequestRouterRefreshesOnceAfterCandidate401s(t *testing.T) {
	plan := requestPlan("one", "managed")
	refreshAttempt := plan.Attempts[0]
	plan.refreshAttempt = &refreshAttempt
	scope := &queuedRequestScope{plans: []CodexRequestPlan{plan}}
	refresher := &fakeReferenceRefresher{ref: codex.CandidateRef{AccountKey: "one", CandidateID: "refreshed"}, revision: "new"}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"managed":   {{status: http.StatusUnauthorized}},
		"refreshed": {{status: http.StatusOK}},
	}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Refresher: refresher}
	response, _, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if refresher.calls != 1 || attempt.Candidate.CandidateID != "refreshed" {
		t.Fatalf("refresh calls=%d attempt=%+v", refresher.calls, attempt)
	}
}

func TestCodexRequestRouterFailsOverOnlyForPreAdmissionHardFailures(t *testing.T) {
	tests := []struct {
		name       string
		first      attemptResult
		wantStatus int
		wantCalls  string
	}{
		{name: "401", first: attemptResult{status: http.StatusUnauthorized}, wantStatus: http.StatusOK, wantCalls: "first,second"},
		{name: "hard 429", first: attemptResult{status: http.StatusTooManyRequests, body: `{"error":{"type":"insufficient_quota"}}`}, wantStatus: http.StatusOK, wantCalls: "first,second"},
		{name: "soft 429", first: attemptResult{status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_exceeded"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := &queuedRequestScope{plans: []CodexRequestPlan{requestPlan("one", "first"), requestPlan("two", "second")}}
			executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
				"first":  {test.first},
				"second": {{status: http.StatusOK}},
			}}
			router := &CodexRequestRouter{Scope: scope, Executor: executor}
			response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus || strings.Join(candidateStrings(executor.calls), ",") != test.wantCalls {
				t.Fatalf("status=%d calls=%v", response.StatusCode, executor.calls)
			}
		})
	}
}

func TestCodexRequestRouterNeverFailsOverNetworkError(t *testing.T) {
	scope := &queuedRequestScope{plans: []CodexRequestPlan{requestPlan("one", "first"), requestPlan("two", "second")}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{"first": {{err: errors.New("connection reset")}}}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor}
	_, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err == nil || err.Error() != "connection reset" || scope.calls != 1 {
		t.Fatalf("err=%v scope calls=%d", err, scope.calls)
	}
}

func TestCodexRequestRouterHardLimitRecordsCapacityAndPreservesFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	scope := &queuedRequestScope{plans: []CodexRequestPlan{requestPlan("one", "first")}}
	body := `{"error":{"type":"insufficient_quota","message":"used"}}`
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{"first": {{status: http.StatusTooManyRequests, body: body}}}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Capacity: ledger, Now: func() time.Time { return now }}
	response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	view := ledger.Capacity("one", CapacityBucketForModel("gpt-5.4"))
	if string(data) != body || view.State != CapacityZero {
		t.Fatalf("body=%s capacity=%+v", data, view)
	}
}

func candidateStrings(ids []codex.CandidateID) []string {
	result := make([]string, len(ids))
	for index, id := range ids {
		result[index] = string(id)
	}
	return result
}
