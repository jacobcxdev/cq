# CQ operational output examples

Hand-authored output examples for all 89 commands, collected without their help pages. These are proposed terminal designs using fictional data. Each scenario records invocation, output streams and exit status. Edit the owning accounts/routing/runtime document, then rebuild this collection.

## `cq auth refresh`

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

Invocation: `cq claude proxy pin clear --timeout 1ms`. The save returned before the deadline, then observation timed out.

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq claude proxy pin clear

Inspect saved state
  cq claude proxy pin show
```

Stdout:

```text
Claude proxy pin · Not configured
  Account          none
  Application      Hot reload · asynchronous
  Restart required No
  Live application unconfirmed
```

Exit: 7. The saved selector is known; live application remains unconfirmed.



## `cq claude proxy pin set`

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

Invocation: `cq claude proxy pin set alex@example.test --timeout 1ms`. The save returned before the deadline, then observation timed out.

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq claude proxy pin set

Inspect saved state
  cq claude proxy pin show
```

Stdout:

```text
Claude proxy pin · Configured
  Account          alex@example.test
  Application      Hot reload · asynchronous
  Restart required No
  Live application unconfirmed
```

Exit: 7. The saved selector is known; live application remains unconfirmed.



## `cq claude proxy pin show`

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


## `cq codex proxy fallback clear`

### Success

Invocation: `cq codex proxy fallback clear`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · already absent

No absent-versus-removed field exists; the receipt does not invent one.

Invocation: `cq codex proxy fallback clear`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy fallback clear`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy fallback clear
```

Exit: `1`.


### Timeout · no receipt

Invocation: `cq codex proxy fallback clear --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy fallback clear
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy fallback set`

### Success

Invocation: `cq codex proxy fallback set alex@example.test`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · same resolved account

The existing selection already contains acct-alex. The receipt remains configured-only.

Invocation: `cq codex proxy fallback set alex@example.test`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy fallback set alex@example.test`

Stdout: empty.

Stderr:

```text
Error · Account reference is ambiguous; use a unique alias or AccountKey.
  Code: routing_account_ambiguous
  Command: cq codex proxy fallback set
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy fallback set alex@example.test --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy fallback set
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy fallback show`

### Success

Invocation: `cq codex proxy fallback show`

Stdout:

```text
Codex fallback · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked
```

Stderr: empty.

Exit: `0`.


### Empty · no configured fallback

Invocation: `cq codex proxy fallback show`

Stdout:

```text
Codex fallback · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy fallback show`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy fallback show
```

Exit: `1`.


### Timeout · observation unavailable

Invocation: `cq codex proxy fallback show --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy fallback show
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy pin clear`

### Success

Invocation: `cq codex proxy pin clear`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · already absent

No absent-versus-removed field exists; the receipt does not invent one.

Invocation: `cq codex proxy pin clear`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pin clear`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy pin clear
```

Exit: `1`.


### Timeout · no receipt

Invocation: `cq codex proxy pin clear --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pin clear
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy pin set`

### Success

Invocation: `cq codex proxy pin set alex@example.test`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · same resolved account

The existing selection already contains acct-alex. The receipt remains configured-only.

Invocation: `cq codex proxy pin set alex@example.test`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pin set alex@example.test`

Stdout: empty.

Stderr:

```text
Error · Account reference is ambiguous; use a unique alias or AccountKey.
  Code: routing_account_ambiguous
  Command: cq codex proxy pin set
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy pin set alex@example.test --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pin set
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy pin show`

### Success

Invocation: `cq codex proxy pin show`

Stdout:

```text
Codex pin · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked
```

Stderr: empty.

Exit: `0`.


### Empty · no configured pin

Invocation: `cq codex proxy pin show`

Stdout:

```text
Codex pin · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pin show`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy pin show
```

Exit: `1`.


### Timeout · observation unavailable

Invocation: `cq codex proxy pin show --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pin show
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy lease invalidate`

### Success

Invocation: `cq codex proxy lease invalidate`

Stdout:

```text
Codex leases invalidated
  Invalidated leases  1,240
  Journal generation  42

Active authority and required account continuity remain intact.
```

Stderr: empty.

Exit: `0`.


### No-op · no reusable affinity

Invocation: `cq codex proxy lease invalidate`

Stdout:

```text
Codex leases invalidated
  Invalidated leases  0
  Journal generation  43

Active authority and required account continuity remain intact.
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy lease invalidate`

Stdout: empty.

Stderr:

```text
Error · Lease journal is unavailable for invalidation.
  Code: lease_journal_unavailable
  Command: cq codex proxy lease invalidate
```

Exit: `4`.


### Timeout · no receipt

Invocation: `cq codex proxy lease invalidate --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy lease invalidate
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy policy apply`

### Success

Invocation: `cq codex proxy policy apply --file /Users/alex/cq-policy.json`

Stdout:

```text
Saved Codex routing policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy policy apply --file /Users/alex/cq-policy.json`

Stdout: empty.

Stderr:

```text
Error · Policy generations conflict; show current policy and prepare a fresh document.
  Code: policy_generation_conflict
  Command: cq codex proxy policy apply
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy policy apply --file /Users/alex/cq-policy.json --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy policy apply
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy policy show`

### Success

Invocation: `cq codex proxy policy show`

Stdout:

```text
Codex routing policy
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Empty · published policy without collections

Invocation: `cq codex proxy policy show`

Stdout:

```text
Codex routing policy
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  none

Session bindings
  none

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Offline · application not checked

Invocation: `cq codex proxy policy show --state-dir /Users/alex/cq-state`

Stdout:

```text
Codex routing policy
  Authority generation  12
  Routing generation    18
  Effective generation  17
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: routing generation not yet effective
Source: offline policy state; live application not checked
```

Stderr: empty.

Exit: `0`.


### Success · retained capability and delegation evidence

