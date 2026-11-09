# Architecture and ownership

CQ is a Go CLI with quota reporting, account management, a local API proxy and
platform services. The native macOS companion is a separate Swift/Xcode product.
User commands and configuration are documented in the [README](../README.md).
Development checks are in [CONTRIBUTING](../CONTRIBUTING.md); delivery gates are in
[the release guide](releases.md).

## Package map

| Owner | Responsibility |
| --- | --- |
| `cmd/cq` | Kong command tree, explicit command dispatch, dependency wiring, platform service adapters and installed validation |
| `cmd/cq-install`, `internal/installer`, `internal/installstate` | Official archive installation, verified downloads, ownership, transactional replacement and rollback |
| `internal/app` | Concurrent provider fetch, reports, account operations and reset orchestration |
| `internal/provider/{claude,codex,gemini,zai,grok}` | Provider-specific account, authentication and quota contracts |
| `internal/auth`, `internal/keyring` | Shared browser/OAuth helpers and legacy Claude credential backends |
| `internal/quota`, `internal/history`, `internal/aggregate` | Domain windows, observed burn estimates, aggregate pace/gauges and reset simulation |
| `internal/output` | TTY model/renderer and JSON output |
| `internal/proxy` | Supervisor/worker runtime, routing/continuity, policy, private control, transport and validation evidence |
| `internal/modelregistry` | Native catalogues, account-scoped discovery/cache, overlays and client projections |
| `internal/dots` | Optional multi-account native Dots adapter, discovery, relay and launcher |
| `internal/harness` | Scoped capability policy, full inventory, native controller, application receipts and embedded curation skill |
| `internal/fsutil`, `internal/userdirs`, `internal/httputil`, `internal/cache` | Secure/testable filesystem operations, platform roots, bounded HTTP and quota cache |
| `internal/tools`, `.github/scripts`, `scripts` | Release, native package, installed-client and retained-authority verification |
| `native/macos-network` | Signed host/system extension, source admission and authenticated TCP/UDP transport |

## Quota reporting

The reporting path has two phases: fetch provider results, then build/render a
report. `Runner.BuildReport` starts selected providers concurrently and recovers
panics at the external-call boundary. Claude, Codex, Z.ai and Grok support multiple
accounts; Gemini uses one Antigravity account. Z.ai has account-specific quota
surfaces, while Grok reports included subscription quota over its actual period.
Each provider returns classified
`quota.Result` errors rather than exposing credential material or raw responses.
Independent account/profile/usage work can run concurrently, with recovery in
each goroutine that calls external or injected code.

The cache is optional: failures degrade reporting without dereferencing a nil
cache. `internal/history` supplies burn estimates; `internal/aggregate` computes
weighted coverage, pace and burndown for multiple usable accounts. The TTY gauge
buckets the demand/supply ratio into overburn, on-pace and underburn severity.
Accompanying fields show coverage gaps and projected waste; an imminent-gap
warning preserves the natural position. The historical sustainability value
remains in JSON for compatibility. Window names
include duration-backed and bucket-scoped windows as well as daily model pools;
do not assume every provider has only `5h` and `7d`.

