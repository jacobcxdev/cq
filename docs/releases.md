# Release and deployment guide

CQ and its macOS native companion share one stable release version. Every
release includes a freshly signed and notarised companion, including Go-only
fixes. Package generation, publication and installed-runtime activation are
separate steps. The executable's version, a green workflow or a healthy listener
alone does not prove delivery.

Use [CONTRIBUTING](../CONTRIBUTING.md) for development checks and
[the native release guide](native-network-releases.md) for signed/notarised
companion assets. The executable configuration is
[GoReleaser](../.goreleaser.yml); the publication sequence is the
[Release workflow](../.github/workflows/release.yml).

## Release inputs and provenance

1. Inspect published stable releases, tags, drafts and other active release work
   before choosing a version. Advance exactly one semver component by one,
   resetting lower components: next patch, next minor or next major. Stop on
   conflicts or unexplained gaps; never silently skip or backfill a stable version.
2. Review the complete diff from the latest published stable release to current
   `origin/main`. Account for every merged component and retain previously shipped
   fixes. Resolve stale drafts explicitly; keep one active attempt per stream.
3. Merge the reviewed change through the normal PR process. Fetch immediately
   before tagging and prove the final release commit is the current `origin/main`
   tip. Do not release a feature branch, pre-merge candidate, stale checkout or a
   different commit with equivalent changes.
4. Complete the relevant build/vet/race, platform and installed-client gates.
   Retain the exact source, client version, package inputs and outcomes.
5. Create signed staging and parent tags at that current main commit, then
   dispatch the Release workflow with an increasing `native_build_number`, using
   the [native release guide](native-network-releases.md). CI builds, signs and
   notarises the companion at the same version, uploads its immutable ZIP to one
   parent draft and derives the temporary asset manifest. A previous version's
   companion cannot qualify a new release.

Read-only provenance checks include:

```sh
git fetch origin
git rev-parse HEAD origin/main
git diff --stat PREVIOUS_STABLE_TAG..origin/main
gh release list --repo jacobcxdev/cq --limit 100
git tag --sort=-v:refname
```

`PREVIOUS_STABLE_TAG` is a placeholder to replace after inspecting the inventory.
Fetching is required again if `main` advances before the tag is created.

## Live Codex qualification

The required **Harness compatibility** workflow first runs every
`internal/dots` `TestHarnessInstalled*` conformance family against the current
official Codex client and the pinned desktop-version baseline. It starts isolated native
app-server processes with temporary homes, synthetic capabilities and a loopback
provider; it does not read account credentials, call a real model or alter the
running desktop app. Both clients must pass without skipped cases before a PR's
existing required check succeeds or the release can build its draft packages.
The same matrix runs weekly and on manual dispatch to detect upstream drift
without waiting for a CQ release.

Before deploying to a desktop version, qualify its actual executable too:

```sh
bash .github/scripts/validate-harness-installed.sh /absolute/path/to/native/codex
```

The validator records the CQ source commit, native version and executable
SHA-256, discovers the installed harness tests, requires every case to run, and
rejects a native executable replaced during qualification. The pinned archive
version and per-architecture digests live in `.github/workflows/harness.yml`;
update them only after the replacement passes the same conformance suite.
Do not change test expectations or skip a client to work around a newly broken
native contract. Diagnose the failing fixture before publication or deployment.
The [harness qualification contract](harness-qualification.md) maps the happy
and unhappy paths to their deciding fixtures and recovery outcomes.

From the final source commit, run the existing release validator:

```sh
scripts/validate-codex-release
```

This performs real upstream calls through isolated proxy listeners using the
installed Codex client. It covers normal WebSocket, repeated HTTP, compaction
and rescue traffic, leaves the production listener unchanged and publishes the
commit-bound `cq/live-normal-routing` status consumed by release preflight.
Treat that status publication and upstream traffic as part of an authorised
release qualification run.

