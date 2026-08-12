package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexLiveUsageLimitBody = `{"error":{"eligible_promo":null,"message":"The usage limit has been reached","plan_type":"pro","resets_at":1786832019,"resets_in_seconds":539708,"type":"usage_limit_reached"}}`

type queuedRequestScope struct {
	plans        []CodexRequestPlan
	calls        int
	exhaustedErr error
}

func (s *queuedRequestScope) Plan(_ context.Context, _ CodexRouteRequirements, _ codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	s.calls++
	index := len(exclude)
	if index >= len(s.plans) {
		if s.exhaustedErr != nil {
			return CodexRequestPlan{}, s.exhaustedErr
		}
		return CodexRequestPlan{}, errors.New("no account")
	}
	return s.plans[index], nil
}

func (s *queuedRequestScope) PlanChoice(_ context.Context, choice RouteChoice, _ codex.Revision) (CodexRequestPlan, error) {
	for _, plan := range s.plans {
		if plan.Choice.AccountKey == choice.AccountKey {
			return plan, nil
		}
	}
	if s.exhaustedErr != nil {
		return CodexRequestPlan{}, s.exhaustedErr
	}
	return CodexRequestPlan{}, errors.New("no account")
}

type attemptResult struct {
	status int
	body   string
	header http.Header
	resp   *http.Response
	err    error
	before func()
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
	if result.before != nil {
		result.before()
	}
	if result.err != nil {
		return nil, result.err
	}
	if result.resp != nil {
		return result.resp, nil
	}
	header := result.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: result.status, Header: header, Body: io.NopCloser(strings.NewReader(result.body))}, nil
}

type queuedPredispatchAttemptExecutor struct {
	results map[codex.CandidateID][]attemptResult
	calls   []codex.CandidateID
}

func (e *queuedPredispatchAttemptExecutor) Do(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request) (*http.Response, error) {
	response, _, err := e.doOnDispatch(ctx, choice, attempt, req, nil)
	return response, err
}

func (e *queuedPredispatchAttemptExecutor) doOnDispatch(_ context.Context, _ RouteChoice, attempt CandidateAttempt, _ *http.Request, onDispatch func(CandidateAttempt)) (*http.Response, CandidateAttempt, error) {
	id := attempt.Candidate.CandidateID
	e.calls = append(e.calls, id)
	queue := e.results[id]
	if len(queue) == 0 {
		return nil, attempt, errors.New("unexpected attempt")
	}
	result := queue[0]
	e.results[id] = queue[1:]
	if result.before != nil {
		result.before()
	}
	if result.err != nil {
		return nil, attempt, result.err
	}
	if onDispatch != nil {
		onDispatch(attempt)
	}
	if result.resp != nil {
		return result.resp, attempt, nil
	}
	header := result.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: result.status, Header: header, Body: io.NopCloser(strings.NewReader(result.body))}, attempt, nil
}

type trackingReadCloser struct {
	io.Reader
	closed     bool
	closeCalls int
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	r.closeCalls++
	return nil
}

type failingReadCloser struct {
	closeCalls int
}

func (r *failingReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("upstream body read failed")
}

func (r *failingReadCloser) Close() error {
	r.closeCalls++
	return nil
}

type fakeReferenceRefresher struct {
	ref              codex.CandidateRef
	revision         codex.Revision
	err              error
	calls            int
	acceptedRef      codex.CandidateRef
	acceptedRevision codex.Revision
}

func (r *fakeReferenceRefresher) RefreshReference(_ context.Context, acceptedRef codex.CandidateRef, acceptedRevision codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	r.calls++
	r.acceptedRef = acceptedRef
	r.acceptedRevision = acceptedRevision
	return r.ref, r.revision, r.err
}

type resolvedAttemptExecutor struct {
	actual CandidateAttempt
	result attemptResult
}

func (e *resolvedAttemptExecutor) Do(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request) (*http.Response, error) {
	response, _, err := e.doOnDispatch(ctx, choice, attempt, req, nil)
	return response, err
}

func (e *resolvedAttemptExecutor) doOnDispatch(_ context.Context, _ RouteChoice, _ CandidateAttempt, _ *http.Request, onDispatch func(CandidateAttempt)) (*http.Response, CandidateAttempt, error) {
	if e.result.err != nil {
		return nil, e.actual, e.result.err
	}
	if onDispatch != nil {
		onDispatch(e.actual)
	}
	header := e.result.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: e.result.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(e.result.body)),
	}, e.actual, nil
}