Recorded evidence is fictional. Supported/eligible refer to recorded capability facts, not overall health or a fresh probe. Effective generation 17 lags requested routing generation 18.

Invocation: `cq codex proxy policy show`

Stdout:

```text
Codex routing policy
  Authority generation  12
  Routing generation    18
  Effective generation  17
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  Account key  State
  acct-alex    supported
  acct-sam     unknown

Capability routing
  Pool  work
  Predicates
    Schema 1; capability program-access
    Product surface codex; access path responses; authentication mode oauth
    Requested model gpt-5.4; effective model *
  Evidence
    Schema 1; account acct-alex
    Account HMAC 89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    Workspace example-workspace
    Capability program-access; product surface codex
    Access path responses; authentication mode oauth
    Requested model gpt-5.4; effective model gpt-5.4
    Source authenticated-probe; state eligible; authenticated yes
    Observed 4m 59s ago · 23 Sep 2026, 16:25:00.125 BST (UTC+01:00)
    Expires in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
    Routing generation 18

Delegations
  Caller example-runner
  Accounts acct-alex
           acct-sam
  Expires in 1h 30m · 23 Sep 2026, 18:00 BST (UTC+01:00)

Application: routing generation not yet effective
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy policy show`

Stdout: empty.

Stderr:

```text
Error · Running CQ proxy control is unavailable.
  Code: routing_control_unavailable
  Command: cq codex proxy policy show
```

Exit: `4`.


### Timeout · observation unavailable

Invocation: `cq codex proxy policy show --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy policy show
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy pool rename`

### Success

Invocation: `cq codex proxy pool rename work primary`

Stdout:

```text
Saved Codex pool policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  primary  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    primary

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pool rename work primary`

Stdout: empty.

Stderr:

```text
Error · Pool name already exists: primary.
  Code: pool_name_conflict
  Command: cq codex proxy pool rename
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy pool rename work primary --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pool rename
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy pool set`

### Success

Invocation: `cq codex proxy pool set work --account alex@example.test --account sam@example.test --value 10`

Stdout:

```text
Saved Codex pool policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### No-op · equivalent policy

Membership/value already match. Do not infer a changed-count from the complete policy receipt.

Invocation: `cq codex proxy pool set work --account alex@example.test --account sam@example.test --value 10`

Stdout:

```text
Saved Codex pool policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pool set work --account alex@example.test --account sam@example.test --value 10`

Stdout: empty.

Stderr:

```text
Error · Account reference is ambiguous; use a unique alias or AccountKey.
  Code: routing_account_ambiguous
  Command: cq codex proxy pool set
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy pool set work --account alex@example.test --account sam@example.test --value 10 --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pool set
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy pool value`

### Success

Invocation: `cq codex proxy pool value work 20`

Stdout:

```text
Saved Codex pool policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  20     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### No-op · equivalent policy

Membership/value already match. Do not infer a changed-count from the complete policy receipt.

Invocation: `cq codex proxy pool value work 20`

Stdout:

```text
Saved Codex pool policy.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  20     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy pool value work 20`

Stdout: empty.

Stderr:

```text
Error · Pool not found: work.
  Code: pool_not_found
  Command: cq codex proxy pool value
```

Exit: `3`.


### Timeout · no receipt

Invocation: `cq codex proxy pool value work 20 --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pool value
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy prime disable`

### Success

Invocation: `cq codex proxy prime disable`

Stdout:

```text
Saved Codex priming configuration.
Codex priming · configured disabled
  Application  Live process acknowledgement not checked

Model overrides
  Window       Model
  gpt-reserve  gpt-5.4
Source: saved configuration

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · same configured state

The setting already matches. The operation still does not acknowledge the live process.

Invocation: `cq codex proxy prime disable`

Stdout:

```text
Saved Codex priming configuration.
Codex priming · configured disabled
  Application  Live process acknowledgement not checked

Model overrides
  Window       Model
  gpt-reserve  gpt-5.4
Source: saved configuration

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy prime disable`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy prime disable
```

Exit: `1`.


### Timeout · no receipt

Invocation: `cq codex proxy prime disable --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy prime disable
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy prime enable`

### Success

Invocation: `cq codex proxy prime enable`

Stdout:

```text
Saved Codex priming configuration.
Codex priming · configured enabled
  Application  Live process acknowledgement not checked

Model overrides
  Window       Model
  gpt-reserve  gpt-5.4
Source: saved configuration

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### No-op · same configured state

The setting already matches. The operation still does not acknowledge the live process.

Invocation: `cq codex proxy prime enable`

Stdout:

```text
Saved Codex priming configuration.
Codex priming · configured enabled
  Application  Live process acknowledgement not checked

Model overrides
  Window       Model
  gpt-reserve  gpt-5.4
Source: saved configuration

Restart required: yes
Next: cq service restart --component proxy
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy prime enable`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy prime enable
```

Exit: `1`.


### Timeout · no receipt

Invocation: `cq codex proxy prime enable --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy prime enable
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy prime status`

### Success

Invocation: `cq codex proxy prime status`

Stdout:

```text
Codex priming · configured enabled
  Application  Live process acknowledgement not checked

Model overrides
  Window       Model
  gpt-reserve  gpt-5.4
Source: saved configuration
```

Stderr: empty.

Exit: `0`.


### Empty · no model overrides

Invocation: `cq codex proxy prime status`

Stdout:

```text
Codex priming · configured disabled
  Application  Live process acknowledgement not checked

Model overrides: none
Source: saved configuration
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy prime status`

Stdout: empty.

Stderr:

```text
Error · Routing operation failed: load config.
  Code: routing_io_failed
  Command: cq codex proxy prime status
```

Exit: `1`.


### Timeout · observation unavailable

Invocation: `cq codex proxy prime status --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy prime status
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy readiness show`

### Success

Synthetic matching marker fixture. Schema, semantics and gate names follow the current source; artifact hashes and installed evidence are fictional.

