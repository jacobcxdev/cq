# CLI v2 local release evidence — 1.0.0

**Local preparation checkpoint only. Publication, native installation and release acceptance are blocked.** No authorised native target or disposable user has been supplied. No tag, push, PR, merge, GitHub status, release, package installation, service mutation or real reset consumption was performed for T28.

## Immutable source and retained artefacts

- Reviewed T27 base: `659891406fa81acd7d04694ead78d0788415dac7`.
- Signed candidate source: `43638f3a2486815416e4cbd4cbab63efad71d285` (`ci: targeted the CLI v2 release line`). Signature verified. The build began with a clean tracked/nonignored worktree; all six Go build records report that revision and `vcs.modified=false`.
- Runtime/product bytes are unchanged from the T27 base. The source checkpoint changed only the CI candidate version selection, Windows user-directory gate and their existing configuration assertions.
- This document is committed later as evidence only. That later commit is **not** the executable's source revision. No self-referential future commit hash is claimed and no rebuild is implied by documentation changes.
- Exact version linked into every binary: **1.0.0**. Future CI package validation deliberately uses **1.0.0-ci**, retaining its existing CI-only local tag mechanism and previous published-release lookup. That workflow was not run; no temporary tag was created here.
- Build host: macOS arm64. Go `go version go1.27.1 darwin/arm64`. GoReleaser 2.18.2. `CGO_ENABLED=0`, amd64 v1 and arm64 v8.0. Cross-builds establish compilation and metadata only.

All retained paths below are relative to this exact local evidence directory:

```text
/Users/jacob/Developer/src/github/jacobcxdev/cq/.worktrees/cli-v2/.superpowers/sdd/plan/task-28-evidence
```

| Target | Archive under `archives/` | Archive SHA-256 | Executable SHA-256 |
|---|---|---|---|
| darwin/amd64 | `cq_1.0.0_darwin_amd64.tar.gz` | `42d4bfa6ec1e12ccf3801edce1fc4341aaf10269b66a2358141667260bbf6896` | `f849dab7f9caf3e066f57a5332121dfbccaa6748d30fae28ba239b8e6a4a60a9` |
| darwin/arm64 | `cq_1.0.0_darwin_arm64.tar.gz` | `eb91691016d20450ee652417c7b4014a4f696b902ed2327741bd762e7b3bdb44` | `08493839ccd1c55afa0ea36860c048eaad4ac1e730af727e524bf4a4aba9f201` |
| linux/amd64 | `cq_1.0.0_linux_amd64.tar.gz` | `90d7961654496069a70cd97fd48f834174279fb2900fa1fb1a7cd18a40d5a6a5` | `ace37419df4fc7ed7561bfe603cc1565084896d8b5b8111a85dc1a053652c653` |
| linux/arm64 | `cq_1.0.0_linux_arm64.tar.gz` | `cc730b7971306e688a8c1c9f77c92ae898e335ad0fb5ae86afdca434f146c6a8` | `0e192a456d2d6959520959208006d6a35228d2dd54c959d50d36713af91bc321` |
| windows/amd64 | `cq_1.0.0_windows_amd64.zip` | `a8cec1b08f14d1f90952ed936ef67bfcf663389d86f0ca15a31cce1810aafd66` | `bf5d28c059aded041eeaeca89743221acc94d0c1558c354828ebdbc7221dd6c2` |
| windows/arm64 | `cq_1.0.0_windows_arm64.zip` | `6b44412e6d6492506c19b9bb3c5ac1e5ce96a608094be189642f655e46110fab` | `0575fe2b481e8feef285c9178af276d220853f43543cdef495bee637d31e8625` |

`archives/checksums.txt` contains the six archive digests. `artifact-manifest.json` binds every binary, archive member, embedded-help result and build-info record to the source checkpoint; SHA-256 `960ebc6a0ff42cfcbd420f01e8d21eef4bedec2ca6250d71e8f5f83413955570`. Archive verification reopened every archive and compared every member byte with its source. Each contains `cq` (or `cq.exe`), `README.md` and `specs/cli-v2/COMMANDS.md`, matching the tracked allowlist. These are local tar.gz/zip candidates, **not** an MSI, signed/notarised package, generated Cask or published WinGet artefact.