The validator requires the installed runtime's private egress broker. It reads
its port from the installed configuration; `CQ_CODEX_LIVE_EGRESS_PORT` selects
an explicit runtime when qualifying another instance. Direct Go live-test
invocations must set `CQ_CODEX_LIVE_EGRESS_PORT`; an unset port fails closed.
The script sets it automatically after resolving the installed configuration.
HTTP, WebSocket, rescue, exact-executable and latency-baseline traffic borrow
that broker's configured route. Broker failure stops validation; it does not
select direct egress or start another WireGuard tunnel. The latency baseline
bypasses CQ account routing while retaining this egress policy. Its isolated
client uses environment-proxy handling so macOS system-direct routing cannot
override the relay. Model discovery remains network-denied. The candidate
executable uses a temporary, loopback-only
CONNECT adapter for allowlisted Codex hosts; its private broker hop remains
authenticated. Fixture cleanup closes the adapter and its tunnels.

The validator discovers the installed client's recommended model through
`model/list`, records its concrete ID and fixes that selection across direct and
proxied traffic. Discovery uses an isolated profile and fails if it cannot find
an unambiguous visible default. Synthetic checks use their reviewed fixtures;
they do not establish real model availability. Changing the source, client or
other material validation inputs invalidates evidence that depends on them.

## Provider-specific qualification

