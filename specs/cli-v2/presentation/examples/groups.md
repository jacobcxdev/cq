# Root and group help examples

Hand-crafted target terminal fixtures, not captured output. Fences show complete visible text without ANSI; apply the shared colour roles when rendering. Each success has empty stderr. Each failure has empty stdout. All groups accept bare invocation, `-h`, `--help` and `cq help PATH`; the displayed help is identical. Bare `cq` is the quota dashboard, so root help requires an explicit help invocation.

The repeated global-options paragraph is intentional: every page is independently usable. Group help has no group-specific options or positional arguments beyond the selected child command. Unknown-child fixtures preserve the exact existing parser message (`internal/cli/parse.go`); the target layout adds its diagnostic code and entered path. No state access occurs. All sample commands below are help or read-only inspection, except commands explicitly presented as syntax inspection with `--help`.

## `cq --help`

### Help

```sh
cq --help
```

Exit 0 · stdout:

```text
cq
Check provider quotas and manage accounts, routing and services.

Usage
  cq [COMMAND] [OPTIONS]

Without a command, check Claude, Codex and Gemini quotas.

Quotas
  check        Report quota usage for selected providers.

Providers
  claude       Manage Claude accounts and proxy routing pins.
  codex        Manage Codex accounts, resets and proxy controls.
  gemini       Inspect the externally managed Gemini account.

Operations
  auth         Refresh stored provider authentication credentials.
  models       Inspect model registries and publish cache updates.
  proxy        Run and inspect the shared proxy and its state.
  service      Manage proxy and token-refresh background services.

Utilities
  completion   Print a shell completion script.
  help         Show help for a command path.
  version      Show CQ build identity.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Examples
  Show all provider quotas
  cq

  Fetch fresh Codex quotas
  cq check codex --fresh

  Find reserve and other Codex proxy controls
  cq codex proxy --help

  Inspect service installation options
  cq help service install
```

### Failure

```sh
cq inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq inspect
```

## `cq auth`

### Help

```sh
cq auth --help
```

Exit 0 · stdout:

```text
cq auth
Refresh stored provider authentication credentials.

Usage
  cq auth <COMMAND> [OPTIONS]

Commands
  refresh   Refresh eligible stored Claude and Codex OAuth credentials.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq claude account   Add, inspect, select and remove Claude accounts.
  cq codex account    Add, inspect, select and remove Codex accounts.
  cq gemini account   Inspect the externally managed Gemini account.

Examples
  Inspect refresh scope and options
  cq auth refresh --help
```

### Failure

```sh
cq auth inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq auth inspect
```

## `cq claude`

### Help

```sh
cq claude --help
```

Exit 0 · stdout:

```text
cq claude
Manage Claude accounts and proxy routing pins.

Usage
  cq claude <COMMAND> [OPTIONS]

Commands
  account   Authenticate, inspect, activate and remove Claude accounts.
  proxy     Control Claude-specific routing through the shared proxy.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help   All commands.

Examples
  List locally configured Claude accounts
  cq claude account list

  Inspect the proxy account override
  cq claude proxy pin show
```

### Failure

```sh
cq claude inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq claude inspect
```

## `cq claude account`

### Help

```sh
cq claude account --help
```

Exit 0 · stdout:

```text
cq claude account
Authenticate, inspect, activate and remove Claude accounts.

Usage
  cq claude account <COMMAND> [OPTIONS]

An account is a local credential identity. Activation selects the native
client default; proxy pinning is a separate routing choice.

Commands
  activate   Select the native client default Claude account.
  list       List locally configured Claude accounts.
  login      Authenticate and store a Claude account.
  remove     Remove local Claude account credentials.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help claude   Claude commands.

Examples
  Inspect configured accounts
  cq claude account list

  Inspect login options
  cq claude account login --help
```

### Failure

```sh
cq claude account inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq claude account inspect
```

## `cq claude proxy`

### Help

```sh
cq claude proxy --help
```

Exit 0 · stdout:

```text
cq claude proxy
Control Claude-specific routing through the shared proxy.

Usage
  cq claude proxy <COMMAND> [OPTIONS]

Commands
  pin   Inspect, set or clear the Claude proxy account override.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq proxy     Shared proxy runtime, health, state and diagnostics.
  cq service   Manage background services.

Examples
  Inspect the Claude routing override
  cq claude proxy pin show
```

### Failure

```sh
cq claude proxy inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq claude proxy inspect
```

## `cq claude proxy pin`

### Help

```sh
cq claude proxy pin --help
```

Exit 0 · stdout:

```text
cq claude proxy pin
Inspect, set or clear the Claude proxy account override.

Usage
  cq claude proxy pin <COMMAND> [OPTIONS]

A pin chooses an account for new or unbound routed work.

Commands
  clear   Clear the Claude proxy pin.
  set     Set the Claude proxy pin.
  show    Show the Claude proxy pin.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help claude proxy   Claude proxy commands.

Examples
  Inspect the current pin
  cq claude proxy pin show

  Inspect account-selection syntax
  cq claude proxy pin set --help
```

### Failure

```sh
cq claude proxy pin inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq claude proxy pin inspect
```

## `cq codex`

### Help

```sh
cq codex --help
```

Exit 0 · stdout:

```text
cq codex
Manage Codex accounts, quota resets and provider-specific proxy controls.

Usage
  cq codex <COMMAND> [OPTIONS]

Commands
  account   Authenticate, inspect, activate and remove Codex accounts.
  proxy     Control routing, quota protection and transport evidence.
  reset     Inspect, schedule and use banked quota-reset credits.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help   All commands.

Examples
  List locally configured accounts
  cq codex account list

  Inspect quota protection
  cq codex proxy reserve status

  List reset credits without consuming one
  cq codex reset list
```

### Failure

```sh
cq codex inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex inspect
```

## `cq codex account`

### Help

```sh
cq codex account --help
```

Exit 0 · stdout:

```text
cq codex account
Authenticate, inspect, activate and remove Codex accounts.

Usage
  cq codex account <COMMAND> [OPTIONS]

Activation selects the native client default account. Proxy pinning is a
separate routing choice.

Commands
  activate   Select the native client default Codex account.
  list       List locally configured Codex accounts.
  login      Authenticate and store a Codex account.
  remove     Remove local Codex account credentials.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex   Codex commands.

Examples
  Inspect configured accounts
  cq codex account list

  Inspect account activation syntax
  cq codex account activate --help
```

### Failure

```sh
cq codex account inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex account inspect
```

## `cq codex proxy`

### Help

```sh
cq codex proxy --help
```

Exit 0 · stdout:

```text
cq codex proxy
Control Codex routing, quota protection and transport evidence.

Usage
  cq codex proxy <COMMAND> [OPTIONS]

Commands
  canary                Manage a bounded routing-observation run.
  credential-endpoint   Maintain the local credential-owner endpoint.
  fallback              Inspect, set or clear the terminal fallback account.
  fixture               Create sanitised fixtures from saved requests.
  hook                  Produce native client lifecycle receipt annotations.
  lease                 Invalidate reusable routing affinities.
  pin                   Inspect, set or clear the proxy account override.
  policy                Export or replace the complete routing policy.
  pool                  Define account pools and preservation values.
  prime                 Inspect or toggle quota-window priming.
  readiness             Inspect transport evidence for an exact client build.
  reserve               Protect quota on the current system account.
  session               Inspect or change a client session's routing pool.
  trace                 Read or follow retained request traces.
  validate              Request HTTP or run WebSocket transport checks.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq proxy     Shared proxy runtime, health, state and diagnostics.
  cq service   Manage background services.

Examples
  Inspect system-account reserve state
  cq codex proxy reserve status

  Inspect the full routing policy
  cq codex proxy policy show

  Find exact-build readiness arguments
  cq codex proxy readiness show --help
```

### Failure

```sh
cq codex proxy inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy inspect
```

## `cq codex proxy canary`

### Help

```sh
cq codex proxy canary --help
```

Exit 0 · stdout:

```text
cq codex proxy canary
Manage a bounded Codex routing-observation run.

Usage
  cq codex proxy canary <COMMAND> [OPTIONS]

A canary observes real routing with protected-state checks.

Commands
  start    Start a protected observation run.
  status   Show the retained observation run.
  stop     Request a graceful end to the observation run.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect a retained run
  cq codex proxy canary status

  Inspect run limits before starting
  cq codex proxy canary start --help
```

### Failure

```sh
cq codex proxy canary inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy canary inspect
```

## `cq codex proxy credential-endpoint`

### Help

```sh
cq codex proxy credential-endpoint --help
```

Exit 0 · stdout:

```text
cq codex proxy credential-endpoint
Maintain the Codex local credential-owner endpoint.

Usage
  cq codex proxy credential-endpoint <COMMAND> [OPTIONS]

This endpoint is the credential-owner socket, not the HTTP proxy listener.

Commands
  legacy   Inspect and migrate the legacy endpoint protocol.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect legacy endpoint maintenance commands
  cq codex proxy credential-endpoint legacy --help
```

### Failure

```sh
cq codex proxy credential-endpoint inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy credential-endpoint inspect
```

## `cq codex proxy credential-endpoint legacy`

### Help

```sh
cq codex proxy credential-endpoint legacy --help
```

Exit 0 · stdout:

```text
cq codex proxy credential-endpoint legacy
Inspect and migrate the legacy credential-owner endpoint protocol.

Usage
  cq codex proxy credential-endpoint legacy <COMMAND> [OPTIONS]

Commands
  activate   Activate a reversible replacement endpoint.
  finalise   Finalise a verified endpoint replacement.
  inspect    Inspect the legacy socket and migration state.
  prepare    Quarantine a refused legacy socket for replacement.
  resume     Reopen a retained endpoint migration.
  rollback   Restore the quarantined legacy socket.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy credential-endpoint   Credential endpoint commands.

Examples
  Inspect maintenance inputs without changing the socket
  cq codex proxy credential-endpoint legacy inspect --help

  Inspect rollback requirements
  cq codex proxy credential-endpoint legacy rollback --help
```

### Failure

```sh
cq codex proxy credential-endpoint legacy status
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: status. Run cq help.
  Code: unknown_command · Command: cq codex proxy credential-endpoint legacy status
```

## `cq codex proxy fallback`

### Help

```sh
cq codex proxy fallback --help
```

Exit 0 · stdout:

```text
cq codex proxy fallback
Inspect, set or clear the terminal fallback account for Codex routing.

Usage
  cq codex proxy fallback <COMMAND> [OPTIONS]

The fallback account is a terminal routing option, not a first-choice pin.

Commands
  clear   Clear the Codex proxy fallback.
  set     Set the Codex proxy fallback.
  show    Show the Codex proxy fallback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect the configured fallback
  cq codex proxy fallback show

  Inspect fallback-selection syntax
  cq codex proxy fallback set --help
```

### Failure

```sh
cq codex proxy fallback inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy fallback inspect
```

## `cq codex proxy fixture`

### Help

```sh
cq codex proxy fixture --help
```

Exit 0 · stdout:

```text
cq codex proxy fixture
Create sanitised test fixtures from saved Codex request bodies.

Usage
  cq codex proxy fixture <COMMAND> [OPTIONS]

Commands
  create   Create a sanitised fixture from a saved Codex request.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect accepted input formats and sanitisation options
  cq codex proxy fixture create --help
```

### Failure

```sh
cq codex proxy fixture inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy fixture inspect
```

## `cq codex proxy hook`

### Help

```sh
cq codex proxy hook --help
```

Exit 0 · stdout:

```text
cq codex proxy hook
Produce receipt annotations for native client lifecycle hooks.

Usage
  cq codex proxy hook <COMMAND> [OPTIONS]

Hooks are machine integrations. Their payloads contain no terminal styling.

Commands
  stop   Return a private-data-safe routing receipt for a Codex Stop hook.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect the hook input and output protocol
  cq codex proxy hook stop --help
```

