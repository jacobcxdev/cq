package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexNativeHTTPChangesModelAfterBoundCompaction(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "websocket"} {
		for _, failed := range []bool{false, true} {
			name := transport + "/completed"
			if failed {
				name = transport + "/failed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
				runtime := newCodexLeaseRuntimeTest(t, coordinator)
				plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
					AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
				}})
				plan.Key.Lane.Session, plan.Key.Lane.Thread = "session", "thread"
				plan.RequestKind, plan.CompactionPhase = CodexRequestCompaction, CodexCompactionPreTurn
				plan.RequestedModel, plan.EffectiveModel = "gpt-6.1-sol", "gpt-6.1-sol"
				first, err := runtime.BeginRequest(plan)
				if err != nil {
					t.Fatal(err)
				}
				first, err = first.MarkDispatched()
				if err != nil {
					t.Fatal(err)
				}
				if transport == "websocket" {
					lifecycle, err := newCodexWSLifecycle(first, 41, 43)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := lifecycle.ObserveFrame(context.Background(), 43, []byte(`{"type":"response.created","response":{"id":"response-a"}}`)); err != nil {
						t.Fatal(err)
					}
					terminal := `{"type":"response.completed","response":{"id":"response-a","end_turn":false}}`
					if failed {
						terminal = `{"type":"response.failed","response":{"error":{"code":"cyber_policy"}}}`
					}
					if _, err := lifecycle.ObserveFrame(context.Background(), 43, []byte(terminal)); err != nil {
						t.Fatal(err)
					}
					first = lifecycle.handle
				} else {
					first, err = first.AdmitHTTP2xx()
					if err != nil {
						t.Fatal(err)
					}
					if failed {
						first, err = first.ProviderFailed(CodexHTTPResponseEvidence{})
					} else {
						first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				first, err = first.Drain()
				if err != nil {
					t.Fatal(err)
				}

				factory := codexHTTPRequestPlanTestFactory(runtime)
				factory.Routes, factory.Authority = coordinator, plan.Authority
				factory.TransportKind = "http"
				body := bytes.ReplaceAll(frozenRequestBody("gpt-6-sol", CodexRequestCompaction, "history"), []byte("standalone_turn"), []byte("pre_turn"))
				const responseBody = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-b\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-b\",\"end_turn\":false}}\n\n"
				events := []string{}
				dispatcher := &codexNativeHTTPDispatcher{
					wantURL: "https://codex.example/responses", wantBody: body, events: &events,
					response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(responseBody))},
				}
				handler, err := NewCodexNativeHTTPHandler(factory, &CodexHTTPRequestSession{Executor: dispatcher}, "https://codex.example")
				if err != nil {
					t.Fatal(err)
				}
				ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{
					Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
				})
				ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")
				request := httptest.NewRequest(http.MethodPost, "http://cq.test/responses", bytes.NewReader(body)).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				writer := httptest.NewRecorder()
				(&Server{CodexNativeHTTP: handler}).handleNativeCodex(writer, request)
				if writer.Code != http.StatusOK || writer.Body.String() != responseBody {
					t.Fatalf("model-switch status/body = %d/%s", writer.Code, writer.Body.String())
				}
				restored, err := coordinator.store.loadLaneSnapshot(plan.Key, plan.Accounts, plan.Authority)
				if err != nil {
					t.Fatal(err)
				}
				current, found := runtime.restoredRecord(restored, restored.Fence.Current)
				if !found || current.Identity != first.identity || current.AccountKey != "account" || current.Record.Generation != first.RequestGeneration()+1 || current.Record.EffectiveModel != "gpt-6-sol" {
					t.Fatalf("model switch lost turn/account authority or new request model: %+v", current)
				}
				if current.Record.RoutingRefs != 0 || current.Record.AttemptRefs != 0 || current.Record.ResponseObserverRefs != 0 {
					t.Fatalf("model-switch response did not drain: %+v", current.Record)
				}
			})
		}
	}
}

func TestCodexLeaseRuntimeChangesBoundRequestModels(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, requested, effective string
	}{
		{"requested alias", "gpt-6.1-sol[1m]", "gpt-6.1-sol"},
		{"effective model", "gpt-6.1-sol", "gpt-6-sol"},
		{"both models", "gpt-6-sol", "gpt-6-sol"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtime := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
				AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
			}})
			plan.RequestedModel, plan.EffectiveModel = "gpt-6.1-sol", "gpt-6.1-sol"
			first := completeCodexLeaseRuntimeTurn(t, runtime, plan)
			plan.ExpectedBound = &CodexLeaseBoundExpectation{Identity: first.identity, AccountKey: "account", RecordGeneration: first.record.RecordGeneration}
			plan.RequestedModel, plan.EffectiveModel = test.requested, test.effective
			plan.RequiresAccountContinuity, plan.authenticatedCallerContinuity = true, true
			next, err := runtime.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			if next.identity != first.identity || next.AccountKey() != "account" || next.RequestGeneration() != first.RequestGeneration()+1 ||
				next.record.RequestedModelHash != coordinator.store.hash("requested-model", test.requested) || next.record.EffectiveModel != test.effective {
				t.Fatalf("model transition lost bound authority or request model: %+v", next.record)
			}
			if _, err := next.AbandonBeforeDispatch(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexLeaseRuntimeModelSwitchWaitsForTerminalDrain(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		name := "streaming"
		if completed {
			name = "undrained observer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtime := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
				AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
			}})
			first, err := runtime.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			first, err = first.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			first, err = first.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				first, err = first.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
				if err != nil {
					t.Fatal(err)
				}
			}
			plan.ExpectedBound = &CodexLeaseBoundExpectation{Identity: first.identity, AccountKey: "account", RecordGeneration: first.record.RecordGeneration}
			plan.RequestedModel, plan.EffectiveModel = "gpt-6-sol", "gpt-6-sol"
			before := append([]byte(nil), coordinator.store.journalBytes...)
			generation := coordinator.store.Generation()
			if _, err := runtime.BeginRequest(plan); !errors.Is(err, ErrCodexConcurrentTurn) {
				t.Fatalf("model-switch error = %v, want concurrent turn", err)
			}
			if coordinator.store.Generation() != generation || !bytes.Equal(before, coordinator.store.journalBytes) {
				t.Fatal("model switch mutated an active or undrained request")
			}
		})
	}
}
