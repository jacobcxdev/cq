package main

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/history"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexReserveForecastUsesFreshImmutablePortfolio(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reads := 0
	estimates := history.RateEstimates{
		{ProviderID: "codex", AccountKey: "system-id", Window: "7d"}: {RatePctPerS: .01, Samples: 3, LastSeenUnix: now.Unix()},
		{ProviderID: "codex", AccountKey: "other-id", Window: "7d"}:  {RatePctPerS: .03, Samples: 3, LastSeenUnix: now.Unix()},
	}
	var readErr error
	forecast := newCodexReserveForecast(func() (history.RateEstimates, error) { reads++; return estimates, readErr }, func() time.Time { return now })
	forecast.ObserveInventory(codex.Inventory{Accounts: []codex.LogicalAccount{
		{Key: "system", Routable: true}, {Key: "other", Routable: true}, {Key: "unstable", Routable: true, Unstable: true}, {Key: "excluded"},
	}})
	result := func(id string, remaining float64) quota.Result {
		return quota.Result{AccountID: id, Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: int(remaining), RemainingPctExact: &remaining, ResetAtUnix: now.Add(time.Hour).Unix()}}}
	}
	system := result("system-id", 5)
	forecast.ObserveUsage("system", system, now)
	if got := forecast.DrainRate("system", "7d"); got != 0 {
		t.Fatalf("missing account rate = %v", got)
	}
	forecast.ObserveUsage("other", result("other-id", 0), now)
	check := func(want float64) {
		t.Helper()
		if got := forecast.DrainRate("system", "7d"); math.Abs(got-want) > 1e-12 {
			t.Fatalf("rate = %v, want %v", got, want)
		}
	}
	check(.04)
	*system.Windows["7d"].RemainingPctExact = 0
	estimates[history.BurnRateKey{ProviderID: "codex", AccountKey: "system-id", Window: "7d"}] = history.RateEstimate{RatePctPerS: 100, Samples: 3, LastSeenUnix: now.Unix()}
	check(.04)
	if reads != 2 {
		t.Fatalf("rate reads touched disk: %d", reads)
	}
	now = now.Add(121 * time.Second)
	check(0)
	forecast.ObserveInventory(codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Routable: true}, {Key: "other", Routable: true}}})
	forecast.ObserveUsage("system", result("system-id", 5), now)
	readErr = errors.New("history unavailable")
	forecast.ObserveUsage("other", result("other-id", 0), now)
	check(0)
}

func TestCodexReserveForecastColdHistoryAndInvalidEvidence(t *testing.T) {
	now := time.Unix(1800000000, 0)
	forecast := newCodexReserveForecast(func() (history.RateEstimates, error) { return nil, nil }, func() time.Time { return now })
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Routable: true}}}
	forecast.ObserveInventory(inventory)
	result := quota.Result{Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, ResetAtUnix: now.Add(time.Hour).Unix()}}}
	forecast.ObserveUsage("system", result, now)
	if got := forecast.DrainRate("system", "7d"); got <= 0 {
		t.Fatalf("cold history lost cycle-average estimate: %v", got)
	}
	forecast.ObserveUsage("system", result, now.Add(time.Second))
	if got := forecast.DrainRate("system", "7d"); got != 0 {
		t.Fatalf("future observation rate = %v", got)
	}
	forecast.ObserveUsage("system", result, now)
	result.Status = quota.StatusError
	forecast.ObserveUsage("system", result, now)
	if got := forecast.DrainRate("system", "7d"); got != 0 {
		t.Fatalf("invalid observation rate = %v", got)
	}
}

func TestCodexReserveForecastConcurrentReaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	forecast := newCodexReserveForecast(func() (history.RateEstimates, error) { return nil, nil }, func() time.Time { return now })
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Routable: true}}}
	result := quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, ResetAtUnix: now.Add(time.Hour).Unix()}}}
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				forecast.ObserveInventory(inventory)
				forecast.ObserveUsage("system", result, now)
				_ = forecast.DrainRate("system", "7d")
			}
		}()
	}
	workers.Wait()
}
