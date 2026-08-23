# Direct Gemini Antigravity HTTP Design

- **Status:** Approved
- **Date:** 2026-08-23
- **Scope:** Gemini quota fetch performance, authentication, and concurrency
- **Supersedes:** Gemini runtime-authority and non-goal decisions in
  `2026-08-20-gemini-antigravity-migration.md`

## Outcome

`cq --refresh` keeps fetching Claude, Codex, and Gemini concurrently. Gemini
stops launching `agy` and instead calls Antigravity's quota HTTP API directly.
CQ reuses Antigravity's existing Keychain credential without taking ownership
of it.

Provider ID, account ID, cache key, output shape, and quota-window mapping stay
compatible. No CLI fallback remains.

## Evidence and root cause

Five sequential live samples from installed CQ 0.23.7 produced:

| Command | Minimum | Median | Mean | Maximum |
|---|---:|---:|---:|---:|
| all providers, forced refresh | 3.554s | 4.005s | 3.983s | 4.444s |
| Gemini, forced refresh | 3.351s | 3.384s | 3.404s | 3.464s |
| Codex, forced refresh | 0.659s | 0.705s | 0.694s | 0.716s |
| Claude, forced refresh | 0.040s | 0.041s | 0.042s | 0.044s |

Current source already starts every selected provider in its own goroutine.
Claude and Codex also fetch accounts concurrently; Claude fetches independent
profile and usage requests concurrently. Global serialization is not the
regression.

Gemini dominates wall time because its provider launches `agy` for `/usage`.
Observed Antigravity traffic performs quota-unrelated startup work, including
user and model discovery, and issues redundant quota-summary requests. Direct
read-only probes of the required HTTP path measured:

| Direct path | Minimum | Median | Mean | Maximum |
|---|---:|---:|---:|---:|
| valid access token, 8 samples | 0.195s | 0.314s | 0.314s | 0.433s |
| OAuth refresh required, 8 samples | 0.331s | 0.600s | 0.607s | 0.907s |

The direct refresh exchange itself measured approximately 0.06-0.10s. Process
startup is therefore only part of the regression; removing unrelated and
duplicate Antigravity work is the main win.

## Existing provider pattern

Claude and Codex perform OAuth refresh and quota requests through an injected
`httputil.Doer`. Both keep public OAuth client IDs in source. Neither requires
a client secret for its installed-client refresh exchange.

Antigravity's existing refresh token additionally requires its installed-client
secret. CQ follows the same injected HTTP pattern, but release builds inject
that secret at link time. The secret never appears in source, tests,
documentation, diagnostics, or committed build configuration.

## Runtime architecture

Gemini provider dependencies are:

- shared `httputil.Doer` from command wiring;
- `fsutil.FileSystem` for Antigravity's cached project ID;
- a package-local credential-reader interface for Keychain access;
- link-time OAuth client secret supplied by command wiring.

Production construction supplies OS-backed implementations. Tests inject
fakes. `cmd/cq` passes the same shared HTTP client used by Claude and Codex.

### Local discovery

At fetch start, CQ reads these independent local inputs concurrently:

1. Keychain service `gemini`, account `antigravity`;
2. `~/.gemini/antigravity-cli/cache/default_project_id.txt`.

Each goroutine has mandatory panic recovery. Results are joined before network
work. Keychain JSON is decoded into only required fields: access token, refresh
token, token type, and RFC3339 expiry. Project content is trimmed and bounded;
empty, oversized, or control-character-bearing values are rejected.

Account discovery uses credential presence, not `agy` presence. Stable account
ID remains `antigravity-cli`, preserving cache affinity. Label remains
`Antigravity CLI` for output compatibility even though CQ no longer executes
it.

### Token selection and refresh

CQ uses the stored access token when it remains valid beyond a small expiry
skew. When expired or near expiry, CQ sends one form-encoded request to:

`POST https://oauth2.googleapis.com/token`

Exact form members are `client_id`, injected `client_secret`, `refresh_token`,
and `grant_type=refresh_token`.

OAuth refresh must finish before authenticated Antigravity calls. It is a true
dependency and is not parallelized. A rotated refresh token is retained only
for the current process.

CQ never writes Antigravity's Keychain entry or project cache. This preserves
external credential authority and avoids races with Antigravity. Consequence:
when the externally stored access token stays expired, each forced CQ process
performs the approximately 0.06-0.10s refresh exchange again.

### Project resolution

When cached project ID is usable, CQ skips project discovery. Otherwise it
sends:

`POST https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`

with exact JSON body:

```json
{"metadata":{"ideType":"ANTIGRAVITY"}}
```

CQ extracts `cloudaicompanionProject` for the current request only. Project
resolution depends on a valid access token, so it cannot run alongside OAuth
refresh.

### Quota request

CQ sends:

`POST https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`

with JSON body containing the resolved project:

```json
{"project":"<project-id>"}
```

Both Antigravity requests set:

- `Authorization: Bearer <access-token>`;
- `Content-Type: application/json`;
- `User-Agent: antigravity/cli/cq`.

Live probes established that generic CQ user agents receive
`SUBSCRIPTION_REQUIRED`; the Antigravity namespace is part of request
eligibility. Explicit request header overrides the shared client's default CQ
user agent.

All responses use `httputil.ReadBody` and its 1 MiB limit. Redirect protection
from the shared client remains active.

### Required ordering

Runtime flow is:

```text
credential read ─┐
                 ├─> optional OAuth refresh ─> optional project discovery ─> quota
project read ────┘
```

Only independent operations run concurrently. Speculative quota calls with an
expired token and parallel calls that lack a project are excluded because they
add failed traffic rather than reduce useful latency.

