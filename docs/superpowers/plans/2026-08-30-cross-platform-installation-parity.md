# Cross-platform installation parity implementation plan

> **For agentic workers:** Execute this plan task by task with
> `superpowers:executing-plans`. Use `superpowers:test-driven-development` for
> every behaviour change and `superpowers:verification-before-completion` before
> any completion claim.

**Goal:** Make official CQ installation, upgrade, service lifecycle, and
uninstallation complete and consistent on macOS, Windows, and Linux through a
Homebrew Cask, WinGet installer, and Go installer runner.

**Architecture:** `cq service` owns one transactional lifecycle over proxy and
refresh components. Small platform adapters translate that lifecycle into
LaunchAgents, Task Scheduler tasks, or systemd user units. A separate
`cq-install` command downloads and verifies official release archives, replaces
the binary transactionally, delegates services to `cq service`, and applies
package-manager metadata through platform integrations. Package manifests call
the same installer and lifecycle surfaces; none duplicate service semantics.

**Tech stack:** Go 1.26, standard library, existing `internal/fsutil`, existing
`internal/userdirs`, `golang.org/x/sys`, LaunchServices `launchctl`, Windows
`schtasks.exe` and HKCU registry, Linux `systemctl --user`, GoReleaser v2,
Homebrew Cask DSL, WinGet community manifests, GitHub Actions.

**Spec:**
[`docs/superpowers/specs/2026-08-30-cross-platform-installation-parity.md`](../specs/2026-08-30-cross-platform-installation-parity.md)

**Global constraints:**

- Preserve linked Linux task ownership. This branch owns systemd unit creation
  and mutation. It must consume, not duplicate, Linux supervisor, proxy
  inspection, process attestation, confinement, or transport validation.
- Keep services per-user and non-elevated.
- Keep `headroom-ai` separate and optional.
- Treat plain `go install github.com/jacobcxdev/cq/cmd/cq@latest` and release
  archives as development or portable binaries, not complete installs.
- Keep every HTTP response bounded through `internal/httputil`; never call
  `io.ReadAll` directly on a network response.
- Never print credentials or release signing material.
- Use `go test -race -count=1` for every Go test invocation.
- Keep installer test roots temporary and remove them in `t.Cleanup`, `defer`,
  PowerShell `finally`, or shell `trap`.
- Commit messages follow repository policy: conventional prefix, past tense,
  subject under 50 characters, British English body.

## File and responsibility map

| Area | Files | Responsibility |
|---|---|---|
| Ownership state | `internal/installstate/state.go`, `internal/installstate/state_test.go` | Validate, load, atomically save, and remove `install.json` |
| Shared lifecycle | `cmd/cq/service.go`, `cmd/cq/service_test.go`, `cmd/cq/main.go`, `cmd/cq/help.go`, `cmd/cq/help_test.go` | Parse `cq service`, transact over proxy plus refresh, emit stable status |
| macOS services | `cmd/cq/service_darwin.go`, `cmd/cq/service_darwin_test.go`, `cmd/cq/launchagent_darwin.go`, `cmd/cq/launchagent_darwin_test.go`, `cmd/cq/proxy_darwin.go`, `cmd/cq/proxy_darwin_test.go` | Render, bootstrap, inspect, restart, and remove both LaunchAgents |
| Windows services | `cmd/cq/service_windows.go`, `cmd/cq/service_windows_definition.go`, `cmd/cq/service_windows_definition_test.go`, `cmd/cq/service_windows_test.go`, `cmd/cq/testdata/windows/*.xml` | Render and manage two Task Scheduler tasks without elevation |
| Linux services | `cmd/cq/service_linux.go`, `cmd/cq/service_systemd.go`, `cmd/cq/service_systemd_test.go`, `cmd/cq/service_linux_test.go`, `cmd/cq/testdata/systemd/*` | Render and manage fixed systemd user units and timer |
| Non-native stubs | `cmd/cq/launchagent_stub.go`, `cmd/cq/proxy_stub.go`, `cmd/cq/proxy_http_validation_service_stub.go` | Narrow build tags so native adapters compile on each platform |
| HTTP and archive integrity | `internal/httputil/client.go`, `internal/httputil/client_test.go`, `internal/installer/release.go`, `internal/installer/release_test.go`, `internal/installer/archive.go`, `internal/installer/archive_test.go` | Resolve fixed release assets, bound downloads, verify checksums, safely extract CQ |
| Installer transaction | `internal/installer/installer.go`, `internal/installer/installer_test.go`, `internal/installer/lock.go`, `internal/installer/lock_test.go` | Stage, replace, validate, rollback, uninstall, and serialise mutations |
| Installer destinations | `internal/installer/destination.go`, `internal/installer/destination_test.go`, `internal/installer/platform_unix.go`, `internal/installer/platform_windows.go`, `internal/installer/platform_windows_test.go` | Go path selection, Windows PATH/ARP/uninstaller ownership, package metadata |
| Go/WinGet entrypoint | `cmd/cq-install/main.go`, `cmd/cq-install/main_test.go` | Tagged-version runner and durable Windows installer CLI |
| WinGet generation | `packaging/winget/*.yaml.tmpl`, `internal/tools/wingetmanifest/main.go`, `internal/tools/wingetmanifest/main_test.go` | Render versioned x64/arm64 community manifests from release metadata |
| Release packaging | `.goreleaser.yml`, `.github/workflows/release.yml`, `cmd/cq/release_provenance_test.go` | Build both commands, publish Cask, verify assets |
| CI and acceptance | `.github/workflows/ci.yml`, `.github/scripts/validate-windows-install.ps1`, `.github/scripts/validate-linux-install.sh` | Hosted builds plus disposable native install proof |
| User guidance | `README.md`, `cmd/cq/readme_test.go` | Complete methods first, portable/development limitations explicit |

