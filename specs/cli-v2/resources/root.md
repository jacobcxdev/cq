# Root, quota and shared service resource contracts

Normative CLI-v2 supplement. The sibling `../quota-annex-v1/manifest.json` identifies frozen source bytes and tests. Paths in this supplement resolve inside that annex. The annex remains the historical baseline. The user-authorised default-branch integration deltas in [`../default-branch-integration.md`](../default-branch-integration.md) supersede its exhaustion forecast arithmetic; window ordering, schema and sentinels remain authoritative. A mutable checkout reference is not a substitute for this recorded provenance. Quota resources intentionally retain version-1 field names, omission rules and numeric sentinels inside the version-2 CLI envelope. All other resources below include every declared field, using null where allowed. Unknown extra CLI resource fields are prohibited. JSON strings are valid UTF-8; numbers are finite. Arrays are arrays, never null, except the explicitly preserved quota v1 omission rules below.

## QuotaReportV1

Object with `generated_at` (RFC3339Nano UTC timestamp) and `providers` (ProviderQuotaV1 array, selected argument order; default claude,codex,gemini). This complete type is the `data.report` of check. A usable observation means result.status is ok or exhausted; exhausted is not an operational failure. A cached successful row is usable, but fallback following a failed fresh fetch produces partial exit8 and a warning.

### ProviderQuotaV1

- `id`: enum claude|codex|gemini.
- `name`: matching literal Claude|Codex|Gemini.
- `availability`: QuotaAvailabilityV1.
- `results`: array<QuotaAccountV1>, ordered by account_id then email bytewise, absent values last.
- `aggregate`: optional QuotaAggregateV1; omitted when fewer than two usable accounts or no aggregable windows.
- `proxy_eligibility`: optional QuotaEligibilityV1; omitted when no routing eligibility view was collected.
- `proxy_pools`: optional array<QuotaPoolV1>; omitted when empty, sorted by name bytewise. Only strict account subsets are included, preserving AddProxyPool semantics.

### QuotaAvailabilityV1

`state`: available|limited|exhausted; `guidance`: string from frozen availabilityForMargin/guidanceWithReset; `reason`: unavailable|unknown_quota|exhausted_quota|low_remaining_quota|healthy_quota; `min_remaining_pct`: integer -1..100; optional `resets_in_s`: positive integer seconds, omitted when zero/unknown. Availability selects the highest-ranked usable account (available > limited > exhausted), retaining first equal rank. Unknown quota gives available with min=-1; zero gives exhausted; 1..5 limited; 6..100 available. All error rows give exhausted/reason unavailable/min=-1. This legacy state label does not turn errors into confirmed quota depletion. Frozen reset-horizon calculation remains unchanged.

### QuotaAccountV1

`active`: boolean; `status`: ok|exhausted|error. Optional nonempty strings `account_id`, `email`, `plan`, `tier`, `rate_limit_tier`; omit each empty value. `error`: optional QuotaErrorV1, omitted if absent. `windows`: optional map<WindowNameV1,QuotaWindowV1>, omitted if empty. `cache_age_s`: optional positive integer age in seconds, omitted at zero. These are display/domain identifiers; account_id is not guaranteed to be the selectable AccountKey. No credentials, bearer tokens or raw credential files may be emitted.

QuotaErrorV1: required `code` nonempty string; optional `message` nonempty sanitised diagnostic; optional `http_status` integer100..599, omitted when unavailable/zero. Codes retain provider domain values (including not_configured, no_token, auth_expired, fetch_error, fetch_panic, api_error, parse_error); they are data, not the command-level exit taxonomy. Never echo provider bodies or credentials as messages.

QuotaWindowV1: required `remaining_pct` integer0..100; optional `remaining_pct_exact` finite number0..100 when supplied, including exact zero; optional `reset_at_unix` integer Unix seconds, omitted when zero. Percentage and pace presentation retain rounded remaining_pct. Exhaustion forecasts use valid remaining_pct_exact when available, as specified by the recorded default-branch integration delta.

