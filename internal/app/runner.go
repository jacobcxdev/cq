package app

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type Runner struct {
	Clock    Clock
	Cache    Cache   // nil = no caching
	History  History // nil = cold-start, no burn-rate smoothing
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

// ReportWarning names a failed optional operation without leaking its error.
type ReportWarning struct{ Code, Message string }

// BuildReportObserved retains completed rows on cancellation and captures safe
// optional-operation diagnostics. BuildReport preserves legacy diagnostics.
func (r *Runner) BuildReportObserved(ctx context.Context, req RunRequest) (Report, []ReportWarning, error) {
	var mu sync.Mutex
	var warnings []ReportWarning
	detached := false
	ctx = provider.WithObservation(ctx, provider.Observation{Warning: func(code, message string) {
		mu.Lock()
		defer mu.Unlock()
		if !detached && ctx.Err() == nil {
			warnings = append(warnings, ReportWarning{code, message})
		}
	}})
	report, err := r.BuildReport(ctx, req)
	mu.Lock()
	defer mu.Unlock()
	detached = true
	captured := append([]ReportWarning(nil), warnings...)
	sort.Slice(captured, func(i, j int) bool {
		if captured[i].Code != captured[j].Code {
			return captured[i].Code < captured[j].Code
		}
		return captured[i].Message < captured[j].Message
	})
	unique := captured[:0]
	for _, warning := range captured {
		if len(unique) == 0 || unique[len(unique)-1] != warning {
			unique = append(unique, warning)
		}
	}
	return report, unique, err
}

func (r *Runner) BuildReport(ctx context.Context, req RunRequest) (Report, error) {
	now := r.Clock.Now()
	observed := provider.Observed(ctx)

	// Validate all requested providers exist
	for _, id := range req.Providers {
		svc, ok := r.Services[id]
		if !ok || svc.Usage == nil {
			return Report{}, fmt.Errorf("unknown provider: %s", id)
		}
	}

	fetched := make(map[provider.ID][]quota.Result, len(req.Providers))
	var mu sync.Mutex
	frozen := false
	var wg sync.WaitGroup

	for _, id := range req.Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					code, message := "panic", fmt.Sprintf("%v", rv)
					if observed {
						code, message = "fetch_panic", "Quota provider failed."
					} else {
						fmt.Fprintf(os.Stderr, "cq: panic in %s provider: %v\n%s\n", id, rv, debug.Stack())
					}
					mu.Lock()
					if !frozen {
						fetched[id] = []quota.Result{quota.ErrorResult(code, message, 0)}
					}
					mu.Unlock()
				}
			}()
			work := ctx
			if observed {
				work = provider.WithResultObserver(ctx, func(rows []quota.Result) {
					mu.Lock()
					if !frozen {
						fetched[id] = rows
					}
					mu.Unlock()
				})
			}
			results := r.fetchOne(work, now, req.Refresh, id)
			mu.Lock()
			if !frozen && (ctx.Err() == nil || len(results) > 0) {
				fetched[id] = provider.CloneResults(results)
			}
			mu.Unlock()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	if observed {
		select {
		case <-done:
		case <-ctx.Done():
		}
	} else {
		<-done
	}
	// Workers may still be releasing their own resources after cancellation.
	// Freeze completed rows before constructing or enriching the report.
	mu.Lock()
	frozen = true
	snapshot := make(map[provider.ID][]quota.Result, len(fetched))
	for id, rows := range fetched {
		snapshot[id] = provider.CloneResults(rows)
	}
	mu.Unlock()

	var burnRates history.BurnRates
	if r.History != nil && ctx.Err() == nil {
		type historyResult struct {
			rates history.BurnRates
			err   error
		}
		update := func() (result historyResult) {
			if ctx.Err() != nil {
				result.err = ctx.Err()
				return
			}
			defer func() {
				if recover() != nil {
					result.err = fmt.Errorf("history update panicked")
				}
			}()
			if h, ok := r.History.(interface {
				UpdateAndGetBurnRatesObserved(context.Context, map[string][]quota.Result, int64, func(string, string)) (history.BurnRates, error)
			}); observed && ok {
				result.rates, result.err = h.UpdateAndGetBurnRatesObserved(ctx, providerFetched(snapshot), now.Unix(), func(code, message string) { provider.ObserveWarning(ctx, code, message) })
			} else {
				result.rates, result.err = r.History.UpdateAndGetBurnRates(ctx, providerFetched(snapshot), now.Unix())
			}
			return
		}
		var result historyResult
		if observed {
			completed := make(chan historyResult, 1)
			go func() { completed <- update() }()
			select {
			case result = <-completed:
			case <-ctx.Done():
			}
		} else {
			result = update()
		}
		burnRates = result.rates
		if result.err != nil && !provider.ObserveWarning(ctx, "history_update_failed", "Quota history update failed.") {
			fmt.Fprintf(os.Stderr, "cq: history update failed: %v\n", result.err)
		}
	}
	return buildReport(now, req.Providers, snapshot, burnRates), nil
}