### Failure

```sh
cq codex proxy hook inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy hook inspect
```

## `cq codex proxy lease`

### Help

```sh
cq codex proxy lease --help
```

Exit 0 · stdout:

```text
cq codex proxy lease
Invalidate reusable Codex routing affinities.

Usage
  cq codex proxy lease <COMMAND> [OPTIONS]

A lease is a reusable task-to-account routing affinity.

Commands
  invalidate   Invalidate reusable Codex task-account affinity.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect invalidation scope before making a change
  cq codex proxy lease invalidate --help
```

### Failure

```sh
cq codex proxy lease inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy lease inspect
```

## `cq codex proxy pin`

### Help

```sh
cq codex proxy pin --help
```

Exit 0 · stdout:

```text
cq codex proxy pin
Inspect, set or clear the Codex proxy account override.

Usage
  cq codex proxy pin <COMMAND> [OPTIONS]

A pin chooses an account for new or unbound routed work.

Commands
  clear   Clear the Codex proxy pin.
  set     Set the Codex proxy pin.
  show    Show the Codex proxy pin.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect the current pin
  cq codex proxy pin show

  Inspect account-selection syntax
  cq codex proxy pin set --help
```

### Failure

```sh
cq codex proxy pin inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy pin inspect
```

## `cq codex proxy policy`

### Help

```sh
cq codex proxy policy --help
```

Exit 0 · stdout:

```text
cq codex proxy policy
Export or replace the complete Codex routing-policy document.

Usage
  cq codex proxy policy <COMMAND> [OPTIONS]

Commands
  apply   Apply a complete Codex routing-policy document.
  show    Show the complete Codex routing-policy document.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect the complete configured policy
  cq codex proxy policy show

  Inspect replacement validation and file inputs
  cq codex proxy policy apply --help
```

### Failure

```sh
cq codex proxy policy inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy policy inspect
```

## `cq codex proxy pool`

### Help

```sh
cq codex proxy pool --help
```

Exit 0 · stdout:

```text
cq codex proxy pool
Define account pools and their relative capacity-preservation values.

Usage
  cq codex proxy pool <COMMAND> [OPTIONS]

Commands
  rename   Rename a pool without changing its identity.
  set      Create a pool or replace its account membership.
  value    Set a pool's relative capacity-preservation value.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect pool membership syntax
  cq codex proxy pool set --help

  Inspect how relative preservation values work
  cq codex proxy pool value --help
```

### Failure

```sh
cq codex proxy pool inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy pool inspect
```

## `cq codex proxy prime`

### Help

```sh
cq codex proxy prime --help
```

Exit 0 · stdout:

```text
cq codex proxy prime
Inspect or toggle automatic Codex quota-window priming.

Usage
  cq codex proxy prime <COMMAND> [OPTIONS]

Commands
  disable   Disable automatic Codex quota-window priming.
  enable    Enable automatic Codex quota-window priming.
  status    Show configured Codex quota-window priming.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect current priming configuration
  cq codex proxy prime status

  Inspect the effects of enabling priming
  cq codex proxy prime enable --help
```

### Failure

```sh
cq codex proxy prime inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy prime inspect
```

## `cq codex proxy readiness`

### Help

```sh
cq codex proxy readiness --help
```

Exit 0 · stdout:

```text
cq codex proxy readiness
Inspect retained transport-readiness evidence for an exact client build.

Usage
  cq codex proxy readiness <COMMAND> [OPTIONS]

Commands
  show   Check retained HTTP readiness evidence for an exact Codex build.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect required build identity and evidence options
  cq codex proxy readiness show --help
```

### Failure

```sh
cq codex proxy readiness inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy readiness inspect
```

## `cq codex proxy reserve`

### Help

```sh
cq codex proxy reserve --help
```

Exit 0 · stdout:

```text
cq codex proxy reserve
Protect remaining quota on the current Codex system account.

Usage
  cq codex proxy reserve <COMMAND> [OPTIONS]

Commands
  clear     Remove the system-account reserve configuration.
  disable   Disable the reserve until a verified reset.
  enable    Enable the configured system-account reserve.
  set       Set the system-account quota reserve.
  status    Show reserve state and evidence.
  windows   List observed system-account quota window selectors.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect reserve protection and evidence
  cq codex proxy reserve status

  Find available quota windows
  cq codex proxy reserve windows

  Inspect threshold syntax before changing protection
  cq codex proxy reserve set --help
```

### Failure

```sh
cq codex proxy reserve inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy reserve inspect
```

## `cq codex proxy session`

### Help

```sh
cq codex proxy session --help
```

Exit 0 · stdout:

```text
cq codex proxy session
Inspect or change the routing pool bound to a client session.

Usage
  cq codex proxy session <COMMAND> [OPTIONS]

Commands
  bind     Bind one exact session to a Codex pool.
  digest   Return the keyed digest for a session selector.
  list     List all session-to-pool bindings.
  show     Show one session-to-pool binding.
  unbind   Remove one exact session-to-pool binding.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect retained bindings
  cq codex proxy session list

  Inspect exact-session selector syntax
  cq codex proxy session show --help
```

### Failure

```sh
cq codex proxy session inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy session inspect
```

## `cq codex proxy validate`

### Help

```sh
cq codex proxy validate --help
```

Exit 0 · stdout:

```text
cq codex proxy validate
Request HTTP checks or run WebSocket transport validation.

Usage
  cq codex proxy validate <COMMAND> [OPTIONS]

Commands
  http        Request startup validation of an installed candidate listener.
  websocket   Validate routing with an isolated installed Codex client.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex proxy   Codex proxy controls.

Examples
  Inspect HTTP validation prerequisites
  cq codex proxy validate http --help

  Inspect WebSocket client and evidence options
  cq codex proxy validate websocket --help
```

### Failure

```sh
cq codex proxy validate inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex proxy validate inspect
```

## `cq codex reset`

### Help

```sh
cq codex reset --help
```

Exit 0 · stdout:

```text
cq codex reset
Inspect, schedule and use banked Codex quota-reset credits.

Usage
  cq codex reset <COMMAND> [OPTIONS]

A reset credit restores shared usage percentages. Consuming one spends it.

Commands
  list        List Codex banked reset credits.
  recommend   Recommend when to use Codex reset credits.
  use         Consume one Codex banked reset credit.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help codex   Codex commands.

Examples
  List credits without consuming one
  cq codex reset list

  Inspect consumption and confirmation requirements
  cq codex reset use --help
```

### Failure

```sh
cq codex reset inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex reset inspect
```

## `cq gemini`

### Help

```sh
cq gemini --help
```

Exit 0 · stdout:

```text
cq gemini
Inspect the externally managed Gemini account.

Usage
  cq gemini <COMMAND> [OPTIONS]

Commands
  account   Show the single externally managed Gemini account.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help   All commands.

Examples
  Inspect Gemini account discovery
  cq gemini account show
```

### Failure

```sh
cq gemini inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq gemini inspect
```

## `cq gemini account`

### Help

```sh
cq gemini account --help
```

Exit 0 · stdout:

```text
cq gemini account
Show the single externally managed Gemini account.

Usage
  cq gemini account <COMMAND> [OPTIONS]

Gemini credentials are managed externally. CQ does not add, activate or
remove Gemini accounts.

Commands
  show   Show the externally managed Gemini account.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help gemini   Gemini commands.

Examples
  Inspect the discovered account
  cq gemini account show
```

### Failure

```sh
cq gemini account inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq gemini account inspect
```

## `cq models`

### Help

```sh
cq models --help
```

Exit 0 · stdout:

```text
cq models
Inspect model registries and publish local model-cache updates.

Usage
  cq models <COMMAND> [OPTIONS]

Commands
  list      List active models from local registry caches.
  overlay   Add, remove or prune local model-registry additions.
  refresh   Refresh model sources and publish client model caches.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help   All commands.

Examples
  Inspect locally cached model availability
  cq models list

  Inspect refresh and publication options
  cq models refresh --help
```

