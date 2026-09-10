# Account and reset specification notes

Normative companion to `../commands.json`. This describes the target CLI, not a claim that today's implementation already fulfils it. Twelve canonical leaves preserve every legacy account/reset leaf: four Claude, four Codex, one Gemini, three Codex reset. Validation/canary remain assigned to the proxy specification owner.

## Groups and illustrative group help

The following group help blocks are illustrative and non-normative. The root specification generates the exact group help, including global options. Every group prints help and exits 0 without inventory, environment, service or network access.

`cq claude account`:

    Usage: cq claude account <command> [flags]

    Manage local Claude account credentials.

    Commands:
      login      Authenticate and store an account
      list       List locally configured accounts
      activate   Select the native client default account
      remove     Remove local account credentials

    Run "cq claude account <command> --help" for arguments, options and examples.

`cq codex account` follows the same illustrative structure with Codex/codex.

`cq gemini account`:

    Usage: cq gemini account <command> [flags]

    Inspect the externally managed Gemini account.

    Commands:
      show       Show Antigravity account configuration

    Run "cq gemini account show --help" for options and examples.

`cq codex reset`:

    Usage: cq codex reset <command> [flags]

    Inspect, plan and use Codex banked reset credits.

    Commands:
      list        List current reset-credit inventories
      recommend   Recommend portfolio-wide timing without consuming credits
      use         Consume one credit after confirmation

    Banked resets restore shared usage percentages; natural reset dates do not change.
    Run "cq codex reset <command> --help" for arguments, options and examples.

Legacy `codex resets` is an alias for `codex reset`, including help. Legacy provider groups remain root-owned and list canonical singular account/reset groups. Every legacy leaf maps in the JSON `aliases` array. There is no new login/logout/switch/remove path for Gemini.

## Rendering and schema conventions

All fields below are mandatory in their containing resource unless explicitly nullable. Unknown or unavailable optional facts use JSON null, never omitted fields, invented empty identifiers, or `0` timestamps. Arrays are always arrays, including empty arrays. Timestamps are RFC3339 UTC strings ending Z. Integers and numbers are finite and non-negative unless stated otherwise. Text originates from provider metadata and must be terminal-control escaped for human output. Human templates interpolate escaped values; loop rows occur zero times for empty arrays. Boolean interpolation is literal `true` or `false`. A missing optional human string renders the fallback specified in its template. A null account_reference always renders an em dash (—) in human output; it is never printed as null or an empty selector.

The command object's output.fields describes envelope `data`, not extra envelope keys. Partial inspection failures retain the typed data plus `ok=false` and errors. Pre-validation failures use data=null. A successful cancelled prompt is `ok=true`, removed=false or outcome=cancelled, exit 0. No raw access/refresh/ID token, password, auth header, credential JSON, native credential path, idempotency secret or project credential appears anywhere.

### AccountSummary

| Field | Type and exact meaning |
|---|---|
| provider | enum `claude`, `codex`, `gemini` |
| account_reference | string or null. For stable Codex accounts, exact opaque AccountKey copied from current inventory. For Claude, unique trimmed email. For Gemini, null because no mutating selector exists. Null also for unstable/unselectable inventory entries. |
| account_id | string or null. Provider's non-secret account identifier; never an auth token. |
| email | string or null. Known account email; preserve provider spelling. |
| display_name | string. Email when available, else provider label, else account_id, else `Unknown account`. Gemini uses `Antigravity CLI` when configured but no safe identity is available. |
| label | string or null. Subscription/plan label from local metadata. |
| rate_limit_tier | string or null. Known local quota tier; no network lookup just to populate it. |
| active | boolean. Native client default credential identity; never a proxy routing guarantee. Gemini true when its sole external identity exists. |
| sources | array of enum `cq_managed`, `native_client`, `platform_keychain`, `external`. Distinct sorted lexical values indicating credential ownership/source. Gemini `["external"]`. |
| aliases | string array. Existing non-secret CQ aliases for this identity, sorted case-insensitively then bytewise. Empty when none; no alias-create command is introduced. |
| stable | boolean. One stable logical identity is known; false prevents activation/removal/reset selection. |

No new persistent account-key format for Claude is introduced. Claude accepts unique email only; its help says so explicitly. Codex reuses its existing opaque AccountKey and existing resolver. Exact key matches take precedence over metadata matches; trimmed case-insensitive email and alias matches are considered together and must resolve to one logical identity. No prefix matching. Whitespace-only input is invalid usage. Duplicate stable keys or metadata collisions produce conflict rather than choosing first result. List ordering: null account_reference after non-null; bytewise account_reference then display_name.

### ResetCredit

| Field | Type and meaning |
|---|---|
| id | non-empty string without surrounding whitespace; opaque upstream credit ID |
| reset_type | non-empty string; `codex_rate_limits` is currently the sole consumable type. Other upstream values are retained for inspection and never silently treated as supported. |
| status | non-empty string; known values `available`, `redeeming`, `redeemed`. Other upstream values remain visible and ineligible. |
| granted_at | UTC timestamp |
| expires_at | UTC timestamp or null for no expiry |
| title | string or null |
| description | string or null |
| supported | boolean, true iff reset_type is `codex_rate_limits` |

