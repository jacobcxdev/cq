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

func TestCodexCapacityObservationStreamOrdersFacts(t *testing.T) {
	ledger := NewCodexCapacityLedger(nil, time.Hour)
	firstStream := ledger.NewObservationStream()
	first := firstStream.Stamp(CapacityFact{ConnectionGeneration: 99, Sequence: 99})
	second := firstStream.Stamp(CapacityFact{})
	secondStreamFirst := ledger.NewObservationStream().Stamp(CapacityFact{})

	if first.ConnectionGeneration == 0 {
		t.Fatal("first generation is zero")
	}
	if second.ConnectionGeneration != first.ConnectionGeneration {
		t.Fatalf("second generation = %d, want %d", second.ConnectionGeneration, first.ConnectionGeneration)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("first stream sequences = %d, %d, want 1, 2", first.Sequence, second.Sequence)
	}
	if secondStreamFirst.ConnectionGeneration <= first.ConnectionGeneration {
		t.Fatalf("second stream generation = %d, want greater than %d", secondStreamFirst.ConnectionGeneration, first.ConnectionGeneration)
	}
	if secondStreamFirst.Sequence != 1 {
		t.Fatalf("second stream sequence = %d, want 1", secondStreamFirst.Sequence)
	}
}

func TestCodexCapacityObservationStreamsAreRaceSafe(t *testing.T) {
	const (
		streamCount    = 32
		factsPerStream = 32
	)
	type cursor struct {
		generation uint64
		sequence   uint64
	}

	ledger := NewCodexCapacityLedger(nil, time.Hour)
	cursors := make(chan cursor, streamCount*factsPerStream)
	var wg sync.WaitGroup
	for range streamCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream := ledger.NewObservationStream()
			for range factsPerStream {
				fact := stream.Stamp(CapacityFact{})
				cursors <- cursor{generation: fact.ConnectionGeneration, sequence: fact.Sequence}
			}
		}()
	}
	wg.Wait()
	close(cursors)

	seen := make(map[cursor]bool, streamCount*factsPerStream)
	sequencesByGeneration := make(map[uint64]map[uint64]bool, streamCount)
	for got := range cursors {
		if got.generation == 0 || got.sequence == 0 {
			t.Fatalf("zero cursor component: %+v", got)
		}
		if seen[got] {
			t.Fatalf("duplicate cursor: %+v", got)
		}
		seen[got] = true
		if sequencesByGeneration[got.generation] == nil {
			sequencesByGeneration[got.generation] = make(map[uint64]bool, factsPerStream)
		}
		sequencesByGeneration[got.generation][got.sequence] = true
	}
	if len(sequencesByGeneration) != streamCount {
		t.Fatalf("generation count = %d, want %d", len(sequencesByGeneration), streamCount)
	}
	for generation, sequences := range sequencesByGeneration {
		if len(sequences) != factsPerStream {
			t.Fatalf("generation %d sequence count = %d, want %d", generation, len(sequences), factsPerStream)
		}
		for sequence := uint64(1); sequence <= factsPerStream; sequence++ {
			if !sequences[sequence] {
				t.Fatalf("generation %d missing sequence %d", generation, sequence)
			}
		}
	}
}

func TestCodexCapacityLedgerSourcePrecedence(t *testing.T) {
	now := time.Unix(1_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	for _, fact := range []CapacityFact{
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 80, ObservedAt: now},
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHTTPHeaders, Sequence: 1, RemainingPct: 60, ObservedAt: now},
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, Sequence: 1, RemainingPct: 40, ObservedAt: now},
	} {
		if !ledger.Observe(fact) {
			t.Fatalf("Observe(%+v) rejected", fact)
		}
	}
	view := ledger.Capacity(key, CapacityBucketBase)
	if view.State != CapacityPositive || view.RemainingPct != 40 || view.Source != CapacitySourceLiveRateLimits {
		t.Fatalf("capacity = %+v, want live 40%%", view)
	}
}

func TestCodexCapacityLedgerRejectsOutOfOrderFacts(t *testing.T) {
	now := time.Unix(2_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	base := CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 3, RemainingPct: 60, ObservedAt: now}
	if !ledger.Observe(base) {
		t.Fatal("initial fact rejected")
	}
	for _, late := range []CapacityFact{
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, Sequence: 99, RemainingPct: 0, ObservedAt: now.Add(time.Minute)},
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Minute)},
	} {
		if ledger.Observe(late) {
			t.Fatalf("late fact accepted: %+v", late)
		}
	}
	if got := ledger.Capacity(key, CapacityBucketBase).RemainingPct; got != 60 {
		t.Fatalf("remaining = %d, want 60", got)
	}
	if !ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 3, Sequence: 1, RemainingPct: 70, ObservedAt: now}) {
		t.Fatal("new connection generation rejected")
	}
}

func TestCodexCapacityLedgerExpiresAtResetOrHorizon(t *testing.T) {
	now := time.Unix(3_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 10*time.Minute)
	key := codex.AccountKey("account-a")
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHTTPHeaders, Sequence: 1, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(2 * time.Minute)})
	now = now.Add(2 * time.Minute)
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("at reset capacity = %+v, want unknown", view)
	}
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 40, ObservedAt: now})
	now = now.Add(10*time.Minute + time.Nanosecond)
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("past horizon capacity = %+v, want unknown", view)
	}
}