Build used the current root `.goreleaser.yml` through an isolated copy, adding only an evidence output directory and `snapshot.version_template: 1.0.0`. Exact command:

```sh
goreleaser build --snapshot --config '/Users/jacob/Developer/src/github/jacobcxdev/cq/.worktrees/cli-v2/.superpowers/sdd/plan/task-28-evidence/goreleaser-local.yml'
```

Root configuration SHA-256: `5b08cc9348a7ee1d17585164d5b6f6dea3eb4ef9eefbdcbbf9cfdde85b6cfe90`. Isolated configuration SHA-256: `6048ef7637b5475cfb60dd6e392a253317bfb564988f8b2e4b4cb95ba7ec5964`. The build-only command produced the six binaries; retained `package-local.py` assembled archives with Python's tar/gzip/zip support using the current names, formats and file allowlist. It did not invoke GoReleaser release or package lifecycle scripts. A first attempt used unsupported `snapshot.version`, failed before building, and is retained in `six-target-build.log` with `goreleaser-invalid-initial.yml`; installed-tool JSON schema supplied the corrected field.

The GoReleaser metadata still reports pre-existing Git tag `v0.32.5` as its discovery context. That is neither this candidate's version nor a new tag. The metadata version, linked version and source commit are separately verified.

The release-time Gemini OAuth client secret was **explicitly empty** in the isolated build environment; no secret was read, sourced, copied or printed. These binaries cannot establish Gemini release qualification. The unchanged release requirement still requires the real release-time secret before a publishable build. No placeholder was treated as production authority. Stage-11 provenance is derived from the existing reviewed manifest for version 1.0.0: `4dd0c380fcff85171d29028c65e8cb4f58357c9d0ce98384b1344ecedca46f41`. This preserves the existing linker flag; it does not confer historical CU acceptance.

## Gate results and boundaries

| Gate | Result at this checkpoint | Evidence / remaining requirement |
|---|---|---|
| Plan and generated catalogue | Passed | Baseline plus final plan/generator checks, including `--go-output internal/cli/catalogue_gen.go`. 89 leaves, 37 groups, 127 help pages. |
| Build / vet | Passed | `go build ./...`; `go vet ./...` on source checkpoint. |
| Final integrated full race | **Failed** | `go test -race -count=1 -json ./...`: 4,134 top-level passes, four failures, 29 skips; no race report. Full log preserved. |
| Four affected fixture tests | Passed separately | Same source, `-race`, four passes/no skips after correcting only the synthetic TMPDIR group. This does not rewrite the full run as passed. |
| All six artefacts | Local build/archive checks passed | Exact version, source revision, clean provenance, build flags, archive bytes and all 127 full help byte strings checked per binary. No foreign executable was run. |
| Extracted darwin/arm64 executable | Bounded runtime checks passed | All 127 exact help outputs; 127 schema 2 syntax-error envelopes; version 1.0.0/schema 2; three malformed machine forms and six valid child forms failing closed without inherited descriptors. No valid package/service authority exercised. |
| Packaged Bash/Zsh/Fish completion | Passed on host | Actual Tab/Enter PTY checks using completion output from the extracted binary: stock Bash, PATH Bash, Zsh and Fish; inline enum, Unicode path and command/directory collision, 12 cases. No Linux/Windows shell runtime claim. |
| Package consumer / machine ABI | Local synthetic coverage passed | Source race suite covered frozen classifier/dispatch corpus, synthetic package JSON consumers, installer transaction/rollback fixtures. Extracted-binary rejection checks above supplement this; successful installed machine ABI remains unverified. |
| macOS native launchd/quarantine/Cask | **Unverified** | No approved disposable native user. Disabled proxy `load -F` remains provisional: require unchanged disabled policy, correct GUI domain/PID/executable and authenticated health. Disabled refresh must preserve installed roots/environment, exact-once receipt and cancellation/reaping. |
| Linux native systemd/confinement | **Unverified** | Require both supported architectures, selected-component policy and unit-bound roots, refresh receipts, T17 signal/cause/cleanup, namespaces/Landlock and descendant cancellation. Cross-build does not run them. |
| Windows native roots/scheduler/WiX | **Unverified** | Require native userdirs/fsutil race execution, actual matched reset/account tests, scheduler GUID/EnginePID/immediate-child identity, auxiliary ownership/completion, MSI upgrade failure rollback and WinGet ownership. Cross-build and parent HOME overrides prove none of these. |
| T22 removal authority | Partial historical local evidence only | Earlier Darwin temporary-owned-child proof is not installed acceptance; Linux/Windows process/removal checks remain open. Windows delete request alone is not final unlink proof. |
| T11 vendor model cache tolerance | **Unverified** | Digest/payload tests do not prove installed Codex/Claude tolerate CQ-native metadata. Validate only in an approved preserved-state client environment. |
| Installed transport | **Unverified** | Exact installed executable and approved real HTTP 200 plus WebSocket 101, zero 503 and zero continuity mismatch remain mandatory. Health/status alone cannot close this gate. |
| Historical CU-0/CU-1 signed release proof | **Unavailable / blocking** | Frozen rosters bind removed public-parser tests. Separate reviewed provenance work is required; compatibility-only roster cannot substitute. |
| Publication / rollout | **Not authorised** | No target host/user, previous installation baseline or publication authority. No release-ready claim. |

