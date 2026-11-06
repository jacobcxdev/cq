# AGENTS.md

Universal instruction file for all AI coding assistants working on cq.

## Project

CQ is a Go CLI for quota reporting, account management and local API routing for Claude, Codex, Gemini and Z.ai. It also owns per-user services and optional native macOS integrations. Use Go **1.26.1**, as declared in `go.mod`.

## Commands

```bash
go build ./...                    # Build
go vet ./...                      # Lint
go test -race -count=1 ./...      # Test (always use -race)
go run ./cmd/cq                   # Run (all providers)
go run ./cmd/cq check claude      # Run (single provider)
```

## Architecture

- **Reporting phases:** Fetch provider quota results, then build/render TTY or JSON reports
- **Four providers:** Claude, Codex and Z.ai support multiple accounts; Gemini uses a single Antigravity account
- **Concurrent fetch:** Each provider runs in its own goroutine with panic recovery
- **Aggregate layer:** Weighted pace, correction-deadline gauge, and burndown across 2+ accounts

The [architecture guide](docs/architecture.md) covers runtime ownership, credentials, routing pools, resets, installation and harness contracts. The [release guide](docs/releases.md) covers packaging and delivery. Current source and tests remain authority.

## Package Structure

| Package | Purpose | AGENTS.md |
|---------|---------|-----------|
| `cmd/cq` | CLI entry point (kong), wires providers/cache/renderer | [AGENTS.md](cmd/cq/AGENTS.md) |
| `internal/provider` | Provider interface + ID constants | [AGENTS.md](internal/provider/AGENTS.md) |
| `internal/provider/claude` | Multi-account, OAuth refresh, parallel profile+usage | [AGENTS.md](internal/provider/claude/AGENTS.md) |
| `internal/provider/codex` | Multi-account, automatic reads only (shared system credentials) | [AGENTS.md](internal/provider/codex/AGENTS.md) |
| `internal/provider/gemini` | Single account, direct Antigravity OAuth and quota HTTP | [AGENTS.md](internal/provider/gemini/AGENTS.md) |
| `internal/provider/zai` | Multi-account Coding Plan, Start and ZCode quota/authentication | Root rules and [routing guide](docs/zcode-routing.md) |
| `internal/proxy` | Supervisor/worker, routing, continuity, private control and transport | [Architecture](docs/architecture.md) |
| `internal/modelregistry` | Native catalogues, overlays and client projections | [Architecture](docs/architecture.md) |
| `internal/dots` | Optional multi-account native adapter and launcher | [Dots](docs/dots.md) |
| `internal/harness` | Scoped capability policy, inventory and application | [Architecture](docs/architecture.md) |
| `internal/installer`, `internal/installstate`, `cmd/cq-install` | Complete per-user installation and ownership | [Releases](docs/releases.md) |
| `internal/history`, `internal/userdirs` | Burn estimates and platform user roots | [Architecture](docs/architecture.md) |
| `native/macos-network` | Separate signed host and network system extension | [README](native/macos-network/README.md) |
| `internal/tools`, `.github`, `scripts` | Build, packaging and qualification gates | [Releases](docs/releases.md) |
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

1. **Dependency injection everywhere.** `httputil.Doer` for HTTP, `fsutil.FileSystem` for filesystem, a narrow credential reader for Antigravity Keychain access, and `app.Cache` interface. All testable via fakes.
2. **Panic recovery is mandatory.** Every goroutine that calls external code must have `defer recover()`. Inner goroutines (Claude profile+usage) need their own recovery.
3. **Atomic file writes.** All credential/cache persistence uses write-to-tmp + rename. No partial writes.
4. **Nil-safe cache.** Runner guards all cache access on `r.Cache != nil`. Cache failure degrades gracefully.
5. **Error classification.** `quota.ErrorResult` with specific codes: `not_configured`, `no_token`, `auth_expired`, `fetch_error`, `fetch_panic`, `api_error`, `parse_error`. Never return bare errors to the runner.
6. **Preserve each filesystem owner's authority contract.** Legacy credential discovery accepts ordinary file-read races; this does not weaken coordinator, routing-policy, installer or durable-journal requirements for locking, no-follow access, identity checks and crash-safe commits.
7. **Direct stderr for diagnostics.** Standard CLI pattern. Not a library — no need to inject `io.Writer` everywhere.

## Git Workflow

Read `CONTRIBUTING.md` for the full git strategy. Key rules:

- **Trunk-based on `main`.** Every change lands via a short-lived branch and a squash-merged PR.
- **Branch naming:** `{kind}/{slug}` (e.g. `feat/gemini-provider`, `fix/oauth-callback`).
- **Commits:** always signed; `type: description` with a past-tense subject under 50 characters and a concise body listing significant changes. Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.

### Releases

- Release tags must identify the current, fetched `origin/main` tip after PR
  review and merge. Never release from feature branches, PR heads or pre-merge
  candidates. The release workflow enforces this before packaging.
- Publish stable versions sequentially and in order. Increment exactly one
  semver component by one, resetting lower components. Inspect published
  releases, tags and drafts first; stop on conflicts or unexplained gaps rather
  than skipping versions or publishing an older version later.
- Review the entire diff from the latest published release to `origin/main`.
  Include every merged fix and retain previously shipped changes. Passing tests
  on a stale branch does not establish a complete release.
- Changed `native/macos-network` sources require a new signed, notarised native
  companion and reviewed `.github/native-network-release.json` pin. A newer Go
  version must not ship an older companion which omits merged native fixes.
- Follow `CONTRIBUTING.md` and `docs/native-network-releases.md`. Verify published
  Casks, installed companion/provider versions and real affected traffic before
  claiming delivery. A merge, tag, draft or Go executable upgrade is insufficient.
- Keep one active release attempt per stream. Account for failed or superseded
  drafts. Preserve published tags and assets; correct omissions in the next
  complete release and annotate affected historical release notes.

## Coding Standards

- Go 1.26.1 (`go.mod` is the toolchain authority)
- `go vet` and `-race` on all test runs — no exceptions
- Provider IDs are lowercase constants (`claude`, `codex`, `gemini`, `zai`); display names are capitalised in output
- Newtypes for domain concepts: `provider.ID`, `quota.WindowName`, `quota.Status`
- `fmt.Fprintf(os.Stderr, ...)` for diagnostic messages, never `log.Printf`
- File permissions: `0o600` for secrets, `0o700` for credential directories
- HTTP responses bounded via `httputil.ReadBody` (1 MiB limit)

## Gotchas

- Claude, Codex and Z.ai have **multi-account** support; Gemini is single-account
- Gemini keeps provider ID `gemini`, reads Antigravity-owned local state concurrently, and calls private Antigravity HTTP endpoints directly
- Gemini OAuth client secret is release-time link data; never add runtime environment, CLI, binary-scraping, or persistence fallbacks
- `keyring.DiscoverClaudeAccounts()` calls real keychain — tests must mock at provider level
- `mergeAnonymousFresh` uses token affinity (`sameStoredAccount`) to match anonymous entries — never merges blindly
- `dedup` in Claude parser prefers usable results over errors on key collision
- OAuth `sync.Once` gates only the **valid** callback path — invalid requests don't consume it
- `MinRemainingPct()` returns `-1` for empty windows (not `0`) to distinguish "no data" from "depleted"
- Aggregate `Burndown == 0` is ambiguous (exhausted or no data) — callers show em-dash for both
- Aggregate gauge severity uses demand/supply ratio; accompanying overburn metrics show dry-spot timing and underburn metrics show projected waste. The imminent-gap override preserves the natural gauge position. `Sustainability` remains in JSON for backward compatibility

## Testing

- Run every Go test with `-race`; passing evidence applies to the tested source, toolchain and environment
- HTTP provider tests use `urlRewriter` or an injected `httputil.Doer`; filesystem tests use `fakeFS` / `fsutil.MemFS`; Gemini tests inject credential, filesystem, and HTTP fakes
- `t.Setenv` for environment-dependent and helper-process tests
- `t.TempDir` for file-based tests
- Every rule: test the happy path, the error path, and at least one edge case

- Native macOS changes also require `swift test --package-path native/macos-network --sanitize thread` and the applicable signed/installed gates; a Go cross-build does not qualify native behaviour.
- Keep implementation plans and review evidence local and ignored. Maintain current product/maintainer contracts in published guides; local planning and review records are not clean-checkout dependencies.

## What NOT To Do

- Do not log token values or credentials in error messages
- Do not use `io.ReadAll` for HTTP responses — always use `httputil.ReadBody`
- Do not skip `-race` in test runs
- Do not add `log` package imports — use `fmt.Fprintf(os.Stderr, ...)`
- Do not mutate shared variables in goroutines without synchronisation
- Do not collapse package boundaries to move faster
