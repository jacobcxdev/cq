package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexCapacityUsageObserverFollowsValidPublication(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &codexRoutingUsageReaderStub{
		results: map[codex.AccountKey]codex.UsageObservation{"system": {Result: quota.Result{Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, ResetAtUnix: now.Add(time.Hour).Unix()}}}}},
		errors:  make(map[codex.AccountKey]error), calls: make(map[codex.AccountKey]int),
	}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	calls := 0
	refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }, Interval: time.Second,
		OnUsage: func(account codex.AccountKey, result quota.Result, observed time.Time) {
			calls++
			windows, at := ledger.WindowSnapshot(account)
			if account != "system" || result.RateLimitTier != "codex_pro_20x" || !observed.Equal(now) || !at.Equal(now) || windows["7d"].RemainingPct != 5 {
				t.Errorf("observer preceded publication or lost metadata: %s %+v %v", account, result, observed)
			}
		},
	}
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"system"}) || calls != 1 {
		t.Fatal("valid observation not delivered")
	}
	now = now.Add(time.Second)
	reader.errors["system"] = errors.New("usage unavailable")
	if refresher.Refresh(context.Background(), []codex.AccountKey{"system"}) || calls != 1 {
		t.Fatal("failed observation delivered")
	}
}

func TestCodexCapacityUsageObserverPanicDoesNotStrandRefresh(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &codexRoutingUsageReaderStub{results: map[codex.AccountKey]codex.UsageObservation{"system": {Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5}}}}}, calls: make(map[codex.AccountKey]int)}
	refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: NewCodexCapacityLedger(func() time.Time { return now }, time.Hour), Now: func() time.Time { return now }, Interval: time.Second,
		OnUsage: func(codex.AccountKey, quota.Result, time.Time) { panic("observer failure") },
	}
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"system"}) {
		t.Fatal("callback panic lost publication")
	}
	now = now.Add(time.Second)
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"system"}) || reader.callCount("system") != 2 {
		t.Fatal("callback panic stranded in-flight refresh")
	}
}
