package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func resetTestSnapshot(at time.Time, name quota.WindowName, exact float64) QuotaSnapshot {
	return QuotaSnapshot{FetchedAt: at, Result: quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
		name: {RemainingPct: int(exact), RemainingPctExact: &exact, ResetAtUnix: at.Add(7 * 24 * time.Hour).Unix()},
	}}}
}

// Removing accepted live reset detection must leave this account on its old route.
func TestCodexQuotaResetDetectsLiveRecoveryOnceOutsideLock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	var events []CodexQuotaResetEvent
	ledger.OnReset = func(event CodexQuotaResetEvent) error {
		_ = ledger.Capacity(event.AccountKey, CapacityBucketBase)
		events = append(events, event)
		return nil
	}
	zero := resetTestSnapshot(now, quota.Window7Day, 0)
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", zero)
	if view := ledger.Capacity("account", CapacityBucketBase); view.State == CapacityZero {
		t.Fatal("live usage zero changed advisory admission policy")
	}
	now = now.Add(time.Second)
	positive := resetTestSnapshot(now, quota.Window7Day, 100)
	w := positive.Result.Windows[quota.Window7Day]
	w.ResetAtUnix = zero.Result.Windows[quota.Window7Day].ResetAtUnix
	positive.Result.Windows[quota.Window7Day] = w
	ledger.ObserveQuotaSnapshot("account", positive)
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", positive)
	now = now.Add(time.Second)
	positive.FetchedAt = now
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", positive)
	if len(events) != 1 || events[0].AccountKey != "account" || events[0].EventID == "" {
		t.Fatalf("reset events = %+v, want one account recovery", events)
	}
}

// Cached, expired, reordered, rounded and model-scoped observations must not migrate base chats.
func TestCodexQuotaResetRejectsUnprovenTransitions(t *testing.T) {
	for _, scenario := range []string{"initial", "cache", "stale", "reordered", "rounded", "scoped", "error", "future", "expired", "cached-result", "non-finite"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			calls := 0
			ledger.OnReset = func(CodexQuotaResetEvent) error { calls++; return nil }
			name := quota.Window7Day
			if scenario == "scoped" {
				name = quota.WindowName("7d:gpt-5.3-codex-spark")
			}
			zero := resetTestSnapshot(now, name, 0)
			if scenario == "rounded" {
				w := zero.Result.Windows[name]
				exact := 0.1
				w.RemainingPctExact = &exact
				zero.Result.Windows[name] = w
			}
			oldStream := ledger.NewObservationStream()
			if scenario == "cache" {
				ledger.ObserveQuotaSnapshot("account", zero)
			} else if scenario != "initial" {
				ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", zero)
			}
			now = now.Add(time.Second)
			if scenario == "stale" {
				now = now.Add(time.Minute)
			}
			positive := resetTestSnapshot(now, name, 100)
			if scenario == "future" {
				positive.FetchedAt = now.Add(time.Second)
			}
			if scenario == "expired" {
				w := positive.Result.Windows[name]
				w.ResetAtUnix = now.Unix()
				positive.Result.Windows[name] = w
			}
			if scenario == "cached-result" {
				positive.Result.CacheAge = 1
			}
			if scenario == "non-finite" {
				w := positive.Result.Windows[name]
				exact := math.NaN()
				w.RemainingPctExact = &exact
				positive.Result.Windows[name] = w
			}
			if scenario == "error" {
				positive.Result.Status = quota.StatusError
			}
			stream := ledger.NewObservationStream()
			if scenario == "reordered" {
				stream = oldStream
			}
			ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", positive)
			if calls != 0 {
				t.Fatalf("reset calls = %d, want none", calls)
			}
		})
	}
}

func TestCodexQuotaResetConcurrentRecoveryDeliveredOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	var calls atomic.Int32
	ledger.OnReset = func(event CodexQuotaResetEvent) error {
		calls.Add(1)
		_ = ledger.Capacity(event.AccountKey, CapacityBucketBase)
		return nil
	}
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now.Add(-time.Second), quota.Window7Day, 0))
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now, quota.Window7Day, 100))
		}()
	}
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent reset deliveries = %d, want one", calls.Load())
	}
}

