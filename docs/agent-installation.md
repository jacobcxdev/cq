# Installing CQ with an AI agent

[README](../README.md) · [Configuration](configuration.md) · [Troubleshooting](troubleshooting.md)

Read this guide before installing or configuring CQ for a user. Follow their
requested outcome, preserve existing state, and verify the client they intend
to use. Installing a package is not proof that its optional integrations work.

## 1. Establish the requested outcome

Infer the goal from the user's request. Ask only when the choice materially
changes the work; do not enable every feature by default.

| Requested outcome | Required setup |
|---|---|
| Check subscription quota | Install CQ and use supported existing credentials or the provider's login flow. No client routing is required. |
| Route a compatible CLI through CQ | Install CQ, connect the client to its local proxy, and verify a real client request. See [client setup](usage.md#connecting-a-client). |
| Access a primary Dot before Dots is available in the user's region | On supported macOS, install CQ with its signed CQ Network companion, configure a **US WireGuard profile** and enable native network routing. Normal ChatGPT launching and primary onboarding remain available through OpenAI's own UI; the multi-account Dots adapter is not required. |
| Show Dots from multiple accounts in one native sidebar | Configure accounts with existing onboarded Dots and install the optional [Dots adapter](dots.md). This requires its CQ launcher; network routing alone does not aggregate accounts. Add US WireGuard egress when regional access requires it. |

Normal ChatGPT network routing, usage response adaptation and multi-account
Dots are separate features. Do not install certificates, enable usage
adaptation, add accounts or install the Dots adapter unless the goal needs them.
The WireGuard integration's user-facing purpose is regional Dot access, not a
general-purpose system VPN. A US exit does not grant an upstream account
entitlement. Primary onboarding remains OpenAI's normal native or web flow;
CQ does not provide onboarding slots for secondary Dots.

## 2. Inspect existing installation and preserve state

Before mutation, inspect the executable selected by the shell, its version,
package-manager records and the service owner. On macOS, useful reads are:

```sh
command -v cq
type -a cq
brew list --cask
brew list --formula
```

When CQ is present, also inspect:

```sh
cq --version
cq service status --json
```

A missing executable or failed status is evidence to investigate, not permission
to delete configuration. Preserve credentials, WireGuard profiles, generated
local tokens, client settings and unrelated software. Keep any configuration
backup private. Do not print tokens, profile contents or other secrets in the
terminal transcript, final report, patches or repository.

For an existing supported installation, prefer its normal package upgrade.
If the user requests removal and reinstallation, or a package-owner migration
requires it, use the owning installer's uninstall procedure first. Do not layer
another service owner over it or delete arbitrary binaries and LaunchAgents.
Uninstall preserves CQ user data; do not add a purge step.

For a legacy Homebrew formula, use the [documented formula-to-Cask
migration](../README.md#macos--homebrew-cask). For a Go-runner installation,
use its [documented uninstall](../README.md#install) before installing the Cask;
using that legacy uninstaller does not make Go the new macOS installation path.
Read [Homebrew upgrades](homebrew-upgrades.md) before migrating old service
layouts or repairing failed package hooks. Coordinate a maintenance window when
removal or first adoption will interrupt active clients. Never kill active work
to make an installation check pass.

## 3. Use the platform package

### macOS: Homebrew Cask

Use the official Cask for normal user installations:

```sh
brew install --cask jacobcxdev/tap/cq
```

For an existing Cask installation, upgrade with:

```sh
brew upgrade --cask jacobcxdev/tap/cq
```

The complete Cask installs CQ and its current-user proxy and refresh services.
On macOS 13 and later, it also includes the separately versioned signed CQ
Network companion. CQ and CQ Network version numbers need not match. Let the
package manage the reviewed companion version.

Go, Xcode, Python and a source checkout are not prerequisites for this
installation. Do not substitute `go install`, the Go installer runner or a
locally built binary. Do not overwrite the package executable or patch the
installed app. Do not remove a user's existing developer tools as cleanup.
If Homebrew is unavailable, explain that prerequisite and arrange its setup
under the user's instructions rather than silently changing install methods.

### Windows and Linux

On Windows, use the official package:

```powershell
winget install jacobcxdev.cq
```

For Linux and explicit alternative installation requirements, follow the
[platform installation instructions](../README.md#install). The Go installer
runner downloads an official release but needs a Go toolchain; a source-built
`go install` binary is not an equivalent complete installation. Do not apply
macOS-only CQ Network or native Dots instructions to other platforms.

### Verify installation ownership

Check `cq --version` and `cq service status --json` after installation. Confirm
the shell resolves the expected package and the service owner matches it. The
service may use a retained immutable executable rather than the package's
symlink; inspect its reported active and pending versions instead of requiring
identical executable paths. A deferred Homebrew runtime upgrade is not proof
of failure and does not require repeated reinstalls.

Do not run manual `cq proxy install` or `cq service install` as a normal
post-install step; complete installers already own both background components.

## 4. Configure only the selected integration

Use installed `cq COMMAND --help` for exact supported arguments. Reuse existing
credentials when suitable; do not switch the user's active account merely to
add one to CQ. Follow [account management](usage.md#accounts-and-authentication) and [client
setup](usage.md#connecting-a-client) for ordinary quota and routing workflows.

### Primary Dot access before regional rollout

1. Confirm the intended signed-in ChatGPT account and obtain the user's US
   WireGuard profile. Preserve an existing primary Dot. If it is not onboarded,
   use OpenAI's normal primary onboarding after routing is ready; do not require
   a secondary-account onboarding workaround.
2. Follow [Codex proxy egress](configuration.md#codex-proxy-egress) to merge an
   absolute private `codex_wireguard_profile` path into the actual CQ proxy
   configuration. Preserve the generated token and unrelated fields. WireGuard
   and `codex_egress_proxy` are mutually exclusive; resolve an existing setting
   with the user rather than silently discarding it.
3. With the CQ service running, request native routing:

   ```sh
   cq codex network enable
   cq codex network status
   ```

4. If macOS requests system-extension or proxy-configuration approval, explain
   the exact pending approval and where the user must act. Do not bypass an OS
   approval. After approval, rerun `enable` if necessary and check `status`.
   An interrupted activation can resume; do not reinstall CQ solely because
   approval is pending.
5. Verify native traffic as described below. Continue using the normal
   ChatGPT app and Dock icon; a single primary Dot does not need the
   `ChatGPT via CQ` launcher.

Enabling routing does not restart ChatGPT. Existing connections keep their
original route until they reconnect, and the app can retain account-availability
state. If a restart is necessary, coordinate it with the user; do not interrupt
their other sessions without authorisation. Do not change system DNS, a VPN,
Private Relay or Keychain trust as a speculative repair.

### Multiple account-owned Dots

Follow [the Dots installation guide](dots.md#install). Each secondary entry is
the owning account's already-onboarded primary Dot. If setup is missing, use a
separate browser profile/private window, or coordinate a quit and open the
original desktop app without the multi-account adapter. Sign into that account,
complete normal onboarding, restore the original account and quit before
reopening through CQ. Do not switch native accounts inside an adapted session.
A private browser window does not change its region; CQ Network's
ChatGPT route does not cover the browser. After onboarding, sync CQ and arrange
a relaunch through CQ when `restart_required` is reported.

Its launcher and native adapter are additional setup, not a replacement for
package installation.
Review its account, renderer and port requirements. Automatic setup sends an
ordinary one-time message to each discovered Dot. Do not manually re-prime or
broadcast merely to test installation. Respect `restart_required` and preserve
an already-running app until the user authorises the transition.

## 5. Verify the requested outcome

Inspect service and routing state without confusing them with client success:

```sh
cq service status --json
cq proxy status --strict --json
```

For native WireGuard routing, also run `cq codex network status`. It requires
the extension, registered broker and authenticated CQ connection to be ready.
If a check fails, preserve the error and diagnose the failing layer using
[troubleshooting](troubleshooting.md); do not patch a diagnostic to turn it green.

| Evidence | What it establishes |
|---|---|
| Installed version and package records | Package installation and ownership. |
| Healthy service or proxy status | Reported service/runtime readiness, not native ChatGPT capture or upstream entitlement. |
| Synthetic HTTP and WebSocket requests through CQ | Those explicit proxy paths work; independently launched ChatGPT may use another path. |
| Public exit IP checked through the configured CQ WireGuard egress | That tested path's public exit. An ordinary browser or terminal IP check may use another route. IP geolocation alone does not prove account eligibility. |
| Native extension flow counters or metadata traces correlated with a ChatGPT action | Actual app traffic reaches the native routing path. Combine this with tunnel/egress evidence; readiness alone is insufficient. |
| Dot appears and its conversation opens | Account availability and conversation access in that app session. |
| An authorised harmless message receives a reply | End-to-end messaging for that tested account and path. |

For quota-only use, verify a fresh result from the configured provider. For CLI
routing, verify a real request from the intended client. For native Dot use,
verify the app path and conversation, not just the proxy. Use existing user
authorisation for test messages or quota use; if it does not cover the proposed
test, obtain it first. Do not replay an uncertain send.
If a required observation is unavailable, report it as unverified rather than
inventing a command, inferring success or applying a speculative patch.

## 6. Report completion or the exact remaining step

State the installed package/version, service owner, selected integration and
verified outcome. Separate observed failures from hypotheses. Include the
exact next action for a pending approval or client restart. For example:

> Setup incomplete: CQ is installed through Homebrew and its proxy works.
> Native ChatGPT routing is waiting for macOS approval. Approve CQ Network,
> then rerun `cq codex network enable` and `cq codex network status`.
> ChatGPT routing and Dot access are not yet verified.

Do not report "all working" from a healthy service alone. Never include account
tokens, private keys, authentication URLs or private conversation contents in
the completion report.
