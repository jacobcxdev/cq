//go:build !windows

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

// The installed binary runs only against isolated credentials, loopback fixtures
// and a temporary HOME. No desktop or installed proxy process is changed.
func TestCodexInstalledCyberOffToolContinuationKeepsSocket(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatal(err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Error(err)
		}
	})
	core.inventory.inventory.Accounts = core.inventory.inventory.Accounts[:2]
	now := time.Now()
	core.capacity = NewCodexCapacityLedger(time.Now, time.Hour)
	frozenDispatchObserveCapacity(t, core.capacity, codexInstalledHTTPValidationAccountA, CapacityBucketBase, 60, now)
	exactZero := float64(0)
	depleted := QuotaSnapshot{FetchedAt: now, Result: quota.Result{Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 0, RemainingPctExact: &exactZero, ResetAtUnix: now.Add(time.Hour).Unix()}}}}
	core.capacity.ObserveQuotaSnapshot(codexInstalledHTTPValidationAccountB, depleted)
	core.capacity.ObserveLivePositiveQuotaSnapshot(core.capacity.NewObservationStream(), codexInstalledHTTPValidationAccountB, depleted)
	policy := RoutingPolicyV2{SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1, Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []codex.AccountKey{codexInstalledHTTPValidationAccountA}}}}
	planner := &CodexHTTPRequestPlanFactory{Inventory: core.inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime, DefaultAccountKey: codexInstalledHTTPValidationAccountA, Authority: CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true}, SessionPolicy: NewSessionPolicyResolver(make([]byte, 32), policy), CyberEligibility: NewCyberEligibilityStore(&RoutingPolicyStore{current: &policy}), DispatchPermits: &sessionPolicyPermitRecorder{}, TransportKind: "websocket", Now: time.Now}
	var connections, turns, prewarms, toolOutputs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "validation-upstream-a" {
			t.Errorf("provider account = %q, want usable Cyber member", got)
			return
		}
		conn, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			pending, err := newCodexWSPendingFrameOwned(kind, data)
			if err != nil {
				t.Error(err)
				return
			}
			prewarm := pending.prewarm
			frame := append([]byte(nil), data...)
			pending.Release()
			id := "fixture-prewarm"
			replies := []string{}
			if prewarm {
				prewarms.Add(1)
			} else if turns.Add(1) == 1 {
				id = "fixture-tool"
				replies = append(replies, `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"exec_command","arguments":"{\"cmd\":\"/usr/bin/true\",\"yield_time_ms\":1000}"}}`)
			} else {
				id = "fixture-final"
				var request struct {
					Input []struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Output string `json:"output"`
					} `json:"input"`
				}
				if err := json.Unmarshal(frame, &request); err != nil {
					t.Error(err)
					return
				}
				for _, item := range request.Input {
					if item.Type == "function_call_output" && item.CallID == "call_fixture" {
						if !strings.Contains(item.Output, "Process exited with code 0") {
							t.Error("fixture command did not exit successfully")
						}
						toolOutputs.Add(1)
					}
				}
				replies = append(replies, `{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_fixture","content":[{"type":"output_text","text":"PONG"}]}}`)
			}
			replies = append([]string{`{"type":"response.created","response":{"id":"` + id + `"}}`}, replies...)
			endTurn := "true"
			if id == "fixture-tool" {
				endTurn = "false"
			}
			replies = append(replies, `{"type":"response.completed","response":{"id":"`+id+`","end_turn":`+endTurn+`,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			for _, frame := range replies {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(upstream.Close)
	executor := NewCodexWebSocketAttemptExecutor(core.inventory, core.inventory)
	executor.Dialer.Proxy = nil
	broker, err := NewCodexTerminatingWebSocketHandler(planner, executor, core.inventory, core.capacity, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Use the regular runtime handler and caller authority rather than bypassing native admission.
	token, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Config: &Config{LocalToken: token, ClaudeUpstream: upstream.URL, CodexUpstream: upstream.URL}, CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1}}, CodexWebSocketBroker: broker}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	models, closeModels, err := newCodexInstalledModelsHandler(token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeModels)
	candidate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			models.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
	listener, _ := newCodexRuntimeSupervisorAcceptanceServerWithCredential(t, candidate, NormalCallerCredentialV1{Domain: NormalCallerCodex, Bearer: token, SubjectID: string(codexInstalledHTTPValidationAccountA) + "\x00validation-candidate-a\x00validation-revision-1", ValidUntil: time.Now().Add(time.Hour)})
	isolation := newCodexTaskAffinityAcceptanceIsolation(t, token)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptancePromptForTransport(ctx, codexReadOnlyTaskAffinityAcceptanceRunner{stdin: "Run /usr/bin/true, then reply with PONG."}, clientProof, listener.URL, isolation, false, true, true, "Run /usr/bin/true, then reply with PONG.", "PONG"); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 1 || turns.Load() != 2 || toolOutputs.Load() != 1 {
		t.Fatalf("installed client flow = %d sockets, %d requests, %d tool outputs, %d prewarms; want 1/2/1", connections.Load(), turns.Load(), toolOutputs.Load(), prewarms.Load())
	}
}
