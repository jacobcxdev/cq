# Routing CLI v2 resources and migration

This is a proposed specification, not an implementation claim. Sources are the released v0.32.5 extraction `release v0.32.5 source`; current checkout also has the pin interception regression documented in the audit. Thirty command leaves appear in `../commands.json`. Global envelope, parser, JSON, error and help rules come from the ../README.md.

## Shared resource conventions

All object fields below are required in CLI v2 output unless explicitly nullable; output uses null rather than omitting unknown values. Input policy optional arrays may be omitted and mean empty. JSON objects reject unknown fields in policy input. `uint64` means JSON integer in [0,18446744073709551615]; consumers must not round it through an IEEE-754-only representation. `Timestamp` means RFC3339Nano UTC string; unknown timestamps are null. AccountKey is a nonempty opaque string from the account inventory; never derive it from an email or parse its structure. Digest means 64 lowercase hex characters. Pool names are valid UTF-8, non-whitespace and without control characters; selection is case-insensitive and display casing preserved.

### RoutingReserveWindow

- `selector`: string, exact observed canonical quota-window selector.
- `remaining_pct`: integer, observed rounded remaining percentage, 0–100.
- `remaining_pct_exact`: number|null, exact percentage 0–100 when supplied.
- `reset_at`: Timestamp|null, time represented by existing reset_at_unix; null when unavailable.

### RoutingReserveStatus

- `configured`: boolean, a window/threshold exists.
- `window`: string|null, configured selector.
- `percent`: number|null, threshold percentage points of the full window; null when unconfigured; otherwise >0 and <100.
- `account_key`: AccountKey|null, observed current system account.
- `email`: string|null, display email, never credential material.
- `enabled`: boolean, threshold configured and no active temporary bypass.
- `blocked`: boolean, reserve currently excludes this system account.
- `reason`: enum null|system_account_unavailable|reserve_state_write_failed|disabled_until_reset|usage_stale|usage_unsettled|reserve_reached. Null means no additional reason.
- `remaining_pct`: number|null, freshest selected-window percentage.
- `reset_at`: Timestamp|null, observed reset boundary.
- `observed_at`: Timestamp|null, selected-window observation time.
- `windows`: array<RoutingReserveWindow>, sorted by selector.

`status` and `windows` may trigger live service reconciliation of reset evidence or changed system identity. They do not issue provider requests themselves. Preserve existing reserve rearming evidence checks, account-only selection and fail-closed stale/expired/unsettled treatment. `disable` is temporary; `clear` removes protection. Source `internal/proxy/codex_reserve.go:129,231,326`.

### RoutingSessionBinding

- `session_digest`: Digest, keyed session identifier.
- `pool`: string, configured pool display name.

### RoutingPool

- `name`: pool-name string.
- `value`: integer 0–4294967295; omitted input means 0 in a new document. Larger value preserves capacity relative to smaller values.
- `members`: nonempty array<AccountKey>, no duplicates.

### RoutingCapabilityEvidence

- `account_key`: AccountKey.
- `state`: enum supported|unsupported|unknown.

### RoutingCapabilityPredicate

- `schema_version`: constant 1.
- `capability`: nonempty string, exact capability identifier.
- `product_surface`, `access_path`, `auth_mode`, `requested_model`, `effective_model`: nonempty strings; a literal `*` matches every value for that field. No other glob syntax.

### RoutingCapabilityRoutingEvidence

- `schema_version`: constant 1.
- `account_key`: AccountKey.
- `account_key_hmac`: Digest.
- `workspace`, `capability`, `product_surface`, `access_path`, `auth_mode`, `requested_model`, `effective_model`, `source`: nonempty strings. The six scope fields from capability through effective_model cannot equal `*`.
- `state`: enum eligible|ineligible|unknown.
- `observed_at`: nonzero Timestamp, cannot be after evaluation time.
- `expires_at`: Timestamp|null; when nonnull, must be after evaluation time.
- `routing_generation`: uint64, equals containing policy generation.
- `authenticated`: constant true for importable evidence.

