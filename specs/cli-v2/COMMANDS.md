# Canonical command reference

Generated from [commands.json](commands.json). Do not edit generated files.

Every option below inherits the parser, output, consent and error rules in [README.md](README.md). Exact help includes global options at every node. Null defaults mean no literal default; command rules define omission.

## `cq auth`

Refresh stored provider authentication credentials.

```text
cq auth <COMMAND> [OPTIONS]
```

[Exact help](help/auth.txt) · Family: models · Kind: group

Refresh stored provider authentication credentials.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `auth`: Credential operations, distinct from quota data.

### Arguments

None.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/auth.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq auth --help
```

Show group help.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/help.go:83`
- `cmd/cq/help.go:341`

## `cq auth refresh`

Refresh eligible stored Claude and Codex OAuth credentials.

```text
cq auth refresh [PROVIDER...] [OPTIONS]
```

[Exact help](help/auth-refresh.txt) · Family: models · Kind: command

Refresh credentials expiring within 30 minutes, including expired credentials with an eligible refresh token. Credentials with unknown expiry or more than 30 minutes remaining are skipped.
Claude refresh may reconcile fresher anonymous Keychain credentials with identified accounts. Codex refresh applies only to CQ-managed candidates through the credential authority; system and external credentials remain read-only.
In an interactive terminal, Claude accounts requiring login offer a per-account prompt: press Enter to open the browser, or type s to skip. JSON and non-terminal execution never open a browser; they report reauthentication required and direct you to cq claude account login.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
Authentication HTTP requests have a fixed 10-second timeout. Claude browser callback waiting has a fixed 5-minute timeout after the browser flow starts; the preceding terminal prompt has no deadline.

### Naming rationale

- `auth`: Credential operations, distinct from quota data.
- `refresh`: Update the named resource from its source.

### Arguments

#### `PROVIDER`

Providers whose eligible OAuth credentials to refresh: claude and codex. Omitted: both, in that order. Gemini uses in-process Antigravity refresh during quota checks and is not supported here.

Type: enum. Required: false. Repeatable: true.

Default: ["claude", "codex"].

Choices: claude, codex.

Constraint: Reject duplicate providers and any provider other than claude or codex.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Contact selected provider authentication endpoints and persist successfully refreshed eligible credentials.
- Invalidate quota caches only for providers whose credentials changed.
- Do not activate accounts, install services, refresh quota percentages, consume reset credits, or mutate unselected providers.
- Independent account failures do not hide successful refreshes; every selected provider receives a result.
- Track committed credential changes independently from final success or failure status; an account may fail after a successful write and still has credentials_changed: true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `providers`: array<AuthRefreshProviderResult>: one result per selected provider in requested order
- `credentials_changed`: boolean: at least one credential persistence committed, including reconciliation or partial success
- `changed_count`: integer >= 0: number of distinct logical accounts with at least one committed credential change

Human output template:

```text
{provider}: {refreshed} refreshed, {unchanged} unchanged, {reauth_required} need login, {failed} failed

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| auth_refresh_failed | 5 | All eligible refresh attempts fail authentication or all remaining work requires reauthentication. | Credentials require authentication; inspect provider results. |
| auth_refresh_partial | 8 | Some refresh work succeeds and some fails or requires reauthentication. | Credential refresh completed partially; inspect provider results. |
| auth_authority_unavailable | 4 | Selected Codex credential authority cannot be reached or opened. | Codex credential authority is unavailable. |
| auth_inventory_degraded | 6 | Codex external-source inventory has a nonoptional error. | Codex credential inventory is degraded; refresh was not attempted. |
| auth_store_failed | 1 | Refreshed credentials cannot be persisted. | Cannot persist refreshed credentials for {account_label}. |
| auth_interrupted | 130 | Command is interrupted; successful earlier writes remain committed. | Credential refresh interrupted. |

### Examples

```sh
cq auth refresh
```

Refresh eligible Claude and Codex credentials.

```sh
cq auth refresh codex --json
```

Refresh only eligible CQ-managed Codex credentials without interaction.

```sh
cq auth refresh claude
```

Refresh Claude credentials and offer terminal reauthentication when needed.

### Compatibility spellings

- `cq refresh` → `auth refresh`. Exact legacy no-argument command only; extra arguments fail instead of becoming selectors.

### Source evidence

- `cmd/cq/refresh.go:23`
- `cmd/cq/refresh.go:50`
- `cmd/cq/refresh.go:174`
- `v0.32.5:cmd/cq/refresh.go:23`

## `cq check`

Report quota usage for selected providers.

```text
cq check [PROVIDER...] [OPTIONS]
```

[Exact help](help/check.txt) · Family: root · Kind: command

Fetch quota usage for Claude, Codex and Gemini concurrently. Omit providers to check all three; this command never chooses a proxy routing account.
Successful provider rows may be cached. --fresh skips the initial cache lookup; usable stale data may still be returned with a warning when a fresh fetch fails. Exhausted quota is a successful observation, not a command failure.

### Naming rationale

- `check`: Fetch and report quota usage; this is an observation, not a credential switch.

### Arguments

#### `PROVIDER`

Providers to check: claude, codex, gemini. Omit to check all three in this order.

Type: enum. Required: false. Repeatable: true.

Default: [].

Choices: claude, codex, gemini.

Constraint: Exact lowercase IDs; duplicates invalid; one or more values when supplied.

### Options

#### `--fresh`

Fetch fresh quota data instead of initially reading the quota cache. Default: false.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Does not refresh OAuth credentials as a separate operation. Existing provider refresh needed for a fetch still applies.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 30s.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Provider configuration may be absent; absence is reported as a provider result, not created.

### Effects and completion

- Read provider credentials and make quota requests for selected providers; refresh credentials only through existing authorised provider integration.
- Maintain quota cache and burn history; never install or start background services.
- Keep all existing report metrics and computations; the redesign does not alter quota arithmetic.
- Preserve requested provider order. No providers: claude,codex,gemini. Account row order: account_id bytes then email bytes; missing values sort last.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `report`: QuotaReportV1: frozen quota report schema in root notes; generated_at timestamp and provider results, aggregates and routing-scope summaries.

Human output template:

```text
{QuotaReportHumanV2 rendering defined exactly in root notes}
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| check_partial | 8 | At least one usable provider result and at least one provider/account failed or stale-fallback row was used. | Some quota results could not be refreshed. |
| check_unavailable | 4 | No usable quota result, and failures are absent configuration/unavailable integration. | No quota results are available. |
| check_authentication | 5 | No usable result and at least one authentication failure. | Authentication is required for the selected providers. |
| check_failed | 1 | No usable result, no authentication failure, and at least one operational fetch/parse/panic failure. This takes precedence over check_unavailable. | Quota checks failed for the selected providers. |
| check_timeout | 7 | Command-wide deadline expires, regardless of whether partial rows were collected; retain any completed report rows. | Quota checking timed out. |

### Examples

```sh
cq check codex
```

Check Codex only.

```sh
cq check claude codex --fresh --json
```

Fetch selected providers and emit a v2 JSON envelope.

```sh
cq
```

Check all supported providers.

### Compatibility spellings

- `cq --refresh / -r [check ...]` → `check ... --fresh`. Legacy quota-cache flag accepted only on root default/check path; never accepted on account mutations.

### Source evidence

- `cmd/cq/main.go:435`
- `internal/app/report.go:49`
- `internal/quota/result.go:1`

## `cq claude`

Manage Claude accounts and proxy routing pins.

```text
cq claude <COMMAND> [OPTIONS]
```

[Exact help](help/claude.txt) · Family: navigation · Kind: group

Manage Claude accounts and proxy routing pins.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/claude.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq claude --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq claude account`

Authenticate, inspect, activate and remove Claude accounts.

```text
cq claude account <COMMAND> [OPTIONS]
```

[Exact help](help/claude-account.txt) · Family: navigation · Kind: group

Authenticate, inspect, activate and remove Claude accounts.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/claude-account.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq claude account --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq claude account activate`

Select the native client default Claude account.

```text
cq claude account activate ACCOUNT [OPTIONS]
```

[Exact help](help/claude-account-activate.txt) · Family: accounts · Kind: command

Resolve exactly one stored account, then activate its credentials for the native client. Existing clients may need reconnection to load them.
This command does not pin proxy routing, rebind sessions, sign out other accounts or change upstream account state.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `activate`: Select native client default credentials; clearer than switch.

### Arguments

#### `ACCOUNT`

Select one Claude account by its unique email, matched case-insensitively after trimming. Ambiguous identities are rejected; no alias or opaque-key syntax is accepted.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

- One stable account resolves.
- Target has CQ-managed activatable credentials, or is already the active identity.

### Effects and completion

- Update native active credential projection through provider credential owner.
- Refresh provider metadata only where required by existing Claude activation flow; Codex activation does not adopt external credentials.
- Invalidate provider quota cache after successful activation.
- Activating the already active identity is a successful no-op; no service restart or proxy policy change.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account`: AccountSummary
- `changed`: boolean: false when already active

Human output template:

```text
Claude native client default: {account.display_name}.
Changed: {changed}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| account_not_activatable | 6 | Target has no eligible managed activation candidate. | Account has no managed credentials that can be activated. |
| account_activation_partial | 8 | System auth changed but metadata projection failed. | Native client credentials changed, but account metadata needs recovery. |

### Examples

```sh
cq claude account activate user@example.com
```

Select the unique account with this email.

### Compatibility spellings

- `cq claude switch` → `claude account activate (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:213`
- `v0.32.5:internal/provider/codex/accounts.go:161`

## `cq claude account list`

List locally configured Claude accounts.

```text
cq claude account list [OPTIONS]
```

[Exact help](help/claude-account-list.txt) · Family: accounts · Kind: command

List discovered local identities without fetching quota or refreshing credentials. Active means native client default; proxy routing may choose a different account.
No accounts is a successful empty result. Account keys are opaque; use the exact returned account_reference for later commands.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `list`: Enumerate zero or more configured identities.

### Arguments

None.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

None beyond global rules.

### Effects and completion

- Read local inventory and non-secret account metadata only.
- Never activate, remove, refresh credentials, contact provider endpoints, create compatibility state or install services.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `accounts`: AccountSummary[]; stable order by account_reference, then display_name

Human output template:

```text
Accounts: {accounts.length}
{for each accounts: account_reference or "—"}	{display_name}	active={active}	{sources joined by comma}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |

### Examples

```sh
cq claude account list
```

Inspect configured accounts and native default.

```sh
cq claude account list --json
```

Read stable structured account metadata.

### Compatibility spellings

- `cq claude accounts` → `claude account list (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:167`
- `v0.32.5:internal/provider/codex/accounts.go:116`

## `cq claude account login`

Authenticate and store a Claude account.

```text
cq claude account login [OPTIONS]
```

[Exact help](help/claude-account-login.txt) · Family: accounts · Kind: command

Open the Claude browser OAuth flow and save the authenticated account in CQ-managed local storage. Login alone does not change the native client default account.
Use --activate to select the authenticated account as the native client default after credentials are saved. Existing clients may need reconnection. Proxy pins and session bindings do not change.
No refresh agent or proxy service is installed implicitly. JSON mode suppresses terminal prompts; browser authentication still requires the user to complete the provider flow.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `login`: Authenticate and store an account; activation is explicit.

### Arguments

None.

### Options

#### `--activate`

Make this account the native client default after successful login (default false). Existing clients may need reconnection.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--timeout DURATION`

Limit total command work to this duration (default 10m; minimum 1s; maximum 30m).

Type: duration. Required: false. Repeatable: false.

Default: "10m".

Constraint: Go duration syntax; 1s <= value <= 30m

### Preconditions

- Browser OAuth callback transport can bind its local callback endpoint.
- CQ-owned credential store is writable.

### Effects and completion

- Launch browser OAuth and exchange the valid authorisation code once.
- Persist credentials atomically with credential file mode 0600 and directory mode 0700.
- When --activate is true, activate only the newly authenticated exact identity and invalidate provider quota cache.
- No proxy restart, routing-policy mutation, automatic service installation or upstream account creation.
- Re-login updates the same stable stored identity; partial activation failure preserves saved credentials and reports partial result.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account`: AccountSummary or null only when post-save account observation failed
- `activated`: boolean or null: whether native client default was selected; null only for an indeterminate attempted activation
- `credentials_saved`: boolean: true once durable save completed

Human output template:

```text
Logged in to Claude as {account.display_name}.
Native client default: {activated}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_auth_failed | 5 | OAuth rejected, expired or cancelled by provider. | Browser authentication failed. |
| account_login_partial | 8 | Credentials were saved but requested activation failed. | Credentials were saved, but native client activation failed. |
| account_login_postcheck_partial | 8 | Credentials were saved but the final account-state observation or metadata postcheck failed. | Credentials were saved, but account state could not be verified. |

### Examples

```sh
cq claude account login
```

Store an additional account without activation.

```sh
cq claude account login --activate
```

Authenticate and select the account as native client default.

### Compatibility spellings

- `cq claude login` → `claude account login (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/main.go:196`
- `v0.32.5:internal/app/accounts.go:34`
- `v0.32.5:internal/app/accounts.go:103`

## `cq claude account remove`

Remove local Claude account credentials.

```text
cq claude account remove ACCOUNT [OPTIONS]
```

[Exact help](help/claude-account-remove.txt) · Family: accounts · Kind: command

Remove the selected account from locally managed credential storage. This does not delete the upstream provider account, subscription or history.
Preview the account and whether native active credentials will be removed. Ask "Remove local credentials for {account.display_name}? [y/N]" on stderr; only y or yes, case-insensitive, confirms. A negative or empty answer cancels without mutation.
Use --yes for unattended removal. --json and non-terminal stdin require --yes before any credential mutation. External read-only credential files are never deleted.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `remove`: Delete local credentials; never delete an upstream account.

### Arguments

#### `ACCOUNT`

Select one Claude account by its unique email, matched case-insensitively after trimming. Ambiguous identities are rejected; no alias or opaque-key syntax is accepted.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--yes`

Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Only this operation is authorised; no future operation inherits consent.

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m). Time spent waiting for terminal confirmation is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

- One stable account resolves.
- Consent supplied with --yes or an affirmative terminal response.
- At least one selected credential is owned by CQ or the native client.

### Effects and completion

- Delete selected CQ-managed credentials and matching native active credential projection through the credential owner; Claude also removes matching platform Keychain entries.
- Do not delete declared read-only external files; retained external sources are reported.
- Invalidate provider quota cache; never select a replacement account or change routing-policy membership automatically.
- Repeat removal of an absent account returns account_not_found; after an interrupted durable Codex removal, reconcile that same operation before starting another.
- No service installation or restart.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account_reference`: string: resolved input reference
- `removed`: boolean: false on interactive cancellation
- `active_credentials_removed`: boolean
- `retained_external_sources`: string[]: external source labels only, no credential paths

Human output template:

```text
Local Claude credentials removed: {removed}.
Native active credentials removed: {active_credentials_removed}.
Retained external sources: {retained_external_sources joined by comma or "none"}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| account_confirmation_required | 6 | --json or non-terminal stdin without --yes. | Local credential removal requires --yes in non-interactive mode. |
| account_read_only | 6 | Only external read-only credential sources exist. | Account credentials are externally managed and cannot be removed by CQ. |
| account_removal_partial | 8 | Some credential mutations completed but durable removal requires recovery. | Local credential removal is incomplete; recover the pending operation before retrying. |

### Examples

```sh
cq claude account remove user@example.com
```

Preview and confirm local credential removal.

```sh
cq claude account remove user@example.com --yes --json
```

Remove explicitly confirmed local credentials and emit JSON.

### Compatibility spellings

- `cq claude remove` → `claude account remove (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:239`
- `v0.32.5:internal/provider/claude/accounts.go:119`
- `v0.32.5:internal/provider/codex/credential_coordinator.go:569`

## `cq claude proxy`

Control Claude-specific routing through the shared proxy.

```text
cq claude proxy <COMMAND> [OPTIONS]
```

[Exact help](help/claude-proxy.txt) · Family: navigation · Kind: group

Control Claude-specific routing through the shared proxy.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/claude-proxy.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq claude proxy --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq claude proxy pin`

Inspect, set or clear the Claude proxy account override.

```text
cq claude proxy pin <COMMAND> [OPTIONS]
```

[Exact help](help/claude-proxy-pin.txt) · Family: navigation · Kind: group

Inspect, set or clear the Claude proxy account override.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `claude`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `pin`: Explicit account override for new or unbound routed work.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/claude-proxy-pin.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq claude proxy pin --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq claude proxy pin clear`

Clear the Claude proxy pin.

```text
cq claude proxy pin clear [OPTIONS]
```

[Exact help](help/claude-proxy-pin-clear.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Claude pin changes hot-reload in a running proxy.

### Naming rationale

- `claude`: Provider-first scope: these operations affect Claude routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `clear`: Remove the configuration; safe when already absent.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- Running Claude proxy reloads the setting automatically.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant claude
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: string|null; unique stored Claude account email
- `application`: constant hot_reload; running proxy applies asynchronously
- `restart_required`: boolean; false for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq claude proxy pin clear
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy pin claude --clear` → `claude proxy pin clear`. Deprecated provider-last spelling; translate before common validation. Legacy UUID must resolve through known inventory to one unique email; otherwise reject without mutation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq claude proxy pin set`

Set the Claude proxy pin.

```text
cq claude proxy pin set ACCOUNT [OPTIONS]
```

[Exact help](help/claude-proxy-pin-set.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Claude pin changes hot-reload in a running proxy.

### Naming rationale

- `claude`: Provider-first scope: these operations affect Claude routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `set`: Create or replace the named configuration.

### Arguments

#### `ACCOUNT`

Unique known Claude account email from cq claude account list.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Nonempty email matching exactly one known Claude account.

Constraint: UUIDs are not canonical account references; the legacy alias resolves a UUID to its unique known email or fails before saving.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.
- Complete provider account inventory and aliases for set.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- Running Claude proxy reloads the setting automatically.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant claude
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: string|null; unique stored Claude account email
- `application`: constant hot_reload; running proxy applies asynchronously
- `restart_required`: boolean; false for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |

### Examples

```sh
cq claude proxy pin set user@example.com
```

Requires the account_reference preparation in routing notes.

### Compatibility spellings

- `cq proxy pin claude ACCOUNT` → `claude proxy pin set ACCOUNT`. Deprecated provider-last spelling; translate before common validation. Legacy UUID must resolve through known inventory to one unique email; otherwise reject without mutation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq claude proxy pin show`

Show the Claude proxy pin.

```text
cq claude proxy pin show [OPTIONS]
```

[Exact help](help/claude-proxy-pin-show.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Claude pin changes hot-reload in a running proxy.

### Naming rationale

- `claude`: Provider-first scope: these operations affect Claude routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `show`: Read one resource without changing it.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Read configuration only; do not query or modify running routing.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant claude
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: string|null; unique stored Claude account email
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; false for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq claude proxy pin show
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy pin claude` → `claude proxy pin show`. Deprecated provider-last spelling; translate before common validation. Legacy UUID must resolve through known inventory to one unique email; otherwise reject without mutation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex`

Manage Codex accounts, quota resets and provider-specific proxy controls.

```text
cq codex <COMMAND> [OPTIONS]
```

[Exact help](help/codex.txt) · Family: navigation · Kind: group

Manage Codex accounts, quota resets and provider-specific proxy controls.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex account`

Authenticate, inspect, activate and remove Codex accounts.

```text
cq codex account <COMMAND> [OPTIONS]
```

[Exact help](help/codex-account.txt) · Family: navigation · Kind: group

Authenticate, inspect, activate and remove Codex accounts.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-account.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex account --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex account activate`

Select the native client default Codex account.

```text
cq codex account activate ACCOUNT [OPTIONS]
```

[Exact help](help/codex-account-activate.txt) · Family: accounts · Kind: command

Resolve exactly one stored account, then activate its credentials for the native client. Existing clients may need reconnection to load them.
This command does not pin proxy routing, rebind sessions, sign out other accounts or change upstream account state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `activate`: Select native client default credentials; clearer than switch.

### Arguments

#### `ACCOUNT`

Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Type: account-reference. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

- One stable account resolves.
- Target has CQ-managed activatable credentials, or is already the active identity.

### Effects and completion

- Update native active credential projection through provider credential owner.
- Refresh provider metadata only where required by existing Claude activation flow; Codex activation does not adopt external credentials.
- Invalidate provider quota cache after successful activation.
- Activating the already active identity is a successful no-op; no service restart or proxy policy change.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account`: AccountSummary
- `changed`: boolean: false when already active

Human output template:

```text
Codex native client default: {account.display_name}.
Changed: {changed}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| account_not_activatable | 6 | Target has no eligible managed activation candidate. | Account has no managed credentials that can be activated. |
| account_activation_partial | 8 | System auth changed but metadata projection failed. | Native client credentials changed, but account metadata needs recovery. |

### Examples

```sh
cq codex account activate user@example.com
```

Select the unique account with this email.

### Compatibility spellings

- `cq codex switch` → `codex account activate (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:213`
- `v0.32.5:internal/provider/codex/accounts.go:161`

## `cq codex account list`

List locally configured Codex accounts.

```text
cq codex account list [OPTIONS]
```

[Exact help](help/codex-account-list.txt) · Family: accounts · Kind: command

List discovered local identities without fetching quota or refreshing credentials. Active means native client default; proxy routing may choose a different account.
No accounts is a successful empty result. Account keys are opaque; use the exact returned account_reference for later commands.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `list`: Enumerate zero or more configured identities.

### Arguments

None.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

None beyond global rules.

### Effects and completion

- Read local inventory and non-secret account metadata only.
- Never activate, remove, refresh credentials, contact provider endpoints, create compatibility state or install services.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `accounts`: AccountSummary[]; stable order by account_reference, then display_name

Human output template:

```text
Accounts: {accounts.length}
{for each accounts: account_reference or "—"}	{display_name}	active={active}	{sources joined by comma}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |

### Examples

```sh
cq codex account list
```

Inspect configured accounts and native default.

```sh
cq codex account list --json
```

Read stable structured account metadata.

### Compatibility spellings

- `cq codex accounts` → `codex account list (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:167`
- `v0.32.5:internal/provider/codex/accounts.go:116`

## `cq codex account login`

Authenticate and store a Codex account.

```text
cq codex account login [OPTIONS]
```

[Exact help](help/codex-account-login.txt) · Family: accounts · Kind: command

Open the Codex browser OAuth flow and save the authenticated account in CQ-managed local storage. Login alone does not change the native client default account.
Use --activate to select the authenticated account as the native client default after credentials are saved. Existing clients may need reconnection. Proxy pins and session bindings do not change.
No refresh agent or proxy service is installed implicitly. JSON mode suppresses terminal prompts; browser authentication still requires the user to complete the provider flow.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `login`: Authenticate and store an account; activation is explicit.

### Arguments

None.

### Options

#### `--activate`

Make this account the native client default after successful login (default false). Existing clients may need reconnection.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--timeout DURATION`

Limit total command work to this duration (default 10m; minimum 1s; maximum 30m).

Type: duration. Required: false. Repeatable: false.

Default: "10m".

Constraint: Go duration syntax; 1s <= value <= 30m

### Preconditions

- Browser OAuth callback transport can bind its local callback endpoint.
- CQ-owned credential store is writable.

### Effects and completion

- Launch browser OAuth and exchange the valid authorisation code once.
- Persist credentials atomically with credential file mode 0600 and directory mode 0700.
- When --activate is true, activate only the newly authenticated exact identity and invalidate provider quota cache.
- No proxy restart, routing-policy mutation, automatic service installation or upstream account creation.
- Re-login updates the same stable stored identity; partial activation failure preserves saved credentials and reports partial result.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account`: AccountSummary or null only when post-save account observation failed
- `activated`: boolean or null: whether native client default was selected; null only for an indeterminate attempted activation
- `credentials_saved`: boolean: true once durable save completed

Human output template:

```text
Logged in to Codex as {account.display_name}.
Native client default: {activated}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_auth_failed | 5 | OAuth rejected, expired or cancelled by provider. | Browser authentication failed. |
| account_login_partial | 8 | Credentials were saved but requested activation failed. | Credentials were saved, but native client activation failed. |
| account_login_postcheck_partial | 8 | Credentials were saved but the final account-state observation or metadata postcheck failed. | Credentials were saved, but account state could not be verified. |

### Examples

```sh
cq codex account login
```

Store an additional account without activation.

```sh
cq codex account login --activate
```

Authenticate and select the account as native client default.

### Compatibility spellings

- `cq codex login` → `codex account login (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/main.go:196`
- `v0.32.5:internal/app/accounts.go:34`
- `v0.32.5:internal/app/accounts.go:103`

## `cq codex account remove`

Remove local Codex account credentials.

```text
cq codex account remove ACCOUNT [OPTIONS]
```

[Exact help](help/codex-account-remove.txt) · Family: accounts · Kind: command

Remove the selected account from locally managed credential storage. This does not delete the upstream provider account, subscription or history.
Preview the account and whether native active credentials will be removed. Ask "Remove local credentials for {account.display_name}? [y/N]" on stderr; only y or yes, case-insensitive, confirms. A negative or empty answer cancels without mutation.
Use --yes for unattended removal. --json and non-terminal stdin require --yes before any credential mutation. External read-only credential files are never deleted.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `remove`: Delete local credentials; never delete an upstream account.

### Arguments

#### `ACCOUNT`

Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Type: account-reference. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--yes`

Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Only this operation is authorised; no future operation inherits consent.

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m). Time spent waiting for terminal confirmation is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

- One stable account resolves.
- Consent supplied with --yes or an affirmative terminal response.
- At least one selected credential is owned by CQ or the native client.

### Effects and completion

- Delete selected CQ-managed credentials and matching native active credential projection through the credential owner; Claude also removes matching platform Keychain entries.
- Do not delete declared read-only external files; retained external sources are reported.
- Invalidate provider quota cache; never select a replacement account or change routing-policy membership automatically.
- Repeat removal of an absent account returns account_not_found; after an interrupted durable Codex removal, reconcile that same operation before starting another.
- No service installation or restart.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account_reference`: string: resolved input reference
- `removed`: boolean: false on interactive cancellation
- `active_credentials_removed`: boolean
- `retained_external_sources`: string[]: external source labels only, no credential paths

Human output template:

```text
Local Codex credentials removed: {removed}.
Native active credentials removed: {active_credentials_removed}.
Retained external sources: {retained_external_sources joined by comma or "none"}.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| account_inventory_unavailable | 4 | Credential inventory cannot be read. | Account inventory is unavailable. |
| account_timeout | 7 | Total deadline expires before a terminal outcome. | Account command timed out. |
| account_io_failed | 1 | Credential or registry persistence fails. | Account state could not be saved. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| account_confirmation_required | 6 | --json or non-terminal stdin without --yes. | Local credential removal requires --yes in non-interactive mode. |
| account_read_only | 6 | Only external read-only credential sources exist. | Account credentials are externally managed and cannot be removed by CQ. |
| account_removal_partial | 8 | Some credential mutations completed but durable removal requires recovery. | Local credential removal is incomplete; recover the pending operation before retrying. |

### Examples

```sh
cq codex account remove user@example.com
```

Preview and confirm local credential removal.

```sh
cq codex account remove user@example.com --yes --json
```

Remove explicitly confirmed local credentials and emit JSON.

### Compatibility spellings

- `cq codex remove` → `codex account remove (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:internal/app/accounts.go:239`
- `v0.32.5:internal/provider/claude/accounts.go:119`
- `v0.32.5:internal/provider/codex/credential_coordinator.go:569`

## `cq codex proxy`

Control Codex routing, quota protection and transport evidence.

```text
cq codex proxy <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy.txt) · Family: navigation · Kind: group

Control Codex routing, quota protection and transport evidence.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy canary`

Manage a bounded Codex routing-observation run.

```text
cq codex proxy canary <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-canary.txt) · Family: navigation · Kind: group

Manage a bounded Codex routing-observation run.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `canary`: A bounded observation run of real Codex routing, with protected-state checks.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-canary.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy canary --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy canary start`

Start a protected observation run of Codex routing.

```text
cq codex proxy canary start [OPTIONS]
```

[Exact help](help/codex-proxy-canary-start.txt) · Family: validation · Kind: command

A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `canary`: A bounded observation run of real Codex routing, with protected-state checks.
- `start`: Begins a new observation run.

### Arguments

None.

### Options

None.

### Preconditions

- HTTP routing enforcement enabled, payload diagnostics disabled, current HTTP readiness marker available.

### Effects and completion

- Creates a protected local observation record for the current exact build/readiness tuple.
- Does not generate traffic or restart the service. The running service must attach the canary; restarting the proxy is a separate explicit action if required.
- Starting when an active run exists is a conflict; an inactive completed record may be replaced by a new run.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state`: Canary resource

Human output template:

```text
Canary: {state.run_id}
Active: {state.active}
Finalised: {state.finalised}
Admitted turns: {state.admitted_turns}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| canary_missing | 3 | No run recorded | No Codex canary is recorded. |
| canary_precondition_failed | 6 | Active run already exists for start, readiness stale, or protected state changed | The Codex canary preconditions are not satisfied. |
| canary_state_unavailable | 1 | State read/write fails | The Codex canary state is unavailable. |

### Examples

```sh
cq codex proxy canary start
```

Create a run before attaching the installed service.

### Compatibility spellings

- `cq codex canary start` → `codex proxy canary start`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_canary.go:15`
- `internal/proxy/codex_canary.go:80`
- `v0.32.5:cmd/cq/codex_canary.go:15`

## `cq codex proxy canary status`

Show the retained Codex routing observation run.

```text
cq codex proxy canary status [OPTIONS]
```

[Exact help](help/codex-proxy-canary-status.txt) · Family: validation · Kind: command

A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `canary`: A bounded observation run of real Codex routing, with protected-state checks.
- `status`: Inspects current state without requesting a transition.

### Arguments

None.

### Options

None.

### Preconditions

- Existing owner-controlled canary store and readable protected sources.

### Effects and completion

- Reads retained canary state and verifies protected-state digests; never requests a transition.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state`: Canary resource

Human output template:

```text
Canary: {state.run_id}
Active: {state.active}
Finalised: {state.finalised}
Admitted turns: {state.admitted_turns}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| canary_missing | 3 | No run recorded | No Codex canary is recorded. |
| canary_precondition_failed | 6 | Active run already exists for start, readiness stale, or protected state changed | The Codex canary preconditions are not satisfied. |
| canary_state_unavailable | 1 | State read/write fails | The Codex canary state is unavailable. |

### Examples

```sh
cq codex proxy canary status
```

Inspect progress without changing routing.

### Compatibility spellings

- `cq codex canary status` → `codex proxy canary status`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_canary.go:15`
- `internal/proxy/codex_canary.go:80`
- `v0.32.5:cmd/cq/codex_canary.go:15`

## `cq codex proxy canary stop`

Request a graceful end to the Codex routing observation run.

```text
cq codex proxy canary stop [OPTIONS]
```

[Exact help](help/codex-proxy-canary-stop.txt) · Family: validation · Kind: command

A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `canary`: A bounded observation run of real Codex routing, with protected-state checks.
- `stop`: Requests the end of an observation run.

### Arguments

None.

### Options

None.

### Preconditions

- Existing owner-controlled canary store and readable protected sources.

### Effects and completion

- Records a stop request; the installed service drains active sessions and finalises the run asynchronously.
- Success means stop requested, not drain complete; inspect status until active=false and finalised=true.
- Repeated stop requests for the same run are idempotent; no forced termination.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state`: Canary resource

Human output template:

```text
Canary: {state.run_id}
Active: {state.active}
Finalised: {state.finalised}
Admitted turns: {state.admitted_turns}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| canary_missing | 3 | No run recorded | No Codex canary is recorded. |
| canary_precondition_failed | 6 | Active run already exists for start, readiness stale, or protected state changed | The Codex canary preconditions are not satisfied. |
| canary_state_unavailable | 1 | State read/write fails | The Codex canary state is unavailable. |

### Examples

```sh
cq codex proxy canary stop
```

Ask the installed service to drain and finalise the run.

### Compatibility spellings

- `cq codex canary stop` → `codex proxy canary stop`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_canary.go:15`
- `internal/proxy/codex_canary.go:80`
- `v0.32.5:cmd/cq/codex_canary.go:15`

## `cq codex proxy credential-endpoint`

Maintain the Codex local credential-owner endpoint.

```text
cq codex proxy credential-endpoint <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint.txt) · Family: navigation · Kind: group

Maintain the Codex local credential-owner endpoint.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-credential-endpoint.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy credential-endpoint --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy credential-endpoint legacy`

Inspect and migrate the legacy credential-owner endpoint protocol.

```text
cq codex proxy credential-endpoint legacy <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy.txt) · Family: navigation · Kind: group

Inspect and migrate the legacy credential-owner endpoint protocol.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-credential-endpoint-legacy.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy credential-endpoint legacy --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy credential-endpoint legacy activate`

Activate a reversible replacement credential endpoint.

```text
cq codex proxy credential-endpoint legacy activate --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-activate.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `activate`: Open the reversible replacement-owner validation window.

### Arguments

None.

### Options

#### `--ticket-file TICKET_FILE`

LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16384 bytes inclusive; no group or other write permission.

#### `--confirm-stopped-and-drained`

Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--non-interactive`

Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

### Effects and completion

- Activate reversible replacement window while retaining exact quarantine for rollback.
- Does not launch shared proxy automatically; operator must start intended owner and verify it before finalise.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint transition-legacy activate` → `codex proxy credential-endpoint legacy activate; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy credential-endpoint legacy finalise`

Finalise a verified credential endpoint replacement.

```text
cq codex proxy credential-endpoint legacy finalise --ticket-file TICKET_FILE --confirm-candidate-healthy [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-finalise.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
Finalisation removes the rollback path only after exact live-owner verification.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `finalise`: Commit verified replacement irreversibly and retire rollback state.

### Arguments

None.

### Options

#### `--ticket-file TICKET_FILE`

LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16384 bytes inclusive; no group or other write permission.

#### `--confirm-candidate-healthy`

Assert the exact replacement credential owner passed live health verification. Required; runtime verifier must also confirm.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--non-interactive`

Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- State activated; exact live candidate owner verification succeeds.

### Effects and completion

- Acquire exact live owner verification lease, persist finalising state, then irreversibly finalise and retire quarantine.
- After finalisation rollback is unavailable; replay must use retained ticket identity and existing committed result.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-candidate-healthy --non-interactive
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint transition-legacy finalise` → `codex proxy credential-endpoint legacy finalise; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy credential-endpoint legacy inspect`

Inspect the legacy Codex credential socket and migration state.

```text
cq codex proxy credential-endpoint legacy inspect [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-inspect.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `inspect`: Read identity proof or current transition without writes.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.

### Effects and completion

- Read refused socket identity or existing transition; no mutation and idempotent.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy inspect
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint inspect-legacy` → `codex proxy credential-endpoint legacy inspect; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy credential-endpoint legacy prepare`

Quarantine a refused legacy credential socket for replacement.

```text
cq codex proxy credential-endpoint legacy prepare --snapshot-file SNAPSHOT_FILE --confirm-stopped-and-drained [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-prepare.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `prepare`: Record identity and quarantine the old socket while drained.

### Arguments

None.

### Options

#### `--snapshot-file SNAPSHOT_FILE`

LegacyEndpointSnapshotV1 extracted from inspect JSON; required, at most 16384 bytes.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16384 bytes inclusive; no group or other write permission.

#### `--confirm-stopped-and-drained`

Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--non-interactive`

Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

### Effects and completion

- Preserve exact legacy socket in quarantine and persist transition ticket; do not start proxy or credential owner.
- Existing transition is a conflict; use resume with its ticket.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file "$INPUTS/endpoint-snapshot.json" --confirm-stopped-and-drained --non-interactive
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint transition-legacy prepare` → `codex proxy credential-endpoint legacy prepare; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy credential-endpoint legacy resume`

Reopen a retained credential endpoint migration.

```text
cq codex proxy credential-endpoint legacy resume --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-resume.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `resume`: Reopen an existing transition without activating it.

### Arguments

None.

### Options

#### `--ticket-file TICKET_FILE`

LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16384 bytes inclusive; no group or other write permission.

#### `--confirm-stopped-and-drained`

Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--non-interactive`

Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

### Effects and completion

- Reopen and validate retained transition; keep its phase, then release maintenance handle.
- Does not activate, finalise or rollback; repeated resume leaves durable state unchanged.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint transition-legacy resume` → `codex proxy credential-endpoint legacy resume; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy credential-endpoint legacy rollback`

Restore the quarantined legacy credential socket.

```text
cq codex proxy credential-endpoint legacy rollback --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

[Exact help](help/codex-proxy-credential-endpoint-legacy-rollback.txt) · Family: candidate · Kind: command

Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.

### Naming rationale

- `codex`: Owns this credential coordination endpoint.
- `proxy`: Endpoint supports Codex proxy credential coordination.
- `credential-endpoint`: Local credential-owner socket; not the HTTP proxy listener.
- `legacy`: Maintenance of the old refused socket format.
- `rollback`: Restore the preserved old socket before finalisation.

### Arguments

None.

### Options

#### `--ticket-file TICKET_FILE`

LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16384 bytes inclusive; no group or other write permission.

#### `--confirm-stopped-and-drained`

Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--non-interactive`

Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

### Effects and completion

- Restore exact preserved legacy socket; refuse if replacement participants are live or finalisation has passed irreversibility.
- Record rolled_back; never create a substitute legacy identity.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `endpoint`: LegacyEndpointResultV2; exact discriminated schema in annex notes.

Human output template:

```text
Credential endpoint: {endpoint.path}
Migration state: {endpoint.state}
Ticket: {endpoint.ticket_id_or_none}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| endpoint_invalid_proof | 2 | Proof JSON fails exact schema, size or required fields. | Credential endpoint proof is invalid: {safe_reason}. |
| endpoint_not_found | 3 | Required socket or transition does not exist. | Credential endpoint or transition does not exist. |
| endpoint_unsupported | 4 | Platform has no supported maintenance backend. | Credential endpoint maintenance is unavailable on this platform. |
| endpoint_conflict | 6 | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. | Credential endpoint migration precondition failed: {safe_reason}. |
| endpoint_timeout | 7 | Deadline expires; preserved journal determines next safe action. | Credential endpoint operation timed out; inspect migration state before continuing. |
| endpoint_io_failed | 1 | Filesystem or owner RPC fails after validation. | Credential endpoint operation failed: {safe_reason}. |

### Examples

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

Uses exact inspected snapshot or prepared ticket, as described in endpoint workflow notes.

### Compatibility spellings

- `cq proxy endpoint transition-legacy rollback` → `codex proxy credential-endpoint legacy rollback; preserve all flags; --json now controls common envelope.`. Deprecated provider-ambiguous spelling.

### Source evidence

- `cmd/cq/proxy_endpoint_maintenance.go:61`
- `internal/provider/codex/credential_endpoint_maintenance.go:34`
- `internal/provider/codex/credential_endpoint_maintenance_journal.go:37`

## `cq codex proxy fallback`

Inspect, set or clear the terminal fallback account for Codex routing.

```text
cq codex proxy fallback <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-fallback.txt) · Family: navigation · Kind: group

Inspect, set or clear the terminal fallback account for Codex routing.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `fallback`: Names the configured fallback-account role; does not falsely promise first-choice routing.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-fallback.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy fallback --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy fallback clear`

Clear the Codex proxy fallback.

```text
cq codex proxy fallback clear [OPTIONS]
```

[Exact help](help/codex-proxy-fallback-clear.txt) · Family: routing · Kind: command

The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `clear`: Remove the configuration; safe when already absent.
- `fallback`: Names the configured fallback-account role; does not falsely promise first-choice routing.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant fallback
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy fallback clear
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy default codex --clear` → `codex proxy fallback clear`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy fallback set`

Set the Codex proxy fallback.

```text
cq codex proxy fallback set ACCOUNT [OPTIONS]
```

[Exact help](help/codex-proxy-fallback-set.txt) · Family: routing · Kind: command

The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `set`: Create or replace the named configuration.
- `fallback`: Names the configured fallback-account role; does not falsely promise first-choice routing.

### Arguments

#### `ACCOUNT`

Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored.

Type: account-reference. Required: true. Repeatable: false.

Default: null.

Constraint: Nonempty.

Constraint: Resolve to exactly one known Codex account.

Constraint: Ambiguous email is rejected; use alias or AccountKey.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.
- Complete provider account inventory and aliases for set.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant fallback
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |

### Examples

```sh
cq codex proxy fallback set "$ACCOUNT_REFERENCE"
```

Requires the account_reference preparation in routing notes.

### Compatibility spellings

- `cq proxy default codex ACCOUNT` → `codex proxy fallback set ACCOUNT`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy fallback show`

Show the Codex proxy fallback.

```text
cq codex proxy fallback show [OPTIONS]
```

[Exact help](help/codex-proxy-fallback-show.txt) · Family: routing · Kind: command

The default is a configured fallback role, not a promise to select this account first. Normal eligible routing remains active; the default is included when compatible and routable. A configured pin takes precedence, and required continuity remains bound.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `show`: Read one resource without changing it.
- `fallback`: Names the configured fallback-account role; does not falsely promise first-choice routing.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Read configuration only; do not query or modify running routing.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant fallback
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy fallback show
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy default codex` → `codex proxy fallback show`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy fixture`

Create sanitised test fixtures from saved Codex request bodies.

```text
cq codex proxy fixture <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-fixture.txt) · Family: navigation · Kind: group

Create sanitised test fixtures from saved Codex request bodies.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `fixture`: A sanitised description of a captured request, for offline regression evidence.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-fixture.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy fixture --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy fixture create`

Create a sanitised fixture from a saved Codex request.

```text
cq codex proxy fixture create --input INPUT --output OUTPUT [OPTIONS]
```

[Exact help](help/codex-proxy-fixture-create.txt) · Family: validation · Kind: command

Reads a previously captured request body from disk. This command does not capture network traffic or validate an installed proxy.
Stores hashes, metadata classifications and presence flags; never stores prompt text, tokens, raw session identifiers or encrypted state.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `fixture`: A sanitised description of a captured request, for offline regression evidence.
- `create`: Writes a new artefact from an existing input.

### Arguments

None.

### Options

#### `--input INPUT`

Saved request-body file; required. Maximum encoded size: 2 MiB.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Readable regular file; not stdin; nonempty path; at most 2097152 bytes.

#### `--output OUTPUT`

Destination fixture JSON file; required. Existing files are rejected.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Nonempty path; must not exist; must not alias the input; parent created with mode 0700 when missing.

#### `--content-encoding CONTENT_ENCODING`

Body encoding. Default: auto, which recognises Zstandard magic bytes and otherwise uses identity.

Type: enum. Required: false. Repeatable: false.

Default: "auto".

Choices: auto, identity, zstd.

Constraint: Exact enum; decoded size <= 8388608 bytes; zstd expansion <=128 times encoded size.

#### `--metadata-json METADATA_JSON`

Literal JSON turn metadata; not a filename. Default: read metadata from the request body.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: When supplied, parse one FixtureMetadata object; reject trailing JSON and duplicate keys.

### Preconditions

None beyond global rules.

### Effects and completion

- Offline only; no credentials or service access.
- Validate complete input before writing; atomically create output with mode 0600, without replacing an existing file.
- Repeated invocation with the same output fails with conflict; use a new output path.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `output_path`: absolute path of created fixture
- `fixture`: Fixture resource

Human output template:

```text
Created sanitised fixture: {output_path}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| fixture_input_invalid | 2 | Malformed metadata, unsupported encoding or size limit exceeded | The request cannot be converted to a sanitised fixture. |
| fixture_output_exists | 6 | Output exists | The output file already exists. |
| fixture_io_failed | 1 | Read or atomic write fails | The fixture file could not be read or written. |

### Examples

```sh
cq codex proxy fixture create --input request.json --output fixture.json --metadata-json '{"session_id":"s1","thread_id":"t1","turn_id":"u1","request_kind":"turn"}'
```

Convert the request file prepared in the fixture workflow.

### Compatibility spellings

- `cq codex validate capture` → `codex proxy fixture create; rename --metadata to --metadata-json; empty --content-encoding maps to auto`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_validate.go:18`
- `internal/proxy/codex_fixture.go:14`
- `internal/proxy/codex_zstd.go:21`
- `v0.32.5:cmd/cq/codex_validate.go:18`

## `cq codex proxy hook`

Produce receipt annotations for native client lifecycle hooks.

```text
cq codex proxy hook <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-hook.txt) · Family: navigation · Kind: group

Produce receipt annotations for native client lifecycle hooks.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `hook`: A machine integration invoked by the Codex client.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-hook.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy hook --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy hook stop`

Return a private-data-safe routing receipt for a Codex Stop hook.

```text
cq codex proxy hook stop [OPTIONS]
```

[Exact help](help/codex-proxy-hook-stop.txt) · Family: validation · Kind: command

Machine integration: read exactly one Codex Stop event JSON object from standard input and look up its turn receipt through authenticated loopback control.
Default output follows the Codex hook protocol: {} when absent, or {"systemMessage":"..."} when found. With --json, wrap that same object in the canonical CQ envelope for manual inspection; do not use --json in Codex hook configuration.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `hook`: A machine integration invoked by the Codex client.
- `stop`: Names the Codex Stop lifecycle event received on stdin; this command does not stop the service.

### Arguments

None.

### Options

None.

### Preconditions

- stdin is one HookStopEvent object.
- Existing proxy configuration includes local control authentication.

### Effects and completion

- Reads stdin up to 16 MiB; ignores additional event fields without logging or storing them.
- Sends only session_id and turn_id to the existing configured loopback endpoint; 5s fixed timeout, redirects forbidden.
- Does not alter routing, allocate an account, write credentials or create configuration.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `systemMessage`: optional string; formatted receipt message defined in validation notes; absent when no receipt found

Human output template:

```text
{"systemMessage":"{receipt_message}"}
OR
{}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| hook_input_invalid | 2 | Invalid JSON, oversized input, wrong event or invalid identifiers | Invalid Codex Stop hook input. |
| hook_unavailable | 4 | Configuration or receipt endpoint unavailable | The Codex turn receipt lookup is unavailable. |
| hook_auth_failed | 5 | Control returns HTTP401/403 | The Codex turn receipt lookup was not authorised. |
| hook_timeout | 7 | 5s elapsed | The Codex turn receipt lookup timed out. |

### Examples

```sh
printf '%s\n' '{"hook_event_name":"Stop","session_id":"s1","turn_id":"u1"}' | cq codex proxy hook stop
```

Look up a synthetic event; an unknown turn returns {}.

### Compatibility spellings

- `cq proxy hook codex-stop` → `codex proxy hook stop`. Deprecated spelling; no deprecation text on stdout; retain exact hook protocol.

### Source evidence

- `cmd/cq/proxy_codex_hook.go:33`

## `cq codex proxy lease`

Invalidate reusable Codex routing affinities.

```text
cq codex proxy lease <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-lease.txt) · Family: navigation · Kind: group

Invalidate reusable Codex routing affinities.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `lease`: Singular namespace for reusable task-account affinity.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-lease.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy lease --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy lease invalidate`

Invalidate reusable Codex task-account affinity.

```text
cq codex proxy lease invalidate [OPTIONS]
```

[Exact help](help/codex-proxy-lease-invalidate.txt) · Family: routing · Kind: command

Invalidate all reusable task-account leases from earlier admissions. Active request authority and required account continuity remain intact; later portable requests choose from current account capacity again.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `lease`: Singular namespace for reusable task-account affinity.
- `invalidate`: Expire reusable affinity without breaking active authority or required continuity.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with writable, migrated lease journal.

### Effects and completion

- Durably advance journal generation and invalidate reusable affinity.
- No restart required; repeated invalidation may advance generation but reports zero when no reusable affinity remains.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `invalidated_leases`: integer >=0; reusable affinities invalidated
- `journal_generation`: uint64; resulting durable journal generation

Human output template:

```text
Invalidated leases: {invalidated_leases}
Journal generation: {journal_generation}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| lease_journal_unavailable | 4 | Journal writer unavailable or quarantined legacy journal cannot mutate. | Lease journal is unavailable for invalidation. |

### Examples

```sh
cq codex proxy lease invalidate
```

Expire reusable affinity without breaking required continuity.

### Compatibility spellings

- `cq proxy leases invalidate` → `codex proxy lease invalidate`. Singular lease namespace; preserve optional port.

### Source evidence

- `v0.32.5:cmd/cq/proxy_leases.go:40`
- `v0.32.5:internal/proxy/codex_lease_invalidation.go:9`

## `cq codex proxy pin`

Inspect, set or clear the Codex proxy account override.

```text
cq codex proxy pin <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-pin.txt) · Family: navigation · Kind: group

Inspect, set or clear the Codex proxy account override.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `pin`: Explicit account override for new or unbound routed work.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-pin.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy pin --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy pin clear`

Clear the Codex proxy pin.

```text
cq codex proxy pin clear [OPTIONS]
```

[Exact help](help/codex-proxy-pin-clear.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `clear`: Remove the configuration; safe when already absent.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy pin clear
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy pin codex --clear` → `codex proxy pin clear`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy pin set`

Set the Codex proxy pin.

```text
cq codex proxy pin set ACCOUNT [OPTIONS]
```

[Exact help](help/codex-proxy-pin-set.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `set`: Create or replace the named configuration.

### Arguments

#### `ACCOUNT`

Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored.

Type: account-reference. Required: true. Repeatable: false.

Default: null.

Constraint: Nonempty.

Constraint: Resolve to exactly one known Codex account.

Constraint: Ambiguous email is rejected; use alias or AccountKey.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.
- Complete provider account inventory and aliases for set.

### Effects and completion

- Validate before saving configuration atomically.
- Repeated set to the same resolved account or clear of an absent value leaves equivalent configuration.
- No implicit restart; return restart_required=true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |

### Examples

```sh
cq codex proxy pin set "$ACCOUNT_REFERENCE"
```

Requires the account_reference preparation in routing notes.

### Compatibility spellings

- `cq proxy pin codex ACCOUNT` → `codex proxy pin set ACCOUNT`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy pin show`

Show the Codex proxy pin.

```text
cq codex proxy pin show [OPTIONS]
```

[Exact help](help/codex-proxy-pin-show.txt) · Family: routing · Kind: command

A pin selects one account for new and unbound work; required existing continuity remains on its bound account. A Codex pin overrides the configured default and allowlist.
Codex configuration changes require cq service restart --component proxy to take effect. They do not change the Codex system account identity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pin`: Explicit account override for new or unbound routed work.
- `show`: Read one resource without changing it.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing readable proxy configuration.

### Effects and completion

- Read configuration only; do not query or modify running routing.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: constant codex
- `kind`: constant pin
- `configured`: boolean; whether a selector is stored
- `account`: AccountKey|null; configured opaque Codex account reference, null when no selection is configured
- `application`: constant configured_only; saved configuration, no claim of live acknowledgement
- `restart_required`: boolean; true for mutations, false for show

Human output template:

```text
{provider} proxy {kind}: {account_or_not_configured}
Application: {application}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy pin show
```

Inspect or update the configured setting.

### Compatibility spellings

- `cq proxy pin codex` → `codex proxy pin show`. Deprecated provider-last spelling; translate before common validation.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:319`
- `v0.32.5:cmd/cq/proxy_codex_default.go:62`
- `v0.32.5:internal/proxy/codex_route_policy.go:145`

## `cq codex proxy policy`

Export or replace the complete Codex routing-policy document.

```text
cq codex proxy policy <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-policy.txt) · Family: navigation · Kind: group

Export or replace the complete Codex routing-policy document.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `policy`: Import or show the complete routing-policy document.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-policy.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy policy --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy policy apply`

Apply a complete Codex routing-policy document.

```text
cq codex proxy policy apply --file FILE [OPTIONS]
```

[Exact help](help/codex-proxy-policy-apply.txt) · Family: routing · Kind: command

Without --state-dir, use authenticated live control. With an explicit existing state root, operate offline; no live process is contacted.
The public document remains schema_version 1, independent of the CLI envelope schema_version 2. Account members are opaque AccountKeys, not email references. See RoutingPolicyDocument in routing notes.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `policy`: Import or show the complete routing-policy document.
- `apply`: Validate and publish a complete policy document.

### Arguments

None.

### Options

#### `--file FILE`

UTF-8 JSON RoutingPolicyDocument to apply. Relative paths resolve against the working directory. This is a complete replacement document, not a patch.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Readable regular file.

Constraint: One strict schema_version 1 JSON document; reject unknown fields and trailing data.

Constraint: Validate document and generation constraints before mutation.

#### `--state-dir STATE_DIR`

Read or update policy offline at this existing state root instead of contacting the running proxy. Omission selects authenticated live control.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: Clean absolute non-root directory.

Constraint: Mutually exclusive with --port.

Constraint: Do not create state; initialise it with cq proxy state initialise first.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing routing-policy state.
- Authenticated running proxy unless --state-dir explicitly selects offline state.

### Effects and completion

- Validate and apply one complete policy atomically; live mode takes effect immediately without restart.
- Offline mode updates selected state only; the caller must ensure no live owner is concurrently using it.
- Generation checks prevent replaying a stale policy; repeated stale input can conflict rather than silently succeed.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; exact public policy resource

Human output template:

```text
{policy_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| policy_document_invalid | 2 | Policy JSON violates schema or references. | Invalid routing-policy document: {detail}. |
| policy_generation_conflict | 6 | Input generations do not follow current authority state. | Policy generations conflict; show current policy and prepare a fresh document. |

### Examples

```sh
cq codex proxy policy apply --file "$POLICY_FILE"
```

POLICY_FILE preparation is specified in routing notes.

### Compatibility spellings

- `cq proxy policy apply` → `codex proxy policy apply; translate --state-root DIR to --state-dir DIR`. status renamed show because this prints a resource, not health; --port newly available for live mode.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:85`
- `v0.32.5:internal/proxy/routing_policy_store.go:129`

## `cq codex proxy policy show`

Show the complete Codex routing-policy document.

```text
cq codex proxy policy show [OPTIONS]
```

[Exact help](help/codex-proxy-policy-show.txt) · Family: routing · Kind: command

Without --state-dir, use authenticated live control. With an explicit existing state root, operate offline; no live process is contacted.
The public document remains schema_version 1, independent of the CLI envelope schema_version 2. Account members are opaque AccountKeys, not email references. See RoutingPolicyDocument in routing notes.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `policy`: Import or show the complete routing-policy document.
- `show`: Read one resource without changing it.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Read or update policy offline at this existing state root instead of contacting the running proxy. Omission selects authenticated live control.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: Clean absolute non-root directory.

Constraint: Mutually exclusive with --port.

Constraint: Do not create state; initialise it with cq proxy state initialise first.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing routing-policy state.
- Authenticated running proxy unless --state-dir explicitly selects offline state.

### Effects and completion

- Read policy only.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; exact public policy resource

Human output template:

```text
{policy_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| policy_document_invalid | 2 | Policy JSON violates schema or references. | Invalid routing-policy document: {detail}. |
| policy_generation_conflict | 6 | Input generations do not follow current authority state. | Policy generations conflict; show current policy and prepare a fresh document. |

### Examples

```sh
cq codex proxy policy show
```

Read live policy.

### Compatibility spellings

- `cq proxy policy status` → `codex proxy policy show; translate --state-root DIR to --state-dir DIR`. status renamed show because this prints a resource, not health; --port newly available for live mode.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:85`
- `v0.32.5:internal/proxy/routing_policy_store.go:129`

## `cq codex proxy pool`

Define account pools and their relative capacity-preservation values.

```text
cq codex proxy pool <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-pool.txt) · Family: navigation · Kind: group

Define account pools and their relative capacity-preservation values.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `pool`: One named collection of Codex accounts.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-pool.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy pool --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy pool rename`

Rename a pool without changing its identity.

```text
cq codex proxy pool rename OLD_NAME NEW_NAME [OPTIONS]
```

[Exact help](help/codex-proxy-pool-rename.txt) · Family: routing · Kind: command

Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pool`: One named collection of Codex accounts.
- `rename`: Change a pool display name while preserving its identity.

### Arguments

#### `OLD_NAME`

Case-insensitive pool name; preserve configured display casing.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.

#### `NEW_NAME`

Case-insensitive pool name; preserve configured display casing.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with routing-policy state.
- Existing pool.

### Effects and completion

- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; complete updated policy, defined in routing notes

Human output template:

```text
Pool policy updated.
{policy_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| pool_not_found | 3 | Named existing pool missing. | Pool not found: {name}. |
| pool_name_conflict | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

### Examples

```sh
cq codex proxy pool rename work primary
```

Uses the account_reference and work pool preparation in routing notes.

### Compatibility spellings

- `cq proxy policy pool rename` → `codex proxy pool rename`. Flatten resource beneath provider proxy; preserve arguments.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:285`
- `v0.32.5:internal/proxy/routing_policy_store.go:640`

## `cq codex proxy pool set`

Create a pool or replace its account membership.

```text
cq codex proxy pool set NAME --account ACCOUNT [OPTIONS]
```

[Exact help](help/codex-proxy-pool-set.txt) · Family: routing · Kind: command

Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pool`: One named collection of Codex accounts.
- `set`: Create or replace the named configuration.

### Arguments

#### `NAME`

Case-insensitive pool name; preserve configured display casing.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.

### Options

#### `--account ACCOUNT`

Unique Codex account email, CQ alias, or opaque AccountKey from cq codex account list. The reference resolves once; only AccountKey is stored. Repeat --account for each member; membership is replaced, not appended.

Type: account-reference. Required: true. Repeatable: true.

Default: null.

Constraint: Nonempty.

Constraint: Resolve to exactly one known Codex account.

Constraint: Ambiguous email is rejected; use alias or AccountKey.

Constraint: At least one member; duplicate resolved accounts rejected.

#### `--value VALUE`

Relative capacity-preservation value, 0–4294967295. Higher values preserve this pool by preferring lower-value viable accounts for ordinary unbound work. If omitted, preserve an existing pool value or use 0 for a new pool.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: existing value, otherwise 0

Constraint: Unsigned 32-bit integer.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with routing-policy state.
- Complete Codex account inventory for all references.

### Effects and completion

- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; complete updated policy, defined in routing notes

Human output template:

```text
Pool policy updated.
{policy_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| pool_not_found | 3 | Named existing pool missing. | Pool not found: {name}. |
| pool_name_conflict | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

### Examples

```sh
cq codex proxy pool set work --account "$ACCOUNT_REFERENCE" --value 10
```

Uses the account_reference and work pool preparation in routing notes.

### Compatibility spellings

- `cq proxy policy pool set` → `codex proxy pool set`. Flatten resource beneath provider proxy; preserve arguments.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:285`
- `v0.32.5:internal/proxy/routing_policy_store.go:640`

## `cq codex proxy pool value`

Set a pool’s relative capacity-preservation value.

```text
cq codex proxy pool value NAME VALUE [OPTIONS]
```

[Exact help](help/codex-proxy-pool-value.txt) · Family: routing · Kind: command

Pools contain Codex accounts. Names are case-insensitive selectors, while display casing is preserved.
Higher values preserve pool capacity by preferring lower-value viable accounts for ordinary unbound work. Values are not quota percentages; session bindings and required affinity remain constraints.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `pool`: One named collection of Codex accounts.
- `value`: Existing preservation value; not a quota limit or account count.

### Arguments

#### `NAME`

Case-insensitive pool name; preserve configured display casing.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.

#### `VALUE`

Relative capacity-preservation value, 0–4294967295. Higher values preserve this pool by preferring lower-value viable accounts for ordinary unbound work.

Type: integer. Required: true. Repeatable: false.

Default: null.

Constraint: Unsigned 32-bit integer.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with routing-policy state.
- Existing pool.

### Effects and completion

- Validate all input and publish one live atomic policy update.
- No proxy restart required.
- Equivalent repeated membership/value updates retain equivalent policy; rename preserves pool identity and bindings.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; complete updated policy, defined in routing notes

Human output template:

```text
Pool policy updated.
{policy_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| routing_account_not_found | 3 | Account reference matches no known account. | Account not found: {account}. |
| routing_account_ambiguous | 6 | Account reference matches multiple known accounts. | Account reference is ambiguous; use a unique alias or AccountKey. |
| routing_inventory_unavailable | 4 | Complete account inventory or alias index unavailable. | Account inventory is unavailable; no routing change was saved. |
| pool_not_found | 3 | Named existing pool missing. | Pool not found: {name}. |
| pool_name_conflict | 6 | New name belongs to a different pool. | Pool name already exists: {name}. |

### Examples

```sh
cq codex proxy pool value work 20
```

Uses the account_reference and work pool preparation in routing notes.

### Compatibility spellings

- `cq proxy policy pool value` → `codex proxy pool value`. Flatten resource beneath provider proxy; preserve arguments.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:285`
- `v0.32.5:internal/proxy/routing_policy_store.go:640`

## `cq codex proxy prime`

Inspect or toggle automatic Codex quota-window priming.

```text
cq codex proxy prime <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-prime.txt) · Family: navigation · Kind: group

Inspect or toggle automatic Codex quota-window priming.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `prime`: Existing quota-window priming feature; help explains the request it sends.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-prime.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy prime --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy prime disable`

Disable automatic Codex quota-window priming.

```text
cq codex proxy prime disable [OPTIONS]
```

[Exact help](help/codex-proxy-prime-disable.txt) · Family: routing · Kind: command

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `prime`: Existing quota-window priming feature; help explains the request it sends.
- `disable`: Disable the named feature; reserve disable is explicitly temporary.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing proxy configuration.

### Effects and completion

- Atomically persist enablement; equivalent repeated action is idempotent.
- No implicit restart or immediate priming request.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `enabled`: boolean; configured enablement
- `model_overrides`: map<string,string>; quota-window scope to model ID
- `restart_required`: boolean; false for status, true for mutation

Human output template:

```text
Codex window priming: {enabled_or_disabled}
Model overrides: {model_override_count}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy prime disable
```

Inspect configuration or persist enablement; restart proxy after a change.

### Compatibility spellings

- `cq proxy prime disable` → `codex proxy prime disable`. Preserve behaviour with explicit provider scope.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:259`
- `v0.32.5:internal/proxy/codex_primer.go:185`
- `v0.32.5:internal/proxy/config.go:21`

## `cq codex proxy prime enable`

Enable automatic Codex quota-window priming.

```text
cq codex proxy prime enable [OPTIONS]
```

[Exact help](help/codex-proxy-prime-enable.txt) · Family: routing · Kind: command

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `prime`: Existing quota-window priming feature; help explains the request it sends.
- `enable`: Enable the named feature.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing proxy configuration.

### Effects and completion

- Atomically persist enablement; equivalent repeated action is idempotent.
- No implicit restart or immediate priming request.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `enabled`: boolean; configured enablement
- `model_overrides`: map<string,string>; quota-window scope to model ID
- `restart_required`: boolean; false for status, true for mutation

Human output template:

```text
Codex window priming: {enabled_or_disabled}
Model overrides: {model_override_count}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy prime enable
```

Inspect configuration or persist enablement; restart proxy after a change.

### Compatibility spellings

- `cq proxy prime enable` → `codex proxy prime enable`. Preserve behaviour with explicit provider scope.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:259`
- `v0.32.5:internal/proxy/codex_primer.go:185`
- `v0.32.5:internal/proxy/config.go:21`

## `cq codex proxy prime status`

Show configured Codex quota-window priming.

```text
cq codex proxy prime status [OPTIONS]
```

[Exact help](help/codex-proxy-prime-status.txt) · Family: routing · Kind: command

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a cache refresh.
Enable and disable update configuration only. Restart the proxy to apply changes. Status reports configured state, not live-process acknowledgement.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `prime`: Existing quota-window priming feature; help explains the request it sends.
- `status`: Inspect configured and effective runtime state.

### Arguments

None.

### Options

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Existing proxy configuration.

### Effects and completion

- Read configured priming state.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `enabled`: boolean; configured enablement
- `model_overrides`: map<string,string>; quota-window scope to model ID
- `restart_required`: boolean; false for status, true for mutation

Human output template:

```text
Codex window priming: {enabled_or_disabled}
Model overrides: {model_override_count}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |

### Examples

```sh
cq codex proxy prime status
```

Inspect configuration or persist enablement; restart proxy after a change.

### Compatibility spellings

- `cq proxy prime status` → `codex proxy prime status`. Preserve behaviour with explicit provider scope.

### Source evidence

- `v0.32.5:cmd/cq/proxy.go:259`
- `v0.32.5:internal/proxy/codex_primer.go:185`
- `v0.32.5:internal/proxy/config.go:21`

## `cq codex proxy readiness`

Inspect retained transport-readiness evidence for an exact client build.

```text
cq codex proxy readiness <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-readiness.txt) · Family: navigation · Kind: group

Inspect retained transport-readiness evidence for an exact client build.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `readiness`: Retained validation evidence; not live service health.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-readiness.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy readiness --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy readiness show`

Check retained HTTP readiness evidence for an exact Codex build.

```text
cq codex proxy readiness show --client-build CLIENT_BUILD [OPTIONS]
```

[Exact help](help/codex-proxy-readiness-show.txt) · Family: validation · Kind: command

Checks the retained HTTP marker against the running CQ executable version, expected Codex build, parser and lease schemas, routing semantics, retry budget and fixture hash.
Success means the retained marker matches those requirements. It does not perform new requests or prove that the proxy is currently healthy.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `readiness`: Retained validation evidence; not live service health.
- `show`: Reads and displays retained evidence without running validation.

### Arguments

None.

### Options

#### `--client-build CLIENT_BUILD`

Expected Codex client version; required. Must match the recorded or exercised client version byte for byte; arbitrary build labels are not accepted.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Match ^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace.

Constraint: Require byte-for-byte equality with the evidence or executed client version; no whitespace trimming, case folding, prefix matching or arbitrary label substitution.

#### `--state-dir STATE_DIR`

Read or write readiness evidence in this absolute directory. Default: the resolved CQ proxy state directory.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: If present, nonempty canonical absolute directory; reject symlink traversal.

Constraint: If omitted, use ResolveDefaultPaths().StateDir; this is not the runtime resilience-state directory.

### Preconditions

None beyond global rules.

### Effects and completion

- Read-only local marker inspection; does not create configuration or renew evidence.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `transport`: literal http
- `client_build`: string, requested client build
- `current`: boolean, true on success
- `validated_at`: RFC3339 UTC timestamp of recorded validation
- `marker`: ReadinessMarker resource

Human output template:

```text
HTTP readiness: current
Codex build: {client_build}
Validated: {validated_at}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| readiness_missing | 3 | No HTTP marker exists | No HTTP readiness evidence is recorded. |
| readiness_stale | 6 | Marker does not match required tuple | HTTP readiness evidence does not match the requested build. |
| readiness_invalid | 1 | Marker is corrupt or unreadable | HTTP readiness evidence could not be read. |

### Examples

```sh
cq codex proxy readiness show --client-build 0.146.0
```

Inspect the recorded HTTP evidence for this exact client version.

### Compatibility spellings

- `cq codex validate http` → `codex proxy readiness show`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_validate.go:109`
- `internal/proxy/codex_readiness.go:85`
- `v0.32.5:cmd/cq/codex_validate.go:109`

## `cq codex proxy reserve`

Protect remaining quota on the current Codex system account.

```text
cq codex proxy reserve <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-reserve.txt) · Family: navigation · Kind: group

Protect remaining quota on the current Codex system account.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `reserve`: Protect capacity on the current system account only.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-reserve.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy reserve --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy reserve clear`

Remove the system-account reserve configuration.

```text
cq codex proxy reserve clear [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-clear.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
Remove threshold configuration and any temporary bypass permanently. This differs from disable, which automatically rearms.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `clear`: Remove the configuration; safe when already absent.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.

### Effects and completion

- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reserve`: RoutingReserveStatus; defined in routing notes

Human output template:

```text
System account reserve: {configured}
Account: {email_or_unavailable}
Window: {window_or_none}
Threshold: {percent}%
Enabled: {enabled}
Blocked: {blocked}
Reason: {reason_or_none}
Remaining: {remaining_pct_or_unknown}
Reset: {reset_at_or_unknown}
Observed: {observed_at_or_unknown}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve clear
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve clear` → `codex proxy reserve clear`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy reserve disable`

Disable the reserve until a verified reset.

```text
cq codex proxy reserve disable [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-disable.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
Disable protection temporarily until fresh usage proves a reset, or the system account changes. Fresh usage and reset evidence are required. After the saved reset time passes without fresh proof, the account remains blocked.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `disable`: Disable the named feature; reserve disable is explicitly temporary.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.
- Configured reserve exists.

### Effects and completion

- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reserve`: RoutingReserveStatus; defined in routing notes

Human output template:

```text
System account reserve: {configured}
Account: {email_or_unavailable}
Window: {window_or_none}
Threshold: {percent}%
Enabled: {enabled}
Blocked: {blocked}
Reason: {reason_or_none}
Remaining: {remaining_pct_or_unknown}
Reset: {reset_at_or_unknown}
Observed: {observed_at_or_unknown}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve disable
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve disable` → `codex proxy reserve disable`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy reserve enable`

Enable the configured system-account reserve.

```text
cq codex proxy reserve enable [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-enable.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
Remove the temporary bypass immediately. Existing threshold configuration is retained; fresh usage still determines eligibility.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `enable`: Enable the named feature.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.
- Configured reserve exists.

### Effects and completion

- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reserve`: RoutingReserveStatus; defined in routing notes

Human output template:

```text
System account reserve: {configured}
Account: {email_or_unavailable}
Window: {window_or_none}
Threshold: {percent}%
Enabled: {enabled}
Blocked: {blocked}
Reason: {reason_or_none}
Remaining: {remaining_pct_or_unknown}
Reset: {reset_at_or_unknown}
Observed: {observed_at_or_unknown}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve enable
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve enable` → `codex proxy reserve enable`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy reserve set`

Set the system-account quota reserve.

```text
cq codex proxy reserve set --window WINDOW --percent PERCENT [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-set.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.
Replace the selected window and percentage and enable protection immediately. Only one reserve window is configured at a time.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `set`: Create or replace the named configuration.

### Arguments

None.

### Options

#### `--window WINDOW`

Exact window selector returned by cq codex proxy reserve windows; for example 7d. No inferred provider or window default.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Must be a currently observed system-account quota window.

Constraint: Canonicalise legacy case, surrounding spaces, and underscores exactly as released parser, then validate observed selector.

#### `--percent PERCENT`

Protect this percentage of the full window quota. The system account is blocked at or below this remaining percentage; 2 means keep the final 2 percentage points, not 2% of current remaining quota.

Type: number. Required: true. Repeatable: false.

Default: null.

Constraint: Finite number strictly greater than 0 and strictly less than 100.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.

### Effects and completion

- POST persists reserve state and applies it live.
- Repeated clear or enable yields equivalent state; repeated disable records the current verified bypass boundary; set replaces any previous bypass.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reserve`: RoutingReserveStatus; defined in routing notes

Human output template:

```text
System account reserve: {configured}
Account: {email_or_unavailable}
Window: {window_or_none}
Threshold: {percent}%
Enabled: {enabled}
Blocked: {blocked}
Reason: {reason_or_none}
Remaining: {remaining_pct_or_unknown}
Reset: {reset_at_or_unknown}
Observed: {observed_at_or_unknown}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve set --window 7d --percent 2
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve set` → `codex proxy reserve set`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy reserve status`

Show system-account reserve state and evidence.

```text
cq codex proxy reserve status [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-status.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `status`: Inspect configured and effective runtime state.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.

### Effects and completion

- GET observes live reserve state; it can reconcile reset evidence and system-account changes. No provider quota request is made by this CLI.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reserve`: RoutingReserveStatus; defined in routing notes

Human output template:

```text
System account reserve: {configured}
Account: {email_or_unavailable}
Window: {window_or_none}
Threshold: {percent}%
Enabled: {enabled}
Blocked: {blocked}
Reason: {reason_or_none}
Remaining: {remaining_pct_or_unknown}
Reset: {reset_at_or_unknown}
Observed: {observed_at_or_unknown}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve status
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve status` → `codex proxy reserve status`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy reserve windows`

List observed system-account quota window selectors.

```text
cq codex proxy reserve windows [OPTIONS]
```

[Exact help](help/codex-proxy-reserve-windows.txt) · Family: routing · Kind: command

Protect only the current Codex system account; other accounts remain eligible independently of pool membership. Changes apply through authenticated live control without restarting the proxy.
Unavailable, stale, expired, or unsettled usage can block the protected account. A nominal reset time alone does not prove renewed capacity.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `reserve`: Protect capacity on the current system account only.
- `windows`: List provider-defined quota-window selectors rather than inventing a fixed enum.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Running authenticated proxy with reserve support.

### Effects and completion

- GET observes live reserve state; it can reconcile reset evidence and system-account changes. No provider quota request is made by this CLI.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `windows`: array<RoutingReserveWindow>, sorted by selector; defined in routing notes

Human output template:

```text
For each window, sorted by selector: {selector}
. If empty: No system-account quota windows available.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| reserve_not_configured | 6 | Enable or disable requested without reserve configuration. | Reserve is not configured; use reserve set first. |
| reserve_evidence_required | 6 | Disable lacks fresh system-account usage/reset evidence. | Fresh usage and reset evidence are required to disable the reserve. |
| reserve_window_unavailable | 6 | Requested window is not currently observed. | Selected window is unavailable; inspect reserve windows. |

### Examples

```sh
cq codex proxy reserve windows
```

For set, first confirm 7d appears in reserve windows; other actions need running proxy.

### Compatibility spellings

- `cq proxy reserve windows` → `codex proxy reserve windows`. Preserve arguments, normalise global --json.

### Source evidence

- `v0.32.5:cmd/cq/proxy_reserve.go:36`
- `v0.32.5:internal/proxy/codex_reserve.go:129`
- `v0.32.5:internal/proxy/codex_reserve.go:326`

## `cq codex proxy session`

Inspect or change the routing pool bound to a client session.

```text
cq codex proxy session <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-session.txt) · Family: navigation · Kind: group

Inspect or change the routing pool bound to a client session.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `session`: An exact privacy-keyed routing binding.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-session.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy session --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy session bind`

Bind one exact session to a Codex pool.

```text
cq codex proxy session bind --pool POOL [OPTIONS]
```

[Exact help](help/codex-proxy-session-bind.txt) · Family: routing · Kind: command

Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `session`: An exact privacy-keyed routing binding.
- `bind`: Create or replace the session-to-pool assignment.

### Arguments

None.

### Options

#### `--pool POOL`

Case-insensitive pool name; preserve configured display casing.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Valid UTF-8, contains at least one non-whitespace character, contains no control characters.

#### `--session-id SESSION_ID`

Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.

Constraint: Mutually exclusive with --session-id-stdin and --digest.

#### `--session-id-stdin`

Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Input must be valid UTF-8 containing 1–4096 bytes when true.

Constraint: Mutually exclusive with --session-id and --digest.

#### `--digest DIGEST`

Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest.

Type: digest. Required: false. Repeatable: false.

Default: null.

Constraint: 64 lowercase hexadecimal characters.

Constraint: Mutually exclusive with --session-id and --session-id-stdin.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Exactly one selector is required except for list.
- Running authenticated proxy, except digest with an existing --digest.
- Pool exists.

### Effects and completion

- Bind replaces an existing binding or creates one; unbind removes an existing binding; publish atomically with no restart.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; complete updated policy

Human output template:

```text
{result_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| session_binding_not_found | 3 | Show or unbind selector has no binding. | Session binding not found. |
| pool_not_found | 3 | Bind names an absent pool. | Pool not found: {name}. |

### Examples

```sh
cq codex proxy session bind --pool work --session-id example-session
```

Uses a literal example session; bind needs prepared work pool.

### Compatibility spellings

- `cq proxy policy session bind` → `codex proxy session bind`. Flatten provider-scoped resource; preserve flags.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:421`
- `v0.32.5:cmd/cq/proxy_policy.go:517`

## `cq codex proxy session digest`

Return the keyed digest for a session selector.

```text
cq codex proxy session digest [OPTIONS]
```

[Exact help](help/codex-proxy-session-digest.txt) · Family: routing · Kind: command

Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.
Raw identifiers are digested by authenticated live control. An existing --digest is returned unchanged; no lookup is required for that case.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `session`: An exact privacy-keyed routing binding.
- `digest`: Compute or normalise the keyed session selector.

### Arguments

None.

### Options

#### `--session-id SESSION_ID`

Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.

Constraint: Mutually exclusive with --session-id-stdin and --digest.

#### `--session-id-stdin`

Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Input must be valid UTF-8 containing 1–4096 bytes when true.

Constraint: Mutually exclusive with --session-id and --digest.

#### `--digest DIGEST`

Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest.

Type: digest. Required: false. Repeatable: false.

Default: null.

Constraint: 64 lowercase hexadecimal characters.

Constraint: Mutually exclusive with --session-id and --session-id-stdin.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Exactly one selector is required except for list.
- Running authenticated proxy, except digest with an existing --digest.

### Effects and completion

- Read only; raw selector requires authenticated live digest request. Existing --digest is returned unchanged without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `session_digest`: digest; 64 lowercase hex

Human output template:

```text
{result_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| session_binding_not_found | 3 | Show or unbind selector has no binding. | Session binding not found. |
| pool_not_found | 3 | Bind names an absent pool. | Pool not found: {name}. |

### Examples

```sh
cq codex proxy session digest --session-id example-session
```

Uses a literal example session; bind needs prepared work pool.

### Compatibility spellings

- `cq proxy policy session digest` → `codex proxy session digest`. Flatten provider-scoped resource; preserve flags.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:421`
- `v0.32.5:cmd/cq/proxy_policy.go:517`

## `cq codex proxy session list`

List all session-to-pool bindings.

```text
cq codex proxy session list [OPTIONS]
```

[Exact help](help/codex-proxy-session-list.txt) · Family: routing · Kind: command

List configured session digests and their pool names; raw identifiers are never returned.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `session`: An exact privacy-keyed routing binding.
- `list`: Read all resources of this kind.

### Arguments

None.

### Options

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Exactly one selector is required except for list.
- Running authenticated proxy, except digest with an existing --digest.

### Effects and completion

- Read only; a raw selector requires an authenticated digest request.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `bindings`: array<RoutingSessionBinding>; sorted by session_digest

Human output template:

```text
{result_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| session_binding_not_found | 3 | Show or unbind selector has no binding. | Session binding not found. |
| pool_not_found | 3 | Bind names an absent pool. | Pool not found: {name}. |

### Examples

```sh
cq codex proxy session list
```

Uses a literal example session; bind needs prepared work pool.

### Compatibility spellings

- `cq proxy policy session list` → `codex proxy session list`. Flatten provider-scoped resource; preserve flags.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:421`
- `v0.32.5:cmd/cq/proxy_policy.go:517`

## `cq codex proxy session show`

Show one session-to-pool binding.

```text
cq codex proxy session show [OPTIONS]
```

[Exact help](help/codex-proxy-session-show.txt) · Family: routing · Kind: command

Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `session`: An exact privacy-keyed routing binding.
- `show`: Read one resource without changing it.

### Arguments

None.

### Options

#### `--session-id SESSION_ID`

Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.

Constraint: Mutually exclusive with --session-id-stdin and --digest.

#### `--session-id-stdin`

Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Input must be valid UTF-8 containing 1–4096 bytes when true.

Constraint: Mutually exclusive with --session-id and --digest.

#### `--digest DIGEST`

Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest.

Type: digest. Required: false. Repeatable: false.

Default: null.

Constraint: 64 lowercase hexadecimal characters.

Constraint: Mutually exclusive with --session-id and --session-id-stdin.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Exactly one selector is required except for list.
- Running authenticated proxy, except digest with an existing --digest.

### Effects and completion

- Read only; a raw selector requires an authenticated digest request.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `binding`: RoutingSessionBinding

Human output template:

```text
{result_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| session_binding_not_found | 3 | Show or unbind selector has no binding. | Session binding not found. |
| pool_not_found | 3 | Bind names an absent pool. | Pool not found: {name}. |

### Examples

```sh
cq codex proxy session show --session-id example-session
```

Uses a literal example session; bind needs prepared work pool.

### Compatibility spellings

- `cq proxy policy session show` → `codex proxy session show`. Flatten provider-scoped resource; preserve flags.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:421`
- `v0.32.5:cmd/cq/proxy_policy.go:517`

## `cq codex proxy session unbind`

Remove one exact session-to-pool binding.

```text
cq codex proxy session unbind [OPTIONS]
```

[Exact help](help/codex-proxy-session-unbind.txt) · Family: routing · Kind: command

Use exactly one selector: raw --session-id, exact bytes on --session-id-stdin, or a full keyed --digest. Session identifiers are not printed in normal output; bindings expose only keyed digests.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `session`: An exact privacy-keyed routing binding.
- `unbind`: Remove the exact session assignment.

### Arguments

None.

### Options

#### `--session-id SESSION_ID`

Exact raw session identifier. Use exactly one session selector; prefer --session-id-stdin to avoid placing the identifier in shell history.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.

Constraint: Mutually exclusive with --session-id-stdin and --digest.

#### `--session-id-stdin`

Read the exact UTF-8 session identifier from stdin, 1–4096 bytes. No newline is removed; use printf rather than echo.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Input must be valid UTF-8 containing 1–4096 bytes when true.

Constraint: Mutually exclusive with --session-id and --digest.

#### `--digest DIGEST`

Existing full keyed session digest; exactly 64 lowercase hexadecimal characters. Obtain it with cq codex proxy session digest.

Type: digest. Required: false. Repeatable: false.

Default: null.

Constraint: 64 lowercase hexadecimal characters.

Constraint: Mutually exclusive with --session-id and --session-id-stdin.

#### `--port PORT`

Connect to the authenticated loopback proxy on this port (1–65535). If omitted, use the existing proxy configuration port, or 19280 when that field is unset.

Type: integer. Required: false. Repeatable: false.

Default: null.

When omitted: configured port, otherwise 19280

Constraint: Integer 1–65535.

Constraint: No configuration file is created to resolve omission.

#### `--timeout TIMEOUT`

Maximum elapsed time for this command, including local preparation and control requests; default 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration.

Constraint: Expiry returns exit 7; a timed-out mutation may already have committed, so inspect state before retrying.

### Preconditions

- Exactly one selector is required except for list.
- Running authenticated proxy, except digest with an existing --digest.

### Effects and completion

- Bind replaces an existing binding or creates one; unbind removes an existing binding; publish atomically with no restart.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `policy`: RoutingPolicyDocument; complete updated policy

Human output template:

```text
{result_json_pretty}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| routing_timeout | 7 | Command deadline expires. | Routing operation timed out; inspect current state before retrying a mutation. |
| routing_control_unavailable | 4 | Required live proxy control is unreachable or unavailable. | Running CQ proxy control is unavailable. |
| routing_auth_failed | 5 | Local proxy token missing or control rejects authentication. | Local proxy authentication failed. |
| routing_conflict | 6 | State changed or runtime preconditions fail. | Routing state conflict: {detail}. |
| session_binding_not_found | 3 | Show or unbind selector has no binding. | Session binding not found. |
| pool_not_found | 3 | Bind names an absent pool. | Pool not found: {name}. |

### Examples

```sh
cq codex proxy session unbind --session-id example-session
```

Uses a literal example session; bind needs prepared work pool.

### Compatibility spellings

- `cq proxy policy session unbind` → `codex proxy session unbind`. Flatten provider-scoped resource; preserve flags.

### Source evidence

- `v0.32.5:cmd/cq/proxy_policy.go:421`
- `v0.32.5:cmd/cq/proxy_policy.go:517`

## `cq codex proxy trace`

Read or follow retained Codex request traces.

```text
cq codex proxy trace [OPTIONS]
```

[Exact help](help/codex-proxy-trace.txt) · Family: routing · Kind: command

Read configured local log files without contacting or changing the proxy. Filters combine with logical AND; apply tail after filtering retained history.
JSON mode is an explicit JSONL stream exception: emit one CLI v2 envelope containing data.record for each event, then exactly one terminal envelope containing data.end. Successful completion uses ok=true, errors=[], exit 0. Timeout uses ok=false with trace_timeout, exit 7; interruption uses ok=false with trace_interrupted, exit 130. No second error envelope follows a terminal envelope.
Follow preserves records across retained rotation. If logs rotate beyond retained history, report a gap instead of silently claiming continuity. Payload mode reads already captured request/response content.

### Naming rationale

- `codex`: Provider-first scope: these operations affect Codex routing.
- `proxy`: Distinguish proxy routing from the provider account used directly by its own client.
- `trace`: Read or follow recorded causal request events.

### Arguments

None.

### Options

#### `--session SESSION`

Match a raw session ID or codex://threads/ID against privacy-safe session/thread keys. Omission matches all sessions.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: After trimming surrounding whitespace and extracting the final slash-delimited component, selector must be nonempty.

Constraint: Use existing privacy key derivation for raw and codex://threads/ID forms; reject empty normalised input rather than dropping the filter.

#### `--trace TRACE`

Match this exact trace ID. Omission matches all trace IDs.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: Nonempty trace ID.

#### `--since SINCE`

Include records at or after command-start time minus this positive duration. Omission imposes no lower time bound.

Type: duration. Required: false. Repeatable: false.

Default: null.

Constraint: Positive Go duration.

#### `--tail TAIL`

Return the newest N matching retained records before following; default 200. Use 0 for all matching retained records.

Type: integer. Required: false. Repeatable: false.

Default: 200.

Constraint: Integer >=0.

#### `--follow`

Continue reading new records and rotated logs until interrupted. No proxy connection is opened.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--payload`

Read the existing opt-in payload log instead of causal route events. This may display recorded user content; it does not enable capture.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--timeout TIMEOUT`

Optional maximum elapsed reading/following time; omission has no command deadline. Expiry returns exit 7.

Type: duration. Required: false. Repeatable: false.

Default: null.

Constraint: Positive Go duration when specified.

### Preconditions

- Existing proxy configuration and configured selected log path.
- Payload mode requires an existing opt-in payload log setting; this command never enables it.

### Effects and completion

- Read local files only.
- Missing files under a configured path produce zero records; follow waits for creation.
- Malformed/nonmatching records are skipped as in the existing reader; accepted records retain chronology across log segments.
- Payload output redacts documented authentication headers and known authentication fields. Arbitrary prompt or response content may still contain secrets; no blanket redaction guarantee is made.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `record`: RoutingTraceRecord|RoutingPayloadRecord; exactly one when an event is emitted
- `end`: RoutingTraceEnd; exactly one terminal data object instead of record, including timeout/interruption

Human output template:

```text
{time} {trace_id} #{sequence} {transport} {phase_or_route_summary} {nonempty_details}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| routing_invalid_argument | 2 | Unknown, duplicate, missing, malformed, or mutually exclusive arguments. | Invalid argument: {detail}. |
| routing_io_failed | 1 | Configuration, state, or output cannot be read or written. | Routing operation failed: {detail}. |
| trace_timeout | 7 | Optional command deadline expires. | Trace reading timed out. |
| trace_not_configured | 4 | Selected diagnostics log path is not configured. | Selected trace log is not configured. |
| trace_history_gap | 8 | Required rotation history no longer retained or ends with incomplete record. | Trace history has a gap: {detail}. |
| trace_interrupted | 130 | Interrupt received during follow. | Trace following interrupted. |

### Examples

```sh
cq codex proxy trace --since 15m --tail 50
```

Read the latest 50 matching records from the last 15 minutes.

```sh
cq codex proxy trace --session codex://threads/example-session --follow --json
```

Follow one session using explicit JSONL envelopes.

### Compatibility spellings

- `cq proxy trace` → `codex proxy trace`. Preserve filters; legacy raw JSONL is retired in favour of typed v2 envelopes.

### Source evidence

- `v0.32.5:cmd/cq/proxy_trace.go:29`
- `v0.32.5:cmd/cq/proxy_trace.go:98`
- `v0.32.5:cmd/cq/proxy_trace.go:170`
- `v0.32.5:internal/proxy/diag.go:512`

## `cq codex proxy validate`

Request HTTP checks or run WebSocket transport validation.

```text
cq codex proxy validate <COMMAND> [OPTIONS]
```

[Exact help](help/codex-proxy-validate.txt) · Family: navigation · Kind: group

Request HTTP checks or run WebSocket transport validation.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `proxy`: The shared local API listener and runtime serving supported providers.
- `validate`: Check release evidence and issue a validation receipt.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-proxy-validate.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex proxy validate --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex proxy validate http`

Request startup validation of an installed candidate HTTP listener.

```text
cq codex proxy validate http --port PORT [OPTIONS]
```

[Exact help](help/codex-proxy-validate-http.txt) · Family: validation · Kind: command

Requires an installed candidate service on an explicitly selected non-live port. Restarts only that verified candidate service in one-shot validation mode.
Success means the request was durably recorded and the restart was accepted; it does not mean validation passed. Use cq codex proxy readiness show --client-build BUILD to inspect recorded evidence after startup.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `validate`: Runs or explicitly requests an active validation exercise.
- `http`: The HTTP request transport.

### Arguments

None.

### Options

#### `--port PORT`

Installed candidate loopback port; required. The live port 19280 is forbidden.

Type: integer. Required: true. Repeatable: false.

Default: null.

Constraint: 1..65535 except 19280.

Constraint: Must match verified loaded candidate service label, process and listener binding.

#### `--timeout TIMEOUT`

Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration, 1s <= value <= 5m.

Constraint: Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

### Preconditions

- Candidate service installed on the selected port.
- Installed candidate executable and listener ownership proof available on this platform.

### Effects and completion

- Durably records one-shot startup validation request and restarts the verified candidate service.
- On restart failure, cancels the request and invalidates affected readiness evidence; reports cleanup failure as operational error.
- Does not wait for validation completion; no implicit retry. Each invocation requests a new validation.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state`: literal requested
- `port`: integer, selected candidate port
- `validation_complete`: literal false
- `outcome`: literal accepted

Human output template:

```text
HTTP validation requested on candidate port {port}.
Validation has not completed.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| validation_candidate_unavailable | 4 | No supported candidate service or attestation | The installed candidate service is unavailable. |
| validation_candidate_changed | 6 | Candidate binding changed after inspection | The candidate service binding changed; no validation was requested. |
| validation_request_failed | 1 | Persistence, restart or cleanup fails | The HTTP validation request failed. |
| validation_timeout | 7 | Elapsed operation budget exceeded | The HTTP validation operation timed out; inspect readiness before retrying. |

### Examples

```sh
cq codex proxy validate http --port 19281
```

Request validation of an already installed candidate service on port 19281.

### Compatibility spellings

- `cq proxy validate-http` → `codex proxy validate http`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/proxy_http_validation_cli.go:45`

## `cq codex proxy validate websocket`

Validate WebSocket routing with an isolated installed Codex client.

```text
cq codex proxy validate websocket --client-build CLIENT_BUILD [OPTIONS]
```

[Exact help](help/codex-proxy-validate-websocket.txt) · Family: validation · Kind: command

Runs the exact installed Codex client against temporary loopback listeners and a synthetic upstream. Checks the executable build before recording readiness evidence.
Does not restart the configured proxy or send requests to a provider. Success means the isolated exercise passed; it is not live production traffic evidence.

### Naming rationale

- `codex`: Scopes this capability to the Codex provider.
- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `validate`: Runs or explicitly requests an active validation exercise.
- `websocket`: The WebSocket transport.

### Arguments

None.

### Options

#### `--client-build CLIENT_BUILD`

Expected Codex client version; required. Must match the recorded or exercised client version byte for byte; arbitrary build labels are not accepted.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Match ^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace.

Constraint: Require byte-for-byte equality with the evidence or executed client version; no whitespace trimming, case folding, prefix matching or arbitrary label substitution.

#### `--client-executable CLIENT_EXECUTABLE`

Codex executable to exercise. Default: search ChatGPT.app, Codex.app, then PATH, in that order.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: If supplied, resolve to absolute regular executable file.

Constraint: Default order: /Applications/ChatGPT.app/Contents/Resources/codex, /Applications/Codex.app/Contents/Resources/codex, then executable codex found through PATH.

Constraint: Observed version must equal --client-build.

#### `--state-dir STATE_DIR`

Read or write readiness evidence in this absolute directory. Default: the resolved CQ proxy state directory.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: If present, nonempty canonical absolute directory; reject symlink traversal.

Constraint: If omitted, use ResolveDefaultPaths().StateDir; this is not the runtime resilience-state directory.

#### `--timeout TIMEOUT`

Maximum elapsed operation time, including preparation, requests and cleanup. Default: 5m. Range: 30s..30m. Reserve the final 5s within this budget for cleanup; interactive confirmation time is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "5m".

Constraint: Go duration; 30s <= value <=30m.

Constraint: Reserve 5s within the configured timeout for cleanup; the main exercise must stop no later than timeout minus 5s.

Constraint: Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

### Preconditions

- Matching supported Codex executable available.
- Selected state directory is owner controlled.

### Effects and completion

- Invalidates existing WebSocket readiness before exercise, after lexical/precondition validation.
- Starts temporary local listeners and child Codex processes; closes and drains them on success, failure, interruption and timeout.
- Writes a new readiness marker only after all gates pass; failure leaves readiness invalidated.
- Repeated execution reruns validation and replaces only the selected WebSocket marker.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `transport`: literal websocket
- `client_build`: string, verified client build
- `validated_at`: RFC3339 UTC timestamp
- `marker`: ReadinessMarker resource

Human output template:

```text
WebSocket validation: passed
Codex build: {client_build}
Validated: {validated_at}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| validation_client_unavailable | 4 | Executable absent or attestation unsupported | The Codex client cannot be validated on this installation. |
| validation_build_mismatch | 6 | Version differs | The Codex executable does not match --client-build. |
| validation_failed | 1 | Exercise or cleanup fails | WebSocket validation failed; readiness was not recorded. |
| validation_timeout | 7 | Duration exceeded | WebSocket validation timed out; readiness was not recorded. |

### Examples

```sh
cq codex proxy validate websocket --client-build 0.146.0 --client-executable /opt/homebrew/bin/codex
```

Exercise the selected installed executable without touching the live service.

### Compatibility spellings

- `cq codex validate websocket` → `codex proxy validate websocket`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.

### Source evidence

- `cmd/cq/codex_validate.go:80`
- `internal/proxy/codex_installed_ws_validation.go:28`
- `v0.32.5:cmd/cq/codex_validate.go:80`

## `cq codex reset`

Inspect, schedule and use banked Codex quota-reset credits.

```text
cq codex reset <COMMAND> [OPTIONS]
```

[Exact help](help/codex-reset.txt) · Family: navigation · Kind: group

Inspect, schedule and use banked Codex quota-reset credits.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `codex`: Explicit provider scope; never inferred from another command.
- `reset`: A banked credit that restores shared usage percentages; singular resource namespace.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/codex-reset.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq codex reset --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq codex reset list`

List Codex banked reset credits.

```text
cq codex reset list [ACCOUNT] [OPTIONS]
```

[Exact help](help/codex-reset-list.txt) · Family: accounts · Kind: command

Use the same visible logical-account inventory as codex account list. Resolve exact opaque keys before unique case-insensitive email or existing CQ alias matches.
Read and consume through existing credential ownership: try eligible candidates for the same logical identity; only authentication failures permit another candidate or eligible CQ-owned refresh. Never rotate accounts, refresh externally owned credentials, switch native auth or broaden routing authority.
Omit ACCOUNT to inspect every visible account. Always fetch current reset-credit inventory; no quota cache is used. Malformed credit rows remain explicit per-account errors rather than disappearing silently.

### Naming rationale

- `codex`: Explicit reset-credit provider.
- `reset`: A banked credit that restores shared usage percentages; singular resource namespace.
- `list`: Inspect available and historical credits.

### Arguments

#### `ACCOUNT`

Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected. Omit to include every visible account.

Type: account-reference. Required: false. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 60s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

None beyond global rules.

### Effects and completion

- Fetch credit inventories; individual provider list requests retain the 5s limit.
- Eligible CQ-owned OAuth refresh may persist refreshed credentials; system and external sources remain read-only.
- Never consume credits, change natural reset dates, install services or activate accounts.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `accounts`: ResetAccountInventory[]
- `complete`: boolean: every account and credit row was inspected successfully

Human output template:

```text
Reset inventories: {accounts.length}; complete={complete}.
{for each account/credit: account.account_reference or "—"}	{credit.id}	{credit.status}	{credit.expires_at or "never"}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| codex_reset_credentials_unavailable | 4 | Visible credential inventory cannot be obtained. | Codex reset credentials are unavailable. |
| codex_reset_auth_failed | 5 | All eligible credential candidates are rejected for authentication. | Codex reset authentication failed. |
| codex_reset_timeout | 7 | Command deadline expires. | Codex reset command timed out. |
| codex_reset_upstream_failed | 1 | Provider rejects the request for a non-authentication reason. | Codex reset request failed. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| codex_reset_inventory_partial | 8 | One or more accounts or credit rows could not be read or validated. | Reset-credit inventory is incomplete. |

### Examples

```sh
cq codex reset list
```

List credits for every visible account.

```sh
cq codex reset list user@example.com --json
```

Inspect one account as JSON.

### Compatibility spellings

- `cq codex resets list` → `codex reset list (preserve ACCOUNT, --credit and --yes where accepted)`. Deprecated plural-group alias; same canonical v2 behaviour and output, warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/codex_resets.go:142`
- `v0.32.5:internal/app/codex_resets.go:115`
- `internal/provider/codex/reset_accounts.go:103`
- `internal/provider/codex/reset_credits.go:86`

## `cq codex reset recommend`

Recommend when to use Codex reset credits.

```text
cq codex reset recommend [OPTIONS]
```

[Exact help](help/codex-reset-recommend.txt) · Family: accounts · Kind: command

Use the same visible logical-account inventory as codex account list. Resolve exact opaque keys before unique case-insensitive email or existing CQ alias matches.
Read and consume through existing credential ownership: try eligible candidates for the same logical identity; only authentication failures permit another candidate or eligible CQ-owned refresh. Never rotate accounts, refresh externally owned credentials, switch native auth or broaden routing authority.
Fetch fresh usage and credit inventories for the whole visible account portfolio, update local burn-history estimates, then recommend timings. No account selector is accepted because a single-account view would change the optimisation problem.
Recommendations are advisory. Missing required input produces complete=false, no actionable use_at values, explicit blockers and exit 8. Low confidence and bounded-search exact=false are labelled independently from completeness.

### Naming rationale

- `codex`: Explicit reset-credit provider.
- `reset`: A banked credit that restores shared usage percentages; singular resource namespace.
- `recommend`: Compute advisory consumption timing without executing it.

### Arguments

None.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 120s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "120s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

None beyond global rules.

### Effects and completion

- Read fresh usage and credits; retain 5s credit-list and existing provider HTTP subrequest limits.
- Update local history estimates and eligible CQ-owned refreshed credentials only.
- Never schedule an automation or consume credits.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `schedule`: ResetSchedule

Human output template:

```text
Reset recommendation: complete={schedule.complete}, exact={schedule.exact}, confidence={schedule.confidence}.
Horizon: {schedule.horizon}.
{for each schedule.items: account_reference}	{credit_id}	{status}	{use_at or "not scheduled"}	{use_by or "no expiry"}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| codex_reset_credentials_unavailable | 4 | Visible credential inventory cannot be obtained. | Codex reset credentials are unavailable. |
| codex_reset_auth_failed | 5 | All eligible credential candidates are rejected for authentication. | Codex reset authentication failed. |
| codex_reset_timeout | 7 | Command deadline expires. | Codex reset command timed out. |
| codex_reset_upstream_failed | 1 | Provider rejects the request for a non-authentication reason. | Codex reset request failed. |
| codex_reset_recommendation_incomplete | 8 | Fresh portfolio inputs are missing or invalid. | Reset recommendation is incomplete; resolve the reported blockers. |

### Examples

```sh
cq codex reset recommend
```

Compute advisory timing for every visible account.

```sh
cq codex reset recommend --json --timeout 120s
```

Read a complete or explicitly blocked machine-readable schedule.

### Compatibility spellings

- `cq codex resets recommend` → `codex reset recommend (preserve ACCOUNT, --credit and --yes where accepted)`. Deprecated plural-group alias; same canonical v2 behaviour and output, warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/codex_resets.go:142`
- `v0.32.5:internal/app/codex_resets.go:115`
- `internal/provider/codex/reset_accounts.go:103`
- `internal/provider/codex/reset_credits.go:86`

## `cq codex reset use`

Consume one Codex banked reset credit.

```text
cq codex reset use ACCOUNT [OPTIONS]
```

[Exact help](help/codex-reset-use.txt) · Family: accounts · Kind: command

Use the same visible logical-account inventory as codex account list. Resolve exact opaque keys before unique case-insensitive email or existing CQ alias matches.
Read and consume through existing credential ownership: try eligible candidates for the same logical identity; only authentication failures permit another candidate or eligible CQ-owned refresh. Never rotate accounts, refresh externally owned credentials, switch native auth or broaden routing authority.
Use one credit for exactly one account. Omit --credit to resume the one unresolved attempt, otherwise choose the eligible available credit with the earliest expiry; credits without expiry sort last, ties sort by credit ID. Multiple unresolved attempts require an explicit credit ID.
Preview account, credit, current shared windows and recommendation on stderr. Prompt "Use reset credit {credit.id} for {account.display_name}? [y/N]"; only y or yes, case-insensitive, confirms. Blank or negative response cancels. --json and non-terminal stdin require --yes.
A banked reset restores shared-window usage percentages without moving natural reset timestamps. Consumption never buys credits. Repeated or uncertain requests reuse the persisted idempotency key for that account and credit; never select another credit to recover an uncertain result.

### Naming rationale

- `codex`: Explicit reset-credit provider.
- `reset`: A banked credit that restores shared usage percentages; singular resource namespace.
- `use`: Consume one exact banked credit after confirmation.

### Arguments

#### `ACCOUNT`

Select one account by its exact opaque account key, unique email, or existing CQ alias. Exact keys take precedence; emails and aliases match case-insensitively after trimming. Ambiguous or unstable identities are rejected.

Type: account-reference. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty if supplied.

Constraint: Resolve against one current provider inventory; never choose the first ambiguous match.

### Options

#### `--credit CREDIT_ID`

Consume this exact credit ID. Omit to resume the single unresolved attempt or choose the eligible credit with earliest expiry; no-expiry credits sort last and ties use bytewise credit ID order.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: Non-empty, no surrounding whitespace.

Constraint: Must belong to selected account.

Constraint: Must be supported codex_rate_limits and eligible, or be the exact unresolved replay credit.

#### `--yes`

Confirm the displayed destructive operation without a terminal prompt (default false). Required with --json or non-terminal stdin.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Only this operation is authorised; no future operation inherits consent.

#### `--timeout DURATION`

Limit total command work to this duration (default 120s; minimum 1s; maximum 10m). A timeout never authorises a second reset consumption. Time spent waiting for terminal confirmation is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "120s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

- One stable logical account and eligible credit resolve.
- Fresh account usage and credit selection validate before confirmation.
- Durable attempt storage is writable before upstream consumption.
- Explicit consent supplied.

### Effects and completion

- Persist or reuse the exact per-account/per-credit idempotency record before calling upstream; each consume HTTP request retains 10s limit.
- After confirmation, revalidate identity, credit and eligibility; do not silently substitute another credit.
- On reset or already_redeemed, clear only terminal attempt state, invalidate quota cache, refetch usage and update history.
- On uncertain transport/5xx/timeout outcome retain attempt and expose exact retry arguments; a retry does not create another idempotency key.
- No account activation, service installation, proxy policy change, reset-date change or credit purchase.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `account_reference`: string
- `credit_id`: string|null: null only when selection never completed
- `outcome`: enum(cancelled,reset,already_redeemed,nothing_to_reset,no_credit,indeterminate)
- `windows_reset`: integer >= 0; provider-reported count, 0 on cancellation/indeterminate
- `changed_windows`: ResetWindowChange[]
- `retry`: ResetRetry|null: present only for unresolved consumption

Human output template:

```text
Reset outcome: {outcome}.
Account: {account_reference}.
Credit: {credit_id or "none"}.
Windows reset: {windows_reset}.
{when retry: Retry the same credit: cq codex reset use {retry.account_reference shell-quoted} --credit {retry.credit_id shell-quoted} --yes}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| codex_reset_credentials_unavailable | 4 | Visible credential inventory cannot be obtained. | Codex reset credentials are unavailable. |
| codex_reset_auth_failed | 5 | All eligible credential candidates are rejected for authentication. | Codex reset authentication failed. |
| codex_reset_timeout | 7 | Command deadline expires. | Codex reset command timed out. |
| codex_reset_upstream_failed | 1 | Provider rejects the request for a non-authentication reason. | Codex reset request failed. |
| account_reference_empty | 2 | ACCOUNT contains only whitespace. | Account reference must not be empty. |
| account_not_found | 3 | No logical account matches ACCOUNT. | Account reference does not resolve. |
| account_ambiguous | 6 | Reference matches more than one logical account. | Account reference is ambiguous; use an exact account key. |
| account_unstable | 6 | Matched account lacks stable identity. | Account reference resolves to an unstable account. |
| codex_reset_confirmation_required | 6 | --json or non-terminal stdin without --yes. | Reset consumption requires --yes in non-interactive mode. |
| codex_reset_credit_not_found | 3 | Explicit credit is absent or changed after confirmation. | Selected reset credit is unavailable; no replacement credit was consumed. |
| codex_reset_credit_ineligible | 6 | Credit expired, unsupported, already terminal without replay, or not yet eligible. | Selected reset credit is not eligible. |
| codex_reset_pending_ambiguous | 6 | Several unresolved attempts exist and --credit is omitted. | Several reset attempts are unresolved; specify --credit. |
| codex_reset_attempt_unavailable | 1 | Durable attempt cannot be saved/read. | Reset attempt state is unavailable; no consumption was started. |
| codex_reset_consume_indeterminate | 1 | Consumption may have occurred but response is inconclusive. | Reset outcome is unknown; retry only the same account and credit. |
| codex_reset_postcheck_partial | 8 | Consumption is terminal but usage verification, history, cache or attempt cleanup fails. | Reset outcome is known, but local verification or cleanup is incomplete. |

### Examples

```sh
cq codex reset use user@example.com
```

Preview and confirm the next eligible reset.

```sh
cq codex reset use user@example.com --credit "$CREDIT_ID" --yes --json
```

Consume or safely replay the exact inventoried credit; prepare CREDIT_ID as described in notes.

### Compatibility spellings

- `cq codex resets use` → `codex reset use (preserve ACCOUNT, --credit and --yes where accepted)`. Deprecated plural-group alias; same canonical v2 behaviour and output, warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/codex_resets.go:142`
- `v0.32.5:internal/app/codex_resets.go:115`
- `internal/provider/codex/reset_accounts.go:103`
- `internal/provider/codex/reset_credits.go:86`

## `cq completion`

Print a shell completion script.

```text
cq completion SHELL [OPTIONS]
```

[Exact help](help/completion.txt) · Family: root · Kind: command

Print a completion script for Bash, Zsh or Fish to standard output. Source or install it yourself. Completion lists public command names, options and fixed enum choices; it never accesses credentials, accounts, network or proxy state.

### Naming rationale

- `completion`: Print a shell completion script without installing or sourcing it.

### Arguments

#### `SHELL`

Shell to generate for: bash, zsh, fish.

Type: enum. Required: true. Repeatable: false.

Default: null.

Choices: bash, zsh, fish.

Constraint: Exact lowercase enum.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Output shell code only in human mode; no files written. JSON returns that code as a string.
- Generated scripts complete canonical public paths, --help/--json/--version and per-command flags; use built-in shell file completion for path fields. Do not complete hidden interfaces or query dynamic sensitive values.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `shell`: enum bash|zsh|fish
- `script`: string UTF-8 complete shell script ending LF

Human output template:

```text
{script}
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq completion zsh > "$HOME/.cq-completion.zsh"
```

Write the generated script to a user-chosen file; source it in Zsh startup configuration.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `new documented interface; generated from commands.json`

## `cq gemini`

Inspect the externally managed Gemini account.

```text
cq gemini <COMMAND> [OPTIONS]
```

[Exact help](help/gemini.txt) · Family: navigation · Kind: group

Inspect the externally managed Gemini account.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `gemini`: Explicit provider scope; never inferred from another command.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/gemini.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq gemini --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq gemini account`

Show the single externally managed Gemini account.

```text
cq gemini account <COMMAND> [OPTIONS]
```

[Exact help](help/gemini-account.txt) · Family: navigation · Kind: group

Show the single externally managed Gemini account.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `gemini`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/gemini-account.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq gemini account --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq gemini account show`

Show the externally managed Gemini account.

```text
cq gemini account show [OPTIONS]
```

[Exact help](help/gemini-account-show.txt) · Family: accounts · Kind: command

Inspect whether the Antigravity-owned Gemini Keychain identity is configured. This is local inspection only; CQ does not authenticate, switch or remove Gemini accounts.
Configured means the external credential entry exists, not that its token is valid or that quota is available. Email is null when not available from safe local identity metadata.

### Naming rationale

- `gemini`: Explicit provider scope; never inferred from another command.
- `account`: A local credential identity, distinct from a proxy routing decision.
- `show`: Inspect the single externally managed Gemini identity.

### Arguments

None.

### Options

#### `--timeout DURATION`

Limit total command work to this duration (default 30s; minimum 1s; maximum 10m).

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 1s <= value <= 10m

### Preconditions

None beyond global rules.

### Effects and completion

- Use the Gemini provider discoverer to inspect the Antigravity credential entry.
- No network, OAuth refresh, project provisioning, credential parsing, credential persistence or service installation.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `configured`: boolean: external credential entry exists
- `account`: AccountSummary|null: null when unconfigured

Human output template:

```text
Gemini configured: {configured}.
Account: {account.display_name or "none"}.
Managed by: Antigravity.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| gemini_account_unavailable | 4 | Platform credential reader is unavailable or denies access. | Antigravity account configuration could not be read. |
| gemini_account_timeout | 7 | Local inspection deadline expires. | Antigravity account inspection timed out. |

### Examples

```sh
cq gemini account show
```

Inspect the single Antigravity-managed identity.

```sh
cq gemini account show --json
```

Inspect configuration without contacting Gemini.

### Compatibility spellings

- `cq gemini accounts` → `gemini account show (preserve arguments and flags)`. Deprecated compatibility alias. Same canonical semantics, validation, confirmation, output and exit codes; warning on stderr only.

### Source evidence

- `v0.32.5:cmd/cq/main.go:421`
- `v0.32.5:internal/provider/gemini/provider.go:51`

## `cq help`

Show help for a command path.

```text
cq help [COMMAND...] [OPTIONS]
```

[Exact help](help/help.txt) · Family: root · Kind: command

Show canonical help for the supplied command path. Omit path to show root help. This is equivalent to appending --help to that path.
Only command words are accepted after help; option values belong to the documented command, not this help request.

### Naming rationale

- `help`: Print exact help for a command path without opening user state.

### Arguments

#### `COMMAND`

Zero or more canonical command words, or a recognised legacy command path.

Type: string. Required: false. Repeatable: true.

Default: [].

Constraint: Resolve exact full group/leaf path; unknown child fails before state access.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Read embedded command specification only. Always plain help text, including when --json supplied; help is a terminating documentation mode.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
{exact generated help text for resolved command}
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq help codex proxy reserve set
```

Show exact reserve-setting syntax.

```sh
cq help
```

Show root help.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `new canonical documentation path replacing inconsistent help intercepts`

## `cq models`

Inspect model registries and publish local model-cache updates.

```text
cq models <COMMAND> [OPTIONS]
```

[Exact help](help/models.txt) · Family: models · Kind: group

Inspect model registries and publish local model-cache updates.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `models`: Existing model-registry namespace.

### Arguments

None.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/models.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq models --help
```

Show group help.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/help.go:83`
- `cmd/cq/help.go:341`

## `cq models list`

List active models from local registry caches.

```text
cq models list [OPTIONS]
```

[Exact help](help/models-list.txt) · Family: models · Kind: command

Reads cached native entries and local overlays without fetching providers. An empty cache produces an empty list; run cq models refresh to populate it.
Provider names are claude and codex; claude identifies the Anthropic API route.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
This command performs local reads only and has no HTTP request timeout.

### Naming rationale

- `models`: Existing model-registry namespace.
- `list`: Read and enumerate entries.

### Arguments

None.

### Options

#### `--provider PROVIDER`

Filter by claude or codex. Omitted: list both providers. Legacy anthropic means claude.

Type: enum. Required: false. Repeatable: false.

Default: null.

Choices: claude, codex.

Constraint: Reject other values; translate legacy anthropic to claude with deprecation warning.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Read model cache and overlays only; do not refresh credentials, fetch upstream data, install services, or rewrite caches.
- Sort results by provider then exact model ID.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `models`: array<ModelEntry>: active merged entries in deterministic order
- `provider`: claude|codex|null: applied filter

Human output template:

```text
MODEL	PROVIDER	SOURCE
{model.id}	{model.provider}	{model.source}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| models_store_failed | 1 | Cannot read, decode, or atomically write model state. | Cannot access model registry state: {detail}. |

### Examples

```sh
cq models list
```

List cached models for both providers.

```sh
cq models list --provider claude --json
```

List Claude models as structured JSON.

### Compatibility spellings

- `cq models list --provider anthropic` → `models list --provider claude`. Legacy provider value only; path unchanged.

### Source evidence

- `cmd/cq/models.go:287`
- `internal/modelregistry/entry.go:25`

## `cq models overlay`

Add, remove or prune local model-registry additions.

```text
cq models overlay <COMMAND> [OPTIONS]
```

[Exact help](help/models-overlay.txt) · Family: models · Kind: group

Add, remove or prune local model-registry additions.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `models`: Existing model-registry namespace.
- `overlay`: Local entries exposing model IDs before native discovery.

### Arguments

None.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/models-overlay.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq models overlay --help
```

Show group help.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/help.go:83`
- `cmd/cq/help.go:341`

## `cq models overlay add`

Add or replace one local model overlay and publish caches.

```text
cq models overlay add --provider PROVIDER --id ID [OPTIONS]
```

[Exact help](help/models-overlay-add.txt) · Family: models · Kind: command

An existing overlay with the same provider and exact ID is replaced. This command does not create an upstream model or prove upstream availability.
Metadata selection follows ModelCloneSelection. The overlay retains its requested ID; JSON reports the selected source and selection mode.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
Proxy refresh has a fixed 5-second context deadline. Local registry source refresh has a fixed 30-second phase deadline, and its HTTP client has a fixed 30-second per-request timeout. The legacy health probe, when needed, has a fixed 2-second timeout. File publication and pruning have no separate deadline.

### Naming rationale

- `models`: Existing model-registry namespace.
- `overlay`: Local entries exposing model IDs before native discovery.
- `add`: Create or replace one entry.

### Arguments

None.

### Options

#### `--provider PROVIDER`

Provider to modify: claude or codex. Required. Legacy value anthropic is accepted as claude.

Type: enum. Required: true. Repeatable: false.

Default: null.

Choices: claude, codex.

Constraint: Reject other values; translate legacy anthropic to claude with deprecation warning.

#### `--id ID`

Exact model ID to add or remove. Required; case-sensitive and non-empty; leading or trailing whitespace is rejected.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty UTF-8 string without leading or trailing whitespace or control characters.

Constraint: Do not normalise case.

#### `--clone-from CLONE_FROM`

Exact native model ID from the same provider to copy missing metadata from. Omitted: infer a source deterministically by model-name similarity. An explicit source absent from the validated native snapshot is an error.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: Non-empty UTF-8 string without leading or trailing whitespace or control characters.

Constraint: Do not normalise case.

Constraint: Explicit source must be native and match selected provider before overlay write.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Validate model identity, explicit clone source, and cross-provider routing collisions before writing.
- Atomically add or replace local overlay, then refresh and publish caches.
- A subsequent publication failure leaves the saved overlay in place and reports overlay_saved: true; no implicit rollback.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `model`: ModelEntry: saved overlay with effective metadata
- `overlay_saved`: boolean: whether overlay write committed
- `selection`: ModelCloneSelectionResult
- `publication`: ModelPublication: refresh source results and each attempted cache publication

Human output template:

```text
Overlay saved: {model.provider}/{model.id}
Metadata source: {selection.source_id_or_none} ({selection.mode})
Publication: {publication.status}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| models_store_failed | 1 | Cannot read, decode, or atomically write model state. | Cannot access model registry state: {detail}. |
| models_refresh_partial | 8 | At least one source or cache publication failed while another source or publication succeeded. | Model refresh completed partially; inspect source and publication results. |
| models_refresh_failed | 1 | No source refresh or required publication succeeded. | Model refresh failed: {detail}. |
| models_conflict | 6 | Same case-insensitive model ID would route to different providers. | Model ID {id} conflicts across providers: {providers}. |
| models_clone_not_found | 3 | Explicit clone source is absent from same-provider native snapshot. | Native clone source {provider}/{id} was not found; run cq models refresh and cq models list --provider {provider}. |

### Examples

```sh
cq models overlay add --provider codex --id "$NEW_MODEL" --clone-from "$SOURCE_MODEL"
```

Use the verified source model and desired new model ID prepared in the notes.

```sh
cq models overlay add --provider claude --id claude-sonnet-next
```

Infer metadata from available native Claude models.

### Compatibility spellings

- `cq models overlay add --provider anthropic` → `models overlay add --provider claude`. Translate provider value; retain remaining arguments.

### Source evidence

- `cmd/cq/models.go:352`
- `cmd/cq/models.go:433`
- `internal/modelregistry/infer.go:10`
- `internal/modelregistry/merge.go:57`
- `cmd/cq/models.go:58`
- `cmd/cq/models.go:162`
- `cmd/cq/models.go:178`
- `cmd/cq/local_registry.go:52`

## `cq models overlay prune`

Remove overlays now supplied by native model sources.

```text
cq models overlay prune [OPTIONS]
```

[Exact help](help/models-overlay-prune.txt) · Family: models · Kind: command

Refresh native model sources, then remove local overlays whose provider and exact model ID are now native. Applies to both providers.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
Proxy refresh has a fixed 5-second context deadline. Local registry source refresh has a fixed 30-second phase deadline, and its HTTP client has a fixed 30-second per-request timeout. The legacy health probe, when needed, has a fixed 2-second timeout. File publication and pruning have no separate deadline.

### Naming rationale

- `models`: Existing model-registry namespace.
- `overlay`: Local entries exposing model IDs before native discovery.
- `prune`: Delete entries made redundant by native discovery.

### Arguments

None.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Fetch sources and publish refreshed caches.
- Prune only provider/ID pairs positively present in refreshed native data; a failed provider source is not evidence for deletion.
- Atomically save remaining overlays. Repeated execution with unchanged sources removes zero entries.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `removed`: array<ModelIdentity>: removed provider and exact ID pairs
- `removed_count`: integer >= 0
- `publication`: ModelPublication: refresh source results and each attempted cache publication

Human output template:

```text
Pruned {removed_count} overlays.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| models_store_failed | 1 | Cannot read, decode, or atomically write model state. | Cannot access model registry state: {detail}. |
| models_refresh_partial | 8 | At least one source or cache publication failed while another source or publication succeeded. | Model refresh completed partially; inspect source and publication results. |
| models_refresh_failed | 1 | No source refresh or required publication succeeded. | Model refresh failed: {detail}. |
| models_conflict | 6 | Same case-insensitive model ID would route to different providers. | Model ID {id} conflicts across providers: {providers}. |

### Examples

```sh
cq models overlay prune
```

Prune redundant overlays across both providers.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/models.go:385`
- `cmd/cq/models.go:481`
- `cmd/cq/models.go:58`
- `cmd/cq/models.go:162`
- `cmd/cq/models.go:178`
- `cmd/cq/local_registry.go:52`

## `cq models overlay remove`

Remove one local model overlay and publish caches.

```text
cq models overlay remove --provider PROVIDER --id ID [OPTIONS]
```

[Exact help](help/models-overlay-remove.txt) · Family: models · Kind: command

Removes only the specified local overlay; native provider entries remain available.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
Proxy refresh has a fixed 5-second context deadline. Local registry source refresh has a fixed 30-second phase deadline, and its HTTP client has a fixed 30-second per-request timeout. The legacy health probe, when needed, has a fixed 2-second timeout. File publication and pruning have no separate deadline.

### Naming rationale

- `models`: Existing model-registry namespace.
- `overlay`: Local entries exposing model IDs before native discovery.
- `remove`: Delete one identified entry.

### Arguments

None.

### Options

#### `--provider PROVIDER`

Provider to modify: claude or codex. Required. Legacy value anthropic is accepted as claude.

Type: enum. Required: true. Repeatable: false.

Default: null.

Choices: claude, codex.

Constraint: Reject other values; translate legacy anthropic to claude with deprecation warning.

#### `--id ID`

Exact model ID to add or remove. Required; case-sensitive and non-empty; leading or trailing whitespace is rejected.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty UTF-8 string without leading or trailing whitespace or control characters.

Constraint: Do not normalise case.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Require matching overlay before write.
- Atomically remove selected entry, then refresh and publish caches.
- Publication failure leaves removal committed and reports overlay_removed: true.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `provider`: claude|codex
- `id`: string: exact removed model ID
- `overlay_removed`: boolean: whether removal committed
- `publication`: ModelPublication: refresh source results and each attempted cache publication

Human output template:

```text
Overlay removed: {provider}/{id}
Publication: {publication.status}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| models_store_failed | 1 | Cannot read, decode, or atomically write model state. | Cannot access model registry state: {detail}. |
| models_refresh_partial | 8 | At least one source or cache publication failed while another source or publication succeeded. | Model refresh completed partially; inspect source and publication results. |
| models_refresh_failed | 1 | No source refresh or required publication succeeded. | Model refresh failed: {detail}. |
| models_conflict | 6 | Same case-insensitive model ID would route to different providers. | Model ID {id} conflicts across providers: {providers}. |
| models_overlay_not_found | 3 | No overlay matches selected provider and exact ID. | Overlay {provider}/{id} was not found. |

### Examples

```sh
cq models overlay remove --provider codex --id gpt-5.5
```

Remove an existing Codex overlay.

### Compatibility spellings

- `cq models overlay remove --provider anthropic` → `models overlay remove --provider claude`. Translate provider value; retain remaining arguments.

### Source evidence

- `cmd/cq/models.go:368`
- `cmd/cq/models.go:454`
- `cmd/cq/models.go:58`
- `cmd/cq/models.go:162`
- `cmd/cq/models.go:178`
- `cmd/cq/local_registry.go:52`

## `cq models refresh`

Refresh model sources and publish client model caches.

```text
cq models refresh [OPTIONS]
```

[Exact help](help/models-refresh.txt) · Family: models · Kind: command

Merge native provider data with local overlays, validate routing ownership, and publish Codex model cache and available Claude Code capability and picker caches.
Use the running proxy registry refresh endpoint when healthy; otherwise refresh locally. A reachable proxy error is reported rather than silently retried through a different owner.
No overall command deadline is imposed. Individual request and phase deadlines are listed below; interactive prompts can wait until input or interruption.
Proxy refresh has a fixed 5-second context deadline. Local registry source refresh has a fixed 30-second phase deadline, and its HTTP client has a fixed 30-second per-request timeout. The legacy health probe, when needed, has a fixed 2-second timeout. File publication and pruning have no separate deadline.

### Naming rationale

- `models`: Existing model-registry namespace.
- `refresh`: Update the named resource from its source.

### Arguments

None.

### Options

None.

### Preconditions

- Validate all syntax and required values before state access or writes.

### Effects and completion

- Fetch provider model data and refresh eligible CQ-owned credentials as required by existing registry pipeline.
- Publish Codex and available Claude Code caches; missing optional client files are skipped and reported.
- Repeated execution refreshes data again; no service installation or restart.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `publication`: ModelPublication: refresh source results and each attempted cache publication

Human output template:

```text
Models refreshed: {publication.active_count}
{publication.target}: {publication.status}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| cli_invalid_usage | 2 | Unknown command, option, value, duplicate nonrepeatable option, or extra positional argument. | Invalid arguments: {detail}. Run cq {path} --help. |
| models_store_failed | 1 | Cannot read, decode, or atomically write model state. | Cannot access model registry state: {detail}. |
| models_refresh_partial | 8 | At least one source or cache publication failed while another source or publication succeeded. | Model refresh completed partially; inspect source and publication results. |
| models_refresh_failed | 1 | No source refresh or required publication succeeded. | Model refresh failed: {detail}. |
| models_conflict | 6 | Same case-insensitive model ID would route to different providers. | Model ID {id} conflicts across providers: {providers}. |

### Examples

```sh
cq models refresh
```

Refresh model data and publish caches.

```sh
cq models refresh --json
```

Report source and publication results as JSON.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/models.go:73`
- `cmd/cq/models.go:154`
- `cmd/cq/models.go:239`
- `cmd/cq/models.go:58`
- `cmd/cq/models.go:162`
- `cmd/cq/models.go:178`
- `cmd/cq/local_registry.go:52`

## `cq proxy`

Run and inspect the shared proxy, state and lifecycle machinery.

```text
cq proxy <COMMAND> [OPTIONS]
```

[Exact help](help/proxy.txt) · Family: navigation · Kind: group

Run and inspect the shared proxy, state and lifecycle machinery.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy candidate`

Manage isolated candidate state and inspect release evidence.

```text
cq proxy candidate <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-candidate.txt) · Family: navigation · Kind: group

Manage isolated candidate state and inspect release evidence.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-candidate.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy candidate --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy candidate client-safety`

Reserve measured client-safety operations; currently unavailable.

```text
cq proxy candidate client-safety <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-candidate-client-safety.txt) · Family: navigation · Kind: group

Reserve measured client-safety operations; currently unavailable.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `client-safety`: Evidence that registered clients cannot send credential-bearing application bytes to an unauthorised runtime.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-candidate-client-safety.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy candidate client-safety --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy candidate client-safety refresh`

Measure and renew client credential-isolation evidence.

```text
cq proxy candidate client-safety refresh --state-dir STATE_DIR --validation-run-id VALIDATION_RUN_ID [OPTIONS]
```

[Exact help](help/proxy-candidate-client-safety-refresh.txt) · Family: candidate · Kind: command

**Availability:** unavailable. Well-formed execution returns exit 4, `data=null`, and no human stdout. Reserved success semantics in the JSON catalogue are design notes, not enabled behaviour.

Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
Measure every registered sender, credential domain and transport. Issue a signed receipt only when no credential-bearing application bytes escaped the authorised runtime boundary.
The old term client-bearer-barrier describes an internal credential boundary; client-safety names the operator purpose. A declared zero or a hash derived from identifiers is not measured evidence.
The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `client-safety`: Evidence that registered clients cannot send credential-bearing application bytes to an unauthorised runtime.
- `refresh`: Re-measure and replace expiring safety evidence.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--validation-run-id VALIDATION_RUN_ID`

Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 150s. Allowed: 150s..5m inclusive; 30s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "150s".

Constraint: Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

### Effects and completion

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_evidence_unavailable | 4 | This reserved capability has no complete implemented proof-backend contract. | Candidate qualification evidence is unavailable; no transition was performed. |

### Examples

```sh
cq proxy candidate client-safety refresh --state-dir "$CANDIDATE" --validation-run-id "$VALIDATION_RUN"
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

- `cq proxy candidate client-bearer-barrier refresh` → `proxy candidate client-safety refresh; option names translate according to CandidateLegacyOptionsV1; a legacy final duration translates to --timeout.`. Deprecated spelling; retain semantic confirmations and reject unknown or duplicate arguments.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate prepare`

Prepare an isolated release candidate without starting it.

```text
cq proxy candidate prepare --state-dir STATE_DIR --port PORT --source-config SOURCE_CONFIG --target-release-bundle TARGET_RELEASE_BUNDLE --release-digest RELEASE_DIGEST --client-build CLIENT_BUILD --client-executable CLIENT_EXECUTABLE --client-registry CLIENT_REGISTRY [OPTIONS]
```

[Exact help](help/proxy-candidate-prepare.txt) · Family: candidate · Kind: command

Bind the input bytes, client identity, release digest and isolated port to a new candidate state directory. Preparation is local bookkeeping, not release validation.
Credential and policy attestation files record operator-supplied bytes; they do not import credentials, configure a running proxy or prove read-only access.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `prepare`: Create and bind inputs without starting a runtime.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

New isolated candidate state directory. Required; absolute clean path below a current-user-owned parent. Existing candidate state is a conflict; never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; parent owner-controlled; no existing candidate state.

#### `--port PORT`

Loopback listener port reserved for this candidate. Required; 1..65535, excluding shared default port 19280.

Type: integer. Required: true. Repeatable: false.

Default: null.

Constraint: Integer 1..65535; must not equal 19280; must not equal any active shared or candidate listener.

#### `--source-config SOURCE_CONFIG`

Source configuration attestation file; required, at most 1048576 bytes. Its byte hash is recorded; contents are not applied.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..1048576 bytes inclusive; no group or other write permission.

#### `--target-release-bundle TARGET_RELEASE_BUNDLE`

Target signed OperationalReleaseBundleV1 file; required, purpose target, at most 16777216 bytes.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16777216 bytes inclusive; no group or other write permission.

#### `--release-digest RELEASE_DIGEST`

Target bundle digest field, not SHA-256 of the bundle file. Required; must equal the verified target bundle digest.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--client-build CLIENT_BUILD`

Exact client version/build string supplied by executable provenance. Required; must match the independently measured client evidence byte-for-byte. An arbitrary operator label cannot satisfy validation.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty UTF-8 string; no NUL or control characters.

Constraint: Must match the client executable provenance and independently measured evidence byte-for-byte; no case folding, trimming, aliasing or inferred version.

Constraint: During release validation must also match the string bound during prepare.

#### `--client-executable CLIENT_EXECUTABLE`

Exact client executable whose bytes will be bound to validation; required, absolute regular file, at most 536870912 bytes.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions.

Constraint: File and trusted parent may be owned by current user or root; system-owned executables are allowed.

Constraint: Size 1..536870912 bytes inclusive.

#### `--client-registry CLIENT_REGISTRY`

ClientSenderRegistryV1 canonical JSON file; required, at most 65536 bytes. Enumerates all request senders and transports participating in validation.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..65536 bytes inclusive; no group or other write permission.

#### `--credential-mode CREDENTIAL_MODE`

Credential attestation mode. Default: none. read-only requires --credential-manifest and --confirm-read-only-credentials; none forbids both.

Type: enum. Required: false. Repeatable: false.

Default: "none".

Choices: none, read-only.

#### `--credential-manifest CREDENTIAL_MANIFEST`

Credential manifest attestation bytes; at most 1048576 bytes. Required only with --credential-mode read-only; never imports or refreshes credentials.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..1048576 bytes inclusive; no group or other write permission.

#### `--confirm-read-only-credentials`

Assert that the attested credentials are restricted to read-only access. Required with read-only mode; forbidden with none. This assertion does not authorise credential refresh.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--policy-snapshot POLICY_SNAPSHOT`

Optional policy attestation bytes, at most 1048576 bytes. Omission records no policy digest; contents are not applied.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..1048576 bytes inclusive; no group or other write permission.

#### `--confirm-payload-capture`

Explicitly authorise capture of request payloads during this candidate workflow. Default: false; omission prohibits capture. Preparation itself captures nothing.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Explicit false equals omission.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 150s. Allowed: 150s..5m inclusive; 30s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "150s".

Constraint: Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- State directory does not contain a prepared candidate; parent is owner-controlled.
- All structured inputs validate before any state is created.
- Manifest/mode/confirmation combinations obey option rules.

### Effects and completion

- Create a new 0700 candidate directory and 0600 state, key and registry files atomically.
- Generate operation_id (32 lowercase hex), instance_id (32 lowercase hex), and validation_run_id (64 lowercase hex); record input SHA-256 digests and phase prepared.
- Do not start a listener, modify the installed service, import credentials, or apply attested configuration.
- Existing candidate state is a conflict; prepare never overwrites or silently reuses it.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `candidate`: CandidateStatusV2; exact fields in candidate annex notes.

Human output template:

```text
Candidate: {candidate.instance_id}
State directory: {candidate.state_dir}
Phase: {candidate.phase}
Port: {candidate.port}
Validation run: {candidate.validation_run_id}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_not_found | 3 | Candidate state directory or requested receipt does not exist. | Candidate state or receipt does not exist. |
| candidate_conflict | 6 | State identity, generation, phase, validation run or file ownership differs from the required state. | Candidate state conflicts with this operation. |
| candidate_io_failed | 1 | A validated local read, write, process operation or receipt publication fails. | Candidate operation failed: {safe_reason}. |
| candidate_timeout | 7 | Total deadline expires; recorded pending state remains inspectable. | Candidate operation timed out; inspect candidate status before retrying. |

### Examples

```sh
cq proxy candidate prepare --state-dir "$CANDIDATE" --port 29280 --source-config "$INPUTS/source-config.bin" --target-release-bundle "$INPUTS/target.json" --release-digest "$RELEASE_DIGEST" --client-build "$CLIENT_BUILD" --client-executable "$CLIENT" --client-registry "$INPUTS/client-registry.json"
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate receipt`

Inspect a retained candidate attempt receipt.

```text
cq proxy candidate receipt <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-candidate-receipt.txt) · Family: navigation · Kind: group

Inspect a retained candidate attempt receipt.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `receipt`: A retained machine-verifiable record of a validation attempt.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-candidate-receipt.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy candidate receipt --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy candidate receipt show`

Show the authenticated receipt for one candidate validation attempt.

```text
cq proxy candidate receipt show --state-dir STATE_DIR --attempt-id ATTEMPT_ID [OPTIONS]
```

[Exact help](help/proxy-candidate-receipt-show.txt) · Family: candidate · Kind: command

Look up an exact retained attempt identifier. A receipt reports its recorded result; lookup performs no live validation.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `receipt`: A retained machine-verifiable record of a validation attempt.
- `show`: Read one exact record without changing it.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--attempt-id ATTEMPT_ID`

Exact attempt identifier returned by candidate validation; required, 32 lowercase hexadecimal characters. No latest-attempt fallback.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 32 lowercase hexadecimal characters.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 10s. Allowed: 1s..30s inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Requested receipt and its key exist under the candidate state directory.

### Effects and completion

- Read and authenticate retained receipt; repeated lookup is idempotent.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `receipt`: CandidateReceiptV2; exact fields in annex notes.

Human output template:

```text
Attempt: {receipt.attempt_id}
Outcome: {receipt.outcome}
Receipt digest: {receipt.receipt_digest}
Promotion digest: {receipt.promotion_digest}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_not_found | 3 | Candidate state directory or requested receipt does not exist. | Candidate state or receipt does not exist. |
| candidate_conflict | 6 | State identity, generation, phase, validation run or file ownership differs from the required state. | Candidate state conflicts with this operation. |
| candidate_io_failed | 1 | A validated local read, write, process operation or receipt publication fails. | Candidate operation failed: {safe_reason}. |
| candidate_timeout | 7 | Total deadline expires; recorded pending state remains inspectable. | Candidate operation timed out; inspect candidate status before retrying. |
| candidate_receipt_conflicted | 6 | The authenticated retained receipt records outcome conflicted. | Candidate receipt is conflicted. |

### Examples

```sh
cq proxy candidate receipt show --state-dir "$CANDIDATE" --attempt-id "$ATTEMPT_ID"
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate release`

Reserve release activation and qualification; currently unavailable.

```text
cq proxy candidate release <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-candidate-release.txt) · Family: navigation · Kind: group

Reserve release activation and qualification; currently unavailable.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `release`: A signed set of runtime artifacts.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-candidate-release.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy candidate release --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy candidate release activate`

Activate the prepared runtime release inside a candidate.

```text
cq proxy candidate release activate --state-dir STATE_DIR --release-digest RELEASE_DIGEST --validation-run-id VALIDATION_RUN_ID --confirm-artifact-switch [OPTIONS]
```

[Exact help](help/proxy-candidate-release-activate.txt) · Family: candidate · Kind: command

**Availability:** unavailable. Well-formed execution returns exit 4, `data=null`, and no human stdout. Reserved success semantics in the JSON catalogue are design notes, not enabled behaviour.

Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
Stop and replace the candidate runtime with the exact signed target artifact set. This does not promote or install the release into the shared service.
The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `release`: A signed set of runtime artifacts.
- `activate`: Make the selected release run in this candidate.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--release-digest RELEASE_DIGEST`

Verified target release digest returned by prepare; required and must match the prepared target.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--validation-run-id VALIDATION_RUN_ID`

Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--confirm-artifact-switch`

Authorise replacement of this candidate runtime with the selected release. Required; must be true.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 90s. Allowed: 90s..2m inclusive; 30s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "90s".

Constraint: Go duration syntax; 90s <= value <= 2m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

### Effects and completion

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_evidence_unavailable | 4 | This reserved capability has no complete implemented proof-backend contract. | Candidate qualification evidence is unavailable; no transition was performed. |

### Examples

```sh
cq proxy candidate release activate --state-dir "$CANDIDATE" --release-digest "$RELEASE_DIGEST" --validation-run-id "$VALIDATION_RUN" --confirm-artifact-switch
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

- `cq proxy candidate artifact switch` → `proxy candidate release activate; option names translate according to CandidateLegacyOptionsV1; a legacy final duration translates to --timeout.`. Deprecated spelling; retain semantic confirmations and reject unknown or duplicate arguments.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate release validate`

Verify candidate release evidence and write a validation receipt.

```text
cq proxy candidate release validate --state-dir STATE_DIR --target-release-bundle TARGET_RELEASE_BUNDLE --rollback-bundle ROLLBACK_BUNDLE --rollback-receipt ROLLBACK_RECEIPT --rollback-receipt-digest ROLLBACK_RECEIPT_DIGEST --client-build CLIENT_BUILD --client-executable CLIENT_EXECUTABLE --validation-run-id VALIDATION_RUN_ID --receipt-file RECEIPT_FILE --confirm-control-health [OPTIONS]
```

[Exact help](help/proxy-candidate-release-validate.txt) · Family: candidate · Kind: command

**Availability:** unavailable. Well-formed execution returns exit 4, `data=null`, and no human stdout. Reserved success semantics in the JSON catalogue are design notes, not enabled behaviour.

Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
Check exact target artifacts, rollback acceptance, source ancestry, client identity, client isolation, stopped-work proof, control health and runtime confinement.
Success permits the receipt to be reviewed for promotion; it does not install or deploy the release. File hashes and self-declared ancestry cannot substitute for the corresponding observations.
The deadline is fixed at 16 minutes, including a 30-second cleanup reserve; there is no timeout option.
The required live-proof and trust-authority backends are not specified by this CLI contract. Until their independent contracts and implementations exist, this command returns candidate_evidence_unavailable (exit 4) and emits no successful qualification or promotion receipt.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `release`: A signed set of runtime artifacts.
- `validate`: Check release evidence and issue a validation receipt.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--target-release-bundle TARGET_RELEASE_BUNDLE`

Exact target OperationalReleaseBundleV1 used by prepare, at most 16777216 bytes.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16777216 bytes inclusive; no group or other write permission.

#### `--rollback-bundle ROLLBACK_BUNDLE`

Accepted rollback OperationalReleaseBundleV1, purpose floor, at most 16777216 bytes. Its source commit must be a strict ancestor of target.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..16777216 bytes inclusive; no group or other write permission.

#### `--rollback-receipt ROLLBACK_RECEIPT`

Original rollback acceptance receipt bytes, at most 65536 bytes; verified by the acceptance-proof backend, never fabricated.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

Constraint: Regular file; size 1..65536 bytes inclusive; no group or other write permission.

#### `--rollback-receipt-digest ROLLBACK_RECEIPT_DIGEST`

SHA-256 of UTF-8 bytes cq/release-import-floor/v1 followed by NUL followed by exact rollback receipt bytes. Required; not an ordinary file SHA-256.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--client-build CLIENT_BUILD`

Exact client version/build string supplied by executable provenance. Required; must match the independently measured client evidence byte-for-byte. An arbitrary operator label cannot satisfy validation.

Type: string. Required: true. Repeatable: false.

Default: null.

Constraint: Non-empty UTF-8 string; no NUL or control characters.

Constraint: Must match the client executable provenance and independently measured evidence byte-for-byte; no case folding, trimming, aliasing or inferred version.

Constraint: During release validation must also match the string bound during prepare.

#### `--client-executable CLIENT_EXECUTABLE`

Exact client executable whose bytes will be bound to validation; required, absolute regular file, at most 536870912 bytes.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions.

Constraint: File and trusted parent may be owned by current user or root; system-owned executables are allowed.

Constraint: Size 1..536870912 bytes inclusive.

#### `--validation-run-id VALIDATION_RUN_ID`

Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.

Type: digest. Required: true. Repeatable: false.

Default: null.

Constraint: Exactly 64 lowercase hexadecimal characters.

#### `--receipt-file RECEIPT_FILE`

New absolute output file for CandidateReleasePromotionReceiptV1. Must not exist; parent must be owner-controlled.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean output path; not /; leaf must not exist, including as a symbolic link.

Constraint: Parent must already exist as a current-user-owned, owner-controlled directory; validate and hold its identity through atomic creation.

Constraint: Create a new regular file with mode 0600; never overwrite or follow an existing leaf.

#### `--confirm-control-health`

Assert the exact candidate passed control-health verification. Required; CQ also verifies live identity and health itself.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

### Preconditions

- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

### Effects and completion

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_evidence_unavailable | 4 | This reserved capability has no complete implemented proof-backend contract. | Candidate qualification evidence is unavailable; no transition was performed. |

### Examples

```sh
cq proxy candidate release validate --state-dir "$CANDIDATE" --target-release-bundle "$INPUTS/target.json" --rollback-bundle "$INPUTS/floor.json" --rollback-receipt "$INPUTS/floor-receipt.bin" --rollback-receipt-digest "$ROLLBACK_DIGEST" --client-build "$CLIENT_BUILD" --client-executable "$CLIENT" --validation-run-id "$VALIDATION_RUN" --receipt-file "$INPUTS/promotion.json" --confirm-control-health
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

- `cq proxy candidate validate-release` → `proxy candidate release validate; option names translate according to CandidateLegacyOptionsV1; a legacy final duration translates to --timeout.`. Deprecated spelling; retain semantic confirmations and reject unknown or duplicate arguments.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate remove`

Delete stopped candidate state and retained receipts.

```text
cq proxy candidate remove --state-dir STATE_DIR --confirm-candidate-state-loss [OPTIONS]
```

[Exact help](help/proxy-candidate-remove.txt) · Family: candidate · Kind: command

Permanently remove only this owned candidate state directory. Export any needed receipts first. Client and runtime absence must be verified independently of lifecycle phase.
The deadline is fixed at 30 seconds, including a 15-second cleanup reserve; there is no timeout option.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `remove`: Delete owned candidate state after proving its runtime is absent.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--confirm-candidate-state-loss`

Authorise permanent deletion of this candidate directory, keys and retained receipts. Required; must be true.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

### Preconditions

- Phase prepared, stopped or validated; no process or listener remains.
- Candidate-state-loss confirmation true; directory identity still matches.

### Effects and completion

- Prove associated process/listener absent, then delete only identity-matched owned files and directory.
- Return final CandidateStatusV2 with phase removed before discarding state.
- Missing directory returns not found; never treats an arbitrary directory as removable candidate state.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `candidate`: CandidateStatusV2; exact fields in candidate annex notes.

Human output template:

```text
Candidate: {candidate.instance_id}
State directory: {candidate.state_dir}
Phase: {candidate.phase}
Port: {candidate.port}
Validation run: {candidate.validation_run_id}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_not_found | 3 | Candidate state directory or requested receipt does not exist. | Candidate state or receipt does not exist. |
| candidate_conflict | 6 | State identity, generation, phase, validation run or file ownership differs from the required state. | Candidate state conflicts with this operation. |
| candidate_io_failed | 1 | A validated local read, write, process operation or receipt publication fails. | Candidate operation failed: {safe_reason}. |
| candidate_timeout | 7 | Total deadline expires; recorded pending state remains inspectable. | Candidate operation timed out; inspect candidate status before retrying. |

### Examples

```sh
cq proxy candidate remove --state-dir "$CANDIDATE" --confirm-candidate-state-loss
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate start`

Start the exact release bound to an isolated candidate.

```text
cq proxy candidate start --state-dir STATE_DIR [OPTIONS]
```

[Exact help](help/proxy-candidate-start.txt) · Family: candidate · Kind: command

**Availability:** unavailable. Well-formed execution returns exit 4, `data=null`, and no human stdout. Reserved success semantics in the JSON catalogue are design notes, not enabled behaviour.

Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
Launch only the prepared release and verify its identity before returning success. A substitute health-only process is not a valid candidate runtime.
The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `start`: Start the exact prepared runtime.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 30s. Allowed: 30s..90s inclusive; 15s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration syntax; 30s <= value <= 90s; cleanup reserve 15s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

### Effects and completion

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_evidence_unavailable | 4 | This reserved capability has no complete implemented proof-backend contract. | Candidate qualification evidence is unavailable; no transition was performed. |

### Examples

```sh
cq proxy candidate start --state-dir "$CANDIDATE"
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate status`

Show retained state for one isolated candidate.

```text
cq proxy candidate status --state-dir STATE_DIR [OPTIONS]
```

[Exact help](help/proxy-candidate-status.txt) · Family: candidate · Kind: command

Read lifecycle phase, immutable input bindings and pending action. A running phase is retained state, not a fresh traffic or health test.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `status`: Inspect retained lifecycle state; does not assert traffic readiness.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--timeout TIMEOUT`

Total deadline, including cleanup. Default: 10s. Allowed: 1s..30s inclusive; 0s reserved for cleanup.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

### Preconditions

- Candidate state exists and authenticates under its owned key.

### Effects and completion

- Read state without starting, repairing or validating a runtime.
- Repeated invocation is idempotent.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `candidate`: CandidateStatusV2; exact fields in candidate annex notes.

Human output template:

```text
Candidate: {candidate.instance_id}
State directory: {candidate.state_dir}
Phase: {candidate.phase}
Port: {candidate.port}
Validation run: {candidate.validation_run_id}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_not_found | 3 | Candidate state directory or requested receipt does not exist. | Candidate state or receipt does not exist. |
| candidate_conflict | 6 | State identity, generation, phase, validation run or file ownership differs from the required state. | Candidate state conflicts with this operation. |
| candidate_io_failed | 1 | A validated local read, write, process operation or receipt publication fails. | Candidate operation failed: {safe_reason}. |
| candidate_timeout | 7 | Total deadline expires; recorded pending state remains inspectable. | Candidate operation timed out; inspect candidate status before retrying. |

### Examples

```sh
cq proxy candidate status --state-dir "$CANDIDATE"
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy candidate stop`

Stop an isolated candidate after its client has stopped.

```text
cq proxy candidate stop --state-dir STATE_DIR --confirm-client-stopped [OPTIONS]
```

[Exact help](help/proxy-candidate-stop.txt) · Family: candidate · Kind: command

Confirm every client using this candidate has stopped before stopping its runtime. Confirmation is mandatory in both human and JSON modes.
A validated candidate can still be running; stop accepts that phase and verifies process absence.
The deadline is fixed at 30 seconds, including a 15-second cleanup reserve; there is no timeout option.

### Naming rationale

- `proxy`: Proxy runtime ownership; these isolated release candidates may cover more than one provider.
- `candidate`: An isolated runtime and state directory used to evaluate a release before changing the installed service.
- `stop`: Stop the runtime after the client is stopped.

### Arguments

None.

### Options

#### `--state-dir STATE_DIR`

Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.

#### `--confirm-client-stopped`

Assert that every client using this candidate has stopped and no request remains active. Required; must be true.

Type: boolean. Required: true. Repeatable: false.

Default: false.

Constraint: Must be true.

### Preconditions

- Phase running or validated; an exact candidate runtime remains.
- Client stopped confirmation true; runtime identity matches stored candidate.

### Effects and completion

- Stop only the identity-matched runtime; prove process/listener absence before phase stopped.
- Already stopped is a phase conflict; no implicit retries or second stop.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `candidate`: CandidateStatusV2; exact fields in candidate annex notes.

Human output template:

```text
Candidate: {candidate.instance_id}
State directory: {candidate.state_dir}
Phase: {candidate.phase}
Port: {candidate.port}
Validation run: {candidate.validation_run_id}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| candidate_not_found | 3 | Candidate state directory or requested receipt does not exist. | Candidate state or receipt does not exist. |
| candidate_conflict | 6 | State identity, generation, phase, validation run or file ownership differs from the required state. | Candidate state conflicts with this operation. |
| candidate_io_failed | 1 | A validated local read, write, process operation or receipt publication fails. | Candidate operation failed: {safe_reason}. |
| candidate_timeout | 7 | Total deadline expires; recorded pending state remains inspectable. | Candidate operation timed out; inspect candidate status before retrying. |

### Examples

```sh
cq proxy candidate stop --state-dir "$CANDIDATE" --confirm-client-stopped
```

Uses inputs prepared in the candidate workflow in the versioned annex notes.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_commands.go:315`
- `cmd/cq/proxy_candidate.go:85`

## `cq proxy health`

Probe proxy HTTP liveness.

```text
cq proxy health [OPTIONS]
```

[Exact help](help/proxy-health.txt) · Family: root · Kind: command

Request /health from one loopback listener. A successful probe establishes HTTP liveness only; it does not prove routing, account continuity or installed-client validation.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `health`: Perform only the bounded HTTP liveness probe, not full readiness validation.

### Arguments

None.

### Options

#### `--port PORT`

Loopback port, 1–65535. Omit to use configured port or 19280 without creating config.

Type: integer. Required: false. Repeatable: false.

Default: null.

Constraint: Integer 1–65535.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 5s.

Type: duration. Required: false. Repeatable: false.

Default: "5s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Read configuration only when --port omitted.

### Effects and completion

- GET /health, no redirects, maximum response 1 MiB; never expose response tokens or raw unexpected content.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `reachable`: boolean
- `healthy`: boolean:2xx response and JSON status field equal ok
- `http_status`: integer100..599|null
- `address`: loopback host:port
- `duration_ms`: integer>=0

Human output template:

```text
Proxy HTTP health: {healthy ? "ok" : "failed"}
Address: {address}
HTTP status: {http_status or "unavailable"}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| proxy_health_failed | 1 | Response received but status not healthy. | Proxy HTTP health check failed. |
| proxy_unreachable | 4 | Listener not reachable. | Proxy is not reachable at {address}. |
| proxy_health_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq proxy health --port 29280
```

Probe a specific loopback listener.

### Compatibility spellings

- `cq proxy status --port PORT` → `proxy health --port PORT`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `cmd/cq/proxy.go:status`
- `cmd/cq/help.go:145`

## `cq proxy operation`

Inspect retained shared proxy lifecycle operation state.

```text
cq proxy operation <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-operation.txt) · Family: navigation · Kind: group

Inspect retained shared proxy lifecycle operation state.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `operation`: A durable shared-runtime operation record.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-operation.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy operation --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy operation status`

Show a retained shared proxy operation record.

```text
cq proxy operation status [OPERATION_ID] [OPTIONS]
```

[Exact help](help/proxy-operation-status.txt) · Family: validation · Kind: command

An operation is a durable coordinator record for a shared runtime transition. Without an ID, inspect the currently selected coordinator record; no record returns state=idle.
This command never performs recovery. Result availability means a retained result exists, not that its underlying action succeeded.

### Naming rationale

- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `operation`: A durable shared-runtime operation record.
- `status`: Inspects current state without requesting a transition.

### Arguments

#### `OPERATION_ID`

Operation ID to inspect. Default: the currently selected coordinator record.

Type: string. Required: false. Repeatable: false.

Default: null.

Constraint: If supplied, exactly 32 lowercase hexadecimal characters.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Read-only existing resilience state; does not create directories, restart processes or resume operations.
- Pending operations are valid status results and exit 0; absent explicitly requested IDs exit 3.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `operation_id`: 32 lowercase hex string or null when idle
- `state`: enum idle|pending|result_available
- `phase`: enum intent|anchor|receipt|terminal or null when idle
- `result_available`: boolean
- `result_digest`: SHA256 or null when no retained value
- `recovery_supported`: literal false

Human output template:

```text
Operation: {operation_id_or_idle}
State: {state}
Phase: {phase_or_dash}
Result available: {result_available}
Recovery supported: false

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| operation_not_found | 3 | Explicit ID missing | The requested operation does not exist. |
| operation_state_unavailable | 4 | State corrupt or unreadable | The operation state is unavailable. |

### Examples

```sh
cq proxy operation status
```

Inspect the currently selected operation.

```sh
cq proxy operation status 0123456789abcdef0123456789abcdef
```

Inspect a specific retained operation.

### Compatibility spellings

- `cq operation status` → `proxy operation status; translate --operation-id ID to positional ID`. Deprecated spelling; preserve semantics and emit deprecation warning on stderr.
- `cq operation recover` → `No execution translation; validate required --operation-id ID, then exit 4 with operation_recovery_unsupported.`. Retired misleading command. Exact message: Operation recovery is not supported. Inspect the record with cq proxy operation status ID. Never return success merely because a receipt exists.

### Source evidence

- `cmd/cq/proxy_operation.go:54`
- `cmd/cq/proxy_commands.go:223`

## `cq proxy rescue`

Inspect or transition the shared runtime rescue-serving mode.

```text
cq proxy rescue <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-rescue.txt) · Family: navigation · Kind: group

Inspect or transition the shared runtime rescue-serving mode.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `rescue`: Shared supervisor fallback traffic mode used while the normal worker is unavailable.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-rescue.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy rescue --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy rescue enter`

Request shared proxy rescue traffic mode.

```text
cq proxy rescue enter [OPTIONS]
```

[Exact help](help/proxy-rescue-enter.txt) · Family: validation · Kind: command

Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.

### Naming rationale

- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `rescue`: Shared supervisor fallback traffic mode used while the normal worker is unavailable.
- `enter`: Requests transition into rescue traffic mode.

### Arguments

None.

### Options

#### `--port PORT`

Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.

Type: integer. Required: false. Repeatable: false.

Default: null.

Constraint: Integer 1..65535; omission resolves existing bootstrap configuration without creating it.

#### `--timeout TIMEOUT`

Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration, 1s <= value <= 5m.

Constraint: Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

### Preconditions

- Existing rescue bootstrap control credentials and reachable supervisor.
- For exit, admitted normal worker available; for enter, rescue handler and durable mode state available.

### Effects and completion

- Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- Repeated enter is idempotent when the same transition is already requested.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `mode`: enum normal|drain|rescue_draining|rescue|rescue_exit_draining
- `generation`: uint64, durable transition generation
- `active_rescue_requests`: nonnegative integer, admitted rescue requests not yet complete
- `draining_sessions`: array of opaque privacy-safe session hints; sorted; [] when none

Human output template:

```text
Proxy mode: {mode}
Generation: {generation}
Active rescue requests: {active_rescue_requests}
Draining sessions: {draining_sessions_count}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| rescue_unavailable | 4 | Supervisor or configured control unavailable | Proxy rescue control is unavailable. |
| rescue_auth_failed | 5 | HTTP401/403 | Proxy rescue control was not authorised. |
| rescue_transition_conflict | 6 | Supervisor refuses transition | The proxy cannot perform this rescue transition. |
| rescue_timeout | 7 | Elapsed operation budget exceeded | The proxy rescue operation timed out; inspect status before retrying. |
| rescue_response_invalid | 1 | Malformed or >64KiB response | Proxy rescue control returned an invalid response. |

### Examples

```sh
cq proxy rescue enter
```

Ask the configured shared proxy to enter rescue mode.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_rescue.go:42`
- `internal/proxy/runtime_supervisor.go:601`

## `cq proxy rescue exit`

Request return to normal proxy traffic.

```text
cq proxy rescue exit [OPTIONS]
```

[Exact help](help/proxy-rescue-exit.txt) · Family: validation · Kind: command

Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.

### Naming rationale

- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `rescue`: Shared supervisor fallback traffic mode used while the normal worker is unavailable.
- `exit`: Requests return from rescue traffic mode to normal traffic.

### Arguments

None.

### Options

#### `--port PORT`

Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.

Type: integer. Required: false. Repeatable: false.

Default: null.

Constraint: Integer 1..65535; omission resolves existing bootstrap configuration without creating it.

#### `--timeout TIMEOUT`

Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration, 1s <= value <= 5m.

Constraint: Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

### Preconditions

- Existing rescue bootstrap control credentials and reachable supervisor.
- For exit, admitted normal worker available; for enter, rescue handler and durable mode state available.

### Effects and completion

- Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- Repeated exit is idempotent when the same transition is already requested.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `mode`: enum normal|drain|rescue_draining|rescue|rescue_exit_draining
- `generation`: uint64, durable transition generation
- `active_rescue_requests`: nonnegative integer, admitted rescue requests not yet complete
- `draining_sessions`: array of opaque privacy-safe session hints; sorted; [] when none

Human output template:

```text
Proxy mode: {mode}
Generation: {generation}
Active rescue requests: {active_rescue_requests}
Draining sessions: {draining_sessions_count}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| rescue_unavailable | 4 | Supervisor or configured control unavailable | Proxy rescue control is unavailable. |
| rescue_auth_failed | 5 | HTTP401/403 | Proxy rescue control was not authorised. |
| rescue_transition_conflict | 6 | Supervisor refuses transition | The proxy cannot perform this rescue transition. |
| rescue_timeout | 7 | Elapsed operation budget exceeded | The proxy rescue operation timed out; inspect status before retrying. |
| rescue_response_invalid | 1 | Malformed or >64KiB response | Proxy rescue control returned an invalid response. |

### Examples

```sh
cq proxy rescue exit
```

Ask the configured proxy to resume normal traffic.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_rescue.go:42`
- `internal/proxy/runtime_supervisor.go:601`

## `cq proxy rescue status`

Show shared proxy rescue and drain state.

```text
cq proxy rescue status [OPTIONS]
```

[Exact help](help/proxy-rescue-status.txt) · Family: validation · Kind: command

Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.

### Naming rationale

- `proxy`: Scopes this capability to proxy routing or the shared proxy runtime.
- `rescue`: Shared supervisor fallback traffic mode used while the normal worker is unavailable.
- `status`: Inspects current state without requesting a transition.

### Arguments

None.

### Options

#### `--port PORT`

Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.

Type: integer. Required: false. Repeatable: false.

Default: null.

Constraint: Integer 1..65535; omission resolves existing bootstrap configuration without creating it.

#### `--timeout TIMEOUT`

Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Go duration, 1s <= value <= 5m.

Constraint: Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

### Preconditions

- Existing rescue bootstrap control credentials and reachable supervisor.

### Effects and completion

- Authenticated loopback read only; no configuration or mode mutation.
- Repeated inspection does not change state.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `mode`: enum normal|drain|rescue_draining|rescue|rescue_exit_draining
- `generation`: uint64, durable transition generation
- `active_rescue_requests`: nonnegative integer, admitted rescue requests not yet complete
- `draining_sessions`: array of opaque privacy-safe session hints; sorted; [] when none

Human output template:

```text
Proxy mode: {mode}
Generation: {generation}
Active rescue requests: {active_rescue_requests}
Draining sessions: {draining_sessions_count}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| rescue_unavailable | 4 | Supervisor or configured control unavailable | Proxy rescue control is unavailable. |
| rescue_auth_failed | 5 | HTTP401/403 | Proxy rescue control was not authorised. |
| rescue_transition_conflict | 6 | Supervisor refuses transition | The proxy cannot perform this rescue transition. |
| rescue_timeout | 7 | Elapsed operation budget exceeded | The proxy rescue operation timed out; inspect status before retrying. |
| rescue_response_invalid | 1 | Malformed or >64KiB response | Proxy rescue control returned an invalid response. |

### Examples

```sh
cq proxy rescue status
```

Inspect the current transition without changing it.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `cmd/cq/proxy_rescue.go:42`
- `internal/proxy/runtime_supervisor.go:601`

## `cq proxy serve`

Run the shared API proxy in the foreground.

```text
cq proxy serve [OPTIONS]
```

[Exact help](help/proxy-serve.txt) · Family: root · Kind: command

Serve Claude and Codex requests on the configured loopback port until interrupted. This starts no operating-system service.
A live port override applies to this process only. Existing clients are unaffected until they connect to this listener.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `serve`: Run the listener in this terminal until stopped; does not install a service.

### Arguments

None.

### Options

#### `--port PORT`

Loopback listen port, 1–65535. Omit to use proxy.json port, or 19280 if absent.

Type: integer. Required: false. Repeatable: false.

Default: null.

Constraint: No ephemeral port 0; reject a listener owned by another process.

#### `--migrate-legacy-managed`

Add missing routing identity metadata to legacy CQ-managed credentials before serving. Default: false.

Type: boolean. Required: false. Repeatable: false.

Default: false.

Constraint: Never changes external or system-owned credentials.

### Preconditions

- Validate config, credential ownership and port before binding. Configuration absence permits explicit creation because serve is a mutation command.
- Do not replace, signal or steal another listener.

### Effects and completion

- Create missing CQ proxy config with a new local token using owner-only atomic writes; never print token.
- Bind loopback only, run until SIGINT/SIGTERM, drain owned requests and close owned listener.
- No service registration or automatic daemonisation. Migration is explicit, idempotent and restricted to CQ-managed records.
- Streaming command: writes ready event only after bind and runtime initialisation; ready means accepting requests, not installed-client qualification.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `event`: enum ready|stopped
- `listen_address`: string loopback host:port
- `pid`: integer process id >0
- `providers`: array enum claude|codex, enabled listeners
- `reason`: string|null: interrupted or shutdown cause on stopped event

Human output template:

```text
Proxy listening on {listen_address} (PID {pid}).
Press Ctrl-C to stop.

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| proxy_port_in_use | 6 | Requested address already has a listener. | Port {port} is already in use. |
| proxy_config_invalid | 6 | Existing configuration cannot safely start. | Proxy configuration is invalid: {reason}. |

### Examples

```sh
cq proxy serve
```

Run on configured port.

```sh
cq proxy serve --port 29280
```

Run an isolated listener on port 29280; choose config deliberately before supplying real traffic.

### Compatibility spellings

- `cq proxy start` → `proxy serve`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `cmd/cq/proxy.go:558`

## `cq proxy state`

Initialise an explicitly selected shared proxy authority directory.

```text
cq proxy state <COMMAND> [OPTIONS]
```

[Exact help](help/proxy-state.txt) · Family: navigation · Kind: group

Initialise an explicitly selected shared proxy authority directory.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `state`: The shared authenticated runtime-control state directory.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/proxy-state.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq proxy state --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq proxy state initialise`

Initialise shared authenticated proxy state.

```text
cq proxy state initialise --state-dir DIR [OPTIONS]
```

[Exact help](help/proxy-state-initialise.txt) · Family: root · Kind: command

Create the shared runtime-control state directory and record its location in proxy configuration. This is shared infrastructure, not a Codex account pool.
This command does not create pools or bind sessions. Use codex proxy policy, pool and session commands for Codex routing.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `state`: The shared authenticated runtime-control state directory.
- `initialise`: Create missing state explicitly and bind it to proxy configuration.

### Arguments

None.

### Options

#### `--state-dir DIR`

Absolute directory to own as shared proxy runtime-control state.

Type: path. Required: true. Repeatable: false.

Default: null.

Constraint: Required clean absolute non-root path; reject symlink path components and existing foreign/incompatible state.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 30s.

Type: duration. Required: false. Repeatable: false.

Default: "30s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Existing service/config ownership must belong to current user.
- Same already-configured authenticated directory succeeds idempotently. Different configured nonempty root is conflict; this command does not migrate it.

### Effects and completion

- Create state owner-only, publish authenticated initial records atomically, set proxy_resilience_state_dir in config.
- Do not restart service automatically. Output restart_required=true iff running service has not adopted state.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state_dir`: absolute path
- `created`: boolean
- `restart_required`: boolean

Human output template:

```text
Proxy state: {state_dir}
Created: {created}
Restart required: {restart_required}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| proxy_state_conflict | 6 | Directory owned by another instance or config refers to another initialised root. | Proxy state ownership conflicts with the requested directory. |
| proxy_state_initialise_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |
| proxy_state_initialise_partial | 8 | A positive authority-creation or configuration-binding mutation receipt is followed by a later failure, without deadline expiry or interruption. Retain the known outcome. | Proxy state initialisation partially completed; inspect state before retrying. |

### Examples

```sh
cq proxy state initialise --state-dir "$HOME/.config/cq/runtime"
```

Create a dedicated shared runtime state root.

### Compatibility spellings

- `cq proxy policy initialise --state-root DIR` → `proxy state initialise --state-dir DIR`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `cmd/cq/proxy_policy.go:90`

## `cq proxy status`

Inspect shared proxy readiness evidence.

```text
cq proxy status [OPTIONS]
```

[Exact help](help/proxy-status.txt) · Family: root · Kind: command

Inspect configured service, process, listener, runtime and data-plane evidence without mutating state. This does not send model requests or create new validation evidence. Inspect provider routing policy and retained transport readiness through their dedicated commands.
Human and JSON output describe exactly the same inspection. Use proxy health for the older, narrower HTTP liveness probe.

### Naming rationale

- `proxy`: The shared local API listener and runtime serving supported providers.
- `status`: Inspect current state without changing it or initiating acceptance validation.

### Arguments

None.

### Options

#### `--state-dir DIR`

Inspect an isolated candidate state directory. Omit to inspect configured live proxy state.

Type: path. Required: false. Repeatable: false.

Default: null.

Constraint: When supplied: existing clean absolute non-root directory; no symlink ancestors; never create it.

#### `--strict`

Return non-zero when inspected proxy is unhealthy, absent or indeterminate. Default: false.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- No configuration or state creation.

### Effects and completion

- Collect inspection facts from explicit candidate root or default live configuration, service manager and loopback runtime.
- Default exit0 means inspection completed; inspect data.state for readiness. --strict changes exit policy only, never inspection scope.
- No --port accepted: explicit root/config identifies coherent process evidence. Use proxy health --port for arbitrary liveness.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `state`: enum ready|degraded|stopped|absent|indeterminate
- `scope`: enum live|candidate
- `state_dir`: absolute path|null
- `collected_at`: UTC RFC3339 timestamp
- `duration_ms`: integer >=0
- `facts`: array<ProxyFactV2> as root notes

Human output template:

```text
Proxy: {state}
Scope: {scope}
{each facts in defined order: name + ": " + state + " — " + detail + "\n"}
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| proxy_unhealthy | 1 | --strict and state degraded. | Proxy readiness is degraded. |
| proxy_absent | 3 | --strict and state absent or stopped. | No ready proxy is running. |
| proxy_indeterminate | 4 | --strict and state indeterminate. | Proxy readiness could not be determined. |
| proxy_status_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq proxy status
```

Inspect live proxy evidence.

```sh
cq proxy status --strict --json
```

Use readiness result as an automation gate without changing output scope.

### Compatibility spellings

- `cq proxy status --human|--json|--strict|--timeout DURATION|--instance-state-root DIR` → `proxy status [--json] [--strict] [--timeout DURATION] [--state-dir DIR]`. --human translates to default renderer; --instance-state-root to --state-dir; old positional timeout -> --timeout. Bare old status meaning deliberately changes in CLI v2; use proxy health.

### Source evidence

- `cmd/cq/help.go:145`
- `cmd/cq/proxy_inspect.go:1`

## `cq service`

Manage proxy and periodic token-refresh background services.

```text
cq service <COMMAND> [OPTIONS]
```

[Exact help](help/service.txt) · Family: navigation · Kind: group

Manage proxy and periodic token-refresh background services.
Choose a subcommand below. A bare group prints this help without reading configuration or changing state.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Print exact generated group help and exit 0 without state access.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.


Human output template:

```text
Exact generated help/service.txt; plain text even with --json.
```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq service --help
```

Show all immediate subcommands and usage.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence


## `cq service install`

Install and start selected CQ services.

```text
cq service install [OPTIONS]
```

[Exact help](help/service-install.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `install`: Register and start selected background components.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 60s.

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Preflight ownership and executable, acquire installer lock, snapshot selected registrations, install proxy before token-refresh when both selected, start and verify each.
- On failure restore previously selected registrations and report rollback outcome; never claim success for partial installation.
- Register current executable identity; package-managed ownership remains protected. No automatic install by check/account commands.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Health verification fails, or strict status detects unhealthy selected component. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_install_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service install --component proxy
```

Apply only to the shared proxy.

```sh
cq service install --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

- `cq agent install` → `service install --component token-refresh`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.
- `cq proxy install` → `service install --component proxy`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq service restart`

Restart selected installed CQ services.

```text
cq service restart [OPTIONS]
```

[Exact help](help/service-restart.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
Stopping or restarting the proxy can interrupt active Claude and Codex traffic.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `restart`: Stop and start selected registered components, then verify their health.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 60s.

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Require selected registrations; restart proxy before token-refresh when both selected. Verify proxy runtime health. For token-refresh, wait for the newly requested run to complete with exit 0; this verifies the restart even when durable enablement remains false and reported healthy remains false.
- Preserve selected components existing enabled/disabled-at-login policy. A disabled component is started for this session only; future automatic starts remain disabled.
- May interrupt provider traffic if proxy selected; does not run model acceptance validation.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Proxy runtime verification fails, or the newly requested token-refresh run completes with nonzero exit. Disabled automatic-start policy alone is not restart failure. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_restart_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service restart --component proxy
```

Apply only to the shared proxy.

```sh
cq service restart --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

- `cq proxy restart` → `service restart --component proxy`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq service start`

Start selected installed CQ services.

```text
cq service start [OPTIONS]
```

[Exact help](help/service-start.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `start`: Start registered components and enable future scheduled/login starts.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 60s.

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Require selected registrations already installed; enable their automatic starts, start proxy before token-refresh and wait for health.
- Already running and enabled healthy selection is idempotent. Never installs missing components.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Health verification fails, or strict status detects unhealthy selected component. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_start_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service start --component proxy
```

Apply only to the shared proxy.

```sh
cq service start --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq service status`

Inspect selected CQ service components.

```text
cq service status [OPTIONS]
```

[Exact help](help/service-status.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `status`: Inspect current state without changing it or initiating acceptance validation.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--strict`

Return non-zero if selected components are not installed, not running, or unhealthy. Default: false.

Type: boolean. Required: false. Repeatable: false.

Default: false.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 10s.

Type: duration. Required: false. Repeatable: false.

Default: "10s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Inspect selected definitions, desired enablement, process and health without writes.
- Default exit0 means inspection completed. --strict requires every selected component installed and healthy: proxy running; token-refresh enabled with a successful latest scheduled run within 35 minutes (idle between runs is normal); no actual model acceptance calls.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Health verification fails, or strict status detects unhealthy selected component. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_status_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service status --component proxy
```

Apply only to the shared proxy.

```sh
cq service status --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq service stop`

Stop and disable selected CQ services.

```text
cq service stop [OPTIONS]
```

[Exact help](help/service-stop.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
Stopping or restarting the proxy can interrupt active Claude and Codex traffic.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `stop`: Stop selected components and disable future automatic starts until explicit start.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 60s.

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Disable future automatic starts, stop token-refresh before proxy, and verify selected components stopped.
- Persist disabled state: later quota/account commands do not restart components; only explicit start/install changes desired enabled state.
- Already stopped selection is idempotent. Do not delete definitions, credentials, caches, logs or configuration.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Health verification fails, or strict status detects unhealthy selected component. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_stop_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service stop --component proxy
```

Apply only to the shared proxy.

```sh
cq service stop --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

No additional spellings; see migration inventory for old flags on unchanged paths.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq service uninstall`

Stop and remove selected CQ service registrations.

```text
cq service uninstall [OPTIONS]
```

[Exact help](help/service-uninstall.txt) · Family: root · Kind: command

CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
Stopping or restarting the proxy can interrupt active Claude and Codex traffic.

### Naming rationale

- `service`: Operating-system-managed background processes owned by CQ.
- `uninstall`: Remove CQ service registrations while retaining user data and credentials.

### Arguments

None.

### Options

#### `--component COMPONENT`

Service component: all, proxy, or token-refresh. Default: all.

Type: enum. Required: false. Repeatable: false.

Default: "all".

Choices: all, proxy, token-refresh.

Constraint: Exact enum; selection never widens after validation.

#### `--timeout DURATION`

Maximum elapsed time, including locks, requests and local verification. Default: 60s.

Type: duration. Required: false. Repeatable: false.

Default: "60s".

Constraint: Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

### Preconditions

- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

### Effects and completion

- Stop and remove selected CQ-owned service registrations, token-refresh before proxy.
- Persist desired disabled state so later check/account commands cannot reinstall them. Keep credentials/config/cache/logs/receipts/installation binary.
- Absence is idempotent; foreign ownership is conflict, never remove foreign registration.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `component`: enum all|proxy|token-refresh
- `action`: enum install|start|stop|restart|status|uninstall
- `components`: array<ServiceComponentV2>, proxy then token-refresh when both selected
- `rollback`: enum not_needed|restored|failed

Human output template:

```text
CQ services ({component}): {action}
{each selected component: id + ": " + state + ", enabled=" + enabled + ", healthy=" + healthy + "\n"}
Rollback: {rollback}

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |
| service_owner_conflict | 6 | Requested write conflicts with package or foreign ownership. | Service ownership conflicts with this operation. |
| service_unavailable | 4 | OS manager unavailable or unsupported. | The current-user service manager is unavailable. |
| service_not_installed | 3 | start/restart targets an unregistered component; strict status observes missing. | Service component {component} is not installed. |
| service_unhealthy | 1 | Health verification fails, or strict status detects unhealthy selected component. | Service component {component} is unhealthy. |
| service_partial | 8 | At least one component changed but final state/rollback incomplete. | Service operation was only partially completed; inspect component results. |
| service_uninstall_timeout | 7 | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. | Operation timed out; inspect state before retrying. |

### Examples

```sh
cq service uninstall --component proxy
```

Apply only to the shared proxy.

```sh
cq service uninstall --component token-refresh
```

Apply only to the scheduled token-refresh component.

### Compatibility spellings

- `cq agent uninstall` → `service uninstall --component token-refresh`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.
- `cq proxy uninstall` → `service uninstall --component proxy`. Deprecated spelling; emit canonical behaviour and v2 output, with the warning required by migration.md.

### Source evidence

- `v0.32.5:cmd/cq/service_command.go:85`
- `v0.32.5:cmd/cq/service.go:72`

## `cq version`

Show CQ build identity.

```text
cq version [OPTIONS]
```

[Exact help](help/version.txt) · Family: root · Kind: command

Print installed CQ version and source revision. No user configuration is read.

### Naming rationale

- `version`: Print CQ build identity without opening user state.

### Arguments

None.

### Options

None.

### Preconditions

None beyond global rules.

### Effects and completion

- Read build metadata embedded in current executable only.

### Output

Fields below belong to envelope `data`; global envelope keys are specified once in README.md.

- `version`: string release SemVer, or dev
- `revision`: string40lowerhex|null when unknown
- `dirty`: boolean|null when build provenance unknown
- `cli_schema_version`: integer constant2

Human output template:

```text
cq {version}
Revision: {revision or "unknown"}
CLI schema: 2

```

### Command errors

| Code | Exit | Condition | Exact message |
| --- | ---: | --- | --- |

### Examples

```sh
cq version --json
```

Read machine-readable build identity.

### Compatibility spellings

- `cq --version / -v` → `version`. Terminating global action; no deprecation warning.

### Source evidence

- `cmd/cq/main.go:215`
