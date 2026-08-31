# Cross-platform installation parity

**Status:** Approved

**Date:** 2026-08-30

**Scope:** macOS, Windows, and Linux release installation

## Problem

CQ currently has different installation semantics across platforms. Homebrew
installs a release binary on macOS, but service setup is a separate command.
`go install` builds a binary, reports a development version, omits release-time
Gemini credentials, and cannot execute a post-install hook. Windows and Linux
release archives contain a usable CLI binary but their proxy and refresh service
commands are stubs.

A successful supported installation must install CQ as a complete product. It
must not leave proxy or refresh setup as a manual follow-up.

## Goals

- Give every supported complete installation method the same observable
  install, upgrade, and uninstall behaviour within platform limits.
- Install an official release binary, not a source-built substitute.
- Register and start both CQ background components:
  - the long-running loopback proxy;
  - the periodic credential refresh job.
- Keep all services in the installing user's security context. CQ credentials
  and configuration are user-owned and must never be moved into a machine or
  administrator account.
- Make upgrades transactional and recover the prior working installation when
  replacement or service validation fails.
- Make uninstall stop and remove package-owned services and executables while
  preserving user configuration, credentials, caches, history, and logs.
- Validate real installed behaviour on each operating system without adding a
  bespoke persistent CI runner.

## Non-goals

- Installing optional `headroom-ai`. It remains a separate opt-in dependency.
- Managing Claude, Codex, or Gemini login as part of package installation.
- Running Linux user services after logout on systems without user lingering.
- Supporting a background service on systems without a functional native user
  service manager.
- Treating direct release archives or plain `go install` as complete installs.
- Purging user data during normal uninstall.

## Complete-install contract

An installation succeeds only after all of these conditions hold:

1. `cq` resolves to the selected official release binary from a new shell.
2. `cq --version` reports the selected release version, never `dev`.
3. One proxy service is registered for the current user, is running the exact
   installed binary, owns the configured loopback listener, and passes CQ's
   runtime health check.
4. One refresh job is registered for the current user, invokes the exact
   installed binary, runs once during installation, and is scheduled every 30
   minutes.
5. Installation ownership and exact binary/service identities are recorded in
   CQ's state root.
6. No manual `cq proxy install`, `cq agent install`, service-manager command, or
   reboot is required.

Install failure means the complete contract was not established. Package hooks
must return failure instead of silently accepting a binary-only installation.

Upgrade has the same end state. It stages and verifies the new binary before it
stops the old service, replaces the binary, recreates both jobs with the new
absolute path, starts them, and re-runs installed validation. Failure restores
the old binary, service definitions, and prior running state.

Uninstall stops and removes both jobs, removes the package-owned binary and
installer metadata, and leaves all user data in place. Uninstall is idempotent.

## Supported methods

| Method | macOS | Windows | Linux | Complete |
|---|---:|---:|---:|---:|
| Homebrew Cask | yes | no | no | yes |
| WinGet installer | no | yes | no | yes |
| Go installer runner | yes | yes | yes | yes |
| Plain `go install .../cmd/cq@latest` | development only | development only | development only | no |
| Direct release archive | portable/manual | portable/manual | portable/manual | no |

The supported Go-based command is:

```text
go run github.com/jacobcxdev/cq/cmd/cq-install@latest
```

Literal `go install` cannot meet the contract because the Go command only builds
and copies an executable; it has no post-install hook. Documentation will call
plain `go install` a development binary build, not an installation method. The
dedicated runner is the closest native Go workflow that can perform and verify
the full installation.

## Shared lifecycle command

CQ gains a top-level lifecycle surface used by package hooks and available for
diagnosis:

```text
cq service install
cq service restart
cq service status [--json]
cq service uninstall
```

`cq service install` transactionally reconciles the proxy and refresh jobs for
the current platform. It calls the same platform adapters as the existing
component commands. Existing `cq proxy install|restart|uninstall` and
`cq agent install|uninstall` remain available for focused maintenance.

`cq service status --json` reports stable component state, manager, service ID,
configured executable, live executable, PID where applicable, listener, health,
last refresh result, and ownership conflicts. Package hooks use bounded status
polling rather than parsing human output.

