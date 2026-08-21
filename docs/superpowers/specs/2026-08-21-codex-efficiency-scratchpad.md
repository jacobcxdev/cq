# Codex Efficiency Scratchpad

**Status:** Living research and delivery record. Non-normative until a phase is
promoted into an approved implementation plan.

**Started:** 2026-08-21

**Worktree:** `.worktrees/codex-efficiency-phase-0`

**Branch:** `jacobcxdev/feat/codex-efficiency-phase-0`

**Base:** `origin/main` at `dd03042` (`v0.23.7`)

## Purpose

Reduce Codex subscription usage per accepted outcome without hiding effective
model, effort, provider, account, or goal state from CLI and Desktop users.

This file preserves research that would otherwise exist only in one Codex
task. Update it when evidence, decisions, implementation checkpoints, or
parked work change. Do not store prompts, credentials, account identifiers,
tool outputs, cache keys, or raw task identifiers here.

## Success measure

Primary measure:

```text
normalised subscription credits / accepted capability
```

Guardrails:

- Product acceptance and escaped defects.
- Retry and user-correction rate.
- Elapsed and active time.
- User interventions per active hour.
- Runnable capabilities per active hour.
- Reviewer and subagent share of total usage.
- Out-of-scope edits and reverted work.

Token count alone is not success. A cheaper failed attempt is not a saving.

## Evidence ledger

### Measured locally

- Authenticated 2026-08-20 analytics: 2.554 billion text tokens and 46,723.47
  normalised credits.
- Approximate components: 2.461 billion cached input, 84.7 million uncached
  input, and 8.59 million output tokens. Input cache ratio was 96.67%.
- Local classification lower bound attributed 95.25% of billable tokens to
  GPT-5.6-Sol.
- Two accounts processed approximately 1.23 and 1.28 billion tokens. A third
  processed 44.6 million.
- Seventeen of seventeen strict, rapid Sol effort changes caused an immediate
  first-call cache regression. Median cache ratio changed from 98.53% on
  unchanged-effort boundaries to 6.95% after a switch.
- Strict lost-cache proxy: 1.91 million tokens and approximately 214.5 Sol
  credits. Broader thirty-minute proxy: 2.66 million tokens and approximately
  298.7 Sol credits.
- Cache generally recovered on the second or third recorded call.
- One investigated workload contained 47 waits, including 33 fixed polling
  waits, and 24 agent spawns. This proves amplification can become pathological;
  it does not establish a representative avoidable percentage.

Credits are rate-card equivalents, not API charges. Exact subscription quota
weighting remains undocumented.

### Source-confirmed mechanics

- Codex WebSocket incremental reuse requires matching model, reasoning object,
  tools, instructions, service tier, text configuration, cache key, and other
  request properties. An effort change therefore prevents use of
  `previous_response_id` for that request.
- Codex `/goal` continuation preserves the full objective, rejects narrowing,
  audits every named requirement, and treats uncertainty as a reason to keep
  working. Large workflow-heavy designs therefore amplify into programme-wide
  completion contracts.
- Native goal budgets count uncached input plus output at safe turn boundaries.
  Source shows no descendant or subagent aggregation.
- `fork_turns` supports `all`, `none`, and a bounded positive turn count. Full
  history is not free: each child has its own inference and tool loop.
- Codex 0.149 adds `[skills].max_context_tokens`; installed CLI and Desktop were
  still 0.148 when this research was performed.

### Public evidence

- Published pricing values were unchanged across archived official pages from
  2026-08-05 through 2026-08-20.
- Supplied Reddit sample contained at least thirty distinct firsthand reports
  after obvious duplicate removal, but no controlled backend A/B.
- No current OpenAI admission of an August entitlement reduction was found.
- Broad directory-hash or account-switch throttling remains unsupported by raw
  tests, staff confirmation, or reproducible evidence.

### Unresolved

- Server-side prompt-cache partitioning by effort is not exposed by the client.
- Whether an undocumented quota policy changed in August cannot be proved or
  excluded from public material.