func requestPlan(account codex.AccountKey, ids ...codex.CandidateID) CodexRequestPlan {
	identity := codex.AccountIdentity{AccountID: "test-account:" + string(account), UserID: "test-user:" + string(account)}
	plan := CodexRequestPlan{Choice: RouteChoice{
		AccountKey: account, RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4",
		RequiredBuckets: []CapacityBucket{CapacityBucketForModel("gpt-5.4")},
	}}
	for index, id := range ids {
		plan.Attempts = append(plan.Attempts, CandidateAttempt{
			AccountKey: account,
			Candidate:  codex.CandidateRef{AccountKey: account, CandidateID: id},
			Revision:   codex.Revision("revision-" + id),
			Source:     codex.SourceSystem,
			Identity:   identity,
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
	refresher := &fakeReferenceRefresher{ref: refreshAttempt.Candidate, revision: "new"}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"managed": {{status: http.StatusUnauthorized}, {status: http.StatusOK}},
	}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Refresher: refresher}
	response, _, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if refresher.calls != 1 || attempt.Candidate != refreshAttempt.Candidate || attempt.Revision != "new" {
		t.Fatalf("refresh calls=%d attempt=%+v", refresher.calls, attempt)
	}
}

func TestCodexRequestRouterRejectsInvalidRefreshReferenceBeforeDispatch(t *testing.T) {
	tests := []struct {
		name     string
		ref      codex.CandidateRef
		revision codex.Revision
	}{
		{name: "different candidate", ref: codex.CandidateRef{AccountKey: "one", CandidateID: "replacement"}, revision: "revision-new"},
		{name: "empty revision", ref: codex.CandidateRef{AccountKey: "one", CandidateID: "managed"}},
		{name: "unchanged revision", ref: codex.CandidateRef{AccountKey: "one", CandidateID: "managed"}, revision: "revision-managed"},
	}
	for _, test := range tests {
		for _, pinned := range []bool{false, true} {
			name := test.name + "/routed"
			if pinned {
				name = test.name + "/pinned"
			}
			t.Run(name, func(t *testing.T) {
				plan := requestPlan("one", "managed")
				refreshAttempt := plan.Attempts[0]
				plan.refreshAttempt = &refreshAttempt
				executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
					"managed": {{status: http.StatusUnauthorized}},
				}}
				router := &CodexRequestRouter{
					Scope:     &queuedRequestScope{plans: []CodexRequestPlan{plan}},
					Executor:  executor,
					Refresher: &fakeReferenceRefresher{ref: test.ref, revision: test.revision},
				}

				var response *http.Response
				var err error
				if pinned {
					response, _, _, err = router.DoPinned(context.Background(), plan.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
				} else {
					response, _, _, err = router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
				}
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusUnauthorized || !slices.Equal(executor.calls, []codex.CandidateID{"managed"}) {
					t.Fatalf("status = %d, calls = %v", response.StatusCode, executor.calls)
				}
			})
		}
	}
}

func TestCodexRequestRouterReturnsActualRelistedRevision(t *testing.T) {
	plan := requestPlan("one", "candidate")
	actual := plan.Attempts[0]
	actual.Revision = "revision-new"
	executor := &resolvedAttemptExecutor{actual: actual, result: attemptResult{status: http.StatusOK}}
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor}

	response, choice, returned, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if choice.AccountKey != plan.Choice.AccountKey || choice.RequestedModel != plan.Choice.RequestedModel ||
		choice.EffectiveModel != plan.Choice.EffectiveModel || returned != actual {
		t.Fatalf("choice = %+v, returned = %+v, want %+v / %+v", choice, returned, plan.Choice, actual)
	}
}

func TestCodexRequestRouterDoPinnedReturnsActualRelistedRevision(t *testing.T) {
	plan := requestPlan("one", "candidate")
	actual := plan.Attempts[0]
	actual.Revision = "revision-new"
	router := &CodexRequestRouter{
		Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}},
		Executor: &resolvedAttemptExecutor{
			actual: actual, result: attemptResult{status: http.StatusOK},
		},
	}

	response, returned, failure, err := router.DoPinned(context.Background(), plan.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if returned != actual || failure != CodexPinnedAccepted {
		t.Fatalf("returned = %+v, failure = %v", returned, failure)
	}
}

