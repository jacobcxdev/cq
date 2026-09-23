# Accounts, quotas, models and utilities: concrete output examples

These are hand-authored design fixtures, not captured runtime results. The reference clock is 23 Sep 2026, 16:30 BST (UTC+01:00). Accounts, identifiers and model data are fictional. The text is the complete uncoloured terminal content for each labelled stream; colour follows the shared design. Empty streams are explicitly identified outside fences. Help examples also cover `-h` and `cq help PATH`. Boolean flags accept a bare flag or `=true`/`=false`; duplicate spellings are rejected. The help fixtures repeat this rule so each page stands alone. No scenario executes a mutation.

## `cq auth refresh`

### Help

Invocation: `cq auth refresh --help`. Stdout:

```text
cq auth refresh
Refresh eligible stored Claude and Codex OAuth credentials.

Usage
  cq auth refresh [PROVIDER…] [options]

Refresh credentials with 30 minutes or less remaining, including expired tokens.
Unknown expiry is skipped. Codex system and external credentials stay read-only.
Claude may offer browser login in a terminal; JSON and redirected input do not.
Gemini refreshes during quota checks. No overall deadline applies: HTTP requests
allow 10s and a started Claude browser callback allows 5m; prompts have no deadline.

Arguments
  PROVIDER  claude or codex; omit for both, in that order. No duplicates.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account login
  cq codex account login
  cq check

Examples
  Refresh both providers
    cq auth refresh
  Refresh CQ-managed Codex credentials without prompts
    cq auth refresh codex --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq auth refresh codex`. Stdout:

```text
Authentication refresh
  Provider  Refreshed  Unchanged  Need login  Failed
  Codex             1          1           0       0

  alex@example.test · Refreshed
    Reference: account-alex
    Credentials: Saved
  sam@example.test · Unchanged · More than 30 minutes remaining
    Reference: account-sam
    Credentials: Unchanged

Credentials saved for 1 account
```

Stderr: empty. Exit: 0.

### No-op

Invocation: `cq auth refresh codex`. Stdout:

```text
Authentication refresh
  Provider  Refreshed  Unchanged  Need login  Failed
  Codex             0          1           0       0

  alex@example.test · Unchanged · Expiry unknown
    Reference: account-alex
    Credentials: Unchanged

No credentials needed refreshing.
```

Stderr: empty. Exit: 0.

### Partial

Invocation: `cq auth refresh codex`. Stdout:

```text
Authentication refresh · Partial
  Provider  Refreshed  Unchanged  Need login  Failed
  Codex             1          0           1       0

  alex@example.test · Refreshed
    Reference: account-alex
    Credentials: Saved
  sam@example.test · Login required
    Reference: account-sam
    Credentials: Unchanged

Credentials saved for 1 account
```

Stderr:

```text
Warning · Credential refresh completed partially; inspect provider results.
  Code: auth_refresh_partial
  Command: cq auth refresh
```

Exit: 8.

### Failure

Invocation: `cq auth refresh codex`. Stdout: empty. Stderr:

```text
Error · Codex credential authority is unavailable.
  Code: auth_authority_unavailable
  Command: cq auth refresh
```

Exit: 4.

### Interrupted after a confirmed write

Invocation: `cq auth refresh codex`. Stdout:

```text
Authentication refresh · Interrupted
  alex@example.test · Refreshed
    Reference: account-alex
    Credentials: Saved

Credentials saved for 1 account
Remaining refresh outcomes: unknown
```

Stderr:

```text
Warning · Credential refresh interrupted.
  Code: auth_interrupted
  Command: cq auth refresh
```

Exit: 130. This fixture assumes a returned receipt proves the first write.

## `cq check`

### Help

Invocation: `cq check --help`. Stdout:

```text
cq check
Report quota usage for selected providers.

Usage
  cq check [PROVIDER…] [options]

Bare cq checks all providers too. Exhausted quota is a valid observation.
A failed fetch may return labelled stale data, even with --fresh.
This command does not select a proxy routing account.

Arguments
  PROVIDER  claude, codex or gemini. Omit for all three, in that order.
            Exact lowercase names; no duplicates.

Options
  --fresh             Skip the initial quota cache lookup. Default: false.
                      Does not separately refresh authentication credentials.
  --timeout DURATION  Total work deadline. Default: 30s.
                      Positive Go duration, at most 10m; bare numbers invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq auth refresh
  cq codex reset recommend

Examples
  Check Codex
    cq check codex
  Fetch selected providers and emit JSON
    cq check claude codex --fresh --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq check codex --fresh`. Stdout:

```text
Codex pro 20x · alex@example.test · Native default
  7d  ━━━━━━━━|━━━━━━━━━━╌╌  Remaining 89%  Reset 2d 19h  Pace +49pp  Empty 33d 22h

  Reset: in 2d 19h · 26 Sep 2026, 11:30 BST (UTC+01:00)

Observed 23 Sep 2026, 16:30:00 BST (UTC+01:00)
```

Stderr: empty. Exit: 0. One account is configured, so no aggregate is emitted. The window lasts 7 days, its remaining duration is 67 hours (39.88% expected remaining), and its observed burn rate is 89 percentage points per 33 days 22 hours. These inputs give the displayed +49pp pace and burndown.

### Partial with stale data

Invocation: `cq check codex --fresh`. Stdout:

```text
Codex pro 20x · alex@example.test · Stale · refresh failed
  7d  ━━━━━━━━|━━━━━━━━━━╌╌  Remaining 89%  Reset 2d 19h  Pace +49pp  Empty 33d 22h
  Reset: in 2d 19h · 26 Sep 2026, 11:30 BST (UTC+01:00)
  Cached observation: 4m ago · 23 Sep 2026, 16:26:00 BST (UTC+01:00)
```

Stderr:

```text
Warning · Some quota results could not be refreshed.
  Code: check_partial
  Command: cq check
  Cause: quota_stale_fallback
    Cached quota used after a failed fetch for codex.
```

Exit: 8.

### Failure

Invocation: `cq check gemini`. Stdout:

```text
Gemini · Authentication required
  antigravity-cli
  Authentication is required.
```

Stderr:

```text
Error · Authentication is required for the selected providers.
  Code: check_authentication
  Command: cq check
```

Exit: 5.

### Timeout with no completed rows

Invocation: `cq check codex --timeout 1s`. Stdout: empty. Stderr:

```text
Warning · Quota checking timed out.
  Code: check_timeout
  Command: cq check
```

Exit: 7.

## `cq claude account activate`

### Help

Invocation: `cq claude account activate --help`. Stdout:

