# Codex routing — worked terminal examples

These are manually authored design fixtures, not captures from the current binary. Each scenario is independent. All accounts, keys, digests, paths and evidence are fictional. The fixed display clock is 23 Sep 2026, 16:30 BST (UTC+01:00). `alex@example.test` resolves to `acct-alex`; `sam@example.test` resolves to `acct-sam`. Terminal width is 100 columns. Frozen trace records remain single physical lines even when they exceed that width. These text bodies are the NO_COLOR terminal variant; colour roles follow the shared design, not escape sequences embedded in Markdown. Redirected legacy output and JSON are separate frozen contracts.

Each Help fixture also applies to `-h` and `cq help PATH`. All help scenarios have empty stderr and exit 0. Unless a scenario explicitly says otherwise, success has empty stderr, failure has empty stdout. No command in this file adds a confirmation prompt: the existing routing commands do not ask for one. Atomic routing updates have no fabricated row-by-row partial success. Timeout examples preserve uncertainty when no receipt exists. Codex pin, fallback and priming examples also show confirmed local saves after a timeout or interruption, with live application still unconfirmed. Raw trace retains its stream contract.
## `cq codex proxy fallback clear`

### Help

Invocation: `cq codex proxy fallback clear --help`

Stdout:

```text
cq codex proxy fallback clear
Clear the Codex proxy fallback.

Usage
  cq codex proxy fallback clear [options]

A fallback is a configured routing role, not guaranteed first choice. A pin takes precedence;
required continuity remains bound.
Save configuration atomically; no implicit restart or system-account switch. Repeating the same
setting, or clearing an absent setting, leaves equivalent configuration.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Inspect or remove the configured fallback.
  cq codex proxy fallback clear
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy fallback clear --timeout 1ms`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy fallback clear
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy fallback clear
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy fallback set`

### Help

Invocation: `cq codex proxy fallback set --help`

Stdout:

```text
cq codex proxy fallback set
Set the Codex proxy fallback.

Usage
  cq codex proxy fallback set ACCOUNT [options]

A fallback is a configured routing role, not guaranteed first choice. A pin takes precedence;
required continuity remains bound.
Save configuration atomically; no implicit restart or system-account switch. Repeating the same
setting, or clearing an absent setting, leaves equivalent configuration.

Arguments
  ACCOUNT
      Required. Unique Codex email, CQ alias or opaque AccountKey from cq codex account list.
      Must be nonempty and resolve to one known account. Ambiguous emails are rejected.
      Resolves once; only the AccountKey is stored.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Configure routing for Alex.
  cq codex proxy fallback set alex@example.test
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy fallback set alex@example.test --timeout 1ms`

Stdout:

```text
Saved Codex fallback configuration.
Codex fallback · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy fallback set
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy fallback set
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy fallback show`

### Help

Invocation: `cq codex proxy fallback show --help`

Stdout:

```text
cq codex proxy fallback show
Show the Codex proxy fallback.

Usage
  cq codex proxy fallback show [options]

A fallback is a configured routing role, not guaranteed first choice. A pin takes precedence;
required continuity remains bound.
Read saved configuration only; no live-process acknowledgement is checked.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Inspect or remove the configured fallback.
  cq codex proxy fallback show
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy pin clear --help`

Stdout:

```text
cq codex proxy pin clear
Clear the Codex proxy pin.

Usage
  cq codex proxy pin clear [options]

A pin selects one account for new or unbound work and overrides fallback and allowlist.
Required existing continuity remains on its bound account.
Save configuration atomically; no implicit restart or system-account switch. Repeating the same
setting, or clearing an absent setting, leaves equivalent configuration.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Inspect or remove the configured pin.
  cq codex proxy pin clear
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy pin clear --timeout 1ms`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · not configured
  Account key  not configured
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pin clear
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy pin clear
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy pin set`

### Help

Invocation: `cq codex proxy pin set --help`

Stdout:

```text
cq codex proxy pin set
Set the Codex proxy pin.

Usage
  cq codex proxy pin set ACCOUNT [options]

A pin selects one account for new or unbound work and overrides fallback and allowlist.
Required existing continuity remains on its bound account.
Save configuration atomically; no implicit restart or system-account switch. Repeating the same
setting, or clearing an absent setting, leaves equivalent configuration.

Arguments
  ACCOUNT
      Required. Unique Codex email, CQ alias or opaque AccountKey from cq codex account list.
      Must be nonempty and resolve to one known account. Ambiguous emails are rejected.
      Resolves once; only the AccountKey is stored.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Configure routing for Alex.
  cq codex proxy pin set alex@example.test
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy pin set alex@example.test --timeout 1ms`

Stdout:

```text
Saved Codex pin configuration.
Codex pin · configured
  Account key  acct-alex
  Source       Saved configuration
  Application  Live process acknowledgement not checked

Restart required: yes
Next: cq service restart --component proxy

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy pin set
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy pin set
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy pin show`

### Help

Invocation: `cq codex proxy pin show --help`

Stdout:

```text
cq codex proxy pin show
Show the Codex proxy pin.

Usage
  cq codex proxy pin show [options]

A pin selects one account for new or unbound work and overrides fallback and allowlist.
Required existing continuity remains on its bound account.
Read saved configuration only; no live-process acknowledgement is checked.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq service restart --component proxy

Examples
  Inspect or remove the configured pin.
  cq codex proxy pin show
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy lease invalidate --help`

Stdout:

```text
cq codex proxy lease invalidate
Invalidate reusable Codex task-account affinity.

Usage
  cq codex proxy lease invalidate [options]

Expire reusable leases from earlier admissions. Active request authority and required account
continuity remain intact. Later portable requests choose from current capacity again.
Requires authenticated live control and a writable migrated lease journal. No restart.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq proxy status

Examples
  Expire reusable affinity without breaking required continuity.
  cq codex proxy lease invalidate
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy policy apply --help`

Stdout:

```text
cq codex proxy policy apply
Apply a complete Codex routing policy.

Usage
  cq codex proxy policy apply --file FILE [options]

Policy schema is version 1, separate from CLI envelope version 2. Members are opaque AccountKeys.
Validate and publish one complete document atomically. Live updates need no restart.
Offline writes affect selected state only; ensure no live owner uses it concurrently.
Authority and routing generations must each advance by one; effective generation cannot decrease.
A stale replay conflicts. Preserve valid exported evidence; never fabricate authenticated evidence.

Options
  --file FILE
      Required. Readable regular UTF-8 JSON file. Relative paths use the working directory.
      Complete replacement, not a patch: strict schema_version 1, no unknown fields/trailing data.
      Validate the document and generations before mutation.
  --state-dir STATE_DIR
      Use this existing offline state root instead of authenticated live control.
      Clean absolute non-root directory; mutually exclusive with --port.
      Does not create state. Initialise with cq proxy state initialise first.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy pool --help
  cq codex proxy session --help

Examples
  Apply a prepared complete document.
  cq codex proxy policy apply --file /Users/alex/cq-policy.json
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy policy show --help`

Stdout:

```text
cq codex proxy policy show
Show the Codex routing policy.

Usage
  cq codex proxy policy show [options]

Policy schema is version 1, separate from CLI envelope version 2. Members are opaque AccountKeys.
Read the complete policy; never publish or create state.

Options
  --state-dir STATE_DIR
      Use this existing offline state root instead of authenticated live control.
      Clean absolute non-root directory; mutually exclusive with --port.
      Does not create state. Initialise with cq proxy state initialise first.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy pool --help
  cq codex proxy session --help

Examples
  Inspect authenticated live policy.
  cq codex proxy policy show
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy pool rename --help`

Stdout:

```text
cq codex proxy pool rename
Rename a Codex account pool.

Usage
  cq codex proxy pool rename OLD_NAME NEW_NAME [options]

Validate all input, then publish one atomic live policy update. No restart.
Equivalent repeated membership/value updates retain equivalent policy. Rename preserves identity
and bindings. Existing account continuity still takes precedence.

Arguments
  OLD_NAME
      Required. Existing pool name.
      Pool names are case-insensitive; display casing is preserved.
      Valid UTF-8, at least one non-whitespace character, no control characters.
  NEW_NAME
      Required. New pool name; cannot belong to another pool.
      Pool names are case-insensitive; display casing is preserved.
      Valid UTF-8, at least one non-whitespace character, no control characters.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq codex proxy policy show
  cq codex proxy session bind --help

Examples
  Rename work and preserve its bindings.
  cq codex proxy pool rename work primary
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy pool set --help`

Stdout:

```text
cq codex proxy pool set
Create or replace Codex pool membership.

Usage
  cq codex proxy pool set NAME --account ACCOUNT [--account ACCOUNT] [options]

Validate all input, then publish one atomic live policy update. No restart.
Equivalent repeated membership/value updates retain equivalent policy. Rename preserves identity
and bindings. Existing account continuity still takes precedence.

Arguments
  NAME
      Required. Pool to create or replace.
      Pool names are case-insensitive; display casing is preserved.
      Valid UTF-8, at least one non-whitespace character, no control characters.

Options
  --account ACCOUNT
      Required, repeatable. Unique Codex email, CQ alias or opaque AccountKey.
      Nonempty; resolve each reference once to exactly one known account. Store AccountKeys only.
      At least one member; reject duplicate resolved accounts and ambiguous references.
      Replaces membership rather than appending.
  --value VALUE
      Integer 0–4294967295. Higher values preserve this pool relative to lower-value viable pools.
      Omit to retain the existing value, or use 0 for a new pool. Not a capacity multiplier.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq codex proxy policy show
  cq codex proxy session bind --help

Examples
  Replace work membership with Alex and Sam.
  cq codex proxy pool set work --account alex@example.test --account sam@example.test --value 10
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy pool value --help`

Stdout:

```text
cq codex proxy pool value
Set a Codex pool preservation value.

Usage
  cq codex proxy pool value NAME VALUE [options]

Validate all input, then publish one atomic live policy update. No restart.
Equivalent repeated membership/value updates retain equivalent policy. Rename preserves identity
and bindings. Existing account continuity still takes precedence.

Arguments
  NAME
      Required. Existing pool name.
      Pool names are case-insensitive; display casing is preserved.
      Valid UTF-8, at least one non-whitespace character, no control characters.
  VALUE
      Required. Integer 0–4294967295; relative preservation value, not quota percentage.
      Higher values preserve capacity by preferring lower-value viable accounts for unbound work.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex account list
  cq codex proxy policy show
  cq codex proxy session bind --help

Examples
  Preserve work capacity relative to lower-value pools.
  cq codex proxy pool value work 20
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy prime disable --help`

Stdout:

```text
cq codex proxy prime disable
Disable Codex quota-window priming.

Usage
  cq codex proxy prime disable [options]

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a
cache refresh. Save configuration only. No immediate priming request or implicit restart.
Restart the proxy to apply this setting. Repeating this action is idempotent.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq service restart --component proxy
  cq proxy status

Examples
  Persist the priming setting, then restart when ready.
  cq codex proxy prime disable
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy prime disable --timeout 1ms`

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

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy prime disable
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy prime disable
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy prime enable`

### Help

Invocation: `cq codex proxy prime enable --help`

Stdout:

```text
cq codex proxy prime enable
Enable Codex quota-window priming.

Usage
  cq codex proxy prime enable [options]

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a
cache refresh. Save configuration only. No immediate priming request or implicit restart.
Restart the proxy to apply this setting. Repeating this action is idempotent.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq service restart --component proxy
  cq proxy status

Examples
  Persist the priming setting, then restart when ready.
  cq codex proxy prime enable
```

Stderr: empty.

Exit: `0`.

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

### Timeout · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

Invocation: `cq codex proxy prime enable --timeout 1ms`

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

Live application unconfirmed
```

Stderr:

```text
Warning · Routing operation timed out; inspect current state before retrying a mutation.
  Code: routing_timeout
  Command: cq codex proxy prime enable
```