func TestCodexRequestRouterRefreshesActualRelistedRevision(t *testing.T) {
	plan := requestPlan("one", "managed")
	refreshAttempt := plan.Attempts[0]
	refreshAttempt.Source = codex.SourceManaged
	plan.Attempts[0] = refreshAttempt
	plan.refreshAttempt = &refreshAttempt
	actual := refreshAttempt
	actual.Revision = "revision-new"
	inventory := staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key: actual.AccountKey, Identity: actual.Identity, Routable: true,
		Candidates: []codex.CredentialCandidate{{
			Ref: actual.Candidate, Revision: actual.Revision, Source: actual.Source, Routable: true,
		}},
	}}}}
	resolver := &testExactSecretResolver{
		errors: map[codex.Revision]error{refreshAttempt.Revision: codex.ErrStaleRevision},
		materials: map[codex.Revision]codex.CredentialMaterial{
			actual.Revision: testExactCredentialMaterial(actual.Identity, "rotated-secret"),
		},
	}
	refresher := &fakeReferenceRefresher{err: codex.ErrRefreshUnavailable}
	executor := &CodexAttemptExecutor{
		Inventory: inventory,
		Secrets:   resolver,
		Transport: &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get("Authorization"); got != "Bearer rotated-secret" {
				t.Fatalf("authorization = %q", got)
			}
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody}, nil
		})},
	}
	router := &CodexRequestRouter{
		Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor, Refresher: refresher,
	}

	response, _, returned, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || returned != actual {
		t.Fatalf("status = %d, returned = %+v, want %+v", response.StatusCode, returned, actual)
	}
	if refresher.calls != 1 || refresher.acceptedRef != actual.Candidate || refresher.acceptedRevision != actual.Revision {
		t.Fatalf("refresh calls = %d, accepted ref = %+v, accepted revision = %q", refresher.calls, refresher.acceptedRef, refresher.acceptedRevision)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.plans) != 2 || resolver.plans[0].Revision != refreshAttempt.Revision || resolver.plans[1].Revision != actual.Revision {
		t.Fatalf("resolved plans = %+v", resolver.plans)
	}
}

