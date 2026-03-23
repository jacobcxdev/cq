package app

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type Runner struct {
	Clock    Clock
	Cache    Cache                              // nil = no caching
	Services map[provider.ID]provider.Services
	Renderer Renderer
}

func (r *Runner) Run(ctx context.Context, req RunRequest) error {
	report, err := r.BuildReport(ctx, req)
	if err != nil {
		return err
	}
	return r.Renderer.Render(ctx, report)
}

func (r *Runner) BuildReport(ctx context.Context, req RunRequest) (Report, error) {
	now := r.Clock.Now()

	// Validate all requested providers exist
	for _, id := range req.Providers {
		svc, ok := r.Services[id]
		if !ok || svc.Usage == nil {
			return Report{}, fmt.Errorf("unknown provider: %s", id)
		}
	}

	fetched := make(map[provider.ID][]quota.Result, len(req.Providers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, id := range req.Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Fprintf(os.Stderr, "cq: panic in %s provider: %v\n%s\n", id, rv, debug.Stack())
					mu.Lock()
					fetched[id] = []quota.Result{quota.ErrorResult("panic", fmt.Sprintf("%v", rv), 0)}
					mu.Unlock()
				}
			}()
			results := r.fetchOne(ctx, now, req.Refresh, id)
			mu.Lock()
			fetched[id] = results
			mu.Unlock()
		}()
	}
	wg.Wait()

	return buildReport(now, req.Providers, fetched), nil
}

func (r *Runner) fetchOne(ctx context.Context, now time.Time, refresh bool, id provider.ID) []quota.Result {
	if !refresh && r.Cache != nil {
		if cached, ok, err := r.Cache.Get(ctx, string(id)); err == nil && ok {
			return cached
		}
	}

	p := r.Services[id].Usage
	results, err := p.Fetch(ctx, now)
	if err != nil {
		return []quota.Result{quota.ErrorResult("fetch_failed", err.Error(), 0)}
	}
	if len(results) == 0 {
		return []quota.Result{quota.ErrorResult("empty_result", "provider returned no results", 0)}
	}

	// Backfill transient errors from cache: if an individual account failed
	// but we have a recent cached result for the same account, use it.
	if r.Cache != nil {
		results = r.backfillFromCache(ctx, id, results)
	}

	// Only cache results that have at least one usable entry.
	if r.Cache != nil {
		hasUsable := false
		for _, res := range results {
			if res.IsUsable() {
				hasUsable = true
				break
			}
		}
		if hasUsable {
			if err := r.Cache.Put(ctx, string(id), results); err != nil {
				fmt.Fprintf(os.Stderr, "cq: cache put %s: %v\n", id, err)
			}
		}
	}
	return results
}

// backfillFromCache replaces error results with cached usable results for the
// same account (matched by AccountID or Email). This handles transient failures
// like 429 rate limits — the user sees stale-but-usable data instead of an error.
func (r *Runner) backfillFromCache(ctx context.Context, id provider.ID, results []quota.Result) []quota.Result {
	cached, ok, err := r.Cache.Get(ctx, string(id))
	if err != nil || !ok {
		return results
	}
	age, hasAge := r.Cache.Age(ctx, string(id))
	ageS := int64(0)
	if hasAge {
		ageS = int64(age.Seconds())
	}

	// Index cached results by account identity.
	byID := make(map[string]quota.Result)
	byEmail := make(map[string]quota.Result)
	for _, c := range cached {
		if !c.IsUsable() {
			continue
		}
		if c.AccountID != "" {
			byID[c.AccountID] = c
		}
		if c.Email != "" {
			byEmail[c.Email] = c
		}
	}

	out := make([]quota.Result, len(results))
	copy(out, results)
	for i, res := range out {
		if res.IsUsable() {
			continue
		}
		var found quota.Result
		var ok bool
		if res.AccountID != "" {
			found, ok = byID[res.AccountID]
		}
		if !ok && res.Email != "" {
			found, ok = byEmail[res.Email]
		}
		if ok {
			found.CacheAge = ageS
			found.Error = res.Error // preserve original error for display
			out[i] = found
		}
	}
	return out
}