## Task 1: Persist installation ownership safely

**Files:**

- Create: `internal/installstate/state.go`
- Create: `internal/installstate/state_test.go`

- [ ] Add failing tests for valid round-trip, missing file, unknown schema,
  invalid owner, relative executable, duplicate service IDs, mismatched owner,
  atomic-write failure, and idempotent removal.

  ```go
  func TestStoreRoundTrip(t *testing.T) {
      roots := userdirs.Roots{State: t.TempDir()}
      store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
      want := installstate.Record{
          SchemaVersion: 1,
          Owner: installstate.OwnerGo,
          Version: "0.27.0",
          Executable: filepath.Join(roots.State, "bin", "cq"),
          Services: []string{"dev.jacobcx.cq.proxy", "dev.jacobcx.cq.refresh"},
      }
      if err := store.Save(want); err != nil {
          t.Fatalf("Save() error = %v", err)
      }
      got, err := store.Load()
      if err != nil {
          t.Fatalf("Load() error = %v", err)
      }
      if !reflect.DeepEqual(got, want) {
          t.Fatalf("Load() = %#v, want %#v", got, want)
      }
  }
  ```

- [ ] Run `go test -race -count=1 ./internal/installstate`; expect package
  missing.

- [ ] Implement fixed schema and owner vocabulary.

  ```go
  type Owner string

  const (
      OwnerManual   Owner = "manual"
      OwnerHomebrew Owner = "homebrew"
      OwnerWinGet   Owner = "winget"
      OwnerGo       Owner = "go"
  )

  type Record struct {
      SchemaVersion int      `json:"schema_version"`
      Owner         Owner    `json:"owner"`
      Version       string   `json:"version"`
      Executable    string   `json:"executable"`
      Services      []string `json:"services"`
  }
  ```

- [ ] Implement `Store.Path()` as `filepath.Join(Roots.State,
  "install.json")`; use `fsutil.SecureAtomicWrite` with mode enforced by its
  secure directory boundary. Decode with `json.Decoder.DisallowUnknownFields`.
  Return exported sentinel errors `ErrNotInstalled`, `ErrUnknownSchema`,
  `ErrInvalidRecord`, and `ErrOwnershipConflict`.

- [ ] Implement `CheckClaim(owner, executable)` so same owner plus exact cleaned
  absolute path is idempotent and every other installed claim fails closed.

- [ ] Run `go test -race -count=1 ./internal/installstate`; expect pass.

- [ ] Commit:

  ```text
  feat: added installation ownership

  - persisted versioned installer ownership atomically
  - rejected invalid schemas, owners, and executable paths
  ```

## Task 2: Define shared service status and transaction

**Files:**

- Create: `cmd/cq/service.go`
- Create: `cmd/cq/service_test.go`

- [ ] Add table tests for fresh install, idempotent install, proxy-install
  failure, refresh-install rollback, status failure rollback, ownership
  conflict, restart ordering, uninstall ordering, partial uninstall error
  joining, and JSON schema stability.

  ```go
  type servicePlatform interface {
      Preflight(context.Context, string) error
      InstallProxy(context.Context, string) error
      InstallRefresh(context.Context, string) error
      RestartProxy(context.Context) error
      RestartRefresh(context.Context) error
      RemoveProxy(context.Context) error
      RemoveRefresh(context.Context) error
      Inspect(context.Context) (serviceStatus, error)
  }
  ```

- [ ] Run `go test -race -count=1 ./cmd/cq -run '^TestService'`; expect missing
  lifecycle types.

- [ ] Implement stable status structures. Keep JSON field names exact.

  ```go
  type componentStatus struct {
      ID                   string `json:"id"`
      Manager              string `json:"manager"`
      Registered           bool   `json:"registered"`
      Running              bool   `json:"running"`
      ConfiguredExecutable string `json:"configured_executable,omitempty"`
      LiveExecutable       string `json:"live_executable,omitempty"`
      PID                  int    `json:"pid,omitempty"`
      Listener             string `json:"listener,omitempty"`
      Healthy              bool   `json:"healthy"`
      LastResult           string `json:"last_result,omitempty"`
      Error                string `json:"error,omitempty"`
  }

  type serviceStatus struct {
      SchemaVersion int               `json:"schema_version"`
      Owner         installstate.Owner `json:"owner,omitempty"`
      Executable    string            `json:"executable,omitempty"`
      Proxy         componentStatus   `json:"proxy"`
      Refresh       componentStatus   `json:"refresh"`
      Conflict      string            `json:"conflict,omitempty"`
  }
  ```

- [ ] Implement `installAllServices` transaction: resolve current executable;
  validate owner and version; preflight; claim existing state; install proxy;
  install refresh; poll status for at most 20 seconds; save ownership only after
  both components validate. Roll back installed components in reverse order on
  every post-mutation failure.

- [ ] Implement `restartAllServices`, `statusAllServices`, and
  `uninstallAllServices`. Uninstall must attempt both removals, remove metadata
  only after both are absent, and join errors with `errors.Join`.

- [ ] Run `go test -race -count=1 ./cmd/cq -run '^TestService'`; expect pass.

