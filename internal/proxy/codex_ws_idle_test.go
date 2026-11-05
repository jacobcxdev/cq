package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// A silent provider must not retain a streaming attempt indefinitely, even
// while the client keeps its socket open or another turn waits behind it.
func TestCodexWSResponseIdleTimeoutReleasesAttempt(t *testing.T) {
	for _, test := range []struct {
		name         string
		reused       bool
		blockedWrite bool
	}{{name: "fresh"}, {name: "reused", reused: true}, {name: "downstream write", blockedWrite: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{
				AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
			}})
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			lifecycle, err := newCodexWSLifecycle(handle, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			upstream := &codexWSBrokerConnStub{
				roleKnown:     true,
				reads:         []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)}},
				readGateAfter: 1, readGate: make(chan struct{}),
			}
			active := codexWSActiveUpstream{conn: upstream, account: "account-a", generation: 1}
			if test.reused {
				startCodexWSUpstreamReader(context.Background(), &active)
			}
			var downstream websocketRelayConn = &codexWSBrokerConnStub{}
			if test.blockedWrite {
				blocked := newCodexWSJoinedCancellationConn()
				close(blocked.releaseCancellation)
				downstream = blocked
			}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
				Plans: &codexWSBrokerPlannerStub{}, Upstream: &codexWSBrokerDialerStub{},
				UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 1,
				ResponseIdleTimeout: 20 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			pending, err := newCodexWSPendingFrameOwned(websocket.TextMessage, codexTerminatingWSFrame("turn-a", ""))
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				index := 0
				_, serveErr := broker.serveDispatchedFrame(ctx, downstream, pending, lifecycle, nil, &index, false, false, &active, codexWSDialResult{})
				done <- serveErr
			}()
			select {
			case err = <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("silent response = %v, want inactivity deadline", err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("stalled response retained attempt after inactivity deadline")
			}
			record := lifecycle.handle.record
			if record.State != LeaseOrphaned || codexLeaseCurrentAttemptState(record) != CodexAttemptIndeterminate ||
				record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct {
				t.Fatalf("idle attempt not released: state=%s attempt=%s refs=%d/%d/%d extinct=%t", record.State,
					codexLeaseCurrentAttemptState(record), record.RoutingRefs, record.AttemptRefs, record.ResponseObserverRefs, record.SocketLineageExtinct)
			}
			upstream.mu.Lock()
			closed := upstream.closed
			upstream.mu.Unlock()
			if !closed {
				t.Fatal("released attempt still has live upstream transport")
			}
			// Exercise persisted state rather than only the lifecycle's local copy.
			ready, reason, err := runtimeLease.requestPlanningAvailable(plan.Key, []codex.AccountKey{"account-a"}, plan.Authority)
			if err != nil || !ready {
				t.Fatalf("successor planning after idle timeout = %t, %q, %v", ready, reason, err)
			}
			successor, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatalf("successor blocked by idle attempt: %v", err)
			}
			successor, err = successor.MarkDispatched()
			if err == nil {
				successor, err = successor.AdmitHTTP2xx()
			}
			if err == nil {
				successor, err = successor.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			}
			if err == nil {
				_, err = successor.Drain()
			}
			if err != nil {
				t.Fatalf("successor did not complete: %v", err)
			}
		})
	}
}

func TestCodexWSResponseIdleTimeoutRenewsForProgress(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	planner := &codexWSBrokerPlannerStub{
		runtime: newCodexLeaseRuntimeTest(t, coordinator),
		slots:   []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}},
	}
	downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}}}
	upstream := &codexWSIdleProgressConn{
		codexWSBrokerConnStub: &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.output_text.delta","delta":"one"}`)},
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.output_text.delta","delta":"two"}`)},
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.output_text.delta","delta":"three"}`)},
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.output_text.delta","delta":"four"}`)},
			{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":true}}`)},
		}},
		interval: 60 * time.Millisecond,
	}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans: planner, Upstream: &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{"account-a": {upstream}}},
		UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 1,
		ResponseIdleTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := broker.Serve(ctx, downstream); err != nil {
		t.Fatalf("progressing response exceeded total idle interval: %v", err)
	}
	if got := len(downstream.writtenPayloads()); got != 6 {
		t.Fatalf("relayed response frames = %d, want 6 including completion", got)
	}
}

type codexWSIdleProgressConn struct {
	*codexWSBrokerConnStub
	interval time.Duration
}

func (conn *codexWSIdleProgressConn) ReadMessage() (int, []byte, error) {
	time.Sleep(conn.interval)
	return conn.codexWSBrokerConnStub.ReadMessage()
}
