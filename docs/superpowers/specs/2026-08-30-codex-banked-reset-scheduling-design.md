# Codex Banked Reset Scheduling Design

- **Status:** Draft for review
- **Date:** 2026-08-30
- **Scope:** Codex reset-credit discovery, portfolio-wide scheduling recommendations,
  explicit per-account consumption, retry safety, and burn-history correction

## Outcome

CQ recommends when to consume every useful Codex banked reset across all known
accounts. Recommendation is portfolio-wide: it considers account quota,
observed burn, natural window resets, reset-credit expiries, account
multipliers, weekly gating, and projected pool coverage together. Credits with
the same expiry can therefore receive different recommended consumption times.

CQ never consumes a reset automatically. A separate CLI command consumes one
selected credit for one explicitly supplied account after confirmation. Any
account shown by `cq codex accounts` is a valid command target. Consumption
never changes the active Codex account.

A banked reset changes usage percentages only. It sets affected used
percentages to zero, equivalently restoring remaining percentages to 100. It
does not change a 5-hour or 7-day window's reset date. Both recommendation and
history processing preserve that invariant.

## Non-goals

- Consume credits from `recommend`, background agent, proxy, or another
  automatic path.
- Switch, activate, adopt, remove, or project Codex credentials while listing,
  recommending, or consuming resets.
- Refresh system, borrowed, exported, external, legacy, or uncertain
  credentials.
- Optimise model choice, task admission, or proxy routing policy.
- Add user-configurable optimiser weights, beam widths, lead times, or demand
  models.
- Support unknown reset types speculatively.
- Claim an incomplete portfolio schedule is optimal.

## Upstream contract

Design follows `openai/codex` commit
`b8c86376a258e55efc8e5ecfbabc21c16c07d814` from 2026-08-29. Relevant upstream
owners are:

- `codex-rs/backend-client/src/client/rate_limit_resets.rs` for paths and
  request bodies;
- `codex-rs/backend-client/src/types.rs` for response types and outcomes;
- `codex-rs/app-server/src/request_processors/account_processor/rate_limit_resets.rs`
  for validation, authentication, and outcome mapping;
- `codex-rs/tui/src/chatwidget/reset_credits.rs` for available-credit filtering
  and earliest-expiry ordering.

For ChatGPT-backed credentials CQ uses:

```text
GET  https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
POST https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume
```

Credit-list requests use a 5-second timeout. Consume requests use a 10-second
timeout, matching upstream app-server boundaries.

Requests carry the same account authority as CQ's existing usage request:

```text
Authorization: Bearer <access token>
ChatGPT-Account-Id: <account ID>
Content-Type: application/json
```

Consume body:

```json
{
  "redeem_request_id": "<idempotency key>",
  "credit_id": "<selected credit ID>"
}
```

CQ supports reset type `codex_rate_limits` and statuses `available`,
`redeeming`, and `redeemed`. Unknown values remain visible in list output but
are ineligible for default selection and recommendation.

Consume outcomes are `reset`, `already_redeemed`, `nothing_to_reset`, and
`no_credit`. `reset` also reports `windows_reset`. `reset` and
`already_redeemed` are successful terminal outcomes. The other two are
definitive no-ops.

The `/wham` contract is upstream-owned but not a public stable API. CQ isolates
it behind one client, bounds response bodies through `httputil.ReadBody`, and
fails closed on path, schema, reset-type, or outcome drift. CQ does not fall
back to account switching or scrape Codex output.

## CLI contract

```text
cq codex resets list [account-reference]
cq codex resets recommend
cq codex resets use <account-reference> [--credit <credit-id>] [--yes]
```

`account-reference` accepts a unique email, CQ alias, or opaque `AccountKey`
through the existing `ResolveAccountReference` rules. Normal TTY and public
JSON identify accounts with existing email and account-ID fields; they do not
expose internal `AccountKey` values.

`list` reads reset credits for every visible account or one supplied account.
It renders credit ID, reset type, status, grant time, expiry, title, and
description. A failed account is rendered with a typed error while other
accounts remain visible.

`recommend` always covers every account visible in `cq codex accounts`. It has
no account selector. It bypasses quota cache, refreshes credit details, updates
burn history, and returns one joint schedule. It never invokes consume.

`use` requires an account but makes `--credit` optional. Selection rules are:

1. omitted `--credit` resumes the account's oldest unresolved pending attempt;
2. explicit `--credit` matching a pending attempt resumes that attempt;
3. explicit `--credit` differing from an unresolved attempt is rejected until
   the pending outcome is resolved;
4. without a pending attempt, explicit `--credit` selects that credit;
5. otherwise CQ selects the available, non-expired `codex_rate_limits` credit
   with earliest expiry;
6. non-expiring credits sort after every expiring credit;
7. equal expiries sort by grant time, then credit ID.

