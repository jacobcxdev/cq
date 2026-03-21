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
	id      provider.ID // used in struct literals for clarity; not part of the interface
	results []quota.Result
	err     error
	called  bool
}

func (m *mockProvider) Fetch(_ context.Context, _ time.Time) ([]quota.Result, error) {
	m.called = true
	return m.results, m.err
}

// mockCache implements Cache.
type mockCache struct {
	data    map[string][]quota.Result
	putErr  error
	getErr  error
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

func (c *mockCache) Put(_ context.Context, id string, results []quota.Result) error {
	if c.putErr != nil {
		return c.putErr
	}
	c.data[id] = results
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