WindowNameV1: nonempty string from a provider. Canonical duration windows use the largest exact whole unit among d,h,m,s, optionally `:BUCKET`; examples 5h,7d,7d:codex. Fixed Gemini keys are pro,flash,^lite. Other keys are preserved but not presumed aggregable. `internal/quota/constants.go` defines exact parsing, period bounds, bucket extraction and ordering: shared duration windows increasing period; scoped duration windows in the frozen bucket rank and period order; pro,flash,^lite; unknown keys sorted by base/bucket/key. A map has no semantic JSON key order; human output follows this ordering.

### QuotaAggregateV1

`provider_id`: provider enum; `kind`: weighted_pace|proxy_eligible_weighted_pace|proxy_pool_weighted_pace; `summary`: QuotaAccountSummaryV1; `windows`: nonempty map<WindowNameV1,QuotaAggregateWindowV1>.

QuotaAccountSummaryV1: `count` integer>=2, number of usable accounts; `total_multi` positive integer sum of extracted capacity multipliers; `label` string produced by frozen BuildLabel. Exact multiplier parsing is `internal/quota/multiplier.go`; it is not a user-supplied pool preservation value.

QuotaEligibilityV1: `discovered_count`, `eligible_count`, `excluded_count` nonnegative integers; discovered=eligible+excluded; optional `aggregate` QuotaAggregateV1, omitted if insufficient usable eligible rows. Eligibility may include discovered error rows because selection and usable quota are different tests. QuotaPoolV1 has `name` nonempty string plus the three counts and optional aggregate from QuotaEligibilityV1. Provider-wide metrics remain unchanged when proxy views are added. Eligibility and pool aggregates pass no gauge EWMA map to the compute function; report-only recent forecast annotations remain attached to their constituent windows.

QuotaAggregateWindowV1 fields:

| Field | Type and meaning |
|---|---|
| remaining_pct | required integer0..100, rounded weighted remaining percentage |
| expected_pct | required integer0..100, rounded weighted remaining percentage for linear pace |
| pace_diff | required integer-100..100, remaining_pct minus expected_pct in percentage points |
| burndown_s | optional positive integer, estimated seconds to depletion; zero omitted means exhausted or unavailable estimate, never proof of zero elapsed time |
| sustainability | optional finite number, legacy maximum sustainable rate multiplier; -1 unknown, 0 omitted, positive up to100; not the displayed gauge position |
| gauge_pos | required integer -1..6; -1 unknown, 0/1/2 severe/moderate/mild overburn, 3 on pace, 4/5/6 mild/moderate/severe underburn |
| gap_start_s | optional positive integer seconds until predicted first gap; omitted zero can mean immediate gap or no gap; interpret with gap_duration_s |
| gap_duration_s | optional positive integer duration of first projected uncovered interval; omitted zero means no positive gap recorded |
| wasted_pct | optional positive integer0..100, rounded timing-weighted projected waste percentage; omitted zero |
| waste_deadline_s | optional positive integer seconds to earliest wasting reset; omitted zero can mean immediate or unavailable |
| gauge_override | optional literal imminent_block; omitted when no override |

The v1 zero-value ambiguities above are retained deliberately for arithmetic/schema compatibility; callers must not infer new facts from omitted fields. The released exhaustion forecast correction is preserved by the default-branch integration delta. For current arithmetic see `internal/aggregate/aggregate.go`, `sustain.go`, `label.go`, `internal/history/store.go`, and their tests, subject to that delta. In particular: weekly depletion gates matching 5h scope; weighted values round using Go math.Round; missing reset fallback is one period from observation; burndown is weighted positive remaining capacity divided by weighted selected burn rate for qualifying ungated rows; each account selects the faster of valid recent and whole-window rates before weighting; gauge uses cumulative demand/supply with 0.95..1.05 on-pace band. All ungated accounts, including depleted accounts, contribute their capacity and observed burn to gauge supply and demand. Only non-depleted ungated accounts receive active drain allocation. A depleted Pro 20x account therefore contributes twenty times the capacity and observed demand of a depleted Plus account (the latter has multiplier 1); dropping depleted accounts would falsely imply spare headroom. Weekly depletion still gates matching 5h scope. EWMA affects imminent-block warning only, never rewrites the natural gauge position. Interval coverage and projected waste use frozen functions. No new CLI option changes these computations.

### Check exit precedence