Pending attempt wins because an earlier response may have been lost after the
backend consumed that credit. Selecting a new default in that state could
consume a second credit.

Before POST, `use` prints account, selected credit, expiry, current percentages,
and current portfolio recommendation when available. Confirmation defaults to
No. `--yes` is explicit non-interactive authority for this one command; CQ does
not generate or invoke it.

An explicit credit lets the user override recommended timing. Recommendation
never blocks manual consumption.

Global `--json` applies to list, recommend, and use output. JSON carries stable
reason and error codes rather than requiring consumers to parse prose.

## Account and credential boundary

Account listing and reset commands must project the same sanitised credential
inventory so eligibility cannot drift. The reset path opens existing
`CredentialControl`, calls `List`, resolves an account reference, selects one
current candidate, creates a `PlannedCandidate`, and resolves that exact
generation through `ResolvePlannedCandidate`.

Reset code may call only read and exact-resolution capabilities. It never calls
`Activate`, `Adopt`, `SaveLogin`, `RemoveManaged`, or system projection.

Every visible account class can be targeted when its current exact credential
resolves:

- system credentials are read without refresh or write;
- managed CQ-owned credentials may refresh only through the existing
  coordinator broker after authentication failure;
- borrowed, exported, external, legacy, and uncertain credentials are never
  refreshed;
- invalid non-refreshable credentials return `auth_expired` without narrowing
  account visibility.

External credential authorities remain read-only. Secrets never enter result
types, errors, logs, pending-attempt files, recommendation state, or public
output.

## Reset-credit domain model

```go
type ResetCredit struct {
	ID          string
	ResetType   ResetType
	Status      ResetCreditStatus
	GrantedAt   time.Time
	ExpiresAt   *time.Time
	Title       string
	Description string
}

type ConsumeResetResult struct {
	Outcome      ConsumeResetOutcome
	WindowsReset int64
}
```

Timestamps must parse as RFC 3339. Invalid IDs, timestamps, negative available
counts, or contradictory status/count data make that account's credit inventory
unusable for recommendation and default selection. List output can still show
well-formed entries alongside a typed parse error.

Available credits whose expiry is at or before current time are expired, not
selectable. Missing expiry means no deadline and sorts last.

## Portfolio recommendation input

Recommendation input contains:

- one fresh `quota.Result` for every visible account;
- supported reset credits grouped by stable internal account key;
- account multipliers from current rate-limit tiers;
- per-account, per-window EWMA burn estimates;
- cycle-average fallback rates when EWMA is unavailable;
- current 5-hour and 7-day remaining percentages and natural reset epochs;
- current time.

Fresh usage and successful credit-list reads are required for every visible
account, including accounts with no available credit. Missing capacity can
change portfolio demand allocation and invalidate every recommended time.

EWMA estimates retain sample count and last-observation time for confidence
reporting. When an EWMA is unavailable, CQ uses existing cycle-average
consumption semantics and marks affected schedule items `low` confidence.
Missing history alone does not make the portfolio incomplete.

## Percentage-only state transitions

For a supported banked reset at time `t`, simulator applies:

```text
remaining_pct       = 100
remaining_pct_exact = 100
reset_at_unix       = unchanged
```

Natural resets independently set the matching window to 100 and advance its
reset epoch by its normal period. Banked reset never advances, restarts, or
re-phases either window.

Simulator treats shared 5-hour and 7-day windows as joint account constraints.
An account can serve projected demand only while both required shared windows
have capacity. Weekly exhaustion gates 5-hour capacity using existing
aggregate semantics. Additional scoped windows remain reported but do not
drive general portfolio scheduling until upstream exposes reset coverage for
them explicitly.

## Demand and capacity simulation

Future demand uses CQ's existing multiplier-weighted aggregate model. For each
shared window, total projected demand comes from current usable account burn
estimates. Demand is distributed proportionally across currently available
account multipliers. When either required window exhausts an account, demand
reallocates across remaining accounts. A natural or banked reset can make that
account available again.

Simulation is discrete-event rather than fixed-step. State advances directly
to next relevant event:

- projected account exhaustion;
- projected portfolio coverage gap;
- natural 5-hour or 7-day reset;
- credit expiry;
- candidate banked reset.

This preserves exact reset epochs and avoids time-grid artefacts.

Planning horizon ends one maximum supported window period after latest finite
credit expiry. If every credit is non-expiring, horizon is one 7-day period.
Non-expiring credits that have no useful event inside rolling horizon remain
deferred and are reconsidered next run.

## Optimisation objective

Every supported credit forecast to restore at least one percentage point in
any supported shared window at some feasible time before expiry must receive a
schedule entry. This is a hard constraint, not a weighted preference. A credit
that cannot yet restore one percentage point remains `not_yet_useful` until
demand or deadline creates a useful event.