```text
cq claude account activate
Select the native client default Claude account.

Usage
  cq claude account activate ACCOUNT [options]

Requires managed activatable credentials unless already active.
Existing clients may need reconnection. Proxy pins and session bindings do not change.
Selecting the already active account succeeds without changing credentials.

Arguments
  ACCOUNT  Unique known email, case-insensitive after trimming.
           Empty, ambiguous and unstable references are rejected; no keys or aliases.

Options
  --timeout DURATION  Total work deadline. Default: 30s; range: 1s to 10m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude proxy pin show

Examples
  Select the known account
    cq claude account activate alex@example.test
  Allow one minute
    cq claude account activate alex@example.test --timeout 1m
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude account activate alex@example.test`. Stdout:

```text
Claude native client default · Changed
  alex@example.test
  Reference       alex@example.test
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            claude_max_20x

Existing clients may need reconnection.
```

Stderr: empty.

Exit: 0.

### No-op

Invocation: `cq claude account activate alex@example.test`. Stdout:

```text
Claude native client default · Already active
  alex@example.test
  Reference       alex@example.test
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            claude_max_20x
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq claude account activate sam@example.test`. Stdout: empty.

Stderr:

```text
Error · Account has no managed credentials that can be activated.
  Code: account_not_activatable
  Command: cq claude account activate
```

Exit: 6.

### Partial

Invocation: `cq claude account activate alex@example.test`. Stdout:

```text
Claude native client default · Partial
  Credentials changed
  Metadata verification: incomplete
```

Stderr:

```text
Warning · Native client credentials changed, but account metadata needs recovery.
  Code: account_activation_partial
  Command: cq claude account activate
```

Exit: 8.

## `cq claude account list`

### Help

Invocation: `cq claude account list --help`. Stdout:

```text
cq claude account list
List locally configured Claude accounts.

Usage
  cq claude account list [options]

Active means native client default, not proxy routing selection or authentication health.
Reads local metadata only; does not fetch quota, refresh or activate credentials.
An empty inventory succeeds. Copy returned references exactly.

Options
  --timeout DURATION  Total work deadline. Default: 30s; range: 1s to 10m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account activate
  cq check

Examples
  Inspect local identities
    cq claude account list
  Read structured metadata
    cq claude account list --json
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude account list`. Stdout:

```text
Claude accounts · 1

  alex@example.test
  Reference       alex@example.test
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            claude_max_20x
```

Stderr: empty.

Exit: 0.

### Empty

Invocation: `cq claude account list`. Stdout:

```text
Claude accounts · 0
  No configured accounts.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq claude account list`. Stdout: empty.

Stderr:

```text
Error · Account inventory is unavailable.
  Code: account_inventory_unavailable
  Command: cq claude account list
```

Exit: 4.

## `cq claude account login`

### Help

Invocation: `cq claude account login --help`. Stdout:

```text
cq claude account login
Authenticate and store a Claude account.

Usage
  cq claude account login [options]

Opens browser authentication and saves CQ-managed credentials.
Login alone does not select the native default. No service is installed.
JSON suppresses terminal prompts; browser authentication still needs your input.
Proxy pins and session bindings do not change.

Options
  --activate          Select this account as native default after saving. Default: false.
                      Existing clients may need reconnection.
  --timeout DURATION  Total work deadline. Default: 10m; range: 1s to 30m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude account activate

Examples
  Store an account without selecting it
    cq claude account login
  Store and select the account
    cq claude account login --activate
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude account login`. Stdout:

```text
Claude login · Credentials saved
  alex@example.test
  Native default: Not selected
  Reference       alex@example.test
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Not active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            claude_max_20x
```

Stderr: empty.

Exit: 0.

### Success with activation

Invocation: `cq claude account login --activate`. Stdout:

```text
Claude login · Credentials saved
  alex@example.test
  Native default: Selected
  Reference       alex@example.test
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            claude_max_20x

Existing clients may need reconnection.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq claude account login`. Stdout: empty.

Stderr:

```text
Error · Browser authentication failed.
  Code: account_auth_failed
  Command: cq claude account login
```

Exit: 5.

### Partial activation

Invocation: `cq claude account login --activate`. Stdout:

```text
Claude login · Partial
  Credentials: Saved
  Account: alex@example.test
  Native default: Unknown
```

Stderr:

```text
Warning · Credentials were saved, but native client activation failed.
  Code: account_login_partial
  Command: cq claude account login
```

Exit: 8.

### Timeout before credentials saved

Invocation: `cq claude account login --timeout 1s`. Stdout: empty.

Stderr:

```text
Warning · Account command timed out.
  Code: account_timeout
  Command: cq claude account login
```

Exit: 7. The absent receipt does not assert rollback or prove that no external authentication occurred.

## `cq claude account remove`

### Help

Invocation: `cq claude account remove --help`. Stdout:

```text
cq claude account remove
Remove CQ-managed local Claude credentials.

Usage
  cq claude account remove ACCOUNT [options]

Shows the resolved account and native credential impact before confirmation.
Removes local managed credentials only; external sources and upstream accounts remain.
Externally managed credentials cannot be removed by CQ.

Arguments
  ACCOUNT  Unique known email, case-insensitive after trimming.
           Empty, ambiguous and unstable references are rejected; no keys or aliases.

Options
  --yes               Confirm this removal without a prompt. Default: false.
                      Required with --json or non-terminal stdin; applies only once.
  --timeout DURATION  Work deadline, excluding confirmation. Default: 30s.
                      Go duration from 1s to 10m; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude account login

Examples
  Review and confirm removal
    cq claude account remove alex@example.test
  Explicitly confirm non-interactive removal
    cq claude account remove alex@example.test --yes --json
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude account remove alex@example.test --yes`. Stdout:

```text
Claude local credentials · Removed
  Reference           alex@example.test
  Native credentials  Removed
  External sources    none
```

Stderr: empty.

Exit: 0.

### Confirmation declined

Invocation: `cq claude account remove alex@example.test`. Stdout:

```text
Cancelled · no change requested
  No credentials removed.
```

Stderr:

```text
Remove Claude local credentials
  Account: alex@example.test
  Reference: alex@example.test
  Native active credentials will be removed: Yes
  Upstream account: Retained

Remove local credentials for alex@example.test? [y/N]
```

Exit: 0. Input: `n` followed by Enter. The input is not part of either output stream.

### Failure with redirected input

Invocation: `cq claude account remove alex@example.test`. Stdout: empty.

Stderr:

```text
Error · Local credential removal requires --yes in non-interactive mode.
  Code: account_confirmation_required
  Command: cq claude account remove
```

Exit: 6.

### Partial removal

Invocation: `cq claude account remove alex@example.test --yes`. Stdout: empty.

Stderr:

```text
Warning · Local credential removal is incomplete; recover the pending operation before retrying.
  Code: account_removal_partial
  Command: cq claude account remove
```

Exit: 8. No resource was returned in this fixture. Do not infer that credentials were retained or restored.

## `cq codex account activate`

### Help

Invocation: `cq codex account activate --help`. Stdout:

```text
cq codex account activate
Select the native client default Codex account.

Usage
  cq codex account activate ACCOUNT [options]

Requires managed activatable credentials unless already active.
Existing clients may need reconnection. Proxy pins and session bindings do not change.
Selecting the already active account succeeds without changing credentials.

Arguments
  ACCOUNT  Exact opaque key, unique email or existing CQ alias. Exact keys win.
           Email and alias matching ignores case and surrounding whitespace.
           Empty, ambiguous and unstable references are rejected.

Options
  --timeout DURATION  Total work deadline. Default: 30s; range: 1s to 10m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex account list
  cq codex proxy pin show

Examples
  Select the known account
    cq codex account activate alex@example.test
  Allow one minute
    cq codex account activate alex@example.test --timeout 1m
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq codex account activate alex@example.test`. Stdout:

```text
Codex native client default · Changed
  alex@example.test
  Reference       account-alex
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            codex_pro_20x

Existing clients may need reconnection.
```

Stderr: empty.

Exit: 0.

### No-op

Invocation: `cq codex account activate alex@example.test`. Stdout:

```text
Codex native client default · Already active
  alex@example.test
  Reference       account-alex
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            codex_pro_20x
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq codex account activate sam@example.test`. Stdout: empty.

Stderr:

```text
Error · Account has no managed credentials that can be activated.
  Code: account_not_activatable
  Command: cq codex account activate
```

Exit: 6.

### Partial

Invocation: `cq codex account activate alex@example.test`. Stdout:

```text
Codex native client default · Partial
  Credentials changed
  Metadata verification: incomplete
```

Stderr:

```text
Warning · Native client credentials changed, but account metadata needs recovery.
  Code: account_activation_partial
  Command: cq codex account activate
```

Exit: 8.

## `cq codex account list`

### Help

Invocation: `cq codex account list --help`. Stdout:

```text
cq codex account list
List locally configured Codex accounts.

Usage
  cq codex account list [options]

Active means native client default, not proxy routing selection or authentication health.
Reads local metadata only; does not fetch quota, refresh or activate credentials.
An empty inventory succeeds. Copy returned references exactly.

Options
  --timeout DURATION  Total work deadline. Default: 30s; range: 1s to 10m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex account activate
  cq check

Examples
  Inspect local identities
    cq codex account list
  Read structured metadata
    cq codex account list --json
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq codex account list`. Stdout:

```text
Codex accounts · 1

  alex@example.test
  Reference       account-alex
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            codex_pro_20x
```

Stderr: empty.

Exit: 0.

### Empty

Invocation: `cq codex account list`. Stdout:

```text
Codex accounts · 0
  No configured accounts.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq codex account list`. Stdout: empty.

Stderr:

```text
Error · Account inventory is unavailable.
  Code: account_inventory_unavailable
  Command: cq codex account list
```

Exit: 4.

## `cq codex account login`

### Help

Invocation: `cq codex account login --help`. Stdout:

```text
cq codex account login
Authenticate and store a Codex account.

Usage
  cq codex account login [options]

Opens browser authentication and saves CQ-managed credentials.
Login alone does not select the native default. No service is installed.
JSON suppresses terminal prompts; browser authentication still needs your input.
Proxy pins and session bindings do not change.

Options
  --activate          Select this account as native default after saving. Default: false.
                      Existing clients may need reconnection.
  --timeout DURATION  Total work deadline. Default: 10m; range: 1s to 30m.
                      Use Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex account list
  cq codex account activate

Examples
  Store an account without selecting it
    cq codex account login
  Store and select the account
    cq codex account login --activate
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq codex account login`. Stdout:

```text
Codex login · Credentials saved
  alex@example.test
  Native default: Not selected
  Reference       account-alex
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Not active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            codex_pro_20x
```

Stderr: empty.

Exit: 0.

### Success with activation

Invocation: `cq codex account login --activate`. Stdout:

```text
Codex login · Credentials saved
  alex@example.test
  Native default: Selected
  Reference       account-alex
  Account ID      acct_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         cq_managed
  Aliases         none
  Tier            codex_pro_20x

Existing clients may need reconnection.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq codex account login`. Stdout: empty.

Stderr:

```text
Error · Browser authentication failed.
  Code: account_auth_failed
  Command: cq codex account login
```

Exit: 5.

### Partial activation

Invocation: `cq codex account login --activate`. Stdout:

```text
Codex login · Partial
  Credentials: Saved
  Account: alex@example.test
  Native default: Unknown
```

Stderr:

```text
Warning · Credentials were saved, but native client activation failed.
  Code: account_login_partial
  Command: cq codex account login
```

Exit: 8.

### Timeout before credentials saved

Invocation: `cq codex account login --timeout 1s`. Stdout: empty.

Stderr:

```text
Warning · Account command timed out.
  Code: account_timeout
  Command: cq codex account login
```

Exit: 7. The absent receipt does not assert rollback or prove that no external authentication occurred.

## `cq codex account remove`

### Help

Invocation: `cq codex account remove --help`. Stdout:

```text
cq codex account remove
Remove CQ-managed local Codex credentials.

Usage
  cq codex account remove ACCOUNT [options]

Shows the resolved account and native credential impact before confirmation.
Removes local managed credentials only; external sources and upstream accounts remain.
Externally managed credentials cannot be removed by CQ.

Arguments
  ACCOUNT  Exact opaque key, unique email or existing CQ alias. Exact keys win.
           Email and alias matching ignores case and surrounding whitespace.
           Empty, ambiguous and unstable references are rejected.

Options
  --yes               Confirm this removal without a prompt. Default: false.
                      Required with --json or non-terminal stdin; applies only once.
  --timeout DURATION  Work deadline, excluding confirmation. Default: 30s.
                      Go duration from 1s to 10m; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex account list
  cq codex account login

Examples
  Review and confirm removal
    cq codex account remove alex@example.test
  Explicitly confirm non-interactive removal
    cq codex account remove alex@example.test --yes --json
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq codex account remove alex@example.test --yes`. Stdout:

```text
Codex local credentials · Removed
  Reference           account-alex
  Native credentials  Removed
  External sources    none
```

Stderr: empty.

Exit: 0.

### Confirmation declined

Invocation: `cq codex account remove alex@example.test`. Stdout:

```text
Cancelled · no change requested
  No credentials removed.
```

Stderr:

```text
Remove Codex local credentials
  Account: alex@example.test
  Reference: account-alex
  Native active credentials will be removed: Yes
  Upstream account: Retained

Remove local credentials for alex@example.test? [y/N]
```

Exit: 0. Input: `n` followed by Enter. The input is not part of either output stream.

### Failure with redirected input

Invocation: `cq codex account remove alex@example.test`. Stdout: empty.

Stderr:

```text
Error · Local credential removal requires --yes in non-interactive mode.
  Code: account_confirmation_required
  Command: cq codex account remove
```

Exit: 6.

### Partial removal

Invocation: `cq codex account remove alex@example.test --yes`. Stdout: empty.

Stderr:

```text
Warning · Local credential removal is incomplete; recover the pending operation before retrying.
  Code: account_removal_partial
  Command: cq codex account remove
```

Exit: 8. No resource was returned in this fixture. Do not infer that credentials were retained or restored.

## `cq codex reset list`

### Help

Invocation: `cq codex reset list --help`. Stdout:

```text
cq codex reset list
List Codex banked reset credits.

Usage
  cq codex reset list [ACCOUNT] [options]

Inspect credits without consuming them. Incomplete inventories retain readable rows.

Arguments
  ACCOUNT  Exact opaque key, unique email or existing CQ alias. Omit for all accounts.
           Exact keys win; email and alias matching ignores case and outer whitespace.
           Empty, ambiguous and unstable references are rejected.

Options
  --timeout DURATION  Total work deadline. Default: 60s; range: 1s to 10m.
                      Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex reset recommend
  cq codex reset use

Examples
  Inspect all accounts
    cq codex reset list
  Inspect one account as JSON
    cq codex reset list alex@example.test --json
```

Stderr: empty. Exit: 0.

### Success · all accounts

Invocation: `cq codex reset list`. Stdout:

```text
Codex reset credits · Complete

  alex@example.test
    Reference: account-alex
    Account ID: acct_alex_01
    Available: 1 credit

    Credit       credit-september-01
    Type         codex_rate_limits
    Status       available
    Supported    Yes
    Granted      2d ago · 21 Sep 2026, 16:30 BST (UTC+01:00)
    Expires      in 2d · 25 Sep 2026, 16:30 BST (UTC+01:00)

  sam@example.test
    Reference: account-sam
    Account ID: acct_sam_01
    Available: 0 credits

    No reset credits.
```

Stderr: empty. Exit: 0. Both account inventories were fetched successfully. Zero available credits is a known result, not an authentication failure or an unknown inventory.

### Success · one account

Invocation: `cq codex reset list alex@example.test`. Stdout:

```text
Codex reset credits · Complete
  alex@example.test
  Reference: account-alex
  Account ID: acct_alex_01
  Available: 1 credit

  Credit       credit-september-01
  Type         codex_rate_limits
  Status       available
  Supported    Yes
  Granted      2d ago · 21 Sep 2026, 16:30 BST (UTC+01:00)
  Expires      in 2d · 25 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty. Exit: 0.

### Empty

Invocation: `cq codex reset list alex@example.test`. Stdout:

```text
Codex reset credits · Complete
  alex@example.test
  Reference: account-alex
  Account ID: acct_alex_01
  Available: 0 credits

No reset credits.
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq codex reset list`. Stdout: empty. Stderr:

```text
Error · Codex reset authentication failed.
  Code: codex_reset_auth_failed
  Command: cq codex reset list
```

Exit: 5.

### Partial inventory

Invocation: `cq codex reset list`. Stdout:

```text
Codex reset credits · Incomplete
  alex@example.test
    Reference: account-alex
    Account ID: acct_alex_01
    Available: 0 credits
    No reset credits.

  sam@example.test
    Reference: account-sam
    Account ID: acct_sam_01
    Available: unknown
    credits_unavailable: Reset-credit inventory is unavailable.
```

Stderr:

```text
Warning · Reset-credit inventory is incomplete.
  Code: codex_reset_inventory_partial
  Command: cq codex reset list
```

Exit: 8.

## `cq codex reset recommend`

### Help

Invocation: `cq codex reset recommend --help`. Stdout:

```text
cq codex reset recommend
Recommend when to use Codex reset credits.

Usage
  cq codex reset recommend [options]

Read fresh portfolio inputs and compute advisory timing for all visible accounts.
Does not consume credits. Incomplete inputs expose blockers, not actionable times.
No options change the scheduler objective, algorithm or planning horizon.

Options
  --timeout DURATION  Total work deadline. Default: 120s; range: 1s to 10m.
                      Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex reset list
  cq codex reset use

Examples
  Compute advisory timing
    cq codex reset recommend
  Inspect the structured schedule
    cq codex reset recommend --json --timeout 120s
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq codex reset recommend`. Stdout:

```text
Codex reset plan · Advisory
  Inputs      Complete
  Search      Exact
  Confidence  High
  Generated   23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Horizon     in 9d · 2 Oct 2026, 16:30 BST (UTC+01:00)

  alex@example.test · Scheduled
    Reference    account-alex
    Account ID   acct_alex_01
    Credit       credit-september-01
    Use at       in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
    Use by       in 2d · 25 Sep 2026, 16:30 BST (UTC+01:00)
    Confidence   High
    Gap avoided  45m
    Restored     5h 100% · 7d 80%
    Reasons      gap_avoidance, expiry_pressure

Objective
  Unmet demand                   0 %-seconds
  Gap duration                   0s
  Useful credits expired unused  0
  Weighted discarded quota       4%
  Restored quota                 180%

Blockers
  None
```

Stderr: empty. Exit: 0. Objective percentages are sums and can exceed 100%; they are not percentage bars.

### No action recommended

Invocation: `cq codex reset recommend`. Stdout:

```text
Codex reset plan · Advisory
  Inputs      Complete
  Search      Exact
  Confidence  High
  Generated   23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Horizon     in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)

No reset use recommended.

Objective
  Unmet demand                   0 %-seconds
  Gap duration                   0s
  Useful credits expired unused  0
  Weighted discarded quota       0%
  Restored quota                 0%

