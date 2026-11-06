# CQ setup and feature guide for agents

[README](../README.md) · [Configuration](configuration.md) · [Troubleshooting](troubleshooting.md)

Read this guide before installing or configuring CQ for a user, including when
they supply only the repository URL. Act as a guide as well as an installer:
explain what CQ could do for their actual clients and accounts, help them
choose, preserve existing state, and verify the requested outcome. A new user
should not need to know command names or discover features by accident.
Installing a package is not proof that its optional integrations work.

## 1. Discover needs and explain the choices

First identify the user's platform, installed clients, intended providers and
whether they use one or several accounts. Distinguish Codex CLI from ChatGPT
Desktop's Codex chats; both can be clients of CQ. Claude Code and other
explicitly compatible API clients are also candidates. Do not assume every
desktop app supports changing its API endpoint, or that Gemini quota support
means Gemini request routing. Inspect relevant app/package configuration without
dumping credentials or scanning unrelated private files.

If the user already chose a feature, carry that choice forward. Otherwise,
present a short, tailored menu of benefits using the catalogue below. Offer
relevant options proactively; do not merely ask whether they want "CQ" or wait
for them to name advanced features. Do not overwhelm them with every command,
turn discovery into a mandatory questionnaire, or silently enable everything.

### Three independent setup choices

The base package provides quota reports and background services. A user can
stop there. Explain these three additional choices separately; any combination
may be useful:

| User-facing choice | What it enables and how to configure it |
|---|---|
| **Connect my AI apps to CQ.** | Account-aware routing and the relevant models, pools, reserves, automation and capability profiles described below. Includes compatible desktop apps and CLIs. Connect each selected client's model traffic to CQ's loopback proxy. Adding an account, installing CQ Network or publishing a model does not do this by itself. |
| **Access my Dot before regional rollout.** | Reach OpenAI's normal primary Dot experience through a **US WireGuard profile**. On supported macOS, enable the signed CQ Network companion. Normal ChatGPT launching works; primary onboarding stays in OpenAI's own UI. No multi-account adapter is required. |
| **Bring my accounts' Dots into one sidebar.** | View and use already-onboarded account-owned Dots together, with optional peer coordination. Install the optional [Dots adapter](dots.md) and launch through CQ. Secondary Dots must be onboarded separately. Add US WireGuard egress when regional access requires it. |

Normal ChatGPT network routing, usage response adaptation and multi-account
Dots are separate features. Do not install certificates, enable usage
adaptation, add accounts or install the Dots adapter unless the goal needs them.
The WireGuard integration's user-facing purpose is regional Dot access, not a
general-purpose system VPN. A US exit does not grant an upstream account
entitlement. Primary onboarding remains OpenAI's normal native or web flow;
CQ does not provide onboarding slots for secondary Dots.

### Feature catalogue: reporting, accounts and connected clients

Use these stories to explain benefits. Ask about features relevant to the
user's work and record selected, declined and deferred choices. Provider and
client limits matter; these are not universal switches for every application.
Command names below are entry points; follow the linked procedure and installed
leaf help for exact arguments before making changes.