Feasible schedules are compared lexicographically:

1. minimise unmet projected demand;
2. minimise portfolio coverage-gap duration;
3. minimise useful supported credits expiring unused;
4. minimise multiplier-weighted capacity discarded at natural resets;
5. maximise percentage restored by each consumed credit;
6. delay consumption when every earlier objective ties, preserving option
   value.

Lexicographic comparison avoids arbitrary cross-unit weights. Same-day credit
expiries do not imply same consumption time. Search can place each credit at
the latest event that avoids a gap or maximises restored capacity for its
account.

## Search strategy

CQ uses deterministic event-driven branch-and-bound with dominance pruning.
At each event, search branches between waiting and consuming each eligible
credit. States with same event and remaining credit set are dominated when
another state has no less capacity in every supported account/window and no
worse objective prefix.

Search keeps at most 512 non-dominated states per event. If pruning stays below
that bound, result is `exact=true`. If beam truncation occurs, result is
`exact=false`; deterministic objective order and stable account/credit IDs
choose survivors. Width is internal and not configurable.

For each chosen mathematical deadline, CLI recommends action five minutes
earlier:

```text
UseAt = max(Now, UseBy - 5 minutes)
```

`UseBy` is exhaustion or expiry boundary. Five-minute lead absorbs user
reaction and request latency. It does not alter simulator's natural reset
dates. `due_now` means current time is at or beyond `UseAt`.

## Recommendation result

```go
type ResetSchedule struct {
	GeneratedAt time.Time
	Horizon     time.Time
	Exact       bool
	Complete    bool
	Confidence  ResetScheduleConfidence
	Items       []ResetScheduleItem
	Objective   ResetScheduleObjective
	Blockers    []ResetScheduleBlocker
}

type ResetScheduleItem struct {
	AccountEmail  string
	AccountID     string
	CreditID      string
	UseAt         time.Time
	UseBy         time.Time
	Status        ResetScheduleStatus
	Confidence    ResetScheduleConfidence
	RestoredPct   map[quota.WindowName]float64
	AvoidedGapSec int64
	ReasonCodes   []ResetScheduleReason
}
```

Statuses include `due_now`, `scheduled`, `deferred`, `not_yet_useful`, and
`unsupported`. Reason codes identify gap avoidance, expiry pressure, natural
reset interaction, waste reduction, and low-confidence fallback.

TTY groups schedule rows chronologically and prints account email, credit
title or ID, recommended time, deadline, estimated restoration, and primary
reason. JSON includes full credit IDs and objective measurements. Neither form
contains tokens, credential revisions, candidate IDs, or internal account
keys.

## Consume retry safety

Optional credit selection makes cross-process retry state necessary. Consider
a POST that succeeds remotely but times out locally. A new default lookup may
hide redeemed credit and choose next expiry, consuming two credits.

CQ stores one pending file per account/credit under:

```text
<cache-dir>/reset-attempts-v1/<sha256(account-key + NUL + credit-id)>.json
```

Directory mode is `0o700`; file mode is `0o600`. File contains schema version,
opaque account key, credit ID, idempotency key, and start time. It contains no
credential material or account metadata. Creation and replacement use
exclusive create or write-to-temporary plus atomic rename as appropriate.
Independent per-credit files avoid lost updates between concurrent accounts.

Idempotency key is `cq-reset-v1-` followed by the hexadecimal SHA-256 digest of
account key, a NUL separator, and credit ID. Concurrent processes targeting
same credit therefore send same key. Pending file records selection before
POST and lets account-default invocation resume same credit after process exit.
If CQ cannot persist or verify that file, it fails before POST.

Terminal `reset`, `already_redeemed`, `nothing_to_reset`, or `no_credit`
removes matching pending file. Timeout, transport failure, HTTP `5xx`, or
malformed success response retains it and returns `consume_indeterminate` with
exact retry command. Failure to remove a terminal attempt produces a warning;
same-key replay remains safe. No cleanup removes another account or credit's
file.

After `reset` or `already_redeemed`, CQ invalidates Codex quota cache and
refetches account usage. Successful refetch updates history and reports changed
windows. Refetch failure is warning after successful mutation, never a failed
consume outcome.

## Burn-history correction

Current history logic treats higher remaining percentage with unchanged reset
epoch as negative consumption, censors delta to zero, and then blends a zero
rate sample into EWMA. That weakens future forecasts after a banked reset.

When current remaining percentage exceeds previous remaining percentage and
reset epoch is unchanged, history must:

- preserve existing EWMA rate;
- preserve sample count;
- update last-seen time, remaining percentage, and reset epoch;
- avoid producing a zero-rate sample.