func (r *Runner) fetchOne(ctx context.Context, now time.Time, refresh bool, id provider.ID) []quota.Result {
	if ctx.Err() != nil {
		return nil
	}
	if !refresh && r.Cache != nil {
		if cached, ok, err := r.cacheGet(ctx, id); err == nil && ok {
			cached = markCachedResults(cached, r.cacheAge(ctx, id))
			provider.ObserveResults(ctx, cached)
			return r.enrichCachedWithDiscovered(ctx, id, cached)
		} else if err != nil {
			provider.ObserveWarning(ctx, "cache_get_failed", "Quota cache read failed for "+string(id)+".")
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	p := r.Services[id].Usage
	results, err := p.Fetch(ctx, now)
	if err != nil {
		if ctx.Err() != nil {
			return results
		}
		message := err.Error()
		if provider.Observed(ctx) {
			message = "Quota request failed."
		}
		return []quota.Result{quota.ErrorResult("fetch_failed", message, 0)}
	}
	if len(results) == 0 {
		return []quota.Result{quota.ErrorResult("empty_result", "provider returned no results", 0)}
	}

	provider.ObserveResults(ctx, results)
	if ctx.Err() != nil {
		return results
	}
	// Backfill transient errors from cache: if an individual account failed
	// but we have a recent cached result for the same account, use it.
	if r.Cache != nil {
		results = r.backfillFromCache(ctx, id, results)
	}

	provider.ObserveResults(ctx, results)
	// Only cache usable rows. This prevents auth_expired and other transient
	// error rows from polluting the cache and being served as stale data.
	if r.Cache != nil && ctx.Err() == nil {
		var usable []quota.Result
		for _, res := range results {
			if res.IsUsable() {
				usable = append(usable, res)
			}
		}
		if len(usable) > 0 {
			if err := r.cachePut(ctx, id, usable); err != nil {
				if !provider.ObserveWarning(ctx, "cache_put_failed", "Quota cache write failed for "+string(id)+".") {
					fmt.Fprintf(os.Stderr, "cq: cache put %s: %v\n", id, err)
				}
			}
		}
	}
	return results
}

// Optional cache failures must not replace successfully observed provider rows.
func recoverCachePanic(ctx context.Context, err *error) {
	if value := recover(); value != nil {
		if !provider.Observed(ctx) {
			panic(value)
		}
		*err = fmt.Errorf("quota cache operation panicked")
	}
}

func (r *Runner) cacheGet(ctx context.Context, id provider.ID) (rows []quota.Result, ok bool, err error) {
	defer recoverCachePanic(ctx, &err)
	if c, ok := r.Cache.(interface {
		GetObserved(context.Context, string) ([]quota.Result, bool, error)
	}); provider.Observed(ctx) && ok {
		return c.GetObserved(ctx, string(id))
	}
	return r.Cache.Get(ctx, string(id))
}

func (r *Runner) cachePut(ctx context.Context, id provider.ID, rows []quota.Result) (err error) {
	defer recoverCachePanic(ctx, &err)
	return r.Cache.Put(ctx, string(id), rows)
}

func (r *Runner) cacheAge(ctx context.Context, id provider.ID) (age int64) {
	age = 1
	defer func() {
		if value := recover(); value != nil {
			if !provider.ObserveWarning(ctx, "cache_age_failed", "Quota cache age read failed for "+string(id)+".") {
				panic(value)
			}
		}
	}()
	if duration, ok := r.Cache.Age(ctx, string(id)); ok && duration > 0 {
		age = max(int64(duration.Seconds()), 1)
	}
	return age
}

func markCachedResults(results []quota.Result, age int64) []quota.Result {
	marked := append([]quota.Result(nil), results...)
	for index := range marked {
		marked[index].CacheAge = max(marked[index].CacheAge, age, 1)
	}
	return marked
}

// enrichCachedWithDiscovered takes cached results and merges in locally discovered
// accounts. For each discovered account:
//   - if a cached usable row already matches, keep it as-is
//   - if no cached row exists, emit an unverified row so the account remains visible
//
// Discovery alone does not prove that an account's credentials expired.
func (r *Runner) enrichCachedWithDiscovered(ctx context.Context, id provider.ID, cached []quota.Result) []quota.Result {
	if ctx.Err() != nil {
		return cached
	}
	svc := r.Services[id]
	disc, ok := svc.Usage.(provider.Discoverer)
	if !ok {
		// Provider doesn't support discovery — return cache as-is.
		return cached
	}
	accounts, err := disc.DiscoverAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		return cached
	}

	// Index cached rows by account identity.
	byID := make(map[string]bool)
	byEmail := make(map[string]bool)
	for _, c := range cached {
		if c.IsUsable() {
			if c.AccountID != "" {
				byID[c.AccountID] = true
			}
			if c.Email != "" {
				byEmail[c.Email] = true
			}
		}
	}

	out := make([]quota.Result, len(cached))
	copy(out, cached)

	for _, acct := range accounts {
		if acct.AccountID != "" && byID[acct.AccountID] {
			continue // already represented by a usable cached row
		}
		if acct.Email != "" && byEmail[acct.Email] {
			continue
		}
		// No usable cached row and no provider contact — keep the account visible
		// without claiming that its credentials expired.
		row := quota.ErrorResult("usage_unverified", "usage not available in cache", 0)
		row.AccountID = acct.AccountID
		row.Email = acct.Email
		row.Active = acct.Active
		out = append(out, row)
	}
	return out
}

// backfillFromCache replaces error results with cached usable results for the
// same account (matched by AccountID or Email). This handles transient failures
// like 429 rate limits — the user sees stale-but-usable data instead of an error.
func (r *Runner) backfillFromCache(ctx context.Context, id provider.ID, results []quota.Result) []quota.Result {
	if ctx.Err() != nil {
		return results
	}
	cached, ok, err := r.cacheGet(ctx, id)
	if err != nil || !ok {
		if err != nil {
			provider.ObserveWarning(ctx, "cache_get_failed", "Quota fallback cache read failed for "+string(id)+".")
		}
		return results
	}
	ageS := r.cacheAge(ctx, id)

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
			provider.ObserveWarning(ctx, "quota_stale_fallback", "Cached quota used after a failed fetch for "+string(id)+".")
			found.CacheAge = ageS
			found.Error = res.Error // preserve original error for display
			out[i] = found
		}
	}
	return out
}