Exit: `7`.

### Interruption · saved configuration

The local configuration save returned successfully before the operation stopped. Live application remains unconfirmed.

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

Live application unconfirmed
```

Stderr:

```text
Warning · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq codex proxy prime enable
  Result: completion unknown
```

Exit: `130`.

## `cq codex proxy prime status`

### Help

Invocation: `cq codex proxy prime status --help`

Stdout:

```text
cq codex proxy prime status
Show configured Codex quota-window priming.

Usage
  cq codex proxy prime status [options]

Priming sends small real Codex requests to start eligible dormant quota windows; it is not a
cache refresh. Read configured state only; live-process acknowledgement is not checked.

Options
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq service restart --component proxy
  cq proxy status

Examples
  Inspect priming configuration.
  cq codex proxy prime status
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy readiness show --help`

Stdout:

```text
cq codex proxy readiness show
Check retained HTTP readiness evidence for an exact Codex build.

Usage
  cq codex proxy readiness show --client-build CLIENT_BUILD [options]

Read the local HTTP marker against this CQ executable, client build, parser and lease schemas,
routing semantics, retry budget and fixture hash. Success means the retained tuple matches;
no new request or live health check occurs. Does not create or renew evidence.

Options
  --client-build CLIENT_BUILD
      Required. Exact client version: three numeric components, optional -suffix or +suffix.
      Suffix characters: ASCII letters, digits, dot and hyphen. No whitespace or arbitrary labels.
      Byte-for-byte match: no trimming, case folding or prefix matching.
  --state-dir STATE_DIR
      Nonempty canonical absolute directory; reject symlink traversal.
      Omit to use the resolved CQ proxy state directory, not the runtime resilience directory.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy validate http --help
  cq service status --component proxy

Examples
  Inspect retained evidence for this exact client version.
  cq codex proxy readiness show --client-build 0.146.0
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve clear --help`

Stdout:

```text
cq codex proxy reserve clear
Remove the system-account reserve configuration.