Invocation: `cq codex proxy readiness show --client-build 0.146.0`

Stdout:

```text
Codex HTTP readiness · current
  Codex build  0.146.0
  Validated    5m ago · 23 Sep 2026, 16:25:00 BST (UTC+01:00)
  Evidence     Retained validation; no live health check

Validation identity
  Marker version  3
  CQ build  1.0.0
  Transport  http
  Parser schema  1
  Lease schema  3
  Semantics revision  http-conservative-routing-v3
  Client build  0.146.0
  Retry budget  1
  Fixture hash  618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  CQ executable SHA-256  2222222222222222222222222222222222222222222222222222222222222222
  Client executable SHA-256  3333333333333333333333333333333333333333333333333333333333333333
  Service kind  launchd
  Service identity SHA-256  4444444444444444444444444444444444444444444444444444444444444444
  Installed result  passed
  Completed gates
    admitted-no-migration
    deterministic-fallback
    exact-pre-admission-hard429-replay
    frozen-single-transform-envelope
    installed-listener
    terminal-default-once
    v2-journal-runtime
    warm-affinity
  Marker validated  23 Sep 2026, 16:25:00 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy readiness show --client-build 0.146.1`

Stdout: empty.

Stderr:

```text
Error · HTTP readiness evidence does not match the requested build.
  Code: readiness_stale
  Command: cq codex proxy readiness show
```

Exit: `6`.


### Empty · no marker

Invocation: `cq codex proxy readiness show --client-build 0.146.0`

Stdout: empty.

Stderr:

```text
Error · No HTTP readiness evidence is recorded.
  Code: readiness_missing
  Command: cq codex proxy readiness show
```

Exit: `3`.



## `cq codex proxy reserve clear`

### Success

Invocation: `cq codex proxy reserve clear`

Stdout:

```text
Removed system-account reserve configuration.
Codex reserve · not configured
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         not configured
  Protected      not configured
  Remaining      unknown
  Enabled        no
  Routing        not excluded by reserve
  Reason         none
  Reset          unknown
  Observed       unknown

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### No-op · already unconfigured

Invocation: `cq codex proxy reserve clear`

Stdout:

```text
Removed system-account reserve configuration.
Codex reserve · not configured
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         not configured
  Protected      not configured
  Remaining      unknown
  Enabled        no
  Routing        not excluded by reserve
  Reason         none
  Reset          unknown
  Observed       unknown

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve clear`

Stdout: empty.

Stderr:

```text
Error · Running CQ proxy control is unavailable.
  Code: routing_control_unavailable
  Command: cq codex proxy reserve clear
```

Exit: `4`.


### Timeout · no receipt

Invocation: `cq codex proxy reserve clear --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve clear
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy reserve disable`

### Success

Invocation: `cq codex proxy reserve disable`

Stdout:

```text
Temporarily bypassed system-account reserve.
Codex reserve · temporarily bypassed
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      12.5%
  Enabled        no
  Routing        not excluded by reserve
  Reason         bypassed until verified reset (disabled_until_reset)
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve disable`

Stdout: empty.

Stderr:

```text
Error · Fresh usage and reset evidence are required to disable the reserve.
  Code: reserve_evidence_required
  Command: cq codex proxy reserve disable
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy reserve disable --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve disable
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy reserve enable`

### Success

Invocation: `cq codex proxy reserve enable`

Stdout:

```text
Enabled system-account reserve protection.
Codex reserve · protection enabled
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      12.5%
  Enabled        yes
  Routing        not excluded by reserve
  Reason         none
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### No-op · already enabled

Invocation: `cq codex proxy reserve enable`

Stdout:

```text
Enabled system-account reserve protection.
Codex reserve · protection enabled
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      12.5%
  Enabled        yes
  Routing        not excluded by reserve
  Reason         none
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve enable`

Stdout: empty.

Stderr:

```text
Error · Reserve is not configured; use reserve set first.
  Code: reserve_not_configured
  Command: cq codex proxy reserve enable
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy reserve enable --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve enable
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy reserve set`

### Success

Invocation: `cq codex proxy reserve set --window 7d --percent 2.05`

Stdout:

```text
Saved system-account reserve.
Codex reserve · protection enabled
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      12.5%
  Enabled        yes
  Routing        not excluded by reserve
  Reason         none
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve set --window 7d --percent 2.05`

Stdout: empty.

Stderr:

```text
Error · Selected window is unavailable; inspect reserve windows.
  Code: reserve_window_unavailable
  Command: cq codex proxy reserve set
```

Exit: `6`.


### Timeout · no receipt

Invocation: `cq codex proxy reserve set --window 7d --percent 2.05 --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve set
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy reserve status`

### Success

Invocation: `cq codex proxy reserve status`

Stdout:

```text
Codex reserve · protection enabled
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      12.5%
  Enabled        yes
  Routing        not excluded by reserve
  Reason         none
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Threshold reached · valid observation

Invocation: `cq codex proxy reserve status`

Stdout:

```text
Codex reserve · excludes system account
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      2%
  Enabled        yes
  Routing        excluded by reserve
  Reason         protected threshold reached (reserve_reached)
  Reset          in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)
  Observed       5s ago · 23 Sep 2026, 16:29:55 BST (UTC+01:00)

System-account windows
  Window     7d
  Remaining  2%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Unavailable observation · fail-closed reserve

Exit 0 reports live reserve state. Yellow uncertainty and explicit exclusion must not become green health.

Invocation: `cq codex proxy reserve status`

Stdout:

```text
Codex reserve · excludes system account
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         7d
  Protected      2.05% of full window
  Remaining      unknown
  Enabled        yes
  Routing        excluded by reserve
  Reason         usage observation stale (usage_stale)
  Reset          unknown
  Observed       unknown

No system-account quota windows available.
```

Stderr: empty.

Exit: `0`.


### Empty · unconfigured reserve

Invocation: `cq codex proxy reserve status`

