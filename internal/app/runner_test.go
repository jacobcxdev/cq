package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

// fixedClock implements Clock with a fixed time.
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// mockProvider implements provider.Provider.
type mockProvider struct {
	id         provider.ID // used in struct literals for clarity; not part of the interface
	results    []quota.Result
	err        error
	called     bool
	discovered []provider.Account
}

func (m *mockProvider) Fetch(_ context.Context, _ time.Time) ([]quota.Result, error) {
	m.called = true
	return m.results, m.err
}

func (m *mockProvider) DiscoverAccounts(_ context.Context) ([]provider.Account, error) {
	return m.discovered, nil
}

// mockCache implements Cache.
type mockCache struct {
	data    map[string][]quota.Result
	putErr  error
	getErr  error
	ageVal  time.Duration
	ageOK   bool
}

func (c *mockCache) Get(_ context.Context, id string) ([]quota.Result, bool, error) {
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	if r, ok := c.data[id]; ok {
		return r, true, nil
	}
	return nil, false, nil
}

func (c *mockCache) Age(_ context.Context, _ string) (time.Duration, bool) {
	return c.ageVal, c.ageOK
}

func (c *mockCache) Put(_ context.Context, id string, results []quota.Result) error {
	if c.putErr != nil {
		return c.putErr
	}
	c.data[id] = results
	return nil
}

func (c *mockCache) Delete(_ context.Context, id string) error {
	delete(c.data, id)
	return nil
}

// captureRenderer implements Renderer, storing the last rendered report.
type captureRenderer struct{ report Report }

func (r *captureRenderer) Render(_ context.Context, report Report) error {
	r.report = report
	return nil
}

func TestRunnerRun(t *testing.T) {
	p := &mockProvider{
		id: provider.Codex,
		results: []quota.Result{
			{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 80},
			}},
		},
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(cr.report.Providers) != 1 {
		t.Fatalf("report has %d providers, want 1", len(cr.report.Providers))
	}
	if cr.report.Providers[0].ID != provider.Codex {
		t.Errorf("provider ID = %q, want %q", cr.report.Providers[0].ID, provider.Codex)
	}
}

func TestRunnerFetchErrorBecomesErrorResult(t *testing.T) {
	p := &mockProvider{
		id:  provider.Codex,
		err: errors.New("network failure"),
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("result status = %q, want %q", results[0].Status, quota.StatusError)
	}
	if results[0].Error == nil || results[0].Error.Code != "fetch_failed" {
		t.Errorf("unexpected error info: %+v", results[0].Error)
	}
}

func TestRunnerUnknownProviderErrors(t *testing.T) {
	runner := &Runner{
		Clock:    fixedClock(time.Unix(1000, 0)),
		Services: map[provider.ID]provider.Services{},
		Renderer: &captureRenderer{},
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}

func TestRunnerUsesCache(t *testing.T) {
	cached := []quota.Result{{Status: quota.StatusOK}}
	p := &mockProvider{id: provider.Codex}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Codex): cached,
		}},
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p.called {
		t.Error("provider was called despite cache hit")
	}
	if len(cr.report.Providers[0].Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(cr.report.Providers[0].Results))
	}
}

func TestRunnerUsesCacheAndAddsExpiredDiscoveredAccount(t *testing.T) {
	cached := []quota.Result{{
		Status:    quota.StatusOK,
		AccountID: "acct-1",
		Email:     "cached@example.com",
		Plan:      "plus",
	}}
	p := &mockProvider{
		id: provider.Codex,
		discovered: []provider.Account{
			{AccountID: "acct-1", Email: "cached@example.com"},
			{AccountID: "acct-2", Email: "expired@example.com"},
		},
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Codex): cached,
		}},
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p.called {
		t.Error("provider was called despite cache hit")
	}
	results := cr.report.Providers[0].Results
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if results[1].Status != quota.StatusError {
		t.Fatalf("results[1].Status = %q, want error", results[1].Status)
	}
	if results[1].Error == nil || results[1].Error.Code != "auth_expired" {
		t.Fatalf("results[1].Error = %+v, want auth_expired", results[1].Error)
	}
	if results[1].Email != "expired@example.com" {
		t.Fatalf("results[1].Email = %q, want expired@example.com", results[1].Email)
	}
	if results[1].AccountID != "acct-2" {
		t.Fatalf("results[1].AccountID = %q, want acct-2", results[1].AccountID)
	}
}