Expiry eligibility is evaluated against command snapshot time; expiry equal to now is expired. Available supported unexpired credits are eligible. An unresolved exact persisted replay can revisit its original credit through existing idempotency semantics; it cannot select a different credit. Omitted credit selection sorts earliest non-null expiry, then no expiry, then bytewise id. Input/output credit IDs are never interpreted as file paths.

### ResetInventoryError

`{code: enum(auth_failed,credits_unavailable,invalid_id,missing_reset_type,missing_status,invalid_granted_at,invalid_expires_at,invalid_entry,invalid_inventory), message: string, entry_index: integer|null}`. `entry_index` is the zero-based position in upstream credits when a row failed; null denotes whole-inventory/account failure. Messages are stable descriptions, never upstream response bodies. Unknown upstream extra JSON keys are ignored. Required fields and accepted timestamp shapes remain validated. Error-code mapping collapses an otherwise unclassified malformed entry to invalid_entry and invalid inventory/count mismatch to invalid_inventory.

### ResetAccountInventory

`{account_reference: string|null, account_id: string|null, email: string|null, credits: ResetCredit[], available_count: integer|null, errors: ResetInventoryError[]}`. Null available_count means reliable provider inventory was unavailable. Valid credits remain present when a malformed sibling credit exists; complete=false and exit 8 make the omission explicit. Accounts sorted by account_reference then account_id/email; credit list sorted by expires_at null-last, then id. Empty account portfolio is successful. Unsupported but well-formed credit types are visible, not malformed rows.

### ResetSchedule

| Field | Type and meaning |
|---|---|
| generated_at | UTC timestamp of fresh portfolio snapshot |
| horizon | UTC timestamp; end of the current scheduler's computed optimisation horizon |
| complete | boolean; all required portfolio inputs supplied |
| exact | boolean; bounded search found exact optimum rather than bounded approximation |
| confidence | enum `high`, `low`; confidence in demand estimate |
| items | ResetScheduleItem[] |
| objective | ResetScheduleObjective |
| blockers | ResetScheduleBlocker[] |

`ResetScheduleItem` = `{account_reference: string, account_email: string|null, account_id: string|null, credit_id: string, use_at: timestamp|null, use_by: timestamp|null, status: enum(due_now,scheduled,deferred,not_yet_useful,unsupported), confidence: enum(high,low), restored_pct: ResetRestoredWindow[], avoided_gap_seconds: integer, reason_codes: enum(gap_avoidance,expiry_pressure,natural_reset_interaction,waste_reduction,low_confidence_fallback)[]}`. `ResetRestoredWindow` = `{name: enum(5h,7d), percentage: number in [0,100]}`. `use_by` is credit expiry or null; use_at null means no actionable scheduled time. Incomplete schedules set every use_at=null and retain blockers. Items sort use_at null-last, account_reference then credit_id. Reason codes deduplicated in declared enum order. For valid input, horizon is the latest finite credit expiry plus 7 days; if no finite expiry exists, generated_at plus 7 days. For entirely invalid input that cannot construct a schedule, the adapter uses generated_at plus 7 days with complete=false and a blocker. No CLI option changes scheduler objective, algorithm or horizon.

`ResetScheduleObjective` = `{unmet_demand_pct_seconds: number, gap_duration_seconds: integer, useful_expired_unused: integer, weighted_discarded_pct: number, restored_pct: number}`. These aggregate objective terms are non-negative; percentage sums may exceed 100 across accounts/windows. No new planner knobs are added.

`ResetScheduleBlocker` = `{code: enum(usage_unavailable,usage_account_missing,usage_account_ambiguous,usage_windows_invalid,auth_failed,credits_unavailable,inventory_invalid), account_reference: string|null, account_email: string|null, account_id: string|null}`. Global blockers have null account fields. Current unmatched/ambiguous usage diagnostics map to the corresponding missing/ambiguous codes; malformed credit inputs map inventory_invalid. Blockers sort account_reference null-first then code.

### ResetWindowChange

`{name: enum(5h,7d), before: ResetWindow, after: ResetWindow}`. `ResetWindow` = `{remaining_pct: number in [0,100], reset_at: timestamp, period_seconds: integer > 0}`. Arrays ordered 5h then 7d. No model-specific quota window is implied to reset. Missing successful post-reset refresh produces an empty changed_windows array, known terminal outcome, explicit warning/error and exit 8; it must not fabricate before/after percentages.

### ResetRetry

`{account_reference: string, credit_id: string}`. These are exact selected replay coordinates. The durable idempotency key remains internal. Retry syntax is `cq codex reset use ACCOUNT --credit CREDIT_ID --yes`; documentation and human rendering quote arguments using POSIX single-quote rules. Consumer code should pass fields as argv elements, not evaluate a generated shell string. Timeout after consumption starts produces outcome=indeterminate with retry present and exit 7. Non-timeout inconclusive outcome gives exit 1. Cancellation before consumption returns outcome=cancelled and retry=null; if unresolved prior work already exists, cancelling its replay leaves it intact.