- [ ] Commit:

  ```text
  feat: added shared service lifecycle

  - transacted proxy and refresh installation together
  - exposed stable component status and rollback errors
  ```

## Task 3: Add `cq service` CLI and stop implicit mutation

**Files:**

- Modify: `cmd/cq/main.go`
- Modify: `cmd/cq/help.go`
- Modify: `cmd/cq/help_test.go`
- Modify: `cmd/cq/main_test.go`

- [ ] Add failing command tests for `service install`, `restart`, `status`,
  `status --json`, and `uninstall`; invalid owner; owner hidden from normal
  help; owner accepted after action; exit code propagation; and ordinary `cq`
  dispatch never calling `ensureAgent`.

- [ ] Run `go test -race -count=1 ./cmd/cq -run
  '^(TestRunService|TestHelpService|TestMainDoesNotEnsureAgent)'`; expect fail.

- [ ] Add command interception beside existing `agent` and `proxy` dispatch.

  ```go
  func runService(args []string) error {
      action, owner, jsonOutput, err := parseServiceArgs(args)
      if err != nil { return err }
      switch action {
      case "install":
          return installAllServices(context.Background(), owner)
      case "restart":
          return restartAllServices(context.Background())
      case "status":
          return printServiceStatus(context.Background(), jsonOutput)
      case "uninstall":
          return uninstallAllServices(context.Background(), owner)
      default:
          return fmt.Errorf("unknown service action %q", action)
      }
  }
  ```

- [ ] Parse hidden `--owner=homebrew`, `--owner=winget`, and `--owner=go`
  flags without listing them in manual help. Default direct invocation to
  `manual`. Reject owner flags on `restart` and `status`.

- [ ] Remove unconditional `ensureAgent()` after ordinary Kong dispatch.
  Preserve focused `cq agent` and `cq proxy` commands.

- [ ] Add exact manual help text and regenerate README command index through
  existing test helpers.

- [ ] Run `go test -race -count=1 ./cmd/cq -run
  '^(TestRunService|TestHelpService|TestMainDoesNotEnsureAgent|Test.*Help)'`;
  expect pass.

- [ ] Commit:

  ```text
  feat: added service command

  - exposed install, restart, status, and uninstall actions
  - removed background mutation from ordinary quota checks
  ```

## Task 4: Reconcile both macOS LaunchAgents

**Files:**

- Create: `cmd/cq/service_darwin.go`
- Create: `cmd/cq/service_darwin_test.go`
- Modify: `cmd/cq/launchagent_darwin.go`
- Modify: `cmd/cq/launchagent_darwin_test.go`
- Modify: `cmd/cq/proxy_darwin.go`
- Modify: `cmd/cq/proxy_darwin_test.go`

- [ ] Add golden-value tests for labels, plist paths, absolute executable,
  arguments, `RunAtLoad`, refresh `StartInterval=1800`, log paths, XML escaping,
  and owner-only file creation.

- [ ] Add command-runner tests requiring `launchctl bootout gui/UID/LABEL`,
  `launchctl bootstrap gui/UID PLIST`, and `launchctl kickstart -k
  gui/UID/LABEL`; tolerate only documented not-loaded `bootout` exit.

- [ ] Add conflict tests for legacy `homebrew.mxcl.cq` and for a CQ label whose
  plist points at another executable.

- [ ] Run `go test -race -count=1 ./cmd/cq -run
  '^(TestDarwinService|TestLaunchAgent|TestProxyAgent)'`; expect fail.

- [ ] Extract shared atomic plist writer and injected launchctl runner. Keep
  existing component functions as wrappers around new adapter.

  ```go
  const (
      proxyLaunchLabel   = "dev.jacobcx.cq.proxy"
      refreshLaunchLabel = "dev.jacobcx.cq.refresh"
      refreshInterval    = 30 * time.Minute
  )
  ```

- [ ] Implement refresh LaunchAgent as exact arguments `cq refresh`, with
  `RunAtLoad` and `StartInterval`. Implement `restartAgent` and inspection of
  both plist and `launchctl print gui/UID/LABEL`.

- [ ] Implement aggregate health by combining launchd identity with existing
  proxy listener and HTTP health validation. Refresh is healthy after successful
  immediate `kickstart` plus registered interval.

- [ ] Run focused tests above; expect pass. Then run
  `GOOS=darwin GOARCH=amd64 go test -c ./cmd/cq` and remove generated
  `cq.test` immediately; expect compile pass.

- [ ] Commit:

  ```text
  feat: reconciled macOS services

  - installed proxy and refresh LaunchAgents atomically
  - verified launchd labels and live executable identity
  ```

## Task 5: Add Linux systemd user adapter

**Files:**

- Create: `cmd/cq/service_linux.go`
- Create: `cmd/cq/service_linux_test.go`
- Create: `cmd/cq/service_systemd.go`
- Create: `cmd/cq/service_systemd_test.go`
- Create: `cmd/cq/testdata/systemd/cq-proxy.service`
- Create: `cmd/cq/testdata/systemd/cq-refresh.service`
- Create: `cmd/cq/testdata/systemd/cq-refresh.timer`
- Modify: `cmd/cq/launchagent_stub.go`
- Modify: `cmd/cq/proxy_stub.go`
- Modify: `cmd/cq/proxy_http_validation_service_stub.go`

