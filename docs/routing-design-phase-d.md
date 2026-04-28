# Phase D Routing Design

## Evidence Source

- Diagnostics log: `~/Library/Logs/cq/routes-v0.18.0-uat.jsonl`
- Corpus marker: `~/Library/Logs/cq/routes-v0.18.0-uat.start` (manually created with `date -u` to record the start of collection before UAT traffic)
- Collection window: 2026-04-28T11:10:56.349183Z to 2026-04-28T11:22:22.404814Z
- cq version: `0.18.0`
- Runtime: Homebrew service, restarted after adding `diagnostics_log` because the field is read at proxy startup.
- Diagnostics file mode: `0600` (`-rw-------`).

Safety validation commands used:

```bash
jq . "$DIAG_LOG" >/dev/null
jq -r '.route_kind // "(missing)"' "$DIAG_LOG" | sort | uniq -c | sort -rn
jq -r 'select(.account_hint != null and .account_hint != "") | .account_hint' "$DIAG_LOG" \
  | grep -vE '^(claude|codex):[0-9a-f]{12}$'
grep -E 'Bearer|sk-|oauth|refresh_token|access_token|local_token|@' "$DIAG_LOG" || true
jq 'select((.latency_ms // 0) < 0)' "$DIAG_LOG"
jq 'select((.status_code // 0) < 0)' "$DIAG_LOG"
```

Results: JSONL parsed successfully, the account-hint format check produced zero invalid lines, no broad credential-leak patterns were found, and no negative latency or status values were present.

## Corpus Coverage

Route-kind distribution:

```text
 288 anthropic_messages
   7 codex_native
   5 anthropic_count_tokens
   2 health
   2 codex_compact
   1 codex_legacy_websocket
   1 codex_app_server
```

Covered route kinds:

- `health`
- `anthropic_messages`
- `anthropic_count_tokens`
- `codex_native`
- `codex_compact` (emitted from `internal/proxy/codex_compact.go`)
- `codex_legacy_websocket`
- `codex_app_server`

Not observed:

- No known route kinds remained unobserved in the refreshed corpus. Some hard-to-trigger route kinds were covered by later client traffic while diagnostics remained enabled.

Notes:

- Real CLI usage produced `health`, `anthropic_messages`, `anthropic_count_tokens`, `codex_native`, and `codex_legacy_websocket` traffic.
- Minimal local proxy requests were used to cover `codex_native`, `codex_compact`, and `codex_app_server` where the live clients did not naturally exercise those paths.
- Synthetic route-coverage requests generated expected invalid/request-shape responses for some paths and should not be interpreted as product failures.

Provider/model distribution:

```text
 115 codex  anthropic_messages      gpt-5.5
  87 claude anthropic_messages
  59 claude anthropic_messages      claude-sonnet-4-6
  27 claude anthropic_messages      claude-opus-4-7
   6 codex  codex_native
   5 codex  anthropic_count_tokens  gpt-5.5
   2 proxy  health
   2 codex  codex_compact           gpt-5.5
   1 codex  codex_legacy_websocket
   1 codex  codex_native            gpt-5.5
   1 codex  codex_app_server
```

Latency by route kind:

```text
anthropic_count_tokens count=5 avg=357.2 max=1094
anthropic_messages count=288 avg=6837.2 max=117481
codex_app_server count=1 avg=0.0 max=0
codex_compact count=2 avg=77451.5 max=154813
codex_legacy_websocket count=1 avg=469782.0 max=469782
codex_native count=7 avg=22437.6 max=98836
health count=2 avg=102.5 max=105
```

The higher `codex_native`, `codex_compact`, and `codex_legacy_websocket` maxima came from live Codex/client traffic and should be treated as small-sample observations, not routing-policy conclusions. The zero-millisecond `codex_app_server` latency came from an immediately rejected non-upgrade request, so it should not be compared with full upstream requests.

## Observed Request Boundaries

Observed traffic arrived in bursts over roughly 686 seconds. Minute clustering was:

```text
   4 2026-04-28T11:10
  19 2026-04-28T11:11
  22 2026-04-28T11:12
  15 2026-04-28T11:13
  24 2026-04-28T11:14
  43 2026-04-28T11:15
  40 2026-04-28T11:16
  24 2026-04-28T11:17
  33 2026-04-28T11:18
  15 2026-04-28T11:19
  22 2026-04-28T11:20
  31 2026-04-28T11:21
  14 2026-04-28T11:22
```

Sorted events showed many idle gaps over five seconds, ranging from about 5.0s to 37.7s before the next request. This suggests an idle-gap heuristic could identify coarse bursts, but the corpus still does not justify a threshold because long model latency and active client sessions can also create similar gaps.

Candidate boundaries evaluated against the corpus:

- End of a full `/v1/messages` response: plausible. Each `anthropic_messages` event has final status/latency and can act as a safe point after a completed request.
- `count_tokens` immediately followed by `messages`: observed for Codex-routed Anthropic-compatible traffic. These likely belong to the same request burst and should not independently trigger account rebalance before the following message request.
- End of a Codex app-server/WebSocket session: partially confirmed. One `codex_legacy_websocket` event was observed with long latency, but only one event is insufficient to define close-boundary policy. `codex_app_server` was still covered only by a non-upgrade request that produced `websocket_upgrade_required`.
- Compaction boundaries: two `codex_compact` requests were observed. They remain plausible boundaries because compaction relates to conversation summarisation, but the small count does not prove whether compaction should end or continue stickiness.
- Idle gap between bursts: promising for coarse session segmentation, but sensitive to streaming duration, client retries, and concurrent requests.

The diagnostics log is JSONL-safe but request events are not guaranteed to be written in chronological order under concurrent traffic. Any analysis or future helper should sort by `time` before inferring adjacency.

## Account-Hint and Failover Findings

Distinct account hints:

- Claude: 1 (`claude:6697888dcf5a`)
- Codex: 1 (`codex:de879f4afc23`)
- Proxy/health: 0

Account-hint distribution:

```text
 115 anthropic_messages     codex  codex:de879f4afc23
  86 anthropic_messages     claude claude:6697888dcf5a
   7 codex_native           codex  codex:de879f4afc23
   5 anthropic_count_tokens codex  codex:de879f4afc23
   2 codex_compact          codex  codex:de879f4afc23
   1 codex_legacy_websocket codex  codex:de879f4afc23
```

No account-hint churn was observed within either provider because the corpus selected only one usable Claude account hint and one Codex account hint. That means the corpus supports privacy/format validation, but does not answer how often real multi-account selection changes inside longer bursts.

Failover was not observed naturally. This is acceptable for the initial corpus because credentials/quota failures were not deliberately forced.

`pin_active` behaved as expected at the event level: pinned Claude requests showed `pin_active: true` and the unpinned request showed `pin_active` absent/null. Some invalid-proxy-token synthetic requests also reported `pin_active: true` while no account was selected because `PinActive` reflects server pin state before auth validation, not proof that the pin was applied to a handled request; Phase D should pair it with `account_hint` and terminal status context.

Error summary:

```text
  87 authentication_error:invalid_proxy_token
   6 api_error:codex_upstream_error
   1 invalid_request_error:websocket_upgrade_required
```

The `invalid_proxy_token` events came from local non-production/synthetic request paths and demonstrate safe error-code logging. The Codex upstream errors were redacted safe error codes, and the app-server route produced the expected safe WebSocket-upgrade error code for a non-upgrade request.

## Candidate Natural Boundaries

1. Completed provider request
   - Treat successful or terminal `/v1/messages`, `/responses`, and compact responses as safe points for changing account selection.
   - Do not change account mid-stream.

2. Explicit route-kind boundaries
   - `codex_compact` may indicate a natural summarisation boundary.
   - `codex_app_server`/`codex_legacy_websocket` should be treated as session-like once a successful upgrade/close can be observed.

3. Idle-gap boundary
   - Candidate only after more data.
   - The current corpus shows multiple gaps above five seconds, but not enough diversity to distinguish normal model latency from a true user/session boundary.