func TestCodexCapacityLedgerHardZeroFence(t *testing.T) {
	now := time.Unix(4_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, Sequence: 1, RemainingPct: 70, ObservedAt: now})
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second), ResetAt: now.Add(time.Hour)})
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityZero || view.Source != CapacitySourceHardLimit {
		t.Fatalf("fenced capacity = %+v, want hard zero", view)
	}
	if !ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, Sequence: 3, RemainingPct: 55, ObservedAt: now.Add(2 * time.Second)}) {
		t.Fatal("newer live fact rejected")
	}
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 55 {
		t.Fatalf("lifted capacity = %+v, want positive 55", view)
	}
}

func TestCodexCapacityLedgerScopedBucketAndSharedFallback(t *testing.T) {
	now := time.Unix(5_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{
		FetchedAt: now,
		Result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour:                          {RemainingPct: 0},
			quota.WindowName("5h:gpt-5.3-codex-spark"): {RemainingPct: 70},
			quota.WindowName("7d:gpt-5.3-codex-spark"): {RemainingPct: 40},
		}},
	})
	spark := ledger.Capacity(key, CapacityBucketForModel(codexSparkModel))
	if spark.State != CapacityPositive || spark.RemainingPct != 40 || !spark.Exact {
		t.Fatalf("spark capacity = %+v, want exact 40%%", spark)
	}
	other := ledger.Capacity(key, CapacityBucket("model:other"))
	if other.State != CapacityZero || other.Exact {
		t.Fatalf("other capacity = %+v, want shared zero fallback", other)
	}
}

func TestCodexCapacityLedgerRejectsOlderUsageSnapshot(t *testing.T) {
	now := time.Unix(5_500, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	result := func(remaining int) quota.Result {
		return quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: remaining},
		}}
	}
	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: result(75), FetchedAt: now})
	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: result(0), FetchedAt: now.Add(-time.Minute)})
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 75 {
		t.Fatalf("capacity = %+v, want newer 75%% snapshot", view)
	}
}

func TestCodexRouteChoicePrefersKnownPositiveAndIgnoresSystemActive(t *testing.T) {
	now := time.Unix(6_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "known", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 20, ObservedAt: now})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "unknown", AccessToken: "token-a", IsActive: true},
			{AccountKey: "known", AccessToken: "token-b"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "known" {
		t.Fatalf("account = %q, want known", choice.AccountKey)
	}
}

func TestCodexRouteChoiceUsesActiveLeaseCountOnlyAsTieBreak(t *testing.T) {
	now := time.Unix(7_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	for _, key := range []codex.AccountKey{"busy", "idle"} {
		ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 50, ObservedAt: now})
	}
	ledger.SetActiveLeases("busy", 2)
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "busy", AccessToken: "a"}, {AccountKey: "idle", AccessToken: "b", IsActive: true}}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "idle" {
		t.Fatalf("account = %q, want idle", choice.AccountKey)
	}
}

func TestCodexRouteChoiceUsesExactSparkCapacityAndScopedFallback(t *testing.T) {
	now := time.Unix(8_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0, ObservedAt: now})
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 60, ObservedAt: now})
	ledger.Observe(CapacityFact{AccountKey: "plus", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 40, ObservedAt: now})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "plus", AccessToken: "plus-token", PlanType: "plus"},
			{AccountKey: "pro", AccessToken: "pro-token", PlanType: "pro"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: codexSparkModel})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "pro" || choice.EffectiveModel != codexSparkModel {
		t.Fatalf("choice = %+v, want pro Spark", choice)
	}
	if len(choice.RequiredBuckets) != 1 || choice.RequiredBuckets[0] != CapacityBucketForModel(codexSparkModel) {
		t.Fatalf("buckets = %v, want Spark", choice.RequiredBuckets)
	}

	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second)})
	choice, err = selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: codexSparkModel})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "plus" || choice.EffectiveModel != codexFallbackModel {
		t.Fatalf("fallback choice = %+v, want plus base model", choice)
	}
}

func TestCodexRouteChoiceRequiresAllPreturnBuckets(t *testing.T) {
	now := time.Unix(9_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0, ObservedAt: now})
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 80, ObservedAt: now})
	ledger.Observe(CapacityFact{AccountKey: "plus", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 30, ObservedAt: now})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "pro", AccessToken: "pro-token", PlanType: "pro"},
			{AccountKey: "plus", AccessToken: "plus-token", PlanType: "plus"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{
		RequestedModel: codexSparkModel,
		RequiredModels: []string{codexFallbackModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "plus" {
		t.Fatalf("account = %q, want plus with shared capacity", choice.AccountKey)
	}
}

func TestCodexRouteChoiceReturnsTypedCachedLimit(t *testing.T) {
	now := time.Unix(10_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "zero", Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, Sequence: 1, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(time.Hour)})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "zero", AccessToken: "token"}}
	}, nil, ledger)
	_, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("error = %v, want CachedUsageLimitError", err)
	}
	if limit.ResetAt != now.Add(time.Hour) {
		t.Fatalf("reset = %v, want %v", limit.ResetAt, now.Add(time.Hour))
	}
}
