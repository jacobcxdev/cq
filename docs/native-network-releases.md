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

Build native sources from the current reviewed, merged `origin/main` tip, then
review and merge their immutable asset pin before tagging the parent release.
Validation proves the build source is an ancestor of the final parent source
and both complete `native/macos-network` Git trees match. A changed native tree
requires a fresh build and pin. Apple signing keys and notarisation credentials
stay on the authorised release Mac; GitHub Actions downloads and verifies
completed artefacts.

The generated CQ Cask reconciles the companion during installation and upgrade.
It checks the dependency against that release's reviewed native pin and uses
Homebrew's own upgrade transaction when the installed companion is older.
An already-current companion is left alone. Metadata inspection and download-only
commands do not upgrade it. Existing native routing is restored after a companion
replacement or rollback; replacement can briefly cycle that routing. Restoration
failures are reported explicitly. A previously disabled companion stays disabled.

## Native source and documentation boundary

The pin compares the entire `native/macos-network` Git tree, including its
`README.md`, package manifest, tests and packaging files. A documentation-only
edit inside that tree changes its identity even if executable behaviour is
unchanged. Keep general maintainer guidance in this guide or
[Architecture](architecture.md). After staging a native build, merge only changes
that preserve its complete native tree; otherwise rebuild and review a new pin
before continuing. Do not weaken the tree comparison. The existing
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

## Stage the shared release

1. Inspect stable releases, tags, drafts and other release work. Select the next
   sequential parent version and account for failed or superseded attempts. Keep
   one active draft for that version. Merge all intended source changes first.
2. Fetch immediately before creating a signed `cq-native-stage-vVERSION` tag and
   verify its source is the current reviewed `origin/main` tip. Record this full
   commit SHA as `NATIVE_SOURCE_COMMIT`. Keep the tag on that commit permanently.
   Build those sources using the [signing instructions](../native/macos-network/README.md).
   Set the host and provider short versions to the parent `VERSION`, use a new
   increasing build number, and include both `arm64` and `x86_64` slices.
3. Notarise the app and staple its accepted ticket. Create
   `CQNetwork_VERSION_universal.zip`, preserving `CQNetwork.app` as the single
   top-level application. Create one unpublished stable GitHub draft with tag
   `cq-native-stage-vVERSION`, title `vVERSION` and target equal to the full native
   source SHA. Keep `draft=true` and `prerelease=false`. Upload the final ZIP once;
   never replace it. This draft will become the parent release.
4. Record the real positive uploaded asset ID, SHA-256 and app's
   `LSMinimumSystemVersion` in a schema-2 base pin:

   ```sh
   go run ./internal/tools/nativepackage pin-parent \
     "$VERSION" "$NATIVE_SOURCE_COMMIT" "$NATIVE_ASSET_ID" \
     "$NATIVE_ZIP_SHA256" "$NATIVE_MINIMUM_MACOS" \
     > .github/native-network-release.json
   ```

5. Review and merge that pin through the normal PR process. Do not put generated
   bundle digests in the source pin: those archives contain the final parent CLI
   and are assembled later. Validate the staged native asset:

   ```sh
   .github/scripts/prepare-native-network.sh \
     .github/native-network-release.json /tmp/cq-native-cask --allow-draft
   ```

6. Qualify the final parent source and fetch `origin/main` again. Prove the final
   commit is its current tip, the native build source is its ancestor, and their
   complete native trees match. Create and push the signed parent `vVERSION` tag
   at that final commit. Retarget the same unpublished draft, retaining its
   native asset ID and bytes:

   ```sh
   gh release edit "cq-native-stage-v$VERSION" \
     --repo jacobcxdev/cq --tag "v$VERSION" \
     --target "$FINAL_SOURCE_COMMIT" --title "v$VERSION"
   ```

   Keep the draft unpublished and stable. Do not move either Git tag or create a
   second release. Before adding CLI assets, the workflow requires the draft's
   tag and title to equal `vVERSION`, its target to equal the full final source
   SHA, and `draft=true`.
7. Dispatch the parent Release workflow against `vVERSION`. It verifies the
   pinned ZIP, adds the CLI archives and combined macOS bundles to that draft,
   and generates both Casks from the same per-architecture bundle map. After all
   required package gates pass, it publishes one stable parent release and the
   paired Casks. There is no separate native publication step.

Missing, incomplete or mismatched pins fail the release gate. Validation checks
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
