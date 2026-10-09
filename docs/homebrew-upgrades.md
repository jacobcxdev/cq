# Homebrew upgrades on macOS

Compatible installations keep their proxy LaunchAgent and listener registered.
CQ stages each executable in private, immutable storage before package cleanup.
It waits up to 30 seconds for active HTTP requests, SSE streams and WebSocket
turns to finish. Idle broker WebSocket sessions do not block that boundary;
clients reconnect after the runtime changes. Raw relayed WebSocket connections
must close before upgrading. New connections can wait in the kernel backlog
while the successor starts. A full backlog or loss of both successor and guard
can still interrupt service; this is not an unconditional zero-downtime promise.

Use `brew upgrade jacobcxdev/tap/cq` for normal package upgrades. Homebrew's
same-token successor callback keeps CQ services installed. Genuine uninstall
removes both jobs, retained runtimes and the upgrade journal after verifying
that all runtime owners released their lifecycle lock. Credentials, caches,
configuration and logs remain available.

`cq service status` shows active and pending runtime versions. The JSON form
adds `active_runtime_version`, `pending_runtime_version` and `upgrade_phase`.
A busy runtime defers the upgrade and keeps its predecessor active. The package
hook exits unsuccessfully instead of claiming the candidate became active.
A failed successor resumes the retained predecessor. A refresh-job failure
selects the previous runtime through a fresh transaction and restores refresh
and package ownership; failure remains visible to Homebrew for package rollback.

For an explicit compatible runtime selection:

```sh
cq service upgrade --owner=homebrew --candidate-executable=/absolute/path/to/cq --json
```

The stable package executable remains the ownership identity. LaunchAgent
arguments name retained runtime copies; the proxy's startup reads the journal
to select the committed copy or recover an unfinished handoff. Compatible
upgrade does not boot out the proxy job. Refresh changes only after runtime
commit. Linux and Windows runtime upgrades are unsupported.

## First adoption

Older CQ releases and previously installed cask metadata cannot use the new
handoff protocol. Finish outstanding turns and stop clients before the first
`brew upgrade jacobcxdev/tap/cq`. Homebrew runs the predecessor's existing
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
boot, post-exec and refresh errors. The supplied executables are hashed as inputs;
this run qualifies the source lifecycle, not those exact release bytes or real
Codex compatibility. Release qualification needs a separately authorised run.

Measurements record completed admitted turns, HTTP `200`, WebSocket `101`,
continuation mismatches, `503`, refused connections, maximum admission delay,
managed PID and listener identity. Legacy bootstrap interruption is recorded
separately. Coordinator process sampling supplements the lifecycle lock fence;
sampling alone cannot prove that no transient overlap occurred. Failed assertions
exit nonzero and preserve the isolated fixture for diagnosis.