| User story to offer | Benefit and scope | Set up or inspect |
|---|---|---|
| **"Tell me which account has quota and when it resets."** | Reports Claude, Codex, Antigravity/Gemini and Z.ai quota and known capacity-weighted totals. JSON supports another agent or status tool. Unknown/unavailable data is distinct from zero quota. | `cq check`, `cq --json check`; [reports](usage.md#quota-checks-and-output). No client routing is needed just to report quota. |
| **"Forecast whether my subscriptions will last until reset."** | Burn history, pace, exhaustion estimates and aggregate/pool summaries help identify shortfalls and unused capacity. Forecasts need sufficiently fresh observations and warmup. | `cq --refresh check`; [history and forecasts](usage.md#cache-and-history). Missing capacity weights are not invented. |
| **"Add several accounts without repeatedly logging my app out."** | Store multiple Claude/Codex/Z.ai accounts and discover supported existing credentials. Ordinary CQ routing does not sign the desktop app into a different account. Gemini sign-in remains with Antigravity. | Provider account commands; [accounts](usage.md#accounts-and-authentication). Plain Claude/Codex login adds an account; activation/switch is a separate change. |
| **"Keep eligible credentials refreshed in the background."** | Complete installers schedule refresh for eligible Claude/Codex credentials. Declared external managed sources retain their ownership rules. | `cq refresh`, `cq service status`; [credential ownership](usage.md#credential-ownership). System/borrowed Codex credentials are not automatically rewritten, and owner reauthentication can still be needed. |
| **"Use available account capacity instead of stopping at the first exhausted account."** | Claude/Codex quota-aware routing balances eligible capacity; Z.ai supports bounded subscription fallback. Codex respects continuity, model compatibility and policy. This is not round-robin account switching on every message. | Connect the client and inspect configured/effective transport modes with `cq proxy status --json`; [routing](usage.md#client-routing). Package installation alone does not establish enforced per-turn routing. |
| **"Keep particular work on a chosen account."** | Pins/defaults influence selection for new or unbound work; hard continuity can keep an established chat on its existing account. | `cq proxy pin`, `cq proxy default codex`; [pins/defaults](usage.md#pins-defaults-and-priming). Follow documented reload/restart needs. |
| **"Give related chats a dedicated group of accounts."** | Named Codex pools limit eligible accounts for each explicitly bound chat. Bind several chosen chats to the same pool for a shared account group. Histories stay separate. | `cq proxy policy`; [pools and session bindings](usage.md#capability-policy-and-session-pools). Each exact session is bound separately; there is no automatic parent/child session-family binding. |
| **"Save scarce or specialised accounts for work that needs them."** | Pool values preserve higher-value capacity by preferring lower-value viable accounts for ordinary work. The system account has a separate preservation tier. | `cq proxy policy pool value`; [pool values](usage.md#capability-policy-and-session-pools). Values are preferences, not guaranteed percentage reserves or entitlement overrides. |
| **"Use eligible accounts when I request Cyber access."** | Account-scoped programme/model discovery can maintain a generated `Cyber` pool and select it for Cyber requests while retaining the chat's ordinary binding. | Policy authority and status; [Cyber-aware routing](usage.md#cyber-aware-routing). CQ does not grant Cyber entitlement or accept fabricated capability evidence. |
| **"Leave quota for my phone or other clients using my main account."** | A Codex system-account reserve protects a percentage of one selected window from CQ routing. Other eligible accounts can continue. Outside-CQ usage can still spend it. | `cq proxy reserve`; [reserve](usage.md#system-account-reserve). Distinct from the planner's automatic preference to preserve the system account. |
| **"Let existing chats use replenished capacity when it is safe."** | Lease redistribution requests fresh Codex account selection at safe boundaries, including after a successful reset. Active responses and account-owned opaque continuations are preserved. | `cq proxy leases redistribute`; [continuity](usage.md#lease-redistribution-and-invalidation). Not an immediate forced move of every chat. |
| **"Use my subscriptions' models in my preferred client."** | Provider discovery exposes supported models to compatible clients, including Z.ai models in Codex and supported Messages-to-Responses translation. Picker publication is client-specific. | `cq models list`, `cq models refresh`; [models](usage.md#model-registry), [Z.ai](usage.md#zai-coding-plan). Start/paid routes have separate entitlement and billing. Gemini is quota-only. |
| **"Expose a supported model before my client lists it natively."** | Overlays add real upstream model IDs/metadata; pruning removes entries now supplied natively. They do not create model access. | `cq models overlay`; [overlays](usage.md#model-registry). Prefer native discovery over unnecessary manual entries. |

### Feature catalogue: automation and capability selection

| User story to offer | Benefit and scope | Set up or inspect |
|---|---|---|
| **"Help me use banked Codex resets before they expire."** | Inspect credits and preview a portfolio recommendation based on fresh usage, burn and expiry. A recommendation is advisory and consumes nothing. | `cq codex resets list`, `recommend`, `use`; [Codex resets](usage.md#codex). Manual consumption needs authorisation; uncertain attempts retain their identity. |
| **"Schedule the resets I approve instead of watching them manually."** | Codex supports accepted fixed schedules and explicitly chosen adaptive timing for exact accepted credits. Execution needs the CQ runtime. | `cq codex resets recommend --schedule`, `cq codex resets schedule`; [scheduling](usage.md#codex). Adaptive timing does not enrol newly found credits. New/manual entries and enablement have documented confirmation/defaults. |
| **"Use or schedule my Z.ai reset cards deliberately."** | Choose an exact account/card/window or a fixed schedule. Weekly cards can affect both weekly and five-hour quota; Codex reset semantics differ. | `cq zai resets`; [Z.ai cards](usage.md#zai-coding-plan). Providers' credits are not interchangeable; Z.ai schedules are fixed, not adaptive. |
| **"Manage my ZCode subscription, Start offers and billing route."** | Inspect supported provider offers/cards and explicitly choose the account/route used by compatible Codex clients. | `cq zai zcode`, `cq zai route`; [Z.ai setup](usage.md#zai-coding-plan). Start never silently switches to paid quota; standard API keys and Coding Plan keys have different billing. |
| **"Start eligible usage windows before my next substantial task."** | Optional Codex/Z.ai priming sends a minimal real request to start a dormant window. It uses inference quota, not reset credits, and does not increase allowance. | `cq proxy prime`, `cq zai prime`; [priming](usage.md#pins-defaults-and-priming), [Z.ai](usage.md#zai-coding-plan). Honour provider-specific timing and restart rules. |
| **"Give this chat only the tools and instructions it needs."** | Codex harness profiles select optional plugins, MCP servers and skills without changing chat identity/history. `minimal` reduces optional capabilities; `full` restores the native baseline. | `cq codex harness inventory`, `profile`, `bind`, `explain`, `status`; [profiles](usage.md#harness-capability-profiles). Saving a definition is not proof it is applied. |
| **"Use different tool sets per project/model, and share them."** | Bind ordered profiles at chat/project/global scope, optionally by model/family; export/import stable definitions and portable project rules. A CQ curation skill helps an agent build profiles from inventory. | `cq codex harness skill install`, scoped `bind`, `export`, `import`; [scope/sharing](usage.md#harness-capability-profiles). Plugin bundles can own MCP dependencies; use inventory IDs rather than guessing. |
| **"Try reducing request size where compression is compatible."** | Optional Headroom integration supports cache-oriented or token-oriented compression. It needs a separately compatible Python/Headroom installation; CQ does not install it. | [Existing compression integration](configuration.md#optional-request-compression). Advanced optional feature; verify compatibility/benefit rather than promising savings or fresh third-party qualification. |

For harness profiles, distinguish enforcement paths. Normal ChatGPT launching
can apply plugin exclusions after CQ observes a request; first-turn application
is not guaranteed, and standalone MCP/skill catalogues remain available. The CQ
launcher adds broader standalone MCP and fresh skill-catalogue controls. Core
tools/project instructions remain, historical context is not erased, and native
subagents inherit their parent's capability configuration. Do not promise an
independent model-specific first-turn policy for an internally spawned child.
Capability profiles select tools/instructions; routing pools select accounts.

### Feature catalogue: native ChatGPT and Dot collaboration

| User story to offer | Benefit and scope | Set up or inspect |
|---|---|---|
| **"Use Dots before regional rollout, without a system-wide VPN."** | US WireGuard plus CQ Network routes the supported native ChatGPT app. This is the regional-access enrolment, not account load balancing. | `cq codex network`; primary-Dot procedure below. Browser/unrelated apps keep their own route. |
| **"Keep my accounts' Dots together in one app."** | Adapter aggregates existing Dots and routes messaging, uploads, status and computer signalling by owner. Memories, connectors, plugins and permissions remain account-owned. | `cq codex dots install`, `list`, `start`, `status`, `sync`, `stop`; multi-account procedure below. Device access/local MCP configuration is not automatically shared. |
| **"Arrange Dots in my preferred order, even after a rename."** | Private user-defined order uses stable handles, including when names duplicate. Native dragging is not supported. | `cq codex dots order`; [ordering](dots.md#sidebar-order). Unlisted Dots follow the listed entries in their existing order. |
| **"Let Dots find and message each other when collaboration helps."** | One-time setup teaches an attributed relay and on-demand directory. Directory shares names/handles, not private histories, inferred roles or current work. Manual reminders are available. | `cq codex dots prime`; [setup/relay](dots.md#setup-and-manual-reminders). No CQ MCP/shared relay task is added; do not promise lossless delivery through long outages. |
| **"Send one announcement to all my Dots."** | Broadcast sends an ordinary attributed message per discovered Dot, with individual outcomes. | `cq codex dots broadcast`; [broadcast](dots.md#broadcast). Real send, not an installation health check. |
| **"Give my Dots one common local message board."** | Bind an existing local chat for explicit bounded reads and attributed posts, avoiding a new relay task per exchange. | `cq codex dots board`; [board](dots.md#shared-message-board). Binding sends notices; busy chats are not silently steered and history is not continuously mirrored. |
| **"Make ChatGPT's usage display reflect CQ's routed availability."** | Separate response adaptation changes displayed availability, not upstream balances or account-routing authority. | `cq codex network usage`; [usage adaptation](configuration.md#native-chatgpt-network-routing). Separate certificate-trust approval; regional routing alone does not need that trust. |

CQ does not copy connectors, merge memories or grant cross-account conversation
access. Native cross-account `@` selection and persistent secondary Reboot remain
unsupported. Check [current Dots limits](dots.md#current-limits) before promising
a particular local executor, paired-device or cloud-computer outcome.

### Feature catalogue: visibility, maintenance and recovery

Offer these when relevant. Do not turn a normal installation into a release
qualification exercise or present operator recovery as a default feature toggle.

| User story to offer | Benefit and scope | Set up or inspect |
|---|---|---|
| **"Understand which account handled a task and why it was slow."** | Metadata traces correlate routing, eligibility, waits, transport and outcomes. An explicitly configured Codex Stop hook can return a compact efficiency receipt. | `cq proxy trace`, `cq proxy hook codex-stop`; [diagnostics](troubleshooting.md#routing-diagnostics), [hook](usage.md#codex-stop-hook-and-efficiency-receipt). Raw payload capture is a separate sensitive opt-in. |
| **"Keep CQ running and upgrade without repeated manual repair."** | Package-owned proxy/refresh services, retained macOS runtime handoff and deferred upgrades preserve active work where supported. Inspect active/pending versions. | `cq service status`, package upgrade, coordinated `cq service restart`; [lifecycle](homebrew-upgrades.md). First adoption/removal can need maintenance; no zero-downtime promise. |
| **"Recover interrupted work without duplicating it."** | Inspect/reconcile exact durable operations, affinity or explicit rescue state. Rescue is not automatic failover and can suspend normal account-pool policy. | `cq operation`, lease/rescue commands; [operations](usage.md#durable-operations), [rescue](usage.md#rescue-mode). Follow the relevant authorised recovery path. |
| **"Validate a custom deployment or migrate a legacy endpoint safely."** | Installed-client checks, canaries, isolated candidates and endpoint transitions have dedicated evidence/approval requirements. | [Validation/operations](usage.md#codex-validation-and-operational-controls), [release procedures](releases.md). Not prerequisites for ordinary onboarding. |

### Turn discovery into an agreed setup

Present a short recommendation with benefits and expected changes. For example,
after detecting ChatGPT Desktop and two Codex accounts:

> I can connect your Codex chats to CQ so eligible work can use both accounts.
> You can also keep chosen chats in an account pool, leave quota for your phone,
> and preview or schedule banked resets. Separately, a US WireGuard profile can
> provide regional Dot access, and the multi-account adapter can put your
> already-onboarded Dots together. Which would you like configured now?

Tailor this to actual evidence; never claim clients/accounts were detected when
they were not. Reuse decisions already supplied by the user. Record chosen
clients/accounts, routing preferences, optional automation/capabilities and
restart constraints in the setup plan, with a check per selected outcome.
Defer unchosen features. Do not repeat approval questions for already-authorised
work; reset consumption, certificate trust and real sends must still be covered
by that authorisation.

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

### Connected desktop apps and CLI clients

For each chosen client, identify the API, effective configuration home, base
URL/provider selection and authentication source it actually uses. Follow
[client setup](usage.md#connecting-a-client), including the dedicated
[ChatGPT Desktop procedure](usage.md#chatgpt-desktop-integrations). A terminal
environment setting is not proof that a Dock-launched desktop app inherited it.
Keep native provider credentials separate from CQ's private local bearer.

For Codex account balancing, inspect configured and effective HTTP/WebSocket
routing modes and any inhibition reason. Do not merely change a JSON mode to
`enforce` and declare it active; follow its readiness requirements and preserve
continuity. Claude, Codex and Z.ai selection/fallback semantics differ.

Then configure only the chosen options from the catalogue: account preferences,
policy authority/pools/session bindings, reserve, models, harness profiles or
automation. Initial policy-authority activation, Codex pin/default changes and
some priming changes need a coordinated runtime restart; other commands apply
live. Read each procedure rather than restarting everything after every change.
Keep ordinary account routing separate from the two native Dot additions below.

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

For quota-only use, verify a fresh result from the configured provider. For
desktop or CLI routing, verify a real request from that actual client, not an
unrelated terminal probe. For native Dot use,
verify the app path and conversation, not just the proxy. Use existing user
authorisation for test messages or quota use; if it does not cover the proposed
test, obtain it first. Do not replay an uncertain send.
If a required observation is unavailable, report it as unverified rather than
inventing a command, inferring success or applying a speculative patch.

Verify selected optional features too: show pool membership and the exact chat
binding; inspect reserve status; preview a reset plan without consuming it;
check schedule enablement and live executor state; inspect a harness application
receipt rather than only its saved definition; confirm the intended client sees
a newly offered model. A schedule being saved is not proof it executed, and
ordinary routing does not prove either native Dot enrolment works.

## 6. Report completion or the exact remaining step

State the installed package/version, service owner, selected clients and
integrations, optional features enabled and verified outcomes. List declined
or deferred options briefly so they remain discoverable. Separate observed
failures from hypotheses. Include the
exact next action for a pending approval or client restart. For example:

> Setup incomplete: CQ is installed through Homebrew and its proxy works.
> Native ChatGPT routing is waiting for macOS approval. Approve CQ Network,
> then rerun `cq codex network enable` and `cq codex network status`.
> ChatGPT routing and Dot access are not yet verified.

Do not report "all working" from a healthy service alone. Never include account
tokens, private keys, authentication URLs or private conversation contents in
the completion report.