### RoutingDelegation

- `caller`: nonempty string, exact caller selector.
- `accounts`: nonempty unique array<AccountKey>.
- `expires_at`: nonzero Timestamp in UTC.

### RoutingPolicyDocument

Public policy schema stays version 1 even when carried in CLI schema 2. The internal authenticated storage schema is not exposed as the CLI document.

- `schema_version`: constant 1.
- `authority_generation`: uint64 >0.
- `routing_generation`: uint64 >0.
- `effective_generation`: uint64 <=routing_generation.
- `pools`: array<RoutingPool>, unique case-folded names.
- `session_bindings`: array<RoutingSessionBinding>, unique digests, every pool names a pool in this document.
- `capability_evidence`: array<RoutingCapabilityEvidence>, unique account keys.
- `capability_pool`: string|null; must name a pool when predicates/evidence present; otherwise null. Legacy input empty string normalises to null in CLI output and back to empty string for engine import.
- `capability_predicates`: array<RoutingCapabilityPredicate>, unique tuples of six semantic fields.
- `capability_routing_evidence`: array<RoutingCapabilityRoutingEvidence>.
- `delegations`: array<RoutingDelegation>, unique callers.

Predicates and routing evidence must both be empty or both nonempty. During update authority_generation and routing_generation must each equal prior value+1; effective_generation must not decrease. The complete document replaces prior content. Import validates the whole resource before writing. A user must not fabricate authenticated evidence: retain exported evidence only while it remains valid, or use an authorised evidence producer. Source `internal/proxy/routing_policy_store.go:116,129,435,532` and `internal/proxy/capability_routing.go:265,311`.

## Trace resource and stream contract

`trace --json` explicitly produces JSONL. Each event line is the global v2 envelope with `data: {"record": RECORD}`. Final line uses `data: {"end": END}`; do not place record and end together. Timeout and interruption each emit exactly one terminal envelope with `data: {"end": END}`, `ok=false`, and errors containing `trace_timeout` (exit 7) or `trace_interrupted` (exit 130). No additional error envelope follows. Other operational failures emit exactly one terminal error envelope with ok=false, data=null and the specified exit code. Empty successful query emits only the final end envelope, so success is distinguishable from process failure.

### RoutingTraceRecord

- `kind`: constant `route`.
- `time`: Timestamp.
- `event_type`: string, original event type (`codex_trace` or empty legacy type).
- `trace_id`, `connection_id`, `session_key`, `thread_key`, `transport`, `phase`, `stage`, `outcome`, `reason`, `error_class`, `account_hint`, `pool`, `close_reason`, `event_name`, `direction`: strings copied from the recorded event, empty when absent. These are recorded diagnostic vocabulary, not parser enums; new values are allowed without changing structure.
- `sequence`: uint64.
- `attempt`, `status_code`, `upstream_status`, `close_code`: integers >=0, zero when absent.

### JSONValue

A recursively defined value: null, boolean, finite number, UTF-8 string, array<JSONValue>, or object whose keys are strings and whose values are JSONValue. Used only for recorded payload body because upstream JSON shape belongs to the upstream protocol; never for configuration or undefined CLI resource structure.

### RoutingPayloadRecord

- `kind`: constant `payload`.
- `time`: Timestamp.
- `event_type`: constant `codex_payload`.
- `trace_id`, `connection_id`, `transport`, `direction`, `method`, `path`, `provider`, `route_kind`, `model`, `client_kind`, `session_key`, `session_source`, `thread_key`, `session_signal`, `frame_type`, `account_hint`, `body_encoding`: strings, empty if absent.
- `frame_index`, `attempt`, `status_code`, `body_bytes`: nonnegative integers, zero if absent.
- `headers`: map<string,array<string>>, previously captured permitted header values; known authentication headers must be redacted before output.
- `complete`, `truncated`: booleans.
- `body`: JSONValue|null, captured JSON or encoded string; null when absent. May contain user prompts and responses. Redact known authentication fields and headers: Authorization, Proxy-Authorization, Cookie, Set-Cookie, X-API-Key and API-Key headers case-insensitively; JSON object fields access_token, refresh_token, id_token, api_key, password and client_secret recursively. Replace their values with [REDACTED]. Arbitrary prompt/response content may contain secrets; no guarantee is made that arbitrary user content is secret-free.

