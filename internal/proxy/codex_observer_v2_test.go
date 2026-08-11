package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexV2ObserverPersistsOnlyFinalAcceptedLegacyDispatch(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10, Authoritative: false}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	if observer.Store != nil {
		t.Fatal("v2 observer retained a legacy journal store")
	}
	if observer.Leases.mu != coordinator.leases.mu || observer.Prewarm != coordinator.prewarms {
		t.Fatal("v2 observer did not reuse the continuity coordinator core")
	}

	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-final")
	firstChoice := codexV2ObserverTestChoice("account-a")
	finalChoice := codexV2ObserverTestChoice("account-b")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), firstChoice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	observeCodexAttempt(withCodexObservation(context.Background(), handle), finalChoice, codexV2ObserverTestAttempt("account-b", "candidate-b"))
	handle.Selected(finalChoice, true)
	if choice, found, err := handle.pinnedChoice(); err != nil || found || choice.AccountKey != "" || choice.RequestedModel != "" || choice.EffectiveModel != "" || len(choice.RequiredBuckets) != 0 {
		t.Fatalf("v2 observer made a prospective decision: choice=%#v found=%t err=%v", choice, found, err)
	}

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":       {"text/event-stream"},
			"X-Codex-Turn-State": {"private-response-state"},
		},
		Body: io.NopCloser(strings.NewReader(completedSSE("private-response-id"))),
	}
	if err := handle.PrepareV2Response(response); err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(*codexV2ObservedBody); !ok {
		t.Fatalf("accepted body = %T, want v2 observer", response.Body)
	}
	handle.Response(response)
	observeCodexResponseBody(response, handle)
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := coordinator.Store().LoadLane(handle.key, []codex.AccountKey{"account-a", "account-b"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	var record CodexJournalRecordV2
	for _, resolved := range restored.ResolvedRecords {
		if resolved.Identity == restored.RequestedIdentity {
			record = resolved.Record
			break
		}
	}
	if restored.Classification != CodexRestoredLaneCurrent || !record.EverAdmitted || record.Authoritative || record.State != LeaseBoundQuiescent || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 {
		t.Fatalf("final shadow record = %#v, classification=%s", record, restored.Classification)
	}
	store := coordinator.Store()
	if record.AccountHash != store.hash("account", "account-b") {
		t.Fatalf("account hash = %q, want final accepted account", record.AccountHash)
	}
	if len(record.AttemptEnvelope.Slots) != 1 || record.AttemptEnvelope.Slots[0].AccountHash != store.hash("account", "account-b") || record.AttemptEnvelope.Slots[0].CandidateHash != store.hash("candidate", "candidate-b") {
		t.Fatalf("durable slots = %#v, want only final accepted dispatch", record.AttemptEnvelope.Slots)
	}
	if record.HasTurnState != true || record.HasResponseAnchor != true || record.HasEncryptedState {
		t.Fatalf("durable response evidence = %#v", record)
	}
}

func TestCodexV2ObserverFailsClosedBeforeWrappingOnCommitError(t *testing.T) {
	owner := &codexLeaseRuntimeFailingOwner{err: errors.New("forced owner failure")}
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinatorWithOwner(t, owner)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10, Authoritative: false}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-commit-error")
	choice := codexV2ObserverTestChoice("account-a")
	attempt := codexV2ObserverTestAttempt("account-a", "candidate-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, attempt)
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completedSSE("response")))}
	generation := coordinator.Store().Generation()
	owner.failAt.Store(owner.begins.Load() + 1)

	if err := handle.PrepareV2Response(response); err == nil {
		t.Fatal("PrepareV2Response succeeded despite durable commit failure")
	}
	if coordinator.Store().Generation() != generation {
		t.Fatalf("journal generation = %d, want unchanged %d", coordinator.Store().Generation(), generation)
	}
	if _, ok := response.Body.(*codexV2ObservedBody); ok {
		t.Fatal("response body was wrapped after failed durable admission")
	}
}