4. Model-change boundary
   - The corpus includes `gpt-5.5`, `claude-sonnet-4-6`, and `claude-opus-4-7` events.
   - Model changes can indicate a new request class, but the same broad time window included concurrent Codex and Claude traffic, so model changes alone should not force a boundary.

## Candidate Session Signals

- Idle-gap heuristic: useful as a fallback for clients that do not identify sessions, but only after sorting by event time and choosing a conservative threshold from a larger corpus.
- Explicit future header such as `X-CQ-Session-ID`: strongest option when clients can supply it. It would avoid guessing from timing and model names, but requires client support and privacy review.
- Model-change boundary: weak signal. It may help terminate stickiness when switching between materially different models, but should not be the primary session key.
- Route-kind boundary: strong for known session lifecycle routes once natural `codex_app_server`, `codex_legacy_websocket`, and `codex_compact` coverage is available.
- Account hint continuity: useful for analysing selector behaviour, but current `account_hint` is last-selected-account only and cannot reconstruct attempted failover sequences.

## RouteEvent Gaps for Phase D

Only add fields after a Phase D implementation plan justifies them. The current corpus suggests these possible additions:

- `request_id`: would let analysis correlate client preflight/main requests and sort/reconstruct concurrent event sequences without relying only on timestamps.
- `session_hint`: only if clients can provide an explicit session identifier, for example via `X-CQ-Session-ID`.
- `stream_complete`: would distinguish completed streaming responses from early termination when natural-boundary routing depends on full response completion.
- `websocket_close_code`: would make app-server/WebSocket session boundaries visible without logging payloads.
- `failover_count`: would improve policy analysis beyond the current boolean while avoiding raw attempted-account sequences.
- Attempted-account summary: if needed, log only redacted hints and bounded counts; do not log full IDs or tokens.
- Quota snapshot age/min remaining at selection time: useful for explaining selector choices, but must avoid leaking sensitive account metadata.

Do not add raw request bodies, response bodies, bearer tokens, local proxy tokens, OAuth refresh tokens, API keys, full emails, full account UUIDs, or full credential/account secrets.

## Proposed Routing Policy Changes

These proposals are design-only output from this UAT phase. No proxy source changes or routing-policy changes are authorised until a separate implementation plan is approved.

### Proposal: Manual pin overrides first

**Evidence:** Pinned Claude events showed `pin_active: true` with a redacted Claude account hint on successful requests. Unpinned events showed `pin_active` absent/null.

**Behaviour:** If a manual pin is active and the pinned account is usable, route eligible Claude traffic to that account before quota balancing or session stickiness. Existing failover behaviour still applies for auth/quota emergencies.

**Files likely touched:**

- `internal/proxy/router.go`
- `internal/proxy/transport.go`
- `internal/proxy/server.go`
- `cmd/cq/proxy.go`
- `internal/proxy/server_test.go`

**Tests required:** Unit tests for pinned selection precedence, integration tests for diagnostics `pin_active`, and UAT with one pinned and one unpinned Claude request. Assertions that prove a pin handled a request must pair `pin_active` with a valid `account_hint` and a 2xx status.

**Risks:** User surprise if a stale pin silently overrides balancing, cache affinity if pinned and unpinned flows interleave, and privacy if diagnostics over-explain pin identity.

### Proposal: Emergency failover on auth/quota errors

**Evidence:** No natural failover occurred, but diagnostics already provide safe error codes and the corpus confirmed no credential leakage in error fields.

**Behaviour:** Preserve immediate failover on auth/quota errors even when a session or natural-boundary policy would otherwise prefer stickiness. Diagnostics should record that failover occurred without logging raw attempted credentials.

**Files likely touched:**

- `internal/proxy/transport.go`
- `internal/proxy/codex_transport.go`
- `internal/proxy/diag.go`
- `internal/proxy/server_test.go`

**Tests required:** Existing 401/429 failover tests should be extended to assert natural-boundary/session stickiness does not block emergency failover, plus diagnostics tests for `failover` and future `failover_count` if added.

**Risks:** Cache affinity breaks on emergency failover, stale quota state may cause repeated retries, and richer failover diagnostics could reveal account topology if not bounded/redacted.

