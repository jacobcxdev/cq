# cq Architecture Refactor Design

## Overview

Refactor `cq` (~2,200 lines, 16 files) from its current monolithic-file structure to a **Functional Core, Imperative Shell** architecture. The goal is a fundamentally sustainable, modular, testable, extensible, and composable codebase with high performance.

## Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Architecture | Functional Core, Imperative Shell | Pure computation core, thin I/O shell. TCA-inspired: logic is pure, effects at edges. |
| CLI framework | `kong` (struct-based) | Clean, lightweight, growing ecosystem. New dependency. |
| HTTP abstraction | `httputil.Doer` interface + domain interfaces | Two layers: transport-level mock + domain-level mock. |
| Display | Structured render model + `io.Writer` + `lipgloss` | Testable model, composable styles, no raw ANSI. New dependency (replaces hand-rolled ANSI). |
| Tests | Full suite, 80%+ coverage | Validates architecture as it's built. |
| Output | Unified `Report` read model for JSON and TTY | One place for "Claude aggregate if multi-account" rule. |
| Time normalisation | Epoch-only in domain types | `Window.ResetAtUnix int64` replaces dual `ResetsAt string` + `ResetsEpoch int64`. ISO strings derived at the output edge only. **Breaking change to `cq --json` output**: `resets_at` string field removed, replaced by `reset_at_unix` integer. |

## New Dependencies

| Package | Purpose | Replaces |
|---------|---------|----------|
| `github.com/alecthomas/kong` | CLI parsing | Hand-rolled `parseArgs` |
| `github.com/charmbracelet/lipgloss` | Terminal styling | Raw ANSI escape codes |
| `golang.org/x/sync` | `errgroup` for concurrent provider fetching | `sync.WaitGroup` |

## Package Structure

```
cmd/cq/
  main.go                — kong CLI struct, explicit dependency wiring

internal/
  app/
    runner.go            — orchestrates fetch -> aggregate -> render pipeline
    report.go            — unified Report read model (JSON + TTY)

  quota/
    result.go            — Result, Window, ErrorInfo (epoch-only time)
    constants.go         — Status, WindowName, PeriodFor(), OrderedWindows
    time.go              — ParseResetTime, CleanResetTime, EpochToISO (shared across providers)

  provider/
    provider.go          — Provider, Authenticator, AccountManager interfaces + ID type/constants
    claude/
      provider.go        — implements provider.Provider
      client.go          — HTTP client (usage + profile)
      parser.go          — pure: parseUsage, parseProfile, dedup
      refresh.go         — token refresh
      accounts.go        — implements provider.AccountManager
      credentials.go     — Claude credential types + persistence
    codex/
      provider.go        — implements provider.Provider
      parser.go          — pure parsing
      refresh.go         — token refresh + auth file update
    gemini/
      provider.go        — implements provider.Provider
      parser.go          — pure parsing
      refresh.go         — token refresh
      creds.go           — OAuth credential extraction (sync.Once cached, env var fallback)

  aggregate/
    types.go             — AggregateResult, AccountSummary
    compute.go           — Compute (pure: []Result, time.Time -> map[WindowName]AggregateResult, AccountSummary)
    sustain.go           — computeSustainability, coversPeriod, sustainAccount, interval types (pure math)
    label.go             — BuildLabel (pure)

  output/
    json.go              — JSONRenderer: Report -> JSON via io.Writer (terminal-aware: pretty-print + colorise if TTY)
    tty_model.go         — TTY display model types
    tty_build.go         — BuildTTYModel (pure: Report -> TTYModel)
    tty_renderer.go      — TTYRenderer: io.Writer + lipgloss
    tty_bar.go           — bar rendering (pure)
    tty_gauge.go         — sustainability gauge (pure)
    tty_style.go         — lipgloss style definitions
    tty_format.go        — fmtDuration, gauge formatting

  auth/
    oauth.go             — concrete PKCE/token exchange helpers
    jwt.go               — JWT email extraction (pure)
    config.go            — OAuthConfig struct
    browser.go           — browser-open helpers (darwin/linux/windows)

  keyring/
    client.go            — thin secret-storage adapter over go-keyring
    platform_darwin.go   — macOS security CLI
    platform_other.go    — no-op fallback

  cache/
    cache.go             — file-based TTL cache implementing app.Cache
    fs.go                — FileSystem interface + OS implementation

  httputil/
    client.go            — Doer interface, NewClient with timeout/user-agent/error wrapping
```