func TestCodexV2ObserverAbandonsPreparedRecordWhenMarkDispatchedFails(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	stageErr := errors.New("forced mark failure")
	revalidations := 0
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		revalidations++
		if revalidations == 2 {
			return stageErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(runtimeLease, policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-mark-failure")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completedSSE("response")))}

	if err := handle.PrepareV2Response(response); !errors.Is(err, stageErr) {
		t.Fatalf("PrepareV2Response error = %v, want mark failure", err)
	}
	record := codexV2ObserverTestRecord(t, coordinator, handle.key, policy)
	if state := codexLeaseCurrentAttemptState(record); record.State != LeaseProvisional || state != CodexAttemptAbandonedBeforeDispatch || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
		t.Fatalf("prepared cleanup record = %#v, attempt=%v", record, state)
	}
}

func TestCodexV2ObserverAbandonsPreparedRecordWhenBeginReturnsHandleAndError(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(runtimeLease, policy)
	if err != nil {
		t.Fatal(err)
	}
	stageErr := errors.New("forced begin result failure")
	observer.v2.runtime = codexV2ObserverBeginErrorRuntime{runtime: runtimeLease, err: stageErr}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-begin-result-failure")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	handle.ctx = canceled
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completedSSE("response")))}

	if err := handle.PrepareV2Response(response); !errors.Is(err, stageErr) {
		t.Fatalf("PrepareV2Response error = %v, want begin result failure", err)
	}
	record := codexV2ObserverTestRecord(t, coordinator, handle.key, policy)
	if state := codexLeaseCurrentAttemptState(record); record.State != LeaseProvisional || state != CodexAttemptAbandonedBeforeDispatch || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
		t.Fatalf("begin cleanup record = %#v, attempt=%v", record, state)
	}
}

func TestCodexV2ObserverFailsClosedWhenBeginReturnsNilHandle(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	observer.v2.runtime = codexV2ObserverNilRuntime{}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-begin-nil-handle")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completedSSE("response")))}
	generation := coordinator.Store().Generation()

	if err := handle.PrepareV2Response(response); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("PrepareV2Response error = %v, want writer unavailable", err)
	}
	if coordinator.Store().Generation() != generation {
		t.Fatalf("nil Begin handle changed journal generation: got %d want %d", coordinator.Store().Generation(), generation)
	}
}

func TestCodexV2ObserverIndeterminateDrainsWhenAdmissionFails(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	stageErr := errors.New("forced admission failure")
	revalidations := 0
	runtimeLease, err := NewCodexLeaseRuntime(coordinator, func(context.Context, codex.AccountKey) error {
		revalidations++
		if revalidations == 3 {
			return stageErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(runtimeLease, policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-admission-failure")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completedSSE("response")))}

	if err := handle.PrepareV2Response(response); !errors.Is(err, stageErr) {
		t.Fatalf("PrepareV2Response error = %v, want admission failure", err)
	}
	record := codexV2ObserverTestRecord(t, coordinator, handle.key, policy)
	if state := codexLeaseCurrentAttemptState(record); record.State != LeaseOrphaned || state != CodexAttemptIndeterminate || !record.NonMigratable || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
		t.Fatalf("admission cleanup record = %#v, attempt=%v", record, state)
	}
}

func TestCodexV2ObserverReadPanicIsPrivateAndDrainsIndeterminate(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-read-panic")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: codexV2ObserverPanickingReadBody{}}
	if err := handle.PrepareV2Response(response); err != nil {
		t.Fatal(err)
	}

	read, err := response.Body.Read(make([]byte, 1))
	if read != 0 || err == nil {
		t.Fatalf("panicking Read = (%d, %v), want private-safe error", read, err)
	}
	if strings.Contains(err.Error(), "private read panic") {
		t.Fatalf("panicking Read exposed private panic: %v", err)
	}
	assertCodexV2ObserverIndeterminateDrained(t, codexV2ObserverTestRecord(t, coordinator, handle.key, policy))
}

func TestCodexV2ObserverClosePanicIsPrivateAndDrainsIndeterminate(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-close-panic")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: codexV2ObserverPanickingCloseBody{Reader: strings.NewReader("")}}
	if err := handle.PrepareV2Response(response); err != nil {
		t.Fatal(err)
	}

	err = response.Body.Close()
	if err == nil {
		t.Fatal("panicking Close error = nil, want private-safe error")
	}
	if strings.Contains(err.Error(), "private close panic") {
		t.Fatalf("panicking Close exposed private panic: %v", err)
	}
	assertCodexV2ObserverIndeterminateDrained(t, codexV2ObserverTestRecord(t, coordinator, handle.key, policy))
}

func TestCodexV2ObserverPrepareFailureDoesNotChangeUpstreamResponse(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		compact bool
	}{
		{name: "responses", path: legacyCodexResponsesPath},
		{name: "compact", path: legacyCodexCompactResponsesPath, compact: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), CodexLeaseAuthorityPolicy{ModeEpoch: 10})
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			router := testCodexRequestRouter(
				&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "token", AccountID: "account-a"}},
				roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       codexV2ObserverPanickingCloseBody{Reader: strings.NewReader("response"), closed: &closed},
					}, nil
				}),
			)
			server := &Server{
				Config:        &Config{CodexUpstream: "https://codex.example"},
				CodexRequests: router,
				CodexObserver: observer,
			}
			requestBody := `{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"observer-session","thread_id":"observer-thread","turn_id":"handler-close-panic","request_kind":"turn"}}}`
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			if test.compact {
				server.handleNativeCodexCompact(response, request, test.path)
			} else {
				server.handleNativeCodex(response, request)
			}

			if response.Code != http.StatusOK || response.Body.String() != "response" {
				t.Fatalf("status/body = %d/%q, want %d/%q", response.Code, response.Body.String(), http.StatusOK, "response")
			}
			if !closed {
				t.Fatal("relayed upstream response body was not closed")
			}
			if strings.Contains(response.Body.String(), "private close panic") {
				t.Fatalf("handler exposed private close panic: %s", response.Body.String())
			}
		})
	}
}

