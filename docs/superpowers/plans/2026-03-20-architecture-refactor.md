# cq Architecture Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor cq from monolithic-file structure to Functional Core / Imperative Shell architecture with full test coverage.

**Architecture:** Pure domain types in `quota/`, pure computation in `aggregate/`, provider sub-packages with injected HTTP clients, unified `Report` read model in `app/`, structured TTY rendering via lipgloss, kong CLI.

**Tech Stack:** Go 1.26, kong (CLI), lipgloss (terminal styling), golang.org/x/sync (errgroup)

**Spec:** `docs/superpowers/specs/2026-03-20-architecture-refactor-design.md`

---

## Phase 1: Foundation

### Task 1: Create `quota/` package — domain types + constants

**Files:**
- Create: `internal/quota/constants.go`
- Create: `internal/quota/result.go`
- Create: `internal/quota/time.go`
- Test: `internal/quota/result_test.go`
- Test: `internal/quota/time_test.go`

- [ ] **Step 1: Write tests for Result helpers**

```go
// internal/quota/result_test.go
package quota

import "testing"

func TestResultIsUsable(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusOK, true},
		{StatusExhausted, true},
		{StatusError, false},
	}
	for _, tt := range tests {
		if got := (Result{Status: tt.status}).IsUsable(); got != tt.want {
			t.Errorf("IsUsable(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestResultMinRemainingPct(t *testing.T) {
	tests := []struct {
		name    string
		windows map[WindowName]Window
		want    int
	}{
		{"no windows", nil, 0},
		{"single", map[WindowName]Window{Window5Hour: {RemainingPct: 42}}, 42},
		{"min of two", map[WindowName]Window{
			Window5Hour: {RemainingPct: 80},
			Window7Day:  {RemainingPct: 30},
		}, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Result{Windows: tt.windows}
			if got := r.MinRemainingPct(); got != tt.want {
				t.Errorf("MinRemainingPct() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestErrorResult(t *testing.T) {
	r := ErrorResult("api_error", "server error", 500)
	if r.Status != StatusError {
		t.Fatalf("status = %q, want %q", r.Status, StatusError)
	}
	if r.Error == nil || r.Error.Code != "api_error" || r.Error.HTTPStatus != 500 {
		t.Fatalf("error = %+v, want code=api_error http=500", r.Error)
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL (package doesn't exist)**

Run: `go test ./internal/quota/ -v`
Expected: FAIL — package not found

- [ ] **Step 3: Implement `quota/constants.go`**

```go
// internal/quota/constants.go
package quota

import "time"

type Status string

const (
	StatusOK        Status = "ok"
	StatusExhausted Status = "exhausted"
	StatusError     Status = "error"
)

type WindowName string

const (
	Window5Hour WindowName = "5h"
	Window7Day  WindowName = "7d"
	WindowQuota WindowName = "quota"
)

func PeriodFor(name WindowName) time.Duration {
	switch name {
	case Window5Hour:
		return 5 * time.Hour
	case Window7Day:
		return 7 * 24 * time.Hour
	case WindowQuota:
		return 24 * time.Hour
	default:
		return 0
	}
}

var OrderedWindows = []WindowName{Window5Hour, Window7Day, WindowQuota}
```

- [ ] **Step 4: Implement `quota/result.go`**

```go
// internal/quota/result.go
package quota