Blockers
  None
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq codex reset recommend`. Stdout: empty. Stderr:

```text
Error · Codex reset credentials are unavailable.
  Code: codex_reset_credentials_unavailable
  Command: cq codex reset recommend
```

Exit: 4.

### Incomplete recommendation

Invocation: `cq codex reset recommend`. Stdout:

```text
Codex reset plan · Incomplete
  Inputs      Incomplete
  Search      Bounded
  Confidence  Low
  Generated   23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Horizon     in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)

Recommendation unavailable until blockers are resolved.

Objective
  Unmet demand                   0 %-seconds
  Gap duration                   0s
  Useful credits expired unused  0
  Weighted discarded quota       0%
  Restored quota                 0%

Blockers
  usage_unavailable · alex@example.test
    Reference: account-alex
    Account ID: acct_alex_01
```

Stderr:

```text
Warning · Reset recommendation is incomplete; resolve the reported blockers.
  Code: codex_reset_recommendation_incomplete
  Command: cq codex reset recommend
```

Exit: 8. Objective zeroes are returned values, not evidence that demand is satisfied.

## `cq codex reset use`

### Help

Invocation: `cq codex reset use --help`. Stdout:

```text
cq codex reset use
Consume one Codex reset credit for one account.

Usage
  cq codex reset use ACCOUNT [options]

Shows account, selected credit and current windows before confirmation.
A timeout or unknown result never authorises another credit. Retry only the same
account and credit when the returned retry receipt instructs you to do so.

Arguments
  ACCOUNT  Exact opaque key, unique email or existing CQ alias. Exact keys win.
           Email and alias matching ignores case and outer whitespace.
           Empty, ambiguous and unstable references are rejected.

Options
  --credit CREDIT_ID  Exact, non-empty ID with no outer whitespace; must belong to ACCOUNT.
                      Must be an eligible codex_rate_limits credit or unresolved replay.
                      Omit to resume the single unresolved attempt, otherwise choose
                      earliest expiry. No-expiry credits sort last; ties use ID bytes.
  --yes               Confirm this consumption without a prompt. Default: false.
                      Required with --json or non-terminal stdin; applies only once.
  --timeout DURATION  Work deadline excluding confirmation. Default: 120s.
                      Go duration from 1s to 10m; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq codex reset list
  cq codex reset recommend

Examples
  Review one known credit before deciding
    cq codex reset use alex@example.test --credit credit-september-01
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · Completed
  Account        account-alex
  Credit         credit-september-01
  Windows reset  1

  7d
    Remaining before  5%
    Remaining after   100%
    Reset before      in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
    Reset after       in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
    Period before     7d
    Period after      7d
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         5% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
```

Exit: 0. The current command emits a preview even with `--yes`; do not remove that stream from comparisons.

### Confirmation declined

Invocation: `cq codex reset use alex@example.test --credit credit-september-01`. Stdout:

```text
Cancelled · no change requested
  Account: account-alex
  Credit: credit-september-01
  No credit consumed.
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         5% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)

Use reset credit credit-september-01 for alex@example.test? [y/N]
```

Exit: 0. Input: `n` followed by Enter, not part of output.

### Failure

Invocation: `cq codex reset use alex@example.test --credit credit-missing --yes`. Stdout: empty. Stderr:

```text
Error · Selected reset credit is unavailable; no replacement credit was consumed.
  Code: codex_reset_credit_not_found
  Command: cq codex reset use
```

Exit: 3.

### Indeterminate consumption

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · Completion unknown
  Account        account-alex
  Credit         credit-september-01
  Windows reset  unknown

Retry only this request
  cq codex reset use account-alex --credit credit-september-01
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         5% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)

Error · Reset outcome is unknown; retry only the same account and credit.
  Code: codex_reset_consume_indeterminate
  Command: cq codex reset use
```

Exit: 1. The domain's zero count for an indeterminate outcome must not be presented as proof that no window reset.

### Known reset, incomplete postcheck

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · Completed · Verification incomplete
  Account        account-alex
  Credit         credit-september-01
  Windows reset  1
  Window changes  Not verified
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         5% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)

Warning · Reset outcome is known, but local verification or cleanup is incomplete.
  Code: codex_reset_postcheck_partial
  Command: cq codex reset use
  Cause: codex_reset_usage_refetch_failed
    Fresh quota could not be read.
```

Exit: 8. No retry suggestion for another credit.

### Already redeemed replay

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · Already completed
  Account        account-alex
  Credit         credit-september-01
  Windows reset  0

This reset request was already completed.
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         100% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
```

Exit: 0. This receipt reports zero windows reset by this replay; it does not erase the earlier reset.

### Nothing to reset

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · Nothing to reset
  Account        account-alex
  Credit         credit-september-01
  Windows reset  0
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         100% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         100% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
```

Exit: 0.

### No credit returned by consumption

Invocation: `cq codex reset use alex@example.test --credit credit-september-01 --yes`. Stdout:

```text
Codex reset · No eligible reset credit
  Account        account-alex
  Credit         credit-september-01
  Windows reset  0
```

Stderr:

```text
Codex reset preview
  Account    alex@example.test
  Reference  account-alex
  Credit     credit-september-01
  5h         75% remaining · in 2h · 23 Sep 2026, 18:30 BST (UTC+01:00)
  7d         5% remaining · in 2d 18h · 26 Sep 2026, 10:30 BST (UTC+01:00)
```

Exit: 0. The credit was selected successfully, but the provider returned `no_credit`; distinguish this terminal outcome from pre-selection `codex_reset_credit_not_found`.

## `cq claude proxy pin clear`

### Help

Invocation: `cq claude proxy pin clear --help`. Stdout:

```text
cq claude proxy pin clear
Remove the saved Claude proxy account pin.

Usage
  cq claude proxy pin clear [options]

Saves configuration; a running proxy applies it asynchronously without a restart.
Does not change the native client default or rewrite existing session bindings.

Options
  --timeout DURATION  Total work deadline, including preparation and requests.
                      Default: 10s. Positive Go duration; bare numbers invalid.
                      A timed-out write may have committed: inspect state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude proxy pin
  cq proxy

Examples
  Remove the saved Claude proxy account pin.
    cq claude proxy pin clear
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude proxy pin clear`. Stdout:

```text
Claude proxy pin · Not configured
  Account          none
  Application      Hot reload · asynchronous
  Restart required No
```

Stderr: empty.

Exit: 0.

### No pin configured

Invocation: `cq claude proxy pin clear`. Stdout:

```text
Claude proxy pin · Not configured
  Account          none
  Application      Hot reload · asynchronous
  Restart required No
