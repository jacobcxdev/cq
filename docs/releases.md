# Release and deployment guide

CQ has a stable Go release stream and an independently versioned macOS native
companion. Package generation, publication and installed-runtime activation are
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
4. Check `.github/native-network-release.json`. Validation compares the pinned
   source commit's native Git tree with the release sources. Changed native
   sources require a new signed/notarised immutable companion and a reviewed pin;
   a Go-only release reuses the existing pin. Do not ship a newer Go package with
   an older companion that omits merged native fixes.
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

The validator discovers the installed client's recommended model through
`model/list`, records its concrete ID and fixes that selection across direct and
proxied traffic. Discovery uses an isolated profile and fails if it cannot find
an unambiguous visible default. Synthetic checks use their reviewed fixtures;
they do not establish real model availability. Changing the source, client or
other material validation inputs invalidates evidence that depends on them.

## Tag and dispatch

After the current-main and qualification gates pass, create a signed annotated
tag for the selected stable version and push that tag. For example, substitute
the selected version in `git tag -s vX.Y.Z` and `git push origin vX.Y.Z`.
Never move a published tag or force-push shared release history.

The **Release** workflow is manually dispatched against the tag. Pushing the
tag alone does not publish CQ. Its source preflight rejects a tag outside the
current reviewed `origin/main` tip and rejects invalid stable version ordering.

The workflow stages are:

| Stage | Gate/output |
| --- | --- |
| `release-source` | Current reviewed main provenance and sequential stable version |
| `native-network`, `validate-codex` | Verify pinned signed/notarised native asset; installed Codex normal/compaction/hard-limit/rescue and reviewed corpus gates |
| `draft-build` | Require the commit-bound live routing status, bind corpus provenance/native pin/Gemini secret, generate immutable candidate package inputs and GitHub draft |
| `release` | macOS Cask formatting, quarantine and actual predecessor-to-candidate lifecycle checks |
| `windows-packages`, `windows-acceptance` | Build AMD64/ARM64 per-user MSI and manifests, validate package/installer lifecycle |
| `publish` | Publish qualified GitHub assets and generated companion/main Casks; reject draft companion dependencies |
| `linux-install`, `windows-deployed` | Download public packages and verify deployed installer/lifecycle behaviour |
| `winget-publish` | Submit the community manifest after deployed checks, then account for superseded candidates |

Some jobs run in parallel; dependency edges in the workflow remain authoritative.
The post-publication checks can fail after GitHub/Homebrew publication. Report
which channels are already public and the exact failed stage. Do not describe a
partially published attempt as either an unpublished draft or completed delivery.

## Toolchain and package configuration

GoReleaser v2 builds `cmd/cq` for Darwin, Linux and Windows, each in AMD64 and
ARM64, with `CGO_ENABLED=0` for distributed executables. Archives are tarballs
except Windows ZIPs. The version, Gemini OAuth secret and reviewed Codex corpus
provenance are linked at build time. Race validation runs separately on supported
native hosts; a cross-build does not replace it.

Windows packaging uses .NET 8 and WiX 5.0.2. MSI lifecycle and WinGet manifests
are separate from ZIP archives and the Go installer runner. macOS packaging
installs the signed universal companion through its reviewed immutable pin on
macOS 13 and later. CI downloads/verifies that companion; it has no Apple signing
or notarisation credentials. Preserve the third-party ZCode licence, published
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
- Homebrew Casks point to the reviewed package and immutable companion. Check
  generated Cask bytes and both pinned/installed companion versions.
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
state; a native rollback selects a previously reviewed immutable asset through a
new pin change. Never overwrite published tags/assets. Repair release omissions
with the next complete sequential version, and annotate affected release notes
so previous publication state remains clear.