Canonical HTTP validation remains unavailable before IO on Linux and Windows (`validation_candidate_unavailable`, exit 4); Darwin requires an existing attested candidate. This does not waive Linux WebSocket/fixture/readiness/native legacy gates. Four reserved candidate commands remain unavailable, exit 4.

## Isolation, failures and skipped tests

The final full run started 2026-09-23T12:21:26.218892+00:00 and lasted 125.573 seconds. The runner supplied an allowlisted environment instead of inheriting credentials or live opt-in flags. Fresh task roots were:

```text
HOME=/private/tmp/cq28.266pjizt/h
XDG_CONFIG_HOME=/private/tmp/cq28.266pjizt/c
XDG_CACHE_HOME=/private/tmp/cq28.266pjizt/k
XDG_STATE_HOME=/private/tmp/cq28.266pjizt/s
XDG_DATA_HOME=/private/tmp/cq28.266pjizt/d
CODEX_HOME=/private/tmp/cq28.266pjizt/codex
CLAUDE_CONFIG_DIR=/private/tmp/cq28.266pjizt/claude
TMPDIR=/private/tmp/cq28.266pjizt/tmp
```

Explicit existing tool/cache paths were retained: GOROOT `/opt/homebrew/Cellar/go/1.27.1/libexec`, GOPATH `/Users/jacob/go`, GOMODCACHE `/Users/jacob/go/pkg/mod`, GOCACHE `/Users/jacob/Library/Caches/go-build`; GOTOOLCHAIN=local, GOPROXY=off, GOSUMDB=off. PATH was `/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`. Exact nonsecret environment is retained in every gate JSON and `environment.json`. Host binary checks used the same isolated roots. These Unix root overrides are additional containment, not native Windows-root qualification.

The four failures all requested setgid fixtures under the new temporary tree. On this host `/private/tmp` descendants inherited group 0 (`wheel`), which is not a caller group. A retained synthetic-only probe showed requested mode 02700 became 0700; assigning that synthetic file group 20 and repeating chmod retained 02700. Production rejection checks therefore saw a file without the prohibited bit. No production code or assertions were changed.

A new task-owned temporary directory `/private/tmp/cq28.266pjizt/tmp-staff` with caller group 20 was used for the source-identical covering run. Only TMPDIR changed; all other roots stayed isolated. Exact test selection:

```sh
go test -race -count=1 -json ./cmd/cq ./internal/proxy -run '^(TestCaptureCodexInstalledServiceConfigurationRejectsSpecialModes|TestReadInstalledHTTPValidationRegularFileRejectsSpecialModes|TestDarwinServiceFreshInstallExecutableAuthority|TestSystemdServiceSelectedPreflightSafety)$'
```

All four passed, with no skips. `setgid-isolation-probe.json`, `targeted-environment-delta.json` and both original/covering logs remain available. Mock manager transcripts in the failures are fixture observations, not live launchctl/systemd operations. No second full suite was run.

The first extended Fish PTY attempt timed out because the new Fish terminal queried device attributes. The retained evidence harness now replies to the terminal capability handshake; it does not replace or simulate completion results. All 12 actual Tab/Enter cases then passed. Both attempts and harness versions are retained.