- [ ] Add golden tests for fixed filenames and exact unit content.

  ```ini
  [Service]
  ExecStart=/home/test/bin/cq proxy start
  Restart=always
  RestartSec=2
  ```

  ```ini
  [Service]
  Type=oneshot
  ExecStart=/home/test/bin/cq refresh
  ```

  ```ini
  [Timer]
  OnStartupSec=0
  OnUnitActiveSec=30min
  Persistent=true
  Unit=cq-refresh.service
  ```

- [ ] Add runner tests requiring this order: `systemctl --user show-environment`,
  atomic writes, `daemon-reload`, `enable --now cq-proxy.service
  cq-refresh.timer`, `start cq-refresh.service`, inspect. Add rollback and
  idempotent removal cases.

- [ ] Keep unit rendering and injected manager orchestration in untagged
  `service_systemd.go`. Run `go test -race -count=1 ./cmd/cq -run
  '^TestSystemdService'`; expect missing adapter.

- [ ] Implement unit path from `${XDG_CONFIG_HOME}/systemd/user`, falling back
  to `~/.config/systemd/user`. Reject unavailable user manager before writes.
  Render absolute paths without shell indirection and escape systemd arguments
  using a dedicated encoder.

- [ ] Implement `Inspect` from `systemctl --user show` properties
  `LoadState`, `ActiveState`, `SubState`, `MainPID`, `ExecStart`,
  `FragmentPath`, `UnitFileState`, `Result`, and timer timestamps. Consume Linux
  runtime inspection facts when linked branch lands; do not add process or
  transport collectors here.

- [ ] Narrow non-native build tags to `!darwin && !linux && !windows` where
  native functions now exist.

- [ ] Run systemd focused tests with race detection. Cross-compile native Linux
  package with `GOOS=linux GOARCH=amd64 go test -c ./cmd/cq`; remove `cq.test`;
  expect compile pass. Native Linux task runs tagged tests with
  `go test -race -count=1` on Linux.

- [ ] Commit:

  ```text
  feat: added systemd user services

  - managed fixed proxy and refresh units transactionally
  - verified unit definitions through user systemd
  ```

## Task 6: Add Windows Task Scheduler adapter

**Files:**

- Create: `cmd/cq/service_windows.go`
- Create: `cmd/cq/service_windows_test.go`
- Create: `cmd/cq/service_windows_definition.go`
- Create: `cmd/cq/service_windows_definition_test.go`
- Create: `cmd/cq/testdata/windows/proxy-task.xml`
- Create: `cmd/cq/testdata/windows/refresh-task.xml`
- Modify: `cmd/cq/launchagent_stub.go`
- Modify: `cmd/cq/proxy_stub.go`
- Modify: `cmd/cq/proxy_http_validation_service_stub.go`

- [ ] Add golden XML tests for `\\cq\\Proxy` and `\\cq\\Refresh`; current SID;
  `InteractiveToken`; least privilege; logon trigger; refresh 30-minute
  repetition; proxy restart-on-failure; no proxy execution limit; absolute
  command path; arguments separate from command; XML escaping.

- [ ] Add runner tests for `/Create /TN /XML /F`, immediate `/Run`, `/Query /XML`,
  `/End`, and `/Delete /F`; cover already-running and already-absent exit codes.

- [ ] Keep XML rendering and injected task-runner orchestration in untagged
  `service_windows_definition.go`. Run `go test -race -count=1 ./cmd/cq -run
  '^TestWindowsTaskDefinition'`; expect missing implementation.

- [ ] Implement current SID lookup with `windows.OpenCurrentProcessToken` and
  `Tokenuser`; never request administrator privileges or store credentials.

- [ ] Implement task XML and inspection using `schtasks.exe` through injected
  `commandRunner`. Parse returned XML into typed structs and reject mismatched
  SID, action path, arguments, triggers, or task namespace.

- [ ] Integrate existing Windows proxy process/listener health facts instead of
  inferring liveness from Task Scheduler state alone.

- [ ] Run untagged definition tests with race detection. Cross-compile native
  package with `GOOS=windows GOARCH=amd64 go test -c ./cmd/cq`; remove
  `cq.test.exe`; expect compile pass. Copy source to one temporary directory on
  `h1`, run tagged tests with `go test -race -count=1 ./cmd/cq -run
  '^TestWindowsService'`, then remove remote directory.

- [ ] Commit:

  ```text
  feat: added Windows user tasks

  - registered proxy and refresh without elevation
  - verified task principals, actions, and runtime state
  ```

## Task 7: Bound release downloads and archive extraction

**Files:**

- Modify: `internal/httputil/client.go`
- Modify: `internal/httputil/client_test.go`
- Create: `internal/installer/release.go`
- Create: `internal/installer/release_test.go`
- Create: `internal/installer/archive.go`
- Create: `internal/installer/archive_test.go`

- [ ] Add failing `ReadBodyLimit` tests for exact limit, one byte over, negative
  limit, read error, and close ownership. Keep existing 1 MiB `ReadBody`
  behaviour unchanged.

- [ ] Implement `ReadBodyLimit`; make `ReadBody` delegate to it.

  ```go
  func ReadBodyLimit(r io.Reader, limit int64) ([]byte, error) {
      if limit < 0 { return nil, fmt.Errorf("invalid body limit %d", limit) }
      lr := &io.LimitedReader{R: r, N: limit + 1}
      body, err := io.ReadAll(lr)
      if err != nil { return nil, err }
      if int64(len(body)) > limit { return nil, ErrBodyTooLarge }
      return body, nil
  }
  ```