## Key Types

### Domain (`quota/`)

```go
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

func PeriodFor(name WindowName) time.Duration

var OrderedWindows = []WindowName{Window5Hour, Window7Day, WindowQuota}

type ErrorInfo struct {
    Code       string `json:"code"`            // e.g. "not_configured", "auth_expired", "api_error", "parse_error"
    Message    string `json:"message,omitempty"`
    HTTPStatus int    `json:"http_status,omitempty"`
}

type Window struct {
    RemainingPct int   `json:"remaining_pct"`
    ResetAtUnix  int64 `json:"reset_at_unix,omitempty"` // Unix seconds; 0 = unknown
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

func (r Result) MinRemainingPct() int // min across all windows, 0 if no windows

func ErrorResult(code, msg string, httpStatus int) Result // convenience constructor
```

### Shared Time Helpers (`quota/time.go`)

```go
// ParseResetTime parses an ISO 8601 / RFC 3339 string to Unix seconds.
// Used by Claude (string reset times) and Codex (string fallback).
func ParseResetTime(s string) int64

// CleanResetTime normalises fractional seconds and timezone suffixes.
func CleanResetTime(s string) string

// EpochToISO converts Unix seconds to an ISO 8601 string for output.
func EpochToISO(epoch int64) string
```

### Provider Contract (`provider/`)

```go
type ID string

const (
    Claude ID = "claude"
    Codex  ID = "codex"
    Gemini ID = "gemini"
)

type Provider interface {
    ID() ID
    // Fetch returns results for all accounts. May return non-nil error for
    // truly fatal issues (e.g. context cancelled). Recoverable failures
    // (auth expired, API error) should be encoded as quota.ErrorResult in
    // the returned slice with a nil error.
    Fetch(ctx context.Context, now time.Time) ([]quota.Result, error)
}

type LoginOptions struct {
    Activate bool // set as active account after login
}

type LoginResult struct {
    Account   Account `json:"account"`
    Activated bool    `json:"activated"`
}

type Account struct {
    ID       string `json:"id"`
    Email    string `json:"email,omitempty"`
    Label    string `json:"label,omitempty"`
    Active   bool   `json:"active"`
    SwitchID string `json:"switch_id,omitempty"` // identifier for Switch()
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

// Services bundles the capabilities a provider exposes.
// Auth and Accounts are nil for providers without managed auth.
type Services struct {
    Usage    Provider
    Auth     Authenticator
    Accounts AccountManager
}
```

### App Orchestration (`app/`)

```go
type Clock interface {
    Now() time.Time
}

type Cache interface {
    // Get returns cached results if fresh. Returns (nil, false, nil) on miss.
    Get(ctx context.Context, id provider.ID) ([]quota.Result, bool, error)
    // Put stores results. Errors are non-fatal (logged, not propagated).
    Put(ctx context.Context, id provider.ID, results []quota.Result) error
}

type Renderer interface {
    Render(ctx context.Context, report Report) error
}

type RunRequest struct {
    Providers []provider.ID
    Refresh   bool
}

type Runner struct {
    Clock    Clock
    Cache    Cache                              // nil = no caching
    Services map[provider.ID]provider.Services
    Renderer Renderer
}

func (r *Runner) Run(ctx context.Context, req RunRequest) error
// Run calls BuildReport then Renderer.Render.

func (r *Runner) BuildReport(ctx context.Context, req RunRequest) (Report, error)
// BuildReport fetches providers concurrently (errgroup), applies cache,
// then calls the pure BuildReport function below for assembly.
// Unknown provider ID -> orchestration error.
// Individual provider fetch failure -> quota.ErrorResult in results, not fatal.

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
    Kind    string                                        `json:"kind"` // "weighted_pace"
    Summary aggregate.AccountSummary                      `json:"summary"`
    Windows map[quota.WindowName]aggregate.AggregateResult `json:"windows"`
}

// buildReport is a pure function that assembles a Report from fetched results.
// Runner.BuildReport calls this after fetching. Exported for testing.
// Applies the aggregate rule: if provider is Claude and has 2+ usable results,
// compute aggregate. This rule lives here once, not duplicated in output paths.
func buildReport(now time.Time, ordered []provider.ID, fetched map[provider.ID][]quota.Result) Report
```