func TestRunnerUsesCacheAndAddsExpiredDiscoveredClaudeAccount(t *testing.T) {
	cached := []quota.Result{{
		Status:    quota.StatusOK,
		AccountID: "claude-acct-1",
		Email:     "cached@example.com",
		Plan:      "max",
	}}
	p := &mockProvider{
		id: provider.Claude,
		discovered: []provider.Account{
			{AccountID: "claude-acct-1", Email: "cached@example.com"},
			{AccountID: "claude-acct-2", Email: "expired@example.com"},
		},
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Claude): cached,
		}},
		Services: map[provider.ID]provider.Services{
			provider.Claude: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Claude}})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p.called {
		t.Error("provider was called despite cache hit")
	}
	results := cr.report.Providers[0].Results
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Fatalf("results[0].Status = %q, want ok", results[0].Status)
	}
	if results[1].Error == nil || results[1].Error.Code != "auth_expired" {
		t.Fatalf("results[1].Error = %+v, want auth_expired", results[1].Error)
	}
	if results[1].Email != "expired@example.com" {
		t.Fatalf("results[1].Email = %q, want expired@example.com", results[1].Email)
	}
	if results[1].AccountID != "claude-acct-2" {
		t.Fatalf("results[1].AccountID = %q, want claude-acct-2", results[1].AccountID)
	}
}

func TestRunnerUsesCacheAndAddsExpiredDiscoveredGeminiAccount(t *testing.T) {
	p := &mockProvider{
		id: provider.Gemini,
		discovered: []provider.Account{{Email: "gemini@example.com"}},
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Gemini): {},
		}},
		Services: map[provider.ID]provider.Services{
			provider.Gemini: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Gemini}})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p.called {
		t.Error("provider was called despite cache hit")
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Fatalf("results[0].Status = %q, want error", results[0].Status)
	}
	if results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results[0].Error = %+v, want auth_expired", results[0].Error)
	}
	if results[0].Email != "gemini@example.com" {
		t.Fatalf("results[0].Email = %q, want gemini@example.com", results[0].Email)
	}
}

func TestRunnerUsesCacheAndMarksSynthesisedDiscoveredAccountActive(t *testing.T) {
	p := &mockProvider{
		id: provider.Gemini,
		discovered: []provider.Account{{Email: "active@example.com", Active: true}},
	}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Gemini): {},
		}},
		Services: map[provider.ID]provider.Services{
			provider.Gemini: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Gemini}})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if !results[0].Active {
		t.Fatalf("results[0].Active = %v, want true", results[0].Active)
	}
}

func TestRunnerBypassesCacheOnRefresh(t *testing.T) {
	cached := []quota.Result{{Status: quota.StatusOK}}
	fresh := []quota.Result{{Status: quota.StatusOK, Plan: "pro"}}
	p := &mockProvider{id: provider.Codex, results: fresh}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{
			string(provider.Codex): cached,
		}},
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
		Refresh:   true,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !p.called {
		t.Error("provider was not called despite refresh=true")
	}
	if cr.report.Providers[0].Results[0].Plan != "pro" {
		t.Errorf("expected fresh result with plan=pro, got %q", cr.report.Providers[0].Results[0].Plan)
	}
}

