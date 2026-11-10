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
4. Stage the signed/notarised companion for that same version using the
   [native release guide](native-network-releases.md), then merge its reviewed
   schema-2 pin in `.github/native-network-release.json`. The pin records the
   immutable native ZIP's source, asset ID and digest. Validation proves that
   source is an ancestor of the final release commit and both native Git trees
   match. A previous version's companion cannot qualify a new release.
5. Complete the relevant build/vet/race, platform and installed-client gates.
   Retain the exact source, client version, package inputs and outcomes.

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
bypasses CQ account routing while retaining this egress policy. Model discovery
remains network-denied. The candidate executable uses a temporary, loopback-only
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

Retarget the existing unpublished staging draft to this parent tag and the full
final commit SHA, keeping its title `vVERSION`, `draft=true` and
`prerelease=false`. Keep the staging tag and uploaded native ZIP unchanged. This
is the same draft, not a second release; the workflow adds the CLI packages and
combined macOS bundles before publishing it. See the native guide for the exact
staging and pin-review order.

The **Release** workflow is manually dispatched against the tag. Pushing the
tag alone does not publish CQ. Its source preflight rejects a tag outside the
current reviewed `origin/main` tip and rejects invalid stable version ordering.

The workflow stages are:

| Stage | Gate/output |
| --- | --- |
| `release-source` | Current reviewed main provenance, sequential stable version and matching schema-2 native pin |
| `native-network`, `validate-codex` | Verify pinned signed/notarised native asset; installed Codex normal/compaction/hard-limit/rescue and reviewed corpus gates |
| `draft-build` | Require the commit-bound live routing status, bind corpus provenance/native pin/Gemini secret, add CLI packages and verified combined macOS bundles to the existing draft |
| `release` | macOS Cask formatting, quarantine and actual predecessor-to-candidate lifecycle checks |
| `windows-packages`, `windows-acceptance` | Build AMD64/ARM64 per-user MSI and manifests, validate package/installer lifecycle |
| `publish` | Publish the qualified parent release and paired companion/main Casks together |
| `linux-install`, `windows-deployed` | Download public packages and verify deployed installer/lifecycle behaviour |
| `winget-publish` | Submit the community manifest after deployed checks, then account for superseded candidates |

Some jobs run in parallel; dependency edges in the workflow remain authoritative.
The post-publication checks can fail after GitHub/Homebrew publication. Report
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
changed files. The paired Homebrew Casks are committed together; unchanged bytes
create no commit. WinGet submission uses the same signed path and checks the
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

CI downloads and verifies the reviewed companion before assembly, then verifies
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
| `HOMEBREW_TAP_TOKEN` | Write generated Casks to `jacobcxdev/homebrew-tap`; use a token authorised for that target repository |
| `GEMINI_ANTIGRAVITY_CLIENT_SECRET` | Required release-link input for the direct Antigravity OAuth client |
| `WINGET_PKGS_TOKEN` | Submit CQ's manifest PR through the authorised WinGet fork and manage superseded CQ candidates |
| Authorised release Mac's Xcode account/signing environment | Native Developer ID signing, provisioning and notarisation, outside GitHub Actions |

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
- WinGet's community PR is accepted and the manifest is available through the
  intended channel. Submission or successful validation of a local manifest does
  not prove community publication.

Use the [Homebrew upgrade guide](homebrew-upgrades.md) to interpret active versus
pending runtime state. A genuine rollback uses retained verified runtime/package
state; a package rollback restores its matching reviewed native payload and
previous routing intent. Never overwrite published tags/assets. Repair release omissions
with the next complete sequential version, and annotate affected release notes
so previous publication state remains clear.
