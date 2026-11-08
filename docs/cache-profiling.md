# Cache profiling

CQ records private cache profiles by default for routed inference. Run:

```sh
cq proxy cache --since 1h
cq proxy cache --since 24h --json
cq proxy cache --events --json
cq proxy cache --session REPORTED_SESSION_HASH --events --json
```

These commands read existing files. They do not contact a provider, warm a cache,
spend quota, change profiles or restart the proxy. `--log /absolute/path` selects
an isolated capture. The default period is 24 hours.

## What is measured

Provider-reported input, cached input, cache-write and output tokens are kept
separate from structural prefix measurements. Hit rate is total cached input
divided by total input across requests with known cache counters; it is not an
average of per-request percentages. Missing counters remain unknown. Anthropic
input is normalised to include uncached input, cache reads and cache writes.

Ordered keyed fingerprints identify configuration, tools, system instructions,
message boundaries and content blocks. Long text uses ordered UTF-8 chunks of
at most 4 KiB, so editing its tail can retain measured earlier overlap.
Comparisons report reusable canonical
**bytes**, not token estimates or a promise that a provider cached those bytes.
Provider cache eligibility may end at different message or block boundaries.
Account, model, session and cache partition changes start separate comparisons.
Keys change with the proxy process, so a restart starts a new observation period.
`cold_start` means no earlier comparison observation exists; it does not assert
that the provider missed its cache.

Profiles contain no prompt text, tool names, credentials or raw account/session
identifiers. They live in the platform log root under
`cache-profiles/requests.jsonl`, with an 8 MiB target and two rotated files.
The in-memory comparison history is bounded. Set
`cache_profiling_disabled:true` in proxy configuration to opt out, then activate
the configuration through the normal service lifecycle.

## Reading reductions

A changed early prefix can invalidate reuse of a large unchanged conversation.
Profiles identify the first changed component. Observed cached-token decreases
within the same known account, model, route and cache partition are flagged
separately, including opaque continuations where prefix comparison is unavailable.
These flags do not establish a regression: provider retention, a shorter input
and concurrent traffic can explain them. A short new request,
an explicit curation change, account/model switch or compaction can legitimately
reduce a hit percentage. Raw percentages alone cannot attribute a regression.

JSON/SSE usage is observed without changing forwarded bytes. Native Codex
WebSocket requests are also observed. Opaque incremental continuations,
unsupported formats, limits and absent cache counters are reported as incomplete
or unknown; they never establish successful cache reuse. The profiler does not
reconstruct provider-owned history or enable caching on a provider's behalf.

## Regression contracts

Required PR race tests run the shared profiler against append-only conversation
fixtures and real ZCode conversion. User follow-ups, tool continuations and new
runtime developer/system instructions must preserve the complete earlier content
prefix. The test rejects an unexplained loss and reports the changed component.
The historical converter that hoisted runtime reminders fails this contract.
The case matrix covers native GPT and converted GLM requests, warm and cold
usage observations, earlier user-message edits, developer reminders, goal
continuations, compaction and tool/skill changes. Cold cache state is an observed
provider counter, not something inferred from elapsed time or a new chat ID.

Compaction intentionally replaces history. Its fixture records an explicit,
case-specific justification. Any new exception or reduced reuse expectation must
include its reason and be reviewed with the implementation. Do not refresh a
baseline merely to make a failure pass. New inference protocols and conversion
paths need corresponding fixtures, including unknown usage and continuation
behaviour, before shipping.

Offline contracts catch changes CQ controls. They cannot certify provider cache
retention. Qualifying a real hit-rate improvement additionally requires a bounded
live before/after observation with the same model, account, route and workload.
