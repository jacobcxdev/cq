# Accounts, quota, resets, authentication, models and utilities

Status: proposed presentation design; not implemented output or runtime evidence. This chapter covers 25 canonical leaves. The shared presentation contract in [README.md](README.md) supplies colour, terminal detection, escaping, streams and responsive layout. Existing JSON resources, exit codes, confirmation rules and command semantics remain unchanged. These frames apply to terminal output only; existing redirected/plain reference output remains byte-compatible. NO_COLOR removes styling without removing semantic words.

Placeholders in braces name actual output resource fields. `{count(...)}` and display labels are presentation-only derived values. Repeated rows are marked `[repeat …]`; these instructions are not literal output. In placeholders, `or unknown` and `or none` mean null/absent only, never boolean truthiness: numeric zero remains zero and false remains false. No sample frame asserts real account state. Values come from the single command outcome, never an additional fetch performed for decoration.

## Rendering primitives used in this chapter

- Help: bold command title and section labels; cyan command/option syntax; body in default foreground; muted annotations. Print summary, Usage, essential context, Arguments (when any), Options, Global options, Related commands, then Examples. Domain semantics below each command appear in a compact Notes paragraph; validation details belong immediately under the relevant argument or option, not a generic block of type metadata. No `Required: false`, `Repeatable: false` or redundant boolean defaults.
- The separate Global options section always includes `-h, --help  Show this help`, `-j, --json  Emit the documented JSON output; disable prompts`, `-v, --version  Show CQ version`. They follow local options; the descriptions for these globals are identical on every page. Help remains human text when `--json` is supplied, but may use normal help colour rules on a terminal.
- Quantities: right-align integer/percentage columns; exact integers with locale-independent comma grouping and explicit units (`tokens`, `credits`, `windows`). Quota display retains its established integer percentage precision; fractional percentages use the shared one-decimal rule, including <0.1% and >99.9% edge labels. Never present a missing value as zero. `unknown` means an unavailable observation; `—` means not applicable; `none` means known absence. Boolean statuses are domain words, not `true`/`false`, except when describing source fields in this specification.
- Dates: show local dates such as `23 Sep 2026, 16:30 BST (UTC+01:00)` alongside a relative duration, using the shared written-month form and retaining seconds/fractional precision when needed for event ordering, calculated against one rendering timestamp. State timezone once beside the date column heading or in the card label; UTC fallback is explicit. Remaining durations round up at the final displayed unit and elapsed ages round down; and past deadlines say `expired … ago`. Never display a relative countdown alone for credit expiry or reset deadlines. Null expiry says `No expiry`; unknown expiry says `Unknown` only when the resource distinguishes it.
- Status: green only for confirmed successful observations/commits. Native `Active` means selected native credentials, not healthy or proxy-selected. Configuration and account identity do not prove authentication. Amber for stale, incomplete, unresolved and low-confidence states; red for failed operations. Every colour has a word equivalent.
- Width: use tables only if all mandatory columns fit without truncating command-copyable identifiers. Otherwise one record per card, in the exact column order below. Full account references, credit IDs, model IDs and retry command arguments wrap onto continuation lines; never ellipsise them. Secondary lists sit under their parent row in both layouts. At narrow width, help syntax occupies its own line and the description follows indented two spaces.
- Error frame: stderr receives `Error · {exact message}` (red), then separate `Code: {code}` and `Command: cq {path}` secondary lines, followed by returned safe details. Exit 8 instead uses `Warning · {exact message}` (amber), with Partial next to the resource title. Timeout uses `Warning · {exact message}` and Timed out on the resource; interruption uses Interrupted (amber). A resource/receipt already produced stays on stdout. Never claim rollback, no changes or cancellation unless the outcome proves it. Error codes are muted identifiers, not substitutes for prose. No diagnostic prints tokens, OAuth callback secrets, raw credentials or secret-bearing paths.
- Parser rejection applies to every leaf, including those with no domain errors: exit 2, `Error · Invalid arguments: {detail}. Run cq {path} --help.`, then `Code: cli_invalid_usage · Command: cq {path}` No state access or mutation. The per-command tables repeat this rule where declared and add it explicitly otherwise.
- SIGINT: use the existing dispatcher outcome, preserving its exit and any returned receipt. Where a leaf has no separately declared interrupt error, presentation does not invent one or promise a new exit contract. If control returns with unknown mutation completion, use `Completion unknown`, not `Cancelled`. Print no fake terminal-success frame for an interrupted raw stream.

### Account details block

Where a frame names `{account details}`, expand to these exact labelled lines, omitting only the optional lines explicitly marked below:

```text
Reference       {account.account_reference or unknown}
Account ID      {account.account_id or unknown}
Email           {account.email or unknown}
Native default  {Active if account.active else Not active}
Identity        {Stable if account.stable else Unstable}
Sources         {account.sources joined with comma, or none}
Aliases         {account.aliases joined with comma, or none}
Tier            {account.rate_limit_tier or unknown}
Label           {account.label}                         [only when non-null]
```

`account.display_name` is the card title and `account.provider` supplies the provider heading. Display names are escaped terminal text. Do not infer activatable/removable status from these fields; that authority is not part of AccountSummary.

### Publication block

Where a frame names `{publication block}`, expand using the actual ModelPublication resource:

```text
Publication  {publication.status} · via {publication.via}
Models       {publication.active_count} active

Sources
Provider  Result             Native models  Malformed rows
{source.provider} {source.status} {source.native_count} {source.malformed_count}
  {source.error_code}: {source.message}                 [when non-null]

Client caches
Target           Result             Reason
{target.target}  {target.status}    {target.reason}
  Path: {target.path}
  {target.error_code}: {target.message}                 [when non-null]
```

Repeat every returned source and target; no silent omission of skipped optional targets. `written` is green, `skipped` neutral unless its reason signals incomplete work, `failed` red. An empty target list is `No cache publication results`, never `All caches updated`. Path is a model-cache path, not a credential path; escape terminal control characters. Counts reflect the receipt even if later work failed.


## `cq auth refresh`

**Decision:** Did eligible credentials refresh, and which accounts still need login?

### Help

```text
cq auth refresh
Refresh eligible stored Claude and Codex OAuth credentials.

Usage
  cq auth refresh [PROVIDER…] [options]

Arguments
  PROVIDER  Providers whose eligible OAuth credentials to refresh: claude and codex. Omitted: both, in that order. Gemini uses in-process Antigravity refresh during quota checks and is not supported here.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account login
  cq codex account login
  cq service status

Examples
  cq auth refresh
    Refresh eligible Claude and Codex credentials.
  cq auth refresh codex --json
    Refresh only eligible CQ-managed Codex credentials without interaction.
  cq auth refresh claude
    Refresh Claude credentials and offer terminal reauthentication when needed.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `providers`: Reject duplicate providers and any provider other than claude or codex.

**Notes:** Each provider appears in request order. Show all per-account rows, including skipped and login-required rows. Counts are right-aligned; changed_count counts logical accounts, not writes. No timestamps exist in this receipt, so do not invent expiry dates. For no provider account rows, display `No stored accounts for {provider}.`; the outcome, rather than presentation, decides whether this is success or failure. Claude prompt remains the existing Enter/browser or s/skip prompt; print account name before it. No whole-command spinner claims a deadline; request timeout is 10s and browser callback timeout is 5m.

### Success and no-op

```text
Authentication refresh
Provider  Refreshed  Unchanged  Need login  Failed
{provider} {refreshed} {unchanged} {reauth_required} {failed}
  {account.account_label} · {account.status} · {account.reason}
    Reference: {account.account_key or unknown}
    Credentials: {Saved if account.credentials_changed else Unchanged}
    {account.error_code}: {account.message} [when present]