func TestCodexRequestRouterFailsOverOnlyForExactHardQuota(t *testing.T) {
	tests := []struct {
		name       string
		first      attemptResult
		wantStatus int
		wantCalls  string
	}{
		{name: "401", first: attemptResult{status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized, wantCalls: "first"},
		{name: "403", first: attemptResult{status: http.StatusForbidden}, wantStatus: http.StatusForbidden, wantCalls: "first"},
		{name: "live hard 429", first: attemptResult{status: http.StatusTooManyRequests, body: codexLiveUsageLimitBody}, wantStatus: http.StatusOK, wantCalls: "first,second"},
		{name: "hard 429", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusOK, wantCalls: "first,second"},
		{name: "hard 429 status alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status_code":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusOK, wantCalls: "first,second"},
		{name: "zero status with matching alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":0,"status_code":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "null status with matching alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":null,"status_code":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "matching status with zero alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"status_code":0,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "matching status with null alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"status_code":null,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "status conflicts with transport", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":400,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "conflicting status aliases", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"status_code":400,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "duplicate top-level status", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":400,"status":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "duplicate top-level status alias", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status_code":400,"status_code":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "duplicate top-level type", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"response.failed","type":"error","status":429,"error":{"type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "duplicate nested error type", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"error":{"type":"rate_limit_exceeded","type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "duplicate nested error code", first: attemptResult{status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"first","code":"second"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "case-variant authority keys", first: attemptResult{status: http.StatusTooManyRequests, body: `{"Type":"error","Status":429,"Error":{"Type":"usage_limit_reached"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
		{name: "legacy insufficient quota", first: attemptResult{status: http.StatusTooManyRequests, body: `{"error":{"type":"insufficient_quota"}}`}, wantStatus: http.StatusTooManyRequests, wantCalls: "first"},
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

func TestCodexRequestRouterReturnsEncodedHard429Unchanged(t *testing.T) {
	bodyText := `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`
	body := &trackingReadCloser{Reader: strings.NewReader(bodyText)}
	header := make(http.Header)
	header.Set("Content-Encoding", "zstd")
	header.Add("X-Upstream", "one")
	header.Add("X-Upstream", "two")
	want := &http.Response{
		Status:     "429 encoded upstream response",
		StatusCode: http.StatusTooManyRequests,
		Header:     header,
		Body:       body,
	}
	scope := &queuedRequestScope{plans: []CodexRequestPlan{
		requestPlan("one", "first"),
		requestPlan("two", "second"),
	}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first":  {{resp: want}},
		"second": {{status: http.StatusOK}},
	}}
	ledger := NewCodexCapacityLedger(time.Now, time.Hour)
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Capacity: ledger}

	response, choice, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != want || choice.AccountKey != "one" || attempt.Candidate.CandidateID != "first" {
		t.Fatalf("response=%p want=%p choice=%q attempt=%q", response, want, choice.AccountKey, attempt.Candidate.CandidateID)
	}
	if scope.calls != 1 || !slices.Equal(executor.calls, []codex.CandidateID{"first"}) {
		t.Fatalf("scope calls=%d attempts=%v", scope.calls, executor.calls)
	}
	if view := ledger.Capacity("one", CapacityBucketForModel("gpt-5.4")); view.State == CapacityZero {
		t.Fatalf("encoded response zeroed capacity: %+v", view)
	}
	if body.closeCalls != 0 {
		t.Fatalf("body close calls before downstream close = %d", body.closeCalls)
	}
	data, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != bodyText || response.Status != want.Status || response.Header.Get("Content-Encoding") != "zstd" || !slices.Equal(response.Header.Values("X-Upstream"), []string{"one", "two"}) {
		t.Fatalf("status=%q encoding=%q headers=%v body=%q", response.Status, response.Header.Get("Content-Encoding"), response.Header, data)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if body.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", body.closeCalls)
	}
}

func TestCodexRequestRouterReturnsUncompressedHard429Unchanged(t *testing.T) {
	bodyText := `{"error":{"type":"usage_limit_reached"}}`
	body := &trackingReadCloser{Reader: strings.NewReader(bodyText)}
	want := &http.Response{
		Status:       "429 transparently decoded upstream response",
		StatusCode:   http.StatusTooManyRequests,
		Header:       make(http.Header),
		Body:         body,
		Uncompressed: true,
	}
	scope := &queuedRequestScope{plans: []CodexRequestPlan{
		requestPlan("one", "first"),
		requestPlan("two", "second"),
	}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first":  {{resp: want}},
		"second": {{status: http.StatusOK}},
	}}
	ledger := NewCodexCapacityLedger(time.Now, time.Hour)
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Capacity: ledger}

	response, choice, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != want || choice.AccountKey != "one" || attempt.Candidate.CandidateID != "first" {
		t.Fatalf("response=%p want=%p choice=%q attempt=%q", response, want, choice.AccountKey, attempt.Candidate.CandidateID)
	}
	if scope.calls != 1 || !slices.Equal(executor.calls, []codex.CandidateID{"first"}) {
		t.Fatalf("scope calls=%d attempts=%v", scope.calls, executor.calls)
	}
	if view := ledger.Capacity("one", CapacityBucketForModel("gpt-5.4")); view.State == CapacityZero {
		t.Fatalf("uncompressed response zeroed capacity: %+v", view)
	}
	data, readErr := io.ReadAll(response.Body)
	if readErr != nil || string(data) != bodyText || !response.Uncompressed {
		t.Fatalf("uncompressed=%v body=%q error=%v", response.Uncompressed, data, readErr)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if body.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", body.closeCalls)
	}
}

func TestCodexRequestRouterReturnsDefaultTransportDecodedGzip429Unchanged(t *testing.T) {
	bodyText := `{"error":{"type":"usage_limit_reached"}}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		writer.WriteHeader(http.StatusTooManyRequests)
		compressed := gzip.NewWriter(writer)
		if _, err := compressed.Write([]byte(bodyText)); err != nil {
			t.Errorf("write gzip response: %v", err)
		}
		if err := compressed.Close(); err != nil {
			t.Errorf("close gzip response: %v", err)
		}
	}))
	defer server.Close()

	want, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer want.Body.Close()
	if !want.Uncompressed || want.Header.Get("Content-Encoding") != "" {
		t.Fatalf("default transport response uncompressed=%v encoding=%q", want.Uncompressed, want.Header.Get("Content-Encoding"))
	}
	scope := &queuedRequestScope{plans: []CodexRequestPlan{
		requestPlan("one", "first"),
		requestPlan("two", "second"),
	}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first":  {{resp: want}},
		"second": {{status: http.StatusOK}},
	}}
	ledger := NewCodexCapacityLedger(time.Now, time.Hour)
	router := &CodexRequestRouter{Scope: scope, Executor: executor, Capacity: ledger}

	response, choice, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != want || choice.AccountKey != "one" || attempt.Candidate.CandidateID != "first" {
		t.Fatalf("response=%p want=%p choice=%q attempt=%q", response, want, choice.AccountKey, attempt.Candidate.CandidateID)
	}
	if scope.calls != 1 || !slices.Equal(executor.calls, []codex.CandidateID{"first"}) {
		t.Fatalf("scope calls=%d attempts=%v", scope.calls, executor.calls)
	}
	if view := ledger.Capacity("one", CapacityBucketForModel("gpt-5.4")); view.State == CapacityZero {
		t.Fatalf("default transport decoded response zeroed capacity: %+v", view)
	}
	data, readErr := io.ReadAll(response.Body)
	if readErr != nil || string(data) != bodyText || !response.Uncompressed {
		t.Fatalf("uncompressed=%v body=%q error=%v", response.Uncompressed, data, readErr)
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
	body := codexLiveUsageLimitBody
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

func TestCodexRequestRouterReturnsFinalUnauthorizedResponse(t *testing.T) {
	firstBody := &trackingReadCloser{Reader: strings.NewReader("first rejected")}
	lastBody := &trackingReadCloser{Reader: strings.NewReader("last rejected")}
	firstResponse := &http.Response{
		Status:     "401 account rejected",
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"X-Upstream": {"first"}},
		Body:       firstBody,
	}
	lastResponse := &http.Response{
		Status:     "403 policy rejected",
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Upstream": {"last-a", "last-b"}},
		Body:       lastBody,
	}
	plan := requestPlan("one", "first", "last")
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first": {{resp: firstResponse}},
		"last":  {{resp: lastResponse}},
	}}
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor}

	response, choice, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != lastResponse || choice.AccountKey != "one" || attempt.Candidate.CandidateID != "last" {
		t.Fatalf("response=%p choice=%q attempt=%q", response, choice.AccountKey, attempt.Candidate.CandidateID)
	}
	if !firstBody.closed || lastBody.closed {
		t.Fatalf("closed first=%v last=%v", firstBody.closed, lastBody.closed)
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.Status != "403 policy rejected" || string(data) != "last rejected" || strings.Join(response.Header.Values("X-Upstream"), ",") != "last-a,last-b" {
		t.Fatalf("status=%q body=%q headers=%v", response.Status, data, response.Header)
	}
}

func TestCodexRequestRouterClosesRejectedResponseBeforeReplacementAttempt(t *testing.T) {
	plan := requestPlan("one", "first", "second")
	firstBody := &trackingReadCloser{Reader: strings.NewReader("first")}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first": {{resp: &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       firstBody,
		}}},
		"second": {{
			status: http.StatusOK,
			before: func() {
				if !firstBody.closed {
					t.Error("first rejected response remained open during replacement dispatch")
				}
			},
		}},
	}}
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor}

	response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
}

func TestCodexRequestRouterReturnsRejectedResponseWhenRefreshFailsBeforeDispatch(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "routed"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			plan := requestPlan("one", "managed")
			refreshAttempt := plan.Attempts[0]
			plan.refreshAttempt = &refreshAttempt
			body := &trackingReadCloser{Reader: strings.NewReader("upstream auth body")}
			header := make(http.Header)
			header.Add("X-Upstream-Value", "one")
			header.Add("X-Upstream-Value", "two")
			want := &http.Response{StatusCode: http.StatusUnauthorized, Header: header, Body: body}
			router := &CodexRequestRouter{
				Scope:     &queuedRequestScope{plans: []CodexRequestPlan{plan}},
				Executor:  &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{"managed": {{resp: want}}}},
				Refresher: &fakeReferenceRefresher{err: errors.New("refresh broker unavailable")},
			}

			var response *http.Response
			var failure CodexPinnedFailure
			var err error
			if pinned {
				response, _, failure, err = router.DoPinned(context.Background(), plan.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
			} else {
				response, _, _, err = router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
				failure = CodexPinnedAuthFailure
			}
			if err != nil {
				t.Fatal(err)
			}
			if response != want || failure != CodexPinnedAuthFailure || body.closed {
				t.Fatalf("response=%p want=%p failure=%v closed=%v", response, want, failure, body.closed)
			}
			if got := response.Header.Values("X-Upstream-Value"); !slices.Equal(got, []string{"one", "two"}) {
				t.Fatalf("headers = %v", got)
			}
			data, readErr := io.ReadAll(response.Body)
			if readErr != nil || string(data) != "upstream auth body" {
				t.Fatalf("body=%q error=%v", data, readErr)
			}
			_ = response.Body.Close()
		})
	}
}

func TestCodexRequestRouterInventoryDegradedOverridesRetainedRejection(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "routed"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			first := requestPlan("one", "managed", "external")
			scope := &queuedRequestScope{plans: []CodexRequestPlan{first, requestPlan("two", "other")}}
			body := &trackingReadCloser{Reader: strings.NewReader("private upstream rejection")}
			executor := &queuedPredispatchAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
				"managed":  {{resp: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body}}},
				"external": {{err: codex.ErrCredentialInventoryDegraded}},
				"other":    {{status: http.StatusOK}},
			}}
			router := &CodexRequestRouter{Scope: scope, Executor: executor}

			var response *http.Response
			var err error
			if pinned {
				response, _, _, err = router.DoPinned(context.Background(), first.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
			} else {
				response, _, _, err = router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
			}
			if response != nil || !errors.Is(err, codex.ErrCredentialInventoryDegraded) {
				t.Fatalf("response=%p error=%v", response, err)
			}
			if body.closeCalls != 1 {
				t.Fatalf("retained body close calls = %d, want 1", body.closeCalls)
			}
			if !slices.Equal(executor.calls, []codex.CandidateID{"managed", "external"}) {
				t.Fatalf("attempt calls = %v", executor.calls)
			}
			wantScopeCalls := 1
			if pinned {
				wantScopeCalls = 0
			}
			if scope.calls != wantScopeCalls {
				t.Fatalf("scope calls = %d, want %d", scope.calls, wantScopeCalls)
			}
		})
	}
}

func TestCodexRequestRouterRefreshDegradedOverridesRetainedRejection(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "routed"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			first := requestPlan("one", "managed")
			refreshAttempt := first.Attempts[0]
			first.refreshAttempt = &refreshAttempt
			scope := &queuedRequestScope{plans: []CodexRequestPlan{first, requestPlan("two", "other")}}
			body := &trackingReadCloser{Reader: strings.NewReader("private upstream rejection")}
			executor := &queuedPredispatchAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
				"managed": {{resp: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body}}},
				"other":   {{status: http.StatusOK}},
			}}
			router := &CodexRequestRouter{
				Scope: scope, Executor: executor,
				Refresher: &fakeReferenceRefresher{err: codex.ErrCredentialInventoryDegraded},
			}

			var response *http.Response
			var err error
			if pinned {
				response, _, _, err = router.DoPinned(context.Background(), first.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
			} else {
				response, _, _, err = router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
			}
			if response != nil || !errors.Is(err, codex.ErrCredentialInventoryDegraded) {
				t.Fatalf("response=%p error=%v", response, err)
			}
			if body.closeCalls != 1 {
				t.Fatalf("retained body close calls = %d, want 1", body.closeCalls)
			}
			if !slices.Equal(executor.calls, []codex.CandidateID{"managed"}) {
				t.Fatalf("attempt calls = %v", executor.calls)
			}
			wantScopeCalls := 1
			if pinned {
				wantScopeCalls = 0
			}
			if scope.calls != wantScopeCalls {
				t.Fatalf("scope calls = %d, want %d", scope.calls, wantScopeCalls)
			}
		})
	}
}

func TestCodexRequestRouterPlanningDegradedOverridesRetainedRejection(t *testing.T) {
	t.Run("routed", func(t *testing.T) {
		first := requestPlan("one", "managed")
		scope := &queuedRequestScope{plans: []CodexRequestPlan{first}, exhaustedErr: codex.ErrCredentialInventoryDegraded}
		body := &trackingReadCloser{Reader: strings.NewReader(codexLiveUsageLimitBody)}
		executor := &queuedPredispatchAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
			"managed": {{resp: &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: body}}},
		}}
		router := &CodexRequestRouter{Scope: scope, Executor: executor}

		response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
		if response != nil || !errors.Is(err, codex.ErrCredentialInventoryDegraded) {
			t.Fatalf("response=%p error=%v", response, err)
		}
		if body.closeCalls != 1 || scope.calls != 2 || !slices.Equal(executor.calls, []codex.CandidateID{"managed"}) {
			t.Fatalf("close calls=%d scope calls=%d attempts=%v", body.closeCalls, scope.calls, executor.calls)
		}
	})

	t.Run("pinned", func(t *testing.T) {
		choice := requestPlan("one", "managed").Choice
		scope := &queuedRequestScope{exhaustedErr: codex.ErrCredentialInventoryDegraded}
		body := &trackingReadCloser{Reader: strings.NewReader("private upstream rejection")}
		prior := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body}
		router := &CodexRequestRouter{Scope: scope, Executor: &queuedPredispatchAttemptExecutor{}}

		response, _, _, _, err := router.doPinned(context.Background(), choice, makeCodexRequest(`{"model":"gpt-5.4"}`), prior)
		if response != nil || !errors.Is(err, codex.ErrCredentialInventoryDegraded) {
			t.Fatalf("response=%p error=%v", response, err)
		}
		if body.closeCalls != 1 {
			t.Fatalf("retained body close calls = %d, want 1", body.closeCalls)
		}
	})
}

func TestCodexRequestRouterReturnsRetainedResponseWhenRealExecutorFailsBeforeDispatch(t *testing.T) {
	tests := []struct {
		name             string
		includeSecondKey bool
		mutateRequest    func(*http.Request)
	}{
		{name: "secret resolution", includeSecondKey: false},
		{
			name:             "request preparation",
			includeSecondKey: true,
			mutateRequest: func(request *http.Request) {
				body := []byte(`{"model":"gpt-5.4"}`)
				calls := 0
				request.GetBody = func() (io.ReadCloser, error) {
					calls++
					if calls > 1 {
						return nil, errors.New("request replay unavailable")
					}
					return io.NopCloser(bytes.NewReader(body)), nil
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := requestPlan("one", "first", "replacement")
			materials := map[codex.CandidateID]codex.CredentialMaterial{
				"first": {AccessToken: "first-secret"},
			}
			if test.includeSecondKey {
				materials["replacement"] = codex.CredentialMaterial{AccessToken: "replacement-secret"}
			}
			resolver := &testSecretResolver{materials: materials}
			body := &trackingReadCloser{Reader: strings.NewReader("retained upstream rejection")}
			retained := &http.Response{
				Status:     "401 retained upstream response",
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"X-Upstream": {"one", "two"}},
				Body:       body,
			}
			roundTrips := 0
			transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
				roundTrips++
				if roundTrips != 1 {
					t.Fatalf("round trips = %d, replacement reached upstream", roundTrips)
				}
				return retained, nil
			})}
			router := &CodexRequestRouter{
				Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}},
				Executor: &CodexAttemptExecutor{
					Secrets:   resolver,
					Transport: transport,
				},
			}
			request := makeCodexRequest(`{"model":"gpt-5.4"}`)
			if test.mutateRequest != nil {
				test.mutateRequest(request)
			}

			response, choice, attempt, err := router.Do(context.Background(), CodexRouteRequirements{}, request)
			if err != nil {
				t.Fatalf("predispatch error replaced retained response: %v", err)
			}
			if response != retained || choice.AccountKey != "one" || attempt.Candidate.CandidateID != "first" {
				t.Fatalf("response=%p want=%p choice=%q attempt=%q", response, retained, choice.AccountKey, attempt.Candidate.CandidateID)
			}
			if resolver.calls != 2 || roundTrips != 1 || body.closeCalls != 0 {
				t.Fatalf("secret resolutions=%d round trips=%d retained close calls=%d", resolver.calls, roundTrips, body.closeCalls)
			}
			if !slices.Equal(response.Header.Values("X-Upstream"), []string{"one", "two"}) {
				t.Fatalf("headers = %v", response.Header)
			}
			data, readErr := io.ReadAll(response.Body)
			if readErr != nil || string(data) != "retained upstream rejection" {
				t.Fatalf("body=%q error=%v", data, readErr)
			}
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if body.closeCalls != 1 {
				t.Fatalf("retained close calls = %d, want 1", body.closeCalls)
			}
		})
	}
}

func TestCodexRequestRouterDoPinnedReturnsFinalUnauthorizedResponse(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("rejected")}
	want := &http.Response{
		Status:     "403 exact upstream status",
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Upstream": {"exact"}},
		Body:       body,
	}
	plan := requestPlan("one", "only")
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{"only": {{resp: want}}}}
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor}

	response, attempt, failure, err := router.DoPinned(context.Background(), plan.Choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != want || attempt.Candidate.CandidateID != "only" || failure != CodexPinnedAuthFailure || body.closed {
		t.Fatalf("response=%p attempt=%q failure=%v closed=%v", response, attempt.Candidate.CandidateID, failure, body.closed)
	}
	response.Body.Close()
}

func TestCodexRequestRouterReturnsOversizedUnknown429Unchanged(t *testing.T) {
	prefix := []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
	padding := bytes.Repeat([]byte(" "), codexAttemptResponseLimit+17-len(prefix))
	suffix := []byte("::oversized-response-tail::")
	wantBody := bytes.Join([][]byte{prefix, padding, suffix}, nil)
	body := &trackingReadCloser{Reader: bytes.NewReader(wantBody)}
	want := &http.Response{
		Status:        "429 upstream overload",
		StatusCode:    http.StatusTooManyRequests,
		Header:        http.Header{"X-Upstream": {"one", "two"}},
		Body:          body,
		ContentLength: int64(len(wantBody)),
	}
	scope := &queuedRequestScope{plans: []CodexRequestPlan{
		requestPlan("one", "first"),
		requestPlan("two", "second"),
	}}
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"first":  {{resp: want}},
		"second": {{status: http.StatusOK, body: "unexpected failover"}},
	}}
	router := &CodexRequestRouter{Scope: scope, Executor: executor}

	response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response != want || body.closed {
		t.Fatalf("response=%p want=%p closed=%v", response, want, body.closed)
	}
	if scope.calls != 1 || !slices.Equal(executor.calls, []codex.CandidateID{"first"}) {
		t.Fatalf("plan calls=%d executor calls=%v, want one attempt on first", scope.calls, executor.calls)
	}
	gotBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(gotBody), len(wantBody))
	}
	gotPrefix := gotBody[:len(prefix)]
	gotPadding := gotBody[len(prefix) : len(prefix)+len(padding)]
	gotSuffix := gotBody[len(prefix)+len(padding):]
	if !bytes.Equal(gotPrefix, prefix) || !bytes.Equal(gotPadding, padding) || !bytes.Equal(gotSuffix, suffix) {
		t.Fatalf("replayed segments differ: prefix=%t padding=%t suffix=%t", bytes.Equal(gotPrefix, prefix), bytes.Equal(gotPadding, padding), bytes.Equal(gotSuffix, suffix))
	}
	if response.Status != "429 upstream overload" || response.StatusCode != http.StatusTooManyRequests || response.ContentLength != int64(len(wantBody)) || !slices.Equal(response.Header.Values("X-Upstream"), []string{"one", "two"}) {
		t.Fatalf("body bytes=%d status=%q length=%d headers=%v", len(gotBody), response.Status, response.ContentLength, response.Header)
	}
	if body.closeCalls != 0 {
		t.Fatalf("body close calls before downstream close = %d, want 0", body.closeCalls)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if body.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", body.closeCalls)
	}
}

func TestCodexRequestRouterClosesUnreadable429Once(t *testing.T) {
	body := &failingReadCloser{}
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       body,
	}
	plan := requestPlan("one", "only")
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{
		"only": {{resp: response}},
	}}
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor}

	got, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err == nil || !strings.Contains(err.Error(), "upstream body read failed") {
		t.Fatalf("response=%p error=%v", got, err)
	}
	if got != nil {
		t.Fatalf("response = %p, want nil", got)
	}
	if body.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", body.closeCalls)
	}
}

func TestCodexRequestRouterHardLimitZerosOnlyEffectiveModelBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	plan := requestPlan("one", "only")
	sparkBucket := CapacityBucketForModel(codexSparkModel)
	plan.Choice = RouteChoice{
		AccountKey:      "one",
		RequestedModel:  codexSparkModel,
		EffectiveModel:  codexFallbackModel,
		RequiredBuckets: []CapacityBucket{sparkBucket, CapacityBucketBase},
	}
	body := `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`
	executor := &queuedAttemptExecutor{results: map[codex.CandidateID][]attemptResult{"only": {{status: http.StatusTooManyRequests, body: body}}}}
	router := &CodexRequestRouter{
		Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Executor: executor,
		Capacity: ledger, Now: func() time.Time { return now },
	}

	response, _, _, err := router.Do(context.Background(), CodexRouteRequirements{}, makeCodexRequest(`{"model":"gpt-5.3-codex-spark"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if view := ledger.Capacity("one", CapacityBucketBase); view.State != CapacityZero {
		t.Fatalf("effective bucket = %+v", view)
	}
	ledger.mu.RLock()
	_, requestedObserved := ledger.facts[capacityFactKey{account: "one", bucket: sparkBucket, source: CapacitySourceHardLimit}]
	ledger.mu.RUnlock()
	if requestedObserved {
		t.Fatal("hard limit was recorded against the requested model bucket")
	}
}

func candidateStrings(ids []codex.CandidateID) []string {
	result := make([]string, len(ids))
	for index, id := range ids {
		result[index] = string(id)
	}
	return result
}