Stdout:

```text
Codex reserve · not configured
  Scope          Current system account only
  Account        alex@example.test
  Account key    acct-alex
  Window         not configured
  Protected      not configured
  Remaining      unknown
  Enabled        no
  Routing        not excluded by reserve
  Reason         none
  Reset          unknown
  Observed       unknown

System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve status`

Stdout: empty.

Stderr:

```text
Error · Running CQ proxy control is unavailable.
  Code: routing_control_unavailable
  Command: cq codex proxy reserve status
```

Exit: `4`.


### Timeout · observation unavailable

Invocation: `cq codex proxy reserve status --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve status
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy reserve windows`

### Success

Invocation: `cq codex proxy reserve windows`

Stdout:

```text
System-account windows
  Window     7d
  Remaining  12.5%
  Reset      in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)

  Window     7d:gpt-reserve
  Remaining  100%
  Reset      in 7d · 30 Sep 2026, 16:30 BST (UTC+01:00)
```

Stderr: empty.

Exit: `0`.


### Empty · no observed windows

Invocation: `cq codex proxy reserve windows`

Stdout:

```text
No system-account quota windows available.
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy reserve windows`

Stdout: empty.

Stderr:

```text
Error · Running CQ proxy control is unavailable.
  Code: routing_control_unavailable
  Command: cq codex proxy reserve windows
```

Exit: `4`.


### Timeout · observation unavailable

Invocation: `cq codex proxy reserve windows --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy reserve windows
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy session bind`

### Success

Invocation: `cq codex proxy session bind --pool work --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout:

```text
Saved Codex session binding.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### No-op · same pool binding

The complete acknowledged policy remains the receipt; no invented changed-count.

Invocation: `cq codex proxy session bind --pool work --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout:

```text
Saved Codex session binding.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy session bind --pool work --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout: empty.

Stderr:

```text
Error · Pool not found: work.
  Code: pool_not_found
  Command: cq codex proxy session bind
```

Exit: `3`.


### Timeout · no receipt

Invocation: `cq codex proxy session bind --pool work --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy session bind
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy session digest`

### Success

Invocation: `cq codex proxy session digest --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout:

```text
Codex session digest
  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `printf %s example-session | cq codex proxy session digest --session-id-stdin`

Stdout: empty.

Stderr:

```text
Error · Local proxy authentication failed.
  Code: routing_auth_failed
  Command: cq codex proxy session digest
```

Exit: `5`.


### Timeout · observation unavailable

Invocation: `printf %s example-session | cq codex proxy session digest --session-id-stdin --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy session digest
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy session list`

### Success

Invocation: `cq codex proxy session list`

Stdout:

```text
Codex session bindings
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work
```

Stderr: empty.

Exit: `0`.


### Empty · no bindings

Invocation: `cq codex proxy session list`

Stdout:

```text
No Codex session bindings configured.
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy session list`

Stdout: empty.

Stderr:

```text
Error · Running CQ proxy control is unavailable.
  Code: routing_control_unavailable
  Command: cq codex proxy session list
```

Exit: `4`.


### Timeout · observation unavailable

Invocation: `cq codex proxy session list --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy session list
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy session show`

### Success

Invocation: `cq codex proxy session show --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout:

```text
Codex session binding
  Digest  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Pool    work
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy session show --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout: empty.

Stderr:

```text
Error · Session binding not found.
  Code: session_binding_not_found
  Command: cq codex proxy session show
```

Exit: `3`.


### Timeout · observation unavailable

Invocation: `cq codex proxy session show --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy session show
  Result: unavailable
```

Exit: `7`.




## `cq codex proxy session unbind`

### Success

Invocation: `cq codex proxy session unbind --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout:

```text
Removed Codex session binding.
  Authority generation  12
  Routing generation    18
  Effective generation  18
  Schema                1

Pools
  Name  Value  Members
  work  10     acct-alex
               acct-sam

Session bindings
  none

Capability account evidence
  none

Capability routing
  Pool        none
  Predicates  none
  Evidence    none

Delegations
  none

Application: generations match
Source: authenticated proxy control
```

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy session unbind --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`

Stdout: empty.

Stderr:

```text
Error · Session binding not found.
  Code: session_binding_not_found
  Command: cq codex proxy session unbind
```

Exit: `3`.


### Timeout · no receipt

Invocation: `cq codex proxy session unbind --digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --timeout 1ms`

Stdout: empty.

Stderr:

```text
Error · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy session unbind
  Result: unconfirmed
```

Exit: `7`.



## `cq codex proxy trace`

### Success

Frozen human trace format: RFC3339 timestamps, no title, no colour and no completion footer.

Invocation: `cq codex proxy trace --since 15m --tail 50`

Stdout:

```text
2026-09-23T15:29:45Z trace-demo-01 #1 http route_summary success acct-alex attempt=1 status=200 pool=work
```

Stderr: empty.

Exit: `0`.


### Empty · configured log has no matching records

Invocation: `cq codex proxy trace --trace trace-missing`

Stdout: empty.

Stderr: empty.

Exit: `0`.


### Failure

Invocation: `cq codex proxy trace --payload`

Stdout: empty.

Stderr:

```text
Error · Selected trace log is not configured.
  Code: trace_not_configured
  Command: cq codex proxy trace
```

Exit: `4`.


### Partial history · retained rotation gap

One emitted record remains evidence; it is not a claim of complete history.

Invocation: `cq codex proxy trace --follow`

Stdout:

```text
2026-09-23T15:29:45Z trace-demo-01 #1 http route_summary success acct-alex attempt=1 status=200 pool=work
```

Stderr:

```text
Warning · Trace history has a gap: diagnostics log rotated beyond retained history while following.
  Code: trace_history_gap
  Command: cq codex proxy trace
```

Exit: `8`.


### Timeout · records already emitted

Invocation: `cq codex proxy trace --follow --timeout 10s`