- Durable cookie-free access to
  `/backend-api/wham/analytics/daily-workspace-usage-counts` is unproven. One
  bounded bearer-only probe returned HTTP 500. Browser cookies must not be
  copied into CQ.
- Warm return after effort `A -> B -> A` inside the published cache TTL lacks a
  valid controlled sample.

## Cost model

Relative token prices:

| Model | Relative price |
|---|---:|
| Sol | 1.00 |
| Terra | 0.40 |
| Luna | 0.04 |

For a 150,000-token prefix:

| Request | Credits |
|---|---:|
| Warm Sol | 1.875 |
| Cold Sol | 18.750 |
| Cold Terra | 7.500 |
| Cold Luna | 0.750 |

Model changes can remain cheaper despite losing continuity. Routing must
compare transition cost, expected remaining work, and correction risk rather
than preserving cache or selecting the cheapest model unconditionally.

Modelled, overlapping opportunity bands:

| Rank | Intervention | Conservative | Base | Upside | Confidence |
|---:|---|---:|---:|---:|---|
| 1 | Model mix | 10.4% | 35.4% | 56.5% | Medium |
| 2 | Fewer unnecessary turns | 5% | 15% | 30% | Low-medium |
| 3 | Tool and context reduction | 3.3% | 9.9% | 19.8% | Medium |
| 4 | Stable effort | 0.46% | 0.64% | 1.3% | High for measured range |
| 5 | Account affinity | Conditional | Conditional | Conditional | Medium |
| - | Pool reservation | 0% | 0% | 0% | Capacity protection only |

Do not sum these bands. Turn removal also removes context replay; effort and
account affinity overlap with cold-cache events; model mix multiplies remaining
volume.

## Ownership boundary

```text
CLI / Desktop
  classify before turn
  choose model and effort
  show desired and effective state
          |
Codex core / app-server
  context, tools, goals, subagents,
  persisted turn state and UI events
          |
CQ
  quota facts, account eligibility,
  pools, affinity, admission and route receipt
          |
OpenAI
```

CQ must not rewrite model or effort after Codex creates turn state. A classifier
may run behind a CQ service, but CLI or Desktop must call it before
`turn/start`. UI truth comes from Codex acknowledgement, not a proxy guess.

CQ owns:

- Quota and reset facts.
- Account eligibility and ranking.
- Account pools and admission policy.
- Cache-aware account affinity.
- Headroom compression.
- Privacy-safe routing telemetry and receipts.

Codex or its client owns:

- Prompt classification and task identity.
- Model, provider, effort, verbosity, and tool profile.
- Context lineage, compaction, forks, and subagents.
- Goal control, queued work, checkpoints, and user-facing effective state.

## Roadmap

### Phase 0 - trustworthy measurement

Status: implemented and verified locally; pending publication.

Dependencies present on `origin/main` through `dd03042`:

- Scoped persistent burn history by provider and account.
- Marked ordinary cache hits with measured cache age.
- Cold-started a clean history schema after contaminated prior observations.
- Landed the broker diagnostics allowlist dependency so WebSocket route events
  persist safely.

Delivered request-shape observations contain four new sparse, privacy-safe
fields. Existing `request_kind` and `route_kind` continue to carry validated
request kind and transport:

- `request_lineage`: exact `previous_response_id` key presence, including
  empty and null values. It is not proof of cache hit, full input, or
  incremental input.
- `requested_reasoning_effort`: closed values `none`, `minimal`, `low`,
  `medium`, `high`, `xhigh`, `max`, `ultra`, `unspecified`, and `unknown`.
- `requested_model_class`: persisted values `gpt_5_6_sol`, `gpt_5_6_terra`,
  and `gpt_5_6_luna` only when exact raw `gpt-5.6-sol`, `gpt-5.6-terra`, or
  `gpt-5.6-luna` model values match; otherwise `other` or `unknown`. This is
  separate from existing `Model` semantics.
- `compaction_phase`: exact validated values `standalone_turn`, `pre_turn`,
  `mid_turn`, `not_applicable`, and `unknown`.

