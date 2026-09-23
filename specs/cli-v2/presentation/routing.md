# Codex routing presentation specification

Status: proposed human-interface design; not a claim of implemented output. Covers 28 individual leaves. `commands.json`, `resources/routing.md`, current `cli_v2_selections.go`, `cli_v2_policy.go`, `cli_v2_reserve.go` and `cli_v2_validation.go` determine data and effects. Changes here concern human presentation only; JSON envelopes, exit statuses, authentication, routing decisions and streaming ABIs remain unchanged.

## Shared rendering contract

The shared [presentation contract](README.md) is authoritative. Wireframes below are terminal variants; existing redirected human text remains byte-compatible during this rollout. Shared numeric/date/colour/help rules apply except the exact configured threshold and frozen trace exceptions stated here.

- Bold title identifies resource. Cyan marks command paths, selectors and identifiers; never conveys health. Prose values use default foreground; quantities and times follow the shared colour contract, and secondary explanation uses muted foreground. Green marks only a confirmed completed write or affirmative validated fact. Amber marks pending application, unknown evidence, bypass, partial results or unavailable observations. Red marks failed command, never ordinary `disabled` configuration.
- No colour when redirected, nonempty `NO_COLOR` is present, or terminal capability forbids it. Words and labels retain every distinction without colour. No emoji or decorative border required. Escape terminal control characters from all external values.
- Width W is min(100, destination terminal width), measured in display cells, excluding ANSI. Use tables only when every complete field fits W minus two margin cells; otherwise use labelled records. Never truncate identifiers, account keys, digests, dates or reasons. Long identifiers wrap on continuation lines without inserted ellipses; existing redirected text retains its current format. Terminal records stack at narrow widths. No horizontal scrolling requirement.
- Quantities right-align and counts use comma grouping; identifiers never do. Observed percentages follow README rounding and retain `%`; configured reserve thresholds retain the exact shortest round-trippable decimal, because they are editable settings; uint64 generations render exact base-10 digits, without float conversion, scientific notation or grouping. Pool values have no unit: they are relative preservation values, not percentages, credits or capacity multipliers. Zero is `0`, null is `unknown`, absent selection is `not configured`, empty collection is `none`. Do not replace any with a dash.
- A time label expands using README: `<relative> · 23 Sep 2026, 16:30:00 BST (UTC+01:00)`, local timestamp zone/offset with year. Preserve nonzero fractional seconds for audit-sensitive evidence. Capture one reference instant. Null time is `unknown`; never infer a future reset from an expired time. Trace retains its existing timestamp representation and line contract.
- Tables use two spaces between columns, no vertical rules. Heading and first row separated by one newline; major sections separated by one blank line. Detail rows use two-space indent and aligned label/value columns only while they fit. At narrow width put value on next indented line. Layouts below specify ordering; angle brackets are substitutions, not literal UI text.
- All ordinary success output goes to stdout. Failures go to stderr, with no synthetic success card. Exact terminal error shell has three lines: `Error · <message>`, `  Code: <code>`, `  Command: cq <path>`; wrapped continuations align beneath message. Preserve exact message in each leaf's error table. Existing redirected diagnostic bytes remain unchanged. Sanitise placeholders before interpolation. Terminal colour affects `Error` and code, not message meaning. Usage failures add `Help: cq <path> --help` after a blank line. No credential or raw session input is echoed.
- For timeout or interruption after a possible write with no returned receipt, show failure and `Result: unconfirmed` in amber; do not claim rollback, no change, or success. If an adapter provides an explicit committed or restored receipt, preserve that known result and mark only the unresolved remainder unconfirmed; failure does not erase evidence. Use the existing message that instructs state inspection. Read-only failure shows `Result: unavailable`, never a stale success card. Standard interruption from current routing adapter is exit 130, code `interrupted`, message `Operation interrupted; inspect state before retrying.`; this supplements per-leaf catalogues except trace's specialised interruption. No invented retry or restart occurs.
- For Codex pin, fallback and priming mutations, a successful local `SaveConfig` return is a confirmed configuration receipt even if the deadline or interruption is observed immediately afterwards. Keep that receipt on terminal stdout, show the warning diagnosis on stderr, and state `Live application unconfirmed`. Do not infer proxy reload or live application from the save. A stop before the save, or an error returned by the save, has no confirmed receipt.
- No progress spinner for quick reads. Mutations may show `Saving…` on TTY stderr while pending, erased before final result; never print `Saved` before acknowledged completion. Redirected output has no transient progress. No partial-operation percentage exists in these resources, so none is displayed.

## Help design used by each leaf

Every `<path> --help`, `cq help <path>` and accepted legacy alias help resolves to the same canonical page. Exit 0; no state reads or writes. Page order follows README: bold canonical command title; one-sentence purpose; Usage; essential context; Arguments when any; Options; Global options; Related commands; Examples. Leaf Behaviour content supplies essential context or indented option detail immediately beneath its associated option. Help does not use success/error colours. Required inputs are visibly marked `required`; repeatable inputs say `repeatable`. Optional boolean defaults false are not repeated. Nontrivial defaults, mutual exclusions, ranges and side effects remain visible in essential context or associated option detail. Exact per-command option descriptions/constraints and examples appear below; they are content requirements, not a request to print JSON metadata labels.

All leaves have Global options with `-h, --help  Show this help`, `-j, --json  Emit the documented JSON output; disable prompts`, `-v, --version  Show CQ version`. Trace replaces the JSON row with `Emit one JSON envelope per record and one terminal envelope`. Global boolean duplicate and `=true`/`=false` rules remain in shared parser reference; do not repeat three metadata paragraphs on every page. Each command's Behaviour includes its listed effects and validation constraints. Unknown flags and duplicate values still fail before state access.

## Reusable output blocks

These are exact expansions referenced by per-leaf wireframes. They avoid duplicating resource schemas while giving every field a fixed presentation.

### Reserve card

```
Codex reserve · <state>
  Scope          Current system account only
  Account        <reserve.email or unavailable>
  Account key    <reserve.account_key or unavailable>
  Window         <reserve.window or not configured>
  Protected      <reserve.percent>% of full window
  Remaining      <reserve.remaining_pct>%
  Enabled        <yes|no from reserve.enabled>
  Routing        <excluded by reserve|not excluded by reserve from reserve.blocked>
  Reason         <reason label>
  Reset          <reserve.reset_at formatted time>
  Observed       <reserve.observed_at formatted time>
```