### Failure

```sh
cq models inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq models inspect
```

## `cq models overlay`

### Help

```sh
cq models overlay --help
```

Exit 0 · stdout:

```text
cq models overlay
Add, remove or prune local model-registry additions.

Usage
  cq models overlay <COMMAND> [OPTIONS]

Overlays expose local model entries before native discovery supplies them.

Commands
  add      Add or replace one local model overlay and publish caches.
  prune    Remove overlays now supplied by native model sources.
  remove   Remove one local model overlay and publish caches.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help models   Model registry commands.

Examples
  Inspect model entry and provider options
  cq models overlay add --help

  Inspect pruning and publication behaviour
  cq models overlay prune --help
```

### Failure

```sh
cq models overlay inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Invalid arguments: Unknown command: inspect. Run cq help.
  Run cq models overlay --help.
  Code: cli_invalid_usage · Command: cq models overlay inspect
```

## `cq proxy`

### Help

```sh
cq proxy --help
```

Exit 0 · stdout:

```text
cq proxy
Run and inspect the shared proxy, state and lifecycle machinery.

Usage
  cq proxy <COMMAND> [OPTIONS]

Commands
  candidate   Manage isolated candidate state and release evidence.
  health      Probe proxy HTTP liveness.
  operation   Inspect retained lifecycle operation state.
  rescue      Inspect or change rescue-serving mode.
  serve       Run the shared API proxy in the foreground.
  state       Initialise an explicitly selected authority directory.
  status      Inspect shared proxy readiness evidence.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq codex proxy reserve   Codex quota reserves; formerly cq proxy reserve.
  cq codex proxy policy    Codex routing policy; formerly cq proxy policy.
  cq codex proxy           Pools, sessions, pins, fallback, priming, leases,
                           traces, validation, credential endpoints and hooks.
  cq claude proxy pin      Claude pins; formerly cq proxy pin claude.
  cq service               Install, start, stop, restart or uninstall services.

Examples
  Inspect shared readiness evidence
  cq proxy status

  Inspect Codex quota protection
  cq codex proxy reserve status

  Inspect background services
  cq service status
```

### Failure

```sh
cq proxy inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy inspect
```

## `cq proxy candidate`

### Help

```sh
cq proxy candidate --help
```

Exit 0 · stdout:

```text
cq proxy candidate
Manage isolated candidate state and inspect release evidence.

Usage
  cq proxy candidate <COMMAND> [OPTIONS]

A candidate has isolated runtime state for evaluating a release.
Client-safety refresh and release transitions are currently unavailable.

Commands
  client-safety   Client-isolation measurements; currently unavailable.
  prepare         Prepare an isolated release candidate without starting it.
  receipt         Inspect a retained candidate attempt receipt.
  release         Release activation and qualification; unavailable.
  remove          Delete stopped candidate state and retained receipts.
  start           Start the exact release bound to an isolated candidate.
  status          Show retained state for one isolated candidate.
  stop            Stop a candidate after its client has stopped.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy   Shared proxy commands.

Examples
  Inspect candidate-selection arguments
  cq proxy candidate status --help

  Inspect retained receipt lookup
  cq proxy candidate receipt show --help
```

### Failure

```sh
cq proxy candidate inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy candidate inspect
```

## `cq proxy candidate client-safety`

### Help

```sh
cq proxy candidate client-safety --help
```

Exit 0 · stdout:

```text
cq proxy candidate client-safety
Reserve measured client-safety operations; currently unavailable.

Usage
  cq proxy candidate client-safety <COMMAND> [OPTIONS]

The command below is reserved. Its operational invocation is unavailable;
help succeeds without changing state.

Commands
  refresh   Measure and renew client credential-isolation evidence.
            Currently unavailable.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy candidate   Candidate commands.

Examples
  Inspect the reserved command's contract without invoking it
  cq proxy candidate client-safety refresh --help
```

### Failure

```sh
cq proxy candidate client-safety inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy candidate client-safety inspect
```

## `cq proxy candidate receipt`

### Help

```sh
cq proxy candidate receipt --help
```

