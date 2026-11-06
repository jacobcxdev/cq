# macOS app-scoped routing

The `CQNetwork` library supplies the admission boundary for a transparent Network
Extension. It does not install, activate or configure an extension by itself.

`TransparentFlowAdmission` checks the flow's OS audit token with
Security.framework. A selected source must have an OpenAI Developer ID signature,
an allowed stable signing identifier, and an executable inside the configured,
independently validated ChatGPT bundle. The standalone `codex` signing identifier
also permits an independently signed Homebrew CLI outside that bundle. The
provider has read-only sandbox access to `/opt/homebrew/Caskroom/codex/` and
`/usr/local/Caskroom/codex/` so Security.framework can validate the executable.
A narrow read-only sandbox rule also permits the managed daemon executable at
`/Users/<user>/.codex/packages/app-server-daemon/releases/<release>/bin/codex`.
That rule grants no writes or access to authentication files, sibling paths,
custom homes or custom Codex cache locations. Certificate, identifier and team
requirements remain identical for all supported locations. Other locations fail
closed if the sandbox prevents validation.
Display names, build numbers and CDHashes
are not used as identity. These checks inspect public code signatures and do not
query Keychain or request permissions.

Flows already waiting for the same complete OS audit token and signing
identifier share one in-progress source check. The result is retained only by
those waiting flows; a flow arriving after the check finishes starts another
full check. No successful trust cache, PID-only identity or skipped bundle
validation is used. Cancellation and destination checks remain per flow.
Each in-progress check occupies one scanner worker; waiting flows register
callbacks rather than blocking other workers. Cancelled waiting flows retain
their bounded admission slots until the shared check completes, preventing a
backlog of abandoned verification work.

Metadata-only debug events in subsystem `dev.jacobcx.cq.network`, category
`admission`, distinguish flow-capacity and verification-capacity refusals and
report verification wait time. Broker readiness alone does not establish that
new flows are passing admission.

The `source-verification` category records failed Security.framework stages and
numeric status codes. Both `source_check_failed` and `source_check_verified`
include `source_pid`, `source_pidversion` and the whitelisted `signing_identifier`,
so a shared check can be correlated with one client process instance. Missing or
malformed tokens fail closed and report PID `-1` and version `0`. These fields are
diagnostic metadata, not an admission identity. Events do not record audit-token
bytes, credentials or executable paths.

The provider must interpret admission decisions as follows:

- `passThrough`: return `false`; leave unrelated apps and local destinations on
  their original route.
- `proxy`: return `true` and bridge the flow through CQ's authenticated transport.
- `block`: call `TransparentFlowAdmission.refuse`; do not permit direct fallback.

Once a selected flow has been accepted, broker, tunnel or forwarding errors must
close the affected transport. A failed UDP destination channel is retired without
closing healthy siblings in the same flow. A later datagram can open a new channel
through the same admission and routing policy; uncertain sends are not replayed.
Flow-level errors and cancellation still close the whole flow.
Returning `false` on those failures would bypass CQ. The source hint is
only a prefilter; it cannot authorise a flow without audit-token validation.

Run admission tests with:

```sh
swift test --package-path native/macos-network --sanitize thread
```

Building this package does not prove an installed Network Extension works.
Deployment also needs the native transport, signed host and extension bundles,
appropriate provisioning, macOS approval and live TCP/UDP routing qualification.
For standalone Codex, qualify a normal `codex` launch without configuration
overrides as well as the Homebrew executable. The CLI can select a managed
app-server copy; `codex app-server daemon version` reports its executable path.
A successful stdio `app-server` probe from Homebrew does not qualify that copied
daemon. Keep routing enabled and verify that the normal launch reaches Ready.
Transparent proxies retain macOS system DNS behaviour; this library does not
claim app-only DNS isolation.

Apple references:

- [Transparent proxy behaviour](https://developer.apple.com/documentation/networkextension/netransparentproxyprovider)
- [OS source audit token](https://developer.apple.com/documentation/networkextension/neflowmetadata/sourceappaudittoken)
- [Network Extension deployment](https://developer.apple.com/documentation/technotes/tn3134-network-extension-provider-deployment)

## Signed app bundles

`CQNetwork.xcodeproj` builds a background companion and its embedded transparent
proxy system extension. Both use the JacobCX Development Ltd. team
`K2648T24P4`, hardened runtime and universal `arm64`/`x86_64` binaries. The host
and provider identifiers are `dev.jacobcx.cq.network` and
`dev.jacobcx.cq.network.proxy`. Their app group is
`K2648T24P4.dev.jacobcx.cq.network`; the provider's Mach service adds `.proxy`.

With the company account already signed into Xcode, build and export with:

```sh
CQ_VERSION=1.2.3 CQ_BUILD_NUMBER=123 \
  native/macos-network/Packaging/build-sign.sh /tmp/cq-network-release
```

Use the release's version and increasing build number. `DEVELOPER_DIR` can select
an installed Xcode without changing the system developer directory. Each Xcode
operation has a ten-minute limit. The script archives with automatic development
signing, exports with automatic Developer ID signing, then verifies both
architectures and the complete signature. Xcode creates or updates the required
profiles through the selected company account. It converts the development
`app-proxy-provider` entitlement to
`app-proxy-provider-systemextension` for Developer ID distribution. The script
does not install, activate, launch or alter ChatGPT, and does not query, reset or
export Keychain credentials.

The signed result is `OUTPUT_DIRECTORY/export/CQNetwork.app`. A signed build
alone does not prove routing or user approval. Installation must place the app in
an Applications directory. Activation is an explicit separate operation and macOS
may require the user to approve the network system extension. The companion's
`run --cq /absolute/path/to/cq` command runs as the logged-in user; the extension
receives authenticated connections over XPC rather than reading that user's
WireGuard profile or private broker descriptor itself.

## Notarisation

Each build declares a distinct `NEMachServiceName` using its increasing build
number. Host and provider derive the same name from their signed bundle versions;
release validation checks both versions and the declared service. This avoids
stale service registration during extension replacement (`FB17032197`), following
[Apple DTS guidance](https://developer.apple.com/forums/thread/779395).

For a release, upload the actual archive using the account already signed into
Xcode. Copy the export options and change only its destination:

```sh
cp native/macos-network/Packaging/ExportOptions.plist /tmp/CQNetwork-upload.plist
plutil -insert destination -string upload /tmp/CQNetwork-upload.plist
xcodebuild -exportArchive \
  -archivePath /tmp/cq-network-release/CQNetwork.xcarchive \
  -exportOptionsPlist /tmp/CQNetwork-upload.plist \
  -allowProvisioningUpdates
```

After Apple accepts the archive, export the notarised result and check its ticket:

```sh
xcodebuild -exportNotarizedApp \
  -archivePath /tmp/cq-network-release/CQNetwork.xcarchive \
  -exportPath /tmp/cq-network-release/notarised
xcrun stapler validate /tmp/cq-network-release/notarised/CQNetwork.app
codesign --verify --deep --strict /tmp/cq-network-release/notarised/CQNetwork.app
```

Do not distribute an unaccepted upload as notarised. This workflow uses Xcode's
account session; it does not extract an Apple ID password, signing key or stored
notary credential. An unattended CI runner needs its own authorised signing and
notarisation credentials; the existing CQ release workflow does not supply them.
See Apple's [Developer ID distribution guide](https://developer.apple.com/developer-id/)
and [notarisation guide](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).