### Proposal: Quota balancing only at natural boundaries

**Evidence:** The corpus shows many adjacent `anthropic_messages` events within short bursts and only one account hint per provider. It does not show harmful churn, but it does show enough burstiness that mid-stream or mid-session switching would be difficult to reason about without explicit boundaries.

**Behaviour:** Continue selecting by current quota/headroom semantics, but only re-run balancing when no active session/boundary guard applies. Completed message/native/compact requests and conservative idle gaps are candidate boundaries.

**Files likely touched:**

- `internal/proxy/router.go`
- `internal/proxy/server.go`
- `internal/proxy/transport.go`
- `internal/proxy/codex_transport.go`

**Tests required:** Unit tests for boundary detection, selector tests showing no mid-session rebalance, streaming tests ensuring selection is fixed for a response, and UAT comparing account hints across bursts.

**Risks:** Sticky routing can underuse available quota, idle-gap thresholds may be wrong for slow models, and concurrent requests can blur boundary inference unless events are sorted/correlated.

### Proposal: Cache/session stickiness for ongoing conversations when a reliable signal exists

**Evidence:** The corpus cannot infer true conversation identity from timing alone. It did show that mixed Claude/Codex events and model switches can occur in the same small time window, so timing/model alone are weak session identifiers.

**Behaviour:** Prefer an explicit session signal if available. Use timing or route-kind heuristics only as conservative fallback. Once a session is assigned to an account, keep routing that session to the same account until a natural boundary or emergency failover.

**Files likely touched:**

- `internal/proxy/router.go`
- `internal/proxy/server.go`
- `internal/proxy/config.go` only if a later product decision adds configuration
- `internal/proxy/server_test.go`

**Tests required:** Tests for explicit session header parsing if added, cache expiry/idle-gap tests, concurrent-session isolation tests, and privacy tests ensuring session IDs are not logged raw if sensitive.

**Risks:** Session identifiers may be unavailable, client-provided IDs may be sensitive, sticky cache state can become stale, and users may be surprised if balancing appears less responsive.

### Proposal: Preserve stale/unknown quota eligibility semantics

**Evidence:** The corpus includes successful Codex and Claude traffic with stable account hints, but no failover and no multi-account churn. It does not justify tightening eligibility for stale/unknown quota accounts.

**Behaviour:** Keep current selector semantics for stale or unknown quota data during Phase D. Natural-boundary routing should constrain when selection changes, not redefine which accounts are eligible.

**Files likely touched:**

- `internal/proxy/router.go`
- `internal/proxy/codex_selector.go`
- `internal/proxy/transport.go`
- `internal/proxy/codex_transport.go`

**Tests required:** Regression tests for stale/unknown quota eligibility, boundary tests proving existing selector results are reused only while sticky, and UAT with fresh and stale quota cache states.

**Risks:** Stale quota can over-select a poor account, while over-tightening can strand usable accounts. Diagnostics may need quota snapshot age to explain decisions.

## Non-Goals for Phase D

- Do not log request bodies or response bodies.
- Do not log raw bearer tokens, local proxy tokens, OAuth refresh tokens, API keys, full emails, full account UUIDs, or credential/account secrets.
- Do not use `pin_active` alone as proof that an account handled a request; require an account hint and successful/terminal status context.
- Do not infer full failover attempt order from the current `failover` boolean.
- Do not make route changes in the diagnostics/UAT phase.
- Do not hot-reload `diagnostics_log` as part of routing design.

## Open Questions

- Do Claude Code clients issue `anthropic_count_tokens` consistently across versions/settings, and should observed count-token/message pairs always share one routing decision?
- What natural app-server/WebSocket lifecycle events are visible during a real Codex interactive session?
- Can Claude Code, Codex CLI, or other clients provide a stable, non-sensitive explicit session ID header?
- What idle-gap threshold separates a user/session boundary from normal streaming/model latency?
- How often do account hints churn in longer multi-account Claude corpora when quota pressure is real?
- What should diagnostics expose for failover depth: boolean, count, redacted attempted-account hints, or only final outcome?
- Should session stickiness be per provider, per route kind, per model, or per explicit client session?