```

Stderr: empty.

Exit: 0. Clearing an absent pin also succeeds. No live acknowledgement is implied.

### Failure

Invocation: `cq claude proxy pin clear`. Stdout: empty.

Stderr:

```text
Error · Routing operation failed: cannot read routing configuration.
  Code: routing_io_failed
  Command: cq claude proxy pin clear
```

Exit: 1.

### Timeout

Invocation: `cq claude proxy pin clear --timeout 1ms`. Stdout: empty.

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq claude proxy pin clear

Inspect saved state
  cq claude proxy pin show
```

Exit: 7. No receipt was returned. Do not claim that the old pin remains in place.

## `cq claude proxy pin set`

### Help

Invocation: `cq claude proxy pin set --help`. Stdout:

```text
cq claude proxy pin set
Pin Claude proxy routing to one known account.

Usage
  cq claude proxy pin set ACCOUNT [options]

Saves configuration; a running proxy applies it asynchronously without a restart.
Does not change the native client default or rewrite existing session bindings.

Arguments
  ACCOUNT  Unique known Claude email from cq claude account list.
           Must be non-empty and match exactly one account. UUIDs are not accepted.

Options
  --timeout DURATION  Total work deadline, including preparation and requests.
                      Default: 10s. Positive Go duration; bare numbers invalid.
                      A timed-out write may have committed: inspect state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude proxy pin
  cq proxy

Examples
  Pin Claude proxy routing to one known account.
    cq claude proxy pin set alex@example.test
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude proxy pin set alex@example.test`. Stdout:

```text
Claude proxy pin · Configured
  Account          alex@example.test
  Application      Hot reload · asynchronous
  Restart required No
```

Stderr: empty.

Exit: 0.

### Failure

Invocation: `cq claude proxy pin set sam@example.test`. Stdout: empty.

Stderr:

```text
Error · Account not found: sam@example.test.
  Code: routing_account_not_found
  Command: cq claude proxy pin set
```

Exit: 3.

### Timeout

Invocation: `cq claude proxy pin set alex@example.test --timeout 1ms`. Stdout: empty.

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq claude proxy pin set

Inspect saved state
  cq claude proxy pin show
```

Exit: 7. No receipt was returned. Do not claim that the old pin remains in place.

## `cq claude proxy pin show`

### Help

Invocation: `cq claude proxy pin show --help`. Stdout:

```text
cq claude proxy pin show
Show the saved Claude proxy account pin.

Usage
  cq claude proxy pin show [options]

Displays saved configuration only; does not prove live proxy acknowledgement.

Options
  --timeout DURATION  Total work deadline, including preparation and requests.
                      Default: 10s. Positive Go duration; bare numbers invalid.
                      A timed-out write may have committed: inspect state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq claude account list
  cq claude proxy pin
  cq proxy

Examples
  Show the saved Claude proxy account pin.
    cq claude proxy pin show
```

Stderr: empty.

Exit: 0.

### Success

Invocation: `cq claude proxy pin show`. Stdout:

```text
Claude proxy pin · Configured
  Account          alex@example.test
  Application      Saved configuration only
  Restart required No
```

Stderr: empty.

Exit: 0.

### No pin configured

Invocation: `cq claude proxy pin show`. Stdout:

```text
Claude proxy pin · Not configured
  Account          none
  Application      Saved configuration only
  Restart required No
```

Stderr: empty.

Exit: 0. Clearing an absent pin also succeeds. No live acknowledgement is implied.

### Failure

Invocation: `cq claude proxy pin show`. Stdout: empty.

Stderr:

```text
Error · Routing operation failed: cannot read routing configuration.
  Code: routing_io_failed
  Command: cq claude proxy pin show
```

Exit: 1.

## `cq models list`

### Help

Invocation: `cq models list --help`. Stdout:

```text
cq models list
List active models from the local merged registry.

Usage
  cq models list [options]

Reads cached native data and overlays without refreshing sources.
An empty registry succeeds. Native entries win over same-provider overlays.

Options
  --provider PROVIDER  Filter by claude or codex. Omit to list both.
                       Legacy anthropic means claude and emits a deprecation warning.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq models refresh
  cq models overlay

Examples
  Inspect all cached models
    cq models list
  Inspect Codex model metadata as JSON
    cq models list --provider codex --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq models list --provider codex`. Stdout:

```text
Models · 1 · Codex

  example-code-1 · Example Code 1
    Provider          Codex
    Source            Native
    Aliases           example-code
    Description       Fictional coding model for presentation fixtures
    Context           200,000 tokens
    Maximum context   200,000 tokens
    Maximum output    16,000 tokens
    Visibility        list
    Priority          1
    Explicit clone    none
    Metadata source   none
```

Stderr: empty. Exit: 0.

### Empty

Invocation: `cq models list`. Stdout:

```text
Models · 0
  No cached models.

Fetch model data
  cq models refresh
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq models list`. Stdout: empty. Stderr:

```text
Error · Cannot access model registry state: invalid local registry data.
  Code: models_store_failed
  Command: cq models list
```

Exit: 1.

## `cq models overlay add`

### Help

Invocation: `cq models overlay add --help`. Stdout:

```text
cq models overlay add
Add or replace one local model overlay and publish caches.

Usage
  cq models overlay add --provider PROVIDER --id ID [options]

Replaces an overlay with the same provider and exact ID. Does not create an upstream
model or prove availability. Validation precedes saving. Publication failure leaves
an already saved overlay in place; no implicit rollback.
No overall deadline: proxy refresh 5s; local source refresh 30s; HTTP requests 30s;
legacy health probe 2s. File publication has no separate deadline.

Options
  --provider PROVIDER  Required: claude or codex. Legacy anthropic means claude
                       with a deprecation warning.
  --id ID              Required exact, case-sensitive model ID. Non-empty UTF-8;
                       no outer whitespace or control characters.
  --clone-from ID      Copy missing metadata from this same-provider native ID.
                       Case-sensitive; non-empty UTF-8; no outer whitespace or controls.
                       Missing explicit source fails before saving. Omit to infer by
                       model-name similarity; no match leaves missing metadata unset.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq models list
  cq models refresh
  cq models overlay remove

Examples
  Clone metadata from a verified native model in this fictional fixture
    cq models overlay add --provider codex --id example-code-2 --clone-from example-code-1
  Infer metadata from cached names
    cq models overlay add --provider claude --id example-sonnet-next
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq models overlay add --provider codex --id example-code-2 --clone-from example-code-1`. Stdout:

```text
Model overlay · Saved
  Provider          Codex
  ID                example-code-2
  Display name      Example Code 1
  Source            Overlay
  Aliases           none
  Description       Fictional coding model for presentation fixtures
  Context           200,000 tokens
  Maximum context   200,000 tokens
  Maximum output    16,000 tokens
  Visibility        list
  Priority          1
  Explicit clone    example-code-1
  Metadata source   example-code-1 · Explicit

