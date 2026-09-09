package proxy

import (
	"context"
	"errors"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
	"net/http"
	"testing"
	"time"
)

func TestCodexReserveDispatchGuard(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 2, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	logical := frozenDispatchTestLogicalAccount("system", frozenDispatchCandidate("system", "candidate", "revision", codex.SourceSystem, false, time.Time{}))
	plan, planErr := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{logical}}, Capacity: ledger, Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"}, BoundAccountKey: "system", DefaultAccountKey: "system", Now: now})
	if planErr != nil || len(plan.Accounts()) != 1 {
		t.Fatalf("bound reserve plan = %#v, %v, want guarded dispatch attempt", plan, planErr)
	}
	executor := &CodexAttemptExecutor{Reserve: reserve}
	wsExecutor := &CodexWebSocketAttemptExecutor{Reserve: reserve}
	dialer := codexExplicitWSUpstreamDialer{executor: wsExecutor}
	if err := dialer.reserveDispatchError("system"); err == nil {
		t.Fatal("WebSocket turn admitted protected account")
	}
	if err := dialer.reserveDispatchError("other"); err != nil {
		t.Fatal(err)
	}
	choice := RouteChoice{AccountKey: "system"}
	for _, frozen := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
		var err error
		if frozen {
			_, err = executor.DoFrozen(context.Background(), choice, CandidateAttempt{}, req)
		} else {
			_, err = executor.Do(context.Background(), choice, CandidateAttempt{}, req)
		}
		var limit *CachedUsageLimitError
		if !errors.As(err, &limit) {
			t.Fatalf("frozen=%v error=%v", frozen, err)
		}
	}
	_, _, dispatched, dispatchErr := executor.DispatchFrozen(context.Background(), choice, CandidateAttempt{}, makeCodexRequest("{}"), func(CandidateAttempt) error { t.Fatal("reserved request crossed dispatch fence"); return nil })
	var dispatchLimit *CachedUsageLimitError
	if dispatched || !errors.As(dispatchErr, &dispatchLimit) {
		t.Fatalf("dispatch=%v error=%v", dispatched, dispatchErr)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	_, err = executor.Do(context.Background(), choice, CandidateAttempt{}, req)
	var limit *CachedUsageLimitError
	if errors.As(err, &limit) {
		t.Fatal("usage refresh blocked")
	}
	if err := reserveDispatchError(reserve, "other"); err != nil {
		t.Fatalf("other account blocked: %v", err)
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchError(reserve, "system"); err != nil {
		t.Fatalf("bypass blocked: %v", err)
	}
}

type reserveRelayConn struct {
	*blockingRelayConn
	frame  []byte
	writes [][]byte
}

func (c *reserveRelayConn) ReadMessage() (int, []byte, error) {
	if c.frame != nil {
		frame := c.frame
		c.frame = nil
		return 1, frame, nil
	}
	return c.blockingRelayConn.ReadMessage()
}
func (c *reserveRelayConn) WriteMessage(_ int, frame []byte) error {
	c.writes = append(c.writes, append([]byte(nil), frame...))
	return nil
}

func TestCodexReserveLegacyWebSocketStopsBeforeForward(t *testing.T) {
	left := &reserveRelayConn{blockingRelayConn: newBlockingRelayConn(), frame: []byte(`{"type":"response.create"}`)}
	right := &reserveRelayConn{blockingRelayConn: newBlockingRelayConn()}
	err := relayWebSocketPairGuarded(context.Background(), left, right, nil, func(int, []byte) error { return &CachedUsageLimitError{} })
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) {
		t.Fatal(err)
	}
	if len(right.writes) != 0 {
		t.Fatal("reserved turn reached upstream")
	}
	if len(left.writes) != 1 || string(left.writes[0]) != string(codexReserveWSLimitFrame) {
		t.Fatalf("missing usage error: %q", left.writes)
	}
}
