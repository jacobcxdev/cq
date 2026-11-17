package main

import (
	"math"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/history"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

const codexReserveForecastMaxAge = 2 * time.Minute

type codexReserveObservation struct {
	result quota.Result
	at     time.Time
}

// codexReserveForecast consumes existing capacity observations. Route admission
// reads only this snapshot; it never polls usage or reads history from disk.
type codexReserveForecast struct {
	mu           sync.Mutex
	read         func() (history.RateEstimates, error)
	now          func() time.Time
	accounts     []codex.AccountKey
	inventoryAt  time.Time
	observations map[codex.AccountKey]codexReserveObservation
	estimates    history.RateEstimates
	historyOK    bool
}

func newCodexReserveForecast(read func() (history.RateEstimates, error), now func() time.Time) *codexReserveForecast {
	if now == nil {
		now = time.Now
	}
	return &codexReserveForecast{read: read, now: now, observations: make(map[codex.AccountKey]codexReserveObservation)}
}

func (f *codexReserveForecast) ObserveInventory(inventory codex.Inventory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts = nil
	expected := make(map[codex.AccountKey]bool)
	for _, account := range inventory.Accounts {
		if account.Key != "" && account.Routable && !account.Unstable && !expected[account.Key] {
			f.accounts = append(f.accounts, account.Key)
			expected[account.Key] = true
		}
	}
	for account := range f.observations {
		if !expected[account] {
			delete(f.observations, account)
		}
	}
	f.inventoryAt = f.now()
}

func (f *codexReserveForecast) ObserveUsage(account codex.AccountKey, result quota.Result, observed time.Time) {
	now := f.now()
	valid := account != "" && result.IsUsable() && result.CacheAge == 0 && !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= codexReserveForecastMaxAge
	shared := false
	cloned := result
	cloned.AccountKey = string(account)
	cloned.Windows = make(map[quota.WindowName]quota.Window, len(result.Windows))
	for name, window := range result.Windows {
		if window.RemainingPctExact != nil {
			value := *window.RemainingPctExact
			window.RemainingPctExact = &value
		}
		if window.RecentBurnRate != nil {
			value := *window.RecentBurnRate
			window.RecentBurnRate = &value
		}
		cloned.Windows[name] = window
		if name == quota.Window5Hour || name == quota.Window7Day {
			shared = true
			remaining := float64(window.RemainingPct)
			if window.RemainingPctExact != nil {
				remaining = *window.RemainingPctExact
			}
			valid = valid && !math.IsNaN(remaining) && !math.IsInf(remaining, 0) && remaining >= 0 && remaining <= 100 && time.Unix(window.ResetAtUnix, 0).After(observed)
		}
	}
	var estimates history.RateEstimates
	var readErr error
	if valid && shared && f.read != nil {
		estimates, readErr = f.read()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !valid || !shared {
		delete(f.observations, account)
		return
	}
	if old, ok := f.observations[account]; ok && observed.Before(old.at) {
		return
	}
	f.observations[account] = codexReserveObservation{result: cloned, at: observed}
	f.historyOK = f.read != nil && readErr == nil
	f.estimates = make(history.RateEstimates, len(estimates))
	for key, estimate := range estimates {
		estimate.RecentRatePctPerS = nil
		f.estimates[key] = estimate
	}
}

func (f *codexReserveForecast) DrainRate(account codex.AccountKey, window quota.WindowName) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if !f.historyOK || len(f.accounts) == 0 || f.inventoryAt.IsZero() || f.inventoryAt.After(now) || now.Sub(f.inventoryAt) > codexReserveForecastMaxAge {
		return 0
	}
	results := make([]quota.Result, 0, len(f.accounts))
	found := false
	for _, key := range f.accounts {
		observation, ok := f.observations[key]
		if !ok || observation.at.After(now) || now.Sub(observation.at) > codexReserveForecastMaxAge {
			return 0
		}
		for name, value := range observation.result.Windows {
			if (name == quota.Window5Hour || name == quota.Window7Day) && !time.Unix(value.ResetAtUnix, 0).After(now) {
				return 0
			}
		}
		results = append(results, observation.result)
		found = found || key == account
	}
	if !found {
		return 0
	}
	estimates := make(history.RateEstimates, len(f.estimates))
	for key, estimate := range f.estimates {
		if estimate.LastSeenUnix > 0 && estimate.LastSeenUnix <= now.Unix() && now.Unix()-estimate.LastSeenUnix <= int64((5*time.Minute).Seconds()) {
			estimates[key] = estimate
		}
	}
	return app.CodexReserveDrainRate(now, quota.ReservePolicy{AccountKey: string(account), Window: window}, results, estimates)
}
