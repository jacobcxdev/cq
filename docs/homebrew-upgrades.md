# Homebrew upgrades on macOS

[Install CQ](../README.md#install) · [Service troubleshooting](troubleshooting.md#installation-and-background-services) · [Release operations](releases.md)

Compatible installations keep their proxy LaunchAgent and listener registered.
CQ stages each executable in private, immutable storage before package cleanup.
It stops admitting new work before waiting for active HTTP requests, SSE streams
and WebSocket turns to finish. Each broker WebSocket session reconnects with
`1012` after its current turn completes; idle sessions reconnect immediately.
Normal package upgrades allow 15 seconds for drainage. If active work cannot
finish in that time, CQ resumes admissions on the previous runtime and defers
the upgrade. The retained candidate refresh service automatically retries the
handoff after deferral; repeating the package upgrade is not required. Inspect
`cq service status --json` for the active/pending versions and upgrade phase.
Raw relayed WebSocket connections
must close before upgrading. New connections can wait in the kernel backlog
during drainage and while the successor starts. A full backlog or loss of both
successor and guard can still interrupt service; this is not an unconditional
zero-downtime promise.

Use `brew upgrade --cask jacobcxdev/tap/cq` for normal package upgrades. Homebrew's
same-token successor callback keeps CQ services installed. Genuine uninstall
removes both jobs, retained runtimes and the upgrade journal after verifying
that all runtime owners released their lifecycle lock. Credentials, caches,
configuration and logs remain available.

After each upgrade attempt, CQ prunes unreferenced retained executables once
runtime selection and package ownership agree. Both LaunchAgent executables
and the latest transaction's previous/candidate pair remain available for
startup and rollback. Saved service snapshots pin their executable references
until the snapshot file is removed; restore verifies those executables before
changing jobs. Pruning shares a lock with staging, snapshot writes and transaction
preparation. Unfinished handoffs, unknown files and unsafe paths retain their
contents. Cleanup errors emit a warning without reverting a completed upgrade.

`cq service status` shows active and pending runtime versions. The JSON form
adds `active_runtime_version`, `pending_runtime_version` and `upgrade_phase`.
Normal package upgrades and explicit bounded upgrades can defer and keep their
predecessor active. Package upgrades wait for the recorded transaction to finish.
A failed package hook exits unsuccessfully instead of claiming the candidate
became active.
A failed successor resumes the retained predecessor. A refresh-job failure
selects the previous runtime through a fresh transaction and restores refresh
and package ownership; failure remains visible to Homebrew for package rollback.

For an explicit compatible runtime selection:

```sh
cq service upgrade --owner=homebrew --candidate-executable=/absolute/path/to/cq --json
```

Add `--wait-timeout=15s` to defer if drainage takes longer than 15 seconds,
keeping the previous runtime active. Valid values are whole seconds from `1s`
through `30s`; for this explicit command, the default or `0` waits indefinitely
for drainage. Normal `service install` and Homebrew upgrades use 15 seconds.

The stable package executable remains the ownership identity. LaunchAgent
arguments name retained runtime copies; the proxy's startup reads the journal
to select the committed copy or recover an unfinished handoff. Compatible
upgrade does not boot out the proxy job. Refresh changes only after runtime
commit. Linux and Windows runtime upgrades are unsupported.

## First adoption

Older CQ releases and previously installed cask metadata cannot use the new
handoff protocol. Finish outstanding turns and stop clients before the first
`brew upgrade --cask jacobcxdev/tap/cq`. Homebrew runs the predecessor's existing
uninstall hook, then installs retained runtime copies and new cask metadata.
Keep clients stopped until startup and real routed traffic are verified. This
transition has a listener gap; it is a maintenance upgrade. Do not edit
Homebrew's installed metadata manually. Subsequent upgrades use the installed
successor callback. Unsupported Homebrew callback signatures fail before
package hooks mutate services.

The guard waits at most five minutes and permits three crash recovery attempts.
A successor crash can recover the retained listener through a private socket,
but recovery must prove the previous supervisor and worker owners are absent.
If release cannot be proved, CQ preserves state rather than starting a competing
coordinator. Simultaneous loss of every listener holder requires a new bind.

## Local native validation

Run `scripts/validate-homebrew-upgrade --candidate /absolute/path/to/cq --predecessor /absolute/path/to/old-cq --output-dir /absolute/path/to/evidence` from the checkout on macOS.
The harness uses the installed Homebrew Ruby APIs, temporary HOME and Caskroom,
unique launchd jobs, a random loopback port and synthetic credentials.
It rebuilds the reviewed source and exact `0.33.11` source with mechanical
isolation changes to job labels, HOME and external I/O. Failure fixtures inject
setup, boot, post-exec and refresh errors. The supplied executables are hashed as inputs;
this run qualifies the source lifecycle, not those exact release bytes or real
Codex compatibility. Release qualification needs a separately authorised run.

Measurements record completed admitted turns, HTTP `200`, WebSocket `101`,
continuation mismatches, `503`, refused connections, maximum admission delay,
managed PID and listener identity. Package CLI status, selected-runtime validation
binding, snapshot/restore and full package removal are checked too. Legacy
bootstrap interruption is recorded
separately. Coordinator process sampling supplements the lifecycle lock fence;
sampling alone cannot prove that no transient overlap occurred. Failed assertions
exit nonzero and preserve the isolated fixture for diagnosis.

## Legacy sandboxed Cask hooks

Homebrew versions that sandbox Cask flight hooks cannot run the service hooks
stored by CQ 0.32.8. For that legacy installation, first verify that
`$(brew --prefix)/bin/cq` is a symlink into the installed CQ Caskroom and that
`cq service status --json` reports the `homebrew` owner and that stable executable.
Keep a copy of the executable for rollback. Run `cq service uninstall
--owner=homebrew --service-executable="$(brew --prefix)/bin/cq"` outside Homebrew,
then remove only that verified symlink before running `brew upgrade --cask cq`.
This makes the old hook skip its executable call; the new installer recreates
the link and services. Keep existing CQ configuration. If the upgrade fails,
restore the stable link to the saved executable and reinstall its Homebrew-owned
services before retrying.


See [troubleshooting](troubleshooting.md#installation-and-background-services) before repairing an installation; the executable and service owner must match.
