# Native network releases

The `cq-network` Cask installs the signed `CQNetwork.app` companion. The main `cq`
Cask depends on it on macOS 13 and later, preserving core CQ's macOS 12 support.
Installation does not activate the network extension or
restart ChatGPT. `cq codex network enable` performs the explicit macOS approval
and readiness checks after WireGuard has been configured.

The [parent release guide](releases.md) describes stable CQ provenance and
publication. The [native README](../native/macos-network/README.md) owns build,
provisioning, signing and notarisation commands.

CQ and Network share one version and one public release. Every CQ release,
including Go-only fixes, rebuilds, signs and notarises the companion at that
version. The native build number increases monotonically. Existing
`cq-network-vVERSION` releases remain immutable historical assets; new releases
use the parent `vVERSION` release.

Each `cq_VERSION_darwin_ARCH_bundle.tar.gz` contains `cq` and the universal
`CQNetwork.app`. The `cq` and `cq-network` Homebrew receipts remain separate
internally, but both Casks select the same combined bundle URL and SHA-256 for
the host architecture. Existing CLI-only archive names and contents remain
available for installer compatibility.

Build native sources from a signed staging tag at the current reviewed, merged
`origin/main` tip. After that ZIP passes, tag the final reviewed main commit as
the parent release. The staging tag, draft release, asset ID and server digest
produce a temporary manifest inside the release workflow. No asset pin is
committed or reviewed through a second PR. Apple
signing keys and notarisation credentials stay on the authorised release Mac;
GitHub Actions downloads and verifies completed artefacts.

The generated CQ Cask reconciles the companion during installation and upgrade.
It checks the dependency against that release's verified native manifest and uses
Homebrew's own upgrade transaction when the installed companion is older.
An already-current companion is left alone. Metadata inspection and download-only
commands do not upgrade it. Existing native routing is restored after a companion
replacement or rollback; replacement can briefly cycle that routing. Restoration
failures are reported explicitly. A previously disabled companion stays disabled.

## Native source and documentation boundary

The release manifest compares the entire `native/macos-network` Git tree,
including its `README.md`, package manifest, tests and packaging files.
Build from the staging tag after all intended native changes merge. Before the
parent tag, verify its complete native tree still matches the staged source.
Keep general maintainer
guidance in this guide or [Architecture](architecture.md). The existing
[native README](../native/macos-network/README.md) remains the build/signing entry
point.

The Swift package uses Swift 6 tools, Swift 5 language mode and macOS 13 or later.
Its library, command-line host and signed Xcode app are distinct build products.
Signing requires Xcode, the authorised company account, Developer ID certificates
and Network Extension provisioning. A plain `swift build` does not produce the
distributable signed system extension. Build/export commands can contact Apple
to update provisioning; local package tests do not qualify that signed result.

Keep source identity, universal slices, host/provider versions, signatures,
entitlements, accepted notarisation and the stapled ticket as package evidence.
Installation still does not establish activation: confirm system-extension
approval and installed provider version, then exercise selected TCP/UDP traffic
and a normal standalone Codex launch. Broker readiness and a Homebrew stdio
probe are separate from that installed admission evidence.

## Build the shared release

1. Inspect stable releases, tags, drafts and other release work. Select the next
   sequential parent version and account for failed or superseded attempts. Keep
   one active draft for that version. Merge all intended source changes first.
2. Fetch `origin/main` immediately before creating the signed
   `cq-native-stage-vVERSION` tag and prove its commit is the reviewed current
   tip. Build those tagged sources using the
   [signing instructions](../native/macos-network/README.md). Set the host and
   provider short versions to `VERSION`, use a new increasing build number, and
   include both `arm64` and `x86_64` slices.
3. Notarise the app and staple its accepted ticket. Create
   `CQNetwork_VERSION_universal.zip`, preserving `CQNetwork.app` as the single
   top-level application. Create one unpublished stable GitHub draft with tag
   `cq-native-stage-vVERSION`, title `vVERSION`, target equal to the full staged
   source SHA, and release notes headed `## Changelog`. Keep `draft=true` and
   `prerelease=false`. Upload the final ZIP once; never replace it.
4. Resolve the draft's uploaded asset ID and server digest into a temporary
   schema-2 manifest, then validate the signed and notarised ZIP:

   ```sh
   mkdir -p /tmp/cq-native-cask
   .github/scripts/resolve-native-network.sh "v$VERSION" /tmp/cq-native-cask/native-network-release.json --staging-draft
   .github/scripts/prepare-native-network.sh \
     /tmp/cq-native-cask/native-network-release.json /tmp/cq-native-cask --allow-draft
   ```

5. Complete final source qualification, fetch `origin/main` again, and prove its
   current reviewed tip contains the staged source with an identical complete
   native tree. Create and push the signed parent `vVERSION` tag at that tip.
   Retarget the same unpublished draft to the parent tag and full final source
   SHA, keeping its title, notes, native asset ID and bytes. Dispatch the
   Release workflow against `vVERSION`. It derives and carries the same asset
   identity without a metadata PR, adds CLI archives and combined macOS bundles
   to that draft, and generates both Casks from the same bundle map. After
   package gates pass, it publishes one stable release and paired Casks.
   There is no separate native publication step.

Missing, incomplete or mismatched manifests fail the release gate. Validation checks
GitHub tag, source ancestry and tree identity, asset ID and server digest, then
verifies the downloaded ZIP digest, safe archive paths, company Developer ID
signatures, stapled notarisation ticket, Gatekeeper acceptance, host/provider
versions, minimum macOS and both binary architectures. Final bundle extraction
must preserve and pass the signed-app checks again. Validation never launches
the companion or activates the extension.

Draft ZIPs download through the authenticated GitHub asset API. Local-file Cask
URLs are limited to disposable CI runners. Production Casks reference their
shared parent bundle URL. Fresh macOS packaging CI runs installation, upgrade,
rollback and removal checks while preserving existing routing intent.

## Installed qualification and repair

Installation and publication do not establish activation. Confirm the installed
CQ, host and active provider versions all match, then qualify affected TCP/UDP
traffic after explicit system-extension activation and any macOS approval.
For standalone Codex, test a normal `codex` launch without configuration overrides
as well as the Homebrew executable. The CLI can select a managed app-server copy;
`codex app-server daemon version` reports its executable path. A successful stdio
probe from Homebrew does not qualify that copied daemon. Keep routing enabled
and verify the normal launch reaches Ready.

A package rollback restores the matching reviewed native payload and previous
routing intent. A release repair uses the next complete sequential parent
version, a new increasing native build number and a new immutable ZIP. Preserve
published tags and assets; never replace a ZIP, move a tag or backfill the retired
independent native stream.