type ErrorInfo struct {
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type Window struct {
	RemainingPct int   `json:"remaining_pct"`
	ResetAtUnix  int64 `json:"reset_at_unix,omitempty"`
}

type Result struct {
	AccountID     string                `json:"account_id,omitempty"`
	Email         string                `json:"email,omitempty"`
	Status        Status                `json:"status"`
	Error         *ErrorInfo            `json:"error,omitempty"`
	Plan          string                `json:"plan,omitempty"`
	Tier          string                `json:"tier,omitempty"`
	RateLimitTier string                `json:"rate_limit_tier,omitempty"`
	Windows       map[WindowName]Window `json:"windows,omitempty"`
}

func (r Result) IsUsable() bool {
	return r.Status == StatusOK || r.Status == StatusExhausted
}

func (r Result) MinRemainingPct() int {
	if len(r.Windows) == 0 {
		return 0
	}
	min := 100
	for _, w := range r.Windows {
		if w.RemainingPct < min {
			min = w.RemainingPct
		}
	}
	return min
}

func ErrorResult(code, msg string, httpStatus int) Result {
	return Result{
		Status: StatusError,
		Error: &ErrorInfo{
			Code:       code,
			Message:    msg,
			HTTPStatus: httpStatus,
		},
	}
}
```

- [ ] **Step 5: Run tests — expect PASS**

Run: `go test ./internal/quota/ -v`
Expected: PASS

- [ ] **Step 6: Write tests for time helpers**

```go
// internal/quota/time_test.go
package quota

import "testing"

func TestParseResetTime(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"2026-03-19T12:00:00Z", 1774051200},
		{"2026-03-19T12:00:00.123456Z", 1774051200},
		{"2026-03-19T12:00:00+00:00", 1774051200},
	}
	for _, tt := range tests {
		if got := ParseResetTime(tt.input); got != tt.want {
			t.Errorf("ParseResetTime(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestCleanResetTime(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"", ""},
		{"2026-03-19T12:00:00Z", "2026-03-19T12:00:00Z"},
		{"2026-03-19T12:00:00.123456Z", "2026-03-19T12:00:00Z"},
		{"2026-03-19T12:00:00+00:00", "2026-03-19T12:00:00Z"},
	}
	for _, tt := range tests {
		if got := CleanResetTime(tt.input); got != tt.want {
			t.Errorf("CleanResetTime(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEpochToISO(t *testing.T) {
	if got := EpochToISO(0); got != "" {
		t.Errorf("EpochToISO(0) = %q, want empty", got)
	}
	if got := EpochToISO(1774051200); got != "2026-03-19T12:00:00Z" {
		t.Errorf("EpochToISO(1774051200) = %q, want 2026-03-19T12:00:00Z", got)
	}
}
```

- [ ] **Step 7: Implement `quota/time.go`**

Move `parseResetTime`, `cleanResetTime` from `internal/provider/claude.go` and `epochToISO` from `internal/provider/codex.go` into `internal/quota/time.go`. Export them as `ParseResetTime`, `CleanResetTime`, `EpochToISO`.

- [ ] **Step 8: Run all quota tests — expect PASS**

Run: `go test ./internal/quota/... -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/quota/
git commit -m "feat: create quota package with domain types, constants, time helpers"
```

---

### Task 2: Create `aggregate/` types + migrate compute logic

**Files:**
- Create: `internal/aggregate/types.go` (new)
- Modify: `internal/aggregate/aggregate.go` → refactor to use `quota.Result` instead of `provider.Result`
- Modify: `internal/aggregate/aggregate_test.go` → update imports
- Create: `internal/aggregate/sustain.go` (extract from aggregate.go)
- Create: `internal/aggregate/label.go` (extract from aggregate.go)

- [ ] **Step 1: Create `aggregate/types.go` with new types**

```go
// internal/aggregate/types.go
package aggregate

type AggregateResult struct {
	RemainingPct   int     `json:"remaining_pct"`
	ExpectedPct    int     `json:"expected_pct"`
	PaceDiff       int     `json:"pace_diff"`
	Burndown       int64   `json:"burndown_s,omitempty"`
	Sustainability float64 `json:"sustainability,omitempty"`
}

type AccountSummary struct {
	Count      int    `json:"count"`
	TotalMulti int    `json:"total_multi"`
	Label      string `json:"label"`
}
```

Note: `BurndownFmt` is intentionally removed — verify no callers: `grep -r BurndownFmt internal/`

- [ ] **Step 2: Migrate aggregate.go to use quota types**

Update `internal/aggregate/aggregate.go`:
- Change import from `provider` to `quota`
- Replace `provider.Result` → `quota.Result`
- Replace `provider.Window` → `quota.Window`
- Replace string window names → `quota.WindowName`
- Replace `int64` periods → calls to `quota.PeriodFor()`
- Replace `w.ResetsEpoch` → `w.ResetAtUnix`
- Use `result.IsUsable()` instead of checking `r.Status == "ok" || r.Status == "exhausted"`
- Update `acctInfo` to hold `quota.Result`
- Remove old `AggregateResult` and `AccountSummary` types (now in types.go)

- [ ] **Step 3: Extract sustainability logic to `aggregate/sustain.go`**

Move from `aggregate.go` to `sustain.go`:
- `sustainAccount` struct
- `interval` struct
- `computeSustainability` function
- `coversPeriod` function
- `clampFloat` helper

- [ ] **Step 4: Extract label logic to `aggregate/label.go`**

Move `BuildLabel` and its `planGroup` type from `aggregate.go` to `label.go`.

- [ ] **Step 5: Update tests to use quota types**

Update `internal/aggregate/aggregate_test.go`:
- Change import from `provider` to `quota`
- Replace `provider.Result` → `quota.Result`
- Replace `provider.Window` → `quota.Window`
- Replace string keys `"5h"` → `quota.Window5Hour`
- Replace `ResetsEpoch` → `ResetAtUnix`

- [ ] **Step 6: Run tests — expect PASS**

Run: `go test ./internal/aggregate/... -v`
Expected: All existing tests PASS with new types

- [ ] **Step 7: Compile check — old code still builds**

Run: `go build ./...`
Expected: Old code in `cmd/cq/` and `internal/provider/` still compiles (they still use `provider.Result`)

- [ ] **Step 8: Commit**

```bash
git add internal/aggregate/
git commit -m "refactor: migrate aggregate to quota types, split into types/sustain/label"
```

---

### Task 3: Create `provider/` interfaces + ID type

**Files:**
- Create: `internal/provider/interfaces.go`
- Modify: `internal/provider/provider.go` — keep old types temporarily for backward compat

- [ ] **Step 1: Create `provider/interfaces.go`**

```go
// internal/provider/interfaces.go
package provider

import (
	"context"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

type ID string

const (
	Claude ID = "claude"
	Codex  ID = "codex"
	Gemini ID = "gemini"
)

type NewProvider interface {
	ID() ID
	Fetch(ctx context.Context, now time.Time) ([]quota.Result, error)
}

type LoginOptions struct {
	Activate bool
}

type LoginResult struct {
	Account   Account `json:"account"`
	Activated bool    `json:"activated"`
}

type Account struct {
	AccountID string `json:"id"`
	Email     string `json:"email,omitempty"`
	Label     string `json:"label,omitempty"`
	Active    bool   `json:"active"`
	SwitchID  string `json:"switch_id,omitempty"`
}

type Authenticator interface {
	ProviderID() ID
	Login(ctx context.Context, opts LoginOptions) (LoginResult, error)
}

type AccountManager interface {
	ProviderID() ID
	Discover(ctx context.Context) ([]Account, error)
	Switch(ctx context.Context, identifier string) (Account, error)
}

type Services struct {
	Usage    NewProvider
	Auth     Authenticator
	Accounts AccountManager
}
```

Note: Named `NewProvider` temporarily to avoid conflicting with existing `Provider` interface. Will rename to `Provider` and delete old interface in Task 18 (cleanup).

- [ ] **Step 2: Run compile check**

Run: `go build ./...`
Expected: PASS — new interfaces coexist with old code

- [ ] **Step 3: Commit**

```bash
git add internal/provider/interfaces.go
git commit -m "feat: add new provider interfaces with context and quota types"
```

---

### Task 4: Create `httputil/` package

**Files:**
- Create: `internal/httputil/client.go`
- Test: `internal/httputil/client_test.go`

- [ ] **Step 1: Write test**

```go
// internal/httputil/client_test.go
package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClientTimeout(t *testing.T) {
	c := NewClient(50 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	_, err := c.Do(req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestNewClientSetsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	c := NewClient(5 * time.Second)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	c.Do(req)
	if gotUA != "cq/1.0" {
		t.Errorf("User-Agent = %q, want cq/1.0", gotUA)
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `go test ./internal/httputil/ -v`
Expected: FAIL

- [ ] **Step 3: Implement**

```go
// internal/httputil/client.go
package httputil

import (
	"net/http"
	"time"
)

type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

type uaTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}

func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &uaTransport{base: http.DefaultTransport, ua: "cq/1.0"},
	}
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/httputil/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/httputil/
git commit -m "feat: create httputil package with Doer interface and client constructor"
```

---

## Phase 2: App Layer

### Task 5: Create `app/` report types + buildReport

**Files:**
- Create: `internal/app/report.go`
- Test: `internal/app/report_test.go`

- [ ] **Step 1: Write test for buildReport**

```go
// internal/app/report_test.go
package app

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestBuildReportSingleProvider(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Codex: {{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
		}}},
	}
	r := buildReport(now, []provider.ID{provider.Codex}, fetched)
	if len(r.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(r.Providers))
	}
	if r.Providers[0].Aggregate != nil {
		t.Fatal("single-account provider should not have aggregate")
	}
}

func TestBuildReportClaudeAggregate(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 1000 + 9000},
				}},
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
				}},
		},
	}
	r := buildReport(now, []provider.ID{provider.Claude}, fetched)
	if r.Providers[0].Aggregate == nil {
		t.Fatal("Claude with 2 accounts should have aggregate")
	}
	if len(r.Providers[0].Aggregate.Windows) == 0 {
		t.Fatal("aggregate should have windows")
	}
}

func TestBuildReportPreservesOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	order := []provider.ID{provider.Gemini, provider.Claude, provider.Codex}
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {{Status: quota.StatusOK}},
		provider.Codex:  {{Status: quota.StatusOK}},
		provider.Gemini: {{Status: quota.StatusOK}},
	}
	r := buildReport(now, order, fetched)
	for i, id := range order {
		if r.Providers[i].ID != id {
			t.Errorf("provider[%d] = %q, want %q", i, r.Providers[i].ID, id)
		}
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `go test ./internal/app/ -v`
Expected: FAIL

- [ ] **Step 3: Implement report.go**

```go
// internal/app/report.go
package app

import (
	"context"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type Clock interface {
	Now() time.Time
}

type Cache interface {
	Get(ctx context.Context, id provider.ID) ([]quota.Result, bool, error)
	Put(ctx context.Context, id provider.ID, results []quota.Result) error
}

type Renderer interface {
	Render(ctx context.Context, report Report) error
}

type RunRequest struct {
	Providers []provider.ID
	Refresh   bool
}

type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Providers   []ProviderReport `json:"providers"`
}

type ProviderReport struct {
	ID        provider.ID      `json:"id"`
	Name      string           `json:"name"`
	Results   []quota.Result   `json:"results"`
	Aggregate *AggregateReport `json:"aggregate,omitempty"`
}

type AggregateReport struct {
	Kind    string                                          `json:"kind"`
	Summary aggregate.AccountSummary                        `json:"summary"`
	Windows map[quota.WindowName]aggregate.AggregateResult  `json:"windows"`
}

func buildReport(now time.Time, ordered []provider.ID, fetched map[provider.ID][]quota.Result) Report {
	report := Report{
		GeneratedAt: now,
		Providers:   make([]ProviderReport, 0, len(ordered)),
	}
	for _, id := range ordered {
		results := fetched[id]
		pr := ProviderReport{
			ID:      id,
			Name:    string(id),
			Results: results,
		}
		if id == provider.Claude {
			if windows, summary := aggregate.Compute(results, now); len(windows) > 0 {
				pr.Aggregate = &AggregateReport{
					Kind:    "weighted_pace",
					Summary: summary,
					Windows: windows,
				}
			}
		}
		report.Providers = append(report.Providers, pr)
	}
	return report
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/app/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/
git commit -m "feat: create app package with Report types and buildReport"
```

---

### Task 6: Implement `app/runner.go`

**Files:**
- Create: `internal/app/runner.go`
- Test: `internal/app/runner_test.go`

- [ ] **Step 1: Write runner test with mocks**

```go
// internal/app/runner_test.go
package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

type mockProvider struct {
	id      provider.ID
	results []quota.Result
	err     error
}

func (m *mockProvider) ID() provider.ID { return m.id }
func (m *mockProvider) Fetch(_ context.Context, _ time.Time) ([]quota.Result, error) {
	return m.results, m.err
}

type mockCache struct {
	data map[provider.ID][]quota.Result
}

func (c *mockCache) Get(_ context.Context, id provider.ID) ([]quota.Result, bool, error) {
	if r, ok := c.data[id]; ok {
		return r, true, nil
	}
	return nil, false, nil
}

func (c *mockCache) Put(_ context.Context, id provider.ID, results []quota.Result) error {
	c.data[id] = results
	return nil
}

type captureRenderer struct {
	report Report
}

func (r *captureRenderer) Render(_ context.Context, report Report) error {
	r.report = report
	return nil
}

func TestRunnerRun(t *testing.T) {
	now := time.Unix(1000, 0)
	renderer := &captureRenderer{}
	runner := &Runner{
		Clock:    fixedClock(now),
		Cache:    &mockCache{data: make(map[provider.ID][]quota.Result)},
		Renderer: renderer,
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: &mockProvider{
				id:      provider.Codex,
				results: []quota.Result{{Status: quota.StatusOK}},
			}},
		},
	}
	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(renderer.report.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(renderer.report.Providers))
	}
}

func TestRunnerFetchErrorBecomesErrorResult(t *testing.T) {
	now := time.Unix(1000, 0)
	renderer := &captureRenderer{}
	runner := &Runner{
		Clock:    fixedClock(now),
		Renderer: renderer,
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: &mockProvider{
				id:  provider.Codex,
				err: fmt.Errorf("network timeout"),
			}},
		},
	}
	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	results := renderer.report.Providers[0].Results
	if len(results) != 1 || results[0].Status != quota.StatusError {
		t.Fatalf("expected error result, got %+v", results)
	}
}

func TestRunnerUnknownProviderErrors(t *testing.T) {
	runner := &Runner{
		Clock:    fixedClock(time.Unix(1000, 0)),
		Renderer: &captureRenderer{},
		Services: map[provider.ID]provider.Services{},
	}
	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{"unknown"}})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestRunnerUsesCache(t *testing.T) {
	now := time.Unix(1000, 0)
	cached := []quota.Result{{Status: quota.StatusOK, Plan: "cached"}}
	renderer := &captureRenderer{}
	runner := &Runner{
		Clock:    fixedClock(now),
		Cache:    &mockCache{data: map[provider.ID][]quota.Result{provider.Codex: cached}},
		Renderer: renderer,
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: &mockProvider{id: provider.Codex}},
		},
	}
	err := runner.Run(context.Background(), RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if renderer.report.Providers[0].Results[0].Plan != "cached" {
		t.Fatal("expected cached result")
	}
}

func TestRunnerBypassesCacheOnRefresh(t *testing.T) {
	now := time.Unix(1000, 0)
	renderer := &captureRenderer{}
	runner := &Runner{
		Clock: fixedClock(now),
		Cache: &mockCache{data: map[provider.ID][]quota.Result{
			provider.Codex: {{Status: quota.StatusOK, Plan: "stale"}},
		}},
		Renderer: renderer,
		Services: map[provider.ID]provider.Services{
			provider.Codex: {Usage: &mockProvider{
				id:      provider.Codex,
				results: []quota.Result{{Status: quota.StatusOK, Plan: "fresh"}},
			}},
		},
	}
	err := runner.Run(context.Background(), RunRequest{
		Providers: []provider.ID{provider.Codex},
		Refresh:   true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if renderer.report.Providers[0].Results[0].Plan != "fresh" {
		t.Fatal("expected fresh result when refresh=true")
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `go test ./internal/app/ -v -run TestRunner`
Expected: FAIL

- [ ] **Step 3: Implement runner.go**

```go
// internal/app/runner.go
package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type Runner struct {
	Clock    Clock
	Cache    Cache
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
	fetched := make(map[provider.ID][]quota.Result, len(req.Providers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, id := range req.Providers {
		svc, ok := r.Services[id]
		if !ok || svc.Usage == nil {
			return Report{}, fmt.Errorf("unknown provider: %s", id)
		}
		wg.Add(1)
		go func(id provider.ID, p provider.NewProvider) {
			defer wg.Done()
			results := r.fetchOne(ctx, now, req.Refresh, id, p)
			mu.Lock()
			fetched[id] = results
			mu.Unlock()
		}(id, svc.Usage)
	}
	wg.Wait()

	return buildReport(now, req.Providers, fetched), nil
}

func (r *Runner) fetchOne(ctx context.Context, now interface{ /* unused for now */ }, refresh bool, id provider.ID, p provider.NewProvider) []quota.Result {
	if !refresh && r.Cache != nil {
		if cached, ok, err := r.Cache.Get(ctx, id); err == nil && ok {
			return cached
		}
	}

	results, err := p.Fetch(ctx, r.Clock.Now())
	if err != nil {
		return []quota.Result{quota.ErrorResult("fetch_failed", err.Error(), 0)}
	}
	if len(results) == 0 {
		return []quota.Result{quota.ErrorResult("empty_result", "provider returned no results", 0)}
	}

	if r.Cache != nil {
		_ = r.Cache.Put(ctx, id, results)
	}
	return results
}
```

Note: Uses `sync.WaitGroup` initially; will upgrade to `errgroup` when `golang.org/x/sync` is added in Task 18.

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/app/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/
git commit -m "feat: create app Runner with concurrent fetch, cache, and report building"
```

---

## Phase 3: Output Layer

### Task 7: Add dependencies + create `output/` JSON renderer

**Files:**
- Modify: `go.mod` — add kong, lipgloss, x/sync
- Create: `internal/output/json.go`
- Test: `internal/output/json_test.go`

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/alecthomas/kong
go get github.com/charmbracelet/lipgloss
go get golang.org/x/sync
```

- [ ] **Step 2: Write JSON renderer test**

```go
// internal/output/json_test.go
package output

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestJSONRendererCompact(t *testing.T) {
	var buf bytes.Buffer
	r := &JSONRenderer{W: &buf}
	report := app.Report{
		GeneratedAt: time.Unix(1000, 0),
		Providers: []app.ProviderReport{
			{ID: provider.Codex, Name: "codex", Results: []quota.Result{
				{Status: quota.StatusOK},
			}},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestJSONRendererPretty(t *testing.T) {
	var buf bytes.Buffer
	r := &JSONRenderer{W: &buf, Pretty: true}
	report := app.Report{
		GeneratedAt: time.Unix(1000, 0),
		Providers:   []app.ProviderReport{},
	}
	r.Render(context.Background(), report)
	if !bytes.Contains(buf.Bytes(), []byte("\n")) {
		t.Fatal("expected pretty-printed output with newlines")
	}
}
```

- [ ] **Step 3: Implement JSON renderer**

```go
// internal/output/json.go
package output

import (
	"context"
	"encoding/json"
	"io"

	"github.com/jacobcxdev/cq/internal/app"
)

type JSONRenderer struct {
	W        io.Writer
	Pretty   bool
	Colorise bool
}

func (r *JSONRenderer) Render(_ context.Context, report app.Report) error {
	if r.Pretty {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if r.Colorise {
			data = coloriseJSON(data)
		}
		_, err = r.W.Write(data)
		if err != nil {
			return err
		}
		_, err = r.W.Write([]byte("\n"))
		return err
	}
	return json.NewEncoder(r.W).Encode(report)
}
```

Move `colorizeJSON` from `cmd/cq/main.go` into `internal/output/json.go` as `coloriseJSON` (unexported).

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/output/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/output/ go.mod go.sum
git commit -m "feat: create output package with JSON renderer"
```

---

### Task 8: Create `output/` TTY model + style + format

**Files:**
- Create: `internal/output/tty_model.go`
- Create: `internal/output/tty_build.go`
- Create: `internal/output/tty_style.go`
- Create: `internal/output/tty_format.go`
- Create: `internal/output/tty_bar.go`
- Create: `internal/output/tty_gauge.go`
- Test: `internal/output/tty_build_test.go`
- Test: `internal/output/tty_format_test.go`

This task extracts the pure display logic from `internal/display/display.go` into the new output package with lipgloss styles. The TTY model types represent what to render; the build function converts a Report into the model; the renderer writes the model to an io.Writer.

- [ ] **Step 1: Write tests for format helpers**

Port `fmtDuration` tests, `pctColor` behaviour tests, `calcPace`, `calcBurndown` tests.

- [ ] **Step 2: Implement `tty_format.go`**

Move `fmtDuration`, `calcPace`, `calcBurndown`, `periodSeconds` (now using `quota.PeriodFor`) from `internal/display/display.go`.

- [ ] **Step 3: Write tests for bar rendering**

Test `renderBar` as a pure function returning a string (not writing to stdout).

- [ ] **Step 4: Implement `tty_style.go`, `tty_bar.go`, `tty_gauge.go`**

- `tty_style.go`: Define lipgloss styles for pct colours, diff colours, sustain colours, dim, bold, etc.
- `tty_bar.go`: Port `printBar` from display.go → pure function returning styled string
- `tty_gauge.go`: Port `renderSustainGauge`, `sustainGaugePos` → pure functions

- [ ] **Step 5: Define TTY model types**

```go
// internal/output/tty_model.go
package output

type TTYModel struct {
	Sections []TTYSection
}

type TTYSection struct {
	ProviderID   string
	Separator    string
	Header       string
	WindowRows   []TTYWindowRow
	AggHeader    string
	AggRows      []TTYWindowRow
	ThinSep      string
}

type TTYWindowRow struct {
	Label      string
	Bar        string
	Pct        string
	ResetTime  string
	PaceDiff   string
	Burndown   string
}
```

- [ ] **Step 6: Write build test — Report to TTYModel**

Test that `BuildTTYModel` correctly converts a Report with known values into expected TTYModel sections.

- [ ] **Step 7: Implement `tty_build.go`**

Pure function `BuildTTYModel(report app.Report, now time.Time) TTYModel` — converts each ProviderReport into a TTYSection. Uses format helpers and style functions.

- [ ] **Step 8: Run all output tests — expect PASS**

Run: `go test ./internal/output/... -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/output/
git commit -m "feat: create TTY model, styles, format helpers, bar and gauge rendering"
```

---

### Task 9: Create `output/` TTY renderer

**Files:**
- Create: `internal/output/tty_renderer.go`
- Test: `internal/output/tty_renderer_test.go`

- [ ] **Step 1: Write golden file test**

Render a known Report to a `bytes.Buffer` and compare with expected output. Use `testdata/` golden files.

- [ ] **Step 2: Implement TTY renderer**

```go
// internal/output/tty_renderer.go
package output

import (
	"context"
	"io"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
)

type TTYRenderer struct {
	W   io.Writer
	Now time.Time
}

func (r *TTYRenderer) Render(_ context.Context, report app.Report) error {
	model := BuildTTYModel(report, r.Now)
	return renderTTY(r.W, model)
}

func renderTTY(w io.Writer, model TTYModel) error {
	for _, section := range model.Sections {
		io.WriteString(w, section.Separator+"\n\n")
		io.WriteString(w, section.Header+"\n")
		for _, row := range section.WindowRows {
			io.WriteString(w, "       "+row.Label+"  "+row.Bar+"  "+row.Pct+"  "+row.ResetTime+"  "+row.PaceDiff+"  "+row.Burndown+"\n")
		}
		if section.AggHeader != "" {
			io.WriteString(w, "\n"+section.ThinSep+"\n\n")
			io.WriteString(w, section.AggHeader+"\n")
			for _, row := range section.AggRows {
				io.WriteString(w, "       "+row.Label+"  "+row.Bar+"  "+row.Pct+"  "+row.ResetTime+"  "+row.PaceDiff+"  "+row.Burndown+"\n")
			}
		}
		io.WriteString(w, "\n")
	}
	return nil
}
```

Note: Exact layout will be refined to match current output. Use golden tests to lock in format.

- [ ] **Step 3: Run — expect PASS**

Run: `go test ./internal/output/... -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/output/
git commit -m "feat: create TTY renderer with golden file tests"
```

---

## Phase 4: Provider Extraction

### Task 10: Extract `provider/claude/` — parser (pure functions)

**Files:**
- Create: `internal/provider/claude/parser.go`
- Test: `internal/provider/claude/parser_test.go`

- [ ] **Step 1: Write parser tests with fixture JSON**

Test `parseClaudeUsage` and `parseClaudeProfile` with canned API response JSON. Test `dedup` with various account combinations.

- [ ] **Step 2: Move pure parsing functions from `internal/provider/claude.go`**

Move to `internal/provider/claude/parser.go`:
- `parseClaudeUsage` → uses `quota.ParseResetTime`, `quota.CleanResetTime`, returns `quota.Result`
- `parseClaudeProfile` (the struct + normalisation logic from `fetchClaudeProfile`)
- `dedup` function

- [ ] **Step 3: Run — expect PASS**

Run: `go test ./internal/provider/claude/ -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/provider/claude/
git commit -m "feat: extract Claude parser as pure functions with quota types"
```

---

### Task 11: Extract `provider/claude/` — client + refresh

**Files:**
- Create: `internal/provider/claude/client.go`
- Create: `internal/provider/claude/refresh.go`
- Test: `internal/provider/claude/client_test.go`

- [ ] **Step 1: Write client test with httptest**

Test `fetchUsage` and `fetchProfile` against `httptest.Server` with canned responses.

- [ ] **Step 2: Implement client.go**

Move `fetchClaudeUsage`, `fetchClaudeProfile` from `internal/provider/claude.go`. Inject `httputil.Doer` instead of creating `http.Client` inline.

- [ ] **Step 3: Implement refresh.go**

Move `refreshClaudeToken` from `internal/provider/claude.go`. Inject `httputil.Doer`.

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/provider/claude/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/provider/claude/
git commit -m "feat: extract Claude HTTP client and token refresh with injected Doer"
```

---

### Task 12: Extract `provider/claude/` — provider, accounts, credentials

**Files:**
- Create: `internal/provider/claude/provider.go`
- Create: `internal/provider/claude/accounts.go`
- Create: `internal/provider/claude/credentials.go`

- [ ] **Step 1: Create credentials.go**

Move `ClaudeOAuth`, `ClaudeCredentials`, `TokenAccount` types from `internal/keyring/keyring.go`. Move credential persistence functions (`BackfillCredentialsFile`, `PersistRefreshedToken`, `WriteCredentialsFile`).

- [ ] **Step 2: Create accounts.go**

Implement `provider.AccountManager` for Claude. Move account discovery orchestration from `internal/keyring/keyring.go` (`DiscoverClaudeAccounts`), merging logic (`mergeAnonymousFresh`, `dedupByEmail`), and wire to keyring package for storage.

- [ ] **Step 3: Create provider.go**

Implement `provider.NewProvider` for Claude. Wire together client, parser, refresh, accounts. The `Fetch` method orchestrates: discover accounts → for each account: check token expiry → refresh if needed → fetch profile + usage in parallel → parse → dedup.

- [ ] **Step 4: Run compile check + tests**

Run: `go test ./internal/provider/claude/... -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/provider/claude/
git commit -m "feat: complete Claude provider with accounts, credentials, provider wiring"
```

---

### Task 13: Extract `provider/codex/`

**Files:**
- Create: `internal/provider/codex/provider.go`
- Create: `internal/provider/codex/parser.go`
- Create: `internal/provider/codex/refresh.go`
- Test: `internal/provider/codex/parser_test.go`

- [ ] **Step 1: Write parser tests**

Test `parseCodexUsage` with canned JSON.

- [ ] **Step 2: Move code from `internal/provider/codex.go`**

- `parser.go`: `parseCodexUsage`, `parseNumericResetAt` (uses `quota.ParseResetTime`)
- `refresh.go`: `refreshCodexToken`, `updateCodexAuthFile`
- `provider.go`: `Codex` struct implementing `provider.NewProvider`

Inject `httputil.Doer`.

- [ ] **Step 3: Run — expect PASS**

Run: `go test ./internal/provider/codex/... -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/provider/codex/
git commit -m "feat: extract Codex provider into sub-package"
```

---

### Task 14: Extract `provider/gemini/`

**Files:**
- Create: `internal/provider/gemini/provider.go`
- Create: `internal/provider/gemini/parser.go`
- Create: `internal/provider/gemini/refresh.go`
- Create: `internal/provider/gemini/creds.go`
- Test: `internal/provider/gemini/parser_test.go`

- [ ] **Step 1: Write parser tests**

Test `parseGeminiQuota`, `parseGeminiTier` with canned JSON.

- [ ] **Step 2: Move code from `internal/provider/gemini.go`**

- `parser.go`: `parseGeminiQuota`, `parseGeminiTier`
- `refresh.go`: `refreshGeminiToken`
- `creds.go`: `getGeminiOAuthCreds` + `extractPattern`, wrapped with `sync.Once` caching. Check `GEMINI_CLIENT_ID`/`GEMINI_CLIENT_SECRET` env vars first.
- `provider.go`: `Gemini` struct implementing `provider.NewProvider`

- [ ] **Step 3: Run — expect PASS**

Run: `go test ./internal/provider/gemini/... -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/provider/gemini/
git commit -m "feat: extract Gemini provider with sync.Once credential caching"
```

---

## Phase 5: Auth + Keyring

### Task 15: Refactor `auth/` package

**Files:**
- Modify: `internal/auth/oauth.go` — keep as concrete helper (already generic PKCE)
- Modify: `internal/auth/jwt.go` — no changes needed
- Create: `internal/auth/config.go`
- Create: `internal/auth/browser.go` (extract from oauth.go)

- [ ] **Step 1: Extract browser helpers**

Move `openBrowser`, `openBrowserDarwin`, `openBrowserLinux`, `openBrowserWindows`, `defaultBrowserBundleID`, `parseRegValue`, `parseBrowserPath` from `oauth.go` into `browser.go`.

- [ ] **Step 2: Create config.go**

```go
// internal/auth/config.go
package auth

type OAuthConfig struct {
	ClientID     string
	AuthURL      string
	TokenURL     string
	RedirectURL  string
	Scopes       []string
}
```

- [ ] **Step 3: Run — compile check**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/auth/
git commit -m "refactor: split auth into oauth, browser, config, jwt"
```

---

### Task 16: Refactor `keyring/` package

**Files:**
- Modify: `internal/keyring/keyring.go` — remove Claude-specific types (moved to provider/claude/credentials.go in Task 12)
- Rename: `internal/keyring/keyring.go` → keep as thin adapter

- [ ] **Step 1: Remove Claude-specific types from keyring**

After Task 12 moved `ClaudeOAuth`, `ClaudeCredentials`, `TokenAccount` to `provider/claude/credentials.go`, remove them from `internal/keyring/keyring.go`. Update imports in all callers.

- [ ] **Step 2: Slim keyring to storage primitives**

`keyring/client.go` should expose only:
- `Get(service, user string) (string, error)`
- `Set(service, user, data string) error`
- `Delete(service, user string) error`

Platform files (`platform_darwin.go`, `platform_other.go`) provide `UpdateKeychainEntry` and `discoverPlatformKeychain`.

- [ ] **Step 3: Run — compile + test**

Run: `go test ./... && go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/keyring/
git commit -m "refactor: slim keyring to storage primitives, Claude types moved to provider"
```

---

## Phase 6: Cache + CLI

### Task 17: Refactor `cache/` with FileSystem interface

**Files:**
- Create: `internal/cache/fs.go`
- Modify: `internal/cache/cache.go` — implement `app.Cache`, inject `FileSystem`
- Test: `internal/cache/cache_test.go`

- [ ] **Step 1: Write cache tests with in-memory FileSystem**

```go
// internal/cache/cache_test.go
package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCacheGetMiss(t *testing.T) {
	c := New(NewMemFS(), t.TempDir(), 30*time.Second)
	_, ok, err := c.Get(context.Background(), provider.Codex)
	if err != nil || ok {
		t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
	}
}

func TestCachePutAndGet(t *testing.T) {
	c := New(NewMemFS(), t.TempDir(), 30*time.Second)
	ctx := context.Background()
	results := []quota.Result{{Status: quota.StatusOK, Plan: "test"}}
	c.Put(ctx, provider.Codex, results)
	got, ok, err := c.Get(ctx, provider.Codex)
	if !ok || err != nil {
		t.Fatalf("expected hit, got ok=%v err=%v", ok, err)
	}
	if got[0].Plan != "test" {
		t.Fatalf("plan = %q, want test", got[0].Plan)
	}
}

func TestCacheExpiry(t *testing.T) {
	fs := NewMemFS()
	c := New(fs, "/cache", 1*time.Millisecond)
	ctx := context.Background()
	c.Put(ctx, provider.Codex, []quota.Result{{Status: quota.StatusOK}})
	time.Sleep(5 * time.Millisecond)
	_, ok, _ := c.Get(ctx, provider.Codex)
	if ok {
		t.Fatal("expected expired cache miss")
	}
}
```

- [ ] **Step 2: Implement fs.go**

```go
// internal/cache/fs.go
package cache

import "os"

type FileSystem interface {
	Stat(name string) (os.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	MkdirAll(path string, perm os.FileMode) error
}

type OSFileSystem struct{}

func (OSFileSystem) Stat(name string) (os.FileInfo, error)                    { return os.Stat(name) }
func (OSFileSystem) ReadFile(name string) ([]byte, error)                     { return os.ReadFile(name) }
func (OSFileSystem) WriteFile(n string, d []byte, p os.FileMode) error        { return os.WriteFile(n, d, p) }
func (OSFileSystem) Rename(o, n string) error                                 { return os.Rename(o, n) }
func (OSFileSystem) MkdirAll(p string, perm os.FileMode) error                { return os.MkdirAll(p, perm) }
```

Also implement `MemFS` (in-memory implementation for tests).

- [ ] **Step 3: Rewrite cache.go to implement app.Cache**

Inject `FileSystem`, accept `dir` and `ttl` as constructor params. Use `provider.ID` as cache key. Implement `app.Cache` interface.

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/cache/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cache/
git commit -m "refactor: cache implements app.Cache with injected FileSystem"
```

---

### Task 18: Kong CLI + main.go rewrite

**Files:**
- Rewrite: `cmd/cq/main.go` — kong CLI, dependency wiring
- Delete: `cmd/cq/providers.go`
- Delete: `cmd/cq/subcmds.go`

- [ ] **Step 1: Write new main.go with kong**

```go
// cmd/cq/main.go
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/output"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	geminiprov "github.com/jacobcxdev/cq/internal/provider/gemini"
)

type CLI struct {
	JSON    bool `help:"Output JSON"`
	Refresh bool `help:"Bypass cache"`

	Check  CheckCmd  `cmd:"" default:"withargs" help:"Check quota"`
	Claude ClaudeCmd `cmd:"" help:"Claude account management"`
	Codex  CodexCmd  `cmd:"" help:"Codex account management"`
	Gemini GeminiCmd `cmd:"" help:"Gemini account management"`
}

type CheckCmd struct {
	Providers []string `arg:"" optional:"" enum:"claude,codex,gemini"`
}

type ClaudeCmd struct {
	Login    LoginCmd    `cmd:"" help:"Add Claude account"`
	Accounts AccountsCmd `cmd:"" help:"List Claude accounts"`
	Switch   SwitchCmd   `cmd:"" help:"Switch active Claude account"`
}

type CodexCmd struct {
	Accounts AccountsCmd `cmd:"" help:"Show Codex account"`
}

type GeminiCmd struct {
	Accounts AccountsCmd `cmd:"" help:"Show Gemini account"`
}

type LoginCmd struct {
	Activate bool `help:"Set as active account after login"`
}

type AccountsCmd struct{}

type SwitchCmd struct {
	Email string `arg:"" help:"Email of account to activate"`
}

// ... Run methods for each command, wiring to app.Runner and provider.Services
```

- [ ] **Step 2: Wire dependencies in composition root**

Build `provider.Services` map explicitly. Create `app.Runner` with clock, cache, services, renderer. Route kong commands to runner or auth/account operations.

- [ ] **Step 3: Delete old files**

```bash
rm cmd/cq/providers.go cmd/cq/subcmds.go
```

- [ ] **Step 4: Rename `provider.NewProvider` → `provider.Provider`**

Now that old code is removed, rename the interface back from `NewProvider` to `Provider` and update all references.

- [ ] **Step 5: Run — full build + test**

Run: `go build ./cmd/cq/ && go test ./... -v`
Expected: PASS

- [ ] **Step 6: Manual smoke test**

```bash
go run ./cmd/cq/
go run ./cmd/cq/ --json
go run ./cmd/cq/ claude
go run ./cmd/cq/ --refresh
```

Verify output matches previous behaviour.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat: rewrite main.go with kong CLI, delete old providers.go and subcmds.go"
```

---

## Phase 7: Cleanup + Coverage

### Task 19: Delete old code + verify

**Files:**
- Delete: `internal/provider/claude.go`
- Delete: `internal/provider/codex.go`
- Delete: `internal/provider/gemini.go`
- Delete: `internal/display/` (entire package — replaced by `output/`)
- Modify: `internal/provider/provider.go` — remove old `Result`, `Window`, `Provider` interface (now in quota/ and provider/interfaces.go)

- [ ] **Step 1: Delete old provider files**

```bash
rm internal/provider/claude.go internal/provider/codex.go internal/provider/gemini.go
rm -r internal/display/
```

- [ ] **Step 2: Clean up provider/provider.go**

Remove old `Result`, `Window`, `Provider` types. Keep only the new interfaces from `interfaces.go`, then merge `interfaces.go` into `provider.go` and delete `interfaces.go`.

- [ ] **Step 3: Full build + test**

```bash
go build ./... && go vet ./... && go test ./... -v
```

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor: delete old monolithic provider files and display package"
```

---

### Task 20: Comprehensive test coverage pass

**Files:**
- Multiple test files across all packages

- [ ] **Step 1: Check coverage**

```bash
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1
```

- [ ] **Step 2: Identify gaps**

```bash
go tool cover -func=coverage.out | grep -v "100.0%"
```

- [ ] **Step 3: Add missing tests**

Focus on:
- Parser edge cases (malformed JSON, missing fields)
- Aggregate boundary conditions (0 accounts, 1 account, all exhausted)
- Cache race conditions
- Runner with nil cache
- Output renderer edge cases (empty report, error-only results)

- [ ] **Step 4: Verify 80%+ overall coverage**

```bash
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1
```
Expected: `total: (statements) >= 80.0%`

- [ ] **Step 5: Final verification**

```bash
go build ./cmd/cq/ && go vet ./... && go test ./... -race
```

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "test: comprehensive test coverage pass (80%+ target)"
```

---

## Summary

| Phase | Tasks | Key Deliverable |
|-------|-------|----------------|
| 1: Foundation | 1-4 | `quota/`, `aggregate/types`, `provider/interfaces`, `httputil/` |
| 2: App Layer | 5-6 | `app/report.go`, `app/runner.go` with full tests |
| 3: Output | 7-9 | `output/` JSON + TTY renderers with lipgloss |
| 4: Providers | 10-14 | `provider/claude/`, `provider/codex/`, `provider/gemini/` |
| 5: Auth + Keyring | 15-16 | Slimmed `auth/`, `keyring/` packages |
| 6: Cache + CLI | 17-18 | `cache/` with FileSystem, kong CLI + main.go rewrite |
| 7: Cleanup | 19-20 | Delete old code, 80%+ test coverage |