Credentials saved for {changed_count} accounts
```

### Empty, absent and inapplicable states

No eligible credentials: `No credentials needed refreshing.` with zero counts and unchanged account reasons. No configured account is not represented as a successful refresh.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| auth_refresh_failed | 5 | Credentials require authentication; inspect provider results. | All eligible refresh attempts fail authentication or all remaining work requires reauthentication. Red Error heading; preserve any returned receipt. |
| auth_refresh_partial | 8 | Credential refresh completed partially; inspect provider results. | Some refresh work succeeds and some fails or requires reauthentication. Amber Partial heading; preserve successful rows and committed receipts. |
| auth_authority_unavailable | 4 | Codex credential authority is unavailable. | Selected Codex credential authority cannot be reached or opened. Red Error heading; preserve any returned receipt. |
| auth_inventory_degraded | 6 | Codex credential inventory is degraded; refresh was not attempted. | Codex external-source inventory has a nonoptional error. Red Error heading; preserve any returned receipt. |
| auth_store_failed | 1 | Cannot persist refreshed credentials for {account_label}. | Refreshed credentials cannot be persisted. Red Error heading; preserve any returned receipt. |
| auth_interrupted | 130 | Credential refresh interrupted. | Command is interrupted; successful earlier writes remain committed. Amber Interrupted heading; prior committed writes remain committed. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `providers`: array<AuthRefreshProviderResult>: one result per selected provider in requested order.
- `credentials_changed`: boolean: at least one credential persistence committed, including reconciliation or partial success.
- `changed_count`: integer >= 0: number of distinct logical accounts with at least one committed credential change.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq check`

**Decision:** Can I push harder, or should I slow down before a quota window resets?

### Help

```text
cq check
Report quota usage for selected providers.

Usage
  cq check [PROVIDER…] [options]

Arguments
  PROVIDER  Providers to check: claude, codex, gemini. Omit to check all three in this order.

Options
  --fresh  Fetch fresh quota data instead of initially reading the quota cache. Default: false.
  --timeout DURATION  Maximum elapsed time, including locks, requests and local verification. Default: 30s.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex account list
  cq codex reset recommend
  cq codex proxy reserve status

Examples
  cq check codex
    Check Codex only.
  cq check claude codex --fresh --json
    Fetch selected providers and emit a v2 JSON envelope.
  cq
    Check all supported providers.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `providers`: Exact lowercase IDs; duplicates invalid; one or more values when supplied.
- `--fresh`: Does not refresh OAuth credentials as a separate operation. Existing provider refresh needed for a fetch still applies.
- `--timeout`: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Notes:** Preserve installed quota dashboard bars, weighted account value, existing forecast arithmetic, model-specific subwindows and active-first human ordering. Keep JSON ordering untouched. Add absolute reset detail below each applicable window rather than replacing readable countdowns with timestamps. Unknown forecast says unknown; do not translate exhausted and unavailable into the same word. Hide duplicate proxy-eligible aggregate only when existing renderer already establishes equivalence. Narrow mode stacks remaining, reset, pace and burndown labels under each window; never shows raw internal floating-point sustainability or seconds as the primary display.

### Success and no-op

```text
{Provider display name} {plan/tier} · {account email or safe account label}
{window.name}  {existing quota bar}  {remaining_pct}%  {reset countdown}  {pace}  {burndown}
  Reset: {window.reset_at formatted absolute+relative}
  {additional named windows grouped below their account}

{Provider display name} {aggregate capacity label}
{window.name}  {existing aggregate bar}  {remaining_pct}%  {existing gauge}  {waste or risk}
  Observed: {report.generated_at formatted absolute+relative}
  {stale warning when returned}
```

### Empty, absent and inapplicable states

An unconfigured provider is a compact `{Provider} · not configured` row. Unconfigured providers alongside usable results do not create a partial failure. No usable selected provider produces the exact unavailable/authentication/failure outcome below. Exhausted quota is a successful observation and displays 0%, not an error.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| check_partial | 8 | Some quota results could not be refreshed. | At least one usable provider result and at least one provider/account failed or stale-fallback row was used. Amber Partial heading; preserve successful rows and committed receipts. |
| check_unavailable | 4 | No quota results are available. | No usable quota result, and failures are absent configuration/unavailable integration. Red Error heading; preserve any returned receipt. |
| check_authentication | 5 | Authentication is required for the selected providers. | No usable result and at least one authentication failure. Red Error heading; preserve any returned receipt. |
| check_failed | 1 | Quota checks failed for the selected providers. | No usable result, no authentication failure, and at least one operational fetch/parse/panic failure. This takes precedence over check_unavailable. Red Error heading; preserve any returned receipt. |
| check_timeout | 7 | Quota checking timed out. | Command-wide deadline expires, regardless of whether partial rows were collected; retain any completed report rows. Amber Timed out heading; retain completed observations and known mutations. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `report`: QuotaReportV1: frozen quota report schema in root notes; generated_at timestamp and provider results, aggregates and routing-scope summaries..

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq claude account activate`

**Decision:** Did the native Claude default change to the selected identity?

### Help

```text
cq claude account activate
Select the native client default Claude account.

Usage
  cq claude account activate ACCOUNT [options]

Arguments
  ACCOUNT  Select one Claude account by its unique email, matched case-insensitively after trimming. Ambiguous identities are rejected; no alias or opaque-key syntax is accepted.

Options
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude proxy pin show

Examples
  cq claude account activate user@example.com
    Select the unique account with this email.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** changed=true is a confirmed mutation receipt; changed=false is neutral no-op. Partial activation must say credentials changed and metadata requires recovery. On timeout/error without a reliable changed field use Completion unknown; never infer unchanged. No proxy pin/session claim. No numbers or dates added.

### Success and no-op

```text
Claude native default · {Updated if changed else Already selected}
{account.display_name}
{account details}
Existing clients may need reconnection.
```

### Empty, absent and inapplicable states

Already-active account: successful no-op, `Already selected`; do not print Updated. No match is account_not_found, never empty success.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. Canonical wording mentions an exact key, but Claude accepts unique email only: append `Claude accepts a unique email; inspect cq claude account list.` Do not invent Claude key support. This existing wording conflict requires a separate semantic correction. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| account_not_activatable | 6 | Account has no managed credentials that can be activated. | Target has no eligible managed activation candidate. Red Error heading; preserve any returned receipt. |
| account_activation_partial | 8 | Native client credentials changed, but account metadata needs recovery. | System auth changed but metadata projection failed. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account`: AccountSummary.
- `changed`: boolean: false when already active.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq claude account list`

**Decision:** Which Claude identities exist, and which is the native default?

### Help

```text
cq claude account list
List locally configured Claude accounts.

Usage
  cq claude account list [options]

Options
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account login
  cq claude account activate
  cq check claude

Examples
  cq claude account list
    Inspect configured accounts and native default.
  cq claude account list --json
    Read stable structured account metadata.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Show every AccountSummary field through the details block, even if values are unknown. Stable ordering remains account_reference then display_name. Native-default status is independent of source and stability. No quota calls, timestamps or expiry estimates. Narrow cards use display name, then details block; no truncation.

### Success and no-op

```text
Claude accounts · {count(accounts)} accounts
Account  Native default  Tier  Identity
{account.display_name} {Active if account.active else —} {account.rate_limit_tier or unknown} {Stable if account.stable else Unstable}
  {account details}
```

### Empty, absent and inapplicable states

`No Claude accounts found.` followed by `Run cq claude account login to add one.` Empty accounts is exit 0. No quota or authentication claim.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `accounts`: AccountSummary[]; stable order by account_reference, then display_name.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq claude account login`

**Decision:** Were Claude credentials saved and was requested native activation completed?

### Help

```text
cq claude account login
Authenticate and store a Claude account.

Usage
  cq claude account login [options]