### RoutingTraceEnd

- `records`: integer >=0, number of emitted records.
- `reason`: enum completed|interrupted|timeout.

Human trace line is exact joining algorithm: start `{time} {trace_id} #{sequence} {transport} {phase}` with absent phase replaced by `route_summary`; append nonempty outcome, stage, direction, event_name, account_hint in that order; append `attempt=N` if nonzero; `status=N` using upstream_status if nonzero else status_code; `pool=NAME`, `close=N`, `close_reason=TEXT`, `error_class=TEXT`, `reason=TEXT` if nonempty/nonzero, then newline. Payload human records use zero/default absent route fields and do not print body; use explicit `--payload --json` for captured body. Human trace completion adds no line. Source `cmd/cq/proxy_trace.go:29,365,396` and `internal/proxy/diag.go:512`.

## Output template expansion

`{policy_json_pretty}` and `{result_json_pretty}` are deterministic two-space indented JSON with schema field order as listed in this document, arrays sorted as specified, ending with one newline. `{result_json_pretty}` is exactly the command data object, not the outer v2 envelope. `{account_or_not_configured}` is the stored selector or literal `not configured`. Boolean placeholders use `true` or `false`. `{enabled_or_disabled}` is `enabled` or `disabled`. Nullable human fields render `unknown` unless their placeholder explicitly says `none`, `not_configured`, or `unavailable`. `{model_override_count}` is map entry count. Existing pin mutations return configured_only/restart_required=true for Codex and hot_reload/restart_required=false for Claude; show always indicates configuration only and restart_required=false, never claims live acknowledgement.

## Example preparation and complete workflows

Before account examples, run `cq codex account list` and set shell variable ACCOUNT_REFERENCE to the selected row’s exact `account_reference` value. Use that value directly; no account-alias command is assumed. Set ACCOUNT_KEY to that same account’s opaque AccountKey for the pool workflow below. Examples using `user@example.com` require that exact unique known Claude email; replace it with a unique email from `cq claude account list` otherwise. Canonical Claude commands accept unique email only. Legacy UUID input must resolve to exactly one known unique email or fail before saving. The literals are examples, not promises those accounts already exist. Runtime operations require configured credentials and a running proxy: inspect `cq service status --component proxy` first.

Pool/session workflow, after choosing the actual account key into shell variable ACCOUNT_KEY:

```sh
cq codex proxy pool set work --account "$ACCOUNT_KEY" --value 10
cq codex proxy session bind --pool work --session-id example-session
printf %s example-session | cq codex proxy session digest --session-id-stdin
cq codex proxy session show --session-id example-session
cq codex proxy session unbind --session-id example-session
```

Policy edit workflow, preparing every variable used by the command example (requires python3 in the operator environment):

```sh
POLICY_FILE="$(mktemp)"
cq codex proxy policy show --json > "$POLICY_FILE.envelope"
python3 - "$POLICY_FILE.envelope" "$POLICY_FILE" <<'PY_POLICY'
import json, sys
with open(sys.argv[1]) as f:
    p = json.load(f)['data']['policy']
p['authority_generation'] += 1
p['routing_generation'] += 1
# Keep effective_generation unchanged. This publishes an equivalent policy.
# For nonempty generation-bound evidence, obtain fresh authorised evidence first.
if p['capability_routing_evidence']:
    raise SystemExit('Obtain fresh authorised evidence before applying this policy.')
with open(sys.argv[2], 'w') as f:
    json.dump(p, f)
PY_POLICY
cq codex proxy policy apply --file "$POLICY_FILE"
```