### Aggregate (`aggregate/`)

```go
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

// acctInfo pairs a result with its extracted multiplier for weighted computation.
type acctInfo struct {
    result     quota.Result
    multiplier int
}

// Compute calculates aggregate pace for multiple accounts across windows.
// Returns nil map and zero AccountSummary if fewer than 2 usable accounts.
// Callers check len(map) > 0 to determine whether aggregate is available.
func Compute(results []quota.Result, now time.Time) (map[quota.WindowName]AggregateResult, AccountSummary)
```

### Sustainability (`aggregate/sustain.go`)

```go
// sustainAccount holds per-account state for the binary search.
type sustainAccount struct {
    remaining float64 // current remaining percentage
    rate      float64 // consumption rate (used% / elapsed seconds)
    reset     float64 // seconds until reset from now (clamped to period)
}

// interval represents a time coverage interval.
type interval struct {
    start float64
    end   float64
}

// computeSustainability finds the maximum usage multiplier f such that
// the combined accounts can sustain coverage for the full period.
// Uses binary search over f with dry-gap simulation (post-reset intervals).
// Returns: f >= 0 (multiplier), -1 (insufficient data), 100 (infinite).
//
// NOTE: This is the target signature. The current code uses (string, int64, int64).
// Migration step 1 (domain types) introduces quota.WindowName and time.Duration;
// this function's callers in computeWindow are updated in step 4/5.
func computeSustainability(accounts []acctInfo, winName quota.WindowName, period time.Duration, now time.Time) float64

// coversPeriod checks whether a set of intervals fully covers [0, period].
func coversPeriod(intervals []interval, period float64) bool
```

## Cache FileSystem Interface (`cache/fs.go`)

```go
// FileSystem abstracts OS file operations for testability.
// Production uses OSFileSystem; tests use an in-memory implementation.
type FileSystem interface {
    Stat(name string) (os.FileInfo, error)
    ReadFile(name string) ([]byte, error)
    WriteFile(name string, data []byte, perm os.FileMode) error
    Rename(oldpath, newpath string) error
    MkdirAll(path string, perm os.FileMode) error
}

type OSFileSystem struct{}

func (OSFileSystem) Stat(name string) (os.FileInfo, error)           { return os.Stat(name) }
func (OSFileSystem) ReadFile(name string) ([]byte, error)            { return os.ReadFile(name) }
func (OSFileSystem) WriteFile(n string, d []byte, p os.FileMode) error { return os.WriteFile(n, d, p) }
func (OSFileSystem) Rename(o, n string) error                        { return os.Rename(o, n) }
func (OSFileSystem) MkdirAll(p string, perm os.FileMode) error       { return os.MkdirAll(p, perm) }
```

`cache.New(fs FileSystem, dir string, ttl time.Duration)` — `main.go` reads `CQ_TTL` and passes it in.

## Gemini Credential Extraction (`gemini/creds.go`)

The Gemini provider requires OAuth client credentials (`client_id`, `client_secret`) to refresh tokens. These are extracted from the Gemini CLI binary directory — a fragile heuristic.

