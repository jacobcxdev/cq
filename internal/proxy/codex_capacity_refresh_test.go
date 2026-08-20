package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type codexRoutingUsageReaderStub struct {
	mu      sync.Mutex
	results map[codex.AccountKey]codex.UsageObservation
	errors  map[codex.AccountKey]error
	panics  map[codex.AccountKey]bool
	calls   map[codex.AccountKey]int
}

func (reader *codexRoutingUsageReaderStub) Read(_ context.Context, account codex.AccountKey) (codex.UsageObservation, error) {
	reader.mu.Lock()
	reader.calls[account]++
	shouldPanic := reader.panics[account]
	result := reader.results[account]
	err := reader.errors[account]
	reader.mu.Unlock()
	if shouldPanic {
		panic("private usage reader panic")
	}
	return result, err
}

func (reader *codexRoutingUsageReaderStub) callCount(account codex.AccountKey) int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.calls[account]
}

func TestCodexRoutingCapacityRefresherPublishesUsageAndHonoursInterval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reader := &codexRoutingUsageReaderStub{
		results: map[codex.AccountKey]codex.UsageObservation{
			"depleted": {Result: quota.Result{Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 0},
			}}},
			"available": {Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 27},
			}}},
		},
		errors: make(map[codex.AccountKey]error),
		panics: make(map[codex.AccountKey]bool),
		calls:  make(map[codex.AccountKey]int),
	}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 5*time.Minute)
	refresher := &CodexRoutingCapacityRefresher{
		Usage: reader, Capacity: ledger, Now: func() time.Time { return now }, Interval: 5 * time.Minute,
	}

	accounts := []codex.AccountKey{"depleted", "available", "available", ""}
	if !refresher.Refresh(context.Background(), accounts) {
		t.Fatal("first refresh = false, want published capacity")
	}
	if view := ledger.Capacity("available", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 27 {
		t.Fatalf("available capacity = %+v, want positive 27%%", view)
	}
	if view := ledger.Capacity("depleted", CapacityBucketBase); view.State != CapacityUnknown || view.Source != CapacitySourceUsageCache {
		t.Fatalf("depleted capacity = %+v, want conservative advisory zero", view)
	}
	if refresher.Refresh(context.Background(), accounts) {
		t.Fatal("refresh inside interval = true, want cached result")
	}
	if reader.callCount("depleted") != 1 || reader.callCount("available") != 1 {
		t.Fatalf("calls = depleted:%d available:%d, want one each", reader.callCount("depleted"), reader.callCount("available"))
	}
	reader.mu.Lock()
	reader.results["new"] = codex.UsageObservation{Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
		quota.Window7Day: {RemainingPct: 63},
	}}}
	reader.mu.Unlock()
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"new"}) {
		t.Fatal("disjoint refresh inside interval = false, want uncached account fetched")
	}
	if reader.callCount("new") != 1 {
		t.Fatalf("new account calls = %d, want one", reader.callCount("new"))
	}

	now = now.Add(5 * time.Minute)
	if !refresher.Refresh(context.Background(), accounts) {
		t.Fatal("refresh at interval = false, want new observations")
	}
	if reader.callCount("depleted") != 2 || reader.callCount("available") != 2 {
		t.Fatalf("calls after interval = depleted:%d available:%d, want two each", reader.callCount("depleted"), reader.callCount("available"))
	}
}

func TestCodexRoutingCapacityRefresherContainsFailureAndPanic(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reader := &codexRoutingUsageReaderStub{
		results: map[codex.AccountKey]codex.UsageObservation{
			"available": {Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 41},
			}}},
		},
		errors: map[codex.AccountKey]error{"failed": errors.New("private usage failure")},
		panics: map[codex.AccountKey]bool{"panicked": true},
		calls:  make(map[codex.AccountKey]int),
	}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 5*time.Minute)
	refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }}

	if !refresher.Refresh(context.Background(), []codex.AccountKey{"failed", "panicked", "available"}) {
		t.Fatal("partial refresh = false, want successful account published")
	}
	if view := ledger.Capacity("available", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 41 {
		t.Fatalf("available capacity = %+v, want positive 41%%", view)
	}
	if view := ledger.Capacity("failed", CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("failed capacity = %+v, want unknown", view)
	}
	if view := ledger.Capacity("panicked", CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("panicked capacity = %+v, want unknown", view)
	}

	now = now.Add(30 * time.Second)
	reader.mu.Lock()
	delete(reader.errors, "failed")
	reader.results["failed"] = codex.UsageObservation{Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
		quota.Window7Day: {RemainingPct: 19},
	}}}
	reader.mu.Unlock()
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"failed", "available"}) {
		t.Fatal("failed account retry = false, want retry after 30 seconds")
	}
	if reader.callCount("failed") != 2 || reader.callCount("available") != 1 {
		t.Fatalf("retry calls = failed:%d available:%d, want 2/1", reader.callCount("failed"), reader.callCount("available"))
	}
}
