# Configuring CQ

[Usage](usage.md) · [Troubleshooting](troubleshooting.md) · [Architecture](architecture.md)

Use the CLI for changes it supports. Before editing a file, save a private backup, preserve its generated token and unrelated fields, and follow the reload requirements below. Examples are fragments to merge into existing configuration, not replacement files.

## Proxy configuration

Config lives at `proxy.json` in the platform CQ configuration directory described below (`~/.config/cq` by default on macOS/Linux). First `cq proxy start` creates it with a random local token. Unknown fields are preserved across writes for version compatibility.

| JSON field | Default | Purpose |
|------------|---------|---------|
| `port` | `19280` | Loopback listen port. |
| `claude_upstream` | `https://api.anthropic.com` | Claude API upstream. |
| `codex_upstream` | `https://chatgpt.com/backend-api/codex` | ChatGPT OAuth-compatible Codex upstream. |
| `codex_egress_proxy` | unset | Codex upstream egress: ambient defaults, `direct`, or an HTTP/HTTPS/SOCKS5 proxy origin. Live reload. |
| `codex_wireguard_profile` | unset | Absolute private WireGuard profile path; mutually exclusive with `codex_egress_proxy`. Live reload. |
| `codex_egress_interface` | unset | Optional macOS network interface for WireGuard outer UDP sockets (for example `en0`). Fails closed if binding is unavailable. |
| `local_token` | generated | Local control/proxy bearer token. |
| `headroom` | `false` | Enable request headroom compression. |
| `headroom_mode` | omitted; effective `cache` | `cache` or `token`; setting this field also enables compression, even with `headroom:false`. |
| `pinned_claude_account` | unset | Claude email/account UUID pin. Prefer `cq proxy pin claude`. |
| `codex_turn_routing` | `off` | Codex HTTP routing mode: `off`, `observe`, or `enforce`. |
| `codex_ws_turn_routing` | `off` | Codex WebSocket routing mode: `off`, `observe`, or `enforce`. |
| `codex_routing_default_account_key` | unset | Default opaque Codex account key. |
| `codex_routing_pinned_account_key` | unset | Pinned opaque Codex account key. |
| `codex_routing_account_keys` | unset | Explicit eligible Codex account allowlist. |
| `codex_lease_retention_days` | `7` | Durable continuity retention, valid from 1 to 365 days. |
| `codex_continuity_state_dir` | cq config directory | Optional Codex lease/continuity state-root override. |
| `proxy_resilience_state_dir` | unset | Optional policy/runtime authority root; resilience controls stay inactive while unset. |
| `codex_window_priming` | `{"enabled":false}` | Object with `enabled` and optional `model_overrides`; see below. |
| `zai_window_priming` | `{"enabled":false}` | Object with `enabled`; use `cq zai prime` to change it. |
| `diagnostics_log` | unset | Causal routing trace JSONL path; restart required. |
| `payload_diagnostics_log` | unset | Raw request/response and WebSocket payload JSONL path; restart required. |

### Editing, validation and reload

CQ creates `proxy.json` with a generated `local_token`; preserve it. The following is a non-secret fragment, not a complete replacement file:

```json
{
  "codex_turn_routing": "observe",
  "codex_ws_turn_routing": "off",
  "codex_lease_retention_days": 7,
  "codex_window_priming": {
    "enabled": false,
    "model_overrides": {"5h": "MODEL_ID_FROM_REGISTRY"}
  },
  "zai_window_priming": {"enabled": false}
}
```

Remove the synthetic override or replace its model ID with an available compatible model before enabling priming. Override keys are quota-window scope strings, including scoped window names supplied by the provider; empty keys/IDs are rejected. `enabled` is Boolean. Omission keeps priming disabled. Use `cq proxy prime` / `cq zai prime` for normal enable/disable operations. Priming sends a minimal real request and uses quota; it never consumes a reset credit.

