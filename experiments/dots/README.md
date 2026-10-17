# Isolated Dots

`cq codex dots` runs existing account-owned Dots in a separate native Codex window. The packaged CQ binary includes its Python controller and runtime. Installation creates private app data, a Codex home, a Python environment and loopback certificates. It uses the installed app bundle without patching or resigning it, and leaves system Codex credentials and CQ services unchanged.

This remains an optional macOS feature built on private native endpoints. Live qualification on 17 October 2026 covered a fresh installation, a two-owner native catalog, automatic setup, manual reminders, directory lookup and peer-message delivery. Reinstallation preserved setup guards and skipped both previous attempts. These checks do not qualify cloud computer video or local-task execution on the pinned build; catalog readiness alone does not prove those outcomes.

## Install

Install Python `3.14` separately if it is unavailable, for example with `brew install python@3.14`. This prerequisite applies only to Dots. Sign in to the accounts you want to use with `cq codex login`, and onboard their Dots through the normal native or web UI before discovery.

```sh
cq codex dots install
cq codex dots list
cq codex dots status
```

Installation provisions its own virtual environment with `websockets==17.2`, discovers existing Dots, then starts the isolated app. Use the CQ-launched window for the combined catalog. The signed-in system account must have an onboarded Dot before the combined window can start. If discovery finds no onboarded Dots, installation keeps account discovery running without starting the window; onboard the system Dot, then run `cq codex dots sync`.

If the fresh isolated window shows welcome or role onboarding, complete that native flow through the role stage. There is no supported Skip for that stage. The window stays open while CQ setup messages and the relay pause; the watcher resumes them once the native workspace is ready.

The default installation root is `~/.local/share/cq/dots`. Account discovery reads the signed-in account at `~/.codex/auth.json` and CQ accounts under `~/.codex/accounts`. These credential sources remain read-only. Optional CodexBar discovery reads explicitly declared managed accounts; it does not scan arbitrary account homes or refresh their credentials.

Use absolute paths to override the installation or account sources:

```sh
cq codex dots --root /absolute/private/dots install \
  --app /Applications/ChatGPT.app \
  --system-auth /absolute/private/system/auth.json \
  --cq-accounts /absolute/private/accounts \
  --account-source /absolute/private/codexbar \
  --python /absolute/path/to/python3.14
cq codex dots --root /absolute/private/dots list --json
```

`--account-source` and `--python` are optional. Without `--python`, installation uses `python3.14` from `PATH`; later commands use the installed private virtual environment. The installer generates private configuration, so no hand-written settings file, profile IDs or research directory is needed.

The installation root must be private and outside source checkouts, app bundles and production credential or app-data directories. Existing roots must belong to the current user and have mode `0700`; configuration and payload sources use `0600`. Symlink destinations are rejected. The installer accepts only the exact app version, build and renderer pin in [cli.py](cli.py). An app update requires qualification and a compatible CQ build.

The qualified native app permits authenticated local proxy requests on `localhost:8000`. Dots therefore requires loopback port `8000` to be free. If another service owns it, startup stops without terminating that service or choosing an unsupported port.

## Setup and manual reminders

When the runtime starts, each discovered Dot gets one automatic setup attempt for that installation and stable owner/profile identity. This is an ordinary message from CQ through the native account. It explains explicit relay messages, stable handles and the on-demand peer directory. It requests a brief acknowledgement and does not ask Dots to contact peers immediately.

The setup ledger survives restarts, CQ upgrades, display-name changes and account removal followed by re-addition. Automatic setup does not retry a recorded attempt whose outcome is uncertain. `list` reports each Dot’s setup outcome; acceptance is transport evidence, not proof that the Dot understood or remembered the message.

Send another setup reminder explicitly when needed:

```sh
cq codex dots prime DOT_HANDLE
cq codex dots prime "Unique display name"
cq codex dots prime --all
```

Copy `DOT_HANDLE` from the `id` field returned by `list`. The selector accepts an exact stable handle or an exact display name when that name identifies one Dot. Names are case-sensitive. Duplicate display names require their stable handles; account labels and profile IDs are not alternative selector formats. Handles remain tied to the owner/profile identity when names change. A manual `prime` sends a fresh reminder, including after an unknown automatic outcome.

The directory shares configured handles and display names. Roles and current work remain unknown until a peer explicitly describes them. The relay does not mirror private history, memories or connector authorisations, and peer messages cannot grant user permissions.

## Account changes and lifecycle

The running account watcher checks source metadata every five seconds. When accounts or declared credentials change, it requests discovery again and reconciles the isolated runtime. A changed catalog restarts the isolated app; adding an already-onboarded owner can trigger that owner’s first setup attempt. Removing an owner removes its Dot and closes the old runtime sessions during reconciliation. Failed discovery remains visible as an account availability outcome; a temporary upstream outage can preserve a previously known profile. Temporary backend failures retry with a delay capped at one minute. Invalid local credentials wait for source changes instead.

Request immediate reconciliation, or control the installation manually:

```sh
cq codex dots sync
cq codex dots stop
cq codex dots start
cq codex dots status --json
```

The watcher runs with the isolated runtime. After account changes made while stopped, run `sync` to refresh discovery and resume operation. `start` resumes the saved catalog and its watcher. Changes made only on the remote service, such as onboarding without changing local account metadata, require `sync`. Credential expiry or revocation still needs the account’s normal sign-in flow; this runtime does not refresh credential sources.

Stop the isolated installation before reinstalling or upgrading its payload. Reinstallation checks the existing private runtime before replacing payload files and refuses a running installation. Runtime source activation keeps rollback copies; credentials, isolated user data, setup ledgers and relay state are preserved. There is no public rollback command. Developer source rollback also needs the matching prior candidate before startup; do not start against a candidate whose source hashes differ.

Controls verify process ownership, executable and isolated data or working directory before signalling, and never force termination. No operation changes the normal CQ service or registers a new remote executor.

## Current limits

Discovery includes each available owner’s existing primary Dot. Empty accounts do not gain onboarding slots. Native cross-account `@` selection and persistent secondary Reboot remain unsupported. Fresh local-task execution and its nested macOS sandbox remain unqualified; an executor appearing Connected does not establish general local-task support.

Explicit relay messages carry source attribution and a correlation ID, with at most one request and one reply per correlation. The relay polls the last 20 messages every five seconds and can miss messages after a long outage or heavy traffic. Unknown send outcomes are not retried automatically. Cloud sessions, plugins, memories and connectors remain account-owned. The native sender identity remains the signed-in account.

## Offline validation

These tests use temporary state and fake processes or upstream responses. They do not establish live installation or delivery readiness.

```sh
python3.14 -m unittest discover -s experiments/dots/runtime -q
python3.14 -m unittest discover -s experiments/dots/tests -q
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Run the Python suites in a development environment with `websockets==17.2` installed.