- [ ] Add release mapping tests for six supported OS/architecture pairs and
  reject unsupported OS or architecture. Asset names are
  `cq_0.27.0_windows_amd64.zip` on Windows and
  `cq_0.27.0_linux_arm64.tar.gz` on Linux.

- [ ] Add redirect tests allowing only `github.com`,
  `release-assets.githubusercontent.com`, and `objects.githubusercontent.com`.
  Add non-2xx, archive-over-64-MiB, and checksums-over-1-MiB cases.

- [ ] Add ZIP and tar.gz extraction tests for one `cq`/`cq.exe`; reject absolute
  paths, `..`, links, devices, duplicate executables, unexpected names, extra
  executable entries, oversized files, and checksum mismatch.

- [ ] Run `go test -race -count=1 ./internal/httputil ./internal/installer`;
  expect installer package missing.

- [ ] Implement `Release` with repository fixed to `jacobcxdev/cq`, tagged
  version normalisation, archive URL, checksums URL, expected filename, and
  executable name. Use injected `httputil.Doer` and a client redirect policy.

- [ ] Parse checksum lines as exactly 64 lowercase hexadecimal characters plus
  two spaces plus basename. Require one match for selected archive.

- [ ] Extract into caller-owned temporary directory, create executable with
  `0o700` on Unix, and return exact staged path plus digest.

- [ ] Run `go test -race -count=1 ./internal/httputil ./internal/installer`;
  expect pass.

- [ ] Commit:

  ```text
  feat: verified release downloads

  - bounded GitHub release responses and redirects
  - rejected unsafe archives before installation
  ```

## Task 8: Implement transactional binary install and rollback

**Files:**

- Create: `internal/installer/installer.go`
- Create: `internal/installer/installer_test.go`
- Create: `internal/installer/lock.go`
- Create: `internal/installer/lock_test.go`

- [ ] Build a fake filesystem, downloader, command runner, lifecycle client,
  metadata client, and lock. Add tests for fresh install, idempotent reinstall,
  upgrade, uninstall, staged version mismatch, foreign binary, ownership
  conflict, service failure, status timeout, replacement failure, rollback
  success, rollback validation failure, and temporary-root cleanup.

- [ ] Run `go test -race -count=1 ./internal/installer -run
  '^(TestInstaller|TestInstallLock)'`; expect fail.

- [ ] Define narrow boundaries.

  ```go
  type Lifecycle interface {
      Stop(context.Context) error
      Install(context.Context, installstate.Owner) error
      Status(context.Context) error
      Uninstall(context.Context, installstate.Owner) error
  }

  type PlatformMetadata interface {
      Install(context.Context, Installation) error
      Remove(context.Context, Installation) error
      Inspect(context.Context, Installation) error
  }
  ```

- [ ] Acquire one owner-only lock under state root before network or service
  mutation. Map lock contention to stable `installation_in_progress` error.

- [ ] Implement transaction order: preflight; download and verify; execute
  staged `cq --version`; snapshot old digest and mode; copy old executable to
  same-directory rollback file; stop owned services; atomically replace;
  install services; validate status; install package metadata; persist state;
  sync directory; delete rollback.

- [ ] On post-snapshot failure: uninstall candidate services; restore old
  executable with rename; restore previous services and running state; restore
  metadata; validate rollback. Preserve rollback file and report exact path when
  validation fails.

- [ ] Implement uninstall as service removal, platform metadata removal, exact
  owned binary removal, state removal. Preserve Config, State except
  `install.json`, Cache, Runtime history, and Logs.

- [ ] Run focused installer tests; expect pass.

- [ ] Commit:

  ```text
  feat: added transactional installer

  - restored binaries and services after failed upgrades
  - serialised lifecycle mutations with per-user lock
  ```

## Task 9: Resolve Go destinations and adopt source-built CQ safely

**Files:**

- Create: `internal/installer/destination.go`
- Create: `internal/installer/destination_test.go`
- Create: `internal/installer/platform_unix.go`

- [ ] Add table tests for absolute `GOBIN`, first absolute `GOPATH/bin`, empty
  values, relative values, destination not writable, destination absent from
  `PATH`, Windows case-insensitive PATH equality, and Unix exact PATH equality.

- [ ] Add real temporary binaries built from CQ and from a fixture module. Test
  adoption only when `debug/buildinfo.ReadFile` reports main package
  `github.com/jacobcxdev/cq/cmd/cq`.

- [ ] Run `go test -race -count=1 ./internal/installer -run
  '^(TestGoDestination|TestAdopt)'`; expect fail.

- [ ] Implement destination selection using injected environment and
  `go env GOPATH` fallback. Require absolute, clean, current-user-writable
  directory already represented in PATH. Append `.exe` only on Windows.

- [ ] Implement binary ownership classifier: absent, owned by matching
  `install.json`, adoptable CQ module binary, or foreign. Reject foreign files
  before download.

- [ ] Run focused tests; expect pass.

- [ ] Commit:

  ```text
  feat: resolved Go install paths

  - required writable destinations already on PATH
  - adopted only binaries built from CQ main package
  ```

## Task 10: Add Windows PATH, ARP, and durable uninstaller integration

**Files:**

- Create: `internal/installer/platform_windows.go`
- Create: `internal/installer/platform_windows_test.go`
- Create: `internal/installer/testdata/uninstall.cmd.golden`