Options
  --activate  Make this account the native client default after successful login (default false). Existing clients may need reconnection.
  --timeout DURATION  Limit total command work to this duration (default 10m; minimum 1s; maximum 30m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude account activate

Examples
  cq claude account login
    Store an additional account without activation.
  cq claude account login --activate
    Authenticate and select the account as native client default.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 30m

**Notes:** Browser progress is a separate stderr line `Waiting for browser authentication…`; do not print callback URL/state/code. Preserve existing safe browser-flow instructions. Saved credentials remain a green fact under amber partial heading if activation/postcheck fails. Existing clients may need reconnection only when activation was requested. No invented expiry/timestamp metadata. Timeout may occur after save: retain actual receipt and show unknown activation where necessary.

### Success and no-op

```text
Claude login · {Completed when save and requested activation verified}
Account         {account.display_name or Unverified}
Credentials     {Saved if credentials_saved else Not saved}
Native default  {Selected if activated=true else Not selected if activated=false else Unknown}
{account details when account non-null}
```

### Empty, absent and inapplicable states

There is no successful empty login. account=null after committed save is a partial postcheck result, not a fabricated identity. Without --activate, saved account plus Not selected is success.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_auth_failed | 5 | Browser authentication failed. | OAuth rejected, expired or cancelled by provider. Red Error heading; preserve any returned receipt. |
| account_login_partial | 8 | Credentials were saved, but native client activation failed. | Credentials were saved but requested activation failed. Amber Partial heading; preserve successful rows and committed receipts. |
| account_login_postcheck_partial | 8 | Credentials were saved, but account state could not be verified. | Credentials were saved but the final account-state observation or metadata postcheck failed. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account`: AccountSummary or null only when post-save account observation failed.
- `activated`: boolean or null: whether native client default was selected; null only for an indeterminate attempted activation.
- `credentials_saved`: boolean: true once durable save completed.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq claude account remove`

**Decision:** Were local Claude credentials removed, and which externally managed sources remain?

### Help

```text
cq claude account remove
Remove local Claude account credentials.

Usage
  cq claude account remove ACCOUNT [options]

Arguments
  ACCOUNT  Select one Claude account by its unique email, matched case-insensitively after trimming. Ambiguous identities are rejected; no alias or opaque-key syntax is accepted.

Options
  --yes  Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m). Time spent waiting for terminal confirmation is excluded.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude account login

Examples
  cq claude account remove user@example.com
    Preview and confirm local credential removal.
  cq claude account remove user@example.com --yes --json
    Remove explicitly confirmed local credentials and emit JSON.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--yes`: Only this operation is authorised; no future operation inherits consent.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Preview resolved account and native credential impact on stderr, then exact existing `[y/N]` prompt. Partial removal retains each known mutation; missing result does not mean Retained. Never claim upstream account/subscription deletion. External source labels only; no credential paths. No numeric/time fields in receipt. Narrow layout uses these same labelled lines.

### Success and no-op

```text
Claude local credentials · {Removed if removed else Cancelled only for confirmed cancellation}
Reference           {account_reference}
Native credentials  {Removed if active_credentials_removed else Retained}
External sources    {retained_external_sources joined with comma, or none}
```

### Empty, absent and inapplicable states

Negative/blank response: `Removal cancelled. No credentials removed.` only on pre-mutation cancellation. External-only target is account_read_only, not a misleading success. No match remains exit 3.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. Canonical wording mentions an exact key, but Claude accepts unique email only: append `Claude accepts a unique email; inspect cq claude account list.` Do not invent Claude key support. This existing wording conflict requires a separate semantic correction. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| account_confirmation_required | 6 | Local credential removal requires --yes in non-interactive mode. | --json or non-terminal stdin without --yes. Red Error heading; preserve any returned receipt. |
| account_read_only | 6 | Account credentials are externally managed and cannot be removed by CQ. | Only external read-only credential sources exist. Red Error heading; preserve any returned receipt. |
| account_removal_partial | 8 | Local credential removal is incomplete; recover the pending operation before retrying. | Some credential mutations completed but durable removal requires recovery. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account_reference`: string: resolved input reference.
- `removed`: boolean: false on interactive cancellation.
- `active_credentials_removed`: boolean.
- `retained_external_sources`: string[]: external source labels only, no credential paths.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex account activate`

**Decision:** Did the native Codex default change to the selected identity?

### Help

```text
cq codex account activate
Select the native client default Codex account.

Usage
  cq codex account activate ACCOUNT [options]

Arguments
  ACCOUNT  Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Options
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex account list
  cq codex proxy pin show

Examples
  cq codex account activate user@example.com
    Select the unique account with this email.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** changed=true is a confirmed mutation receipt; changed=false is neutral no-op. Partial activation must say credentials changed and metadata requires recovery. On timeout/error without a reliable changed field use Completion unknown; never infer unchanged. No proxy pin/session claim. No numbers or dates added.

### Success and no-op

```text
Codex native default · {Updated if changed else Already selected}
{account.display_name}
{account details}
Existing clients may need reconnection.
```

### Empty, absent and inapplicable states

Already-active account: successful no-op, `Already selected`; do not print Updated. No match is account_not_found, never empty success.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| account_not_activatable | 6 | Account has no managed credentials that can be activated. | Target has no eligible managed activation candidate. Red Error heading; preserve any returned receipt. |
| account_activation_partial | 8 | Native client credentials changed, but account metadata needs recovery. | System auth changed but metadata projection failed. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account`: AccountSummary.
- `changed`: boolean: false when already active.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex account list`

**Decision:** Which Codex identities exist, and which is the native default?

### Help

```text
cq codex account list
List locally configured Codex accounts.

Usage
  cq codex account list [options]

Options
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex account login
  cq codex account activate
  cq check codex

Examples
  cq codex account list
    Inspect configured accounts and native default.
  cq codex account list --json
    Read stable structured account metadata.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Show every AccountSummary field through the details block, even if values are unknown. Stable ordering remains account_reference then display_name. Native-default status is independent of source and stability. No quota calls, timestamps or expiry estimates. Narrow cards use display name, then details block; no truncation.

### Success and no-op

```text
Codex accounts · {count(accounts)} accounts
Account  Native default  Tier  Identity
{account.display_name} {Active if account.active else —} {account.rate_limit_tier or unknown} {Stable if account.stable else Unstable}
  {account details}
```

### Empty, absent and inapplicable states

`No Codex accounts found.` followed by `Run cq codex account login to add one.` Empty accounts is exit 0. No quota or authentication claim.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `accounts`: AccountSummary[]; stable order by account_reference, then display_name.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex account login`

**Decision:** Were Codex credentials saved and was requested native activation completed?

### Help

```text
cq codex account login
Authenticate and store a Codex account.

Usage
  cq codex account login [options]

Options
  --activate  Make this account the native client default after successful login (default false). Existing clients may need reconnection.
  --timeout DURATION  Limit total command work to this duration (default 10m; minimum 1s; maximum 30m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex account list
  cq codex account activate

Examples
  cq codex account login
    Store an additional account without activation.
  cq codex account login --activate
    Authenticate and select the account as native client default.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 30m

**Notes:** Browser progress is a separate stderr line `Waiting for browser authentication…`; do not print callback URL/state/code. Preserve existing safe browser-flow instructions. Saved credentials remain a green fact under amber partial heading if activation/postcheck fails. Existing clients may need reconnection only when activation was requested. No invented expiry/timestamp metadata. Timeout may occur after save: retain actual receipt and show unknown activation where necessary.

### Success and no-op

```text
Codex login · {Completed when save and requested activation verified}
Account         {account.display_name or Unverified}
Credentials     {Saved if credentials_saved else Not saved}
Native default  {Selected if activated=true else Not selected if activated=false else Unknown}
{account details when account non-null}
```

### Empty, absent and inapplicable states

There is no successful empty login. account=null after committed save is a partial postcheck result, not a fabricated identity. Without --activate, saved account plus Not selected is success.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_auth_failed | 5 | Browser authentication failed. | OAuth rejected, expired or cancelled by provider. Red Error heading; preserve any returned receipt. |
| account_login_partial | 8 | Credentials were saved, but native client activation failed. | Credentials were saved but requested activation failed. Amber Partial heading; preserve successful rows and committed receipts. |
| account_login_postcheck_partial | 8 | Credentials were saved, but account state could not be verified. | Credentials were saved but the final account-state observation or metadata postcheck failed. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account`: AccountSummary or null only when post-save account observation failed.
- `activated`: boolean or null: whether native client default was selected; null only for an indeterminate attempted activation.
- `credentials_saved`: boolean: true once durable save completed.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex account remove`

**Decision:** Were local Codex credentials removed, and which externally managed sources remain?

### Help

```text
cq codex account remove
Remove local Codex account credentials.

Usage
  cq codex account remove ACCOUNT [options]

Arguments
  ACCOUNT  Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Options
  --yes  Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m). Time spent waiting for terminal confirmation is excluded.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex account list
  cq codex account login

Examples
  cq codex account remove user@example.com
    Preview and confirm local credential removal.
  cq codex account remove user@example.com --yes --json
    Remove explicitly confirmed local credentials and emit JSON.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--yes`: Only this operation is authorised; no future operation inherits consent.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Preview resolved account and native credential impact on stderr, then exact existing `[y/N]` prompt. Partial removal retains each known mutation; missing result does not mean Retained. Never claim upstream account/subscription deletion. External source labels only; no credential paths. No numeric/time fields in receipt. Narrow layout uses these same labelled lines.

### Success and no-op

```text
Codex local credentials · {Removed if removed else Cancelled only for confirmed cancellation}
Reference           {account_reference}
Native credentials  {Removed if active_credentials_removed else Retained}
External sources    {retained_external_sources joined with comma, or none}
```

### Empty, absent and inapplicable states

Negative/blank response: `Removal cancelled. No credentials removed.` only on pre-mutation cancellation. External-only target is account_read_only, not a misleading success. No match remains exit 3.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| account_inventory_unavailable | 4 | Account inventory is unavailable. | Credential inventory cannot be read. Red Error heading; preserve any returned receipt. |
| account_timeout | 7 | Account command timed out. | Total deadline expires before a terminal outcome. Amber Timed out heading; retain completed observations and known mutations. |
| account_io_failed | 1 | Account state could not be saved. | Credential or registry persistence fails. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| account_confirmation_required | 6 | Local credential removal requires --yes in non-interactive mode. | --json or non-terminal stdin without --yes. Red Error heading; preserve any returned receipt. |
| account_read_only | 6 | Account credentials are externally managed and cannot be removed by CQ. | Only external read-only credential sources exist. Red Error heading; preserve any returned receipt. |
| account_removal_partial | 8 | Local credential removal is incomplete; recover the pending operation before retrying. | Some credential mutations completed but durable removal requires recovery. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account_reference`: string: resolved input reference.
- `removed`: boolean: false on interactive cancellation.
- `active_credentials_removed`: boolean.
- `retained_external_sources`: string[]: external source labels only, no credential paths.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex reset list`

**Decision:** Which credits are available, for which accounts, and when do they expire?

### Help

```text
cq codex reset list
List Codex banked reset credits.

Usage
  cq codex reset list [ACCOUNT] [options]

Arguments
  ACCOUNT  Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected. Omit to include every visible account.

Options
  --timeout DURATION  Limit total command work to this duration (default 60s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex reset recommend
  cq codex reset use

Examples
  cq codex reset list
    List credits for every visible account.
  cq codex reset list user@example.com --json
    Inspect one account as JSON.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Use exact credit IDs. Show granted/expiry with absolute+relative dates; show past expiry as expired even when source status differs, retaining source status separately. No credit-consumption prompt, no mutation receipt and no automatic use. If incomplete, amber heading and preserve every inspected row.

### Success and no-op

```text
Codex reset credits · {Complete if complete else Incomplete}
{account.email or account.account_reference or Unknown account}
  Reference: {account.account_reference or unknown}
  Account ID: {account.account_id or unknown}
  Available: {account.available_count or unknown} credits
  Credit ID  Type  Status  Supported  Granted  Expires
  {credit.id} {credit.reset_type} {credit.status} {Yes/No} {credit.granted_at} {credit.expires_at or No expiry}
    {credit.title} [when present]
    {credit.description} [when present]
  {error.code}: {error.message} · entry {error.entry_index} [when present]
```

### Empty, absent and inapplicable states

No accounts: `No Codex accounts found.` A configured account with no credits: `No reset credits.` Zero available credits remains `0 credits`, distinct from unknown count `unknown`. Malformed rows stay visible as errors, even if other credits are valid.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| codex_reset_credentials_unavailable | 4 | Codex reset credentials are unavailable. | Visible credential inventory cannot be obtained. Red Error heading; preserve any returned receipt. |
| codex_reset_auth_failed | 5 | Codex reset authentication failed. | All eligible credential candidates are rejected for authentication. Red Error heading; preserve any returned receipt. |
| codex_reset_timeout | 7 | Codex reset command timed out. | Command deadline expires. Amber Timed out heading; retain completed observations and known mutations. |
| codex_reset_upstream_failed | 1 | Codex reset request failed. | Provider rejects the request for a non-authentication reason. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| codex_reset_inventory_partial | 8 | Reset-credit inventory is incomplete. | One or more accounts or credit rows could not be read or validated. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `accounts`: ResetAccountInventory[].
- `complete`: boolean: every account and credit row was inspected successfully.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex reset recommend`

**Decision:** When should I use each credit, and how reliable is that advice?

### Help

```text
cq codex reset recommend
Recommend when to use Codex reset credits.

Usage
  cq codex reset recommend [options]

Options
  --timeout DURATION  Limit total command work to this duration (default 120s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex reset list
  cq codex reset use

Examples
  cq codex reset recommend
    Compute advisory timing for every visible account.
  cq codex reset recommend --json --timeout 120s
    Read a complete or explicitly blocked machine-readable schedule.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Use absolute+relative use-at/use-by, generated and horizon timestamps. Never colour a speculative recommendation as a completed reset. Restore percentages are labelled by window; gap avoided is a duration, not a date. Objective labels below disclose optimisation metrics as secondary information: unmet demand is percentage-seconds, not a normal percentage; restored and discarded figures retain portfolio percentage-point meaning. No automatic reset use, confirmation or consumption receipt.

### Success and no-op

```text
Codex reset plan · advisory
Inputs      {Complete if schedule.complete else Incomplete}
Search      {Exact if schedule.exact else Bounded}
Confidence  {schedule.confidence}
Generated   {schedule.generated_at}
Horizon     {schedule.horizon}

Account  Credit  Recommendation  Use at  Use by  Confidence  Gap avoided
{item.account_email or item.account_reference} {item.credit_id} {item.status} {item.use_at or Not scheduled} {item.use_by or Not scheduled} {item.confidence} {item.avoided_gap_seconds formatted duration}
  Reference: {item.account_reference}
  Account ID: {item.account_id or unknown}
  Restored: {window.name} {window.percentage}% [repeat restored_pct]
  Reasons: {item.reason_codes joined with comma}

Objective
Unmet demand       {schedule.objective.unmet_demand_pct_seconds} %-seconds
Gap duration       {schedule.objective.gap_duration_seconds formatted duration}
Useful credits expired unused  {schedule.objective.useful_expired_unused}
Weighted discarded quota       {schedule.objective.weighted_discarded_pct}%
Restored quota                 {schedule.objective.restored_pct}%

Blockers
{blocker.code} · {blocker.account_email or blocker.account_reference or Portfolio}
```

### Empty, absent and inapplicable states

No actionable items: `No reset use recommended.` This says nothing about credit availability unless inventory supports it. Incomplete inputs: `Recommendation unavailable until blockers are resolved.` Keep blockers; no actionable use_at values. Exact=false and low confidence alone are not failure.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| codex_reset_credentials_unavailable | 4 | Codex reset credentials are unavailable. | Visible credential inventory cannot be obtained. Red Error heading; preserve any returned receipt. |
| codex_reset_auth_failed | 5 | Codex reset authentication failed. | All eligible credential candidates are rejected for authentication. Red Error heading; preserve any returned receipt. |
| codex_reset_timeout | 7 | Codex reset command timed out. | Command deadline expires. Amber Timed out heading; retain completed observations and known mutations. |
| codex_reset_upstream_failed | 1 | Codex reset request failed. | Provider rejects the request for a non-authentication reason. Red Error heading; preserve any returned receipt. |
| codex_reset_recommendation_incomplete | 8 | Reset recommendation is incomplete; resolve the reported blockers. | Fresh portfolio inputs are missing or invalid. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `schedule`: ResetSchedule.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq codex reset use`

**Decision:** Was this exact credit consumed, and what changed?

### Help

```text
cq codex reset use
Consume one Codex banked reset credit.

Usage
  cq codex reset use ACCOUNT [options]

Arguments
  ACCOUNT  Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Options
  --credit CREDIT_ID  Consume this exact credit ID. Omit to resume the single unresolved attempt or choose the eligible credit with earliest expiry; no-expiry credits sort last and ties use bytewise credit ID order.
  --yes  Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.
  --timeout DURATION  Limit total command work to this duration (default 120s; minimum 1s; maximum 10m). A timeout never authorises a second reset consumption. Time spent waiting for terminal confirmation is excluded.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq codex reset list
  cq codex reset recommend

Examples
  cq codex reset use user@example.com
    Preview and confirm the next eligible reset.
  cq codex reset use user@example.com --credit "$CREDIT_ID" --yes --json
    Consume or safely replay the exact inventoried credit; prepare CREDIT_ID as described in notes.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `account`: Non-empty if supplied. Resolve against one current provider inventory; never choose the first ambiguous match.
- `--credit`: Non-empty, no surrounding whitespace. Must belong to selected account. Must be supported codex_rate_limits and eligible, or be the exact unresolved replay credit.
- `--yes`: Only this operation is authorised; no future operation inherits consent.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** Before consumption preserve existing preview and exact confirmation prompt, account, exact credit, current shared windows and recommendation on stderr. Show the retry command without --yes: rerunning still requires consent. For indeterminate, amber receipt plus red error; never propose a different credit. Natural reset dates remain unchanged by semantics; display returned before/after facts and never recalculate. Postcheck partial retains terminal reset result prominently. No new reset may be triggered merely to make a preview.

### Success and no-op

```text
Codex reset · {outcome display label}
Account  {account_reference}
Credit   {credit_id or Not selected}
Windows  {windows_reset} reset

Window  Remaining before  Remaining after  Natural reset
{change.name} {change.before.remaining_pct}% {change.after.remaining_pct}% {change.after.reset_at}
  Before reset: {change.before.reset_at}
  Period before: {change.before.period_seconds formatted duration}
  Period after:  {change.after.period_seconds formatted duration}

Retry only this request [only when retry non-null]
  cq codex reset use {shell-quoted retry.account_reference} --credit {shell-quoted retry.credit_id}
```

### Empty, absent and inapplicable states

Outcome cancelled: `Reset cancelled. No credit consumed.` only for confirmed pre-consumption cancellation. nothing_to_reset: `Nothing to reset.`; no_credit: `No eligible reset credit.`; already_redeemed: `This reset request was already completed.`; reset: `Reset completed.`; indeterminate: `Reset completion unknown.` Never reinterpret windows_reset=0 as proof that consumption did not occur.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| codex_reset_credentials_unavailable | 4 | Codex reset credentials are unavailable. | Visible credential inventory cannot be obtained. Red Error heading; preserve any returned receipt. |
| codex_reset_auth_failed | 5 | Codex reset authentication failed. | All eligible credential candidates are rejected for authentication. Red Error heading; preserve any returned receipt. |
| codex_reset_timeout | 7 | Codex reset command timed out. | Command deadline expires. Amber Timed out heading; retain completed observations and known mutations. |
| codex_reset_upstream_failed | 1 | Codex reset request failed. | Provider rejects the request for a non-authentication reason. Red Error heading; preserve any returned receipt. |
| account_reference_empty | 2 | Account reference must not be empty. | ACCOUNT contains only whitespace. Red Error heading; preserve any returned receipt. |
| account_not_found | 3 | Account reference does not resolve. | No logical account matches ACCOUNT. Red Error heading; preserve any returned receipt. |
| account_ambiguous | 6 | Account reference is ambiguous; use an exact account key. | Reference matches more than one logical account. Red Error heading; preserve any returned receipt. |
| account_unstable | 6 | Account reference resolves to an unstable account. | Matched account lacks stable identity. Red Error heading; preserve any returned receipt. |
| codex_reset_confirmation_required | 6 | Reset consumption requires --yes in non-interactive mode. | --json or non-terminal stdin without --yes. Red Error heading; preserve any returned receipt. |
| codex_reset_credit_not_found | 3 | Selected reset credit is unavailable; no replacement credit was consumed. | Explicit credit is absent or changed after confirmation. Red Error heading; preserve any returned receipt. |
| codex_reset_credit_ineligible | 6 | Selected reset credit is not eligible. | Credit expired, unsupported, already terminal without replay, or not yet eligible. Red Error heading; preserve any returned receipt. |
| codex_reset_pending_ambiguous | 6 | Several reset attempts are unresolved; specify --credit. | Several unresolved attempts exist and --credit is omitted. Red Error heading; preserve any returned receipt. |
| codex_reset_attempt_unavailable | 1 | Reset attempt state is unavailable; no consumption was started. | Durable attempt cannot be saved/read. Red Error heading; preserve any returned receipt. |
| codex_reset_consume_indeterminate | 1 | Reset outcome is unknown; retry only the same account and credit. | Consumption may have occurred but response is inconclusive. Red Error heading; preserve any returned receipt. Completion unknown; print exact same-account/same-credit retry guidance only. |
| codex_reset_postcheck_partial | 8 | Reset outcome is known, but local verification or cleanup is incomplete. | Consumption is terminal but usage verification, history, cache or attempt cleanup fails. Amber Partial heading; preserve successful rows and committed receipts. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `account_reference`: string.
- `credit_id`: string|null: null only when selection never completed.
- `outcome`: enum(cancelled,reset,already_redeemed,nothing_to_reset,no_credit,indeterminate).
- `windows_reset`: integer >= 0; provider-reported count, 0 on cancellation/indeterminate.
- `changed_windows`: ResetWindowChange[].
- `retry`: ResetRetry|null: present only for unresolved consumption.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq completion`

**Decision:** Obtain a shell script without contaminating executable output.

### Help

```text
cq completion
Print a shell completion script.

Usage
  cq completion SHELL [options]

Arguments
  SHELL  Shell to generate for: bash, zsh, fish.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq help

Examples
  cq completion zsh > "$HOME/.cq-completion.zsh"
    Write the generated script to a user-chosen file; source it in Zsh startup configuration.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `shell`: Exact lowercase enum.

**Notes:** Raw stdout exception: no colour, heading, count, success badge, installation suggestion or surrounding fence, even on TTY. With --json use existing shell/script envelope unchanged. Human --help uses designed help. No tables, quantities, dates, prompts, mutation, partial domain result or domain timeout.

### Success and no-op

```text
{script exactly as generated, ending LF}
```

### Empty, absent and inapplicable states

No empty successful script. Invalid or missing shell is a parser error, exit 2. No provider configuration required.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|

**Timeout/interruption/receipt boundary:** No domain timeout or partial state is declared. Parser errors and actual dispatcher interruption remain visible; do not fabricate a successful result after interruption.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `shell`: enum bash|zsh|fish.
- `script`: string UTF-8 complete shell script ending LF.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq gemini account show`

**Decision:** Is externally managed Gemini identity present, and what identity can CQ safely see?

### Help

```text
cq gemini account show
Show the externally managed Gemini account.

Usage
  cq gemini account show [options]

Options
  --timeout DURATION  Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq check gemini

Examples
  cq gemini account show
    Inspect the single Antigravity-managed identity.
  cq gemini account show --json
    Inspect configuration without contacting Gemini.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--timeout`: Go duration syntax; 1s <= value <= 10m

**Notes:** No authentication, switch or removal action. Configured remains neutral rather than healthy green. Email missing displays unknown, not an error. No numbers/dates in this resource beyond optional tier identifier. Timeout retains only actually returned safe account fields; no partial object fabricated.

### Success and no-op

```text
Gemini account · Configured
{account.display_name}
{account details}
Managed by Antigravity · credentials not validated by this command
```

### Empty, absent and inapplicable states

`configured=false`: `Gemini account · Not configured` and `Set up authentication in Antigravity.` This is successful local inspection, exit 0. Do not print an invented Unknown account row when account is null.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| gemini_account_unavailable | 4 | Antigravity account configuration could not be read. | Platform credential reader is unavailable or denies access. Red Error heading; preserve any returned receipt. |
| gemini_account_timeout | 7 | Antigravity account inspection timed out. | Local inspection deadline expires. Amber Timed out heading; retain completed observations and known mutations. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `configured`: boolean: external credential entry exists.
- `account`: AccountSummary|null: null when unconfigured.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq help`

**Decision:** Find syntax, command scope and related operations without reading state.

### Help

```text
cq help
Show help for a command path.

Usage
  cq help [COMMAND…] [options]

Arguments
  COMMAND  Zero or more canonical command words, or a recognised legacy command path.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq completion

Examples
  cq help codex proxy reserve set
    Show exact reserve-setting syntax.
  cq help
    Show root help.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `command-path`: Resolve exact full group/leaf path; unknown child fails before state access.

**Notes:** Use exactly the resolved page design, no `Help generated` receipt and no second heading. Legacy command paths resolve canonical help with migration information according to discovery design. No quantities/dates, prompts, mutation, timeout or domain partial state. --json does not turn help into JSON.

### Success and no-op

```text
{resolved canonical root, group or leaf help page}
```

### Empty, absent and inapplicable states

No command path resolves root help; this is not an empty result. Unknown command word is parser failure, never fallback to root or empty success.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|

**Timeout/interruption/receipt boundary:** No domain timeout or partial state is declared. Parser errors and actual dispatcher interruption remain visible; do not fabricate a successful result after interruption.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- No separate result resource: the resolved help page is the output.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq models list`

**Decision:** Which model IDs route to each provider, and what limits and provenance do they have?

### Help

```text
cq models list
List active models from local registry caches.

Usage
  cq models list [options]

Options
  --provider PROVIDER  Filter by claude or codex. Omitted: list both providers. Legacy anthropic means claude.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq models refresh
  cq models overlay add
  cq models overlay remove

Examples
  cq models list
    List cached models for both providers.
  cq models list --provider claude --json
    List Claude models as structured JSON.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--provider`: Reject other values; translate legacy anthropic to claude with deprecation warning.

**Notes:** Token limits exact grouped integers, not rounded 128k notation. Priority is an ordinal integer, not a capacity. Model IDs and aliases retain case. All metadata shown in secondary detail lines, avoiding 14-column tables. No refresh/mutation performed. Narrow cards start `{provider}/{id}`, followed by source, each token capacity and the same metadata order.

### Success and no-op

```text
Models · {provider or All providers} · {count(models)} models
Provider  Model ID  Source  Context tokens  Max context tokens  Max output tokens
{model.provider} {model.id} {model.source} {model.context_window or unknown} {model.max_context_window or unknown} {model.max_output_tokens or unknown}
  Name: {model.display_name or unknown}
  Description: {model.description or unknown}
  Aliases: {model.aliases joined with comma, or none}
  Visibility: {model.visibility or unknown} · Priority: {model.priority or unknown}
  Clone source: {model.clone_from or none}
  Inferred source: {model.inferred_from or none}
```

### Empty, absent and inapplicable states

`No models in the local registry.` Add `Run cq models refresh to populate it.` If filtered, `No {provider} models in the local registry.` No stale date or freshness claim: the resource has no observation timestamp.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| models_store_failed | 1 | Cannot access model registry state: {detail}. | Cannot read, decode, or atomically write model state. Red Error heading; preserve any returned receipt. |

**Timeout/interruption/receipt boundary:** No domain timeout or partial state is declared. Parser errors and actual dispatcher interruption remain visible; do not fabricate a successful result after interruption.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `models`: array<ModelEntry>: active merged entries in deterministic order.
- `provider`: claude|codex|null: applied filter.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq models overlay add`

**Decision:** Was my overlay saved, where did metadata come from, and which caches received it?

### Help

```text
cq models overlay add
Add or replace one local model overlay and publish caches.

Usage
  cq models overlay add [options]

Options
  --provider PROVIDER  Provider to modify: claude or codex. Required. Legacy value anthropic is accepted as claude.
  --id ID  Exact model ID to add or remove. Required; case-sensitive and non-empty; leading or trailing whitespace is rejected.
  --clone-from CLONE_FROM  Exact native model ID from the same provider to copy missing metadata from. Omitted: infer a source deterministically by model-name similarity. An explicit source absent from the validated native snapshot is an error.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq models list
  cq models refresh
  cq models overlay remove

Examples
  cq models overlay add --provider codex --id "$NEW_MODEL" --clone-from "$SOURCE_MODEL"
    Use the verified source model and desired new model ID prepared in the notes.
  cq models overlay add --provider claude --id claude-sonnet-next
    Infer metadata from available native Claude models.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--provider`: Reject other values; translate legacy anthropic to claude with deprecation warning.
- `--id`: Non-empty UTF-8 string without leading or trailing whitespace or control characters. Do not normalise case.
- `--clone-from`: Non-empty UTF-8 string without leading or trailing whitespace or control characters. Do not normalise case. Explicit source must be native and match selected provider before overlay write.

**Notes:** Saved overlay receipt stays green even if publication subsequently fails; overall heading then amber Partial and publication rows explain failures. Do not say rollback occurred. Token fields follow models-list precision. No command deadline exists; phase timing belongs in help notes, not fabricated progress percentages.

### Success and no-op

```text
Model overlay · {Saved if overlay_saved else Not saved}
Model            {model.provider}/{model.id}
Metadata choice  {selection.mode}
Metadata source  {selection.source_id or none}
Context tokens   {model.context_window or unknown}
Max context      {model.max_context_window or unknown} tokens
Max output       {model.max_output_tokens or unknown} tokens
Name             {model.display_name or unknown}
Description      {model.description or unknown}
Aliases          {model.aliases joined with comma, or none}
Visibility       {model.visibility or unknown}
Priority         {model.priority or unknown}
Source           {model.source}
Clone source     {model.clone_from or none}
Inferred source  {model.inferred_from or none}

{publication block}
```

### Empty, absent and inapplicable states

No empty-success state: provider and exact ID required. Missing explicit clone source is an error, never successful empty metadata. Automatic selection may return source none; retain mode and absent source without inventing a match.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| models_store_failed | 1 | Cannot access model registry state: {detail}. | Cannot read, decode, or atomically write model state. Red Error heading; preserve any returned receipt. |
| models_refresh_partial | 8 | Model refresh completed partially; inspect source and publication results. | At least one source or cache publication failed while another source or publication succeeded. Amber Partial heading; preserve successful rows and committed receipts. |
| models_refresh_failed | 1 | Model refresh failed: {detail}. | No source refresh or required publication succeeded. Red Error heading; preserve any returned receipt. |
| models_conflict | 6 | Model ID {id} conflicts across providers: {providers}. | Same case-insensitive model ID would route to different providers. Red Error heading; preserve any returned receipt. |
| models_clone_not_found | 3 | Native clone source {provider}/{id} was not found; run cq models refresh and cq models list --provider {provider}. | Explicit clone source is absent from same-provider native snapshot. Red Error heading; preserve any returned receipt. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `model`: ModelEntry: saved overlay with effective metadata.
- `overlay_saved`: boolean: whether overlay write committed.
- `selection`: ModelCloneSelectionResult.
- `publication`: ModelPublication: refresh source results and each attempted cache publication.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq models overlay prune`

**Decision:** Which overlays became redundant, and did pruning/publication finish?

### Help

```text
cq models overlay prune
Remove overlays now supplied by native model sources.

Usage
  cq models overlay prune [options]

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq models list
  cq models refresh

Examples
  cq models overlay prune
    Prune redundant overlays across both providers.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- No command-specific arguments or options; reject extras before state access.

**Notes:** Render every removed exact provider/ID pair. Partial refresh or prune failure preserves all proven deletions and publication outcomes. Do not claim zero removed unless receipt confirms it. Counts exact integers. No overall deadline or new confirmation. Narrow mode one provider/ID per line.

### Success and no-op

```text
Model overlays · {removed_count} pruned
Provider  Model ID
{removed.provider} {removed.id}

{publication block}
```

### Empty, absent and inapplicable states

removed_count=0: `No redundant overlays found.` Include publication results because refresh still occurred. Failed provider data is not evidence that its overlay is redundant; no removed row without committed removal.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| models_store_failed | 1 | Cannot access model registry state: {detail}. | Cannot read, decode, or atomically write model state. Red Error heading; preserve any returned receipt. |
| models_refresh_partial | 8 | Model refresh completed partially; inspect source and publication results. | At least one source or cache publication failed while another source or publication succeeded. Amber Partial heading; preserve successful rows and committed receipts. |
| models_refresh_failed | 1 | Model refresh failed: {detail}. | No source refresh or required publication succeeded. Red Error heading; preserve any returned receipt. |
| models_conflict | 6 | Model ID {id} conflicts across providers: {providers}. | Same case-insensitive model ID would route to different providers. Red Error heading; preserve any returned receipt. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `removed`: array<ModelIdentity>: removed provider and exact ID pairs.
- `removed_count`: integer >= 0.
- `publication`: ModelPublication: refresh source results and each attempted cache publication.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq models overlay remove`

**Decision:** Was this local overlay removed, and were caches republished?

### Help

```text
cq models overlay remove
Remove one local model overlay and publish caches.

Usage
  cq models overlay remove [options]

Options
  --provider PROVIDER  Provider to modify: claude or codex. Required. Legacy value anthropic is accepted as claude.
  --id ID  Exact model ID to add or remove. Required; case-sensitive and non-empty; leading or trailing whitespace is rejected.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq models list
  cq models refresh
  cq models overlay add

Examples
  cq models overlay remove --provider codex --id gpt-5.5
    Remove an existing Codex overlay.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- `--provider`: Reject other values; translate legacy anthropic to claude with deprecation warning.
- `--id`: Non-empty UTF-8 string without leading or trailing whitespace or control characters. Do not normalise case.

**Notes:** overlay_removed=true remains visible for publication failure. No confirmation introduced: current contract authorises this exact local overlay action. Counts and status in publication block; no new timestamps available. Narrow layout already card based.

### Success and no-op

```text
Model overlay · {Removed if overlay_removed else Not removed}
Model  {provider}/{id}

{publication block}
```

### Empty, absent and inapplicable states

Missing overlay is models_overlay_not_found, exit 3, not a no-op success. Native provider entry may remain; state `Native model entries are unaffected.` without claiming current upstream availability.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| models_store_failed | 1 | Cannot access model registry state: {detail}. | Cannot read, decode, or atomically write model state. Red Error heading; preserve any returned receipt. |
| models_refresh_partial | 8 | Model refresh completed partially; inspect source and publication results. | At least one source or cache publication failed while another source or publication succeeded. Amber Partial heading; preserve successful rows and committed receipts. |
| models_refresh_failed | 1 | Model refresh failed: {detail}. | No source refresh or required publication succeeded. Red Error heading; preserve any returned receipt. |
| models_conflict | 6 | Model ID {id} conflicts across providers: {providers}. | Same case-insensitive model ID would route to different providers. Red Error heading; preserve any returned receipt. |
| models_overlay_not_found | 3 | Overlay {provider}/{id} was not found. | No overlay matches selected provider and exact ID. Red Error heading; preserve any returned receipt. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `provider`: claude|codex.
- `id`: string: exact removed model ID.
- `overlay_removed`: boolean: whether removal committed.
- `publication`: ModelPublication: refresh source results and each attempted cache publication.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq models refresh`

**Decision:** Did source refresh and each client-cache update succeed?

### Help

```text
cq models refresh
Refresh model sources and publish client model caches.

Usage
  cq models refresh [options]

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq models list
  cq models overlay prune

Examples
  cq models refresh
    Refresh model data and publish caches.
  cq models refresh --json
    Report source and publication results as JSON.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- No command-specific arguments or options; reject extras before state access.

**Notes:** Source refresh and cache publication are separate tables because one can succeed while the other fails. Display via local/proxy from receipt. No elapsed time/date field exists: do not invent one. No overall deadline, only documented per-phase deadlines. Narrow cards preserve target path and reason.

### Success and no-op

```text
Model refresh
{publication block}
```

### Empty, absent and inapplicable states

active_count=0 says `0 active models`, not failed by itself. Missing optional client cache is a labelled skipped target with reason. No target receipts: `No cache publication results`; never claim all caches updated.

### Failures and partial results

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|
| cli_invalid_usage | 2 | Invalid arguments: {detail}. Run cq {path} --help. | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. Red Error heading; preserve any returned receipt. |
| models_store_failed | 1 | Cannot access model registry state: {detail}. | Cannot read, decode, or atomically write model state. Red Error heading; preserve any returned receipt. |
| models_refresh_partial | 8 | Model refresh completed partially; inspect source and publication results. | At least one source or cache publication failed while another source or publication succeeded. Amber Partial heading; preserve successful rows and committed receipts. |
| models_refresh_failed | 1 | Model refresh failed: {detail}. | No source refresh or required publication succeeded. Red Error heading; preserve any returned receipt. |
| models_conflict | 6 | Model ID {id} conflicts across providers: {providers}. | Same case-insensitive model ID would route to different providers. Red Error heading; preserve any returned receipt. |

**Timeout/interruption/receipt boundary:** Use only the declared timeout/partial states above. On interruption, preserve actual dispatcher status and any returned resource; do not invent rollback. An absent receipt means outcome unavailable, not no change.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `publication`: ModelPublication: refresh source results and each attempted cache publication.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq version`

**Decision:** Identify this executable and whether source provenance is known.

### Help

```text
cq version
Show CQ build identity.

Usage
  cq version [options]

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq help

Examples
  cq version --json
    Read machine-readable build identity.
```

**Argument/option details:** These exact constraints belong beneath their owning row; wrap them as prose, not machine metadata.
- No command-specific arguments or options; reject extras before state access.

**Notes:** Version cyan, labels muted, source Unknown amber, Modified amber. Clean neutral: provenance does not prove release qualification. Full 40-character revision preserved and wrapped at narrow widths; schema exact integer. No build date in resource: omit it. No side effects, prompts, timeout, partial domain state or domain error.

### Success and no-op

```text
cq {version}
Revision    {revision or Unknown}
Source tree {Modified if dirty=true else Clean if dirty=false else Unknown}
CLI schema  {cli_schema_version}
```

### Empty, absent and inapplicable states

Unknown revision or dirty=null is successful build metadata inspection: print Unknown, not 0000000 or Clean. dev is valid version identity. No user configuration required.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

Each row is a complete state: use the exact message, code and exit below. Its condition determines applicability; retain the success-shaped resource only when returned, without promoting unknown fields to facts.

| Code | Exit | Exact message | When and presentation |
|---|---:|---|---|

**Timeout/interruption/receipt boundary:** No domain timeout or partial state is declared. Parser errors and actual dispatcher interruption remain visible; do not fabricate a successful result after interruption.

### Field coverage and narrow form

The success frame plus named detail blocks covers these canonical top-level fields (schema types below are design reference, not text printed in the terminal):

- `version`: string release SemVer, or dev.
- `revision`: string40lowerhex|null when unknown.
- `dirty`: boolean|null when build provenance unknown.
- `cli_schema_version`: integer constant2.

At widths where this frame cannot fit, stack each row into labelled fields in the same order. Preserve every identifier, status, unit, absolute deadline and diagnostic. Frames already arranged as cards remain cards. Raw completion output is never reflowed. Numeric/date formatting follows the primitives above; fields absent from the canonical resource are not invented.

## `cq claude proxy pin clear`

**Decision:** Is the explicit Claude routing override now absent?

### Help

```text
cq claude proxy pin clear
Clear the Claude proxy pin.

Usage
  cq claude proxy pin clear [options]

Options
  --timeout TIMEOUT  Total work limit; default 10s. Positive Go duration.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude proxy pin show
  cq claude proxy pin set
  cq claude proxy pin clear

Examples
  cq claude proxy pin clear
```

**Notes:** A pin affects new or unbound Claude work. Existing required continuity stays on its bound account. Native account activation is separate. The saved setting hot-reloads asynchronously; no acknowledgement of live routing is claimed. A timeout may follow a committed write: inspect `cq claude proxy pin show` before retrying.

### Success and no-op

```text
Claude proxy pin · Cleared
Account       {account if configured else None}
Scope         New and unbound work
Application   Hot reload · asynchronous
Restart       Not required
```

Field mapping: provider=claude and kind=pin form the title; configured controls the presence label; account is the full stored email; application=hot_reload supplies the application text; restart_required=false supplies Not required. No changed flag exists: do not claim the command changed bytes or was a no-op from this receipt alone.

### Empty, absent and inapplicable states

An already-absent pin is successful. Cleared describes the resulting absence; do not invent a changed/no-op receipt. No counts, dates, percentages or authentication health appear in this resource. No confirmation prompt is introduced.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

| Code | Exit | Exact message | Presentation |
|---|---:|---|---|
| routing_invalid_argument | 2 | Invalid argument: {detail}. | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. Error heading; show code and command as secondary text. |
| routing_io_failed | 1 | Routing operation failed: {detail}. | Configuration, state, or output cannot be read or written. Error heading; show code and command as secondary text. |
| routing_timeout | 7 | Routing operation timed out; inspect current state before retrying a mutation. | Command deadline expires. Warning heading; known configuration receipt retained. Inspect cq claude proxy pin show before any retry. |

**Interruption:** preserve actual dispatcher exit and any saved-state receipt. If interrupted after write and before observation, say Completion unknown; do not claim rollback or unchanged configuration. No separate exit-8 partial outcome is declared.

### Narrow form

The success frame is already a labelled card. Wrap the complete email on continuation lines, never ellipsise it. Wrap explanation and error text under the heading. Do not add timestamps, live-applied status or health not contained in the resource. Plain redirected output remains existing reference bytes.

## `cq claude proxy pin set`

**Decision:** Was the selected Claude email saved as the proxy pin?

### Help

```text
cq claude proxy pin set
Set the Claude proxy pin.

Usage
  cq claude proxy pin set ACCOUNT [options]

Arguments
  ACCOUNT  Unique known Claude email from cq claude account list.

Options
  --timeout TIMEOUT  Total work limit; default 10s. Positive Go duration.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude proxy pin show
  cq claude proxy pin set
  cq claude proxy pin clear

Examples
  cq claude proxy pin set user@example.com
```

**Notes:** A pin affects new or unbound Claude work. Existing required continuity stays on its bound account. Native account activation is separate. The saved setting hot-reloads asynchronously; no acknowledgement of live routing is claimed. A timeout may follow a committed write: inspect `cq claude proxy pin show` before retrying.

**ACCOUNT validation:** nonempty email matching exactly one known Claude account. UUID, opaque-key and alias syntax are not canonical Claude references. A recognised legacy UUID translates to its unique known email or fails before saving.

### Success and no-op

```text
Claude proxy pin · Saved
Account       {account if configured else None}
Scope         New and unbound work
Application   Hot reload · asynchronous
Restart       Not required
```

Field mapping: provider=claude and kind=pin form the title; configured controls the presence label; account is the full stored email; application=hot_reload supplies the application text; restart_required=false supplies Not required. No changed flag exists: do not claim the command changed bytes or was a no-op from this receipt alone.

### Empty, absent and inapplicable states

No matched account: routing_account_not_found, not empty success. Repeating the same resolved pin is successful; title Saved describes the resulting configuration without claiming a change. No counts, dates, percentages or authentication health appear in this resource. No confirmation prompt is introduced.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

| Code | Exit | Exact message | Presentation |
|---|---:|---|---|
| routing_invalid_argument | 2 | Invalid argument: {detail}. | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. Error heading; show code and command as secondary text. |
| routing_io_failed | 1 | Routing operation failed: {detail}. | Configuration, state, or output cannot be read or written. Error heading; show code and command as secondary text. |
| routing_timeout | 7 | Routing operation timed out; inspect current state before retrying a mutation. | Command deadline expires. Warning heading; known configuration receipt retained. Inspect cq claude proxy pin show before any retry. |
| routing_account_not_found | 3 | Account not found: {account}. | Account reference matches no known account. Error heading; show code and command as secondary text. |
| routing_account_ambiguous | 6 | Account reference is ambiguous; use a unique alias or AccountKey. | Account reference matches multiple known accounts. Error heading; show code and command as secondary text. Existing diagnostic mentions alias/AccountKey but these are unsupported canonical Claude inputs. Append: `Claude accepts a unique email; inspect cq claude account list.` Correct original diagnostic separately rather than teaching unsupported syntax. |
| routing_inventory_unavailable | 4 | Account inventory is unavailable; no routing change was saved. | Complete account inventory or alias index unavailable. Error heading; show code and command as secondary text. |

**Interruption:** preserve actual dispatcher exit and any saved-state receipt. If interrupted after write and before observation, say Completion unknown; do not claim rollback or unchanged configuration. No separate exit-8 partial outcome is declared.

### Narrow form

The success frame is already a labelled card. Wrap the complete email on continuation lines, never ellipsise it. Wrap explanation and error text under the heading. Do not add timestamps, live-applied status or health not contained in the resource. Plain redirected output remains existing reference bytes.

## `cq claude proxy pin show`

**Decision:** Which Claude account is configured for new or unbound proxy work?

### Help

```text
cq claude proxy pin show
Show the Claude proxy pin.

Usage
  cq claude proxy pin show [options]

Options
  --timeout TIMEOUT  Total work limit; default 10s. Positive Go duration.

Global options
  -h, --help     Show this help
  -j, --json     Emit the documented JSON output; disable prompts
  -v, --version  Show CQ version

Related commands
  cq claude account list
  cq claude proxy pin show
  cq claude proxy pin set
  cq claude proxy pin clear

Examples
  cq claude proxy pin show
```

**Notes:** A pin affects new or unbound Claude work. Existing required continuity stays on its bound account. Native account activation is separate. Reads saved configuration only; does not query live routing. Configured does not mean the account is available or healthy.

### Success and no-op

```text
Claude proxy pin · {Configured if configured else Not configured}
Account       {account if configured else None}
Scope         New and unbound work
Application   Saved configuration only
Restart       Not required
```

Field mapping: provider=claude and kind=pin form the title; configured controls the presence label; account is the full stored email; application=configured_only supplies the application text; restart_required=false supplies Not required. No changed flag exists: do not claim the command changed bytes or was a no-op from this receipt alone.

### Empty, absent and inapplicable states

No stored pin: successful `Claude proxy pin · Not configured`, Account None. Missing/unreadable configuration is an I/O failure, not absence. No counts, dates, percentages or authentication health appear in this resource. No confirmation prompt is introduced.

### Failures and partial results

Parser failures retain their existing diagnostic code and exact message, exit 2; display `Error · {message}`, then code and canonical command on separate secondary lines. No state access or mutation.

| Code | Exit | Exact message | Presentation |
|---|---:|---|---|
| routing_invalid_argument | 2 | Invalid argument: {detail}. | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. Error heading; show code and command as secondary text. |
| routing_io_failed | 1 | Routing operation failed: {detail}. | Configuration, state, or output cannot be read or written. Error heading; show code and command as secondary text. |
| routing_timeout | 7 | Routing operation timed out; inspect current state before retrying a mutation. | Command deadline expires. Warning heading; known configuration receipt retained. Inspect cq claude proxy pin show before any retry. |

**Interruption:** preserve actual dispatcher exit and any saved-state receipt. No mutation was requested; interrupted inspection is not evidence that no pin exists. No separate exit-8 partial outcome is declared.

### Narrow form

The success frame is already a labelled card. Wrap the complete email on continuation lines, never ellipsise it. Wrap explanation and error text under the heading. Do not add timestamps, live-applied status or health not contained in the resource. Plain redirected output remains existing reference bytes.