The 29 top-level skips remain skips. `skipped-tests.json` records each exact name, emitted reason and gate mapping; SHA-256 `a0caf5407cbcdebc3eee09c617b94d642443998b31adc5a8b8cb7fa057eb5cf2`. They include 14 unavailable live/installed checks (including process attestation), one unavailable vendor-cache read, eight retired direct-credential-refresh cases, four retired app-server-facade cases, one subprocess helper and one unavailable empty-PATH premise. The exact inventory is authoritative; no skip substitutes for runtime acceptance. Platform-excluded Linux/Windows files are additional unexecuted native work and do not appear as runtime skips on Darwin.

T27 history is also retained: its earlier full race run failed 17 top-level tests, followed by 912 affected-package passes and later source-matched checks. During that run, shared auth/models fixtures could derive ambient credential-control roots. Existing-owner RPC, delegated refresh or other writes cannot be excluded; mock HTTP and later lstat timestamps do not prove absence of effects. T27 fixed all four fixture callers to use explicit roots; T28 adds parent isolation. This later containment does not retroactively resolve the earlier incident. No credentials or ambient socket were read or contacted for T28 investigation.

## Frozen provenance

T28 changed none of the frozen CU manifest, release producer, blueprint or review attestation bytes; `frozen-equality.json` records equality to the reviewed T27 values. Historical CU-0 hash: `b913830014ab778e9584352a2ef0a039e01935f25e703976d180ff4fae5cc64c`. Historical CU-1 hash: `94efe9c4883a8094ba3fcbe140859d247517689c3d69b405eaae3f04e86e50c6`.

`scripts/verify-proxy-cu --cli-v2` is explicitly compatibility-only, roster SHA-256 `b8670225a8b3f2c1d283f53ab00964478e07e4e86efb0f4b686a8648062a235b`. Its source tests ran in the full suite. Historical signed validator/producer formats and exact CU-ID argv remain frozen. No replacement release-proof backend, fake signed acceptance, status write or release script execution was introduced.

## Prepared native rollout and restoration procedure — not executed

Before running any command below, obtain explicit target authority and record: host and OS/architecture, disposable user/SID/UID, user-service domain, all five resolved roots, package owner/channel, current executable/version/digest, saved owner metadata and native definitions, enabled/running state, refresh completion baseline, approved traffic credentials without logging their contents, and the exact prior package/binary plus restoration path. The fields are unknown now; a user's ordinary installation is not a substitute. Transfer the matching archive above unchanged and verify both its SHA-256 and extracted executable SHA-256 from the table. Keep the old executable/definitions on the target until restoration is proved.

Use `CQ` only for the approved target's extracted executable, `SOURCE` for checkout of exact source `43638f3a2486815416e4cbd4cbab63efad71d285`, and `NATIVE_EVIDENCE` for its approved retained evidence directory. These target paths cannot be filled before a target is authorised. The known local source of each archive is the evidence directory plus the exact table filename under `archives/`.

For an approved disposable Unix target, the canonical lifecycle sequence is:

```sh
"$CQ" version --json
"$CQ" service install --component all --json
"$CQ" service status --component all --strict --json
"$CQ" service stop --component proxy --json
"$CQ" service status --component proxy --json
"$CQ" service start --component proxy --json
"$CQ" service stop --component token-refresh --json
"$CQ" service restart --component token-refresh --json
"$CQ" service status --component token-refresh --json
"$CQ" service start --component token-refresh --json
"$CQ" service uninstall --component all --json
```

PowerShell uses the same argv with `& $CQ` in place of `"$CQ"`. After **every** selected operation, capture both components and compare the unselected component's definitions, policy, PID/executable and completion evidence with its prior snapshot. Repeat install/uninstall for `--component proxy` and `--component token-refresh`. Require disabled refresh restart to produce a new successful completion while remaining disabled/unhealthy; require disabled proxy restart to remain disabled but run only in the current session. Repeat with a controlled second-component failure and prove rollback. Those native fixtures are pending; the sequence alone does not constitute the assertions.

Required native race commands, from the exact source checkout (capture JSON/v events and require actual named PASS events, never an empty regex or skip):