func TestCodexV2ObserverDoesNotPersistRejectedResponse(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 10, Authoritative: false}
	observer, err := NewCodexV2TurnObserver(newCodexLeaseRuntimeTest(t, coordinator), policy)
	if err != nil {
		t.Fatal(err)
	}
	handle := beginCodexV2ObserverTestTurn(t, observer, "turn-rejected")
	choice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), handle), choice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	handle.Selected(choice, false)
	generation := coordinator.Store().Generation()
	response := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rejected"))}

	if err := handle.PrepareV2Response(response); err != nil {
		t.Fatal(err)
	}
	if coordinator.Store().Generation() != generation {
		t.Fatalf("rejected response changed journal generation: got %d want %d", coordinator.Store().Generation(), generation)
	}
	if _, ok := response.Body.(*codexV2ObservedBody); ok {
		t.Fatal("rejected response acquired a durable observer")
	}
}

func TestCodexV2ObserversShareLiveHTTPWebSocketShadowCoreWithoutRouting(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	httpObserver, err := NewCodexV2TurnObserver(runtimeLease, CodexLeaseAuthorityPolicy{ModeEpoch: 10})
	if err != nil {
		t.Fatal(err)
	}
	wsObserver, err := NewCodexV2TurnObserver(runtimeLease, CodexLeaseAuthorityPolicy{ModeEpoch: 11})
	if err != nil {
		t.Fatal(err)
	}
	if httpObserver.Leases.mu != wsObserver.Leases.mu || httpObserver.Leases.mu != coordinator.leases.mu {
		t.Fatal("HTTP and WebSocket observers do not share the coordinator live core")
	}

	httpHandle := beginCodexV2ObserverTestTurn(t, httpObserver, "crossover")
	httpChoice := codexV2ObserverTestChoice("account-a")
	observeCodexAttempt(withCodexObservation(context.Background(), httpHandle), httpChoice, codexV2ObserverTestAttempt("account-a", "candidate-a"))
	httpHandle.Selected(httpChoice, false)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(completedSSE("response-a")))}
	if err := httpHandle.PrepareV2Response(response); err != nil {
		t.Fatal(err)
	}
	httpHandle.Response(response)
	defer response.Body.Close()

	frame := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"observer-session","thread_id":"observer-thread","turn_id":"crossover","request_kind":"turn"}}}`)
	wsHandle := wsObserver.BeginWebSocket(context.Background(), frame, nil, 1)
	if choice, found, err := wsHandle.pinnedChoice(); err != nil || found || choice.AccountKey != "" {
		t.Fatalf("WebSocket observer supplied a route decision: choice=%#v found=%t err=%v", choice, found, err)
	}
	wsChoice := codexV2ObserverTestChoice("account-b")
	wsHandle.Selected(wsChoice, false)
	wsHandle.ResponseHeaders(http.StatusSwitchingProtocols, make(http.Header))
	if health := wsObserver.Health(); health.ContinuityErrors != 1 {
		t.Fatalf("WebSocket shadow health = %#v, want one observed crossover mismatch", health)
	}
	if got := len(coordinator.Store().v2.Records); got != 1 {
		t.Fatalf("WebSocket observation created durable routing authority: records=%d", got)
	}
}

func beginCodexV2ObserverTestTurn(t *testing.T, observer *CodexTurnObserver, turn string) *CodexTurnObservation {
	t.Helper()
	body := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"observer-session","thread_id":"observer-thread","turn_id":"` + turn + `","request_kind":"turn"}}}`)
	handle := observer.BeginHTTP(context.Background(), body, "identity", "", false)
	if handle == nil || !handle.request.Metadata.Strong {
		t.Fatalf("observation = %#v, want strong turn", handle)
	}
	handle.observeV2RequestHeaders(make(http.Header))
	return handle
}

