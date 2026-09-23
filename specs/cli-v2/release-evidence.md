# CLI v2 local release evidence — 1.0.0

## Current qualified candidate — 23 September 2026, 14:27 UTC

**Six provider-complete candidate binaries are built and checked from signed source `244678521f4fab55ce423427d32600034a93d860`. The qualified Darwin/arm64 binary is now installed and verified on the existing Mac; the earlier incomplete build and rollback remain historical below.**

The user authorised feature-branch push and candidate-only CI dispatch. [CI run 35873208509](https://github.com/jacobcxdev/cq/actions/runs/35873208509), attempt 1, succeeded using the legitimate release-time provider configuration. It built all six existing release targets; ordinary installation/native qualification jobs were skipped in this candidate-only run. No PR, main-branch merge, tag or published release was performed. Local archive assembly used the current release file allowlist and these exact downloaded binaries; it did not rebuild them or expose linker secret values.

Source includes all 14 default-branch commits through `e019d2bff938317fa794a6c6605fa22aba5436bc`, plus reviewed history cancellation, launchd removal/policy parsing and dashboard corrections. `a00b4dc` also corrects absent integrations incorrectly causing partial failure: genuine authentication and network failures still fail visibly. Subsequent documentation-only commits are not the executable's source revision.

### Current artefacts

Directory: `.superpowers/sdd/plan/qualified-candidate-artifact-evidence/`. `artifact-manifest.json` binds source, run/attempt, downloaded metadata digest, clean build provenance, binary and archive hashes, archive members and all 127 embedded help pages per target. Each archive contains the executable, README.md and specs/cli-v2/COMMANDS.md. Darwin/Linux names are `archives/cq_1.0.0_{os}_{arch}.tar.gz`; Windows names end `.zip`.

| Target | Archive SHA-256 | Executable SHA-256 |
|---|---|---|
| darwin/amd64 | `c02ba4f3210847547a2b5c3f9308e109eb161a7467f846b40b2b6212ca1f9378` | `4321c8f433921b00c14e0074c44c74198d81d06d21da9685730ea4eab8bed73b` |
| darwin/arm64 | `35a9dc8705659277c10db213d2ec32430079dc4b13a89e5a1c73f02937ddba77` | `6679806a694c225a76e2386d18227ce797dcb91159e89db6f04afd1c47d26cc0` |
| linux/amd64 | `0ee8d49cc060cf47983d15f9e50ca0fb2e3bf909e35e348a01745b2f51a15401` | `6db38c8b0368e66e50bd9c3c9f4068e51b63fec008ad95dc783928cde3e41896` |
| linux/arm64 | `fed59979f523f60ba5f9953dec0311c569f8103db442e940959ba313ed263478` | `40ebb4ba482fde4725d763877bdff1e1bb0822c93387c097bb1762e5866981cc` |
| windows/amd64 | `857fdb906bf0c9c513825a91b22eae59a3bae8bdb1bf73280d56d51704933dc0` | `ee1674ff3acbd36395a66865716476097991d3dddf888b0a0bdabe8839f45900` |
| windows/arm64 | `9a0a1169441e533028e3ec24f503e13ee4b9218fcb251dd5a181a4e0f7b4a465` | `5240afcd2212ec2a092cbceed3aa4ef059eb36c4e3e1db1a4368f62fd3a6d4fa` |

These are local archives of the qualified CI binaries, not MSI/Cask/WinGet packages or a published release. Cross-target metadata checks establish build provenance, not foreign runtime acceptance.

### Current verification

- All 25 indexed artefact-evidence hashes were verified by the controller. Packaged Darwin/arm64 checks passed 271 cases: 127 exact help pages, 127 schema-2 syntax errors, one version result, 13 fail-closed machine ABI forms and three completion outputs. Earlier 12 real-shell PTY cases remain applicable: the controller verified no completion/help implementation delta from that tested source, and current packaged completion outputs match exactly. No new foreign-shell execution is claimed.
- Fresh Gemini check at 14:21:47 UTC passed with both 5h and 7d windows, cache age zero, no errors/warnings and empty stderr. Fresh all-provider check at 14:22:06 UTC exited 0, no warnings/stderr and no stale results: configured Codex and Gemini succeeded; absent Claude was represented as not configured without falsely failing the command. Receipts are under `.superpowers/sdd/plan/qualified-candidate-evidence/`; candidate identity records exact binary SHA-256 `6679806a694c225a76e2386d18227ce797dcb91159e89db6f04afd1c47d26cc0`.
- Full integrated race history remains **4179 PASS, one stale timestamp assertion FAIL, 29 SKIP**, with the complete corrected timestamp test passing separately. Subsequent corrections have reviewed, source-bound focused race/vet proof. CI candidate success is not a new all-green full-suite claim.
- Native policy correction `efa05a8a263bd9ec45674ddd44a940524d23893f` passed 40 top-level/144 total targeted race tests and focused vet; scoped review approved. The final installed stop/start cycle also passed, as recorded below.

The combined `.superpowers/sdd/plan/qualified-release-evidence-index.json` binds 28 current candidate/deployment files and the separately verified 25-file artefact index; SHA-256 `57bb969884b8c78b0193a436849f9e9dc3f67b75545103d8d9989fe0d17f9230`. A fresh fetch at 14:27:46 UTC confirmed default `main` remained `e019d2bff938317fa794a6c6605fa22aba5436bc`, an ancestor of the candidate.

### Deployment and completion boundary

The legitimate Homebrew hook completed successfully at 14:24:26 UTC after atomic replacement. Installed `/opt/homebrew/bin/cq` resolves to the existing Caskroom path `/opt/homebrew/Caskroom/cq/0.32.12/cq`; this is a local deployment, not a newly published Homebrew package. The installed binary digest matches the qualified Darwin/arm64 artefact above. Version output is 1.0.0/schema 2; its nullable revision/dirty fields remain null, while source identity is independently bound by verified binary/build metadata and CI receipts.

Actual `service stop --component all` and `service start --component all` both returned 0 with empty stderr. Strict status subsequently returned 0: proxy PID 5344 running and healthy; token refresh enabled, idle and healthy, with successful completion at 14:25:16.993461 UTC. This closes the observed native policy-parser regression for the executed all-component cycle; it does not prove every disabled/selective/failure-injection matrix.

Installed fresh all-provider check at 14:25:51 UTC returned 0, no warnings, no stderr and zero cache ages. Gemini was OK; Codex had three usable accounts and one genuinely exhausted account; Claude remained expected not-configured. An actual terminal invocation also exited 0 with restored bars, active account first, weighted gauge, both Gemini windows and no partial-refresh warning.

Both installed-client transport flows returned exact replies. HTTP observed upstream 429 then successful failover/200. WebSocket observed 101 and terminal success, followed by `cyber_disabled` resynchronisation through HTTP 200. Both recorded intervals contain zero 503 and zero continuity mismatch. This does not claim an exclusively WebSocket conversation.

Evidence directory: `.superpowers/sdd/plan/qualified-deploy-evidence/`, including `attempt-jzit52aa/deployment.json`, `installed-identity.json`, `service-cycle.json`, `installed-service-status.json`, `installed-fresh-check.json` and `transport-acceptance.json`. Original executable, definitions and ownership record remain retained under the earlier local-deploy evidence rollback directory. User Codex connection settings remain unchanged.

The user waived disposable environments and requested ordinary deployment to the existing Mac. Do not restore that prerequisite or describe it as passed. Preserve unavailable native macOS package/disabled-component, Linux confinement/systemd and Windows scheduler/WiX/credential qualification as unverified. Historical CU0/CU1 release provenance and installed vendor model-cache tolerance remain unverified; compatibility-only proof is not signed release acceptance. These limitations forbid broader release/platform claims and do not authorise a new proof backend.

T00–T27 implementation and task reviews remain recorded in the execution ledger. Current T28 candidate preparation and the authorised existing-Mac installed acceptance checkpoint are evidenced above. This closes that local delivery checkpoint; it does not claim published-release or unavailable foreign-platform acceptance. No reset consumption or unrelated native/credential mutation is authorised by this evidence.

---


## Historical rollback checkpoint — 23 September 2026, 14:09 UTC

**CLI v2 deployment was rolled back. The existing Mac is running the restored v0.32.12 installation; no current six-target CLI v2 candidate is qualified for another deployment.** Local source work remains intact. Publication, tagging, pushing and remote workflow execution have not been authorised.

The user explicitly rejected disposable environments and authorised normal deployment to the existing Mac after including default-branch fixes. This supersedes the former disposable-target prerequisite; it does not turn unexecuted platform or package gates into passes. All archive tables and the original gate table below are historical records, not current installation instructions.

### Source and deployment chronology

- Signed merge `5345884c9057d49196db789938b1d80a22c5fee3` included default `main` at `e019d2bff938317fa794a6c6605fa22aba5436bc`, including all 14 commits since the initial CLI v2 baseline. That main revision remains an ancestor of current source.
- Follow-up `2678d9b9a78e304628102bfbbd7c40d33e3f63d7` fixed detached history-input ownership. `17a03667bddede51bcabd51f381660ee76da3442` added bounded launchd registration-removal waiting. `8a4e63b0462954d771cf32cd4cf16ab73dfd7991` restored the established human quota dashboard without changing canonical JSON.
- Source `8a4e63b0462954d771cf32cd4cf16ab73dfd7991` was installed at 14:00 UTC through atomic replacement and the legitimate Homebrew hook. Recorded binary SHA-256 was `cb85338bcf506fdaa833672dbde1c8e93d2ed5ca85e5fb56934e8b5246c71510`. HTTP 200 and WebSocket 101/terminal success were observed, with zero 503 or continuity mismatch in the recorded intervals. The WebSocket flow later resynchronised through HTTP after `cyber_disabled`; this was not an exclusively WebSocket conversation.
- That local build omitted the release-time Gemini OAuth client secret. The user reported previously working Gemini quota refresh had regressed. The empty build input was a known capability loss, not acceptable completion of their normal installation upgrade.
- At 14:05 UTC, the original v0.32.12 binary and exact saved service definitions were restored. The legitimate old Homebrew hook initially rejected the newer plist environment dictionary; restoring the saved definitions allowed rollback. Controller verification recorded proxy PID 81033 healthy on port 19280, default `cq --json` exit 0 and Gemini OK. These are observations at rollback time, not perpetual health claims.
- During rollback, canonical v2 `service stop --component all` returned exit 8, `service_partial` and `rollback=failed`, with both component states indeterminate. Signed source `efa05a8a263bd9ec45674ddd44a940524d23893f` (`fix: parsed native launchd policy values`) now addresses native launchd boolean-policy parsing. Scoped review approved the correction with no findings; 40 top-level/144 total targeted race tests and focused vet passed. It has not been deployed by this checkpoint.

Current incident and deployment evidence is retained under `.superpowers/sdd/plan/local-deploy-evidence/`; default-branch evidence and mapping are under `.superpowers/sdd/plan/default-branch-integration-evidence/` and the corresponding report. Original executable, definitions and ownership record remain retained for rollback. User Codex connection settings were not changed. No reset credit was consumed.

### Gate reconciliation at the rollback checkpoint

| Requirement | Current evidence and status |
|---|---|
| Latest default-branch fixes | Included through `e019d2bff938317fa794a6c6605fa22aba5436bc`; 14-commit behaviour/test mapping retained. |
| Integrated build, vet, catalogue and plan checks | Passed at the default-branch integration checkpoint; subsequent narrow fixes have their own source-bound covering evidence. |
| Integrated full race suite | **4179 PASS, one stale timestamp assertion FAIL, 29 SKIP.** Corrected complete timestamp test passed separately: one top-level and six child tests. No all-green full-suite claim. |
| History cancellation correction | Scoped review approved; 40 top-level/74 total race-test passes, zero failures/skips, focused vet passed. |
| Launchd registration-removal correction | Scoped review approved; 38 top-level/134 total race-test passes, zero failures/skips, focused vet passed. |
| Human quota dashboard restoration | Scoped review approved; covering renderer/check race tests and vet passed. Installed terminal appearance was verified before rollback. |
| Native policy parsing / selected stop | Source fix `efa05a8a263bd9ec45674ddd44a940524d23893f` committed and scoped review approved; 40 top-level/144 total targeted race-test passes and focused vet passed. Failed native stop/rollback result remains retained. No successful redeployment claimed. |
| Current six-target candidates and packaged checks | **Not yet prepared for current source.** All six-target archives below predate upstream integration and later fixes. A replacement candidate workflow is being prepared locally; no remote run or new artefact digest is claimed. |
| Gemini capability | **Unqualified in the rolled-back v2 build.** Another deployment requires the legitimate release-time build input and verification. No binary scraping, runtime fallback or warning suppression substitutes for it. |
| Existing Mac deployment | **Rolled back to v0.32.12.** Earlier v2 HTTP/WebSocket observations are historical, not acceptance of current source or current installation. |
| Other native/package qualifications | Remain explicitly unverified where no native run occurred. Cross-builds establish compilation only. Disposable lifecycle prerequisite was waived, not passed. |
| Historical CU0/CU1 provenance | Unresolved historical release qualification; compatibility-only roster is not signed release acceptance. No replacement proof backend was added. |
| Publication | Not authorised or performed. Local implementation/evidence preparation and a published release remain separate checkpoints. |

Remaining work: prepare a full-provider candidate through the legitimate release build mechanism, refresh current artefacts and packaged checks, then perform any authorised redeployment with rollback retained. Keep implementation goal open; this document does not claim completion. Retain all failed attempts and earlier qualification limits below without treating them as current authority.

---

## Historical candidate: final integration fixes (superseded)

This historical I1/I2/M1 integration wave superseded the original T28 candidates below. **These archives are now superseded too; do not install or treat them as the current CLI v2 candidate.** Source is signed commit `8b377be27b22f7164f8dac6d61d17afcf1fd1c5f` (`fix: preserved CLI projection contracts`), built from a clean worktree. All six binaries identify this exact revision and `vcs.modified=false`; the later evidence-only commit is not their source.

The fixes preserve typed environment diagnostics across canonical preparation boundaries, escape persisted pin/fallback labels in human output without changing JSON, and retain exact fractional seconds in canonical Timestamp projections. Frozen machine/vendor formats and platform-unavailable precedence remain unchanged.

Historical evidence directory:

```text
/Users/jacob/Developer/src/github/jacobcxdev/cq/.worktrees/cli-v2/.superpowers/sdd/plan/final-fix-evidence
```

Archives are `archives/cq_1.0.0_{os}_{arch}.tar.gz` for Darwin/Linux and `.zip` for Windows. `artifact-manifest.json` records each archive member, executable, build metadata and all 127 embedded help-page checks. All hashes below were verified against the archives and their extracted members.

| Target | Archive SHA-256 | Executable SHA-256 |
|---|---|---|
| darwin/amd64 | `766a4237fcfb7fe95aa31e825d5415f03a30691cfef881eb21a1948f77ed375f` | `dc61f43cf145345a635f866019e1d47f7d93e68b8ab8750eef65320cd89d5e30` |
| darwin/arm64 | `8356f64e5afcaad60e5b4def658ff7210721fd18bf664c5d97ff3a644ed7f026` | `4fd1e6ac32cc4da68fbd8664221c6af730922c8fb6eff45aa948b76b480c5ac4` |
| linux/amd64 | `6196dd57a68e323d960404b947ee9381bec227c2fd2bc0f7344f37bcdec179e1` | `77075adaa183ce8133ec214545260053cd6992d38bf5a7f2ba55ac3c7f0cb894` |
| linux/arm64 | `5b2177aa0dbba42201c0c0400d575eb284af6bc6b76ec0dad6e05afdca1c7a1a` | `00be1f9c041fb2489ca6428ffbe82907300aee9ebd48811bede7dfa5ffdad815` |
| windows/amd64 | `586651e0cd6123e752931bd5bf52faf6c8764b3403468489eedda2e5971e68c7` | `d4d006e6fc30f406d226cce98ff4bfe59b1fc5c4e87e9ef74b0ce2a17efccd22` |
| windows/arm64 | `49dd83a8eda51d6bf0073a017615446e884db5df74754bf40e3d35680ada9ac6` | `fad968001e4ab8ce480a2ad028f0994c0fdea95a6be2356ac06a8a0ad7768177` |

The final `red-final.log` reproduces all three findings against reviewed base `3a266ffe237390dbaf44d74dc1827fadfcd88128`, with the final durable regression file included in its source hashes. All exact-second timestamp baselines pass in RED. `covering-race.log` subsequently records **203 passed, zero failed/skipped** affected-command top-level tests; `common-contract-race.log` records **71 passed, zero failed/skipped** CLI/user-directory tests. Both used `go test -race -count=1 -json`; `matched-tests.json` retains exact selected names. `go vet ./...`, `go build ./...` and the generator check passed. No new full-suite run was needed or claimed.

The packaged Darwin/arm64 executable passed **271 cases**: 127 exact help pages, 127 schema-2 syntax failures, version, 13 fail-closed machine ABI forms (ten missing-descriptor and three malformed), and three completion outputs. The copied harness derives these counts from retained cases. Twelve real Tab/Enter PTY cases passed, as did twelve packaged regression probes: six invalid/unavailable-root diagnostics and three human/three JSON control-character label cases. Cross-target checks remain compilation/metadata only.

`source-delta.json` and `source-fix.diff` map the reviewed base to the exact candidate source; `build-inputs.json`, gate JSON/log pairs and `source-signature.txt` retain commands, source hashes, allowlisted environment and signed provenance. `evidence-index.json` is a **new** final-fix index; the old T28 97-file index, candidates and failures remain unchanged. Initial regression-fixture setup failures are retained separately and are not counted as product findings or successful RED evidence.

The parent environment used fresh task-owned HOME/XDG/CODEX/CLAUDE/TMP roots, including XDG_CONFIG_HOME, XDG_CACHE_HOME, XDG_STATE_HOME and XDG_DATA_HOME. The temporary root and TMPDIR use caller group 20; only existing tool/cache paths were shared, with GOPROXY/GOSUMDB disabled. `environment.json` and `isolation.json` retain exact values. No ambient credentials or live opt-in flags were inherited. The Gemini link secret was explicitly empty; no secret discovery occurred.

**The historical full suite remains exit 1: 4,134 PASS, four FAIL and 29 SKIP.** Its four setgid-fixture failures and source-identical targeted correction under caller-group TMPDIR remain as documented below. This final focused green evidence does not rewrite that result. All native, installed-traffic, historical CU0/CU1, Gemini full-release and earlier ambient-incident limitations below remain unresolved. No publication, installation, real reset or native service operation was performed.

## Historical T28 source and retained artefacts

- Reviewed T27 base: `659891406fa81acd7d04694ead78d0788415dac7`.
- Signed candidate source: `43638f3a2486815416e4cbd4cbab63efad71d285` (`ci: targeted the CLI v2 release line`). Signature verified. The build began with a clean tracked/nonignored worktree; all six Go build records report that revision and `vcs.modified=false`.
- Runtime/product bytes are unchanged from the T27 base. The source checkpoint changed only the CI candidate version selection, Windows user-directory gate and their existing configuration assertions.
- This document is committed later as evidence only. That later commit is **not** the executable's source revision. No self-referential future commit hash is claimed and no rebuild is implied by documentation changes.
- Exact version linked into every binary: **1.0.0**. Future CI package validation deliberately uses **1.0.0-ci**, retaining its existing CI-only local tag mechanism and previous published-release lookup. That workflow was not run; no temporary tag was created here.
- Build host: macOS arm64. Go `go version go1.27.1 darwin/arm64`. GoReleaser 2.18.2. `CGO_ENABLED=0`, amd64 v1 and arm64 v8.0. Cross-builds establish compilation and metadata only.

All historical T28 paths in this section are relative to this exact local evidence directory:

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

## Historical T28 gate results and boundaries — not current authority

| Gate | Result at this checkpoint | Evidence / remaining requirement |
|---|---|---|
| Plan and generated catalogue | Passed | Baseline plus final plan/generator checks, including `--go-output internal/cli/catalogue_gen.go`. 89 leaves, 37 groups, 127 help pages. |
| Build / vet | Passed | `go build ./...`; `go vet ./...` on source checkpoint. |
| Final integrated full race | **Failed** | `go test -race -count=1 -json ./...`: 4,134 top-level passes, four failures, 29 skips; no race report. Full log preserved. |
| Four affected fixture tests | Passed separately | Same source, `-race`, four passes/no skips after correcting only the synthetic TMPDIR group. This does not rewrite the full run as passed. |
| All six artefacts | Local build/archive checks passed | Exact version, source revision, clean provenance, build flags, archive bytes and all 127 full help byte strings checked per binary. No foreign executable was run. |
| Extracted darwin/arm64 executable | Bounded runtime checks passed | All 127 exact help outputs; 127 schema 2 syntax-error envelopes; version 1.0.0/schema 2; three malformed machine forms and ten valid child forms failing closed without inherited descriptors (13 ABI cases). Including the three completion outputs, the retained host inventory contains 271 cases. Counts are derived from `host-runtime.json`; the original harness/log undercount of six child forms remains preserved. No valid package/service authority exercised. |
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

## Historical proposed disposable rollout — superseded by user direction

The procedure below is retained as the original, unexecuted T28 proposal. The user subsequently rejected disposable environments and authorised normal installation on their existing Mac. Do not reinstate this procedure as a prerequisite or treat its former unknown-target statements as current facts.

At the time of this proposal: before running any command below, obtain explicit target authority and record: host and OS/architecture, disposable user/SID/UID, user-service domain, all five resolved roots, package owner/channel, current executable/version/digest, saved owner metadata and native definitions, enabled/running state, refresh completion baseline, approved traffic credentials without logging their contents, and the exact prior package/binary plus restoration path. The fields are unknown now; a user's ordinary installation is not a substitute. Transfer the matching current final-fix archive from the first table above unchanged and verify both its SHA-256 and extracted executable SHA-256 from the table. Keep the old executable/definitions on the target until restoration is proved.

Use `CQ` only for the approved target's extracted executable, `SOURCE` for checkout of exact source `8b377be27b22f7164f8dac6d61d17afcf1fd1c5f`, and `NATIVE_EVIDENCE` for its approved retained evidence directory. These target paths cannot be filled before a target is authorised. The known local source of each current archive is the final-fix evidence directory plus its exact filename under `archives/`; the historical T28 archives are superseded.

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
