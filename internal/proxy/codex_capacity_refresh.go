package proxy

import (
	"context"
	"errors"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

const (
	defaultCodexRoutingCapacityRefreshInterval = time.Minute
	defaultCodexRoutingCapacityRetryInterval   = 30 * time.Second
)

// CodexRoutingCapacityRefresher fetches bounded usage observations when route
// capacity has gone stale. It never turns failed observations into capacity.
type CodexRoutingCapacityRefresher struct {
	Usage              CodexPrimerUsageReaderAPI
	Capacity           *CodexCapacityLedger
	Now                func() time.Time
	Interval           time.Duration
	IntervalForAccount func(codex.AccountKey, map[quota.WindowName]quota.Window) time.Duration
	OnInventory        func(codex.Inventory)

	mu          sync.Mutex
	nextRefresh map[codex.AccountKey]time.Time
	inFlight    map[codex.AccountKey]bool
	lastSuccess map[codex.AccountKey]time.Time
}

// CodexPrimerUsageReaderAPI is the read-only usage boundary needed by routing.
type CodexPrimerUsageReaderAPI interface {
	Read(context.Context, codex.AccountKey) (codex.UsageObservation, error)
}

func (r *CodexRoutingCapacityRefresher) Refresh(ctx context.Context, accounts []codex.AccountKey) bool {
	if r == nil || r.Usage == nil || r.Capacity == nil {
		return false
	}
	unique := make([]codex.AccountKey, 0, len(accounts))
	seen := make(map[codex.AccountKey]struct{}, len(accounts))
	for _, account := range accounts {
		if account == "" {
			continue
		}
		if _, ok := seen[account]; ok {
			continue
		}
		seen[account] = struct{}{}
		unique = append(unique, account)
	}
	if len(unique) == 0 {
		return false
	}

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	r.mu.Lock()
	if r.nextRefresh == nil {
		r.nextRefresh = make(map[codex.AccountKey]time.Time)
		r.inFlight = make(map[codex.AccountKey]bool)
		r.lastSuccess = make(map[codex.AccountKey]time.Time)
	}
	eligible := make([]codex.AccountKey, 0, len(unique))
	for _, account := range unique {
		next := r.nextRefresh[account]
		if r.Interval <= 0 && !r.lastSuccess[account].IsZero() {
			windows, _ := r.Capacity.WindowSnapshot(account)
			if len(windows) > 0 {
				next = minRefreshTime(next, r.lastSuccess[account].Add(r.refreshInterval(account, windows)))
			}
		}
		if r.inFlight[account] || now.Before(next) {
			continue
		}
		r.inFlight[account] = true
		eligible = append(eligible, account)
	}
	r.mu.Unlock()
	if len(eligible) == 0 {
		return false
	}

	type result struct {
		account     codex.AccountKey
		observation codex.UsageObservation
		stream      *CodexCapacityObservationStream
		err         error
		panicked    bool
	}
	results := make(chan result, len(eligible))
	for _, account := range eligible {
		go func() {
			outcome := result{account: account, stream: r.Capacity.NewObservationStream()}
			defer func() {
				if recover() != nil {
					outcome.observation = codex.UsageObservation{}
					outcome.panicked = true
				}
				results <- outcome
			}()
			outcome.observation, outcome.err = r.Usage.Read(ctx, account)
		}()
	}

	published := false
	for range eligible {
		outcome := <-results
		completedAt := time.Now()
		if r.Now != nil {
			completedAt = r.Now()
		}
		retryAt := completedAt.Add(defaultCodexRoutingCapacityRetryInterval)
		var httpError *CodexUsageHTTPError
		if errors.As(outcome.err, &httpError) && httpError.RetryAt.After(retryAt) {
			retryAt = httpError.RetryAt
		}
		valid := !outcome.panicked && outcome.err == nil && outcome.observation.Result.IsUsable() && len(outcome.observation.Result.Windows) > 0
		if valid {
			interval := r.refreshInterval(outcome.account, outcome.observation.Result.Windows)
			retryAt = completedAt.Add(interval)
		}
		if valid {
			snapshot := QuotaSnapshot{
				Result:    outcome.observation.Result,
				FetchedAt: now,
			}
			r.Capacity.ObserveQuotaSnapshot(outcome.account, snapshot)
			r.Capacity.ObserveLivePositiveQuotaSnapshot(outcome.stream, outcome.account, snapshot)
			published = true
		}
		r.mu.Lock()
		delete(r.inFlight, outcome.account)
		r.nextRefresh[outcome.account] = retryAt
		if valid {
			r.lastSuccess[outcome.account] = completedAt
		} else {
			delete(r.lastSuccess, outcome.account)
		}
		r.mu.Unlock()
	}
	return published
}

// Run refreshes routable accounts for the lifetime of the service. Route-triggered
// reads share the same in-flight guard and cooldowns.
func (r *CodexRoutingCapacityRefresher) Run(ctx context.Context, inventory codex.CredentialInventory) {
	if r == nil || inventory == nil || r.Usage == nil || r.Capacity == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		r.refreshInventory(ctx, inventory)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *CodexRoutingCapacityRefresher) refreshInventory(ctx context.Context, inventory codex.CredentialInventory) {
	defer func() { _ = recover() }()
	view, err := inventory.List(ctx)
	if err != nil {
		return
	}
	if r.OnInventory != nil {
		r.OnInventory(view)
	}
	accounts := make([]codex.AccountKey, 0, len(view.Accounts))
	for _, account := range view.Accounts {
		if account.Routable && !account.Unstable {
			accounts = append(accounts, account.Key)
		}
	}
	r.Refresh(ctx, accounts)
}

func codexUsageRefreshInterval(windows map[quota.WindowName]quota.Window) time.Duration {
	remaining := 100.0
	for _, window := range windows {
		value := float64(window.RemainingPct)
		if window.RemainingPctExact != nil {
			value = *window.RemainingPctExact
		}
		remaining = min(remaining, value)
	}
	switch {
	case remaining <= 1:
		return 5 * time.Second
	case remaining <= 10:
		return 15 * time.Second
	case remaining <= 25:
		return 30 * time.Second
	default:
		return defaultCodexRoutingCapacityRefreshInterval
	}
}

func minRefreshTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func (r *CodexRoutingCapacityRefresher) refreshInterval(account codex.AccountKey, windows map[quota.WindowName]quota.Window) time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	interval := codexUsageRefreshInterval(windows)
	if r.IntervalForAccount != nil {
		if configured := r.IntervalForAccount(account, windows); configured > 0 {
			interval = min(interval, configured)
		}
	}
	return interval
}