The live Codex validator establishes Codex routing behaviour, not Grok subscription
compatibility. For Grok-affecting changes, use the separate authorised
[Grok checks and dated qualification matrix](usage.md#grok-subscriptions).
Record the tested account/plan, client version, exact model IDs, transports and
remaining gaps. Login and read-only quota/catalogue checks do not prove inference,
tools or compaction; the inference checks spend subscription quota. Reuse dated
evidence only while its relevant source/client/account inputs remain applicable.
Do not extend those results to other models, plans, billing modes or clients.

## Tag and dispatch

After the current-main and qualification gates pass, create a signed annotated
tag for the selected stable version and push that tag. For example, substitute
the selected version in `git tag -s vX.Y.Z` and `git push origin vX.Y.Z`.
Never move a published tag or force-push shared release history.

Create the signed `cq-native-stage-vVERSION` tag at the same current-main commit
as the parent tag. Dispatch with the next increasing native build number:

```sh
gh workflow run release.yml --repo jacobcxdev/cq --ref vX.Y.Z -f native_build_number=BUILD_NUMBER
```

Replace both placeholders after inspecting the published release and native
build. Pushing a tag alone does not publish CQ. CI validates both annotated tag
signatures, current reviewed `origin/main`, stable version ordering and native
build progression before using Apple credentials. It creates or reconciles one
unpublished parent draft, builds/signs/notarises the companion, verifies its
immutable ZIP and adds the CLI packages and combined macOS bundles.
See the [native guide](native-network-releases.md) for secret setup and recovery
of the exact existing notarisation submission.

The workflow stages are:

| Stage | Gate/output |
| --- | --- |
| `release-source` | Current reviewed main provenance and sequential stable version |
| `native-network`, `validate-codex` | Build, sign, notarise and verify the tagged native asset in CI; installed Codex normal/compaction/hard-limit/rescue and reviewed corpus gates |
| `draft-build` | Require the commit-bound live routing status, bind corpus provenance/native manifest/Gemini secret, add CLI packages and verified combined macOS bundles to the existing draft |
| `release` | macOS Cask formatting, quarantine and actual predecessor-to-candidate lifecycle checks |
| `windows-packages`, `windows-acceptance` | Build AMD64/ARM64 per-user MSI and manifests, validate package/installer lifecycle |
| `publish` | Verify the two Scoop ZIP digests, publish the qualified parent release and sign the tap commit with both Homebrew Casks and the Scoop manifest |
| `linux-install`, `windows-deployed` | Download public packages and verify deployed installer/lifecycle behaviour |
| `scoop-deployed` | Verify public Scoop installation, upgrade and removal on native Windows AMD64 and ARM64 |
| `winget-publish` | Submit the community manifest after deployed checks, then account for superseded candidates |

Before first Scoop publication, dispatch **Published Windows validation** with
`scoop_preflight=true` and an existing stable `release_tag`. Native Windows
runners verify the Scoop lifecycle with that release and its predecessor. The
workflow seeds the candidate in a temporary checkout of the public tap.

Some jobs run in parallel; dependency edges in the workflow remain authoritative.
The post-publication checks can fail after GitHub or tap publication. Report
which channels are already public and the exact failed stage. Do not describe a
partially published attempt as either an unpublished draft or completed delivery.

WinGet submission waits for the initial contributor-label policy, then posts the
authorised agreement once after verifying the current owned PR, its version files
and head. If a label appeared after an existing agreement, CQ replays that exact
agreement by editing its trailing whitespace; it preserves one comment and avoids
repeating an edit while the bot is processing it. The existing Windows lifecycle
gates do not establish equivalence with WinGet's Sandbox, ShellExecute or
downloaded file handling. Inspect the community PR's actual failed check before
attributing an installation failure to its URL or package.

Publication writes use GitHub's `createCommitOnBranch` API with an exact expected
branch head, then require the resulting commit's verified signature, parent and
changed files. One commit updates both Homebrew Casks and `bucket/cq.json` in
`jacobcxdev/homebrew-tap`. The Scoop manifest points to the versioned portable
Windows ZIPs. Its SHA-256 values must match `checksums.txt` and the GitHub
release asset digests. Unchanged inputs create no commit.
WinGet submission uses the same signed path and checks the
actual publication token's actor and owned fork's identity and write permission
before creating a branch or PR. These automated community submissions use a
public fork; they do not require upstream push access or a PR assignee. This is
separate from assigning CQ's own development PRs to `jacobcxdev`. Existing
unsigned commits are historical records; do not
rewrite them or append an empty commit to imply they became signed.

If only the WinGet publication helper fails after packages are public, recover
that stage from reviewed `main` with `gh workflow run release.yml --ref main -f
winget_release_tag=vX.Y.Z`, using the existing published version. Recovery runs
the guarded agreement and superseded-candidate cleanup steps without building,
uploading, publishing or replacing package assets. The helper verifies a stable
published release and accepts the Microsoft policy app's latest successful
`license/cla` check on the exact PR head as completed initial CLA processing.

If publication stopped before creating a community PR, recover the original
`winget-manifests` artifact from the qualified release run and verify its version,
installer URLs and hashes against the immutable published packages. Use the same
automated publisher from reviewed `main`, with the normal publication credential:

```sh
RELEASE_TAG=vX.Y.Z bash .github/scripts/submit-winget-manifest.sh /path/to/original/manifests
```

This creates or verifies the signed fork submission without rebuilding packages.
Then run the existing `winget_release_tag` recovery for the agreement and
superseded-candidate cleanup. Do not regenerate manifests or change package
checksums merely to retry publication.

If a published Windows ARM64 installation check was interrupted, use the
read-only **Published Windows validation** workflow from reviewed `main`:

```sh
gh workflow run published-windows.yml --repo jacobcxdev/cq --ref main -f release_tag=v0.48.0
```

Substitute the already published stable version. This downloads its immutable
ARM64 MSI and the published predecessor, checks both GitHub asset digests and
checksums against their exact tag sources, and runs the existing Go-runner and
MSI lifecycle validators on Windows ARM64. It does not build, upload or replace
release packages. A separate read-only job checks the actual WinGet publication
token; a fork-permission failure does not invalidate a successful named
Windows installation job. Neither job submits a community PR. The existing
`winget_release_tag` recovery still handles an already submitted PR's agreement
and cleanup; it does not create a missing submission or establish installation
qualification.

## Toolchain and package configuration

GoReleaser v2 builds `cmd/cq` for Darwin, Linux and Windows, each in AMD64 and
ARM64, with `CGO_ENABLED=0` for distributed executables. Archives are tarballs
except Windows ZIPs. The version, Gemini OAuth secret and reviewed Codex corpus
provenance are linked at build time. Race validation runs separately on supported
native hosts; a cross-build does not replace it.

Windows packaging uses .NET 8 and WiX 5.0.2. MSI lifecycle and WinGet manifests
are separate from ZIP archives and the Go installer runner. Each macOS
`cq_VERSION_darwin_ARCH_bundle.tar.gz` contains `cq` and the signed universal
`CQNetwork.app`, both at the parent version. The existing CLI-only archive names
and contents remain available for installer compatibility. The `cq` and
`cq-network` Homebrew receipts remain separate internally; both Casks select the
same bundle URL and SHA-256 for the host architecture. The companion dependency
applies on macOS 13 and later, preserving core CQ's macOS 12 support.

CI tests the current native source and downloads the latest published companion
as a packaging fixture. The release workflow verifies the new same-version
companion before assembly, then verifies
its signatures and notarisation again after extracting the final bundle. It has
no Apple signing or notarisation credentials. Preserve the third-party ZCode licence, published
as the checksummed `ZCode-LICENSE.txt` asset, alongside the project's [MIT
licence](../LICENCE).