Stdout:

```text
2026-09-23T15:29:45Z trace-demo-01 #1 http route_summary success acct-alex attempt=1 status=200 pool=work
```

Stderr:

```text
Error · Trace reading timed out.
  Code: trace_timeout
  Command: cq codex proxy trace
```

Exit: `7`.


### Interrupted · records already emitted

The user interrupts the follow operation after the shown record. No synthetic completion footer.

Invocation: `cq codex proxy trace --follow`

Stdout:

```text
2026-09-23T15:29:45Z trace-demo-01 #1 http route_summary success acct-alex attempt=1 status=200 pool=work
```

Stderr:

```text
Error · Trace following interrupted.
  Code: trace_interrupted
  Command: cq codex proxy trace
```

Exit: `130`.



## `cq codex proxy canary start`

### Success

Invocation:

```sh
cq codex proxy canary start
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last observed day  unknown
  Ended              —

Observations
  Admitted turns             0
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  0

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy canary start
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary preconditions are not satisfied.
  Code: canary_precondition_failed
  Command: cq codex proxy canary start
```

Exit: 6.


### Failure after publication

A directory-sync failure may occur after replacement publication. Empty stdout does not prove rollback. Inspect the retained run before retrying.

Invocation:

```sh
cq codex proxy canary start
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary state is unavailable.
  Code: canary_state_unavailable
  Command: cq codex proxy canary start
```

Exit: 1.



## `cq codex proxy canary status`

### Success

Invocation:

```sh
cq codex proxy canary status
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            16h ago · 23 Sep 2026, 00:30:00 BST (UTC+01:00)
  Last observed day  15h 30m ago · 23 Sep 2026, 01:00:00 BST (UTC+01:00)
  Ended              —

Observations
  Admitted turns             1,248
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  1

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy canary status
```

Stdout: empty.

Stderr:

```text
Error · No Codex canary is recorded.
  Code: canary_missing
  Command: cq codex proxy canary status
```

Exit: 3.



## `cq codex proxy canary stop`

### Success · stop requested, still active

Invocation:

```sh
cq codex proxy canary stop
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            16h ago · 23 Sep 2026, 00:30:00 BST (UTC+01:00)
  Last observed day  15h 30m ago · 23 Sep 2026, 01:00:00 BST (UTC+01:00)
  Ended              —

Observations
  Admitted turns             1,248
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  1

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.

Stop requested; drain completion has not been observed.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy canary stop
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary state is unavailable.
  Code: canary_state_unavailable
  Command: cq codex proxy canary stop
```

Exit: 1.



## `cq codex proxy credential-endpoint legacy activate`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · activated
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy activate
```

Exit: 6.


### Timeout · inspect before continuing

No result was returned. The journal may contain an effect; timeout is not rollback proof.

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint operation timed out; inspect migration state before continuing.
  Code: endpoint_timeout
  Command: cq codex proxy credential-endpoint legacy activate
```

Exit: 7.



## `cq codex proxy credential-endpoint legacy finalise`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy --non-interactive
```

Stdout:

```text
Credential endpoint · committed
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy finalise
```

Exit: 6.


### Confirmation · pending

Interactive prompt before mutation; input has not yet been supplied. The actual consent phrase remains unchanged.

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy
```

Stdout: empty.

Stderr:

```text
Finalise credential endpoint replacement
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Ticket  0123456789abcdef0123456789abcdef
  Effect  Permanently retire the rollback path after live-owner verification.

Type candidate-healthy to confirm the exact live candidate passed health verification: 
```

Exit: pending.



## `cq codex proxy credential-endpoint legacy inspect`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy inspect
```

Stdout:

```text
Credential endpoint · legacy_refused
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    snapshot
  Ticket  none

Identity observed.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy inspect
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint or transition does not exist.
  Code: endpoint_not_found
  Command: cq codex proxy credential-endpoint legacy inspect
```

Exit: 3.



## `cq codex proxy credential-endpoint legacy prepare`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file /Users/alex/cq-inputs/endpoint-snapshot.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · prepared
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file /Users/alex/cq-inputs/endpoint-snapshot.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy prepare
```

Exit: 6.



## `cq codex proxy credential-endpoint legacy resume`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · activated
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy resume
```

Exit: 6.



## `cq codex proxy credential-endpoint legacy rollback`

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · rolled_back
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy rollback
```

Exit: 6.


### Timeout · inspect before continuing

No result was returned. The journal may contain an effect; timeout is not rollback proof.

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint operation timed out; inspect migration state before continuing.
  Code: endpoint_timeout
  Command: cq codex proxy credential-endpoint legacy rollback
```

Exit: 7.



## `cq codex proxy fixture create`

### Success

Invocation:

```sh
cq codex proxy fixture create --input /Users/alex/cq-inputs/request.json --output /Users/alex/cq-inputs/fixture.json
```

Stdout:

```text
Fixture · created
  File               /Users/alex/cq-inputs/fixture.json
  Request kind       turn
  Input size         2,048 B
  Converted          0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Metadata source    nested
  Session hint       session:111122223333
  Thread hint        thread:444455556666
  Turn hint          turn:777788889999
  Previous response  no
  Encrypted state    no
  Body SHA-256       0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Schema             1
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy fixture create --input /Users/alex/cq-inputs/request.json --output /Users/alex/cq-inputs/fixture.json
```

Stdout: empty.

Stderr:

```text
Error · The output file already exists.
  Code: fixture_output_exists
  Command: cq codex proxy fixture create
```

Exit: 6.



## `cq codex proxy hook stop`

### Success · no receipt

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout:

```text
{}
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
printf '{}\n' | cq codex proxy hook stop
```

Stdout: empty.

Stderr:

```text
Error · Invalid Codex Stop hook input.
  Code: hook_input_invalid
  Command: cq codex proxy hook stop