`state` is `not configured` if configured=false; else `temporarily bypassed` when reason=disabled_until_reset; else `excludes system account` when blocked=true; else `protection enabled` when enabled=true; else `disabled`. Unknown percentages print `unknown` without a percent sign. Unconfigured protected threshold prints `not configured`. The `Routing` label concerns this reserve only; never replace it with `account eligible`, because other constraints may exclude the account. Always include explicit scope. Do not present pooled quota, arbitrary account selector or team reserve.

Reason labels map exactly: null → `none`; system_account_unavailable → `system account unavailable`; reserve_state_write_failed → `reserve state could not be saved`; disabled_until_reset → `bypassed until verified reset`; usage_stale → `usage observation stale`; usage_unsettled → `usage observation unsettled`; reserve_reached → `protected threshold reached`. Include raw reason code in parentheses for nonnull reason. Stale/unsettled/unavailable/write failure is amber even with exit 0; threshold exclusion is amber, not a failed command. Do not calculate extra headroom or gauge. Returned `windows` appear as the reserve-windows table after card, so alternate observed windows are not silently lost.

### Reserve-windows table

```
System-account windows
  Window        Remaining       Reset
  <selector>    <percentage>%   <reset_at formatted time>
```

Sort exact selector ascending. Use remaining_pct_exact when nonnull with shared observed-percentage rounding, otherwise integer remaining_pct. Empty: `No system-account quota windows available.` No synthetic default `7d` or guaranteed reset. Long dates trigger record layout: Window, Remaining, Reset. This table alone has no account identity field; do not invent one.

### Policy detail

```
  Authority generation  <policy.authority_generation>
  Routing generation    <policy.routing_generation>
  Effective generation  <policy.effective_generation>
  Schema                <policy.schema_version>

Pools
  Name    Value    Members
  <name>  <value>  <member account keys, one per continuation line>

Session bindings
  Digest    Pool
  <session_digest>  <pool>

Capability account evidence
  Account key    State
  <account_key>  <state>

Capability routing
  Pool  <policy.capability_pool or none>
  Predicates
    Schema <schema_version>; capability <capability>
    Product surface <product_surface>; access path <access_path>
    Authentication mode <auth_mode>
    Requested model <requested_model>; effective model <effective_model>
  Evidence
    Schema <schema_version>; account <account_key>
    Account HMAC <account_key_hmac>; workspace <workspace>
    Capability <capability>; product surface <product_surface>
    Access path <access_path>; authentication mode <auth_mode>
    Requested model <requested_model>; effective model <effective_model>
    Source <source>; state <state>; authenticated <authenticated>
    Observed <observed_at formatted time>; expires <expires_at formatted time or unknown>
    Routing generation <routing_generation>

Delegations
  Caller <caller>
  Accounts <accounts, one per continuation line>
  Expires <expires_at formatted time>
```

Every collection emits `none` if empty; each record's listed fields appears once. Preserve resource array ordering; map keys sort lexically. Do not elide nonempty collections after N rows. Values keep exact raw spelling, including `*` predicate wildcard and Unicode pool names. `authenticated` is reported evidence, not a fresh security verification. Capability `unknown` is amber, eligible/supported is a recorded fact rather than overall account-health badge. Generations may differ; print `Application: routing generation not yet effective` in amber when effective_generation < routing_generation, otherwise `Application: generations match`. This comparison does not claim live traffic proof. Offline mode additionally prints `Source: offline policy state; live application not checked`; live mode prints `Source: authenticated proxy control`. No invented live-ack flag.

### Selection card

```
Codex <pin|fallback> · <configured|not configured>
  Account key  <account or not configured>
  Source       Saved configuration
  Application  Live process acknowledgement not checked
```

Mutations prepend `Saved Codex <pin|fallback> configuration.` and append amber `Restart required: yes` and `Next: cq service restart --component proxy`. Show omits restart advice; restart_required=false means this read requested no restart, not proof that runtime matches saved config. Pin and fallback remain distinct: pin constrains selection; fallback does not supersede pin or continuity rules. Never show `active account` for either field.


## `cq codex proxy fallback clear`

Decision: Clear the Codex proxy fallback.

### Help variant

Title: `cq codex proxy fallback clear`. Purpose: Clear the Codex proxy fallback.

```text
Usage
  cq codex proxy fallback clear [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy fallback clear` — Inspect or update the configured setting.

Related commands: cq codex account list; cq codex proxy pin show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy default codex --clear` → `cq codex proxy fallback clear`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex fallback configuration.
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy fallback set`

Decision: Set the Codex proxy fallback.

### Help variant

Title: `cq codex proxy fallback set`. Purpose: Set the Codex proxy fallback.

```text
Usage
  cq codex proxy fallback set ACCOUNT [options]
```


Arguments, in displayed order:

| Input | Help | Constraints |
|---|---|---|
| `ACCOUNT` (required) | Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored. | Nonempty. Resolve to exactly one known Codex account. Ambiguous email is rejected; use alias or AccountKey. |

Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy fallback set "$ACCOUNT_REFERENCE"` — Requires the account_reference preparation in routing notes.

Related commands: cq codex account list; cq codex proxy pin show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy default codex ACCOUNT` → `cq codex proxy fallback set`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex fallback configuration.
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_account_not_found` | 3 | Account reference matches no known account. | Account not found: {account}. |
| `routing_account_ambiguous` | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| `routing_inventory_unavailable` | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy fallback show`

Decision: Show the Codex proxy fallback.

### Help variant

Title: `cq codex proxy fallback show`. Purpose: Show the Codex proxy fallback.

```text
Usage
  cq codex proxy fallback show [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Read configuration only; do not query or modify running routing.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy fallback show` — Inspect or update the configured setting.

Related commands: cq codex account list; cq codex proxy pin show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy default codex` → `cq codex proxy fallback show`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy lease invalidate`

Decision: Invalidate reusable Codex task-account affinity.

### Help variant

Title: `cq codex proxy lease invalidate`. Purpose: Invalidate reusable Codex task-account affinity.