Each mutating command accepts a hidden package-owner value supplied only by the
installer integration: `homebrew`, `winget`, or `go`. Direct operator use records
`manual`. The owner, version, absolute executable path, service identifiers, and
schema version are atomically stored as `install.json` under `userdirs.Roots.State`.
An existing different owner or different executable path is an
`ownership_conflict`; CQ refuses to overwrite it. Removing or migrating an
existing owner must be explicit.

## Platform service adapters

### macOS

Both complete methods use per-user LaunchAgents:

- `dev.jacobcx.cq.proxy` runs `cq proxy start`, starts at load, and stays alive.
- `dev.jacobcx.cq.refresh` runs `cq refresh` at load and every 1,800 seconds.

LaunchAgent program arguments contain the stable absolute installed path. Logs
remain under `~/Library/Logs/cq`. LaunchAgents are installed atomically, loaded
with the current `gui/<uid>` launchd domain, and verified against both plist and
live process identity.

The existing formula-owned `homebrew.mxcl.cq` service is legacy ownership. New
installation refuses to create a second proxy while it exists. The Homebrew
formula-to-Cask migration stops and removes that job before Cask installation.

### Windows

Windows uses Task Scheduler, not the Service Control Manager. A machine service
would run under the wrong account and would require elevation.

- `\cq\Proxy` runs the absolute `cq.exe proxy start` path at user logon, runs
  immediately during install, has no execution time limit, and restarts after
  failure.
- `\cq\Refresh` runs the absolute `cq.exe refresh` path at user logon, runs
  immediately during install, and repeats every 30 minutes.

Tasks use the current user's interactive token and store no password. ACLs and
principal SID are checked after registration. Status validates task definition,
task state, process executable, loopback listener, and proxy health. Uninstall
ends running tasks before deleting their definitions.

### Linux

Linux uses the current systemd user manager:

- `cq-proxy.service` runs `<absolute-cq> proxy start` with `Restart=always`.
- `cq-refresh.service` is a oneshot `<absolute-cq> refresh` unit.
- `cq-refresh.timer` starts the refresh service at session start and every 30
  minutes.

Units live in `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user`, contain no shell
indirection, and use owner-only CQ state and log roots. Installation performs
`daemon-reload`, enables and starts proxy and timer, starts one refresh, and
verifies unit plus process identity. Unit filenames and `ExecStart` forms are
fixed integration contracts; runtime code must not infer package ownership from
another path or process name.

Preflight requires a responsive `systemctl --user`. Unsupported init systems or
an unavailable user manager fail before binary or unit mutation. Lingering is
not enabled automatically because it changes host-level login policy.

### Linux parallel-work boundary

Packaging and distribution owns:

- the shared `cq service` transaction and ownership metadata;
- systemd user unit and timer rendering, atomic installation, reload, enable,
  start, restart, stop, removal, and rollback;
- installer and package hooks, artifacts, and documentation;
- service-manager inspection needed to prove exact unit definitions and active
  executable identity.

Linux runtime compatibility owns:

- supervisor and worker lifecycle behaviour invoked by `cq proxy start`;
- Linux proxy, listener, and process inspection collectors;
- installed process and service attestation consumed by status validation;
- installed HTTP, SSE, and WebSocket validation;
- Linux acceptance confinement and native runtime proof.

The packaging adapter exposes one narrow user-service-manager boundary with
`Preflight`, `Install`, `Start`, `Restart`, `Stop`, `Remove`, and `Inspect`
operations over the fixed three-unit service set. Runtime code supplies CQ
entrypoints and inspection facts; it does not create or mutate systemd units.
Packaging code consumes runtime status and validation; it does not reimplement
Linux supervisor, attestation, confinement, or transport internals. Each branch
publishes a commit SHA before integration, and cross-boundary changes are rebased
onto the owning branch instead of duplicated.

## Homebrew packaging

The Tap moves CQ from a formula to a Cask. Formulae have service declarations
but no formula uninstall hook, so an automatically started formula service can
outlive `brew uninstall`. Casks provide lifecycle hooks and declarative launchd
cleanup, which are required by the complete-install contract.

The Cask:

- installs the release archive's `cq` binary into Homebrew's binary prefix;
- pins the archive SHA-256;
- runs `cq service install` in `postflight`;
- runs `cq service uninstall` in `uninstall_preflight`;
- declares both launchd labels and plist paths as uninstall cleanup backstops;
- preserves CQ user data on uninstall and upgrade;
- fails if postflight validation fails.

The first Cask release removes the formula from normal installation and
publishes a one-time migration command for existing formula users. Migration
first runs `brew services stop cq`, uninstalls the formula, and then installs the
Cask. This cross-artifact transition is explicit because Homebrew cannot upgrade
an installed formula into a Cask. Subsequent `brew upgrade --cask cq` follows the
normal complete upgrade contract.

```text
brew services stop cq
brew uninstall --formula cq
brew install --cask jacobcxdev/tap/cq
```

## WinGet packaging

WinGet portable/ZIP manifests cannot run the lifecycle hooks required here.
Windows releases therefore add an x64 and arm64 per-user installer executable.
The WinGet manifest uses installer type `exe`, user scope, pinned SHA-256, and
documented silent install and uninstall switches.

The installer executable is the release-built `cq-install.exe` bootstrap. It:

1. resolves its own tagged release version;
2. downloads that version's CQ archive and `checksums.txt` from GitHub Releases;
3. verifies the archive SHA-256 before extraction;
4. installs `cq.exe` and a durable uninstaller copy under
   `%LOCALAPPDATA%\Programs\cq`;
5. adds that directory to the current user's `PATH` when absent;
6. invokes `cq service install` with WinGet ownership;
7. registers a per-user Add/Remove Programs entry used by WinGet;
8. validates the complete-install contract before returning success.

Upgrade uses the same executable and preserves the prior binary until new
service validation succeeds. Uninstall uses the durable helper, removes the CQ
directory and only the exact PATH entry it added, and preserves user data.

## Go installer runner

`cmd/cq-install` is an installer, not a second CQ build. `go run ...@latest`
reads its module version from `debug.ReadBuildInfo`, requires a tagged version,
maps `GOOS` and `GOARCH` to the matching release asset, and downloads the
official binary plus checksums directly from that release. It never installs the
source-built installer as `cq`.

The destination matches Go binary conventions: `GOBIN` when set, otherwise the
first `GOPATH` entry plus `bin`. Preflight requires an absolute, user-writable
destination and requires that directory to be present in `PATH`; it does not
edit shell profiles. Thus any successful run needs no manual PATH step. Windows
uses `.exe` as usual.

An unowned binary already at that exact destination is adopted only when Go
build metadata identifies `github.com/jacobcxdev/cq/cmd/cq`. This lets existing
plain-Go users move to a complete install without deleting CQ first while still
refusing to overwrite an unrelated executable named `cq`.

Default action is install-or-upgrade. Explicit `uninstall` removes only an
installation whose owner is `go`:

```text
go run github.com/jacobcxdev/cq/cmd/cq-install@latest
go run github.com/jacobcxdev/cq/cmd/cq-install@latest uninstall
```

## Download and integrity rules

- Release URLs are derived from the installer's own tagged version, operating
  system, and architecture. User-supplied arbitrary URLs are not supported.
- HTTP redirects are restricted to GitHub release hosts. Response bodies and
  extracted archive entries are bounded.
- The archive digest must match `checksums.txt`; mismatch fails before mutation.
- Cask and WinGet manifests separately pin the first downloaded artifact.
- Archive extraction rejects absolute paths, traversal, links, duplicate CQ
  binaries, and unexpected executable names.
- Staged files use user-only permissions where supported. Replacement uses
  same-directory rename and directory durability barriers where supported.
- Installer output never includes credentials, tokens, environment contents, or
  CQ configuration.
- Official release binaries retain release-time Gemini OAuth material and other
  provenance link data. Source-built CQ binaries do not substitute for them.
- Darwin release binaries used by the Homebrew Cask are signed with the existing
  Developer ID Application identity and notarised by Apple before archiving.
  Installation never clears Gatekeeper quarantine as a signing substitute.

## Transaction and recovery

One per-user installer lock serialises all lifecycle mutations. Before mutation,
the installer records current owner, binary digest and version, service
definitions, service running state, and listener ownership.

Install and upgrade proceed in this order:

1. Resolve platform prerequisites and reject foreign listener or install owner.
2. Download, bound, extract, checksum, and execute `cq --version` from staging.
3. Save the current binary and service definitions as rollback inputs.
4. Stop only CQ-owned jobs.
5. Replace the binary atomically.
6. Reconcile proxy and refresh jobs.
7. Start both jobs and run bounded installed validation.
8. Commit installer ownership metadata and remove rollback files.

Any failure after step 3 stops the candidate jobs, restores the prior binary and
definitions, restores their prior running state, and validates that restoration.
If restoration cannot be proved, the installer exits non-zero with exact paths
and service identifiers but does not delete evidence needed for recovery.

Temporary download, extraction, and rollback directories are created beneath a
validated per-user temporary root. They are removed on success and after proved
rollback. Tests and remote validation remove their temporary roots in `defer` or
equivalent finally blocks.

## Compatibility and migration

- Existing macOS `dev.jacobcx.cq.proxy` and refresh LaunchAgents are adopted only
  when their executable resolves to the selected binary and no owner metadata
  conflicts.
- Existing `homebrew.mxcl.cq` is removed only by the explicit formula-to-Cask
  migration path.
- Windows and Linux stubs become native implementations without changing proxy
  protocol, credential, state-root, or loopback authentication semantics.
- The implicit macOS `ensureAgent` call after ordinary CQ commands is removed.
  Complete installers own background setup; ordinary quota checks must not
  mutate service registration.
- Component service commands stay backward compatible. Complete installers use
  the new aggregate command so users do not need to know component topology.
- A newer installer schema may read older ownership metadata, but unknown newer
  schemas fail closed.

## Tests and release gates

### Automated tests

- Installer orchestration tests cover fresh install, idempotent reinstall,
  upgrade, uninstall, checksum failure, ownership conflict, foreign listener,
  service failure, rollback success, and rollback failure.
- Platform adapter tests use injected command, filesystem, process, registry,
  and HTTP boundaries. Every service definition has a golden fixture.
- Windows cross-compilation and Linux/macOS builds run in normal GitHub-hosted
  CI. The full Go suite continues to use `go test -race -count=1 ./...`.
- Release configuration tests require all release assets, checksums, Cask hooks,
  WinGet manifests, architectures, and silent switches to agree on version and
  digest.

### Native installed validation

- macOS validates fresh Cask install, Cask upgrade, Cask uninstall, Go-runner
  install/upgrade/uninstall, exact LaunchAgent identity, proxy HTTP and
  WebSocket traffic, refresh execution, and cleanup.
- Windows validates WinGet local-manifest install/upgrade/uninstall and Go-runner
  install/upgrade/uninstall on existing host `h1`, including exact Task
  Scheduler definitions, process/listener identity, real proxy HTTP and
  WebSocket traffic, refresh execution, PATH/ARP state, and cleanup.
- Linux validates Go-runner install/upgrade/uninstall on a disposable native
  systemd-user environment, including units, timer, process/listener identity,
  real proxy traffic, refresh execution, and cleanup.

No new persistent or bespoke CI runner is required. `h1` is an on-demand native
release gate, not repository CI infrastructure. Release acceptance tests the
published assets and package manifests, not only source builds.

## Documentation changes

README installation guidance will list complete methods first:

- macOS: `brew install --cask jacobcxdev/tap/cq`;
- Windows: `winget install jacobcxdev.cq`;
- macOS, Windows, Linux with Go: `go run .../cmd/cq-install@latest`.

It will state that successful complete installs start proxy and refresh services
automatically. Plain `go install` and direct archives move to a clearly labelled
development/portable section with their missing release and lifecycle
capabilities listed.

## External constraints

- Go's install command builds and copies executables but exposes no package
  post-install hook: <https://go.dev/ref/mod#go-install>
- WinGet community manifests prohibit arbitrary scripts, so complete setup needs
  an installer executable: <https://github.com/microsoft/winget-pkgs/blob/master/.github/instructions/manifests.instructions.md>
- Homebrew Casks expose install and uninstall lifecycle hooks needed for exact
  service cleanup: <https://docs.brew.sh/Cask-Cookbook>
- Windows Task Scheduler supplies per-user logon and periodic execution:
  <https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks>