The shared setup action uses Go from `go.mod` and separate module/compiler
caches, scoped by runner image, architecture and toolchain. Current workflows
use upgraded Node24-or-later action generations. Cache hits are a performance
optimisation; they do not replace source provenance or test gates.

## Credentials and publication permissions

Only names and purposes belong in repository documentation:

| Input | Purpose |
| --- | --- |
| `GITHUB_TOKEN` | Workflow-scoped repository read/write and commit status inspection, according to each job's permissions |
| `HOMEBREW_TAP_TOKEN` | Write generated Casks and the Scoop manifest to the authorised `jacobcxdev/homebrew-tap` repository |
| `GEMINI_ANTIGRAVITY_CLIENT_SECRET` | Required release-link input for the direct Antigravity OAuth client |
| `WINGET_PKGS_TOKEN` | Submit CQ's manifest PR through the authorised WinGet fork and manage superseded CQ candidates |
| `APPLE_DEVELOPER_ID_P12_BASE64`, `APPLE_DEVELOPER_ID_P12_PASSWORD` | Password-protected Developer ID Application signing identity for the company team |
| `APPLE_HOST_PROFILE_BASE64`, `APPLE_PROVIDER_PROFILE_BASE64` | Matching Developer ID distribution profiles for the host and provider |
| `APPLE_NOTARY_KEY_P8`, `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID` | Company App Store Connect API key for Apple notarisation |

Apple secrets belong to the `native-release` environment, restricted to release
tags. Only the native job's signing or finishing step receives them,
never to PR checks. CI imports the identity into a temporary keychain, removes
private material and restores the keychain search list on exit. Fastlane and an
interactive Apple account session are not required.

Keep values out of logs, source, evidence and command examples. Release-time
linking does not permit a runtime environment fallback for Gemini.

## Delivery verification and repair

Verify these outcomes separately:

- GitHub release/tag, expected architecture assets and checksums identify the
  intended reviewed commit. Preserve published assets; do not replace them.
- Both Homebrew Casks select the same reviewed macOS bundle URL and digest.
  Check generated Cask bytes and matching CQ, host and provider versions.
- Installed `cq --version` identifies the package. `cq service status --json`
  separately identifies active/pending runtime versions and upgrade phase.
  Deferred activation remains pending until the service actually adopts it.
- Installed Codex HTTP/WebSocket/compaction/rescue checks pass against the intended
  runtime. Native changes also need real affected TCP/UDP admission/routing proof
  after explicit system-extension activation and any required macOS approval.
- Linux and Windows public-package lifecycle checks pass on their native hosts.
  Inspect the actual runner/MSI versions and user service/task ownership.
- Scoop selects the native ZIP on both Windows architectures. The shim runs the
  installed CQ, follows the upgrade and is removed on uninstall.
- WinGet's community PR is accepted and the manifest is available through the
  intended channel. Submission or successful validation of a local manifest does
  not prove community publication.

Use the [Homebrew upgrade guide](homebrew-upgrades.md) to interpret active versus
pending runtime state. A genuine rollback uses retained verified runtime/package
state; a package rollback restores its matching reviewed native payload and
previous routing intent. Never overwrite published tags/assets. Repair release omissions
with the next complete sequential version, and annotate affected release notes
so previous publication state remains clear.
