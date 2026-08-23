# Direct Gemini Antigravity HTTP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Gemini's Antigravity CLI subprocess with direct, bounded HTTP calls while preserving provider output and making every independent fetch concurrent.

**Architecture:** Gemini reads Antigravity credentials and cached project ID concurrently. It refreshes expired OAuth tokens in memory, resolves a missing project through `loadCodeAssist`, then calls `retrieveUserQuotaSummary`. Existing app runner remains provider-concurrent; a characterization test locks that behavior down.

**Tech Stack:** Go 1.21+, `net/http`, `encoding/json`, `github.com/zalando/go-keyring`, existing `fsutil`, `httputil`, `quota`, and race-enabled Go tests.

**Spec:** `docs/superpowers/specs/2026-08-23-direct-gemini-antigravity-http.md`

## Global Constraints

- Never invoke `agy` or another subprocess.
- Never print, persist, or return access tokens, refresh tokens, or OAuth client secret.
- Read Antigravity-owned keychain and project cache only; never write them.
- Bound every HTTP response with `httputil.ReadBody`.
- Recover panics in every goroutine calling injected or external code.
- Run every Go test with `-race -count=1`.
- Keep Gemini provider ID and account identity stable.
- Inject OAuth client secret at release link time; no runtime environment fallback.

---

### Task 1: Parse direct quota summaries

**Files:**
- Modify: `internal/provider/gemini/parser_test.go`
- Modify: `internal/provider/gemini/parser.go`

- [x] Replace CLI-envelope parser tests with direct `groups[].buckets[]` fixtures.
- [x] Cover `gemini-5h`, `gemini-weekly`, ignored `3p-*`, malformed JSON, missing buckets, duplicate buckets, invalid fractions, and invalid reset times.
- [x] Run failing parser tests:
  `go test -race -count=1 ./internal/provider/gemini -run 'TestParseQuotaSummary'`
- [x] Implement minimal direct parser using camel-case fields.
- [x] Map `gemini-5h` to `quota.Window5Hour` and `gemini-weekly` to `quota.Window7Day`.
- [x] Preserve account ID `antigravity-cli`, label `Antigravity CLI`, active status, and provider ID `gemini`.
- [x] Re-run parser tests and commit.

### Task 2: Read local Antigravity inputs concurrently

**Files:**
- Create: `internal/provider/gemini/credentials.go`
- Create: `internal/provider/gemini/credentials_test.go`
- Rewrite: `internal/provider/gemini/provider.go`
- Rewrite: `internal/provider/gemini/provider_test.go`

- [x] Add failing tests for keychain JSON decoding, missing credentials, malformed credentials, project-cache trimming, empty/oversized/control-character project IDs, and missing project cache.
- [x] Add a timing/barrier test proving credentials and project-cache reads start before either is released.
- [x] Run failing tests:
  `go test -race -count=1 ./internal/provider/gemini -run 'Test(ReadCredentials|ReadProjectID|FetchReadsLocalInputsConcurrently)'`
- [x] Add injected credential-reader and filesystem dependencies.
- [x] Decode `token.access_token`, `token.refresh_token`, `token.expiry`, and `token.token_type` without exposing values in errors.
- [x] Read `~/.gemini/antigravity-cli/cache/default_project_id.txt` with a strict size bound and input validation.
- [x] Start credential and project reads in sibling goroutines, recover panics in both, then join.
- [x] Re-run focused tests and commit.

### Task 3: Add bounded Antigravity HTTP client

**Files:**
- Create: `internal/provider/gemini/client.go`
- Create: `internal/provider/gemini/client_test.go`
- Modify: `internal/provider/gemini/provider.go`
- Modify: `internal/provider/gemini/provider_test.go`

- [x] Add failing HTTP tests for exact methods, URLs, form/JSON bodies, `Authorization`, `Content-Type`, and `User-Agent: antigravity/cli/cq`.
- [x] Cover fresh access token, expired-token refresh, optional rotated refresh token, cached project fast path, missing-project `loadCodeAssist` fallback, cancellation, oversized body, 401, other non-200 status, malformed response, and missing OAuth client secret.
- [x] Assert errors and diagnostics never contain token or secret values.
- [x] Run failing tests:
  `go test -race -count=1 ./internal/provider/gemini -run 'Test(RefreshAccessToken|LoadCodeAssist|RetrieveUserQuotaSummary|ProviderFetch)'`
- [x] Implement OAuth refresh against `https://oauth2.googleapis.com/token` using form encoding.
- [x] Implement project resolution against `https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`.
- [x] Implement quota retrieval against `https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`.
- [x] Explicitly set Antigravity user agent on private endpoints and bound all bodies.
- [x] Refresh only when expiry is within skew; retain refreshed values only in process memory.
- [x] Classify boundary failures into existing quota error codes without raw server bodies.
- [x] Re-run focused and package tests, then commit.

### Task 4: Wire production and remove CLI path

**Files:**
- Modify: `cmd/cq/main.go`
- Modify: `.goreleaser.yml`
- Modify: `.github/workflows/release.yml`
- Delete: `internal/provider/gemini/cli.go`
- Delete: `internal/provider/gemini/cli_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [x] Find Antigravity public OAuth client ID without exposing installed client secret.
- [x] Add source-held public client ID and empty production client-secret variable.
- [x] Pass shared bounded HTTP client and link-time client secret into Gemini provider.
- [x] Add GoReleaser `-X` injection from protected `GEMINI_ANTIGRAVITY_CLIENT_SECRET`.
- [x] Make release workflow fail before packaging when protected secret is absent.
- [x] Remove command-runner implementation and tests.
- [x] Remove obsolete direct dependency only if no longer used; retain `go-keyring` because direct credential read requires it.
- [x] Verify no production `agy` references remain:
  `rg -n '\bagy\b|exec\.Command|commandRunner' internal/provider/gemini cmd/cq`
- [x] Run `go test -race -count=1 ./internal/provider/gemini ./cmd/cq`, `go vet ./...`, and `go build ./...`; commit.

### Task 5: Lock down provider concurrency and update guidance

**Files:**
- Modify: `internal/app/runner_test.go`
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `internal/provider/AGENTS.md`
- Modify: `internal/provider/gemini/AGENTS.md`

- [x] Add characterization test proving Claude, Codex, and Gemini all enter `Fetch` before any provider completes.
- [x] Run test:
  `go test -race -count=1 ./internal/app -run 'TestRunnerStartsAllProvidersBeforeWaiting'`
- [x] Update setup and architecture text: Gemini reads Antigravity credentials and uses direct HTTP; no CLI dependency.
- [x] Preserve explicit read-only credential and project-cache ownership boundary.
- [x] Re-run affected tests and commit.

### Task 6: Full verification and benchmark

**Files:**
- Modify only if verification exposes a defect.

- [x] Run `gofmt` on changed Go files and confirm clean formatting diff.
- [x] Run `go test -race -count=1 ./...` without competing test process.
- [x] Run `go vet ./...` and `go build ./...`.
- [x] Build a local binary with privately extracted client secret passed through `-ldflags`; do not print it.
- [x] Run repeated `--refresh` benchmarks for Gemini-only and all-provider paths.
- [x] Compare against measured pre-change Gemini median 3.384 seconds and all-provider median 4.005 seconds.
- [x] Inspect final diff and working-tree status; report any skipped release-only verification.

