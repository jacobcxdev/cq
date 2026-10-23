# Dots output examples

Hand-crafted expected terminal output. These examples do not assert a live installation.

## `cq codex dots`

### Help

Invocation: `cq codex dots --help`

Exit 0 · stdout:

```text
cq codex dots
Manage multi-account Dots and their private installation state.

Usage
  cq codex dots <COMMAND> [OPTIONS]

Manage multi-account Dots for the configured native app or an isolated research profile.

Choose a subcommand below. A bare group prints this help without reading configuration or changing
state.

--root is inherited by every Dots command and can appear before or after the leaf command.

Commands
  install
      Install multi-account Dots for the normal native app.
  start
      Start Dots in the configured native app.
  stop
      Stop the Dots adapter after quitting the app.
  status
      Show Dots status.
  sync
      Refresh Dots account discovery.
  list
      List discovered Dots.
  prime
      Prime one Dot or all discovered Dots.
  broadcast
      Send a user message to every discovered Dot.
  board
      Show or assign one local chat as the shared Dot message board.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Show Dots commands and inherited options.
  cq codex dots --help
```

### Failure

Invocation: `cq codex dots inspect`

Exit 2 · stderr:

```text
Error · Unknown command
  Unknown command: inspect. Run cq help.
  Code: unknown_command · Command: cq codex dots inspect
```

## `cq codex dots install`

### Help

Invocation: `cq codex dots install --help`

Exit 0 · stdout:

```text
cq codex dots install
Install multi-account Dots for the normal native app.

Usage
  cq codex dots install [OPTIONS]

Create the private installation, discover account profiles, and start the Dots adapter.

The result reports whether setup is ready or needs onboarding or a normal app restart.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.
  --mode MODE
      Use the normal native profile or an isolated research profile. Default: native.
      Exact enum native or isolated.
  --app ABS_PATH
      Native app bundle. Default: /Applications/ChatGPT.app.
      Clean absolute path.
  --system-auth ABS_PATH
      Read-only native Codex authentication path. Default: ~/.codex/auth.json.
      Clean absolute path when supplied.
  --cq-accounts ABS_PATH
      Read-only CQ Codex accounts directory. Default: ~/.codex/accounts.
      Clean absolute path when supplied.
  --account-source ABS_PATH
      Optional read-only CodexBar account directory. Omission adds no source.
      Clean absolute path when supplied.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Install Dots using native mode and default paths.
  cq codex dots install
  Install an isolated research profile in a separate root.
  cq codex dots install --root /tmp/cq-dots --mode isolated
```

### Success

Invocation: `cq codex dots install`

Exit 0 · stdout:

```text
Codex Dots · install
  Installed: Yes
  Native catalogue: Ready
  System app modified: No
  Profiles: 2
```

### Failure

Invocation: `cq codex dots install`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots install
```

## `cq codex dots start`

### Help

Invocation: `cq codex dots start --help`

Exit 0 · stdout:

```text
cq codex dots start
Start Dots in the configured native app.

Usage
  cq codex dots start [OPTIONS]

Start the configured Dots adapter and check the native catalogue and workspace.

If the normal app must restart or a Dot needs onboarding, the result explains what remains.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Start Dots in the configured native app.
  cq codex dots start
```

### Success

Invocation: `cq codex dots start`

Exit 0 · stdout:

```text
Codex Dots · start
  Native catalogue: Ready
  Account watcher: Running
  Relay: Running
  System app modified: No
  Profiles: 2
```

### Failure

Invocation: `cq codex dots start`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots start
```

### Pending · restart required

Invocation: `cq codex dots start`

Exit 0 · stdout:

```text
Codex Dots · start
  Started: No
  Restart required: Yes
  Detail: Quit the app, then run cq codex dots start.
```

Exit 0 · stderr:

```text
Warning · Dots has pending or incomplete work; inspect the receipt before retrying.
  Code: dots_incomplete
  Command: cq codex dots start
```

## `cq codex dots stop`

### Help

Invocation: `cq codex dots stop --help`

Exit 0 · stdout:

```text
cq codex dots stop
Stop the Dots adapter after quitting the app.

Usage
  cq codex dots stop [OPTIONS]

Quit the desktop app before stopping its Dots adapter processes.

The result confirms when the adapter has stopped.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Stop the Dots adapter after quitting the app.
  cq codex dots stop
```

### Success

Invocation: `cq codex dots stop`

Exit 0 · stdout:

```text
Codex Dots · stop
  Stopped: Yes
  System app modified: No
```

### Failure

Invocation: `cq codex dots stop`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots stop
```

## `cq codex dots status`

### Help

Invocation: `cq codex dots status --help`

Exit 0 · stdout:

```text
cq codex dots status
Show Dots status.

Usage
  cq codex dots status [OPTIONS]

Read the native catalogue, workspace, and adapter component status.

Readiness describes the observed app state; it does not confirm a Dot has completed a message.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Show Dots status.
  cq codex dots status
```

### Success

Invocation: `cq codex dots status`

Exit 0 · stdout:

```text
Codex Dots · status
  Native catalogue: Ready
  System app modified: No
  Profiles: 2
```

### Failure

Invocation: `cq codex dots status`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots status
```

## `cq codex dots sync`

### Help

Invocation: `cq codex dots sync --help`

Exit 0 · stdout:

```text
cq codex dots sync
Refresh Dots account discovery.

Usage
  cq codex dots sync [OPTIONS]

Refresh account discovery and start or restart adapter components when needed.

A pending normal app restart or incomplete discovery is reported in the result.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Refresh Dots account discovery.
  cq codex dots sync
```