```

Exit: 2.


### Failure · lookup deadline

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout: empty.

Stderr:

```text
Error · The Codex turn receipt lookup timed out.
  Code: hook_timeout
  Command: cq codex proxy hook stop
```

Exit: 7.



### Success · retained receipt protocol

The raw JSON string below is intentionally one line, even beyond 100 cells. Never wrap or colour it.

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout:

```text
{"systemMessage":"CQ route: completed via WebSocket; account codex:0123456789ab (actual); Sol/High; warm affinity. Shadow: no-affinity comparison agreed."}
```

Stderr: empty.

Exit: 0.



## `cq codex proxy validate http`

### Success

Invocation:

```sh
cq codex proxy validate http --port 29280
```

Stdout:

```text
HTTP validation · requested
  Candidate port  29280
  Completion      not yet known

Request accepted; validation has not completed.
Inspect retained readiness evidence before retrying.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy validate http --port 29280
```

Stdout: empty.

Stderr:

```text
Error · The installed candidate service is unavailable.
  Code: validation_candidate_unavailable
  Command: cq codex proxy validate http
```

Exit: 4.


### Timeout · completion unknown

Invocation:

```sh
cq codex proxy validate http --port 29280 --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The HTTP validation operation timed out; inspect readiness before retrying.
  Code: validation_timeout
  Command: cq codex proxy validate http
```

Exit: 7.



## `cq codex proxy validate websocket`

### Success

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.114.0
```

Stdout:

```text
WebSocket validation · passed
  Client build  0.114.0
  Validated     0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  CQ build      1.0.0
  Gates passed  9

Evidence
  Version             3
  Transport           websocket
  Parser schema       1
  Lease schema        3
  Semantics revision  websocket-terminating-routing-v1
  Retry budget        1
  Fixture SHA-256      858d90f3e827194cdfc0dbf5e11d35dfb25e7afc395cf547f17a30936889f9f9
  CQ executable       —
  Client executable   —
  Service identity    —
  Service kind        —
  Installed result    passed
  Completed gates
    admitted-no-migration
    canonical-terminal-error
    compression-subprotocol
    installed-isolated-client
    persistent-account-upstream
    portable-pre-admission-hard429-rotation
    same-account-candidate-auth-recovery
    strong-frame-authority
    upstream-generation-fence
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.113.0
```

Stdout: empty.

Stderr:

```text
Error · The Codex executable does not match --client-build.
  Code: validation_build_mismatch
  Command: cq codex proxy validate websocket
```

Exit: 6.


### Timeout · no readiness recorded

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.114.0 --timeout 30s
```

Stdout: empty.

Stderr:

```text
Error · WebSocket validation timed out; readiness was not recorded.
  Code: validation_timeout
  Command: cq codex proxy validate websocket
```

Exit: 7.



## `cq proxy candidate prepare`

### Success

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json
```

Stdout:

```text
Candidate · prepared
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      1
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate prepare
```

Exit: 6.


### Timeout · inspect retained candidate

No receipt was returned. A retained directory or pending effect is possible; do not assume no change.

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json --timeout 150s
```

Stdout: empty.

Stderr:

```text
Error · Candidate operation timed out; inspect candidate status before retrying.
  Code: candidate_timeout
  Command: cq proxy candidate prepare
```

Exit: 7.



## `cq proxy candidate receipt show`

### Success

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Candidate receipt · published
  Attempt           0123456789abcdef0123456789abcdef
  Receipt digest    5555555555555555555555555555555555555555555555555555555555555555
  Promotion digest  6666666666666666666666666666666666666666666666666666666666666666
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout: empty.

Stderr:

```text
Error · Candidate state or receipt does not exist.
  Code: candidate_not_found
  Command: cq proxy candidate receipt show
```

Exit: 3.


### Failure · retained conflicted receipt

Receipt came from an earlier qualified runtime. This build cannot create new qualification evidence.

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Candidate receipt · conflicted
  Attempt           0123456789abcdef0123456789abcdef
  Receipt digest    5555555555555555555555555555555555555555555555555555555555555555
  Promotion digest  6666666666666666666666666666666666666666666666666666666666666666
```

Stderr:

```text
Error · Candidate receipt is conflicted.
  Code: candidate_receipt_conflicted
  Command: cq proxy candidate receipt show
```

Exit: 6.



## `cq proxy candidate client-safety refresh`

### Failure

Invocation:

```sh
cq proxy candidate client-safety refresh --state-dir /Users/alex/cq-candidate-29280 --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate client-safety refresh
```

Exit: 4.



## `cq proxy candidate start`

### Failure

Invocation:

```sh
cq proxy candidate start --state-dir /Users/alex/cq-candidate-29280
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate start
```

Exit: 4.



## `cq proxy candidate release activate`

### Failure

Invocation:

```sh
cq proxy candidate release activate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 --confirm-artifact-switch
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate release activate
```

Exit: 4.



## `cq proxy candidate release validate`

### Failure

Invocation:

```sh
cq proxy candidate release validate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --rollback-bundle /Users/alex/cq-inputs/floor.json \
  --rollback-receipt /Users/alex/cq-inputs/floor-receipt.bin \
  --rollback-receipt-digest 7777777777777777777777777777777777777777777777777777777777777777 \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 \
  --receipt-file /Users/alex/cq-inputs/promotion.json --confirm-control-health
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate release validate
```

Exit: 4.



## `cq proxy candidate status`

### Success

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout:

```text
Candidate · prepared
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      1
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout: empty.

Stderr:

```text
Error · Candidate state or receipt does not exist.
  Code: candidate_not_found
  Command: cq proxy candidate status
```

Exit: 3.


### Success · pending external effect

Inspection completed, but the recorded stop effect is unresolved. Exit 0 does not mean healthy.

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout:

```text
Candidate · running
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      6
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  stop
  Effect started  yes

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.



## `cq proxy candidate stop`

### Success

Invocation:

```sh
cq proxy candidate stop --state-dir /Users/alex/cq-candidate-29280 --confirm-client-stopped
```

Stdout:

```text
Candidate · stopped
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      7
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         unknown
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         9999999999999999999999999999999999999999999999999999999999999999
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy candidate stop --state-dir /Users/alex/cq-candidate-29280 --confirm-client-stopped
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate stop
```

Exit: 6.



## `cq proxy candidate remove`

### Success

Invocation:

```sh
cq proxy candidate remove --state-dir /Users/alex/cq-candidate-29280 --confirm-candidate-state-loss
```

Stdout:

```text
Candidate · removed
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      10
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         unknown
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         9999999999999999999999999999999999999999999999999999999999999999
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy candidate remove --state-dir /Users/alex/cq-candidate-29280 --confirm-candidate-state-loss
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate remove
```

Exit: 6.



## `cq proxy health`

### Success

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · healthy
  Address    127.0.0.1:19280
  HTTP       200
  Reachable  yes
  Elapsed    12ms

Health endpoint only; model traffic not verified.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · failed
  Address    127.0.0.1:19280
  HTTP       unknown
  Reachable  no
  Elapsed    5ms

Health endpoint only; model traffic not verified.
```


Stderr:

```text
Error · Proxy is not reachable at 127.0.0.1:19280.
  Code: proxy_unreachable
  Command: cq proxy health
```

Exit: 4.


### Failure · reachable but unhealthy

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · failed
  Address    127.0.0.1:19280
  HTTP       503
  Reachable  yes
  Elapsed    14ms

Health endpoint only; model traffic not verified.
```

Stderr:

```text
Error · Proxy HTTP health check failed.
  Code: proxy_health_failed
  Command: cq proxy health
```

Exit: 1.



## `cq proxy operation status`

### Success

Invocation:

```sh
cq proxy operation status 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Proxy operation · result_available
  Operation         0123456789abcdef0123456789abcdef
  Phase             terminal
  Result available  yes
  Result digest     aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  Recovery support  unavailable
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy operation status fedcba9876543210fedcba9876543210
```

Stdout: empty.

Stderr:

```text
Error · The requested operation does not exist.
  Code: operation_not_found
  Command: cq proxy operation status
```

Exit: 3.


### Success · idle coordinator

Invocation:

```sh
cq proxy operation status
```

Stdout:

```text
Proxy operation · idle
  Operation         idle
  Phase             —
  Result available  no
  Result digest     —
  Recovery support  unavailable
```

Stderr: empty.

Exit: 0.



## `cq proxy rescue enter`

### Success · transition acknowledged, drain pending

Invocation:

```sh
cq proxy rescue enter
```

Stdout:

```text
Proxy rescue · rescue_draining
  Generation         12
  Active requests    0
  Draining sessions  2

Draining sessions
  session:123456789abc
  session:abcdef123456
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy rescue enter
```

Stdout: empty.

Stderr:

```text
Error · The proxy cannot perform this rescue transition.
  Code: rescue_transition_conflict
  Command: cq proxy rescue enter
```

Exit: 6.


### Timeout · inspect state before retrying

Invocation:

```sh
cq proxy rescue enter --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The proxy rescue operation timed out; inspect status before retrying.
  Code: rescue_timeout
  Command: cq proxy rescue enter
```

Exit: 7.



## `cq proxy rescue exit`

### Success · transition acknowledged, drain pending

Invocation:

```sh
cq proxy rescue exit
```

Stdout:

```text
Proxy rescue · rescue_exit_draining
  Generation         13
  Active requests    1
  Draining sessions  1

Draining sessions
  session:abcdef123456
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy rescue exit
```

Stdout: empty.

Stderr:

```text
Error · The proxy cannot perform this rescue transition.
  Code: rescue_transition_conflict
  Command: cq proxy rescue exit
```

Exit: 6.


### Timeout · inspect state before retrying

Invocation:

```sh
cq proxy rescue exit --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The proxy rescue operation timed out; inspect status before retrying.
  Code: rescue_timeout
  Command: cq proxy rescue exit
```

Exit: 7.



## `cq proxy rescue status`

### Success · normal

Invocation:

```sh
cq proxy rescue status
```

Stdout:

```text
Proxy rescue · normal
  Generation         14
  Active requests    0
  Draining sessions  0

No draining sessions.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy rescue status
```

Stdout: empty.

Stderr:

```text
Error · Proxy rescue control is unavailable.
  Code: rescue_unavailable
  Command: cq proxy rescue status
```

Exit: 4.



## `cq proxy serve`

### Success · foreground run ended by SIGTERM

Invocation:

```sh
cq proxy serve
```

Stdout:

```text
Proxy · listening
  Address    127.0.0.1:19280
  PID        4217
  Providers  Claude, Codex

Ctrl-C stops this foreground process.

Proxy · stopped
  Reason     interrupted
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy serve
```

Stdout: empty.

Stderr:

```text
Error · Port 19280 is already in use.
  Code: proxy_port_in_use
  Command: cq proxy serve
```

Exit: 6.


### Interruption · completed shutdown after Ctrl-C

SIGINT arrived after readiness; the stopped block requires observed owned-listener closure. A second interrupt before closure must omit it.

Invocation:

```sh
cq proxy serve
```

Stdout:

```text
Proxy · listening
  Address    127.0.0.1:19280
  PID        4217
  Providers  Claude, Codex

Ctrl-C stops this foreground process.

Proxy · stopped
  Reason     interrupted
```

Stderr:

```text
Error · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq proxy serve
```

Exit: 130.



## `cq proxy state initialise`

### Success

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout: empty.

Stderr:

```text
Error · Proxy state ownership conflicts with the requested directory.
  Code: proxy_state_conflict
  Command: cq proxy state initialise
```

Exit: 6.


### Success · safely reused state

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · existing
  Directory         /Users/alex/cq-runtime
  Restart required  no