Publication · Complete · via local
  Models: 2 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Written · Published
    /Users/alex/.codex/models_cache.json
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr: empty. Exit: 0. Cache paths are fictional resolved resource values, not discovery rules.

### Failure before saving

Invocation: `cq models overlay add --provider codex --id example-code-2 --clone-from missing-code`. Stdout: empty. Stderr:

```text
Error · Native clone source codex/missing-code was not found;
  run cq models refresh and cq models list --provider codex.
  Code: models_clone_not_found
  Command: cq models overlay add
```

Exit: 3.

### Saved overlay, partial publication

Invocation: `cq models overlay add --provider codex --id example-code-2 --clone-from example-code-1`. Stdout:

```text
Model overlay · Saved · Publication incomplete
  Provider          Codex
  ID                example-code-2
  Display name      Example Code 1
  Source            Overlay
  Aliases           none
  Description       Fictional coding model for presentation fixtures
  Context           200,000 tokens
  Maximum context   200,000 tokens
  Maximum output    16,000 tokens
  Visibility        list
  Priority          1
  Explicit clone    example-code-1
  Metadata source   example-code-1 · Explicit

Publication · Partial · via local
  Models: 2 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Failed · Write failed
    /Users/alex/.codex/models_cache.json
    models_store_failed: Cannot write model cache.
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr:

```text
Warning · Model refresh completed partially; inspect source and publication results.
  Code: models_refresh_partial
  Command: cq models overlay add
```

Exit: 8.

## `cq models overlay prune`

### Help

Invocation: `cq models overlay prune --help`. Stdout:

```text
cq models overlay prune
Remove overlays now supplied by native model sources.

Usage
  cq models overlay prune [options]

Refreshes sources and publishes caches, then removes overlays whose provider and
exact ID are positively present in refreshed native data. Applies to both providers;
there is no --provider filter. Failed sources never justify deleting their overlays.
Repeating with unchanged sources removes zero entries.
No overall deadline: proxy refresh 5s; local source refresh 30s; HTTP requests 30s;
legacy health probe 2s. File publication and pruning have no separate deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq models list
  cq models overlay remove

Examples
  Prune redundant overlays across both providers
    cq models overlay prune
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq models overlay prune`. Stdout:

```text
Model overlays · Pruned 1
  Codex  example-code-2

Publication · Complete · via proxy
  Models: 2 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              2               0

Client caches
  Codex cache · Written · Published
    /Users/alex/.codex/models_cache.json
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr: empty. Exit: 0.

### No-op

Invocation: `cq models overlay prune`. Stdout:

```text
Model overlays · Pruned 0
  No redundant overlays.

Publication · Complete · via proxy
  Models: 2 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              2               0

Client caches
  Codex cache · Written · Published
    /Users/alex/.codex/models_cache.json
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq models overlay prune`. Stdout: empty. Stderr:

```text
Error · Cannot access model registry state: cannot read overlays.
  Code: models_store_failed
  Command: cq models overlay prune
```

Exit: 1.

## `cq models overlay remove`

### Help

Invocation: `cq models overlay remove --help`. Stdout:

```text
cq models overlay remove
Remove one local model overlay and publish caches.

Usage
  cq models overlay remove --provider PROVIDER --id ID [options]

Requires an existing overlay. Native entries remain available. Removal commits before
publication; a later publication failure does not restore the overlay.
No overall deadline: proxy refresh 5s; local source refresh 30s; HTTP requests 30s;
legacy health probe 2s. File publication has no separate deadline.

Options
  --provider PROVIDER  Required: claude or codex. Legacy anthropic means claude
                       with a deprecation warning.
  --id ID              Required exact, case-sensitive model ID. Non-empty UTF-8;
                       no outer whitespace or control characters.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq models list
  cq models overlay add
  cq models overlay prune

Examples
  Remove the known fictional overlay
    cq models overlay remove --provider codex --id example-code-2
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq models overlay remove --provider codex --id example-code-2`. Stdout:

```text
Model overlay · Removed
  Provider  Codex
  ID        example-code-2

Publication · Complete · via local
  Models: 1 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Written · Published
    /Users/alex/.codex/models_cache.json
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq models overlay remove --provider codex --id missing-code`. Stdout: empty. Stderr:

```text
Error · Overlay codex/missing-code was not found.
  Code: models_overlay_not_found
  Command: cq models overlay remove
```

Exit: 3.

### Removed overlay, partial publication

Invocation: `cq models overlay remove --provider codex --id example-code-2`. Stdout:

```text
Model overlay · Removed · Publication incomplete
  Provider  Codex
  ID        example-code-2

Publication · Partial · via local
  Models: 1 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Failed · Write failed
    /Users/alex/.codex/models_cache.json
    models_store_failed: Cannot write model cache.
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr:

```text
Warning · Model refresh completed partially; inspect source and publication results.
  Code: models_refresh_partial
  Command: cq models overlay remove
```

Exit: 8.

## `cq models refresh`

### Help

Invocation: `cq models refresh --help`. Stdout:

```text
cq models refresh
Refresh model sources and publish client model caches.

Usage
  cq models refresh [options]

Merges native models with overlays and validates routing ownership before publication.
Uses the healthy proxy registry, otherwise local refresh. A reachable proxy error is
reported rather than retried through another owner. Missing optional client files are
reported as skipped. No service is installed or restarted.
No overall deadline: proxy refresh 5s; local source refresh 30s; HTTP requests 30s;
legacy health probe 2s. File publication has no separate deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq models list
  cq models overlay

Examples
  Refresh and publish model caches
    cq models refresh
  Inspect source and publication results
    cq models refresh --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq models refresh`. Stdout:

```text
Publication · Complete · via proxy
  Models: 1 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Written · Published
    /Users/alex/.codex/models_cache.json
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq models refresh`. Stdout: empty. Stderr:

```text
Error · Model refresh failed: proxy registry refresh unavailable.
  Code: models_refresh_failed
  Command: cq models refresh
```

Exit: 1. The reachable proxy returned an error; no silent local retry is implied.

### Partial publication

Invocation: `cq models refresh`. Stdout:

```text
Publication · Partial · via proxy
  Models: 1 active