```sh
go test -race -count=1 -v ./internal/userdirs ./internal/fsutil
go test -race -count=1 -v ./internal/installer ./cmd/cq-install
go test -race -count=1 -v ./cmd/cq -run '^(TestLinuxOwnedRuntimeCancelsOnTermination|TestLinuxAdoptedRuntimeCancelsOnTermination)$'
go test -race -count=1 -v ./internal/proxy -run '^(TestLinuxAcceptanceConfinementUsesNamespacesRelaysAndLandlock|TestLinuxAcceptanceCancellationReapsDescendants|TestRuntimeSupervisorDegradedRescueRelaysHTTPAndWebSocketOverTransport|TestNormalProxyTransportWebSocketHardLimitMigratesBeforeLeak)$'
```

The last two commands apply to approved native Linux targets. Keep stock AppArmor/user-namespace policy; do not disable confinement to pass. For native Windows, use the CI scheduler opt-in only in the approved user, plus:

```powershell
$env:CQ_NATIVE_WINDOWS_SCHEDULER_TEST = '1'
go test -race -count=1 -v ./cmd/cq -run '^(TestWindows|TestRunService|TestServiceSnapshot|TestCLIV2ResetProduction|TestCLIV2AccountMutationRealCodexRemovalFence|TestCLIV2CompleteRegistry|TestCLIV2EveryGroupIsPure)'
go test -race -count=1 -v ./internal/provider/codex -run '^TestWindowsCredential'
```

Retain the `-race` requirement. Missing architecture-specific native toolchain support is a blocker, not permission to drop race or substitute cross-compilation. Before a Windows snapshot-based installer transaction, active auxiliary or disabled-running primary components require canonical `cq service stop --component proxy` / `--component token-refresh`. Direct WiX uninstall has a different owned-cleanup path; do not confuse it with snapshot restoration.

Native package commands remain gated on real preceding artefacts and saved baselines:

```sh
# Approved macOS user; CURRENT_CASK must be generated/reviewed for this exact binary.
"$SOURCE/.github/scripts/validate-homebrew-install.sh" "$PREVIOUS_CASK" "$PREVIOUS_ARCHIVE" "$PREVIOUS_VERSION" "$CURRENT_CASK" "$CURRENT_ARCHIVE" 1.0.0
```

```powershell
# Approved Windows user; CQ is the matching extracted 1.0.0 binary above.
& "$SOURCE/.github/scripts/build-windows-msi.ps1" -Executable $CQ -Version 1.0.0 -Architecture $ARCH -Output $CURRENT_MSI -Wix $WIX
& "$SOURCE/.github/scripts/validate-windows-msi.ps1" -PreviousMSI $PREVIOUS_MSI -CurrentMSI $CURRENT_MSI -PreviousVersion $PREVIOUS_VERSION -CurrentVersion 1.0.0
```

`ARCH` must match `amd64` or `arm64` from the artefact table. MSI/Cask generation and package scripts are **not executed** in this checkpoint; generated artefacts require new digests before any approval to install. The existing Linux package script takes tagged versions and the Go installer downloads published assets; neither can install these unpublished local archives unchanged. Do not point them at v1.0.0 before a separately authorised published release exists. The direct extracted-binary lifecycle above does not prove installer upgrade rollback.

For restoration, package transactions must retain and use their original snapshot/rollback path. Hidden `service snapshot`/`restore` require a real inherited installer lock descriptor; **do not** paste those markers into a shell as a lock bypass. If the approved disposable baseline was verified empty, `"$CQ" service uninstall --component all --json` (or `& $CQ ...`) restores the service absence, followed by native checks for no definitions, enabled policy, owned descendants, listeners or stale owner/lock state. If a prior installation existed, restore the saved previous package/binary atomically with its legitimate package owner and transaction, then verify exact previous digest, native definitions/policy/runtime identity and a new applicable completion. Never overwrite a mapped macOS binary in place. Exact package rollback commands require the still-unknown baseline and channel; therefore that portion of rollout remains unexecutable until authorised target inspection provides them.

Only after installation approval and source/artefact matching, run the approved real HTTP and WebSocket flow against the installed executable and retain status counts, continuity evidence and unchanged credential/state baselines. The existing exact-executable live tests require explicit credential roots, client/CQ paths and live opt-in; their current skips are not acceptance. Do not run `scripts/validate-codex-release` as a local check: it writes GitHub statuses. If restoration or validation fails, stop, retain evidence, and do not consume a reset as a workaround.