This is a real atomic replacement example; it advances generation even when routing contents are unchanged. Review the file before applying substantive changes. Shared state initialisation is owned by `cq proxy state initialise`; offline policy flags use `--state-dir`, and explicit live `--port` cannot combine with it.

Reserve workflow: run `cq codex proxy reserve windows`, select an exact returned selector (example `7d` only if listed), then `cq codex proxy reserve set --window 7d --percent 2`. `disable` permits temporary use until verified reset; `enable` restores protection immediately; `clear` removes configuration. None changes another account's eligibility.

## Required implementation changes, not current guarantees

1. Build one canonical parser; translate legacy paths before common validation. Fix the existing provider-pin arity rejection. Unknown tails, duplicate flags, boolean values, `--`, interleaving and global JSON must follow the contract.
2. Split overloaded pin/fallback read/set/clear into distinct verbs. `fallback` keeps current default-account semantics; it must not reorder first-choice routing or weaken pin/continuity constraints. Legacy `proxy default codex` maps to fallback show; ACCOUNT maps set; --clear maps clear.
3. Canonical Claude pin set must validate a unique email against inventory rather than persist an arbitrary typo. Only legacy aliases translate UUIDs to unique known email, failing on missing or ambiguous matches. This is an intentional boundary-validation change, not an existing guarantee.
4. Add complete per-leaf help and injected global JSON for every mutator. Introduce typed output conversion and structured error codes; existing raw policy JSON, bare HTTP errors, and mixed human strings do not meet v2.
5. Add canonical `--timeout` support to routes that currently use hard-coded 10s or no CLI deadline. Keep priming restart and live reserve/policy/lease behaviour intact. Avoid config creation during show/help/error resolution.
6. Add policy show/apply --port, currently absent. Translate legacy --state-root to --state-dir. Ensure offline existing-state access cannot bootstrap missing state, and require exclusive ownership before offline writes.
7. Trace current --json returns raw JSONL and ignores write errors; v2 envelopes, final stream marker, explicit interrupt exit130, timeout, duplicate checking and write-error propagation are required. Preserve retained-rotation continuity, tail/filter ordering and missing-file behaviour. Known authentication-header and authentication-field redaction is required before payload JSON output; arbitrary prompts and responses remain user content and may contain secrets.
8. Session selector validation must happen before reading stdin or contacting runtime. Require valid UTF-8, preserve exact bytes (including newline), and apply the same 1–4096-byte bound to direct and stdin selectors; preserve existing digest pass-through, and explicit list rejection of selectors.
9. Keep legacy `proxy pin` with no provider as a deprecated aggregate read-only compatibility command returning both pin resources, or retire it with an actionable message listing the two explicit show commands; it must never silently choose a provider. Root owns the aggregate compatibility choice. No other implicit provider is introduced.

## Migration coverage

Canonical mapping per leaf is in JSON. Provider-last pin/default, root proxy prime/reserve/trace, plural leases invalidate and nested policy pool/session paths all receive exact aliases. `proxy policy status` becomes `codex proxy policy show`; shared initialise becomes root-owned proxy state initialise. `proxy policy` input --state-root becomes --state-dir; canonical flags must not retain the old spelling except inside deprecated aliases. Historical malformed/no-op tails are errors, not behaviours to preserve.

## Unicode pool names and hook safety

Pool names retain supported Unicode and spaces; do not tighten their grammar to shell-safe identifiers. All generated hook shell arguments containing pool names must use correct shell escaping for the target shell (or avoid shell evaluation through an argument-vector interface). Quotes, spaces, dollar signs and backticks remain literal data. Test generated invocation with Unicode, embedded spaces, single/double quotes, dollar signs and backticks; no expansion or command substitution may occur. This requirement does not introduce a new hook command.

## Optional computed defaults

For integer options whose default depends on configuration, JSON `default` is null and `omission_resolution` states the exact resolver. Port omission resolves existing configured port, then 19280 only when unset; pool-set value omission preserves an existing pool value or chooses 0 for a new pool. Null is a specification marker here, never the value sent to runtime.