func TestRunnerCachePutErrorIsNonFatal(t *testing.T) {
	// A cache whose Put always fails must not prevent results from being returned.
	fresh := []quota.Result{{Status: quota.StatusOK, Plan: "pro"}}
	p := &mockProvider{id: provider.Codex, results: fresh}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{
			data:   map[string][]quota.Result{},
			putErr: errors.New("disk full"),
		},
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned unexpected error despite non-fatal cache put failure: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 || results[0].Status != quota.StatusOK {
		t.Errorf("expected one OK result, got %+v", results)
	}
}

// panicProvider implements provider.Provider by panicking on Fetch.
type panicProvider struct {
	id  provider.ID // used in struct literals for clarity; not part of the interface
	msg string
}

func (p *panicProvider) Fetch(_ context.Context, _ time.Time) ([]quota.Result, error) {
	panic(p.msg)
}

func TestRunnerPanicInProviderReturnsErrorResult(t *testing.T) {
	p := &panicProvider{id: provider.Codex, msg: "boom"}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned unexpected top-level error after panic: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("expected 1 error result, got %d", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("result status = %q, want %q", results[0].Status, quota.StatusError)
	}
	if results[0].Error == nil || results[0].Error.Code != "panic" {
		t.Errorf("expected error code %q, got %+v", "panic", results[0].Error)
	}
}

func TestRunnerEmptyResultsBecomesErrorResult(t *testing.T) {
	// A provider that returns an empty slice must produce an "empty_result" error.
	p := &mockProvider{id: provider.Codex, results: []quota.Result{}}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("expected 1 error result, got %d", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("result status = %q, want %q", results[0].Status, quota.StatusError)
	}
	if results[0].Error == nil || results[0].Error.Code != "empty_result" {
		t.Errorf("expected error code %q, got %+v", "empty_result", results[0].Error)
	}
}

func TestRunnerBackfillFromCacheOnError(t *testing.T) {
	// Provider returns one OK result and one error. Cache has a previous good
	// result for the errored account. The error should be replaced with cached data.
	okResult := quota.Result{
		Status:    quota.StatusOK,
		AccountID: "acct-1",
		Email:     "a@b.com",
		Plan:      "max",
		Windows:   map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 80}},
	}
	errResult := quota.Result{
		Status:    quota.StatusError,
		AccountID: "acct-2",
		Email:     "c@d.com",
		Error:     &quota.ErrorInfo{Code: "api_error", HTTPStatus: 429},
	}
	cachedGood := quota.Result{
		Status:    quota.StatusOK,
		AccountID: "acct-2",
		Email:     "c@d.com",
		Plan:      "pro",
		Windows:   map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 60}},
	}

	p := &mockProvider{id: provider.Claude, results: []quota.Result{okResult, errResult}}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{
			data:   map[string][]quota.Result{string(provider.Claude): {okResult, cachedGood}},
			ageVal: 5 * time.Minute,
			ageOK:  true,
		},
		Services: map[provider.ID]provider.Services{
			provider.Claude: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Claude},
		Refresh:   true,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}

	// Second result should be backfilled from cache.
	r := results[1]
	if r.Status != quota.StatusOK {
		t.Errorf("backfilled result status = %q, want %q", r.Status, quota.StatusOK)
	}
	if r.Plan != "pro" {
		t.Errorf("backfilled result plan = %q, want %q", r.Plan, "pro")
	}
	if r.CacheAge != 300 {
		t.Errorf("CacheAge = %d, want 300 (5 minutes)", r.CacheAge)
	}
	// Original error preserved on backfilled result for display.
	if r.Error == nil || r.Error.Code != "api_error" || r.Error.HTTPStatus != 429 {
		t.Errorf("expected original error preserved, got %+v", r.Error)
	}
}

