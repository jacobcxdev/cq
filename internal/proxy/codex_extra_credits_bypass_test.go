package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type extraCreditsBypassPolicy map[codex.AccountKey]bool

func (p extraCreditsBypassPolicy) Allowed(account codex.AccountKey) (bool, error) {
	allowed, exists := p[account]
	return !exists || allowed, nil
}

type extraCreditsBypassUsage func(context.Context, codex.AccountKey) (codex.UsageObservation, error)

func (f extraCreditsBypassUsage) Read(ctx context.Context, account codex.AccountKey) (codex.UsageObservation, error) {
	return f(ctx, account)
}

func extraCreditsBypassObservation(remaining int) codex.UsageObservation {
	return codex.ParseUsageObservation([]byte(fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"allowed":%t,"limit_reached":%t,"primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}},"credits":{"has_credits":true,"balance":"100"}}`, remaining > 0, remaining == 0, 100-remaining, time.Now().Add(time.Hour).Unix())), "", "")
}

func extraCreditsBypassExecutor(t *testing.T, guard *CodexExtraCreditsGuard, transport http.RoundTripper) (*CodexAttemptExecutor, RouteChoice, CandidateAttempt) {
	t.Helper()
	identity := codex.AccountIdentity{AccountID: "strong-account-1", UserID: "strong-user-1"}
	choice := RouteChoice{AccountKey: "account-1", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4", RequiredBuckets: []CapacityBucket{CapacityBucketBase}}
	attempt := CandidateAttempt{AccountKey: choice.AccountKey, Candidate: codex.CandidateRef{AccountKey: choice.AccountKey, CandidateID: "candidate-1"}, Revision: "revision-1", Source: codex.SourceSystem, Identity: identity, Ordinal: 1}
	executor := &CodexAttemptExecutor{
		ExtraCredits: guard,
		Secrets: &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
			attempt.Revision: testExactCredentialMaterial(identity, "fixture-secret"),
		}},
		Transport: &CodexTokenTransport{Inner: transport},
	}
	return executor, choice, attempt
}

func TestCodexExtraCreditsPinnedAndDefaultCannotDispatch(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprintf("frozen=%v", frozen), func(t *testing.T) {
			calls := 0
			guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
				return extraCreditsBypassObservation(0), nil
			})}
			executor, choice, attempt := extraCreditsBypassExecutor(t, guard, codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
			}))
			logical := frozenDispatchTestLogicalAccount(choice.AccountKey, frozenDispatchCandidate(choice.AccountKey, attempt.Candidate.CandidateID, attempt.Revision, codex.SourceSystem, false, time.Now().Add(time.Hour)))
			plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{logical}}, Capacity: NewCodexCapacityLedger(time.Now, time.Minute), Requirements: CodexRouteRequirements{RequestedModel: choice.RequestedModel}, BoundAccountKey: choice.AccountKey, DefaultAccountKey: choice.AccountKey, Now: time.Now()})
			if err != nil || len(plan.Accounts()) != 1 {
				t.Fatalf("pinned/default plan = %#v, %v", plan, err)
			}
			if frozen {
				_, _, dispatched, dispatchErr := executor.DispatchFrozen(context.Background(), plan.Accounts()[0].Choice(), attempt, makeCodexRequest(`{"model":"gpt-5.4"}`), func(CandidateAttempt) error { t.Fatal("disabled account crossed dispatch fence"); return nil })
				err = dispatchErr
				if dispatched {
					t.Fatal("disabled account marked dispatched")
				}
			} else {
				router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{{Choice: choice, Attempts: []CandidateAttempt{attempt}}}}, Executor: executor}
				_, _, _, err = router.DoPinned(context.Background(), choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
			}
			var limit *CachedUsageLimitError
			if !errors.As(err, &limit) || calls != 0 {
				t.Fatalf("disabled pinned/default request = %v, upstream calls=%d", err, calls)
			}
		})
	}
}

func TestCodexExtraCreditsRechecksBeforeSameAccountCredentialRetry(t *testing.T) {
	remaining, usageReads, upstreamCalls := 25, 0, 0
	guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
		usageReads++
		return extraCreditsBypassObservation(remaining), nil
	})}
	executor, choice, first := extraCreditsBypassExecutor(t, guard, codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls++
		remaining = 0
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error"}}`))}, nil
	}))
	second := first
	second.Candidate.CandidateID = "candidate-2"
	second.Ordinal = 2
	router := &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{{Choice: choice, Attempts: []CandidateAttempt{first, second}}}}, Executor: executor}
	response, _, _, _ := router.DoPinned(context.Background(), choice, makeCodexRequest(`{"model":"gpt-5.4"}`))
	closeResponse(response)
	if upstreamCalls != 1 || usageReads < 2 {
		t.Fatalf("retry bypassed fresh quota: upstream=%d usage reads=%d", upstreamCalls, usageReads)
	}
}