Usage
  cq codex proxy reserve clear [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Remove protection; repeated clear leaves equivalent state. Persist and apply live.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve status

Examples
  Remove reserve protection.
  cq codex proxy reserve clear
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve disable --help`

Stdout:

```text
cq codex proxy reserve disable
Temporarily bypass the system-account reserve until a verified reset.

Usage
  cq codex proxy reserve disable [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Requires fresh usage and reset evidence. Persist and apply a temporary bypass live.
Re-arm only after verified reset evidence; wall-clock expiry alone is not proof of a reset.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve status

Examples
  Bypass protection until the selected window resets with verified evidence.
  cq codex proxy reserve disable
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve enable --help`

Stdout:

```text
cq codex proxy reserve enable
Re-enable the configured system-account reserve.

Usage
  cq codex proxy reserve enable [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Requires existing configuration. Remove any bypass and apply protection live.
Repeated enable leaves equivalent state.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve status

Examples
  Restore the existing threshold.
  cq codex proxy reserve enable
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve set --help`

Stdout:

```text
cq codex proxy reserve set
Configure the current system-account quota reserve.

Usage
  cq codex proxy reserve set --window WINDOW --percent PERCENT [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Persist and apply live. Replace any previous bypass. No default window or percentage.

Options
  --window WINDOW
      Required. Exact currently observed selector from cq codex proxy reserve windows.
      Legacy case, surrounding spaces and underscores canonicalise before observed-window
        validation.
  --percent PERCENT
      Required. Finite number strictly greater than 0 and less than 100.
      Protect this percentage of the full window. 2 keeps the final 2 percentage points,
      not 2% of currently remaining quota. Exclude the account at or below the threshold.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve status

Examples
  Keep the final 2.05 percentage points of the observed 7d window.
  cq codex proxy reserve set --window 7d --percent 2.05
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve status --help`

Stdout:

```text
cq codex proxy reserve status
Show the current system-account reserve.

Usage
  cq codex proxy reserve status [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Observe live reserve state; reconciliation may check reset evidence or system-account changes.
This CLI does not issue a provider quota request.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve windows

Examples
  Inspect reserve protection and its usage evidence.
  cq codex proxy reserve status
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy reserve windows --help`

Stdout:

```text
cq codex proxy reserve windows
List observed current system-account quota windows.

Usage
  cq codex proxy reserve windows [options]

Scope: the current Codex system account only, not all accounts or a selected pool.
Missing, stale, expired or unsettled usage can block the protected account. A nominal reset
time alone does not prove renewed capacity.
Read observed windows, sorted by selector. This CLI does not issue a provider quota request.
Live reconciliation may check reset evidence or system-account changes.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy reserve --help
  cq codex proxy reserve status

Examples
  Discover valid window selectors before setting a reserve.
  cq codex proxy reserve windows
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy session bind --help`

Stdout:

```text
cq codex proxy session bind
Bind one exact session to a Codex account pool.

Usage
  cq codex proxy session bind --pool POOL (--session-id SESSION_ID | --session-id-stdin | --digest
    DIGEST) [options]

Normal output exposes keyed digests only, never raw session identifiers.
Create or replace one binding and publish the complete policy atomically. No restart.

Options
  --pool POOL
      Required. Existing pool name; case-insensitive selection preserves display casing.
      Valid UTF-8, at least one non-whitespace character, no control characters.
  --session-id SESSION_ID
      Exact raw UTF-8 session identifier, 1–4096 bytes. Prefer stdin to avoid shell history.
  --session-id-stdin
      Read 1–4096 exact UTF-8 bytes from stdin. No newline is removed; use printf, not echo.
  --digest DIGEST
      Existing full keyed digest: exactly 64 lowercase hexadecimal characters.
      Obtain it with cq codex proxy session digest.
      Exactly one selector is required. These three options are mutually exclusive.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq codex proxy pool --help

Examples
  Bind the fictional session to prepared pool work.
  printf %s example-session | cq codex proxy session bind --pool work --session-id-stdin
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy session digest --help`

Stdout:

```text
cq codex proxy session digest
Compute or validate a privacy-keyed session digest.

Usage
  cq codex proxy session digest (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST)
    [options]

Normal output exposes keyed digests only, never raw session identifiers.
A raw selector requires authenticated live control. An existing --digest is validated and
returned unchanged without state access. No binding is created.

Options
  --session-id SESSION_ID
      Exact raw UTF-8 session identifier, 1–4096 bytes. Prefer stdin to avoid shell history.
  --session-id-stdin
      Read 1–4096 exact UTF-8 bytes from stdin. No newline is removed; use printf, not echo.
  --digest DIGEST
      Existing full keyed digest: exactly 64 lowercase hexadecimal characters.
      Obtain it with cq codex proxy session digest.
      Exactly one selector is required. These three options are mutually exclusive.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq codex proxy pool --help

Examples
  Hash exact bytes without putting a session ID in shell history.
  printf %s example-session | cq codex proxy session digest --session-id-stdin
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy session list --help`

Stdout:

```text
cq codex proxy session list
List session-to-pool bindings.

Usage
  cq codex proxy session list [options]

Normal output exposes keyed digests only, never raw session identifiers.
Read-only authenticated live control. Sorted by full digest; session selectors are rejected.

Options
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq codex proxy pool --help

Examples
  Inspect recorded bindings.
  cq codex proxy session list
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy session show --help`

Stdout:

```text
cq codex proxy session show
Show one session-to-pool binding.

Usage
  cq codex proxy session show (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST)
    [options]

Normal output exposes keyed digests only, never raw session identifiers.
Read-only authenticated live control. A raw selector first needs an authenticated digest request.

Options
  --session-id SESSION_ID
      Exact raw UTF-8 session identifier, 1–4096 bytes. Prefer stdin to avoid shell history.
  --session-id-stdin
      Read 1–4096 exact UTF-8 bytes from stdin. No newline is removed; use printf, not echo.
  --digest DIGEST
      Existing full keyed digest: exactly 64 lowercase hexadecimal characters.
      Obtain it with cq codex proxy session digest.
      Exactly one selector is required. These three options are mutually exclusive.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq codex proxy pool --help

Examples
  Inspect the fictional session binding.
  printf %s example-session | cq codex proxy session show --session-id-stdin
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy session unbind --help`

Stdout:

```text
cq codex proxy session unbind
Remove one exact session-to-pool binding.

Usage
  cq codex proxy session unbind (--session-id SESSION_ID | --session-id-stdin | --digest DIGEST)
    [options]

Normal output exposes keyed digests only, never raw session identifiers.
Remove an existing binding and publish the complete policy atomically. No restart.
A missing binding is not a successful no-op.

Options
  --session-id SESSION_ID
      Exact raw UTF-8 session identifier, 1–4096 bytes. Prefer stdin to avoid shell history.
  --session-id-stdin
      Read 1–4096 exact UTF-8 bytes from stdin. No newline is removed; use printf, not echo.
  --digest DIGEST
      Existing full keyed digest: exactly 64 lowercase hexadecimal characters.
      Obtain it with cq codex proxy session digest.
      Exactly one selector is required. These three options are mutually exclusive.
  --port PORT
      Authenticated loopback proxy port, integer 1–65535.
      Omit to use the configured port, or 19280 when unset. Does not create configuration.
  --timeout TIMEOUT
      Maximum total elapsed time, including preparation and control requests. Default: 10s.
      Positive Go duration, for example 500ms or 30s. Expiry exits 7.
      A timed-out write may have committed; inspect current state before retrying.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq codex proxy policy show
  cq codex proxy pool --help

Examples
  Remove the fictional session binding.
  printf %s example-session | cq codex proxy session unbind --session-id-stdin
```

Stderr: empty.

Exit: `0`.

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

### Help

Invocation: `cq codex proxy trace --help`

Stdout:

```text
cq codex proxy trace
Read or follow retained Codex request traces.

Usage
  cq codex proxy trace [options]

Read configured local log files only; do not contact or change the proxy. Filters combine with AND;
tail applies after filtering retained history. Follow preserves retained rotation and reports gaps.
Configured missing files produce zero records; follow waits for creation. Malformed records skip.
JSON is JSONL: one record envelope per event, then exactly one terminal envelope. Successful end
exits 0, timeout 7, interruption 130; no second error envelope follows.
Payload mode reads existing capture. Known authentication fields are redacted; arbitrary prompt
or response content may still contain secrets. Human payload output never prints the body.

Options
  --session SESSION
      Raw session ID or codex://threads/ID, matched using privacy-safe session/thread keys.
      Trim surrounding whitespace and take the final slash component; reject an empty result.
      Omit to match all sessions.
  --trace TRACE
      Exact nonempty trace ID. Omit to match all trace IDs.
  --since SINCE
      Positive Go duration. Include records at or after command start minus this duration.
      Omit for no lower time bound.
  --tail TAIL
      Nonnegative integer. Newest matching retained records before following; default: 200.
      Use 0 for all matching retained records.
  --follow
      Read new records and rotated logs until interrupted. No proxy connection.
  --payload
      Read the existing opt-in payload log instead of route events; never enable capture.
      May expose recorded user content in JSON.
  --timeout TIMEOUT
      Optional positive Go duration for reading/following. Omit for no deadline. Expiry exits 7.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit record envelopes and one terminal envelope as JSONL; disable prompts.
  -v, --version  Show CQ version without reading state.
  Boolean flags default false; accept a bare flag, =true or =false.
  Options occur once unless marked repeatable; duplicate long/short spellings count together.

Related commands
  cq proxy --help
  cq codex proxy policy show

Examples
  Read up to 50 records from the last 15 minutes.
  cq codex proxy trace --since 15m --tail 50

  Follow one fictional session as JSONL envelopes.
  cq codex proxy trace --session codex://threads/example-session --follow --json
```

Stderr: empty.

Exit: `0`.

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
Warning · Trace history has a gap: required rotated segment is no longer retained.
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
