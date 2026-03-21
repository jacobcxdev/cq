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

	if r.Cache != nil {
		if err := r.Cache.Put(ctx, string(id), results); err != nil {
			// Cache write failure is non-fatal; log and continue.
			fmt.Fprintf(os.Stderr, "cq: cache put %s: %v\n", id, err)
		}
	}
	return results
}