Sources
  Provider  Result     Native models  Malformed rows
  Claude    Refreshed              0               0
  Codex     Refreshed              1               0

Client caches
  Codex cache · Failed · Write failed
    /Users/alex/.codex/models_cache.json
    models_store_failed: Cannot write model cache.
  Claude capabilities · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-capabilities.json
  Claude picker · Skipped · Optional client absent
    /Users/alex/.claude/cache/model-picker.json
```

Stderr:

```text
Warning · Model refresh completed partially; inspect source and publication results.
  Code: models_refresh_partial
  Command: cq models refresh
```

Exit: 8.

## `cq completion`

### Help

Invocation: `cq completion --help`. Stdout:

```text
cq completion
Print a shell completion script.

Usage
  cq completion SHELL [options]

Prints raw executable shell text, with no heading, colour or installation action.
With --json, emits the documented envelope containing the shell and complete script.
Does not read credentials or provider configuration.

Arguments
  SHELL  Required: bash, zsh or fish. Exact lowercase spelling.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq help

Examples
  Save a Bash completion script for review
    cq completion bash > cq-completion.bash
  Print a Zsh completion script
    cq completion zsh
  Print a Fish completion script
    cq completion fish
```

Stderr: empty. Exit: 0.

### Success — Bash raw protocol

Invocation: `cq completion bash`. Stdout: the complete bytes in [raw/completion.bash](raw/completion.bash), including its final LF. Stderr: empty. Exit: 0.

SHA-256: `50f273c59fc9bccd10a505b291685ff1629b71712427c253f15dd0be84e07c44`; 99,960 bytes.

### Success — Zsh raw protocol

Invocation: `cq completion zsh`. Stdout: the complete bytes in [raw/completion.zsh](raw/completion.zsh), including its final LF. Stderr: empty. Exit: 0.

SHA-256: `9c52d54e5c76adba20ef387c7f1ae877dbd4a941406ccdb8e5ad7348230f540a`; 99,266 bytes.

### Success — Fish raw protocol

Invocation: `cq completion fish`. Stdout: the complete bytes in [raw/completion.fish](raw/completion.fish), including its final LF. Stderr: empty. Exit: 0.

SHA-256: `67156bd91c9d2baf15b429e164f32004abfde88c479b25e2bb6af5c977c1e0c3`; 91,707 bytes.

These three fixtures are a deliberate exception to hand-authored human examples: they copy the complete `completionByShell` string values from `internal/cli/catalogue_gen.go`, without executing a shell, reading state or changing the protocol. Do not colour or wrap their contents.

### Failure

Invocation: `cq completion --shell bash`. Stdout: empty. Stderr:

```text
Error · Unknown option: --shell. Run cq completion --help.
  Code: unknown_option
  Command: cq completion
```

Exit: 2. `SHELL` is positional; this fails before configuration access.

## `cq gemini account show`

### Help

Invocation: `cq gemini account show --help`. Stdout:

```text
cq gemini account show
Show the externally managed Gemini account.

Usage
  cq gemini account show [options]

Inspects the single Antigravity-managed identity locally. Configured does not mean
credentials are valid. No login, activation, removal or quota request occurs.
An absent account is a successful inspection; configure authentication in Antigravity.

Options
  --timeout DURATION  Total inspection deadline. Default: 30s; range: 1s to 10m.
                      Go duration syntax; bare numbers are invalid.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq check

Examples
  Inspect the external identity
    cq gemini account show
  Inspect local configuration as JSON
    cq gemini account show --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq gemini account show`. Stdout:

```text
Gemini account · Configured
  alex@example.test

  Reference       unknown
  Account ID      gemini_alex_01
  Email           alex@example.test
  Native default  Active
  Identity        Stable
  Sources         external
  Aliases         none
  Tier            unknown

Managed by Antigravity · credentials not validated by this command
```

Stderr: empty. Exit: 0.

### Not configured

Invocation: `cq gemini account show`. Stdout:

```text
Gemini account · Not configured
  Set up authentication in Antigravity.
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq gemini account show`. Stdout: empty. Stderr:

```text
Error · Antigravity account configuration could not be read.
  Code: gemini_account_unavailable
  Command: cq gemini account show
```

Exit: 4.

## `cq help`

### Help

Invocation: `cq help --help`. Stdout:

```text
cq help
Show help for a command path.

Usage
  cq help [COMMAND…] [options]

Resolves an exact complete group or command path without reading state.
Omit COMMAND for root help. Recognised legacy paths show canonical help.
Help remains human text even with --json.

Arguments
  COMMAND  Zero or more command words, in order. Unknown children are errors.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq completion

Examples
  Explore shared proxy commands
    cq help proxy
  Inspect Codex reserve controls
    cq help codex proxy reserve
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq help version`. Stdout:

```text
cq version
Show CQ build identity.

Usage
  cq version [options]

Reads embedded build metadata only. Does not open user configuration.
Unknown provenance is labelled unknown, not clean.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq help

Examples
  Inspect the installed build
    cq version
  Read build metadata as JSON
    cq version --json
```

Stderr: empty. Exit: 0. This body exactly matches the `cq version --help` fixture below.

### Failure

Invocation: `cq help widgets`. Stdout: empty. Stderr:

```text
Error · Unknown command: widgets. Run cq help.
  Code: unknown_command
  Command: cq help
```

Exit: 2.

## `cq version`

### Help

Invocation: `cq version --help`. Stdout:

```text
cq version
Show CQ build identity.

Usage
  cq version [options]

Reads embedded build metadata only. Does not open user configuration.
Unknown provenance is labelled unknown, not clean.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  All options are single-use. Boolean flags default false; accept bare or =true/=false.

Related commands
  cq help

Examples
  Inspect the installed build
    cq version
  Read build metadata as JSON
    cq version --json
```

Stderr: empty. Exit: 0.

### Success

Invocation: `cq version`. Stdout:

```text
CQ 1.0.0
  Revision    0123456789abcdef0123456789abcdef01234567
  Source      Clean
  CLI schema  2
```

Stderr: empty. Exit: 0. Revision is a fictional 40-character hexadecimal fixture.

### Unknown provenance

Invocation: `cq version`. Stdout:

```text
CQ dev
  Revision    unknown
  Source      unknown
  CLI schema  2
```

Stderr: empty. Exit: 0.

### Failure

Invocation: `cq version --verbose`. Stdout: empty. Stderr:

```text
Error · Unknown option: --verbose. Run cq version --help.
  Code: unknown_option
  Command: cq version
```

Exit: 2. There is no provider, network or state failure for valid version invocations.