## Quota mapping

Direct response parser reads `groups[].buckets[]` and maps exact bucket IDs:

| Antigravity bucket | CQ window |
|---|---|
| `gemini-5h` | `5h` |
| `gemini-weekly` | `7d` |

`3p-*` and unknown buckets are ignored. Each required bucket must occur exactly
once and include `remainingFraction`. Fraction remains clamped to 0-1, rounded
to percentage, and paired with parsed `resetTime`. Missing or duplicate
required buckets reject the response; partial quota is never reported as
complete.

The old zero-turn `/usage` envelope checks disappear because CQ no longer asks
Antigravity to execute a model command.

## Error behaviour

- Missing Keychain entry: `not_configured`.
- Malformed credential or project data: `parse_error`.
- Missing access token: `no_token`.
- Expired token without refresh token or injected client secret:
  `auth_expired`.
- OAuth rejection or quota HTTP 401: `auth_expired`.
- Request, cancellation, timeout, or bounded-read failure: `fetch_error`.
- Other non-200 Antigravity response: `api_error` with status code.
- Malformed or incomplete quota body: `parse_error`.
- Recovered provider-local panic: `fetch_panic`.

Messages remain privacy-safe. No token, secret, Keychain payload, project ID,
response body, or OAuth error body enters diagnostics.

Runner cache backfill remains unchanged. `--refresh` continues bypassing cached
quota by definition; direct HTTP makes a true refresh fast instead of weakening
refresh semantics.

## Build and release

`cmd/cq` owns an empty link-time string and passes it into Gemini construction.
GoReleaser binds that string from a protected release environment variable.
Release workflow fails before packaging when the secret is absent. Test builds
use a synthetic injected value; no live secret is required.

A development build without the secret still works while Antigravity's stored
access token is valid. Once expired, it returns `auth_expired` rather than
executing `agy`, scraping an installed binary, or accepting a runtime secret
environment fallback.

## Verification

### Deterministic tests

- Prove all selected providers begin fetch before any provider is released.
- Prove Keychain and project reads begin before either local read is released.
- Assert exact HTTP methods, hosts, paths, bodies, content types, user agent,
  and bearer-token selection.
- Cover valid-token fast path, expired-token refresh, missing project discovery,
  missing build secret, cancellation, timeouts, oversized bodies, 401, other
  non-200 responses, malformed credentials, and malformed quota.
- Cover required bucket uniqueness, `3p-*` exclusion, fraction clamping, reset
  parsing, stable account identity, and cache compatibility.
- Prove no Gemini command runner or `agy` lookup remains.

Every Go test invocation uses `-race -count=1`.

### Live performance gate

Build CQ with injected release-equivalent credential configuration. Collect at
least ten sequential samples for Gemini-only and all-provider forced refresh.
Report minimum, median, mean, p95, and maximum. Do not encode live-network
latency as a flaky unit-test threshold.

Acceptance requires:

- Gemini median below 1.0s on the current machine and account;
- all-provider execution demonstrably concurrent;
- full-refresh wall time bounded by the slowest provider plus small local
  assembly overhead, not the sum of provider durations;
- no `agy` process and no unrelated model/user discovery requests;
- live output containing Gemini `5h` and `7d` windows with unchanged provider
  and account identity.

## Speed and efficiency inventory

### Included

1. Remove `agy` lookup, process launch, slash-command orchestration, and nested
   15/20-second timeout layers.
2. Eliminate unrelated user/model calls and redundant quota requests.
3. Reuse one shared HTTP client and transport.
4. Preserve provider-level, account-level, and Claude profile/usage
   concurrency.
5. Read independent Gemini local inputs concurrently.
6. Skip OAuth refresh while token is valid.
7. Skip `loadCodeAssist` while project cache is usable.
8. Parse direct quota response without command-envelope allocation and checks.
9. Keep response reads bounded and cancellation propagated.

### Rejected or deferred

- Serving cached quota during `--refresh`: violates explicit refresh semantics.
- Mutating Antigravity Keychain: saves only refresh-exchange latency while
  creating shared-authority races.
- CQ-owned duplicate Gemini credentials: adds secret lifecycle and
  synchronization for a single-digit percentage latency gain.
- Speculative parallel refresh/quota requests: sends predictably failing calls.
- Always calling `loadCodeAssist`: adds avoidable sequential network latency.
- Replacing OS Keychain adapter with a platform-native CQ implementation:
  larger cross-platform and release impact for local millisecond-scale savings.
- Adding retries by default: worsens tail latency and can duplicate private API
  traffic. Existing cache backfill handles transient failures.
- Parser micro-optimisation or streaming decode: response is small and bounded;
  network latency dominates.
- Unbounded goroutine fan-out: current account counts are small and existing
  bounded provider/account parallelism already hides independent latency.

## Expected code shape

- `provider.go`: orchestration, concurrent local reads, result classification.
- `credentials.go`: Keychain reader and narrow Antigravity credential decoder.
- `client.go`: OAuth, project, and quota HTTP requests.
- `parser.go`: direct quota response mapping.
- `cmd/cq/main.go`: shared HTTP client and link-time secret wiring.
- `.goreleaser.yml` and release workflow: protected link-time injection.
- Gemini/provider instruction files and user documentation: direct HTTP
  authority and no-CLI contract.
- `cli.go` and command-runner tests: deleted.

## Non-goals

- Gemini login, logout, switching, or multi-account support.
- Writing or rotating Antigravity-owned credentials.
- Renaming provider or changing output schema.
- Reporting Antigravity Claude/GPT buckets.
- Changing cache TTL, aggregation, renderer, or `--refresh` semantics.
- CLI fallback when the private HTTP contract changes.