func TestRunnerBackfillNoCache(t *testing.T) {
	// Provider returns an error but no cached data exists. Error passes through.
	errResult := quota.Result{
		Status:    quota.StatusError,
		AccountID: "acct-1",
		Email:     "a@b.com",
		Error:     &quota.ErrorInfo{Code: "api_error", HTTPStatus: 429},
	}
	p := &mockProvider{id: provider.Claude, results: []quota.Result{errResult}}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{data: map[string][]quota.Result{}},
		Services: map[provider.ID]provider.Services{
			provider.Claude: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Claude},
		Refresh:   true,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("result status = %q, want error (no cache to backfill)", results[0].Status)
	}
}

func TestRunnerDoesNotCacheAllErrors(t *testing.T) {
	// If all results are errors, they should NOT be cached (would overwrite good data).
	errResult := quota.Result{
		Status: quota.StatusError,
		Error:  &quota.ErrorInfo{Code: "api_error", HTTPStatus: 429},
	}
	p := &mockProvider{id: provider.Claude, results: []quota.Result{errResult}}
	cr := &captureRenderer{}
	cache := &mockCache{data: map[string][]quota.Result{}}
	runner := &Runner{
		Clock:    fixedClock(time.Unix(1000, 0)),
		Cache:    cache,
		Services: map[provider.ID]provider.Services{provider.Claude: {Usage: p}},
		Renderer: cr,
	}

	_ = runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Claude},
		Refresh:   true,
	})
	if _, ok := cache.data[string(provider.Claude)]; ok {
		t.Error("all-error results should not be cached")
	}
}

func TestRunnerCachesOnlyUsableRows(t *testing.T) {
	// Provider returns one usable result and one auth_expired error.
	// The cache should only contain the usable result, not the error row.
	usable := quota.Result{
		Status:    quota.StatusOK,
		AccountID: "acct-good",
		Email:     "good@example.com",
		Plan:      "max",
		Windows:   map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 70}},
	}
	errRow := quota.Result{
		Status:    quota.StatusError,
		AccountID: "acct-expired",
		Email:     "expired@example.com",
		Error:     &quota.ErrorInfo{Code: "auth_expired"},
	}

	p := &mockProvider{id: provider.Claude, results: []quota.Result{usable, errRow}}
	cr := &captureRenderer{}
	cache := &mockCache{data: map[string][]quota.Result{}}
	runner := &Runner{
		Clock:    fixedClock(time.Unix(1000, 0)),
		Cache:    cache,
		Services: map[provider.ID]provider.Services{provider.Claude: {Usage: p}},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Claude},
		Refresh:   true,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The renderer should see both results (error row is not filtered from display).
	if len(cr.report.Providers[0].Results) != 2 {
		t.Fatalf("renderer results len = %d, want 2", len(cr.report.Providers[0].Results))
	}

	// But the cache should only contain the usable row.
	cached, ok := cache.data[string(provider.Claude)]
	if !ok {
		t.Fatal("cache has no entry for Claude despite having a usable result")
	}
	for _, c := range cached {
		if !c.IsUsable() {
			t.Errorf("cache contains non-usable row: %+v", c)
		}
	}
	if len(cached) != 1 || cached[0].AccountID != "acct-good" {
		t.Errorf("cache = %+v, want only the usable result", cached)
	}
}

func TestRunnerCacheGetErrorFallsThroughToProvider(t *testing.T) {
	// A cache whose Get returns an error must not block the provider call.
	fresh := []quota.Result{{Status: quota.StatusOK, Plan: "fresh"}}
	p := &mockProvider{id: provider.Codex, results: fresh}
	cr := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(time.Unix(1000, 0)),
		Cache: &mockCache{
			data:   map[string][]quota.Result{},
			getErr: errors.New("cache corrupted"),
			putErr: errors.New("disk full"), // also fail put to keep test simple
		},
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: p},
		},
		Renderer: cr,
	}

	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
	})
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if !p.called {
		t.Error("provider was not called despite cache Get error")
	}
	results := cr.report.Providers[0].Results
	if len(results) != 1 || results[0].Plan != "fresh" {
		t.Errorf("expected fresh result from provider, got %+v", results)
	}
}