HTTP enriches one existing terminal route event. Each successfully parsed,
accepted WebSocket request frame emits one `codex_websocket_frame` event.
Malformed, control, response, and upstream frames emit none. Retries and
account/upstream rotations emit no additional event.
No raw prompt, tool, schema, output, response ID, model string, session value,
or correlation is persisted. Manifest sizes and digests remain deferred because
they are not authoritatively available in this boundary without broader scope.

No cache-hit, full/incremental, directory-hash, or account-switch inference is
made. Unsupported facts remain absent or unknown.

### Phase 1 - controlled no-code experiments

- Freeze model, effort, verbosity, and tool profile for each task epoch.
- Compare Sol, Terra, and Luna by accepted capability, not raw turn.
- Trial lower subagent concurrency separately from model changes.
- Use thin forks for self-contained work and full history only where required.
- Trial native goal budgets as parent-loop circuit breakers.
- After upgrading to Codex 0.149, trial a 1,000-2,000-token skill catalogue cap
  with skill-selection accuracy tests.

Change one variable per cohort.

### Phase 2 - CQ shadow advice

- Explain route and exclusion reasons.
- Warn when request shape changes and continuity may be lost.
- Simulate model, account-pool, reserve, and reset-aware admission choices.
- Never change model, pool, or account based only on shadow advice.

### Phase 3 - suggestion-only classifier

- Classifier returns semantic class, confidence, and bounded reason codes.
- Deterministic policy validates final model and effort against runtime
  `model/list`.
- Initial mapping: Luna Low for clear repeatable work, Terra Medium for ordinary
  multi-step work, Sol High or XHigh for ambiguous high-value work.
- Luna uses supported ChatGPT subscription authentication, not API credits.
- Apple Foundation Models is an optional macOS classifier. Luna or Antigravity
  supplies cross-platform fallback.
- Manual selection wins until explicitly unlocked.
- UI shows suggested state before dispatch and acknowledged effective state
  afterwards.

### Phase 4 - automatic routing

Gate on at least twenty to thirty labelled tasks with accepted-output, retry,
latency, and credit evidence. Keep decisions sticky for a task tree, permit one
bounded upshift before a later turn, and never replay a side-effecting turn.

### Phase 5 - bounded autonomous goals

First evaluate eight to ten representative goals using product-only contracts,
one runnable capability in progress, one coherent review, and a native parent
budget.

If repeated runaway remains, smallest honest app-server watchdog may pause at a
safe boundary on one proven predicate: active-time ceiling or CQ quota floor.
Do not build a generic tool counter, line budget, heartbeat protocol, automatic
completion detector, or second harness.

Longer-term controller shape, if evidence warrants it:

- Keep whole-product outcome and authority in controller.
- Dispatch one short-lived worker from an exact clean commit.
- Require one runnable outcome, explicit non-goals, focused acceptance, and a
  cost/time ceiling.
- Accept a reviewed clean commit or discard work.
- Persist accepted commit, checks, remaining acceptance gap, and next
  capability outside task transcript.
- Interrupt user only for product policy, destructive/data-risk action,
  credentials/publication, or irreconcilable authority conflict.

### Phase 6 - upstream candidates

- Spawn-subtree token budgets.
- Event-driven waits and no-change suppression.
- Thin-fork defaults.
- Lazy tool and skill loading.
- Model-callable pause requests.
- Explicit prompt-cache breakpoints.
- Effective child model and effort visibility.

## Parked work

### Account pools and project bindings

Reason parked: capacity protection, not direct token efficiency. Detailed design
must not displace Phase 0 measurement.

Current source already includes pool CRUD and session bind/unbind CLI. Earlier
claims that this operator surface was absent, or that explicit pools required
capability evidence, came from stale analysis and are rejected.

Remaining candidate design:

- User-defined pool names only; UI reflects configured name.
- Manual task-tree binding overrides project path.
- Longest clean project-root prefix selects pool for new unbound task trees.
- Existing bindings remain frozen when rules change.
- Pool filters candidate accounts; continuity, warm affinity, then quota ranking
  select an account.
- Spend pool and Trusted Access capability remain orthogonal constraints.
- Backend cyber denial must not trigger silent replay. Offer a new eligible task
  after recognised denial until pre-admission classification is proven.