- [ ] Add registry-fake tests for fixed install root
  `%LOCALAPPDATA%\\Programs\\cq`, exact PATH insertion/removal, preserving
  pre-existing PATH entry, ARP values, idempotence, wrong-owner refusal, and
  rollback.

- [ ] Add golden test for `uninstall.cmd`. Script must run
  `cq-install.exe uninstall --owner=winget --silent`, preserve exit code, then
  delete exact `cq.exe`, `cq-install.exe`, script, and CQ program directory.

- [ ] Copy source to one temporary directory on `h1`; run
  `go test -race -count=1 ./internal/installer -run '^TestWindows'`; expect
  missing implementation. Remove remote directory after capturing failure.

- [ ] Implement HKCU PATH through
  `Environment` registry key. Track `path_added` in installer metadata so
  uninstall removes only installer-added exact entry. Broadcast
  `WM_SETTINGCHANGE` after successful mutation.

- [ ] Implement ARP key
  `Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\cq` with
  `DisplayName=CQ`, exact version, publisher `jacobcxdev`, install location,
  `NoModify=1`, `NoRepair=1`, and `UninstallString` invoking durable batch file.

- [ ] Persist release-built `cq-install.exe` beside `cq.exe`; write batch file
  atomically. Delete registry state only after service removal succeeds.

- [ ] Cross-compile with `GOOS=windows GOARCH=amd64 go test -c
  ./internal/installer`; remove generated test binary. Copy source to a fresh
  temporary directory on `h1`, run focused tests with race detection, and remove
  remote directory; expect pass.

- [ ] Commit:

  ```text
  feat: added Windows install metadata

  - managed user PATH and Add or Remove Programs state
  - installed durable self-cleaning uninstaller
  ```

## Task 11: Add tagged Go installer command

**Files:**

- Create: `cmd/cq-install/main.go`
- Create: `cmd/cq-install/main_test.go`

- [ ] Add tests for module version `v0.27.0`, devel version rejection, replaced
  module version rejection, install default, explicit uninstall, supported owner
  values, silent output, help, non-zero propagation, and no secret-bearing
  output.

- [ ] Run `go test -race -count=1 ./cmd/cq-install`; expect package missing.

- [ ] Implement build version resolution. Release builds set
  `main.version=0.27.0`; `go run github.com/jacobcxdev/cq/cmd/cq-install@v0.27.0` falls back to
  `debug.ReadBuildInfo().Main.Version`. Reject `(devel)`, empty, and non-semver
  values.

  ```go
  var version = ""

  func effectiveVersion() (string, error) {
      if version != "" { return normaliseVersion(version) }
      info, ok := debug.ReadBuildInfo()
      if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
          return "", errors.New("cq-install requires a tagged module version")
      }
      return normaliseVersion(info.Main.Version)
  }
  ```

- [ ] Parse default `install`, explicit `uninstall`, hidden
  `--owner=go|winget`, and `--silent`. Direct Go runner permits only owner `go`;
  release-built Windows installer permits `winget`.

- [ ] Wire GitHub release downloader, Go or WinGet destination, transaction,
  platform lifecycle invocation of staged/installed `cq service`, and platform
  metadata.

- [ ] Ensure every temporary root uses `os.MkdirTemp` and deferred cleanup.

- [ ] Run `go test -race -count=1 ./cmd/cq-install ./internal/installer` and
  `go run ./cmd/cq-install --help`; expect pass.

- [ ] Commit:

  ```text
  feat: added Go installer runner

  - downloaded tagged official binaries for complete installs
  - supported verified install, upgrade, and uninstall actions
  ```

## Task 12: Generate WinGet community manifests

**Files:**

- Create: `packaging/winget/jacobcxdev.cq.yaml.tmpl`
- Create: `packaging/winget/jacobcxdev.cq.installer.yaml.tmpl`
- Create: `packaging/winget/jacobcxdev.cq.locale.en-US.yaml.tmpl`
- Create: `internal/tools/wingetmanifest/main.go`
- Create: `internal/tools/wingetmanifest/main_test.go`

- [ ] Add golden rendering tests using `0.27.0`, x64 digest
  `1f3f918c6a83f506aaf78021bc0b0b8a5b235f2629caa6e97a1ee59f0f816adc`, and
  arm64 digest
  `7b8e440c1722ca8daa2f8046d48a03b785730860046e40493616e5af0b564f10`.

- [ ] Assert identifier `jacobcxdev.cq`, manifest version `1.10.0`, user scope,
  installer type `exe`, architectures x64/arm64, upgrade behaviour `install`,
  silent switches `install --owner=winget --silent`, ARP display version, and
  pinned HTTPS release URLs.

- [ ] Run `go test -race -count=1 ./internal/tools/wingetmanifest`; expect
  package missing.

- [ ] Implement generator accepting `--version`, `--x64-url`, `--x64-sha256`,
  `--arm64-url`, `--arm64-sha256`, and `--output`. Validate semantic version,
  lowercase SHA-256, fixed GitHub hosts, and empty output directory.

- [ ] Render three YAML files under
  `manifests/j/jacobcxdev/cq/0.27.0`. Parse generated YAML with WinGet
  `winget validate` during native validation; unit tests compare exact bytes.

- [ ] Run focused tests; expect pass.

- [ ] Commit:

  ```text
  feat: generated WinGet manifests

  - described per-user executable installers for both architectures
  - pinned release URLs, hashes, and silent lifecycle switches
  ```

## Task 13: Replace formula publication with Homebrew Cask

