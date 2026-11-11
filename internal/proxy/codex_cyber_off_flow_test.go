package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

// This exercises the production planner, durable lease runtime and WebSocket
// broker over real loopback sockets. Only provider replies and credentials are fixtures.
func TestCodexCyberOffContinuationFlowRequiresKnownAlternativeCapacity(t *testing.T) {
	for _, knownAlternative := range []bool{false, true} {
		t.Run(fmt.Sprintf("positive_alternative=%t", knownAlternative), func(t *testing.T) {
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
			// Advisory depleted usage remains unknown for admission; it must not evict a working route.
			core.capacity = NewCodexCapacityLedger(time.Now, time.Hour)
			exactZero := float64(0)
			depleted := QuotaSnapshot{FetchedAt: now, Result: quota.Result{
				Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
					quota.Window7Day: {RemainingPct: 0, RemainingPctExact: &exactZero, ResetAtUnix: now.Add(time.Hour).Unix()},
				},
			}}
			core.capacity.ObserveQuotaSnapshot(codexInstalledHTTPValidationAccountB, depleted)
			core.capacity.ObserveLivePositiveQuotaSnapshot(core.capacity.NewObservationStream(), codexInstalledHTTPValidationAccountB, depleted)
			frozenDispatchObserveCapacity(t, core.capacity, codexInstalledHTTPValidationAccountA, CapacityBucketBase, 60, now)
			policy := RoutingPolicyV2{
				SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
				Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []codex.AccountKey{codexInstalledHTTPValidationAccountA}}},
			}
			planner := &CodexHTTPRequestPlanFactory{
				Inventory: core.inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime,
				DefaultAccountKey: codexInstalledHTTPValidationAccountA,
				Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
				SessionPolicy:     NewSessionPolicyResolver(make([]byte, 32), policy),
				CyberEligibility:  NewCyberEligibilityStore(&RoutingPolicyStore{current: &policy}),
				DispatchPermits:   &sessionPolicyPermitRecorder{}, TransportKind: "websocket", Now: time.Now,
			}
			var requests, connections atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("ChatGPT-Account-ID") != "validation-upstream-a" {
					t.Errorf("continuation provider account = %q", r.Header.Get("ChatGPT-Account-ID"))
					return
				}
				connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer connection.Close()
				connections.Add(1)
				for {
					_, data, err := connection.ReadMessage()
					if err != nil {
						return
					}
					var request struct {
						PreviousResponseID string `json:"previous_response_id"`
					}
					if err := json.Unmarshal(data, &request); err != nil {
						t.Error(err)
						return
					}
					n := requests.Add(1)
					wantPrevious := ""
					if n > 1 {
						wantPrevious = fmt.Sprintf("response-%d", n-1)
					}
					if request.PreviousResponseID != wantPrevious {
						t.Errorf("previous response = %q, want %q", request.PreviousResponseID, wantPrevious)
						return
					}
					for _, frame := range []string{
						fmt.Sprintf(`{"type":"response.created","response":{"id":"response-%d"}}`, n),
						fmt.Sprintf(`{"type":"response.completed","response":{"id":"response-%d","end_turn":true}}`, n),
					} {
						if err := connection.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
							return
						}
					}
				}
			}))
			t.Cleanup(upstream.Close)
			executor := NewCodexWebSocketAttemptExecutor(core.inventory, core.inventory)
			executor.Dialer.Proxy = nil
			handler, err := NewCodexTerminatingWebSocketHandler(planner, executor, core.inventory, core.capacity, upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downstream, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer downstream.Close()
				ctx := withRuntimeCallerAuthority(r.Context(), RuntimeCallerAuthorityV1{Domain: NormalCallerLocal, SubjectID: "local", ConsumptionDigest: strings.Repeat("a", 64)})
				done <- handler.Serve(ctx, downstream, nil)
			}))
			t.Cleanup(server.Close)
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 3; n++ {
				if n == 2 && knownAlternative {
					exactPositive := float64(75)
					recovered := QuotaSnapshot{FetchedAt: time.Now(), Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 75, RemainingPctExact: &exactPositive, ResetAtUnix: now.Add(time.Hour).Unix()}}}}
					core.capacity.ObserveLivePositiveQuotaSnapshot(core.capacity.NewObservationStream(), codexInstalledHTTPValidationAccountB, recovered)
				}
				extra := ""
				if n > 1 {
					extra = fmt.Sprintf(`,"previous_response_id":"response-%d"`, n-1)
				}
				if err := client.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame(fmt.Sprintf("turn-%d", n), extra)); err != nil {
					t.Fatal(err)
				}
				_, data, err := client.ReadMessage()
				if n == 2 && knownAlternative {
					if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
						t.Fatalf("known available cheaper account did not request reset: %v %s", err, data)
					}
					break
				}
				if err != nil {
					t.Fatalf("usable continuation %d interrupted: %v", n, err)
				}
				var created struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(data, &created); err != nil || created.Type != "response.created" {
					t.Fatalf("response start = %s, %v", data, err)
				}
				_, data, err = client.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				var completed struct {
					Type     string `json:"type"`
					Response struct {
						ID string `json:"id"`
					} `json:"response"`
				}
				if err := json.Unmarshal(data, &completed); err != nil || completed.Type != "response.completed" || completed.Response.ID != fmt.Sprintf("response-%d", n) {
					t.Fatalf("response completion = %s, %v", data, err)
				}
			}
			wantRequests := int32(3)
			if knownAlternative {
				wantRequests = 1
			}
			if requests.Load() != wantRequests || connections.Load() != 1 {
				t.Fatalf("provider flow = %d requests / %d connections, want %d / 1", requests.Load(), connections.Load(), wantRequests)
			}
			_ = client.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("broker result: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("broker did not release closed socket")
			}
		})
	}
}