**Design:**
- Use `sync.Once` for process-lifetime caching (credentials don't change during a single `cq` invocation)
- Check `GEMINI_CLIENT_ID` / `GEMINI_CLIENT_SECRET` env vars first (fast path, no binary scanning)
- Fall back to binary directory scanning only if env vars are absent
- If extraction fails, return `quota.ErrorResult("gemini_creds_missing", ...)` — do not panic or retry
- Cache key is irrelevant (sync.Once is per-process, not file-backed)

## Kong CLI Structure

```go
type CLI struct {
    JSON    bool `help:"Output JSON"`
    Refresh bool `help:"Bypass cache"`

    Check  CheckCmd   `cmd:"" default:"withargs" help:"Check quota"`
    Claude ClaudeCmd  `cmd:"" help:"Claude account management"`
    Codex  CodexCmd   `cmd:"" help:"Codex account management"`
    Gemini GeminiCmd  `cmd:"" help:"Gemini account management"`
}

type CheckCmd struct {
    Providers []string `arg:"" optional:"" enum:"claude,codex,gemini"`
}

// Claude has full auth support: login, accounts, switch.
type ClaudeCmd struct {
    Login    LoginCmd    `cmd:"" help:"Add Claude account"`
    Accounts AccountsCmd `cmd:"" help:"List Claude accounts"`
    Switch   SwitchCmd   `cmd:"" help:"Switch active Claude account"`
}

// Codex and Gemini: read-only account listing only (for now).
// Login and switch are not yet implemented; subcommands will be added
// when those providers gain managed auth support.
type CodexCmd struct {
    Accounts AccountsCmd `cmd:"" help:"Show Codex account"`
}

type GeminiCmd struct {
    Accounts AccountsCmd `cmd:"" help:"Show Gemini account"`
}
```

This gives:
- `cq [claude|codex|gemini]` — quota check (default)
- `cq claude login [--activate]` — add Claude account
- `cq claude accounts` — list Claude accounts
- `cq claude switch <email>` — switch active Claude account
- `cq codex accounts` — show Codex account info
- `cq gemini accounts` — show Gemini account info

```go
type LoginCmd struct {
    Activate bool `help:"Set as active account after login"`
}

type AccountsCmd struct{}

type SwitchCmd struct {
    Email string `arg:"" help:"Email of account to activate"`
}
```

Provider-specific cmd structs avoid dead subcommands in help output. When Codex or Gemini gain auth, their cmd structs grow — no restructuring needed.

## Error Handling Strategy

- `Provider.Fetch` returns `([]quota.Result, error)`. The `error` return is for truly fatal, unrecoverable issues (context cancelled, unknown provider). Recoverable failures (auth expired, API 500, parse error) are encoded as `quota.ErrorResult` values in the returned slice with a `nil` error.
- `Runner.BuildReport` wraps non-nil `Fetch` errors into `quota.ErrorResult` entries so the report always has data for every requested provider.
- `Cache.Put` errors are non-fatal — logged but not propagated.
- `Renderer.Render` errors are fatal — they propagate to `main` for exit code handling.

## Terminal Detection

`main.go` calls `isTerminal(os.Stdout)` and uses the result to select the renderer:
- TTY: `output.TTYRenderer`
- Non-TTY + `--json`: `output.JSONRenderer{Pretty: false}`
- TTY + `--json`: `output.JSONRenderer{Pretty: true, Colorise: true}`

Terminal detection is a `main.go` concern, not an output concern. Renderers receive their configuration at construction time.

## Data Flow

```
CLI args (kong)
  -> main.go: parse, wire deps, detect terminal, select renderer
  -> app.Runner.Run(ctx, RunRequest)
    -> for each provider (concurrent, errgroup):
        -> cache.Get(id) unless --refresh
        -> provider.Fetch(ctx, now)
          -> internal: account discovery -> token refresh -> HTTP -> pure parser
        -> cache.Put(id, results)
    -> app.BuildReport(now, orderedIDs, fetched)
      -> aggregate.Compute(results, now) for Claude if 2+ usable accounts
    -> app.Report
    -> renderer.Render(ctx, report)
      -> output.JSONRenderer or output.TTYRenderer
    -> io.Writer (stdout)
```

## Testing Strategy

| Layer | Technique | Target |
|-------|-----------|--------|
| Pure parsers (`*/parser.go`) | Table-driven with fixture JSON | 95%+ |
| Aggregate math (`compute.go`, `sustain.go`) | Table-driven (expand existing tests) | 95%+ |
| Shared time helpers (`quota/time.go`) | Table-driven | 95%+ |
| Display model building (`tty_build.go`) | Table-driven: Report -> TTYModel assertions | 90%+ |
| TTY renderer | Golden file: render to `bytes.Buffer`, compare | 80%+ |
| JSON renderer | Golden file: render to `bytes.Buffer`, compare | 80%+ |
| HTTP clients | `httptest.Server` with canned responses | 80%+ |
| Cache | Custom `FileSystem` interface; test read/write/expiry with in-memory impl | 80%+ |
| Runner orchestration | Mock `Provider` + `Cache` + `Renderer` interfaces | 80%+ |
| Credential merge/dedup | Table-driven (pure functions) | 90%+ |
| Auth/keyring | Integration with temp dirs + mock keyring | 70%+ |
| TTY display model types | Internal to `output/`; golden tests lock in rendered output, not model structure | N/A |

## Migration Sequence

Steps have explicit dependencies noted.

1. **Domain types + constants** — `quota/result.go`, `quota/constants.go`, `quota/time.go`, `aggregate/types.go`, `provider/provider.go` with `ID` type. No dependencies on other new packages.
2. **App layer** — `app/runner.go`, `app/report.go`. Depends on step 1 (`quota`, `aggregate/types`, `provider` interfaces).
3. **Output layer** — `output/` with JSON + TTY renderers using lipgloss. Depends on step 2 (`app.Report`). Add `lipgloss` to `go.mod`.
4. **Claude extraction** — split `claude.go` (382 lines) into `provider/claude/{provider,client,parser,refresh,accounts,credentials}.go`. Function placement: `parseClaudeUsage` + `parseClaudeProfile` + `dedup` -> `parser.go`; `fetchClaudeUsage` + `fetchClaudeProfile` -> `client.go`; `refreshClaudeToken` -> `refresh.go`; account discovery orchestration -> `accounts.go`; `ClaudeOAuth` + credential persistence -> `credentials.go`.
5. **Codex + Gemini extraction** — into sub-packages with 2-3 files each.
6. **Auth + keyring refactor** — keep generic helpers in `auth/` (OAuth PKCE, JWT decode used by all providers), move Claude-specific credential types (`ClaudeOAuth`, `ClaudeCredentials`, `TokenAccount`) to `provider/claude/credentials.go`. Delete `cmd/cq/providers.go` (replaced by explicit construction in `main.go`).
7. **Kong CLI** — replace hand-rolled arg parsing. Add `kong` to `go.mod`.
8. **Cache refactor** — implement `app.Cache` interface, inject `FileSystem` interface for testing. `CQ_TTL` env var read in `main.go`, passed as config to `cache.New(dir, ttl)`.
9. **Tests** — add at each step, comprehensive pass at end.
10. **Cleanup** — remove old files, verify `go vet`, `go test ./...`, `go build ./...`.

## Design Principles

- `quota/` has zero dependencies (pure domain types + helpers)
- `aggregate/` depends only on `quota/` (pure math, no I/O)
- `output/` depends on `app/` + domain types (never on provider internals)
- `app/` is the orchestration seam (one Report for any renderer)
- `auth/` and `keyring/` are concrete helper packages (not generic domain boundaries)
- Interfaces live with consumers (Go convention); `Provider`/`Authenticator`/`AccountManager` live in `provider/` because `app/` consumes them and they define the provider contract
- Provider fetch failures become error Results, not fatal errors
- No circular dependencies, no `init()` magic, no self-registering plugins
- `BurndownFmt string` removed from `AggregateResult` in migration step 1 (aggregate/types.go) — TTY rendering computes display strings from `Burndown int64` directly. Current field has `json:"-"` and no callers in display code; verify with grep before removal