```

Stderr: empty.

Exit: 0.


### Partial · creation committed, later binding failed

Durable authority creation and restart requirement are known; later configuration persistence failed. No rollback claim.

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created · incomplete
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr:

```text
Error · Proxy state initialisation partially completed; inspect state before retrying.
  Code: proxy_state_initialise_partial
  Command: cq proxy state initialise
```

Exit: 8.


### Interruption · preserve known creation

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created · incomplete
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr:

```text
Error · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq proxy state initialise
```

Exit: 130.



## `cq proxy status`

### Success

Invocation:

```sh
cq proxy status
```

Stdout:

```text
Proxy · ready    live
  Observed  0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Elapsed   24ms
  State     /Users/alex/cq-runtime

Evidence
  Inspector · known
    Executable  /opt/homebrew/bin/cq
    Version     1.0.0
    Commit      1234567890abcdef1234567890abcdef12345678
    Digest      1111111111111111111111111111111111111111111111111111111111111111
  Desired · known
    Manager     homebrew
    Configured  yes
    Listener    127.0.0.1:19280
  Service · known
    Manager     homebrew
    State       running
    Executable  /opt/homebrew/bin/cq
    PID         4217
  Listener · known
    State       listening
    Listener    127.0.0.1:19280
    Executable  /opt/homebrew/bin/cq
    PID         4217
  Process · known
    PID         4217
    Executable  /opt/homebrew/bin/cq
  Runtime · known
    Reachable   yes
    PID         4217
    Executable  /opt/homebrew/bin/cq
    Health      healthy
  Data plane · known
    Proven      no
    Code        unproven

Readiness inspection does not establish model-traffic acceptance.
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq proxy status --strict
```

Stdout: empty.

Stderr:

```text
Error · Proxy readiness could not be determined.
  Code: proxy_indeterminate
  Command: cq proxy status
```

Exit: 4.


### Timeout · no complete snapshot

Invocation:

```sh
cq proxy status --timeout 1ms
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: proxy_status_timeout
  Command: cq proxy status
```

Exit: 7.



## `cq service install`

### Success

Invocation:

```sh
cq service install
```

Stdout:

```text
CQ services · install    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  idle     yes         yes     —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service install
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service install
```

Exit: 6.


### Partial · proxy started, refresh failed

Both rows are observed final state after incomplete rollback. Proxy remains running; do not summarise this as unchanged.

Invocation:

```sh
cq service install
```

Stdout:

```text
CQ services · install · incomplete    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  failed   yes         no      —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   1
  Error       service_unhealthy
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · failed
```

Stderr:

```text
Error · Service operation was only partially completed; inspect component results.
  Code: service_partial
  Command: cq service install
```

Exit: 8.



## `cq service start`

### Success

Invocation:

```sh
cq service start --component proxy
```

Stdout:

```text
CQ services · start    proxy

Component  State    Auto-start  Health  PID
proxy      running  yes         yes     4217

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service start --component proxy
```

Stdout: empty.

Stderr:

```text
Error · Service component proxy is not installed.
  Code: service_not_installed
  Command: cq service start
```

Exit: 3.


### Timeout · selected verification incomplete

This adapter returned no component observations. The manager may have accepted the start; no rollback or unchanged-state claim.

Invocation:

```sh
cq service start --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_start_timeout
  Command: cq service start
```

Exit: 7.



## `cq service restart`

### Success

Invocation:

```sh
cq service restart --component token-refresh
```

Stdout:

```text
CQ services · restart    token-refresh

Component      State  Auto-start  Health  PID
token-refresh  idle   no          no      —

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Requested run completed successfully; automatic starts remain disabled.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service restart --component token-refresh
```

Stdout: empty.

Stderr:

```text
Error · Service component token-refresh is unhealthy.
  Code: service_unhealthy
  Command: cq service restart
```

Exit: 1.


### Timeout · restart completion unknown

Invocation:

```sh
cq service restart --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_restart_timeout
  Command: cq service restart
```

Exit: 7.



## `cq service status`

### Success

Invocation:

```sh
cq service status
```

Stdout:

```text
CQ services · status    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  idle     yes         yes     —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service status
```

Stdout: empty.

Stderr:

```text
Error · The current-user service manager is unavailable.
  Code: service_unavailable
  Command: cq service status
```

Exit: 4.


### Success · component absent

Inspection succeeded. Missing registration is not an empty table, and no shell-derived roots replace unknown authority.

Invocation:

```sh
cq service status --component token-refresh
```

Stdout:

```text
CQ services · status    token-refresh

Component      State   Auto-start  Health  PID
token-refresh  absent  no          no      —

token-refresh
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure · strict absence

Invocation:

```sh
cq service status --component token-refresh --strict
```

Stdout:

```text
CQ services · status    token-refresh

Component      State   Auto-start  Health  PID
token-refresh  absent  no          no      —

token-refresh
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Rollback · not_needed
```

Stderr:

```text
Error · Service component token-refresh is not installed.
  Code: service_not_installed
  Command: cq service status
```

Exit: 3.



## `cq service stop`

### Success

Invocation:

```sh
cq service stop
```

Stdout:

```text
CQ services · stop    all

Component      State    Auto-start  Health  PID
proxy          stopped  no          no      —
token-refresh  stopped  no          no      —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Selected services stopped or absent; automatic starts disabled where registered.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service stop
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service stop
```

Exit: 6.


### Timeout · stop completion unknown

Invocation:

```sh
cq service stop --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_stop_timeout
  Command: cq service stop
```

Exit: 7.



## `cq service uninstall`

### Success

Invocation:

```sh
cq service uninstall --component proxy
```

Stdout:

```text
CQ services · uninstall    proxy

Component  State   Auto-start  Health  PID
proxy      absent  no          no      —

proxy
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Selected registrations absent; credentials and user data retained.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.


### Failure

Invocation:

```sh
cq service uninstall --component proxy
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service uninstall
```

Exit: 6.
