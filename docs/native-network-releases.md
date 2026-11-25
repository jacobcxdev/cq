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

The Release workflow builds, signs and notarises native sources on a disposable
GitHub macOS runner. Signed staging and parent tags identify the same current,
reviewed `origin/main` commit. CI creates or reconciles one parent draft and
uploads its final native ZIP once; its asset ID and server digest produce a
temporary manifest without a pin PR. Apple credentials live in the `native-release` GitHub Actions environment
and temporary runner storage. Fastlane and a local Xcode account are not
required for release publication.

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

## Configure CI credentials

Configure the `native-release` environment in `jacobcxdev/cq` with a custom
deployment policy for release tags (`v[0-9]*.[0-9]*.[0-9]*`) and no branch
patterns. Store these values in that environment's Actions secrets through an
authorised secure channel. Keep values and passwords out of source, logs, release
notes and chat.

| Secret | Required value |
| --- | --- |
| `APPLE_DEVELOPER_ID_P12_BASE64` | Base64 of a password-protected `.p12` containing exactly the company Developer ID Application certificate and its private key |
| `APPLE_DEVELOPER_ID_P12_PASSWORD` | Password protecting that `.p12` |
| `APPLE_HOST_PROFILE_BASE64` | Base64 of the Developer ID distribution profile for `dev.jacobcx.cq.network` |
| `APPLE_PROVIDER_PROFILE_BASE64` | Base64 of the Developer ID distribution profile for `dev.jacobcx.cq.network.proxy` |
| `APPLE_NOTARY_KEY_P8` | App Store Connect API private key in PEM format, authorised for company notarisation |
| `APPLE_NOTARY_KEY_ID` | Identifier of that API key |
| `APPLE_NOTARY_ISSUER_ID` | Company issuer identifier for that API key |

The signing identity and both profiles must use team `K2648T24P4`. Both profiles
must be unexpired Developer ID distribution profiles authorising that certificate,
the exact application identifier and `app-proxy-provider-systemextension`. CI
checks these before archiving and verifies the signed application group after
export. Refresh
expired profiles or credentials explicitly; CI never creates certificates or
changes Apple portal access automatically.

Only the native signing step receives the `.p12`, password and profiles. The
notarisation submission and finishing steps receive only the API key inputs. PR checks use
synthetic credentials and fake external commands. Private keys, imported
keychains and task-installed profiles are removed on exit, and the prior
keychain search list is restored. Retained request artefacts contain signed
public binaries and receipt metadata, never signing credentials.

## Build the shared release

1. Inspect stable releases, tags, drafts and other release work. Select the next
   sequential parent version and account for failed or superseded attempts. Keep
   one active parent draft. Merge all intended source changes and complete the
   required source/client qualification first.
2. Fetch `origin/main` immediately before tagging and prove the checked-out
   commit is its current reviewed tip. Create and push signed annotated
   `cq-native-stage-vVERSION` and `vVERSION` tags at that same commit. Never move
   an existing tag. The complete native tree, including packaging and README,
   belongs to that source identity.
3. Choose an integer native build number greater than the latest published
   companion's `CFBundleVersion`. Dispatch the existing Release workflow:

   ```sh
   gh workflow run release.yml --repo jacobcxdev/cq --ref vVERSION -f native_build_number=BUILD_NUMBER
   ```

   Replace both placeholders with the inspected version and build number. The
   native job pins Xcode 16.4 on `macos-15`, records the toolchain, verifies both
   GitHub-verified annotated tags, fresh main, sequential version, increasing
   build and sole parent draft before using Apple credentials.
4. CI archives universal `arm64`/`x86_64` host and provider binaries using manual
   Developer ID signing and distinct distribution profiles. It preserves the
   source entitlements while selecting the distribution Network Extension value,
   exports and verifies the app, then submits the exact signed ZIP with
   `notarytool`.
5. Before waiting for Apple, CI retains `payload.zip` and `submission.json` as
   `native-notary-request-RUN_ATTEMPT` for seven days. The receipt binds source
   SHA, version, build, SHA-256 and submission ID. After Apple accepts, CI staples
   and verifies the ticket, signatures, versions and architectures, then uploads
   `CQNetwork_VERSION_universal.zip` to the parent draft without replacement.
6. The existing package, installed-client, platform and publication gates derive
   the temporary manifest from that immutable asset and publish one stable parent
   release and paired Casks. There is no separate native publication or local
   native build step.

## Resume a notarisation attempt

Rerun the failed native job or dispatch the same tag and build again. CI finds
the latest notarisation submission step for the exact release tag and source
commit and recovers its request. Archive or signing failures before that step
can retry. If the submission step started but its artefact is missing or expired,
CI stops before submitting again. Preserve release run history while
a draft is active.
The finishing helper independently verifies its version, build, source, payload
hash and safe archive paths, then waits on the recorded submission ID without
signing or submitting again.

A request marked `submission-uncertain` means upload was interrupted or Apple did
not return a usable ID. CI refuses automatic resubmission. Reconcile it against
Apple's submission history before deciding what to do; do not delete the marker
and rerun blindly. An expired/missing recovery artefact also requires explicit
reconciliation. Preserve the exact request while that investigation remains open.
If Apple rejects the payload, inspect its notarisation log before selecting the
next authorised attempt.

If the parent draft already holds the final native ZIP, CI verifies the existing
asset, digest, signed bundle and requested build number and reuses it. A mismatch
fails; CI never replaces the asset. Do not rerun a complete release after
publication to repair omissions: use the next complete sequential version.

If an unpublished attempt cannot ship from its signed tags, record the failure
and replacement version in the draft notes and set its title to `vVERSION (retired)`.
Keep the draft private with its original tags and assets. CI will reject that
draft as a release target and allow a separate active draft. Choose the next
allowed stable version from the latest published release and account for all
changes in the retired attempt.

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