func TestCodexQuotaResetInvalidWindowDoesNotReplaceBaseline(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	calls := 0
	ledger.OnReset = func(CodexQuotaResetEvent) error { calls++; return nil }
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now, quota.Window7Day, 0))
	now = now.Add(time.Second)
	bad := resetTestSnapshot(now, quota.Window7Day, 100)
	w := bad.Result.Windows[quota.Window7Day]
	exact := math.NaN()
	w.RemainingPctExact = &exact
	bad.Result.Windows[quota.Window7Day] = w
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", bad)
	now = now.Add(time.Second)
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now, quota.Window7Day, 100))
	if calls != 1 {
		t.Fatalf("valid recovery notifications=%d, want one", calls)
	}
}

// Both live traffic transports must signal recovery even before the next poll.
func TestCodexQuotaResetDetectsHTTPAndWebSocketEvidence(t *testing.T) {
	for _, source := range []CapacitySource{CapacitySourceHTTPHeaders, CapacitySourceLiveRateLimits} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			calls := 0
			ledger.OnReset = func(CodexQuotaResetEvent) error { calls++; return nil }
			stream := ledger.NewObservationStream()
			for _, remaining := range []float64{0, 100, 99} {
				ledger.Observe(stream.Stamp(CapacityFact{AccountKey: "account", Bucket: CapacityBucketBase, Source: source,
					RemainingPct: int(remaining), Windows: resetTestSnapshot(now, quota.Window7Day, remaining).Result.Windows,
					ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}))
				now = now.Add(time.Second)
			}
			if calls != 1 {
				t.Fatalf("live reset notifications=%d, want one", calls)
			}
		})
	}
}

func TestCodexQuotaResetPollRecoveryReplacesOlderLiveZero(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	zero := resetTestSnapshot(now, quota.Window7Day, 0)
	exact := 80.0
	zero.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 80, RemainingPctExact: &exact, ResetAtUnix: now.Add(4 * time.Hour).Unix()}
	ledger.Observe(ledger.NewObservationStream().Stamp(CapacityFact{AccountKey: "account", Bucket: CapacityBucketBase,
		RemainingPct: 0, Source: CapacitySourceLiveRateLimits, Windows: zero.Result.Windows,
		ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour), Confidence: CapacityConfidenceAuthoritative}))
	now = now.Add(time.Second)
	positive := resetTestSnapshot(now, quota.Window7Day, 100)
	exact = 100
	positive.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, RemainingPctExact: &exact, ResetAtUnix: now.Add(4 * time.Hour).Unix()}
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", positive)
	if view := ledger.Capacity("account", CapacityBucketBase); view.State != CapacityPositive {
		t.Fatalf("reset quota admission=%+v, want positive", view)
	}
}

func TestCodexQuotaResetPollRecoveryLiftsWeeklyHardFence(t *testing.T) {
	for _, scenario := range []string{"restored", "older cursor", "older timestamp", "weekly zero", "missing weekly", "expired weekly", "weekly epoch regressed"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			stream := ledger.NewObservationStream()
			weeklyReset := now.Add(7 * 24 * time.Hour)
			ledger.Observe(ledger.NewObservationStream().Stamp(CapacityFact{AccountKey: "account", Bucket: CapacityBucketBase,
				RemainingPct: 0, Source: CapacitySourceHardLimit, ObservedAt: now,
				ResetAt: weeklyReset, Confidence: CapacityConfidenceAuthoritative}))
			if scenario != "older cursor" {
				stream = ledger.NewObservationStream()
			}
			now = now.Add(time.Second)
			positive := resetTestSnapshot(now, quota.Window7Day, 100)
			weekly := positive.Result.Windows[quota.Window7Day]
			weekly.ResetAtUnix = weeklyReset.Unix()
			positive.Result.Windows[quota.Window7Day] = weekly
			exact := 100.0
			positive.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, RemainingPctExact: &exact, ResetAtUnix: now.Add(4 * time.Hour).Unix()}
			switch scenario {
			case "older timestamp":
				positive.FetchedAt = now.Add(-2 * time.Second)
			case "weekly zero":
				exhausted := 0.0
				weekly.RemainingPct, weekly.RemainingPctExact = 0, &exhausted
				positive.Result.Windows[quota.Window7Day] = weekly
			case "missing weekly":
				delete(positive.Result.Windows, quota.Window7Day)
			case "expired weekly":
				weekly.ResetAtUnix = now.Add(-time.Second).Unix()
				positive.Result.Windows[quota.Window7Day] = weekly
			case "weekly epoch regressed":
				weekly.ResetAtUnix = weeklyReset.Add(-time.Second).Unix()
				positive.Result.Windows[quota.Window7Day] = weekly
			}
			ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", positive)
			view := ledger.Capacity("account", CapacityBucketBase)
			if scenario == "restored" && view.State != CapacityPositive {
				t.Fatalf("restored shared quota remained fenced: %+v", view)
			}
			if scenario != "restored" && (view.State != CapacityZero || view.Source != CapacitySourceHardLimit) {
				t.Fatalf("unproven reset lifted hard fence: %+v", view)
			}
		})
	}
}

