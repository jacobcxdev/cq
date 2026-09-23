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

func TestCodexRoutingCapacityRefresherLiftsHardFenceAfterReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(7 * 24 * time.Hour)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 5*time.Minute)
	stream := ledger.NewObservationStream()
	if !ledger.Observe(stream.Stamp(CapacityFact{
		AccountKey: "reset", Bucket: CapacityBucketBase, RemainingPct: 0,
		Source: CapacitySourceHardLimit, ResetAt: resetAt, Confidence: CapacityConfidenceAuthoritative,
	})) {
		t.Fatal("hard limit was not observed")
	}

	now = now.Add(time.Second)
	exact := 100.0
	reader := &codexRoutingUsageReaderStub{
		results: map[codex.AccountKey]codex.UsageObservation{
			"reset": {Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 100, RemainingPctExact: &exact, ResetAtUnix: resetAt.Unix()},
			}}},
		},
		errors: make(map[codex.AccountKey]error), panics: make(map[codex.AccountKey]bool), calls: make(map[codex.AccountKey]int),
	}
	refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }}
	if !refresher.Refresh(context.Background(), []codex.AccountKey{"reset"}) {
		t.Fatal("reset usage was not published")
	}
	if view := ledger.Capacity("reset", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 100 || view.Source != CapacitySourceLiveUsage {
		t.Fatalf("capacity after reset = %+v, want authoritative positive 100%%", view)
	}
}

func TestCodexLiveUsageStartedBeforeHardLimitDoesNotLiftFence(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(7 * 24 * time.Hour)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 5*time.Minute)
	usageStream := ledger.NewObservationStream()
	hardStream := ledger.NewObservationStream()
	if !ledger.Observe(hardStream.Stamp(CapacityFact{
		AccountKey: "account", Bucket: CapacityBucketBase, RemainingPct: 0,
		Source: CapacitySourceHardLimit, ResetAt: resetAt, Confidence: CapacityConfidenceAuthoritative,
	})) {
		t.Fatal("hard limit was not observed")
	}
	exact := 100.0
	ledger.ObserveLivePositiveQuotaSnapshot(usageStream, "account", QuotaSnapshot{
		FetchedAt: now,
		Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
			quota.Window7Day: {RemainingPct: 100, RemainingPctExact: &exact, ResetAtUnix: resetAt.Unix()},
		}},
	})
	if view := ledger.Capacity("account", CapacityBucketBase); view.State != CapacityZero || view.Source != CapacitySourceHardLimit {
		t.Fatalf("capacity = %+v, want newer hard fence", view)
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

func TestCodexAdaptiveRefreshInterval(t *testing.T) {
	for _, tc := range []struct {
		remaining int
		want      time.Duration
	}{{100, time.Minute}, {26, time.Minute}, {25, 30 * time.Second}, {10, 15 * time.Second}, {1, 5 * time.Second}, {0, 5 * time.Second}} {
		got := codexUsageRefreshInterval(map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: tc.remaining}})
		if got != tc.want {
			t.Fatalf("remaining %v: got %v want %v", tc.remaining, got, tc.want)
		}
	}
}

func TestCodexRefreshHonoursRetryAfter(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &codexRoutingUsageReaderStub{errors: map[codex.AccountKey]error{"a": &CodexUsageHTTPError{StatusCode: 429, RetryAt: now.Add(2 * time.Minute)}}, calls: make(map[codex.AccountKey]int)}
	r := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: NewCodexCapacityLedger(nil, time.Minute), Now: func() time.Time { return now }}
	r.Refresh(context.Background(), []codex.AccountKey{"a"})
	now = now.Add(time.Minute)
	r.Refresh(context.Background(), []codex.AccountKey{"a"})
	if reader.callCount("a") != 1 {
		t.Fatal("retried before Retry-After")
	}
	now = now.Add(time.Minute)
	r.Refresh(context.Background(), []codex.AccountKey{"a"})
	if reader.callCount("a") != 2 {
		t.Fatal("did not retry at Retry-After")
	}
}

type blockingCapacityUsageReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingCapacityUsageReader) Read(ctx context.Context, _ codex.AccountKey) (codex.UsageObservation, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return codex.UsageObservation{}, ctx.Err()
	}
	return codex.UsageObservation{Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 50}}}}, nil
}
func TestCodexRefreshCoalescesLongRunningReads(t *testing.T) {
	reader := &blockingCapacityUsageReader{entered: make(chan struct{}, 2), release: make(chan struct{})}
	r := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: NewCodexCapacityLedger(nil, time.Minute)}
	done := make(chan struct{})
	go func() { defer close(done); r.Refresh(context.Background(), []codex.AccountKey{"a"}) }()
	<-reader.entered
	r.mu.Lock()
	r.nextRefresh["a"] = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	if r.Refresh(context.Background(), []codex.AccountKey{"a"}) {
		t.Fatal("concurrent read published")
	}
	select {
	case <-reader.entered:
		t.Fatal("duplicate in-flight read")
	default:
	}
	close(reader.release)
	<-done
}
func TestCodexRefresherRunRefreshesInventoryAndStops(t *testing.T) {
	reader := &blockingCapacityUsageReader{entered: make(chan struct{}, 2), release: make(chan struct{})}
	r := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: NewCodexCapacityLedger(nil, time.Minute)}
	inventory := &staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "a", Routable: true}, {Key: "b"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx, inventory) }()
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("no immediate service refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service refresh ignored cancellation")
	}
}

func TestCodexRefreshAcceleratesFromLiveSnapshot(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &codexRoutingUsageReaderStub{results: map[codex.AccountKey]codex.UsageObservation{"a": {Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 90}}}}}, calls: make(map[codex.AccountKey]int)}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	r := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }}
	r.Refresh(context.Background(), []codex.AccountKey{"a"})
	now = now.Add(5 * time.Second)
	ledger.ObserveQuotaSnapshot("a", QuotaSnapshot{Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 1}}}, FetchedAt: now})
	if !r.Refresh(context.Background(), []codex.AccountKey{"a"}) {
		t.Fatal("latest near-exhausted snapshot did not accelerate refresh")
	}
}
func TestCodexReserveIntervalCallbackAndOverride(t *testing.T) {
	r := &CodexRoutingCapacityRefresher{IntervalForAccount: func(account codex.AccountKey, _ map[quota.WindowName]quota.Window) time.Duration {
		if account == "system" {
			return 5 * time.Second
		}
		return time.Minute
	}}
	windows := map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 80}}
	if r.refreshInterval("system", windows) != 5*time.Second || r.refreshInterval("other", windows) != time.Minute {
		t.Fatal("account callback leaked across accounts")
	}
	r.Interval = 2 * time.Minute
	if r.refreshInterval("system", windows) != 2*time.Minute {
		t.Fatal("explicit interval override ignored")
	}
}

func TestCodexAdaptiveRefreshUsesExactPercent(t *testing.T) {
	remaining := 1.4
	windows := map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 1, RemainingPctExact: &remaining}}
	if got := codexUsageRefreshInterval(windows); got != 15*time.Second {
		t.Fatalf("fractional remaining cadence = %v", got)
	}
}
