package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
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