```text
Usage
  cq codex proxy lease invalidate [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Invalidate all reusable task-account leases from earlier admissions. Active request authority and required account continuity remain intact; later portable requests choose from current account capacity again.
- Durably advance journal generation and invalidate reusable affinity.
- No restart required; repeated invalidation may advance generation but reports zero when no reusable affinity remains.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy lease invalidate` — Expire reusable affinity without breaking required continuity.

Related commands: cq service status --component proxy; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy leases invalidate` → `cq codex proxy lease invalidate`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex leases invalidated
  Invalidated leases  <invalidated_leases>
  Journal generation  <journal_generation>
```

Empty / absent state: Zero invalidations is success: `Invalidated leases  0`. Unavailable journal is failure, not zero.

Partial, stale, pending, timeout and interruption truth: Acknowledged count refers to reusable affinities only. Do not claim active requests cancelled or continuity constraints removed. No per-lease partial data exists. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `lease_journal_unavailable` | 4 | Journal writer unavailable or quarantined legacy journal cannot mutate. | Lease journal is unavailable for invalidation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pin clear`

Decision: Clear the Codex proxy pin.

### Help variant

Title: `cq codex proxy pin clear`. Purpose: Clear the Codex proxy pin.

```text
Usage
  cq codex proxy pin clear [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pin clear` — Inspect or update the configured setting.

Related commands: cq codex account list; cq codex proxy fallback show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy pin codex --clear` → `cq codex proxy pin clear`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex pin configuration.
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pin set`

Decision: Set the Codex proxy pin.

### Help variant

Title: `cq codex proxy pin set`. Purpose: Set the Codex proxy pin.

```text
Usage
  cq codex proxy pin set ACCOUNT [options]
```


Arguments, in displayed order:

| Input | Help | Constraints |
|---|---|---|
| `ACCOUNT` (required) | Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored. | Nonempty. Resolve to exactly one known Codex account. Ambiguous email is rejected; use alias or AccountKey. |

Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pin set "$ACCOUNT_REFERENCE"` — Requires the account_reference preparation in routing notes.

Related commands: cq codex account list; cq codex proxy fallback show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy pin codex ACCOUNT` → `cq codex proxy pin set`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex pin configuration.
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_account_not_found` | 3 | Account reference matches no known account. | Account not found: {account}. |
| `routing_account_ambiguous` | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| `routing_inventory_unavailable` | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pin show`

Decision: Show the Codex proxy pin.

### Help variant

Title: `cq codex proxy pin show`. Purpose: Show the Codex proxy pin.

```text
Usage
  cq codex proxy pin show [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
- Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.
- Read configuration only; do not query or modify running routing.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pin show` — Inspect or update the configured setting.

Related commands: cq codex account list; cq codex proxy fallback show; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy pin codex` → `cq codex proxy pin show`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
<Selection card>
```

Empty / absent state: No stored selector is normal success: render `not configured`; set cannot succeed without a resolved account. Clear is idempotent, including when already absent.

Partial, stale, pending, timeout and interruption truth: No partial success resource. Mutations confirm saved configuration only; runtime use remains unconfirmed until explicit restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy policy apply`

Decision: Apply a complete Codex routing-policy document.

### Help variant

Title: `cq codex proxy policy apply`. Purpose: Apply a complete Codex routing-policy document.

```text
Usage
  cq codex proxy policy apply --file FILE [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--file FILE` | UTF-8 JSON RoutingPolicyDocument to apply. Relative paths resolve against the working directory. This is a complete replacement document, not a patch. | required; once; no value |
| `--state-dir STATE_DIR` | Read or update policy offline at this existing state root instead of contacting the running proxy. Omission selects authenticated live control. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Without --state-dir, use authenticated live control. With an explicit existing state root, operate offline; no live process is contacted.
- The public document remains schema_version 1, independent of the CLI envelope schema_version 2. Account members are opaque AccountKeys, not email references. See RoutingPolicyDocument in routing notes.
- Validate and apply one complete policy atomically; live mode takes effect immediately without restart.
- Offline mode updates selected state only; the caller must ensure no live owner is concurrently using it.
- Generation checks prevent replaying a stale policy; repeated stale input can conflict rather than silently succeed.
- `--file`: Readable regular file.
- `--file`: One strict schema_version 1 JSON document; reject unknown fields and trailing data.
- `--file`: Validate document and generation constraints before mutation.
- `--state-dir`: Clean absolute non-root directory.
- `--state-dir`: Mutually exclusive with --port.
- `--state-dir`: Do not create state; initialise it with cq proxy state initialise first.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy policy apply --file "$POLICY_FILE"` — POLICY_FILE preparation is specified in routing notes.

Related commands: cq codex proxy policy show; cq codex proxy pool --help; cq codex proxy session --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy apply` → `cq codex proxy policy apply`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex routing policy.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `policy_document_invalid` | 2 | Policy JSON violates schema or references. | Invalid routing-policy document: {detail}. |
| `policy_generation_conflict` | 6 | Input generations do not follow current authority state. | Policy generations conflict; show current policy and prepare a fresh document. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy policy show`

Decision: Show the complete Codex routing-policy document.

### Help variant

Title: `cq codex proxy policy show`. Purpose: Show the complete Codex routing-policy document.

```text
Usage
  cq codex proxy policy show [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--state-dir STATE_DIR` | Read or update policy offline at this existing state root instead of contacting the running proxy. Omission selects authenticated live control. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Without --state-dir, use authenticated live control. With an explicit existing state root, operate offline; no live process is contacted.
- The public document remains schema_version 1, independent of the CLI envelope schema_version 2. Account members are opaque AccountKeys, not email references. See RoutingPolicyDocument in routing notes.
- Read policy only.
- `--state-dir`: Clean absolute non-root directory.
- `--state-dir`: Mutually exclusive with --port.
- `--state-dir`: Do not create state; initialise it with cq proxy state initialise first.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy policy show` — Read live policy.

Related commands: cq codex proxy policy show; cq codex proxy pool --help; cq codex proxy session --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy status` → `cq codex proxy policy show`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex routing policy
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: policy_generation_conflict from input generations (show has no input document). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `policy_document_invalid` | 2 | Policy JSON violates schema or references. | Invalid routing-policy document: {detail}. |
| `policy_generation_conflict` | 6 | Input generations do not follow current authority state. | Policy generations conflict; show current policy and prepare a fresh document. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pool rename`

Decision: Rename a pool without changing its identity.

### Help variant

Title: `cq codex proxy pool rename`. Purpose: Rename a pool without changing its identity.

```text
Usage
  cq codex proxy pool rename OLD_NAME NEW_NAME [options]
```


Arguments, in displayed order:

| Input | Help | Constraints |
|---|---|---|
| `OLD_NAME` (required) | Case-insensitive pool name; preserve configured display casing. | Valid UTF-8, contains at least one non-whitespace character, contains no control characters. |
| `NEW_NAME` (required) | Case-insensitive pool name; preserve configured display casing. | Valid UTF-8, contains at least one non-whitespace character, contains no control characters. |

Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
- Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.
- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pool rename work primary` — Uses the account_reference and work pool preparation in routing notes.

Related commands: cq codex account list; cq codex proxy policy show; cq codex proxy session bind --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy pool rename` → `cq codex proxy pool rename`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex pool policy.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: routing_account_not_found, routing_account_ambiguous, routing_inventory_unavailable (only set resolves accounts). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `routing_account_not_found` | 3 | Account reference matches no known account. | Account not found: {account}. |
| `routing_account_ambiguous` | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| `routing_inventory_unavailable` | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| `pool_not_found` | 3 | Named existing pool missing. | Pool not found: {name}. |
| `pool_name_conflict` | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pool set`

Decision: Create a pool or replace its account membership.

### Help variant

Title: `cq codex proxy pool set`. Purpose: Create a pool or replace its account membership.

```text
Usage
  cq codex proxy pool set NAME --account ACCOUNT [options]
```


Arguments, in displayed order:

| Input | Help | Constraints |
|---|---|---|
| `NAME` (required) | Case-insensitive pool name; preserve configured display casing. | Valid UTF-8, contains at least one non-whitespace character, contains no control characters. |

Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--account ACCOUNT` | Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored. Repeat --account for each member; membership is replaced, not appended. | required; repeatable; no value |
| `--value VALUE` | Relative capacity-preservation value, 0–4294967295. Higher values preserve this pool by preferring lower-value viable accounts for ordinary unbound work. If omitted, preserve an existing pool value or use 0 for a new pool. | optional; once; existing value, otherwise 0 |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
- Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.
- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.
- `--account`: Nonempty.
- `--account`: Resolve to exactly one known Codex account.
- `--account`: Ambiguous email is rejected; use alias or AccountKey.
- `--account`: At least one member; duplicate resolved accounts rejected.
- `--value`: Unsigned 32-bit integer.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pool set work --account "$ACCOUNT_REFERENCE" --value 10` — Uses the account_reference and work pool preparation in routing notes.

Related commands: cq codex account list; cq codex proxy policy show; cq codex proxy session bind --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy pool set` → `cq codex proxy pool set`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex pool policy.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: pool_name_conflict (only rename). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `routing_account_not_found` | 3 | Account reference matches no known account. | Account not found: {account}. |
| `routing_account_ambiguous` | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| `routing_inventory_unavailable` | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| `pool_not_found` | 3 | Named existing pool missing. | Pool not found: {name}. |
| `pool_name_conflict` | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy pool value`

Decision: Set a pool’s relative capacity-preservation value.

### Help variant

Title: `cq codex proxy pool value`. Purpose: Set a pool’s relative capacity-preservation value.

```text
Usage
  cq codex proxy pool value NAME VALUE [options]
```


Arguments, in displayed order:

| Input | Help | Constraints |
|---|---|---|
| `NAME` (required) | Case-insensitive pool name; preserve configured display casing. | Valid UTF-8, contains at least one non-whitespace character, contains no control characters. |
| `VALUE` (required) | Relative capacity-preservation value, 0–4294967295. Higher values preserve this pool by preferring lower-value viable accounts for ordinary unbound work. | Unsigned 32-bit integer. |

Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
- Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.
- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy pool value work 20` — Uses the account_reference and work pool preparation in routing notes.

Related commands: cq codex account list; cq codex proxy policy show; cq codex proxy session bind --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy pool value` → `cq codex proxy pool value`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex pool policy.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: routing_account_not_found, routing_account_ambiguous, routing_inventory_unavailable (only set resolves accounts); pool_name_conflict (only rename). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `routing_account_not_found` | 3 | Account reference matches no known account. | Account not found: {account}. |
| `routing_account_ambiguous` | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| `routing_inventory_unavailable` | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| `pool_not_found` | 3 | Named existing pool missing. | Pool not found: {name}. |
| `pool_name_conflict` | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy prime disable`

Decision: Disable automatic Codex quota-window priming.

### Help variant

Title: `cq codex proxy prime disable`. Purpose: Disable automatic Codex quota-window priming.

```text
Usage
  cq codex proxy prime disable [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
- Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.
- Atomically persist enablement; equivalent repeated action is idempotent.
- No implicit restart or immediate priming request.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy prime disable` — Inspect configuration or persist enablement; restart proxy after a change.

Related commands: cq codex proxy prime status; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy prime disable` → `cq codex proxy prime disable`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex priming configuration.
Codex priming · configured <enabled|disabled>
  Application  Live process acknowledgement not checked

Model overrides
  Window  Model
  <scope>  <model ID>
Restart required: yes
Next: cq service restart --component proxy
```

Empty / absent state: Empty model_overrides prints `Model overrides: none`. Disabled configuration is normal neutral status. Missing/unreadable configuration is error, not silently disabled.

Partial, stale, pending, timeout and interruption truth: No partial success. Enable does not send a priming request immediately; later priming consumes real provider requests. Disable does not stop a running process without restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy prime enable`

Decision: Enable automatic Codex quota-window priming.

### Help variant

Title: `cq codex proxy prime enable`. Purpose: Enable automatic Codex quota-window priming.

```text
Usage
  cq codex proxy prime enable [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
- Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.
- Atomically persist enablement; equivalent repeated action is idempotent.
- No implicit restart or immediate priming request.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy prime enable` — Inspect configuration or persist enablement; restart proxy after a change.

Related commands: cq codex proxy prime status; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy prime enable` → `cq codex proxy prime enable`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex priming configuration.
Codex priming · configured <enabled|disabled>
  Application  Live process acknowledgement not checked

Model overrides
  Window  Model
  <scope>  <model ID>
Restart required: yes
Next: cq service restart --component proxy
```

Empty / absent state: Empty model_overrides prints `Model overrides: none`. Disabled configuration is normal neutral status. Missing/unreadable configuration is error, not silently disabled.

Partial, stale, pending, timeout and interruption truth: No partial success. Enable does not send a priming request immediately; later priming consumes real provider requests. Disable does not stop a running process without restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy prime status`

Decision: Show configured Codex quota-window priming.

### Help variant

Title: `cq codex proxy prime status`. Purpose: Show configured Codex quota-window priming.

```text
Usage
  cq codex proxy prime status [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
- Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.
- Read configured priming state.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy prime status` — Inspect configuration or persist enablement; restart proxy after a change.

Related commands: cq codex proxy prime status; cq service restart --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy prime status` → `cq codex proxy prime status`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex priming · configured <enabled|disabled>
  Application  Live process acknowledgement not checked

Model overrides
  Window  Model
  <scope>  <model ID>
Source: saved configuration
```

Empty / absent state: Empty model_overrides prints `Model overrides: none`. Disabled configuration is normal neutral status. Missing/unreadable configuration is error, not silently disabled.

Partial, stale, pending, timeout and interruption truth: No partial success. Enable does not send a priming request immediately; later priming consumes real provider requests. Disable does not stop a running process without restart. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy readiness show`

Decision: Check retained HTTP readiness evidence for an exact Codex build.

### Help variant

Title: `cq codex proxy readiness show`. Purpose: Check retained HTTP readiness evidence for an exact Codex build.

```text
Usage
  cq codex proxy readiness show --client-build CLIENT_BUILD [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--client-build CLIENT_BUILD` | Expected Codex client version; required. Must match the recorded or exercised client version byte for byte; arbitrary build labels are not accepted. | required; once; no value |
| `--state-dir STATE_DIR` | Read or write readiness evidence in this absolute directory. Default: the resolved CQ proxy state directory. | optional; once; no value |

Behaviour content, after options:

- Checks the retained HTTP marker against the running CQ executable version, expected Codex build, parser and lease schemas, routing semantics, retry budget and fixture hash.
- Success means the retained marker matches those requirements. It does not perform new requests or prove that the proxy is currently healthy.
- Read-only local marker inspection; does not create configuration or renew evidence.
- `--client-build`: Match ^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace.
- `--client-build`: Require byte-for-byte equality with the evidence or executed client version; no whitespace trimming, case folding, prefix matching or arbitrary label substitution.
- `--state-dir`: If present, nonempty canonical absolute directory; reject symlink traversal.
- `--state-dir`: If omitted, use ResolveDefaultPaths().StateDir; this is not the runtime resilience-state directory.

Examples, exact command text and explanatory captions:

- `cq codex proxy readiness show --client-build 0.146.0` — Inspect the recorded HTTP evidence for this exact client version.

Related commands: cq codex proxy validate http --help; cq service status --component proxy. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq codex validate http` → `cq codex proxy readiness show`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex HTTP readiness · current
  Codex build  <client_build>
  Validated    <validated_at formatted time>
  Evidence     Retained validation; no live health check

Validation identity
  Marker version  <marker.version>
  CQ build  <marker.cq_build>
  Transport  <marker.transport>
  Parser schema  <marker.parser_schema>
  Lease schema  <marker.lease_schema>
  Semantics revision  <marker.semantics_revision>
  Client build  <marker.client_build>
  Retry budget  <marker.retry_budget>
  Fixture hash  <marker.fixture_hash>
  CQ executable SHA-256  <marker.cq_executable_sha256>
  Client executable SHA-256  <marker.client_executable_sha256>
  Service kind  <marker.service_kind>
  Service identity SHA-256  <marker.service_identity_sha256>
  Installed result  <marker.installed_result>
  Completed gates  <marker.completed_gates; one per line>
  Marker validated  <marker.validated_at formatted time>
```

Empty / absent state: Missing marker is readiness_missing, exit 3. A successful HTTP marker requires valid nonzero executable/service SHA-256 digests, a recognised service kind, installed_result=passed and the exact nonempty required gate set. Missing or invalid values never render a current card or successful `not recorded`/`none` placeholders; corrupt/stale evidence follows its declared failure path.

Partial, stale, pending, timeout and interruption truth: Read-only; no command timeout option, no async pending state, no live request. Green current means retained tuple matches, never proxy healthy. No partial success resource. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `readiness_missing` | 3 | No HTTP marker exists | No HTTP readiness evidence is recorded. |
| `readiness_stale` | 6 | Marker does not match required tuple | HTTP readiness evidence does not match the requested build. |
| `readiness_invalid` | 1 | Marker is corrupt or unreadable | HTTP readiness evidence could not be read. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve clear`

Decision: Remove the system-account reserve configuration.

### Help variant

Title: `cq codex proxy reserve clear`. Purpose: Remove the system-account reserve configuration.

```text
Usage
  cq codex proxy reserve clear [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- Remove threshold configuration and any temporary bypass permanently. This differs from disable, which automatically rearms.
- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve clear` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve clear` → `cq codex proxy reserve clear`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Removed system-account reserve configuration.
<Reserve card>

<Reserve-windows table>
```

Empty / absent state: Unconfigured status/clear are successful and show `not configured`. Enable/disable without configuration fail with reserve_not_configured. Missing observations print unknown, never zero.

Partial, stale, pending, timeout and interruption truth: Exit 0 may contain stale/unsettled/unavailable reserve evidence: show amber reason, retain returned blocked/enabled values. Disable lasts until verified reset; clear permanently removes configuration. Never claim other accounts changed. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_not_configured (only enable/disable); reserve_evidence_required (only disable); reserve_window_unavailable (only set). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve disable`

Decision: Disable the reserve until a verified reset.

### Help variant

Title: `cq codex proxy reserve disable`. Purpose: Disable the reserve until a verified reset.

```text
Usage
  cq codex proxy reserve disable [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- Disable protection temporarily until fresh usage proves a reset, or the system account changes. Fresh usage and reset evidence are required. After the saved reset time passes without fresh proof, the account remains blocked.
- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve disable` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve disable` → `cq codex proxy reserve disable`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Temporarily bypassed system-account reserve.
<Reserve card>

<Reserve-windows table>
```

Empty / absent state: Unconfigured status/clear are successful and show `not configured`. Enable/disable without configuration fail with reserve_not_configured. Missing observations print unknown, never zero.

Partial, stale, pending, timeout and interruption truth: Exit 0 may contain stale/unsettled/unavailable reserve evidence: show amber reason, retain returned blocked/enabled values. Disable lasts until verified reset; clear permanently removes configuration. Never claim other accounts changed. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_window_unavailable (only set). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve enable`

Decision: Enable the configured system-account reserve.

### Help variant

Title: `cq codex proxy reserve enable`. Purpose: Enable the configured system-account reserve.

```text
Usage
  cq codex proxy reserve enable [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- Remove the temporary bypass immediately. Existing threshold configuration is retained; fresh usage still determines eligibility.
- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve enable` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve enable` → `cq codex proxy reserve enable`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Enabled system-account reserve protection.
<Reserve card>

<Reserve-windows table>
```

Empty / absent state: Unconfigured status/clear are successful and show `not configured`. Enable/disable without configuration fail with reserve_not_configured. Missing observations print unknown, never zero.

Partial, stale, pending, timeout and interruption truth: Exit 0 may contain stale/unsettled/unavailable reserve evidence: show amber reason, retain returned blocked/enabled values. Disable lasts until verified reset; clear permanently removes configuration. Never claim other accounts changed. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_evidence_required (only disable); reserve_window_unavailable (only set). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve set`

Decision: Set the system-account quota reserve.

### Help variant

Title: `cq codex proxy reserve set`. Purpose: Set the system-account quota reserve.

```text
Usage
  cq codex proxy reserve set --window WINDOW --percent PERCENT [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--window WINDOW` | Exact window selector returned by cq codex proxy reserve windows; for example 7d. No inferred provider or window default. | required; once; no value |
| `--percent PERCENT` | Protect this percentage of the full window quota. The system account is blocked at or below this remaining percentage; 2 means keep the final 2 percentage points, not 2% of current remaining quota. | required; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- Replace the selected window and percentage and enable protection immediately. Only one reserve window is configured at a time.
- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.
- `--window`: Must be a currently observed system-account quota window.
- `--window`: Canonicalise legacy case, surrounding spaces, and underscores exactly as released parser, then validate observed selector.
- `--percent`: Finite number strictly greater than 0 and strictly less than 100.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve set --window 7d --percent 2` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve set` → `cq codex proxy reserve set`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved system-account reserve.
<Reserve card>

<Reserve-windows table>
```

Empty / absent state: Unconfigured status/clear are successful and show `not configured`. Enable/disable without configuration fail with reserve_not_configured. Missing observations print unknown, never zero.

Partial, stale, pending, timeout and interruption truth: Exit 0 may contain stale/unsettled/unavailable reserve evidence: show amber reason, retain returned blocked/enabled values. Disable lasts until verified reset; clear permanently removes configuration. Never claim other accounts changed. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_not_configured (only enable/disable); reserve_evidence_required (only disable). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve status`

Decision: Show system-account reserve state and evidence.

### Help variant

Title: `cq codex proxy reserve status`. Purpose: Show system-account reserve state and evidence.

```text
Usage
  cq codex proxy reserve status [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- GET observes live reserve state; it can reconcile reset evidence and system-account changes. No provider quota request is made by this CLI.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve status` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve status` → `cq codex proxy reserve status`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
<Reserve card>

<Reserve-windows table>
```

Empty / absent state: Unconfigured status/clear are successful and show `not configured`. Enable/disable without configuration fail with reserve_not_configured. Missing observations print unknown, never zero.

Partial, stale, pending, timeout and interruption truth: Exit 0 may contain stale/unsettled/unavailable reserve evidence: show amber reason, retain returned blocked/enabled values. Disable lasts until verified reset; clear permanently removes configuration. Never claim other accounts changed. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_not_configured (only enable/disable); reserve_evidence_required (only disable); reserve_window_unavailable (only set). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy reserve windows`

Decision: List observed system-account quota window selectors.

### Help variant

Title: `cq codex proxy reserve windows`. Purpose: List observed system-account quota window selectors.

```text
Usage
  cq codex proxy reserve windows [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
- Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
- GET observes live reserve state; it can reconcile reset evidence and system-account changes. No provider quota request is made by this CLI.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy reserve windows` — For set, first confirm 7d appears in reserve windows; other actions need running proxy.

Related commands: cq codex proxy reserve windows; cq codex proxy reserve status; cq codex proxy reserve set --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy reserve windows` → `cq codex proxy reserve windows`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
<Reserve-windows table>
```

Empty / absent state: Empty windows are successful empty result, with exact sentence in table contract. Unavailable configured control is error, not empty windows.

Partial, stale, pending, timeout and interruption truth: Read may reconcile reset/system-account evidence; no provider request. Do not show freshness not present in windows data. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: reserve_not_configured (only enable/disable); reserve_evidence_required (only disable); reserve_window_unavailable (only set). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `reserve_not_configured` | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| `reserve_evidence_required` | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| `reserve_window_unavailable` | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy session bind`

Decision: Bind one exact session to a Codex pool.

### Help variant

Title: `cq codex proxy session bind`. Purpose: Bind one exact session to a Codex pool.

```text
Usage
  cq codex proxy session bind --pool POOL (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST) [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--pool POOL` | Case-insensitive pool name; preserve configured display casing. | required; once; no value |
| `--session-id SESSION_ID` | Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history. | optional; once; no value |
| `--session-id-stdin` | Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo. | optional; once; False |
| `--digest DIGEST` | Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.
- Bind replaces an existing binding or creates one; unbind removes an existing binding; publish atomically with no restart.
- `--pool`: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.
- `--session-id`: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.
- `--session-id`: Mutually exclusive with --session-id-stdin and --digest.
- `--session-id-stdin`: Input must be valid UTF-8 containing 1–4096 bytes when true.
- `--session-id-stdin`: Mutually exclusive with --session-id and --digest.
- `--digest`: 64 lowercase hexadecimal characters.
- `--digest`: Mutually exclusive with --session-id and --session-id-stdin.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy session bind --pool work --session-id example-session` — Uses a literal example session; bind needs prepared work pool.

Related commands: cq codex proxy session list; cq codex proxy session digest --help; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy session bind` → `cq codex proxy session bind`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Saved Codex session binding.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: session_binding_not_found (only show/unbind). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `session_binding_not_found` | 3 | Show or unbind selector has no binding. | Session binding not found. |
| `pool_not_found` | 3 | Bind names an absent pool. | Pool not found: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy session digest`

Decision: Return the keyed digest for a session selector.

### Help variant

Title: `cq codex proxy session digest`. Purpose: Return the keyed digest for a session selector.

```text
Usage
  cq codex proxy session digest (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST) [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--session-id SESSION_ID` | Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history. | optional; once; no value |
| `--session-id-stdin` | Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo. | optional; once; False |
| `--digest DIGEST` | Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.
- Raw identifiers are digested by authenticated live control. An existing --digest is returned unchanged; no lookup is required for that case.
- Read only; raw selector requires authenticated live digest request. Existing --digest is returned unchanged without state access.
- `--session-id`: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.
- `--session-id`: Mutually exclusive with --session-id-stdin and --digest.
- `--session-id-stdin`: Input must be valid UTF-8 containing 1–4096 bytes when true.
- `--session-id-stdin`: Mutually exclusive with --session-id and --digest.
- `--digest`: 64 lowercase hexadecimal characters.
- `--digest`: Mutually exclusive with --session-id and --session-id-stdin.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy session digest --session-id example-session` — Uses a literal example session; bind needs prepared work pool.

Related commands: cq codex proxy session list; cq codex proxy session digest --help; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy session digest` → `cq codex proxy session digest`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex session digest
  <session_digest>
```

Empty / absent state: No empty digest success. Existing --digest is pure pass-through after validation and needs no control/configuration.

Partial, stale, pending, timeout and interruption truth: No partial result. Never show raw session selector or echo stdin. Digest is exactly 64 lower-case hexadecimal characters. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: session_binding_not_found (only show/unbind); pool_not_found (only bind); all control/I/O failures when --digest passes validation (pure pass-through; raw selectors still require control). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `session_binding_not_found` | 3 | Show or unbind selector has no binding. | Session binding not found. |
| `pool_not_found` | 3 | Bind names an absent pool. | Pool not found: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy session list`

Decision: List all session-to-pool bindings.

### Help variant

Title: `cq codex proxy session list`. Purpose: List all session-to-pool bindings.

```text
Usage
  cq codex proxy session list [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- List configured session digests and their pool names; raw identifiers are never returned.
- Read only; a raw selector requires an authenticated digest request.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy session list` — Uses a literal example session; bind needs prepared work pool.

Related commands: cq codex proxy session list; cq codex proxy session digest --help; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy session list` → `cq codex proxy session list`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex session bindings
  Digest  Pool
  <session_digest>  <pool>
```

Empty / absent state: No bindings prints `No Codex session bindings configured.` with exit 0. No selector is accepted for list.

Partial, stale, pending, timeout and interruption truth: No partial list exists. Sort by full session_digest. Wrap/stack full digest; no abbreviation. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: session_binding_not_found (only show/unbind); pool_not_found (only bind). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `session_binding_not_found` | 3 | Show or unbind selector has no binding. | Session binding not found. |
| `pool_not_found` | 3 | Bind names an absent pool. | Pool not found: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy session show`

Decision: Show one session-to-pool binding.

### Help variant

Title: `cq codex proxy session show`. Purpose: Show one session-to-pool binding.

```text
Usage
  cq codex proxy session show (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST) [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--session-id SESSION_ID` | Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history. | optional; once; no value |
| `--session-id-stdin` | Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo. | optional; once; False |
| `--digest DIGEST` | Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.
- Read only; a raw selector requires an authenticated digest request.
- `--session-id`: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.
- `--session-id`: Mutually exclusive with --session-id-stdin and --digest.
- `--session-id-stdin`: Input must be valid UTF-8 containing 1–4096 bytes when true.
- `--session-id-stdin`: Mutually exclusive with --session-id and --digest.
- `--digest`: 64 lowercase hexadecimal characters.
- `--digest`: Mutually exclusive with --session-id and --session-id-stdin.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy session show --session-id example-session` — Uses a literal example session; bind needs prepared work pool.

Related commands: cq codex proxy session list; cq codex proxy session digest --help; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy session show` → `cq codex proxy session show`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Codex session binding
  Digest  <binding.session_digest>
  Pool    <binding.pool>
```

Empty / absent state: Missing binding is session_binding_not_found, exit 3, not success with `none`.

Partial, stale, pending, timeout and interruption truth: No partial result. Preserve full digest, opaque pool name and configured casing. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: pool_not_found (only bind). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `session_binding_not_found` | 3 | Show or unbind selector has no binding. | Session binding not found. |
| `pool_not_found` | 3 | Bind names an absent pool. | Pool not found: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy session unbind`

Decision: Remove one exact session-to-pool binding.

### Help variant

Title: `cq codex proxy session unbind`. Purpose: Remove one exact session-to-pool binding.

```text
Usage
  cq codex proxy session unbind (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST) [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--session-id SESSION_ID` | Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history. | optional; once; no value |
| `--session-id-stdin` | Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo. | optional; once; False |
| `--digest DIGEST` | Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest. | optional; once; no value |
| `--port PORT` | Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset. | optional; once; configured port, otherwise 19280 |
| `--timeout TIMEOUT` | Maximum elapsed time for this command, including local preparation and control requests; default 10s. | optional; once; 10s |

Behaviour content, after options:

- Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.
- Bind replaces an existing binding or creates one; unbind removes an existing binding; publish atomically with no restart.
- `--session-id`: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.
- `--session-id`: Mutually exclusive with --session-id-stdin and --digest.
- `--session-id-stdin`: Input must be valid UTF-8 containing 1–4096 bytes when true.
- `--session-id-stdin`: Mutually exclusive with --session-id and --digest.
- `--digest`: 64 lowercase hexadecimal characters.
- `--digest`: Mutually exclusive with --session-id and --session-id-stdin.
- `--port`: Integer 1–65535.
- `--port`: No configuration file is created to resolve omission.
- `--timeout`: Positive Go duration.
- `--timeout`: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

Examples, exact command text and explanatory captions:

- `cq codex proxy session unbind --session-id example-session` — Uses a literal example session; bind needs prepared work pool.

Related commands: cq codex proxy session list; cq codex proxy session digest --help; cq codex proxy policy show. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy policy session unbind` → `cq codex proxy session unbind`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
Removed Codex session binding.
<Policy detail>
```

Empty / absent state: Empty pools/bindings/evidence/delegations are `none`, not missing policy. Unpublished policy is routing_conflict, never a successful empty document. Missing selected pool/binding is declared not-found failure.

Partial, stale, pending, timeout and interruption truth: One returned policy is the acknowledged result; no row-by-row partial publish. Effective generation can lag: display amber application line. Timeout/interruption can leave commit uncertain. Full policy remains available in JSON without redesign. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Action-specific exclusions: pool_not_found (only bind). These shared catalogue entries do not create extra effects or fake failure fixtures.

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `routing_timeout` | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| `routing_control_unavailable` | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| `routing_auth_failed` | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| `routing_conflict` | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| `session_binding_not_found` | 3 | Show or unbind selector has no binding. | Session binding not found. |
| `pool_not_found` | 3 | Bind names an absent pool. | Pool not found: {name}. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## `cq codex proxy trace`

Decision: Read or follow retained Codex request traces.

### Help variant

Title: `cq codex proxy trace`. Purpose: Read or follow retained Codex request traces.

```text
Usage
  cq codex proxy trace [options]
```


Options, in displayed order (then shared globals):

| Syntax | Help | Required / repeatable / default |
|---|---|---|
| `--session SESSION` | Match a raw session ID or codex://threads/ID against privacy-safe session/thread keys. Omission matches all sessions. | optional; once; no value |
| `--trace TRACE` | Match this exact trace ID. Omission matches all trace IDs. | optional; once; no value |
| `--since SINCE` | Include records at or after command-start time minus this positive duration. Omission imposes no lower time bound. | optional; once; no value |
| `--tail TAIL` | Return the newest N matching retained records before following; default 200. Use 0 for all matching retained records. | optional; once; 200 |
| `--follow` | Continue reading new records and rotated logs until interrupted. No proxy connection is opened. | optional; once; False |
| `--payload` | Read the existing opt-in payload log instead of causal route events. This may display recorded user content; it does not enable capture. | optional; once; False |
| `--timeout TIMEOUT` | Optional maximum elapsed reading/following time; omission has no command deadline. Expiry returns exit 7. | optional; once; no value |

Behaviour content, after options:

- Read configured local log files without contacting or changing the proxy. Filters combine with logical AND; apply tail after filtering retained history.
- JSON mode is an explicit JSONL stream exception: emit one CLI v2 envelope containing data.record for each event, then exactly one terminal envelope containing data.end. Successful completion uses ok=true, errors=[], exit 0. Timeout uses ok=false with trace_timeout, exit 7; interruption uses ok=false with trace_interrupted, exit 130. No second error envelope follows a terminal envelope.
- Follow preserves records across retained rotation. If logs rotate beyond retained history, report a gap instead of silently claiming continuity. Payload mode reads already captured request/response content.
- Read local files only.
- Missing files under a configured path produce zero records; follow waits for creation.
- Malformed/nonmatching records are skipped as in the existing reader; accepted records retain chronology across log segments.
- Payload output redacts documented authentication headers and known authentication fields. Arbitrary prompt or response content may still contain secrets; no blanket redaction guarantee is made.
- `--session`: After trimming surrounding whitespace and extracting the final slash-delimited component, selector must be nonempty.
- `--session`: Use existing privacy key derivation for raw and codex://threads/ID forms; reject empty normalised input rather than dropping the filter.
- `--trace`: Nonempty trace ID.
- `--since`: Positive Go duration.
- `--tail`: Integer >=0.
- `--timeout`: Positive Go duration when specified.

Examples, exact command text and explanatory captions:

- `cq codex proxy trace --since 15m --tail 50` — Read the latest 50 matching records from the last 15 minutes.
- `cq codex proxy trace --session codex://threads/example-session --follow --json` — Follow one session using explicit JSONL envelopes.

Related commands: cq codex proxy trace --help; cq proxy --help. Self-links are omitted from rendered Related commands. Deprecated aliases: `cq proxy trace` → `cq codex proxy trace`. Alias help identifies canonical path; alias execution retains existing deprecation warning on stderr.

### Success variant · exit 0

```text
<time> <trace_id> #<sequence> <transport> <phase_or_route_summary> <nonempty_details>
```

Empty / absent state: Configured missing log: zero records, exit 0; follow waits for creation. Unconfigured selected log: trace_not_configured. No success footer, including zero records.

Partial, stale, pending, timeout and interruption truth: Existing human stream contract is frozen: keep exact token order/timestamps and no added title, table or colour bytes. Payload human stream never prints body. JSONL remains record envelopes followed by one terminal envelope; gaps exit 8, timeout 7, interruption 130. Previously printed records remain evidence, not complete history. Shared error/result rules above apply. No additional success states are implied.

### Failure variants

Each row uses the exact shared error layout; no success card accompanies it. Conditions are authoritative applicability, including family-wide codes which cannot occur for this particular action. Do not synthesise unreachable errors merely to show a variant.

| Code | Exit | Condition | Message |
|---|---:|---|---|
| `routing_invalid_argument` | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| `routing_io_failed` | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| `trace_timeout` | 7 | Optional command deadline expires. | Trace reading timed out. |
| `trace_not_configured` | 4 | Selected diagnostics log path is not configured. | Selected trace log is not configured. |
| `trace_history_gap` | 8 | Required rotation history no longer retained or ends with incomplete record. | Trace history has a gap: {detail}. |
| `trace_interrupted` | 130 | Interrupt received during follow. | Trace following interrupted. |

Numbers / times / narrow version: apply shared rules to every field in this leaf and its referenced blocks. Fields absent from its resource are not invented. Help-only and machine-output variants preserve the semantics above; redirected human output retains its existing byte contract, and JSON retains original resource names, types and nulls.


## Acceptance for this design

For every leaf above, capture a colour TTY rendering, NO_COLOR rendering and redirected rendering; help at 120/80/40/20 columns; each listed error condition reachable for that leaf; success and empty variants; and timeout/interruption where supported. Fixtures must exercise decimal reserve thresholds, unknown usage, stale usage, temporary bypass, reserve reached, zero lease invalidations, maximal uint64 generations, Unicode pool names, long account keys, full digests, differing effective/routing generations, all policy subcollections, nonempty priming overrides, absent/current/stale readiness and trace gaps. Preserve machine snapshots independently. No fixture may label configuration as runtime acknowledgement, expiry as reset proof, missing data as zero, or a trace prefix as complete history.