### Success

Invocation: `cq codex dots sync`

Exit 0 · stdout:

```text
Codex Dots · sync
  Native catalogue: Ready
  Discovery complete: Yes
  Catalogue changed: No
  Profiles: 2
```

### Failure

Invocation: `cq codex dots sync`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots sync
```

## `cq codex dots list`

### Help

Invocation: `cq codex dots list --help`

Exit 0 · stdout:

```text
cq codex dots list
List discovered Dots.

Usage
  cq codex dots list [OPTIONS]

Show each discovered Dot's stable handle, display name, account, and setup state.

Use a stable handle or an exact, unambiguous display name with cq codex dots prime.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  List discovered Dots.
  cq codex dots list
```

### Success

Invocation: `cq codex dots list`

Exit 0 · stdout:

```text
Codex Dots · list
  One · dot-one
    Account: primary · Setup: accepted
  Two · dot-two
    Account: secondary · Setup: not_attempted
```

### Failure

Invocation: `cq codex dots list`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots list
```

## `cq codex dots prime`

### Help

Invocation: `cq codex dots prime --help`

Exit 0 · stdout:

```text
cq codex dots prime
Prime one Dot or all discovered Dots.

Usage
  cq codex dots prime [DOT] [OPTIONS]

Request setup for one Dot, or choose --all for every discovered Dot.

Delivery acceptance confirms the request was accepted; the Dot may still be responding.

Arguments
  DOT
      Stable Dot handle or exact unambiguous display name. Choose DOT or --all.
      Non-empty and not whitespace-only when supplied.
      Exactly one of DOT or --all must be selected.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.
  --all
      Prime all discovered Dots instead of selecting DOT.
      Cannot combine --all=true with DOT; --all=false permits DOT.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Prime one Dot by stable handle.
  cq codex dots prime dot-handle
  Prime every discovered Dot.
  cq codex dots prime --all
```

### Success

Invocation: `cq codex dots prime --all`

Exit 0 · stdout:

```text
Codex Dots · prime
Priming
  dot-one · accepted
```

### Failure

Invocation: `cq codex dots prime --all`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots prime
```

## `cq codex dots broadcast`

### Help

Invocation: `cq codex dots broadcast --help`

Exit 0 · stdout:

```text
cq codex dots broadcast
Send a user message to every discovered Dot.

Usage
  cq codex dots broadcast MESSAGE [OPTIONS]

Send one message to every discovered Dot and report each delivery outcome.

Acceptance confirms delivery to the adapter. Inspect unknown outcomes before retrying.

Arguments
  MESSAGE
      Message to send once to every discovered Dot (1 to 3000 characters).
      Valid UTF-8 containing 1 to 3000 Unicode characters; must not be whitespace-only.
      Quote a multi-word message as one argument; use -- before a message beginning with a dash.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Send one message to every discovered Dot.
  cq codex dots broadcast 'Please summarise your current work.'
```

### Success

Invocation: `cq codex dots broadcast "Hello, team!"`

Exit 0 · stdout:

```text
Codex Dots · broadcast
  Broadcast: broadcast-example
Deliveries
  dot-one · accepted
  Name: One
```

### Failure

Invocation: `cq codex dots broadcast "Hello, team!"`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots broadcast
```

### Partial · delivery unknown

Invocation: `cq codex dots broadcast "Hello, team!"`

Exit 0 · stdout:

```text
Codex Dots · broadcast
  Broadcast: broadcast-example
Deliveries
  dot-one · accepted
  Name: One
  dot-two · unknown
  Name: Two
  Failure: TimeoutError
```

Exit 0 · stderr:

```text
Warning · Dots has pending or incomplete work; inspect the receipt before retrying.
  Code: dots_incomplete
  Command: cq codex dots broadcast
```

## `cq codex dots board`

### Help

Invocation: `cq codex dots board --help`

Exit 0 · stdout:

```text
cq codex dots board
Show or assign one local chat as the shared Dot message board.

Usage
  cq codex dots board [THREAD] [OPTIONS]

With no argument, show the shared board binding. Supply THREAD to bind an existing local chat.

Binding a chat announces the board to discovered Dots and reports notice delivery outcomes.

Use --clear to remove the binding.

Arguments
  THREAD
      Existing local thread ID or codex://threads/ID link. Omit to show the binding.
      Exact lower-case UUID or codex://threads/UUID link.
      Cannot combine with --clear=true; --clear=false permits THREAD.

Options
  --root ABS_PATH
      Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after
      the leaf command. This is not a global CQ option.
      Must be a clean absolute path when supplied.
      Must not contain the home directory or overlap native app, credential or application-support
      paths.
  --clear
      Remove the shared message board binding.
      Cannot combine --clear=true with THREAD; --clear=false permits THREAD.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable. Long/short duplicates are rejected.

Examples
  Show the shared board binding.
  cq codex dots board
  Bind one existing local chat as the shared board.
  cq codex dots board codex://threads/01234567-89ab-cdef-0123-456789abcdef
  Remove the shared board binding.
  cq codex dots board --clear
```

### Success

Invocation: `cq codex dots board`

Exit 0 · stdout:

```text
Codex Dots · board
  Board: codex://threads/01a14c1f-6e3c-7622-b4a3-b279ecb2a822
```

### Failure

Invocation: `cq codex dots board`

Exit 1 · stderr:

```text
Error · The Dots operation failed; inspect cq codex dots status before retrying.
  Code: dots_operation_failed
  Command: cq codex dots board
```