- CQ returns effective pool, source, generation, state, account alias, and route
  reason. CLI and Desktop render receipt rather than guessing locally.

Open design question: exact Codex core handshake carrying initial project root,
task/thread identity, and wire session identity before first turn.

### Third-party execution providers

Technically feasible through custom Codex providers, but parked until classifier
value is demonstrated. Provider changes are thread-scoped and raise context,
tool, authentication, and UI-truth costs. Do not build another harness.

### Headless workspace analytics

Adjacent `/wham/usage` works with CQ-managed bearer plus account header.
Daily workspace analytics used a different browser OAuth client/scopes and was
co-present with cookies. Next step, if required, is a synthetic read-only client
and one-variable auth probe matrix. Never copy browser cookies or retry all
accounts blindly.

## Decision log

| Date | Decision | Reason |
|---|---|---|
| 2026-08-21 | Preserve research in this tracked scratchpad. | Task-only state is not durable programme state. |
| 2026-08-21 | Start Phase 0 from `origin/main` at `e158593`. | Burn-history fixes and the broker diagnostics allowlist fix already landed; avoids duplicating `Quota fixes` work. |
| 2026-08-21 | Keep CQ as quota and admission plane. | Downstream model rewriting would make Codex state and UI dishonest. |
| 2026-08-21 | Park detailed pool design. | Pools protect capacity but do not reduce credits directly. |
| 2026-08-21 | Require measurement before automatic classification. | Quality and retry costs can erase nominal model-price savings. |
| 2026-08-21 | Record only four sparse request-shape fields. | CQ receives authoritative protocol metadata at transport boundaries; prompt/body correlation and inferred cache state would exceed the evidence boundary. |
| 2026-08-21 | Emit one frame observation per successfully parsed accepted WebSocket request. | A long-lived socket must not collapse multiple turns into one last-frame snapshot; retries, rotations, and non-request frames are not requests. |
| 2026-08-21 | Keep `request_lineage` factual and preserve existing `Model`. | Key presence is observable; cache/full/incremental state and requested model class are separate claims. |
| 2026-08-21 | Defer manifest sizes and digests. | They are not authoritatively available at this boundary without inspecting or broadening protocol state. |
| 2026-08-21 | Preserve Stage 11 observe-mode event expectations and lifecycle transcript hashes. | The test-only fallout fix covers one legacy connection event plus one frame event per accepted request without changing sealed transcript evidence. |
| 2026-08-21 | Rebase Phase 0 cleanly onto current `origin/main` and `v0.23.7`. | Rewritten implementation history remains equivalent while verification runs against current main. |

## Delivery checkpoints

| Date | Commit | Outcome | Verification | Remaining gap |
|---|---|---|---|---|
| 2026-08-21 | `100f49e` | Provider-scoped burn history and valid cache-age inputs landed on `origin/main`. | Existing commit and full branch baseline inspected. | Privacy-safe request-shape telemetry. |
| 2026-08-21 | `002ff66..d89b18d` | Added privacy-safe request-shape classification and HTTP/WebSocket route observations, plus the Stage 11 test-only fallout fix after rebasing onto current main. | `gofmt` clean; `go vet ./...`; `go build ./...`; `go test -race -count=1 ./...` passed, with `internal/proxy` completing in 243.540s; `git diff --check` passed. | Publication only. |

## Primary references

- [Codex models](https://developers.openai.com/codex/models/)
- [Codex authentication](https://developers.openai.com/codex/auth/)
- [Codex app-server](https://developers.openai.com/codex/app-server/)
- [Codex configuration](https://developers.openai.com/codex/config-reference/)
- [Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [Codex 0.149 release](https://github.com/openai/codex/releases/tag/rust-v0.149.0)
- [Archived pricing, 2026-08-05](https://web.archive.org/web/20260805151040id_/https://learn.chatgpt.com/docs/pricing)
- [Archived pricing, 2026-08-20](https://web.archive.org/web/20260820072122id_/https://learn.chatgpt.com/docs/pricing)