**Files:**

- Modify: `.goreleaser.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `cmd/cq/release_provenance_test.go`

- [ ] Add failing provenance tests requiring two builds (`cq`, `cq-install`),
  Windows-only release archive for `cq-install`, `homebrew_casks` instead of
  `brews`, no Python dependency, no `headroom-ai`, lifecycle hooks, both launchd
  cleanup labels, and release verification of Darwin binaries.

- [ ] Run `go test -race -count=1 ./cmd/cq -run
  '^TestReleaseProvenance'`; expect fail.

- [ ] Add `cq-install` build for Windows amd64 and arm64 with
  `-X main.version={{ .Version }}`. Publish each installer as direct
  `cq-install_0.27.0_windows_amd64.exe` or arm64 executable, not an archive.
  Keep `cq` six-platform archive build and release-time Gemini provenance.

- [ ] Replace `brews` with Cask configuration using `binaries: [cq]`, post
  install hook invoking stable Homebrew symlink `cq service install
  --owner=homebrew`, pre uninstall hook invoking `cq service uninstall
  --owner=homebrew`, and uninstall backstops for
  `dev.jacobcx.cq.proxy` and `dev.jacobcx.cq.refresh`.

- [ ] Upgrade `goreleaser-action` to v7 and retain OSS distribution. Run
  `goreleaser check`, then `goreleaser release --snapshot --clean`. Expect
  config and snapshot pass without App Store Connect credentials.

- [ ] Run provenance tests; expect pass.

- [ ] Commit:

  ```text
  build: added signed package artifacts

  - published complete Homebrew Cask lifecycle hooks
  - built Windows installer and verified macOS artifacts
  ```

## Task 14: Document complete methods and migration

**Files:**

- Modify: `README.md`
- Modify: `cmd/cq/readme_test.go`
- Modify: `cmd/cq/help_test.go`

- [ ] Add failing README tests requiring complete methods first, automatic
  proxy and refresh setup, no manual post-install step, explicit Go runner,
  development-only plain `go install`, portable archive caveat, optional
  headroom separation, formula-to-Cask migration, uninstall commands, preserved
  user data, Linux systemd-user requirement, and Windows non-admin scope.

- [ ] Run `go test -race -count=1 ./cmd/cq -run
  '^(TestREADME|TestGeneratedCommandIndex)'`; expect fail.

- [ ] Rewrite installation section with these commands:

  ```text
  brew install --cask jacobcxdev/tap/cq
  winget install jacobcxdev.cq
  go run github.com/jacobcxdev/cq/cmd/cq-install@latest
  ```

- [ ] Document explicit Homebrew migration:

  ```text
  brew services stop cq
  brew uninstall --formula cq
  brew install --cask jacobcxdev/tap/cq
  ```

- [ ] Document uninstalls: `brew uninstall --cask cq`, `winget uninstall
  jacobcxdev.cq`, and `go run github.com/jacobcxdev/cq/cmd/cq-install@latest uninstall`. State user
  configuration, credentials, cache, history, and logs remain.

- [ ] Move plain `go install` and archives under `Development and portable
  binaries`; list missing official provenance and automatic service lifecycle.

- [ ] Regenerate exact command index and run README/help tests; expect pass.

- [ ] Commit:

  ```text
  docs: documented complete installation

  - listed automatic lifecycle methods for each platform
  - separated portable binaries and optional integrations
  ```

## Task 15: Extend hosted CI and disposable native scripts

**Files:**

- Modify: `.github/workflows/ci.yml`
- Create: `.github/scripts/validate-windows-install.ps1`
- Create: `.github/scripts/validate-linux-install.sh`
- Modify: `cmd/cq/release_provenance_test.go`

- [ ] Add provenance tests requiring hosted Windows builds and installer tests,
  Linux race tests, both cross-compiles, and script cleanup guards.

- [ ] Run relevant provenance tests; expect fail.

- [ ] Extend hosted Windows job to run `go test -race -count=1 ./...`, build
  both `cmd/cq` and `cmd/cq-install`, and run Windows adapter/registry tests.
  Keep standard GitHub-hosted runner; add no persistent runner label.

- [ ] Write PowerShell script using one temporary LOCALAPPDATA, APPDATA, CQ
  state root, and install directory. In `finally`, end/delete `\\cq\\Proxy` and
  `\\cq\\Refresh`, remove test HKCU PATH/ARP values, stop CQ processes owned by
  exact temporary executable, and remove temporary root.

- [ ] Make Windows script prove fresh install, reinstall, upgrade, uninstall,
  exact task XML/principal/actions, refresh run, ARP/PATH, process/listener path,
  real authenticated proxy HTTP request, WebSocket upgrade and message, and no
  remaining test state.

- [ ] Write Linux script using temporary HOME/XDG roots. In shell `trap`, stop
  and remove only temporary CQ units/processes, reload user manager, and remove
  temporary root.

- [ ] Make Linux script prove units, timer, immediate refresh, installed
  process/listener identity, real proxy HTTP and WebSocket transport, uninstall,
  and no remaining units. Consume linked Linux acceptance entrypoints after
  integration.

- [ ] Run `actionlint`, PowerShell parser validation, `shellcheck`, and
  provenance tests; expect pass.

- [ ] Commit:

  ```text
  ci: added native installation gates

  - built installer paths on hosted Windows and Linux
  - added disposable lifecycle and transport validation scripts
  ```

## Task 16: Integrate linked Linux runtime work

**Files:**

- Rebase current branch onto stable commit published by task
  `01a04f6f-0a50-7230-b45f-cbe88ae8e9f8`
- Modify only boundary call sites in `cmd/cq/service_linux.go` and
  `.github/scripts/validate-linux-install.sh`

- [ ] Read linked task final SHA and changed-file list. Verify it owns only
  supervisor/worker lifecycle, proxy inspection, attestation, confinement, and
  transport proof.

- [ ] Rebase onto linked commit. Resolve boundaries by calling its exported or
  package-local inspection functions; do not copy implementations.

- [ ] Run `go test -race -count=1 ./cmd/cq ./internal/proxy` with Linux-focused
  selectors first, then full suite.

- [ ] Run disposable Linux validation on a native user-systemd host. Capture
  unit filenames, exact `ExecStart`, process path, listener PID, HTTP result,
  WebSocket result, refresh result, and cleanup proof.

- [ ] If integration requires code changes, commit:

  ```text
  feat: integrated Linux runtime status

  - consumed native attestation in systemd validation
  - bound installed transport proof to package lifecycle
  ```

## Task 17: Full local and cross-platform verification

**Files:** No intended source changes.

- [ ] Run formatting: `gofmt -w` only on changed Go files. Verify
  `git diff --check` passes.

- [ ] Run `go vet ./...`.

- [ ] Run `go test -race -count=1 ./...`.

- [ ] Run build matrix:

  ```text
  GOOS=darwin GOARCH=amd64 go build ./cmd/cq ./cmd/cq-install
  GOOS=darwin GOARCH=arm64 go build ./cmd/cq ./cmd/cq-install
  GOOS=linux GOARCH=amd64 go build ./cmd/cq ./cmd/cq-install
  GOOS=linux GOARCH=arm64 go build ./cmd/cq ./cmd/cq-install
  GOOS=windows GOARCH=amd64 go build ./cmd/cq ./cmd/cq-install
  GOOS=windows GOARCH=arm64 go build ./cmd/cq ./cmd/cq-install
  ```

- [ ] Remove generated binaries immediately. Run `goreleaser check` and
  unsigned snapshot release. Inspect archive names and both installer assets.

- [ ] Run Cask generation and `brew audit --cask --strict` against generated
  Cask. Verify no Python/headroom dependency and exact lifecycle hooks.

- [ ] Run Windows script on SSH host `h1` using copied source in one temporary
  directory. Do not change persistent runner configuration. Copy results back,
  then remove remote directory in `finally`.

- [ ] Run Linux native script after linked task integration. Verify cleanup.

- [ ] Review `git status --short`, `git diff --stat origin/main...HEAD`, and
  `git diff origin/main...HEAD`; confirm every changed line traces to approved
  spec.

## Task 18: Review, PR, merge, release, and deploy v0.27.0

**Files:** GitHub PR, GitHub release, Homebrew Tap Cask, WinGet community
manifest PR.

- [ ] Use `superpowers:requesting-code-review` for final branch diff. Resolve
  every valid finding and repeat targeted plus full verification.

- [ ] Push `jacobcxdev/feat/cross-platform-installer`. Create PR against `main`,
  assign `jacobcxdev`, include spec, test evidence, Windows h1 evidence, Linux
  evidence, migration, security boundaries, and rollback proof.

- [ ] Wait for every required GitHub check and review. Do not merge on partial
  green. Squash-merge after full green and confirm merged commit on `main`.

- [ ] Tag merged commit `v0.27.0` and push tag. Wait for release workflow
  completion without adding an App Store Connect dependency.

- [ ] Verify published assets: six CQ archives, two Windows installer executables,
  checksums, both Darwin binaries, Cask commit in `jacobcxdev/homebrew-tap`, no
  formula update, release provenance, and correct linked version strings.

- [ ] Generate WinGet `0.27.0` manifests from published installer URLs and
  hashes. Run `winget validate`, fork/update `microsoft/winget-pkgs`, open
  community PR, and wait for merge. Assign/reviewer rules follow upstream.

- [ ] Validate published Homebrew Cask on macOS: migrate old formula, fresh
  install, exact LaunchAgents, refresh, real HTTP and WebSocket proxy traffic,
  upgrade/reinstall, uninstall, preserved user data, and cleanup.

- [ ] Validate published WinGet package on `h1`: fresh install, exact tasks,
  refresh, ARP/PATH, real HTTP and WebSocket proxy traffic, upgrade/reinstall,
  uninstall, preserved user data, and cleanup.

- [ ] Validate published Go runner on macOS, Windows `h1`, and Linux native
  systemd user manager: install, upgrade/reinstall, exact services, refresh,
  real HTTP and WebSocket proxy traffic, uninstall, preserved user data, and
  cleanup.

- [ ] Confirm repository working tree and all validation hosts contain no task
  temporary directories. Mark goal complete only after package-manager
  publication and all installed validation gates pass.

## Plan self-review

- [ ] Trace each approved design section to at least one task above.
- [ ] Search plan for unfinished-marker words, omitted file names, and ambiguous
  responsibility; expect no matches or omissions.
- [ ] Check type names and method signatures remain consistent across service,
  installer, platform, and package tasks.
- [ ] Confirm Linux boundary remains one-way: packaging mutates units; runtime
  supplies behaviour and inspection.
- [ ] Confirm release path uses official binaries, checksum verification, no
  App Store Connect dependency, no bespoke runner, and no optional headroom
  install.