TTY account rows share the active/value/tier/reset hierarchy across all five
providers; the renderer sorts a private view without changing report/JSON or
routing order. Provider-specific capacity rules remain separate: unknown Grok
weights do not become guessed aggregate capacity. See [report ordering](usage.md#tty-report).

## Credential ownership

Credential discovery, ownership and request routing are separate authorities.
Automatic routing must not activate an account or rewrite `~/.codex/auth.json`.
CQ-owned managed Codex lineages renew through the credential coordinator. An
explicitly declared CodexBar managed source has a narrow coordinator renewal
capability with owner locking and exact identity/revision fences. System,
borrowed, exported and uncertain credentials remain read-only outside that
explicit owner capability. Ambiguous rotating exchanges are not replayed. See
[Codex package rules](../internal/provider/codex/AGENTS.md).

Gemini reads Antigravity's Keychain entry and project cache independently and
concurrently. It keeps refreshed OAuth tokens in memory, resolves a missing
project through `loadCodeAssist`, then reads quota through bounded HTTP. It never
writes Antigravity-owned state or invokes `agy`. Its OAuth client secret is a
release-time linker input, with no runtime environment, binary-scraping or CLI
fallback. See [Gemini package rules](../internal/provider/gemini/AGENTS.md).

The legacy Claude `internal/keyring` macOS backend invokes `security`; its
`UpdateKeychainEntry` currently passes serialised credential JSON as the `-w`
argument, not stdin. That is a credential exposure surface: never print command
arguments or credentials in diagnostics. This description does not grant the
proxy worker permission to launch ambient helpers. Preserve its independent
credential-broker and execution restrictions.

Private authority uses the filesystem capabilities required by its owner:
atomic writes, durable sync, exclusive creation/locks, retained directory
identity and no-follow reads where applicable. A legacy file-read trade-off
does not weaken coordinator, routing-policy or installation authority fences.

## Proxy runtime and policy

An owned runtime separates the ingress supervisor from a normal worker. The
supervisor owns the listener, caller authentication, private control and runtime
identity; the worker owns normal provider routing and its replaceable state.
Inspection reconciles desired configuration, service-manager facts, kernel
process/listener evidence and runtime acknowledgements. `/health` alone proves
neither exact ownership nor working provider traffic.

Rescue is an explicit Codex relay mode with caller-supplied authentication. It
cannot enforce normal account-pool policy; entry with bindings requires the
explicit policy suspension supported by the CLI. Normal request failures do not
automatically grant rescue authority. Preserve request commitment, exact-account
continuity and fail-closed policy boundaries across HTTP, SSE and WebSocket.
Ambiguous/admitted attempts cannot be replayed. A complete pre-admission hard
quota rejection is distinct from a transient, transport or policy error.

Routing pools narrow the globally eligible account set. Internal schema v2 uses
stable UUID `PoolID` values; public `RoutingPolicyDocument` schema v1 exposes
names. Names retain casing, reject control characters and collide under Unicode
case folding. Rename preserves pool identity and session bindings. Version-1
stored policy migrates through the store; do not hand-edit authenticated files or
confuse the public document version with the stored authority version.

Pool value is a non-negative relative capacity preference. The common planner
preserves the system account for clients outside CQ before comparing pool value,
then prefers lower-value viable capacity. Hard continuation affinity, session
bindings, eligibility and dispatch commitment remain constraints. Higher value does not override a
continuity or capability restriction. Public/TTY pool labels use names, not UUIDs
or an added `Proxy` prefix; report labels share a terminal-cell-width alignment.
The store and planner tests are authority for migration and ordering details.

Current [routing-policy types and validation](../internal/proxy/routing_policy_store.go),
[capability routing](../internal/proxy/capability_routing.go) and their tests define
the implemented contracts. Historical planning and review documents are not
runtime or clean-checkout verification dependencies.

## Banked resets

`internal/provider/codex` owns `/wham` transport, exact account/credential
resolution and the durable attempt journal. `internal/history` handles
unchanged-epoch upward corrections; `internal/aggregate` performs percentage-only
portfolio simulation; `internal/app` checks completeness and orchestrates
list/recommend/use. Recommendations consider the shared account snapshot,
remaining quota, burn, natural resets, credit expiry and capacity multipliers.
Missing portfolio inputs prevent a complete actionable recommendation.

Plain `recommend` is advisory. Manual `use` and explicitly accepted schedules
are separate write authorities. New schedules default to fixed dates; adaptive
timing is explicit. The scheduler executes accepted enabled entries, rather than
consuming credits merely because a recommendation exists. Commands that accept
or enable consumption require their documented confirmation or explicit `--yes`.

Consumption selects an exact account and credit, durably records a deterministic
idempotency key before POST and reuses unresolved attempts after uncertainty.
Omitting a credit first considers unresolved attempts, then eligible expiry
order. A banked reset restores percentages without moving natural `5h`/`7d`
reset epochs. Consumption never activates the target account. Managed credential
refresh remains with its eligible coordinator owner, rather than becoming an
unrestricted refresh path. Keep tokens, credential revisions and journal internals
out of public results and diagnostic filenames.

## Platform installation and runtime

Complete installations manage a proxy plus periodic credential refresh in the
logged-in user's security context. Package hooks delegate to the common
`cq service` lifecycle; they do not independently reproduce its semantics.
Installation, upgrade, rollback and uninstall preserve ownership. Uninstall
removes package-owned services/executables and retains user data.

| Platform | Complete install/lifecycle owners | Platform proof |
| --- | --- | --- |
| macOS | Homebrew Cask; proxy and refresh LaunchAgents; separately pinned native companion on macOS 13+ | Cask install/upgrade/uninstall, launchd identity, installed Codex traffic and explicit native activation/admission |
| Linux | Go installer runner and systemd user proxy/refresh service plus timer | Exact executable/process/listener inspection, systemd lifecycle and confined installed HTTP/SSE/WebSocket validation |
| Windows | Per-user MSI/WinGet or Go installer runner; Task Scheduler tasks | Native scheduler ownership, MSI/runner install/upgrade/uninstall and deployed package checks |

Linux packaging owns `cq-proxy.service`, `cq-refresh.service` and
`cq-refresh.timer`. Their fixed command forms are `<absolute-cq> proxy start`
and `<absolute-cq> refresh`. Runtime inspection consumes service-manager facts
and independently checks procfs/kernel identity; it does not infer ownership from
process names or mutate units. Installed acceptance confines the client with
user/network namespaces, descriptor-only relays, `no_new_privs`, seccomp and
Landlock. Tests require those gates to pass on supported CI hosts; an unavailable
confinement primitive is not successful qualification. User services require a
working user manager; installation does not silently enable lingering.

macOS native routing validates OS audit tokens, signing identity and executable
location before bridging selected flows. Selected failures block rather than
fall back directly. The Go process owns egress configuration; the extension uses
authenticated transport without reading user credentials or WireGuard profiles.
Build, signing, activation and real traffic are distinct evidence. See
[the native README](../native/macos-network/README.md).

## Harness profiles

Version 2 folds rules from broad to narrow: global, ancestor workspaces, then
chat. Within each scope, all-model rules apply before matching model families
from broad to narrow and finally exact-model rules. Portable workspace files are
`.cq/harness.json`; global definitions, private workspace bindings and chat IDs
stay in CQ's private `harness.json`. At the same workspace, its portable file
applies after the private workspace rule. Version 1 retains its GLM-only legacy
semantics until explicit promotion; version 2 has no implicit GLM restriction.

Named profiles contain deltas for plugins, MCP servers and skills. `inherit`
preserves the native baseline; `reset` clears accumulated CQ decisions back to
that baseline, not unconditional enablement. `full` resets to native behaviour;
`minimal` supplies a disabled default. Narrow rules can restore selected
capabilities. Exported definitions contain stable capability IDs and portable
rules, not private chat bindings or versioned installation paths.

Plugins are controlled as bundles, including their declared MCP servers and
connected apps. The inventory exposes server names and stable app IDs on the
owning plugin; standalone MCP rows remain independent controls. Connected apps
use a separate native feature gate and app table. Restrictive profiles disable
unselected apps while retaining apps owned by selected, eligible plugins. Shared
ownership uses the union of selected owners. Existing disabled app settings,
approvals and tool settings remain authoritative and are restored with the
original app table when the binding is removed. Native plugin detail metadata
resolves a skill's
MCP dependency to the owning plugin when unambiguous. A missing or ambiguous
owner remains an explicit dependency issue rather than a guessed relationship.

Cached plugin skills can remain enabled after their marketplace registration is
removed. When native skills identify an owner missing from the catalogue, CQ
checks the exact plugin ID and cache source, then asks the native SDK to inspect
that bundle through a private temporary local catalogue. Native parsing supplies
MCP and app dependencies; CQ does not match display names or substitute another
marketplace's same-name plugin. The original project's disabled plugin settings
remain authoritative. User marketplace configuration and cached bundles stay
unchanged, and temporary catalogues are removed after the native child exits.
Ambiguous identities, missing directory-link support or failed lookups retain
explicit unresolved ownership instead of inventing a relationship.

`profile save NAME --file FILE` accepts both a bare definition and the versioned
document emitted by `profile show --json` or `export`. The explicit save name
takes precedence over the document name. `import` uses the document name unless
overridden. Input directories may use aliases such as macOS `/tmp`; final file
symlinks remain rejected. Saving definitions does not bind or activate them.

The complete native metadata inventory includes capabilities excluded from the
current reduced context. Application receipts record effective fingerprints and
supported/unsupported/pending outcomes; a saved policy alone is not application
proof. `explain` reports the current receipt's application mode separately from
its state; a `partial` full-bridge receipt still identifies full controls, with
its remaining issues listed. Stock Desktop controls plugins only and cannot
guarantee first-turn application. Launcher controls can cover standalone MCP
servers and skills. Full configuration changes made after an empty chat starts
wait until its first turn completes: native Codex has no durable history to
reload before then. Plugin exclusions still apply immediately through native
settings; standalone MCP and skill configuration remains pending. Ephemeral
chats retain that initial configuration because they cannot be reloaded from
disk. Another native subscriber can retain an existing chat's configuration
even when unsubscribe and resume both succeed. The macOS launcher gives its
stdio broker a private gateway listener. The existing CQ gateway forwards each
qualified native process's relay to that same broker, preserving authentication,
enrolment identity and public message sequences. At an idle boundary, CQ releases
all known subscriptions for only the selected chat, reloads its configuration,
then restores the other notification subscriptions without replaying history.
Automatic goal continuations can keep a chat active across turn boundaries;
pausing the goal lets a queued configuration reload reach an idle boundary.
Observed resume requests remain candidates when large history responses are
chunked; native unsubscribe results establish which candidates were subscribed.
Other chats and spawned children retain their subscriptions. Gateway reconnects
preserve logical ownership; replayed traffic cannot repeat preparation or clear
a newer active turn. Completion or idle notices from a secondary connection
request fresh native status without clearing the activity guard directly. A new
turn also refreshes cached activity when application is pending and no submitted
turn or settings request remains outstanding. Pending
application events report activity, outstanding request counts and rollout guards
without recording message content. Failed native application receipts and events
record the failed stage, classified reason and numeric native RPC code where
available. Rollback outcome and any rollback failure remain separate from the
original failure. Native error messages, error data, request configuration and
environment values are never included. Successful or pending application clears
the previous failure detail. CQ checks the chat's effective plugin and
Apps features and standalone MCP tools, resources and templates after reload.
MCP verification reads the relevant servers through their existing selected-chat
connections; it does not start unrelated discovery across the whole catalogue.
Retained or unverified controls produce a `partial` receipt and leave the chat
usable. Granular app selection remains unverified without a witnessed
configuration reassembly. Skills have no independent per-chat readback; changed
skill settings
remain unverified unless an observed runtime transition confirms configuration
reassembly. Removing a binding does not turn an unverified restoration into a
successful one. CQ preserves the original baseline across partial plugin-only
handoffs. Internal child creation has no independent
model-specific first-turn hook. Model/workspace changes restore the captured native baseline
and recalculate policy while retaining chat history and user edits. Existing
catalogue text already in history is not removed. The embedded
[curation skill](../internal/harness/curate-harness/SKILL.md) describes the CLI
workflow and supported fallbacks.

## Grok and shared provider mechanics

`internal/credentials.Rotate` owns the transaction sequence extracted from
Codex's rotating refresh implementation. Codex retains its existing journal,
record, single-flight and ownership adapters. Grok supplies a separate encrypted
store and account identity adapter using the same transaction sequence and
existing secure filesystem locks. A retained durable response is committed
without repeating its exchange; an attempted exchange without a durable result
is indeterminate. Fresh login generations supersede old retained operations.

`internal/provider/codexbar` reads provider-scoped external configuration. Grok
verifies borrowed bearer identities before exposing usable accounts and deduplicates
by user, principal type, principal and team rather than a stored account ID. Z.ai
browser login verifies exact subscription keys through authenticated personal-project
reads before replacing a CQ key with OAuth. Reporting refresh and account listing
also backfill opaque key aliases for existing browser logins without replacing
credentials or changing routes and logical preference. Request-time Z.ai discovery
stays local. Unresolved external sources contribute no accounts or quota capacity;
matching labels, email, key prefixes or quota values never prove ownership.
External sources remain unchanged and never gain CQ renewal ownership.

Grok quota/model providers join the existing report and catalogue pipelines.
Registry sources distinguish no configured accounts from successful empty model
lists, so removing Grok access clears its entries without hiding other providers'
typed errors. Display catalogue unions never authorize another account's model.

The Grok proxy adapter reuses the WebSocket-to-HTTP bridge extracted from Z.ai and
shared Responses SSE handling. Both original and new callers retain regression
coverage. Grok-specific code owns subscription headers, schema/tool projection,
model/quota admission and native state binding. There is no new background
service or native macOS component.

Verified native reasoning also retains its original container under a bounded
byte budget, since Codex reserializes optional fields and private bookkeeping.
Restoration checks account/model ownership and rejects substantive edits before
dispatch. Expanded request bytes are bounded before serialization. Grok
compaction uses the Responses summary preparation and CQ sealing extracted from
Z.ai, because the subscription endpoint returns 404 for standalone native
compaction. CQ-owned summaries are restored before native-state admission.