| Setting | Validation / application |
|---|---|
| `port` | Use a free TCP port from 1–65535 on loopback; restart and reconnect clients after a port change. `proxy start --port` overrides foreground listening. |
| `local_token` | Required, generated secret. Controls authenticated local operations and CQ compact-state readability. Do not rotate casually or put it in examples. |
| Upstream URLs | Use the intended provider origin/path and normal TLS verification. A different upstream does not grant compatible authentication or model access. Restart required. |
| `codex_turn_routing`, `codex_ws_turn_routing` | `off`, `observe`, `enforce`; restart required. Configured mode and effective readiness can differ. Enforced routing requires the installed-client readiness evidence described in [release operations](releases.md). |
| Codex pin/default/allowlist | Opaque AccountKeys; allowlists must contain unique nonempty keys and include an explicitly configured default. A pin/default never removes hard continuity requirements. Restart required. Prefer CLI selectors. |
| `pinned_claude_account` | Email/UUID selector; hot-reloaded. |
| Retention | Integer 1–365 days; omitted/zero resolves to 7. It does not make incompatible continuations portable. Restart required. |
| Continuity/resilience roots | Clean absolute non-root paths; private state authority. Preserve existing state. Restart after changing the configured root. Resilience controls are inactive while their root is unset. |
| Egress fields and WireGuard profile | Mutually exclusive egress-proxy/profile settings; live reload every five seconds. Interface binding requires a WireGuard profile and macOS support. See below. |
| `diagnostics_log`, `payload_diagnostics_log` | Optional output file paths; require restart. Payload capture is opt-in sensitive content. See [diagnostics](troubleshooting.md#diagnostics-and-privacy). |
| Headroom | `headroom` is Boolean; `headroom_mode` accepts only `cache` or `token`. A nonempty mode enables compression. Restart required. |
| Priming | Nested objects above; Codex changes require runtime restart. Z.ai automatic priming starts on the next runtime launch. Enabled Z.ai reset schedules are separate and pick up schedule edits live. |

`off` retains compatibility routing. `observe` records evaluation while retaining the compatibility path; `enforce` applies readiness-gated per-turn routing. Inspect `cq proxy status --json` for configured/effective modes and inhibition reasons. Changing a JSON word to `enforce` does not manufacture a validation receipt. Foreground startup and installed service lifecycle have different owners; use `cq service restart` only when a restart is intended, and allow existing work to reach a safe boundary.

### Optional request compression

Compression requires a compatible `python3` and `headroom-ai` installation in the interpreter CQ actually finds. CQ checks known Homebrew/local Python paths before `PATH` because background launch environments can be narrow. Install the optional Python package in an environment that this interpreter can use; CQ's installers do not manage Python packages. For the chosen interpreter, `python3 -m pip install 'headroom-ai[all]'` is the upstream-package installation pattern, subject to that Python environment's package-management rules. Do not alter a system-managed Python merely to silence an import error.

`token` compresses for token savings; `cache` preserves a frozen prefix for cache-aware handling. Responses conversion needs a package version exposing its Responses converter. Bridge startup checks imports/protocol readiness; unavailable compression produces an explicit diagnostic. Per-request compression errors preserve the original body with zero savings. Requests containing account-owned continuation state can be ineligible for compression. Verify the installed interpreter and CQ's log instead of assuming that a package installed in an unrelated virtual environment is visible to its service.

### Codex proxy egress

For example, `"codex_egress_proxy": "http://127.0.0.1:8080"` sends Codex
upstream HTTP (including streaming responses) and WebSocket connections through
that proxy. The setting also covers the proxy's Codex model registry requests,
window priming, CQ-owned managed credential refresh, and rescue relay. A failed proxy connection fails the request;
CQ never retries it directly. HTTPS proxy certificates and upstream TLS
certificates are verified normally. Proxy URLs accept only an origin (an
optional trailing slash is allowed), without credentials, query, or fragment.
Use an independently secured local forwarding endpoint for authenticated remote
egress rather than placing proxy credentials in CQ configuration. Both
`socks5://127.0.0.1:1080` and `socks5h://127.0.0.1:1080` use remote DNS and the
same context-aware dialer for HTTP and WebSockets. HTTP CONNECT also supports
WebSockets; SOCKS is an additional choice, not a requirement for WSS.

Omitting the field or setting it to `""` preserves existing environment proxy
behavior; `"direct"` explicitly ignores environment proxies for these Codex
requests. This does not bypass a system VPN. The setting does not change process
environment or Claude/Gemini egress. Standalone Codex quota checks borrow the
running runtime broker when either Codex egress setting is configured, including
`direct`; a missing broker fails that provider check without direct fallback. When this runtime
owns the credential coordinator, its eligible CQ-managed Codex refreshes use the
same configured egress; an already running external coordinator retains its own
transport and must be configured separately. Credential eligibility and
read-only treatment of system/external tokens are unchanged. It routes requests
received by CQ; other
Codex app connections and child processes are not captured by the HTTP proxy.
The optional Dots adapter shares this egress when a supported desktop app is
launched through **ChatGPT via CQ** or `cq codex dots start`. Its catalogue,
messaging, status, owner cloud sessions and computer-signalling connections use
the supervisor's existing private broker. A configured egress failure blocks
these requests instead of falling back to the host route. The adapter must be
running; a normal app launch does not acquire the multi-account catalogue.
Local IPC and unrelated desktop connections retain their existing route.

## Native ChatGPT network routing

Use this integration to access Dots when they have not yet rolled out in your
region, using a **US WireGuard profile**. It routes the normal signed ChatGPT
app without requiring the multi-account Dots adapter. Primary Dot onboarding
remains OpenAI's normal native or web flow. To combine already-onboarded Dots
from several accounts, add the separate [multi-account adapter](dots.md).

On macOS, the signed CQ Network companion can also route native ChatGPT traffic
through the configured WireGuard profile, including launches from the normal
ChatGPT Dock icon:

```sh
cq codex network enable
cq codex network status
cq codex network disable
```

Enable requires the signed companion, a running CQ service and a configured
WireGuard profile. macOS may require approval in System Settings. It does not
restart or modify ChatGPT, install a TLS certificate, or read Keychain items.
The extension verifies the source process's OpenAI signature and installed app
location. Public TCP and UDP flows use CQ's existing WireGuard runtime; other
apps and local destinations retain their original routes. Selected traffic
fails closed if the broker or tunnel is unavailable. Existing connections are
not moved; new connections acquire routing without restarting ChatGPT. System
DNS behaviour remains unchanged. This network routing does not itself enable
multi-account Dots; use the Dots launcher for that separate integration.
Local UDP forwarding requires numeric destinations and uses translated ephemeral
source ports; multicast discovery is not guaranteed.

CQ can also adapt the app's usage display on normal launches. It changes credit
availability in the usage HTTP response and Electron live updates. Provider
balances and account routing remain unchanged. Multi-account Dots use the
existing CQ launcher.

This option needs a dedicated local TLS certificate for `chatgpt.com` and
`ws.chatgpt.com`. The certificate has no CA capability and remains separate from
Dots certificates. Prepare it without changing traffic:

```sh
cq codex network usage prepare
```

Review the printed certificate fingerprint, hosts and expiry. After you approve
the certificate, add SSL trust to your login Keychain. Then enable response handling:

```sh
security add-trusted-cert -r trustRoot -p ssl \
  -k "$HOME/Library/Keychains/login.keychain-db" \
  "$HOME/.config/cq/state/native-usage/usage-cert.pem"
cq codex network usage enable
cq codex network usage status
```

If CQ uses another state directory, use the certificate path from `prepare`.
This adapter handles TCP HTTPS and WebSocket connections. HTTP/3 traffic remains
unchanged. Existing connections acquire response handling after they reconnect.
TLS service names identify usage connections when native flows provide numeric
destinations. The same handling applies to every account.

Disable stops response changes on existing connections too. Remove certificate
trust separately when you no longer need it:

```sh
cq codex network usage disable
security remove-trusted-cert "$HOME/.config/cq/state/native-usage/usage-cert.pem"
```

LAN DNS resolvers with globally allocated addresses stay on their active local
interface. CQ checks the interface subnet and binds the DNS socket's local
address; other DNS destinations retain WireGuard routing. DNS transport traces
record the route, numeric destination, transaction ID, header counts and bounded
error codes. They omit query names, records, payloads and credentials.

Egress configuration and profile contents are checked every five seconds. Valid
changes atomically replace the egress generation without restarting the service
or workers. Connections from the old generation close and clients reconnect;
CQ never runs old and new WireGuard identities concurrently. Invalid changes
block new and existing egress rather than continuing a stale or direct route.
Unchanged profile snapshots preserve existing connections.

For regional Dot access, use a US WireGuard profile and set
`codex_wireguard_profile` to an absolute path
such as `/absolute/private-directory/us.conf`, leaving `codex_egress_proxy`
unset. The profile must be an owner-only regular file (0600) in a private
owner-only directory (0700); CQ never copies its keys into JSON configuration.
The supported WireGuard profile format is `[Interface]` `PrivateKey`,
`Address`, `DNS` (IP literals), optional `MTU`, and `[Peer]` `PublicKey`,
`Endpoint`, `AllowedIPs`, optional `PresharedKey` and `PersistentKeepalive`.
Multiple peers use WireGuard longest-prefix routing. Peer keys must be unique,
and the same exact AllowedIPs prefix cannot belong to two peers; overlapping
broad/narrow prefixes are supported. Scripts/hooks, search-domain DNS entries,
other unsupported directives, and OpenVPN `.ovpn` profiles are rejected. OpenVPN
would require a separate native engine; this integration remains WireGuard-only.

CQ snapshots each profile generation and lazily starts the official
WireGuard userspace stack on the first selected request. HTTP/SSE, WebSocket,
registry and CQ-owned managed refresh traffic share that stack. It creates no
host TUN interface, route, DNS setting, privileged helper, or child process.
Destination DNS runs through the profile's DNS servers inside the tunnel, and
both DNS servers and destination IPs must be covered by `AllowedIPs`. DNS lookups
try profile servers in order, with at most five seconds per server inside the
overall connection budget. Addresses outside those routes fail; there is no
direct fallback. Inner connection/DNS setup is bounded to 30 seconds and honors cancellation; established streams
retain their existing request/session lifetimes. Shutdown closes the stack.
Profile and configuration edits use the live reload behavior above.

By default encrypted outer UDP packets follow host routing. On macOS,
`codex_egress_interface` explicitly binds and verifies IPv4/IPv6 sockets to the
selected network interface; loss or identity change fails closed. This uses
per-socket routing, without editing routes, DNS or firewall settings. Kernel
Network Extension policies and VPN kill switches can still block or mediate
traffic; binding is not a promise to bypass those controls. A hostname in a peer
`Endpoint` uses the system resolver for bootstrap; a literal endpoint IP avoids that lookup. This feature does not
bypass an active system VPN or configure a VPN account. A compatible Nord manual
WireGuard profile can supply the peer configuration; the Nord app is not needed
for CQ's userspace tunnel.

The supervisor owns one egress manager for both workers and rescue traffic.
Workers use a private authenticated Unix socket and receive no WireGuard keys.
Rescue continues through that same manager while workers drain or recover;
there is no second copy of the WireGuard identity. The broker restricts origin
connections to the configured Codex backend and the explicit OpenAI/Codex/Dots
service origins; it is not a general-purpose desktop proxy.
Foreground execution owns the same reloadable manager directly. No proxy
settings are exported to child processes. Do not concurrently use the same
WireGuard keys in another VPN client.

## Environment and files

### Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CQ_TTL` | `30` | Quota cache TTL in seconds; parsed integers clamp to 0–3600, invalid/overflow input uses 30. |
| `XDG_CONFIG_HOME` | `~/.config` on macOS/Linux | CQ config/state base on Unix. An absolute value is used; relative values fall back to the home default. Windows uses known folders. |
| `XDG_CACHE_HOME` | platform user-cache directory | Unix quota cache/history base. Relative values are ignored in favour of the normal cache fallback. Windows uses Local AppData. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Listing fallback and compatibility capability-cache path. Registry publication still uses `~/.claude/cache`; credential discovery remains separate. |
| `CODEX_HOME` | `~/.codex` | Native client/version discovery and read-only native model-cache fallback before CQ's own registry cache exists. CQ does not publish to the native entitlement cache; credential discovery still uses `~/.codex`. |
| `ANTHROPIC_BASE_URL` | unset | Client-side setting to point compatible clients at CQ. |
| `ZAI_API_KEY` | unset | Exclusive per-process override of all saved Z.ai accounts; do not put its value in shell history or logs. |
| `CODEX_THREAD_ID` | unset | Validated current chat identity for harness `--chat current`; CQ does not search transcripts to infer it. |
| `OPENAI_BASE_URL` | unset | Native-client routing input; the Dots launcher sets it to CQ. This is separate from the native network extension. |

### Important paths

The Unix paths below assume default roots. On Windows, resolve CQ configuration from the Windows Roaming AppData known folder, and state/cache/runtime/logs from the Local AppData known folder. These may be redirected by the host; do not assume a literal `C:\Users` directory. Provider-owned `~/.codex` and `~/.claude` paths are distinct from CQ roots.

| Root | macOS | Linux | Windows |
|---|---|---|---|
| Configuration | `~/.config/cq` or absolute `$XDG_CONFIG_HOME/cq` | Same | Roaming AppData `cq` |
| State / runtime | Configuration directory + `state` | Same | Local AppData `cq\state` / `cq\runtime` |
| Cache | `~/Library/Caches/cq` or absolute `$XDG_CACHE_HOME/cq` | User cache directory + `cq`, normally `~/.cache/cq` | Local AppData `cq\cache` |
| Logs | `~/Library/Logs/cq` | Configuration directory + `state/logs` | Local AppData `cq\logs` |

Some legacy lifecycle/continuity files intentionally remain in the CQ configuration root; changing a root is not a migration. Keep existing state and the proxy token when moving an installation.


| Path | Purpose |
|------|---------|
| `~/.config/cq/proxy.json` | Proxy configuration and local token. |
| `~/.config/cq/models.json` | User model overlays. |
| `~/.config/cq/state/` | Compatibility epoch, credential-control endpoint, and Codex removal journal. |
| `~/.config/cq/` | Default live-runtime lifecycle, canary, normal-caller admission, config, and overlay files. |
| Configured `codex_continuity_state_dir`, defaulting to `~/.config/cq/` | Durable Codex continuity and lease state. |
| Configured `proxy_resilience_state_dir` | Routing policy, dispatch-permit, and runtime-mode/rescue authority. No default is assumed; `cq proxy policy initialise --state-root DIR` configures it. |
| Command-supplied `--instance-state-root` | Isolated candidate lifecycle, validation, staged-release, and receipt state. |
| `$XDG_CACHE_HOME/cq/*.json` or platform cache equivalent | Provider quota cache. On macOS, default base is `~/Library/Caches/cq`. |
| `$XDG_CACHE_HOME/cq/burn_state_v2.json` or platform cache equivalent | Smoothed burn history. |
| `~/.claude/.credentials.json` | Claude account credentials. |
| `~/.claude.json` | Claude Code global config and managed picker entries. |
| `~/.codex/auth.json` | System Codex credential, read automatically but not rewritten by routing. |
| `~/.codex/accounts/` | CQ-managed Codex accounts and registry. |
| `~/.codex/models_cache.json` (or `$CODEX_HOME` equivalent) | Native client-owned entitlement cache; CQ reads it as a fallback before its own registry cache exists. |
| CQ cache root `model-registry.json` | CQ-owned native Codex/Z.ai listing/routing metadata; overlays remain in configuration `models.json`. |
| `~/.claude/cache/model-capabilities.json` | Registry pipeline's Claude capability publication; the compatibility startup helper can also write the path selected by `CLAUDE_CONFIG_DIR`. |
| `~/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist` | Background refresh agent on macOS. |
| `~/Library/Logs/cq/refresh.log` | Background refresh log. |
| `~/Library/Logs/cq/proxy.log` | Proxy service log. |

Secret/state writes use owner-only permissions and atomic replacement. Gemini, system Codex and ordinary external Codex credential sources remain read-only during automatic routing. The coordinator owner may renew explicitly declared CodexBar managed accounts through its owner lock and publish renewed credentials to that owner; see [credential ownership](usage.md#accounts-and-authentication).


## Routing policy documents

Use `cq proxy policy pool` and `session` commands for ordinary edits. `apply --file` accepts the name-based public `RoutingPolicyDocument` schema, not the internal MAC-bearing state objects. Export the current shape with `cq proxy policy status`; retain the returned generation values and apply changes with the intended runtime/offline authority. Stale or conflicting generations can be rejected. Do not edit immutable authority files or copy authentication material into a policy example.

This synthetic schema example demonstrates a pool without inventing live identities:

```json
{
  "schema_version": 1,
  "authority_generation": 1,
  "routing_generation": 1,
  "effective_generation": 1,
  "pools": [{"name": "Research", "value": 10, "members": ["ACCOUNT_KEY"]}],
  "session_bindings": [],
  "capability_evidence": [{"account_key": "ACCOUNT_KEY", "state": "supported"}]
}
```

Replace `ACCOUNT_KEY` with an actual opaque key, use the generations for the state you are administering and supply only capability evidence you can establish. This is a format illustration, not a recommendation to assert a capability. `session_bindings` entries contain `session_digest` and `pool`; obtain the full keyed digest through `session digest`, not a plain SHA hash of an ID. Pool `value` is an unsigned 32-bit integer. Pool names must be nonblank valid UTF-8 without control characters. They preserve display casing and resolve case-insensitively; names must be unique. Renames preserve internal identity and bindings.

Advanced document fields are `capability_pool`, `capability_predicates`, `capability_routing_evidence` and `delegations`. A delegation contains `caller`, an array of account keys and optional `expires_at`. Capability predicates/evidence bind exact model, transport and client authority; do not fill them with guessed values. Use the current [source document types and validation](../internal/proxy/routing_policy_store.go) and [capability-routing types and validation](../internal/proxy/capability_routing.go) for their closed schemas, authority generations, expiry and migration rules. `apply` is an operator operation; the small pool/session CLI avoids hand-authoring those claims.

## Harness profile files

Prefer `profile save`, `copy`, `import`, `bind` and `unbind`. A raw profile definition contains optional `description` plus required `changes`. Changes support `reset` (Boolean), `default` and the `plugins`, `mcp`, `skills` decision maps. Decisions are `enabled`, `disabled`, `inherit`; a map key `*` supplies its kind's default. Use inventory IDs, not installation paths. `reset:true` clears previously accumulated changes before this layer. Empty/invalid names and unsupported decisions are rejected.

A portable export envelope is `{"version":1,"name":"review","definition":{"description":"Review","changes":{"default":"disabled"}}}`. `profile save --file` accepts the inner definition or the envelope and uses the explicit save name; `import --file` takes the envelope and uses its name unless overridden. Neither binds it. User policy is private `harness.json`, with the layered schema at version 2 (`enabled`, `profiles`, `global`, `workspaces`, `tasks`); legacy version 1 remains readable. Project `.cq/harness.json` contains `version:1`, optional `profiles` and `rules`. Scope rules have `all` and `models`; each binding contains ordered `profiles` and optional inline `changes`. The CLI preserves migration/locking rules, so avoid rewriting policy manually. See [usage](usage.md#harness-capability-profiles) for precedence and native application limits.

## Other CQ files

| File | User contract |
|---|---|
| Configuration `zai.json` | Private API keys and browser-account references. Use login/switch/remove/route; do not copy raw OAuth credentials into it. `ZAI_API_KEY` exclusively overrides saved accounts for that process. |
| Encrypted ZCode session store and device identity | Browser-login material and stable device registration. Keep its private key material with the configuration during an authorised migration. PATs/signing material remain in memory. |
| Configuration `models.json` | User overlays managed by `models overlay`; provider IDs are `anthropic`, `codex`, `zai`, distinct from quota ID `claude`. |
| Codex managed account registry / credential journal | Coordinator-owned account identity and refresh/removal records. Do not edit to bypass uncertain outcomes or change ownership. |
| Reset schedule/journal state | Exact accepted credits, confirmations and durable uncertain outcomes. Use schedule/list/status commands, not manual file changes. |
| Dots private installation | Defaults to `~/.local/share/cq/dots`, outside source checkouts. Use `dots --root` for a deliberate separate installation; see [Dots](dots.md). |
| Native usage certificate/settings | Under CQ state `native-usage`; `prepare` prints the authoritative public certificate path. Certificate preparation, Keychain trust and enablement are separate. |

Unix secret files use mode `0600` and private directories `0700`; WireGuard and Dots also reject unsafe ownership, non-regular/symlinked paths where required. Windows roots use known folders and platform access controls. User data survives package uninstall. Back up private state securely; it can include secrets even when a public status response is redacted.

## Model overlay format

The overlay file is a versioned object: `{"version":1,"models":[]}`. Use `models overlay add --provider PROVIDER --id MODEL [--clone-from NATIVE_MODEL]` to create it and `remove` / `prune` to maintain it. Supported providers are `anthropic`, `codex` and `zai`.

Each model entry requires `provider` and `id`. Optional public fields are `aliases` (string array), `display_name`, `description`, `context_window`, `max_context_window`, `max_output_tokens`, `visibility`, `priority`, `source`, `clone_from`, `inferred_from` and `raw` provider JSON. `source` is `native` or `overlay`; an omitted source in an overlay file becomes `overlay`. Preserve downloaded `raw` metadata rather than inventing capabilities. Cross-provider duplicate model IDs, compared case-insensitively, are rejected. Cloning copies metadata for an upstream-supported alias; it does not establish provider availability, pricing or entitlement. The CLI and registry infer provider-specific metadata where supported, so prefer those commands to hand-editing a client cache.
