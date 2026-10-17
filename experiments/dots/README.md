# Multi-account Dots

`cq codex dots` defaults to the normal native app, using its existing profile, local tasks and signed-in account. Existing account-owned Dots share that app's sidebar. The packaged CQ binary includes its Python controller and runtime; installation creates private adapter state, a Python environment and loopback certificates. It uses the installed app bundle without patching or re-signing it. Account credential sources and CQ services remain unchanged.

Normal-profile support is under qualification. Use `cq codex dots start` as the CQ launcher for each new app session. This preserves OpenAI's original app signature and uses the existing profile. Direct Dock or Applications launches bypass CQ routing. CQ does not quit an already-running normal app. Installation and account changes report `restart_required` until the user quits it and starts it through CQ.

This remains an optional macOS feature built on private native endpoints. Live qualification on 17 October 2026 used an isolated research profile and covered a fresh installation, a two-owner native catalogue, automatic setup, manual reminders, directory lookup and peer-message delivery. Reinstallation preserved setup guards and skipped both previous attempts. Those results do not qualify the normal profile, cloud computer video or local-task execution on the pinned build; catalogue readiness alone does not prove those outcomes. A separate disposable-profile test verified the private renderer pipe against the installed Electron build.

## Install

Install Python `3.14` separately if it is unavailable, for example with `brew install python@3.14`. This prerequisite applies only to Dots. Sign in to the accounts you want to use with `cq codex login`, and onboard their Dots through the normal native or web UI before discovery.

```sh
cq codex dots install
cq codex dots list
cq codex dots status
```

Installation provisions its own virtual environment with `websockets==17.2`, discovers existing Dots, then starts the normal app when it is not already running. The signed-in system account must have an onboarded Dot before the combined catalogue can start. If discovery finds no onboarded Dots, installation keeps account discovery running without starting the app; onboard the system Dot, then run `cq codex dots sync`.

For disposable research, use `cq codex dots install --mode isolated`. Only that mode creates a separate app-data directory and Codex home. If its fresh window shows welcome or role onboarding, complete that native flow through the role stage. There is no supported Skip for that stage. The window stays open while CQ setup messages and the relay pause; the watcher resumes them once the native workspace is ready. Existing installations without a mode retain their isolated behaviour until explicitly reinstalled.

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

The running account watcher checks source metadata every five seconds. When accounts or declared credentials change, it requests discovery again. A changed normal-profile catalogue requires the user to quit the app and run `cq codex dots start`; existing tasks and transports remain alive until then. Isolated mode can restart its research app automatically. Adding an already-onboarded owner can trigger that owner's first setup attempt once the updated catalogue starts. Removing an owner closes its old runtime sessions on that restart. Failed discovery remains visible as an account availability outcome; a temporary upstream outage can preserve a previously known profile. Temporary backend failures retry with a delay capped at one minute. Invalid local credentials wait for source changes instead.

Request immediate reconciliation, or control the installation manually:

```sh
cq codex dots sync
cq codex dots stop
cq codex dots start
cq codex dots status --json
```

The watcher runs with the adapter. After account changes made while stopped, run `sync` to refresh discovery and resume operation. `start` resumes the saved catalogue and its watcher. Changes made only on the remote service, such as onboarding without changing local account metadata, require `sync`. Credential expiry or revocation still needs the account's normal sign-in flow; this runtime does not refresh credential sources.

Quit the normal app and stop the adapter before reinstalling or upgrading its payload. Reinstallation checks the existing private runtime before replacing payload files and refuses a running installation. Runtime source activation keeps rollback copies; credentials, any isolated user data, setup ledgers and relay state are preserved. There is no public rollback command. Developer source rollback also needs the matching prior candidate before startup; do not start against a candidate whose source hashes differ.

Controls verify process ownership, executable and adapter identity before signalling, and never force termination. Native `stop` refuses while the normal app is alive; quit the app first. No operation changes the normal CQ service or registers a new remote executor.

Normal mode uses a private Chromium debugging pipe and an owner-only Unix socket, with no debugging TCP port. The broker must remain alive for the app session. Ordinary termination and IPC-server failures keep its pipes open until the app exits; forcibly killing the broker or an interpreter crash can close the pipe and cause Electron to quit. The normal updater remains enabled; an updated app needs a newly qualified pin before the next CQ launch.

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