func TestCodexExtraCreditsPrimerCannotDispatchPaidRequest(t *testing.T) {
	upstreamCalls := 0
	guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
		return extraCreditsBypassObservation(0), nil
	})}
	executor, _, _ := extraCreditsBypassExecutor(t, guard, codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))}, nil
	}))
	requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/backend-api/codex/responses"}
	result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
	if upstreamCalls != 0 || result.State == PrimerRequestAdmitted {
		t.Fatalf("primer spent credits: calls=%d result=%+v error=%v", upstreamCalls, result, err)
	}
}

func TestCodexExtraCreditsWebSocketReuseCannotBypassPolicy(t *testing.T) {
	for _, prewarm := range []bool{false, true} {
		t.Run(fmt.Sprintf("prewarm=%v", prewarm), func(t *testing.T) {
			guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
				return extraCreditsBypassObservation(0), nil
			})}
			_, choice, attempt := extraCreditsBypassExecutor(t, guard, nil)
			upstream := &codexWSBrokerConnStub{}
			active := &codexWSActiveUpstream{conn: upstream, account: choice.AccountKey, attempt: attempt, generation: 1}
			account := CodexFrozenDispatchAccount{choice: choice, attempts: []CandidateAttempt{attempt}}
			broker := &codexTerminatingWSBroker{config: codexTerminatingWSBrokerConfig{Upstream: codexExplicitWSUpstreamDialer{executor: &CodexWebSocketAttemptExecutor{ExtraCredits: guard}}}}
			var dial codexWSDialResult
			if prewarm {
				dial = broker.connectPrewarm(context.Background(), account, active)
			} else {
				dial = broker.connect(context.Background(), nil, nil, account, active, true)
			}
			var limit *CachedUsageLimitError
			if !errors.As(dial.err, &limit) {
				t.Fatalf("reused socket admitted paid request: %v", dial.err)
			}
			if writes := upstream.writtenPayloads(); len(writes) != 0 {
				t.Fatalf("paid frame reached reused socket: %q", writes)
			}
		})
	}
}

func TestCodexExtraCreditsWebSocketDialStopsBeforeNetwork(t *testing.T) {
	dials := 0
	guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
		return extraCreditsBypassObservation(0), nil
	})}
	httpExecutor, choice, attempt := extraCreditsBypassExecutor(t, guard, nil)
	executor := &CodexWebSocketAttemptExecutor{ExtraCredits: guard, Secrets: httpExecutor.Secrets, Dialer: websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected paid handshake")
	}}}
	_, _, _, err := executor.Dial(context.Background(), choice, attempt, "wss://chatgpt.example/backend-api/codex/responses", make(http.Header))
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) || dials != 0 {
		t.Fatalf("disabled WebSocket dial = %v, network calls=%d", err, dials)
	}
}

func TestCodexExtraCreditsLegacyWebSocketChecksNestedFrameModel(t *testing.T) {
	guard := &CodexExtraCreditsGuard{Policy: extraCreditsBypassPolicy{"account-1": false}, Usage: extraCreditsBypassUsage(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
		observation := extraCreditsBypassObservation(50)
		available := false
		observation.ModelUsage = map[string]codex.ModelUsage{"gpt-6-astra": {Available: &available, CreditsWouldEnable: true}}
		return observation, nil
	})}
	capacity := NewCodexCapacityLedger(time.Now, time.Minute)
	capacity.ExtraCredits = guard
	left := &reserveRelayConn{blockingRelayConn: newBlockingRelayConn(), frame: []byte(`{"type":"response.create","params":{"model":"gpt-6-astra","input":[]}}`)}
	right := &reserveRelayConn{blockingRelayConn: newBlockingRelayConn()}
	choice := RouteChoice{AccountKey: "account-1", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}
	err := relayWebSocketPairGuarded(context.Background(), left, right, nil, codexExtraCreditsFrameGuard(context.Background(), capacity, choice))
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) || len(right.writes) != 0 {
		t.Fatalf("nested paid model reached upstream: error=%v writes=%q", err, right.writes)
	}
	if len(left.writes) != 1 || string(left.writes[0]) != string(codexReserveWSLimitFrame) {
		t.Fatalf("missing usage rejection: %q", left.writes)
	}
}