func TestCodexIncludedCapacityRejectsNewestAuthenticatedZero(t *testing.T) {
	for _, scenario := range []string{"partial reset", "complete reset", "same timestamp zero", "cached zero", "stale zero", "older cursor zero", "scoped zero", "future zero"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			older := ledger.NewObservationStream()
			positive := resetTestSnapshot(now, quota.Window7Day, 100)
			five := 100.0
			positive.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, RemainingPctExact: &five, ResetAtUnix: now.Add(time.Hour).Unix()}
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", positive)
			if scenario != "same timestamp zero" {
				now = now.Add(time.Second)
			}
			zero := resetTestSnapshot(now, quota.Window7Day, 0)
			exhausted := 0.0
			zero.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 0, RemainingPctExact: &exhausted, ResetAtUnix: positive.Result.Windows[quota.Window5Hour].ResetAtUnix}
			bucket := CapacityBucketBase
			stream := ledger.NewObservationStream()
			switch scenario {
			case "cached zero":
				ledger.ObserveQuotaSnapshot("account", zero)
			case "stale zero":
				zero.FetchedAt = now.Add(-time.Minute)
				ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", zero)
			case "older cursor zero":
				ledger.ObserveLivePositiveQuotaSnapshot(older, "account", zero)
			case "future zero":
				zero.FetchedAt = now.Add(time.Second)
				ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", zero)
			case "scoped zero":
				bucket = CapacityBucketForModel(codexSparkModel)
				zero.Result.Windows = map[quota.WindowName]quota.Window{quota.WindowName("5h:" + codexSparkModel): zero.Result.Windows[quota.Window5Hour]}
				ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", zero)
			default:
				ledger.ObserveLivePositiveQuotaSnapshot(stream, "account", zero)
				if scenario == "partial reset" || scenario == "complete reset" {
					now = now.Add(time.Second)
					recovered := resetTestSnapshot(now, quota.Window7Day, 100)
					recovered.Result.Windows[quota.Window5Hour] = zero.Result.Windows[quota.Window5Hour]
					if scenario == "complete reset" {
						recovered.Result.Windows[quota.Window5Hour] = positive.Result.Windows[quota.Window5Hour]
					}
					ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", recovered)
				}
			}
			if ordinary := ledger.Capacity("account", bucket); ordinary.State != CapacityPositive {
				t.Fatalf("ordinary credit admission changed: %+v", ordinary)
			}
			want := CapacityPositive
			if scenario == "partial reset" || scenario == "same timestamp zero" || scenario == "scoped zero" {
				want = CapacityZero
			}
			if included := ledger.IncludedCapacity("account", bucket); included.State != want {
				t.Fatalf("included capacity=%+v, want state %v", included, want)
			}
		})
	}
}

func TestCodexIncludedCapacityPreservesIndependentScopedQuota(t *testing.T) {
	for _, exactScoped := range []bool{false, true} {
		t.Run(fmt.Sprint(exactScoped), func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			positive := resetTestSnapshot(now, quota.Window7Day, 100)
			five := 100.0
			positive.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, RemainingPctExact: &five, ResetAtUnix: now.Add(time.Hour).Unix()}
			if exactScoped {
				positive.Result.Windows[quota.WindowName("5h:"+codexSparkModel)] = positive.Result.Windows[quota.Window5Hour]
				positive.Result.Windows[quota.WindowName("7d:"+codexSparkModel)] = positive.Result.Windows[quota.Window7Day]
			}
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", positive)
			now = now.Add(time.Second)
			zero := resetTestSnapshot(now, quota.Window7Day, 0)
			exhausted := 0.0
			zero.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 0, RemainingPctExact: &exhausted, ResetAtUnix: now.Add(time.Hour).Unix()}
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", zero)
			want := CapacityZero
			if exactScoped {
				want = CapacityPositive
			}
			if included := ledger.IncludedCapacity("account", CapacityBucketForModel(codexSparkModel)); included.State != want {
				t.Fatalf("scoped included quota=%+v, want state %v", included, want)
			}
		})
	}
}

