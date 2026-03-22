# AGENTS.md

Universal instruction file for all AI coding assistants working on cq.

## Project

cq is a CLI tool that checks API quota/usage limits for Claude, Codex, and Gemini providers. Built in Go.

## Commands

```bash
go build ./...                    # Build
go vet ./...                      # Lint
go test -race -count=1 ./...      # Test (always use -race)
go run ./cmd/cq                   # Run (all providers)
go run ./cmd/cq check claude      # Run (single provider)
```

## Architecture

- **Two phases:** Fetch (provider APIs → quota results) and Render (results → TTY or JSON)
- **Three providers:** Claude (multi-account), Codex (single-account), Gemini (single-account)
- **Concurrent fetch:** Each provider runs in its own goroutine with panic recovery
- **Aggregate layer:** Weighted pace, correction-deadline gauge, and burndown across 2+ accounts

## Package Structure

| Package | Purpose | AGENTS.md |
|---------|---------|-----------|
| `cmd/cq` | CLI entry point (kong), wires providers/cache/renderer | [AGENTS.md](cmd/cq/AGENTS.md) |
| `internal/provider` | Provider interface + ID constants | [AGENTS.md](internal/provider/AGENTS.md) |
| `internal/provider/claude` | Multi-account, OAuth refresh, parallel profile+usage | [AGENTS.md](internal/provider/claude/AGENTS.md) |
| `internal/provider/codex` | Single account, no refresh (shared credentials) | [AGENTS.md](internal/provider/codex/AGENTS.md) |
| `internal/provider/gemini` | Single account, no refresh (shared credentials) | [AGENTS.md](internal/provider/gemini/AGENTS.md) |
| `internal/app` | Runner (concurrent fetch), Report types, account management | [AGENTS.md](internal/app/AGENTS.md) |
| `internal/output` | TTY renderer (lipgloss), JSON renderer | [AGENTS.md](internal/output/AGENTS.md) |
| `internal/aggregate` | Weighted pace, gauge (correction-deadline), burndown computation | [AGENTS.md](internal/aggregate/AGENTS.md) |
| `internal/quota` | Domain types (Result, Window, Status), time helpers | [AGENTS.md](internal/quota/AGENTS.md) |
| `internal/auth` | OAuth PKCE flow, browser detection, JWT email decode | [AGENTS.md](internal/auth/AGENTS.md) |
| `internal/keyring` | Credential discovery (keychain + credentials file + cq keyring) | [AGENTS.md](internal/keyring/AGENTS.md) |
| `internal/cache` | File-based JSON cache with atomic writes | [AGENTS.md](internal/cache/AGENTS.md) |
| `internal/httputil` | HTTP client with User-Agent, body limits, redirect safety | [AGENTS.md](internal/httputil/AGENTS.md) |
| `internal/fsutil` | FileSystem interface for test injection | [AGENTS.md](internal/fsutil/AGENTS.md) |

## Key Decisions (Do Not Re-Litigate)

1. **Dependency injection everywhere.** `httputil.Doer` for HTTP, `fsutil.FileSystem` for filesystem, `app.Cache` interface. All testable via fakes.
2. **Panic recovery is mandatory.** Every goroutine that calls external code must have `defer recover()`. Inner goroutines (Claude profile+usage) need their own recovery.
3. **Atomic file writes.** All credential/cache persistence uses write-to-tmp + rename. No partial writes.
4. **Nil-safe cache.** Runner guards all cache access on `r.Cache != nil`. Cache failure degrades gracefully.
5. **Error classification.** `quota.ErrorResult` with specific codes: `not_configured`, `no_token`, `auth_expired`, `fetch_error`, `fetch_panic`, `api_error`, `parse_error`. Never return bare errors to the runner.
6. **TOCTOU on credential files is accepted.** File-locking adds complexity without proportionate benefit for a CLI tool.
7. **Direct stderr for diagnostics.** Standard CLI pattern. Not a library — no need to inject `io.Writer` everywhere.

## Git Workflow

Read `CONTRIBUTING.md` for the full git strategy. Key rules:

- **Trunk-based on `main`.** Every change lands via a short-lived branch and a squash-merged PR.
- **Branch naming:** `{kind}/{slug}` (e.g. `feat/gemini-provider`, `fix/oauth-callback`).
- **Commit messages:** `type: description` (imperative). Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.

## Coding Standards

- Go 1.21+ (uses built-in `min`, `max`)
- `go vet` and `-race` on all test runs — no exceptions
- Provider IDs are lowercase constants (`claude`, `codex`, `gemini`); display names are capitalised in output
- Newtypes for domain concepts: `provider.ID`, `quota.WindowName`, `quota.Status`
- `fmt.Fprintf(os.Stderr, ...)` for diagnostic messages, never `log.Printf`
- File permissions: `0o600` for secrets, `0o700` for credential directories
- HTTP responses bounded via `httputil.ReadBody` (1 MiB limit)

## Gotchas

- Claude has **multi-account** support; Codex/Gemini are single-account
- `keyring.DiscoverClaudeAccounts()` calls real keychain — tests must mock at provider level
- `mergeAnonymousFresh` uses token affinity (`sameStoredAccount`) to match anonymous entries — never merges blindly
- `dedup` in Claude parser prefers usable results over errors on key collision
- OAuth `sync.Once` gates only the **valid** callback path — invalid requests don't consume it
- `MinRemainingPct()` returns `-1` for empty windows (not `0`) to distinguish "no data" from "depleted"
- Aggregate `Burndown == 0` is ambiguous (exhausted or no data) — callers show em-dash for both
- Aggregate gauge is asymmetric: left (overburn) = dry spot deadline, right (underburn) = projected waste magnitude; `Sustainability` float retained for JSON backward compat

## Testing

- All tests use `-race`; the codebase is race-free
- Provider tests use `urlRewriter` (test HTTP transport) and `fakeFS` / `fsutil.MemFS`
- `t.Setenv` for environment-dependent tests (Gemini credentials)
- `t.TempDir` for file-based tests (cache, Gemini provider)
- Every rule: test the happy path, the error path, and at least one edge case

## What NOT To Do

- Do not log token values or credentials in error messages
- Do not use `io.ReadAll` for HTTP responses — always use `httputil.ReadBody`
- Do not skip `-race` in test runs
- Do not add `log` package imports — use `fmt.Fprintf(os.Stderr, ...)`
- Do not mutate shared variables in goroutines without synchronisation
- Do not collapse package boundaries to move faster
