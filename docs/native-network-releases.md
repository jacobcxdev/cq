# Native network releases

The `cq-network` Cask installs the signed `CQNetwork.app` companion. The main `cq`
Cask depends on it on macOS 13 and later, preserving core CQ's macOS 12 support.
Installation does not activate the network extension or
restart ChatGPT. `cq codex network enable` performs the explicit macOS approval
and readiness checks after WireGuard has been configured.

The [parent release guide](releases.md) describes stable CQ provenance and
publication. The [native README](../native/macos-network/README.md) owns build,
provisioning, signing and notarisation commands.

The native companion has an independent version. A Go-only CQ release reuses the
same reviewed native asset and does not need another signature or notarisation.
Validation fetches the exact pinned source commit and compares its
`native/macos-network` Git tree with the current release. Changed or uncommitted
native sources require a new signed companion. Release the native sources from
the final reviewed, merged commit on current fetched `origin/main`; keep that tag on its original source commit. If native
sources need a new asset, merge the source update first, stage the native release
from that main commit, then review the pin update before the parent release.
Do not publish a native feature-branch or pre-merge candidate.
Apple signing keys and notarisation credentials stay on the authorised release
Mac; GitHub Actions only downloads and verifies completed artefacts.

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
[Architecture](architecture.md) when reusing the existing native pin. Do not
weaken the tree comparison or rebuild a companion solely to relocate general
documentation. The existing [native README](../native/macos-network/README.md)
remains the build/signing entry point.

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

## Stage a native release

1. Fetch immediately before tagging and verify the native source commit is the
   current reviewed `origin/main` tip. Inspect native releases, tags and drafts;
   select the next sequential version in that stream and account for failed or
   superseded attempts. Build those sources using the [signing instructions](../native/macos-network/README.md).
   Record the exact source commit. Both the host and provider must contain `arm64` and `x86_64` slices.
2. Notarise the app and staple its accepted ticket. Create a ZIP preserving the
   application bundle as the single top-level app, named
   `CQNetwork_VERSION_universal.zip`.
3. Create a signed tag and stage a separate draft GitHub prerelease named
   `cq-network-vVERSION`, targeting the verified full source commit SHA.
   Mark it as non-latest. Upload the final ZIP once; never replace it. Keep native releases as prereleases so the
   existing stable Go release inventory remains unambiguous.
4. Record the uploaded asset ID, SHA-256 and app's `LSMinimumSystemVersion` in a
   reviewed pin:

   ```sh
   go run ./internal/tools/nativepackage pin \
     "$NATIVE_VERSION" "$NATIVE_SOURCE_COMMIT" "$NATIVE_ASSET_ID" \
     "$NATIVE_ZIP_SHA256" "$NATIVE_MINIMUM_MACOS" \
     > .github/native-network-release.json
   ```

5. Validate the staged asset and generate the companion Cask:

   ```sh
   .github/scripts/prepare-native-network.sh \
     .github/native-network-release.json /tmp/cq-native-cask --allow-draft
   ```

The pin is required before merging or releasing these packaging changes. Missing
or incomplete pins fail the release gate. The validation checks the exact GitHub
tag, source commit, asset ID and server digest, then verifies the downloaded ZIP
digest, safe archive paths, company Developer ID signatures, stapled notarisation
ticket, Gatekeeper acceptance, bundle versions, minimum macOS and both binary
architectures. It never launches the companion or activates the extension.

Validation downloads draft ZIPs through the authenticated GitHub asset API and
uses a local-file Cask URL only on disposable CI runners. The production Cask
always references the pinned release URL. This lets the workflow build a proper
Go draft release before local installed-traffic qualification.

After that qualification passes, publish the native prerelease without changing
its asset. The final package-publication gate rejects a draft native asset.
For standalone Codex, qualify a normal `codex` launch without configuration
overrides as well as the Homebrew executable. The CLI can select a managed
app-server copy; `codex app-server daemon version` reports its executable path.
A successful stdio `app-server` probe from Homebrew does not qualify that copied
daemon. Keep routing enabled and verify that the normal launch reaches Ready.
Then the release workflow publishes the generated `cq-network.rb` before `cq.rb`.
An unchanged pin produces unchanged companion Cask bytes on later Go releases.
Fresh macOS packaging CI installs the verified companion dependency, then runs
the existing CQ installation, upgrade and removal checks. It leaves network
activation to separate installed-traffic qualification.

## Publish a native update

Use a new native version, increasing build number, prerelease and ZIP. Update the
pin through review. Replacing a ZIP under the same filename changes its asset ID
or digest and is rejected. A rollback selects a previously reviewed immutable
native release through a new pin change; it does not overwrite a released asset.