Exit 0 · stdout:

```text
cq proxy candidate receipt
Inspect a retained candidate attempt receipt.

Usage
  cq proxy candidate receipt <COMMAND> [OPTIONS]

A receipt is a retained, machine-verifiable validation-attempt record.

Commands
  show   Show the authenticated receipt for one validation attempt.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy candidate   Candidate commands.

Examples
  Inspect candidate and attempt selector requirements
  cq proxy candidate receipt show --help
```

### Failure

```sh
cq proxy candidate receipt inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy candidate receipt inspect
```

## `cq proxy candidate release`

### Help

```sh
cq proxy candidate release --help
```

Exit 0 · stdout:

```text
cq proxy candidate release
Reserve release activation and qualification; currently unavailable.

Usage
  cq proxy candidate release <COMMAND> [OPTIONS]

Both commands below are reserved. Operational invocations are unavailable;
help succeeds without activating or qualifying a release.

Commands
  activate   Activate a prepared runtime release inside a candidate.
             Currently unavailable.
  validate   Verify release evidence and write a validation receipt.
             Currently unavailable.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy candidate   Candidate commands.

Examples
  Inspect the reserved activation contract
  cq proxy candidate release activate --help

  Inspect the reserved validation contract
  cq proxy candidate release validate --help
```

### Failure

```sh
cq proxy candidate release inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy candidate release inspect
```

## `cq proxy operation`

### Help

```sh
cq proxy operation --help
```

Exit 0 · stdout:

```text
cq proxy operation
Inspect retained shared proxy lifecycle operation state.

Usage
  cq proxy operation <COMMAND> [OPTIONS]

Commands
  status   Show a retained shared proxy operation record.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy   Shared proxy commands.

Examples
  Inspect operation ID and state-directory inputs
  cq proxy operation status --help
```

### Failure

```sh
cq proxy operation inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy operation inspect
```

## `cq proxy rescue`

### Help

```sh
cq proxy rescue --help
```

Exit 0 · stdout:

```text
cq proxy rescue
Inspect or transition the shared runtime rescue-serving mode.

Usage
  cq proxy rescue <COMMAND> [OPTIONS]

Rescue is the shared supervisor's fallback traffic mode while the normal
worker is unavailable.

Commands
  enter    Request shared proxy rescue traffic mode.
  exit     Request return to normal proxy traffic.
  status   Show shared proxy rescue and drain state.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy   Shared proxy commands.

Examples
  Inspect rescue and drain observation options
  cq proxy rescue status --help

  Inspect transition requirements without requesting a transition
  cq proxy rescue enter --help
```

### Failure

```sh
cq proxy rescue inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy rescue inspect
```

## `cq proxy state`

### Help

```sh
cq proxy state --help
```

Exit 0 · stdout:

```text
cq proxy state
Initialise an explicitly selected shared proxy authority directory.

Usage
  cq proxy state <COMMAND> [OPTIONS]

Commands
  initialise   Initialise shared authenticated proxy state.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq help proxy   Shared proxy commands.

Examples
  Inspect authority-directory and owner requirements
  cq proxy state initialise --help
```

### Failure

```sh
cq proxy state inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq proxy state inspect
```

## `cq service`

### Help

```sh
cq service --help
```

Exit 0 · stdout:

```text
cq service
Manage proxy and periodic token-refresh background services.

Usage
  cq service <COMMAND> [OPTIONS]

Commands
  install     Install and start selected CQ services.
  restart     Restart selected installed CQ services.
  start       Start selected installed CQ services.
  status      Inspect selected CQ service components.
  stop        Stop and disable selected CQ services.
  uninstall   Stop and remove selected CQ service registrations.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.
  Boolean flags default to false. Use a bare flag or =true / =false.
  Specify each flag once; long and short spellings are the same flag.

Related commands
  cq proxy   Inspect readiness or run the proxy in the foreground.

Examples
  Inspect installed service components
  cq service status

  Inspect component selection and installation options
  cq service install --help
```

### Failure

```sh
cq service inspect
```

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq service inspect
```