Syntax failure occurs before discovery. Interrupt returns130; total deadline returns7 and retains completed report rows. Otherwise: usable rows plus any failed row other than `not_configured`, or stale fallback =>8/check_partial; no usable rows plus any authentication failure =>5/check_authentication; no usable rows plus any operational failure =>1/check_failed; no usable rows and only absent/unavailable integrations =>4/check_unavailable; usable rows with no failures other than absent `not_configured` integrations =>0. An HTTP 401 remains an authentication failure even if its error code is inconsistent. This preserves the default check behaviour when unused providers are not configured; their report rows remain present. Empty selected-provider result is unavailable, not successful empty work. Optional persistence failures are warning records, with their concrete failed cache/history operation named; they do not fabricate a failed upstream row. Bare cq invokes check without a deprecation warning.

## QuotaReportHumanV2

`cq` and `cq check` retain the established quota dashboard: provider icons, account headers with plan multipliers, percentage bars, relative reset times, pace and burndown, and capacity-weighted aggregate gauges. Active accounts appear first; remaining accounts retain canonical order. Additional quota buckets remain indented below their account or aggregate. Provider separators span rendered content; no terminal-width-dependent omission occurs. Relative times use the report observation timestamp. Proxy eligibility appears only when accounts are excluded; named pools retain their own blocks. This intentionally supersedes the initial raw field-by-field human projection at the user's request on 2026-09-23.

The existing `internal/output` TTY model and renderer define dashboard layout, icons, spacing, duration formatting and gauge presentation. User-provided labels escape C0/C1/DEL as literal backslash-u plus four lowercase hex digits before styling; normal Unicode remains intact. JSON retains every schema-2 field, canonical ordering and original label values, including metrics not shown individually in the dashboard. Exit codes, partial-result diagnostics and authentication semantics are unchanged.

## ProxyFactV2 and reconciliation

Fact resource fields: `name` enum inspector|desired|service|listener|process|runtime|data_plane; `state` enum known|absent|invalid|unavailable|permission_denied; `detail` string; `error_code` nonempty string|null; `value` the named value resource below or null. Exactly one fact of each name in this order. known requires value; all other states require null. absent has null error_code; invalid/unavailable/permission_denied have a nonempty safe error code. Detail is `not observed` for absent, error_code for other nonknown states, or compact JSON of value for known. This avoids an untyped object or ad-hoc prose as the only retained evidence.

Value schemas (all fields required, absent facts use value=null):

- InspectorFactValueV2: `executable`, `version`, `commit`, `digest`: string|null; executable absolute, digest SHA256 when present.
- DesiredFactValueV2: `manager` string|null, `configured` boolean, `listener` string|null.
- ServiceFactValueV2: `manager`, `state`, `executable` string|null; `pid` positive integer|null.
- ListenerFactValueV2: `state`, `listener`, `executable` string|null; `pid` positive integer|null.
- ProcessFactValueV2: `pid` positive integer, `executable` absolute string.
- RuntimeFactValueV2: `reachable` boolean; `pid` positive integer|null; `executable`, `health` string|null.
- DataPlaneFactValueV2: `proven` boolean, `code` string|null.

Provider/native manager and fact-state payload strings retain observed vocabulary; they are not CLI options. Reconcile with frozen `internal/proxy/proxy_snapshot.go` after converting null to original absent values. Map verdict healthy=>ready; legacy/degraded/conflicted=>degraded; indeterminate=>indeterminate; down=>absent if both desired configuration and service registration absent, else stopped. Conflict must retain its concrete invalid fact/error; ready never follows from HTTP health alone. `--strict` only changes exit policy. No evidence collector may create missing state. Candidate scope uses explicit candidate paths; live scope uses configured shared paths. Typed fact projection is an implementation delta, not a claim every current projection already exposes it.

## ServiceComponentV2

Required fields: `id` proxy|token-refresh; `manager` launchd|systemd|task-scheduler|null; `owner` cq|package|foreign|none; `installed` boolean; `enabled` boolean|null; `state` absent|running|idle|stopped|failed|indeterminate; `healthy` boolean|null; `pid` positive integer|null; `executable` absolute string|null; `last_run_at` UTC RFC3339 timestamp|null; `last_exit_code` integer|null; `error_code` nonempty string|null. Also required: `config_dir`, `state_dir`, `cache_dir`, `runtime_dir`, `log_dir`, each an absolute string|null obtained from the installed component authority. Null means unobserved; never substitute the invoking shell's roots. A component whose required root identity cannot be established is indeterminate.