// Failed journal writes must leave reset notification pending for the next fresh observation.
func TestCodexQuotaResetRetriesFailedNotificationWithSameID(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	var ids []string
	ledger.OnReset = func(event CodexQuotaResetEvent) error {
		ids = append(ids, event.EventID)
		if len(ids) == 1 {
			return errors.New("journal unavailable")
		}
		return nil
	}
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now, quota.Window7Day, 0))
	for range 3 {
		now = now.Add(time.Second)
		ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", resetTestSnapshot(now, quota.Window7Day, 100))
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("notification IDs = %v, want one retry", ids)
	}
}

// Reset control must fetch despite the ordinary cooldown and never invent successful capacity.
func TestCodexCapacityForceRefreshBypassesCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reader := &codexRoutingUsageReaderStub{results: map[codex.AccountKey]codex.UsageObservation{
		"account": {Result: resetTestSnapshot(now, quota.Window7Day, 0).Result},
	}, errors: map[codex.AccountKey]error{}, panics: map[codex.AccountKey]bool{}, calls: map[codex.AccountKey]int{}}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }, Interval: time.Hour}
	refresher.Refresh(context.Background(), []codex.AccountKey{"account"})
	now = now.Add(time.Second)
	reader.results["account"] = codex.UsageObservation{Result: resetTestSnapshot(now, quota.Window7Day, 100).Result}
	reader.results["account"].Result.Windows[quota.Window5Hour] = resetTestSnapshot(now, quota.Window5Hour, 100).Result.Windows[quota.Window5Hour]
	if !refresher.ForceRefresh(context.Background(), []codex.AccountKey{"account"}) || ledger.Capacity("account", CapacityBucketBase).State != CapacityPositive {
		t.Fatal("forced reset refresh did not publish positive quota")
	}
	reader.errors["account"] = errors.New("usage unavailable")
	if refresher.ForceRefresh(context.Background(), []codex.AccountKey{"account"}) {
		t.Fatal("failed forced refresh reported published quota")
	}
}

func TestCodexCapacityForceRefreshRequiresFreshPositiveSharedWindows(t *testing.T) {
	for _, scenario := range []string{"zero", "partial", "stale", "cached", "reordered"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
			prior := resetTestSnapshot(now, quota.Window7Day, 100)
			prior.Result.Windows[quota.Window5Hour] = resetTestSnapshot(now, quota.Window5Hour, 100).Result.Windows[quota.Window5Hour]
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", prior)
			now = now.Add(time.Second)
			reader := &codexRoutingUsageReaderStub{results: map[codex.AccountKey]codex.UsageObservation{"account": {Result: prior.Result}},
				errors: map[codex.AccountKey]error{}, panics: map[codex.AccountKey]bool{}, calls: map[codex.AccountKey]int{}}
			switch scenario {
			case "zero":
				reader.results["account"] = codex.UsageObservation{Result: resetTestSnapshot(now, quota.Window7Day, 0).Result}
			case "partial":
				delete(reader.results["account"].Result.Windows, quota.Window5Hour)
			case "stale":
				w := reader.results["account"].Result.Windows[quota.Window7Day]
				w.ResetAtUnix = now.Unix()
				reader.results["account"].Result.Windows[quota.Window7Day] = w
			case "cached":
				result := reader.results["account"]
				result.Result.CacheAge = 1
				reader.results["account"] = result
			}
			refresher := &CodexRoutingCapacityRefresher{Usage: reader, Capacity: ledger, Now: func() time.Time { return now }}
			if scenario == "reordered" {
				refresher.Usage = codexResetUsageReadFunc(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
					ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "account", prior)
					return reader.results["account"], nil
				})
			}
			if refresher.ForceRefresh(context.Background(), []codex.AccountKey{"account"}) {
				t.Fatalf("%s reset refresh accepted old or unusable included quota", scenario)
			}
		})
	}
}

type codexResetUsageReadFunc func(context.Context, codex.AccountKey) (codex.UsageObservation, error)

func (f codexResetUsageReadFunc) Read(ctx context.Context, account codex.AccountKey) (codex.UsageObservation, error) {
	return f(ctx, account)
}
