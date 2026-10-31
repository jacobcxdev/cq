# Native network releases

The `cq-network` Cask installs the signed `CQNetwork.app` companion. The main `cq`
Cask depends on it on macOS 13 and later, preserving core CQ's macOS 12 support.
Installation does not activate the network extension or
restart ChatGPT. `cq codex network enable` performs the explicit macOS approval
and readiness checks after WireGuard has been configured.

The native companion has an independent version. A Go-only CQ release reuses the
same reviewed native asset and does not need another signature or notarisation.
Validation fetches the exact pinned source commit and compares its
`native/macos-network` Git tree with the current release. Changed or uncommitted
native sources require a new signed companion. Keep the native release tag on its
original source commit so it remains available after a squash merge.
Apple signing keys and notarisation credentials stay on the authorised release
Mac; GitHub Actions only downloads and verifies completed artefacts.

## Stage a native release

1. Build the final committed native sources using the signing instructions in
   `native/macos-network/README.md`. Record the exact source commit. Both the
   host and provider must contain `arm64` and `x86_64` slices.
2. Notarise the app and staple its accepted ticket. Create a ZIP preserving the
   application bundle as the single top-level app, named
   `CQNetwork_VERSION_universal.zip`.
3. Stage a separate draft GitHub prerelease named `cq-network-vVERSION`,
   targetting the full source commit SHA. Mark it as non-latest. Upload the final
   ZIP once; never replace it. Keep native releases as prereleases so the
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