## Errors and warnings

Shared parser errors and interrupt exit 130 come from root contract. All human errors render `cq: {message}\n` on stderr. Namespaced codes in each command object are stable. Parameter/reference errors are determined before writes. Missing account is exit 3; ambiguity/stale identity/consent/read-only ownership is exit 6. Remote authentication failure is exit 5. Platform credential access unavailable is exit 4. IO failure is exit 1. Successful no-op provider outcomes no_credit and nothing_to_reset are exit 0 because the requested attempt reached a definite terminal response.

Post-reset cleanup warnings map to envelope warning codes `codex_reset_attempt_cleanup_failed`, `codex_reset_cache_invalidate_failed`, `codex_reset_usage_refetch_failed`, `codex_reset_usage_match_failed`, `codex_reset_history_update_failed`. Each warning message is respectively `Terminal reset attempt cleanup failed.`, `Quota cache invalidation failed.`, `Fresh quota could not be read.`, `Fresh quota could not be matched to the selected account.`, `Quota history update failed.` These known-outcome failures also return codex_reset_postcheck_partial exit 8. Never encourage another credit consumption as remediation. All terminal outcomes including no_credit/nothing_to_reset must surface attempt cleanup failure as partial result.

Activation and removal errors after some local writes retain output data sufficient to identify the exact affected account, with exit 8. Do not imply rollback succeeded unless verified. Account list is read-only; pending owner recovery that requires mutation is not silently executed during listing: report account_inventory_unavailable or structured warning with available safe snapshot instead.

## Workflows and example preparation

Ordinary literal `user@example.com` means a configured example identity; users substitute an email from account list. To obtain a concrete reset credit ID for the shell-variable example:

    cq codex account list
    cq codex reset list user@example.com
    read -r CREDIT_ID
    cq codex reset use user@example.com --credit "$CREDIT_ID" --yes --json

The read command accepts one credit ID copied from the preceding inventory. An exact AccountKey can replace the email in every Codex account activate/remove and reset list/use invocation. Users copy it from account list; they must not derive its structure. No example embeds a token, executes external JSON as shell, or invents a real credit ID.

Interactive removal/reset timeout accounting excludes time spent awaiting user consent; it limits all discovery, validation, network and persistence work in aggregate. Login timeout includes browser authentication wait because OAuth callback expiry must be bounded. Non-interactive JSON requires --yes only for remove/reset use; browser OAuth is explicit login authentication, not a terminal confirmation. Parser validation of consent requirements precedes discovery in JSON/non-terminal mode. Every example invoking login explicitly authorises browser flow; JSON login still waits for external browser completion.

## Required implementation deltas and current dirty work

1. Replace split/manual account and reset help with canonical generated definitions, dedicated resource groups and same alias parser. Scope timeout/JSON handling consistently; remove implicit refresh-agent installation from account commands. Legacy --refresh migration belongs to root's compatibility policy.
2. Gemini account show must call `gemini.Provider.DiscoverAccounts` (`release v0.32.5 source/internal/provider/gemini/provider.go:51`). Current dispatch calls unsupported AccountManager and cannot work. Existing discoverer already performs local presence inspection without network/credential parsing, so do not add a new auth mechanism.
3. Expose safe account keys/source/alias metadata in account list. Reuse Codex reference resolver for activate/remove; current email-only `matchingLogicalAccount` cannot resolve duplicate emails. Claude keeps email grammar but gains explicit ambiguity checks/case-insensitive matching; no speculative UUID/alias subsystem.
4. Add explicit destructive confirmation for local remove and JSON result envelopes for all account mutations. Preserve native-client credential-owner transaction boundaries and external source immutability.
5. Existing reset list can hide entry errors and return success despite failed account rows. New schema preserves errors and exit 8. Reset use currently emits preview to stdout in human mode and may read stdin in JSON mode; canonical preview/prompt goes stderr and JSON requires --yes before work.
6. Reset output adds account_reference to disambiguate same-email portfolio entries, null-stable shapes, terminal/cancelled/indeterminate outcomes, and exact retry coordinates. Preserve existing scheduler; resource shape is an adapter, not a replacement algorithm.
7. Current dirty `internal/provider/codex/accounts.go:98` and `system_activator.go:178` derive expiry from access-token JWT/CQ fallback. Preserve that current work; do not restore the released ID-token-derived expiry.
8. Current dirty `internal/provider/codex/reset_accounts.go:103` tries existing candidate credentials before eligible managed refresh and only retries authentication failures for the same logical identity. Preserve it. Never replace this with account failover or refresh external/system credentials.
9. Current dirty `internal/provider/codex/reset_credits.go:86,244,260` tolerates unknown upstream JSON fields while requiring reset_type/status presence. Preserve it; strictness remains at actual required fields. Canonical row-error adapter must not silently reintroduce DisallowUnknownFields.
10. Command-wide deadlines, side-effect-free account listing and post-result partial exits require implementation and regression tests; they are normative target changes, not claims about released code.