`enabled` is durable automatic-start policy, not current process liveness. Unknown facts are null and produce indeterminate. Absent means no registration, installed=false, enabled=false, healthy=false, pid=null. Foreign ownership is never adopted. An installed proxy is healthy iff its configured owned process/listener is running and authenticated runtime health succeeds; this is service health, not full model acceptance. A token-refresh job is allowed idle between scheduled runs: healthy iff enabled, latest completed run succeeded, and its completion is no older than35 minutes. Use last_run_at as completion time. Running refresh keeps previous completed-run evidence; first run must finish before health can be true. Disabled token-refresh has healthy=false even if it last succeeded. No latest completed run means healthy=null until bounded install/start verification finishes; a failed last run means healthy=false. The 35-minute bound is the 30-minute schedule plus 5-minute grace and is a fixed CLI contract, not a new setting. Managers must retain last-run evidence; implementation must add safe reporting where absent today.

Output `components` follows proxy then token-refresh irrespective of execution order. Install/start verification waits for proxy health and one completed refresh job as applicable, within command timeout. Status --strict requires healthy=true for every selected component; it must not require periodic token-refresh process to remain running. Standard status may report stopped/idle/unhealthy with exit0 because inspection completed. `rollback` is not_needed when none attempted, restored only when prior selected registration/enabled state was verified restored, failed otherwise; no unselected component is included in a rollback. A stop/uninstall result reports healthy=false for stopped components without treating that intended outcome as operation failure. Restart of a disabled token-refresh component likewise succeeds when the newly requested run completes with exit0 while preserving enabled=false and healthy=false. Its success verifies the one requested run, not future scheduled execution. Restart must identify that new completion, never reuse an earlier successful run.

## Proxy state initialisation outcomes

State initialisation retains positive authority-creation or configuration-binding receipts when later verification or persistence fails. Such partial completion returns exit8 with `proxy_state_initialise_partial`; timeout7 and interruption130 retain precedence and the known result. A failed or uncertain write is never proof that state was unchanged. `created=true` requires successful durable authority creation, not a prior absence check. `restart_required` requires service/adoption evidence; unavailable evidence fails before mutation where possible, without inventing a boolean result.

## Remaining root outputs and streaming

Completion data contains only shell (bash|zsh|fish) and script (UTF-8 shell source ending LF); human mode emits exactly script. Version data contains only version, revision, dirty, cli_schema_version as command entry defines; unknown revision/dirty are null. Help is always generated human help, including when --json is present; no JSON resource is emitted and no state read occurs. All bare groups follow the same rule. This exception is parser presentation, never command execution.

Proxy serve --json emits JSONL v2 envelopes: ready event after bind/initialisation; stopped event after owned listener closes. Each data object has event,listen_address,pid,providers,reason per root command. reason=null on ready, interrupted on signal-driven stopped, shutdown otherwise. Interim ready envelope has ok=true, errors=[], independently of the eventual process exit. SIGINT emits stopped with ok=false and errors=[{code:"interrupted",message:"Operation interrupted; inspect state before retrying."}] then exits130; SIGTERM emits stopped with ok=true, errors=[] then exits0. Terminal envelope ok reflects final exit; interim event ok reflects whether that event succeeded. Runtime failure emits final error envelope and exits1, without a successful stopped event. Human mode prints root ready template once and no success shutdown line. A second interrupt may abort draining and exit130; it must not claim drain completion. This explicitly overrides the ordinary single-document JSON rule.

All nonquota human nulls render em dash unless command template explicitly defines another fallback. Booleans render true/false, integers decimal, timestamps UTC. Human failures use `cq: MESSAGE\n` on stderr. Format interpolation terminal-escapes untrusted text as above. Stream output errors terminate exit1; broken stdout is never success. These contracts introduce no implicit installation, daemonisation, provider selection or model requests.

Source snapshots use the `.go.txt` suffix to keep documentation outside Go package discovery. Prose source paths such as `internal/proxy/example.go` name the corresponding `.go.txt` snapshot listed in the annex manifest.