This rule is also correct for benign upward provider corrections: neither a
banked reset nor correction proves demand fell to zero. Natural reset unwrapping
continues to require reset-epoch advancement by a clean window-period multiple.

## Failure handling

Stable reset error codes include:

- `account_reference_invalid`
- `credential_unavailable`
- `auth_expired`
- `credits_unavailable`
- `credit_not_found`
- `credit_expired`
- `unsupported_reset_type`
- `recommendation_incomplete`
- `consume_indeterminate`
- `consume_failed`

`list` is best effort across accounts and exposes per-account errors.
`recommend` requires complete fresh account, usage, and credit inventory. Any
missing account returns `complete=false`, lists blockers, emits no actionable
portfolio times, and exits non-zero. Approximate search with complete inputs
remains actionable, exits zero, and labels `exact=false`.

`use` requires only target account. Unknown or unsupported default credit fails
before confirmation. Explicit manual selection never bypasses account identity,
expiry, reset-type, or credential checks.

All HTTP errors exclude token and raw credential data. Context cancellation is
preserved. Every goroutine calling external code has panic recovery. Response
bodies use existing 1 MiB bound.

## Package ownership

- `internal/provider/codex/reset_credits.go` owns upstream transport, DTOs,
  parsing, and consume outcomes.
- `internal/provider/codex/reset_accounts.go` owns visible-account projection,
  reference resolution, candidate planning, and exact secret resolution.
- `internal/provider/codex/reset_attempts.go` owns pending per-credit attempt
  files and deterministic idempotency.
- `internal/aggregate/reset_schedule.go` owns pure portfolio simulation,
  objective comparison, dominance, and beam search. It reuses aggregate quota,
  multiplier, weekly-gating, gap, and waste semantics rather than duplicating
  them in CLI or provider code.
- `internal/history/store.go` owns same-epoch upward-observation handling and
  richer rate-estimate metadata.
- `internal/app/codex_resets.go` owns fresh orchestration and complete-plan
  gating.
- `cmd/cq/codex_resets.go` owns arguments, confirmation, and TTY/JSON command
  output. `cmd/cq/main.go` and `cmd/cq/help.go` expose command tree and help.

## Verification

Transport tests prove:

- exact upstream paths, headers, request body, timeout, and body limit;
- details parsing, expiry ordering, unknown reset types, and all outcomes;
- authentication failures never expose credential material.

Credential tests prove:

- every account row source can be targeted without changing active auth;
- exact generation and identity fences survive stale inventory relist;
- only eligible CQ-owned managed credentials refresh;
- system, borrowed, exported, external, legacy, and uncertain credentials do
  not refresh or mutate.

History tests prove:

- same-epoch upward percentage preserves EWMA and samples;
- ordinary consumption still updates EWMA;
- natural reset still unwraps only clean epoch advances;
- exhausted-zero censoring remains unchanged.

Optimiser tests prove:

- three accounts with same-day expiries receive staggered schedules when their
  burn and natural resets differ;
- banked reset restores percentages without changing reset epochs;
- 5-hour and 7-day capacity, weekly gating, natural resets, and reallocation
  interact correctly;
- every credit able to restore at least one point is scheduled;
- zero-restoration credits defer until useful or deadline;
- no credit is scheduled after expiry or more than once;
- selected objective never ranks worse than no-reset baseline;
- dominance pruning and beam truncation are deterministic;
- exact versus approximate status is correct;
- missing account data prevents actionable optimality claim.

Consume tests prove:

- omitted or matching explicit credit resumes pending attempt;
- differing explicit credit is rejected while an attempt remains unresolved;
- explicit credit overrides normal expiry ordering when no attempt is pending;
- omitted credit chooses next expiry and non-expiring credit last;
- indeterminate result retains attempt and retry uses same credit and key;
- concurrent same-credit calls share idempotency;
- terminal outcomes clear only matching attempt;
- successful consume invalidates cache and attempts fresh usage refetch;
- confirmation defaults to No and `--yes` is explicit.

CLI tests cover parsing, manual help, TTY output, JSON output, reason/error codes,
and non-zero incomplete recommendation. Repository gates remain:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

## Acceptance criteria

- `cq codex resets recommend` produces one complete portfolio schedule from
  fresh data for all visible Codex accounts.
- Same-day credits can receive distinct times based on forecasted joint usage.
- Every useful supported credit is scheduled, but no command consumes it.
- `cq codex resets use ACCOUNT` selects account's next expiring available
  supported credit unless resuming pending attempt.
- `--credit` selects another credit explicitly when no different attempt is
  unresolved.
- Banked reset simulation and history never move natural reset dates.
- Consumption never switches active Codex auth or broadens refresh authority.
- Indeterminate retry cannot consume next credit accidentally.
- Incomplete data never produces actionable timing or an optimality claim.
- Focused tests and full race, vet, and build gates pass.
