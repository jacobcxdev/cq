package proxy

import (
	"context"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	defaultCodexRoutingCapacityRefreshInterval = 5 * time.Minute
	defaultCodexRoutingCapacityRetryInterval   = 30 * time.Second
)

// CodexRoutingCapacityRefresher fetches bounded usage observations when route
// capacity has gone stale. It never turns failed observations into capacity.
type CodexRoutingCapacityRefresher struct {
	Usage    CodexPrimerUsageReaderAPI
	Capacity *CodexCapacityLedger
	Now      func() time.Time
	Interval time.Duration

	mu          sync.Mutex
	nextRefresh map[codex.AccountKey]time.Time
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
	}
	eligible := make([]codex.AccountKey, 0, len(unique))
	for _, account := range unique {
		if now.Before(r.nextRefresh[account]) {
			continue
		}
		r.nextRefresh[account] = now.Add(defaultCodexRoutingCapacityRetryInterval)
		eligible = append(eligible, account)
	}
	r.mu.Unlock()
	if len(eligible) == 0 {
		return false
	}

	type result struct {
		account     codex.AccountKey
		observation codex.UsageObservation
		err         error
		panicked    bool
	}
	results := make(chan result, len(eligible))
	for _, account := range eligible {
		go func() {
			outcome := result{account: account}
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
	var successful []codex.AccountKey
	for range eligible {
		outcome := <-results
		if outcome.panicked || outcome.err != nil || !outcome.observation.Result.IsUsable() || len(outcome.observation.Result.Windows) == 0 {
			continue
		}
		r.Capacity.ObserveQuotaSnapshot(outcome.account, QuotaSnapshot{
			Result:    outcome.observation.Result,
			FetchedAt: now,
		})
		published = true
		successful = append(successful, outcome.account)
	}
	if published {
		interval := r.Interval
		if interval <= 0 {
			interval = defaultCodexRoutingCapacityRefreshInterval
		}
		r.mu.Lock()
		for _, account := range successful {
			r.nextRefresh[account] = now.Add(interval)
		}
		r.mu.Unlock()
	}
	return published
}