func codexV2ObserverTestChoice(account codex.AccountKey) RouteChoice {
	return RouteChoice{
		AccountKey:      account,
		RequestedModel:  "gpt-5.4",
		EffectiveModel:  "gpt-5.4",
		RequiredBuckets: []CapacityBucket{CapacityBucketForModel("gpt-5.4")},
	}
}

func codexV2ObserverTestAttempt(account codex.AccountKey, candidate codex.CandidateID) CandidateAttempt {
	return CandidateAttempt{
		AccountKey: account,
		Candidate:  codex.CandidateRef{AccountKey: account, CandidateID: candidate},
		Revision:   "revision",
		Source:     codex.SourceSystem,
		Ordinal:    1,
	}
}

func codexV2ObserverTestRecord(t *testing.T, coordinator *CodexContinuityCoordinator, key LeaseKey, policy CodexLeaseAuthorityPolicy) CodexJournalRecordV2 {
	t.Helper()
	restored, err := coordinator.Store().LoadLane(key, []codex.AccountKey{"account-a", "account-b"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, resolved := range restored.ResolvedRecords {
		if resolved.Identity == restored.RequestedIdentity {
			return resolved.Record
		}
	}
	t.Fatalf("requested record is absent: %#v", restored)
	return CodexJournalRecordV2{}
}

func assertCodexV2ObserverIndeterminateDrained(t *testing.T, record CodexJournalRecordV2) {
	t.Helper()
	if state := codexLeaseCurrentAttemptState(record); record.State != LeaseOrphaned || state != CodexAttemptIndeterminate || !record.NonMigratable || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
		t.Fatalf("terminal record = %#v, attempt=%v; want drained indeterminate", record, state)
	}
}

type codexV2ObserverPanickingReadBody struct{}

func (codexV2ObserverPanickingReadBody) Read([]byte) (int, error) {
	panic("private read panic")
}

func (codexV2ObserverPanickingReadBody) Close() error { return nil }

type codexV2ObserverPanickingCloseBody struct {
	io.Reader
	closed *bool
}

func (body codexV2ObserverPanickingCloseBody) Close() error {
	if body.closed != nil {
		*body.closed = true
	}
	panic("private close panic")
}

type codexV2ObserverBeginErrorRuntime struct {
	runtime *CodexLeaseRuntime
	err     error
}

func (runtime codexV2ObserverBeginErrorRuntime) BeginRequestContext(_ context.Context, plan CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	handle, err := runtime.runtime.BeginRequestContext(context.Background(), plan)
	if err != nil {
		return handle, err
	}
	return handle, runtime.err
}

type codexV2ObserverNilRuntime struct{}

func (codexV2ObserverNilRuntime) BeginRequestContext(context.Context, CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	return nil, nil
}
