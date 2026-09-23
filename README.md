# cq

`cq` is a quota dashboard, account manager, model-registry publisher, and local Claude/Codex API router. It checks **Claude**, **Codex**, and **Gemini** quota in parallel, presents per-account and aggregate burn information, and can keep local AI clients on healthy accounts without exposing provider credentials.

![cq output](assets/screenshot.png)

## Install

Complete installers place an official release binary, install and start the
current-user proxy, and schedule periodic credential refresh. No manual
post-install command is required.

### macOS — Homebrew Cask

```bash
brew install --cask jacobcxdev/tap/cq
```

Existing formula installations must migrate once so the legacy Homebrew service
cannot conflict with Cask-owned services:

```bash
brew services stop cq
brew uninstall --formula cq
brew install --cask jacobcxdev/tap/cq
```

Homebrew versions that sandbox Cask flight hooks cannot run the service hooks
stored by CQ 0.32.8. For that legacy installation, first verify that
`$(brew --prefix)/bin/cq` is a symlink into the installed CQ Caskroom and that
`cq service status --json` reports the `homebrew` owner and that stable executable.
Keep a copy of the executable for rollback. Run `cq service uninstall
--owner=homebrew --service-executable="$(brew --prefix)/bin/cq"` outside Homebrew,
then remove only that verified symlink before running `brew upgrade --cask cq`.
This makes the old hook skip its executable call; the new installer recreates
the link and services. Keep existing CQ configuration. If the upgrade fails,
restore the stable link to the saved executable and reinstall its Homebrew-owned
services before retrying.

### Windows — WinGet

```powershell
winget install jacobcxdev.cq
```

WinGet installs CQ, its uninstaller, PATH and Add/Remove Programs metadata, and
both scheduled tasks for the current Windows user without administrator access.

### macOS, Windows, and Linux — Go installer runner

```bash
go run github.com/jacobcxdev/cq/cmd/cq-install@latest
```

The runner resolves its tagged release, downloads and verifies the matching
official CQ asset, then installs the same current-user services as each native
package. Linux requires a functional systemd user manager. Running services
after logout can also require systemd user lingering configured by the host.

Run the same install command to upgrade or repair an installation. Native
package-manager upgrades are also supported:

```bash
brew upgrade --cask cq
winget upgrade jacobcxdev.cq
```

Uninstall through the method that owns the installation:

```bash
brew uninstall --cask cq
winget uninstall jacobcxdev.cq
go run github.com/jacobcxdev/cq/cmd/cq-install@latest uninstall
```

Uninstall removes package-owned executables and services. User configuration,
credentials, cache, history, and logs remain.

## Development and portable binaries

Plain `go install` builds CQ from source for development:

```bash
go install github.com/jacobcxdev/cq/cmd/cq@latest
```

This binary lacks official release provenance and release-time Gemini refresh
material. It does not install or manage services. Direct release archives
contain official binaries, but extraction also does not install or manage
services. Use a complete installer when proxy and refresh lifecycle are wanted.

`headroom-ai` remains a separate, optional integration. CQ installers do not
install Python or optional integrations.

## Quick start

```bash
cq                                # Check Claude, Codex, and Gemini
cq check claude codex             # Check selected providers
cq check gemini                   # Check Gemini through Antigravity HTTP APIs
cq check --json                   # Machine-readable report
cq check --fresh                  # Bypass quota cache
cq version
```

`check` accepts `claude`, `codex`, and `gemini`. Provider fetches run concurrently. Multi-account work also runs concurrently within each provider.

Use context-sensitive help for exact flags and safety confirmations:

```bash
cq --help
cq check --help
cq claude --help
cq codex --help
cq gemini --help
cq proxy --help
cq models --help
cq operation --help
```

## CLI 1.0.0 migration

The public CLI uses one canonical grammar and JSON envelope schema 2. Supported deprecated spellings remain until 2.0.0 and warn on stderr; they do not retain old JSON or exit-code contracts. Help and version are pure, and invalid input is rejected before IO.

| Previous spelling | Canonical spelling |
| --- | --- |
| `cq codex accounts` | `cq codex account list` |
| `cq claude switch EMAIL` | `cq claude account activate EMAIL` |
| `cq proxy reserve status` | `cq codex proxy reserve status` |
| `cq proxy policy status` | `cq codex proxy policy show` |
| `cq proxy default codex ACCOUNT` | `cq codex proxy fallback set ACCOUNT` |
| `cq agent install` | `cq service install --component token-refresh` |
| `cq proxy restart` | `cq service restart --component proxy` |
| `cq refresh` | `cq auth refresh` |

Frozen package hooks and installed launch arguments remain unchanged. On Windows, the snapshot-based Go installer requires `cq service stop --component proxy` or `cq service stop --component token-refresh` before upgrade/uninstall if an auxiliary exists or a disabled primary is still running. Direct WiX/legacy service uninstall uses its owned cleanup path instead of installer snapshots.

Install shell completions from the candidate binary:

```sh
cq completion bash > ~/.local/share/bash-completion/completions/cq
cq completion zsh > ~/.zfunc/_cq
cq completion fish > ~/.config/fish/completions/cq.fish
```

Create the destination directory first. Bash must load its completion directory; add `~/.zfunc` to Zsh `fpath` before `compinit`. Completion never invokes credentials, network or services.

## Capability map

| Area | Capabilities |
|------|--------------|
| Quota | Parallel Claude, Codex, and Gemini checks; provider selection; cache bypass; stale-account backfill; TTY and JSON output. |
| Analysis | Remaining quota, reset times, pace, smoothed burn rate, projected burndown, correction-deadline gauge, multi-account aggregate, provider availability. |
| Accounts | Claude and Codex OAuth login, listing, activation, switching, removal, and token refresh; read-only Gemini account inspection. |
| Background work | One-shot OAuth refresh and per-user periodic refresh on macOS, Windows, and Linux. |
| Proxy | Loopback Claude/Codex routing, Anthropic Messages compatibility, native Codex Responses HTTP/WebSocket routing, model discovery, local authentication, health/status inspection. |
| Routing controls | Claude/Codex pins, Codex default, account allowlist, quota/fairness selection, durable continuity, quota-window priming, capability pools, session bindings, rescue mode. |
| Codex assurance | Installed HTTP/WebSocket validation, routing canary, candidate runtime lifecycle, client-bearer barrier, release receipts, durable operation recovery, legacy endpoint transition. |
| Efficiency feedback | Privacy-safe Codex request-shape telemetry, turn receipts, observational no-affinity comparison, Codex Stop-hook output. |
| Model registry | Anthropic/Codex source merge, local listing, refresh, overlays, Codex cache publication, Claude Code capability and picker publication. |
| Diagnostics | Strict proxy snapshot, routing metadata JSONL, opt-in raw payload JSONL, health and runtime evidence, safe error classification. |

## Quota checks and output

### Providers

- **Claude** discovers stored accounts, fetches profile and usage data in parallel, refreshes eligible tokens, and reports each account.
- **Codex** discovers system, CQ-managed, and declared read-only external accounts. Ordinary checks do not activate, remove, refresh, or rewrite system credentials. Eligible CQ-owned credentials refresh only through CQ's coordinator.
- **Gemini** reads Antigravity Keychain credential and cached project ID concurrently, calls Antigravity OAuth and quota HTTP APIs directly, and never invokes `agy`. Credential/project stores remain read-only; refreshed tokens live only in process memory.

### Cache and history

Successful quota rows are cached by provider. Default TTL is 30 seconds; `--fresh` bypasses it. If one account has a transient fetch failure and a matching usable cached row exists, cq shows stale quota with original error context instead of hiding that account. Auth errors are never written as fresh cache data.

cq also keeps per-account/window burn history. Exhaustion ETA uses the **shorter of the recent-rate and whole-window-average estimates**: fast burns shorten it, while idle periods cannot extend it beyond the whole-window baseline. Recent rates use a 30-minute EWMA half-life, at least five minutes between samples, and a 15-minute observation warmup. Exact percentages are used when available. Resets, upward adjustments, precision changes, and observation gaps over two hours restart warmup; stale or mismatched snapshots fall back to the whole-window average. Matching cached reads can reuse a forecast for up to 15 minutes without advancing history.

Account, aggregate, and proxy-pool ETAs select the same rate per account before weighting capacity and consumption. Budget pace and the main gauge retain their existing window-based calculations; the secondary imminent-block warning retains its separate EWMA. Cache and history failures degrade to uncached/cold-start behaviour.

### TTY report

For each provider/account, cq can show:

- plan/account identity and active credential marker;
- remaining percentage and reset time for each quota window;
- usage pace and smoothed burn rate;
- projected burndown;
- aggregate coverage across two or more usable accounts;
- correction-deadline gauge: overburn deadline on left, on-pace centre, projected waste on right;
- Codex proxy-eligible subset when discovery and routing eligibility differ;
- cached/stale and error context.

TTY icons require a [Nerd Font](https://www.nerdfonts.com/). Recommended: [`jacobcxdev/tap/liga-sf-mono-nerd-font`](https://github.com/jacobcxdev/homebrew-tap).

### JSON report

```bash
cq check --json
cq codex account list --json
cq service status --json
```

CLI 1.0.0 emits schema 2 envelopes with `schema_version`, canonical `command`, `ok`, `data`, `errors` and `warnings`. Inspect `ok` and the process exit code before reading `data`; error messages do not expose credentials. Service components are in `data.components`, keyed by `id` (`proxy` or `token-refresh`). The trace stream and Codex Stop hook have their documented output exceptions. Public spelling aliases return the same schema 2 contract.

## Accounts and authentication

### Claude

```bash
cq claude account login --activate
cq claude account list
cq claude account activate EMAIL
cq claude account remove EMAIL
```

Claude login uses browser OAuth. Stored account credentials support multi-account checks and proxy routing.

### Codex

```bash
cq codex account login --activate
cq codex account list
cq codex account activate EMAIL
cq codex account remove EMAIL
cq codex reset list
cq codex reset recommend
cq codex reset use EMAIL
cq codex reset use EMAIL --credit CREDIT_ID --yes
```

CQ-owned Codex accounts live under `~/.codex/accounts/` with registry metadata. System `~/.codex/auth.json` remains distinct. Automatic quota/routing reads never switch the system account.

`cq codex reset recommend` plans across every account shown by `cq codex account list`. It fetches fresh usage and banked-reset inventories, reports a non-actionable incomplete schedule when any portfolio input is missing, and never consumes a credit. Banked resets restore shared-window percentages without changing natural 5-hour or 7-day reset dates.

`cq codex reset use EMAIL` previews selected credit, current shared usage, and current recommendation, then asks for confirmation with default No. Omit `--credit` to resume a pending attempt or select eligible credit with next expiry. Supply `--yes` only when explicit non-interactive consumption is intended.

### Gemini

```bash
cq gemini account show
cq gemini account show --json
```

Gemini authentication and project selection remain owned by Antigravity. cq reads Keychain service `gemini`, account `antigravity`, plus Antigravity's project cache. It does not provide Gemini login, switch, or removal commands.

### Token refresh

```bash
cq auth refresh
cq service install --component token-refresh
cq service uninstall --component token-refresh
```

`cq auth refresh` refreshes eligible Claude and Codex OAuth credentials before expiry. Complete installers schedule this work using the platform user service manager. Use `--component token-refresh` for focused refresh maintenance. Expired Claude accounts can still require interactive login.

Ordinary quota and account commands do not change background service registration. Complete installers register both components; public maintenance uses `cq service` with an explicit `--component`.

After an explicit account switch, already-running clients or MCP servers may need reconnection to reload credential state.

## Local proxy

`cq proxy` binds to loopback, routes Claude and Codex traffic, and publishes local model metadata. It supports Anthropic Messages clients plus native Codex Responses HTTP and WebSocket traffic, including compact, search, image, and realtime/live routes used by supported clients. The retired `/app-server` compatibility endpoint returns `410 Gone` with guidance to run Codex app-server locally and route its outbound Responses traffic through cq.

### Service lifecycle and status

Complete installers own service installation and removal. Use `cq service` to
inspect or restart both package-owned components together:

```bash
cq service status --json
cq service restart
```

`cq service install` and `cq service uninstall` expose the same transaction for
development and repair work. Package hooks call them automatically; users do
not need them after a complete install.

Focused proxy commands remain available for foreground and compatibility work,
but do not form a complete installation:

```bash
cq proxy serve
cq proxy serve --port 19280
cq service install
cq service restart
cq service uninstall
```

Legacy managed-identity migration flags are retired. Ordinary startup does not rewrite credential routing identities.

`cq proxy status` reconciles desired configuration, service ownership, listener, process, runtime health and data-plane evidence. Use `cq proxy health` for a health probe:

```bash
cq proxy health                    # Authenticated health inspection
cq proxy status            # Reconciled human summary
cq proxy status --json             # Reconciled stable JSON envelope
cq proxy status --strict --json    # Non-zero when reconciled state is unhealthy
cq proxy status --timeout 10s
cq proxy status --state-dir PATH
```

`GET /health` proves runtime reachability only; strict status checks broader ownership and runtime facts.

### Client routing

Point Claude Code-compatible traffic at `http://127.0.0.1:19280` with `ANTHROPIC_BASE_URL`. cq accepts its generated local bearer token and known Claude OAuth tokens, routes Anthropic models to Claude, and translates supported GPT/o-series Anthropic Messages requests to Codex Responses. Native Codex clients use Codex HTTP/WebSocket routes without Anthropic translation.

Core behaviour includes:

- model-registry-first provider selection with prefix fallback;
- Claude and Codex multi-account routing;
- quota/capacity-aware fair selection;
- account eligibility and explicit allowlists;
- hard continuity for bound Codex turns/sessions;
- HTTP per-turn routing and WebSocket connection-aware routing;
- bounded failover before response commitment;
- optional request headroom compression (`cache` or `token` mode);
- durable leases and retention for Codex continuity;
- automatic model-registry publication and drift repair.

### Pins, defaults, and priming

```bash
cq claude proxy pin show
cq codex proxy pin show
cq claude proxy pin set EMAIL_OR_UUID
cq claude proxy pin clear
cq codex proxy pin set EMAIL_ALIAS_OR_KEY
cq codex proxy pin clear

cq codex proxy fallback set EMAIL_ALIAS_OR_KEY
cq codex proxy fallback clear

cq codex proxy prime status
cq codex proxy prime enable
cq codex proxy prime disable
```

Claude pin changes hot-reload. Codex pin/default/priming changes require proxy restart. A Codex pin affects new and unbound work but does not break existing hard continuity.

### System account reserve

The running CQ service can protect a percentage of one Codex quota window for the current system account.
The reserve follows the system account when it changes. Other accounts remain available under their existing routing rules.

```bash
cq codex proxy reserve windows                     # List available window selectors
cq codex proxy reserve set --window 7d --percent 2   # Protect the final 2% of weekly quota
cq codex proxy reserve status
cq codex proxy reserve status --json
cq codex proxy reserve disable                     # Release the reserve until this window resets
cq codex proxy reserve enable                      # Restore protection immediately
cq codex proxy reserve clear                       # Remove the configuration
```

Use the selectors from `reserve windows` for scoped limits, such as `7d:gpt-reserve` or `5h:gpt-5.3-codex-spark`.
The commands require the running service. Changes apply immediately and survive service restarts.

At or below the threshold, the system account becomes unavailable to routing. Pools can continue through their other eligible accounts.
Pinned work receives HTTP 429 or the equivalent WebSocket usage-limit error when its account reaches the reserve.
The reserve applies to the account, not to a pool or its aggregate percentage.

`disable` requires fresh usage and reset evidence. A confirmed natural or forced reset restores protection automatically.
CQ never consumes a reset credit for this feature. A system account change also removes the previous account's temporary bypass.

The service consumes live response telemetry and refreshes usage in the background.
Polling follows Codex's 60/30/15/5-second cadence as usage approaches exhaustion; the reserve threshold can shorten that interval.
Concurrent refreshes share one request per account. Failed reads wait at least 30 seconds and honour longer `Retry-After` values.
If selected-window data exceeds its refresh interval plus 10 seconds, CQ temporarily excludes the protected account until fresh data arrives.
Already-running requests and usage outside CQ can cross the threshold before CQ receives an update.

### Capability policy and session pools

Advanced policy commands manage authenticated, capability-aware Codex account pools and privacy-safe session bindings:

```bash
cq proxy state initialise --state-dir DIR
cq codex proxy policy apply --file FILE
cq codex proxy policy show
cq codex proxy pool set NAME --account ACCOUNT --account ACCOUNT [--value VALUE]
cq codex proxy pool rename OLD_NAME NEW_NAME
cq codex proxy pool value NAME VALUE
cq codex proxy session bind --pool NAME --session-id ID
cq codex proxy session show --session-id ID
cq codex proxy session list
cq codex proxy session unbind --session-id ID
cq codex proxy session digest --session-id ID
```

Session selectors accept `--session-id`, `--session-id-stdin`, or a full keyed `--digest`. Live policy operations use authenticated loopback control; explicit `--state-dir` supports offline state.

Pool names are case-insensitive selectors and retain their configured display casing. Higher values preserve a pool's account capacity by routing ordinary unbound work through lower-value viable accounts first. Session bindings and task affinity remain hard constraints.

### Lease invalidation

```bash
cq codex proxy lease invalidate
```

Lease invalidation clears reusable Codex account affinity across all sessions.
Active requests and required continuity remain unchanged. Each next eligible
request selects again using current pool membership and account capacity.

### Rescue mode

```bash
cq proxy rescue enter
cq proxy rescue status
cq proxy rescue exit
```

Rescue mode is durable, local-token authenticated, and loopback-only. It lets operators move traffic through a bounded recovery path without sending control credentials upstream.

## Codex validation and operational controls

These commands exist for controlled validation, rollout, and recovery. Use each command's help before mutation.

### Installed routing validation

```bash
cq codex proxy fixture create --help
cq codex proxy readiness show --client-build BUILD [--state-dir DIR]
cq codex proxy validate websocket --help
cq codex proxy validate http --port CANDIDATE_PORT
```

Canonical HTTP validation is available only on macOS with an existing attested candidate service. Linux and Windows return `validation_candidate_unavailable` before IO. Port `19280` is forbidden. WebSocket validation, fixture creation and readiness inspection retain their separate contracts; retained readiness accepts `launchd`, `homebrew` and `systemd-user`. See each command’s help for required evidence.

### Routing canary

```bash
cq codex proxy canary start
cq codex proxy canary status
cq codex proxy canary stop
```

Canary start requires enforced HTTP routing and disabled payload diagnostics. Stop requests a drain; it does not discard active continuity.

### Isolated candidate lifecycle

```bash
cq proxy candidate prepare --help
cq proxy candidate status --state-dir PATH
cq proxy candidate receipt show --help
cq proxy candidate stop --help
cq proxy candidate remove --help
```

Preparation and inspection use explicit isolated state and supplied artefacts. A prepared artefact is not release qualification evidence. Four reserved commands return exit 4 before reading environment, credentials or files: `cq proxy candidate start`, `cq proxy candidate client-safety refresh`, `cq proxy candidate release activate` and `cq proxy candidate release validate`.

### Durable operations

```bash
cq proxy operation status
cq proxy operation status OPERATION_ID --json
```

Inspection returns active, retained terminal or idle state. Active recovery is unavailable; the retired recovery spelling returns exit 4 and never mutates state.

### Legacy credential endpoint maintenance

```bash
cq codex proxy credential-endpoint legacy inspect
cq codex proxy credential-endpoint legacy prepare ...
cq codex proxy credential-endpoint legacy resume ...
cq codex proxy credential-endpoint legacy activate ...
cq codex proxy credential-endpoint legacy finalise ...
cq codex proxy credential-endpoint legacy rollback ...
```

Ordinary cq/proxy startup never performs legacy endpoint maintenance. Inspection is read-only. Transition steps require explicit snapshots/tickets, stopped-and-drained or healthy-candidate confirmations, and retain rollback state.

### Codex Stop hook and efficiency receipt

```bash
cq codex proxy hook stop
```

Configured as a Codex `Stop` hook, this command reads hook JSON from stdin, performs an authenticated loopback lookup, and returns a privacy-safe `systemMessage`. Receipt summarises recorded route state, transport, pool/account hint, model/effort, route reason, and observational no-affinity comparison. Recorded state can be planned, attempted, completed, failed, rejected, or indeterminate. Receipt does not include prompts, transcripts, raw IDs, or credentials and does not change routing.

## Model registry

```bash
cq models refresh
cq models list
cq models list --json
cq models list --provider codex
cq models list --provider anthropic

cq models overlay add --provider codex --id gpt-5.5 --clone-from gpt-5.4
cq models overlay remove --provider codex --id gpt-5.5
cq models overlay prune
```

Registry refresh merges provider sources with local overlays, validates entries, and publishes:

- Codex model cache at `$CODEX_HOME/models_cache.json` or `~/.codex/models_cache.json`;
- Claude Code capability cache at `$CLAUDE_CONFIG_DIR/cache/model-capabilities.json` or `~/.claude/cache/model-capabilities.json`;
- managed Claude Code picker entries in `~/.claude.json`.

Overlays expose not-yet-native model IDs and can clone metadata from an existing model. `prune` removes overlays now supplied natively. Overlay store: `$XDG_CONFIG_HOME/cq/models.json` or `~/.config/cq/models.json`.

Proxy endpoints also expose model metadata and authenticated registry refresh/snapshot APIs for local clients.

## Proxy configuration

Config lives at `$XDG_CONFIG_HOME/cq/proxy.json`, or `~/.config/cq/proxy.json`. Canonical commands require existing configuration; complete installers create it with a random local token. Unknown fields are preserved across writes for version compatibility.

| JSON field | Default | Purpose |
|------------|---------|---------|
| `port` | `19280` | Loopback listen port. |
| `claude_upstream` | `https://api.anthropic.com` | Claude API upstream. |
| `codex_upstream` | `https://chatgpt.com/backend-api/codex` | ChatGPT OAuth-compatible Codex upstream. |
| `local_token` | generated | Local control/proxy bearer token. |
| `headroom` | `false` | Enable request headroom compression. |
| `headroom_mode` | `cache` | `cache` or `token` compression strategy. |
| `pinned_claude_account` | unset | Claude email/account UUID pin. Prefer `cq claude proxy pin set`. |
| `codex_turn_routing` | `off` | Codex HTTP routing mode: `off`, `observe`, or `enforce`. |
| `codex_ws_turn_routing` | `off` | Codex WebSocket routing mode: `off`, `observe`, or `enforce`. |
| `codex_routing_default_account_key` | unset | Default opaque Codex account key. |
| `codex_routing_pinned_account_key` | unset | Pinned opaque Codex account key. |
| `codex_routing_account_keys` | unset | Explicit eligible Codex account allowlist. |
| `codex_lease_retention_days` | `7` | Durable continuity retention, valid from 1 to 365 days. |
| `codex_continuity_state_dir` | cq config directory | Optional Codex lease/continuity state-root override. |
| `proxy_resilience_state_dir` | unset | Optional policy/runtime authority root; resilience controls stay inactive while unset. |
| `codex_window_priming` | disabled | Priming enablement and per-window model overrides. |
| `diagnostics_log` | unset | Causal routing trace JSONL path; restart required. |
| `payload_diagnostics_log` | unset | Raw request/response and WebSocket payload JSONL path; restart required. |

## Diagnostics and privacy

### Routing diagnostics

Set `diagnostics_log` and restart. Every Codex HTTP request and WebSocket `response.create` frame receives a trace ID. Ordered JSONL events record ingress, request identity, candidate eligibility and exclusion, selected account hint, pool, durable lease state before and after transitions, credential refresh, upstream dispatch/status, retries, failover, relay result, close/error classification, and terminal outcome. WebSocket events also carry a stable connection ID. Route summaries share the same trace ID. Logs rotate at 64 MiB and retain four prior files. Enabling diagnostics does not change routing policy.

Query one user-facing task, one trace, or recent traffic without manually searching JSONL:

```bash
cq codex proxy trace --session codex://threads/THREAD_ID --since 15m
cq codex proxy trace --trace trace:ID --json
cq codex proxy trace --follow
```

### Payload diagnostics

`payload_diagnostics_log` is disabled by default and requires restart. It records exact HTTP request and response bodies plus downstream and upstream Codex WebSocket frames. Every entry contains its causal trace ID, direction, byte count, encoding, account hint where applicable, and whether capture reached a complete body. Credential-bearing headers are excluded. Query it with `cq codex proxy trace --payload` and the same session/trace/time filters.

> **Warning:** payload diagnostics can contain prompts, system prompts, tool inputs, compact summaries, messages, provider responses, and other sensitive content. Do not share without review. Request and response bodies can themselves contain secrets.

Session and thread keys in diagnostics are short deterministic hashes, not raw identifiers. `session_source` records which header/body/WebSocket signal supplied session correlation.

## Environment and files

### Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CQ_TTL` | `30` | Quota cache TTL in seconds. |
| `XDG_CONFIG_HOME` | `~/.config` | cq config/state base. Must be absolute when supplied. |
| `XDG_CACHE_HOME` | platform user-cache directory | Quota cache and burn-history base; supplied value must be absolute. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Claude Code model-capability cache base. CQ credential discovery still uses macOS Keychain and `~/.claude`. |
| `CODEX_HOME` | `~/.codex` | Codex model-cache and client-discovery base. CQ managed/system credential discovery still uses `~/.codex`. |
| `ANTHROPIC_BASE_URL` | unset | Point compatible clients at cq proxy. |

### Important paths

| Path | Purpose |
|------|---------|
| `~/.config/cq/proxy.json` | Proxy configuration and local token. |
| `~/.config/cq/models.json` | User model overlays. |
| `~/.config/cq/state/` | Compatibility epoch, credential-control endpoint, and Codex removal journal. |
| `~/.config/cq/` | Default live-runtime lifecycle, canary, normal-caller admission, config, and overlay files. |
| Configured `codex_continuity_state_dir`, defaulting to `~/.config/cq/` | Durable Codex continuity and lease state. |
| Configured `proxy_resilience_state_dir` | Routing policy, dispatch-permit, and runtime-mode/rescue authority. No default is assumed; `cq proxy state initialise --state-dir DIR` configures it. |
| Command-supplied `--state-dir` | Isolated candidate lifecycle, validation, staged-release, and receipt state. |
| `$XDG_CACHE_HOME/cq/*.json` or platform cache equivalent | Provider quota cache. On macOS, default base is `~/Library/Caches/cq`. |
| `$XDG_CACHE_HOME/cq/burn_state_v2.json` or platform cache equivalent | Smoothed burn history. |
| `~/.claude/.credentials.json` | Claude account credentials. |
| `~/.claude.json` | Claude Code global config and managed picker entries. |
| `~/.codex/auth.json` | System Codex credential, read automatically but not rewritten by routing. |
| `~/.codex/accounts/` | CQ-managed Codex accounts and registry. |
| `~/.codex/models_cache.json` | Published Codex model cache. |
| `~/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist` | Background refresh agent on macOS. |
| `~/Library/Logs/cq/refresh.log` | Background refresh log. |
| `~/Library/Logs/cq/proxy.log` | Proxy service log. |

Secret/state writes use owner-only permissions and atomic replacement. External Gemini and declared external Codex credential stores remain read-only.

## Complete command index

This index is checked against the canonical command catalogue. The executable registers every one of its 89 leaves; bare groups show help.

<details>
<summary>Show every command path</summary>

<!-- public-command-index:start -->
```text
cq auth
cq auth refresh
cq check
cq claude
cq claude account
cq claude account activate
cq claude account list
cq claude account login
cq claude account remove
cq claude proxy
cq claude proxy pin
cq claude proxy pin clear
cq claude proxy pin set
cq claude proxy pin show
cq codex
cq codex account
cq codex account activate
cq codex account list
cq codex account login
cq codex account remove
cq codex proxy
cq codex proxy canary
cq codex proxy canary start
cq codex proxy canary status
cq codex proxy canary stop
cq codex proxy credential-endpoint
cq codex proxy credential-endpoint legacy
cq codex proxy credential-endpoint legacy activate
cq codex proxy credential-endpoint legacy finalise
cq codex proxy credential-endpoint legacy inspect
cq codex proxy credential-endpoint legacy prepare
cq codex proxy credential-endpoint legacy resume
cq codex proxy credential-endpoint legacy rollback
cq codex proxy fallback
cq codex proxy fallback clear
cq codex proxy fallback set
cq codex proxy fallback show
cq codex proxy fixture
cq codex proxy fixture create
cq codex proxy hook
cq codex proxy hook stop
cq codex proxy lease
cq codex proxy lease invalidate
cq codex proxy pin
cq codex proxy pin clear
cq codex proxy pin set
cq codex proxy pin show
cq codex proxy policy
cq codex proxy policy apply
cq codex proxy policy show
cq codex proxy pool
cq codex proxy pool rename
cq codex proxy pool set
cq codex proxy pool value
cq codex proxy prime
cq codex proxy prime disable
cq codex proxy prime enable
cq codex proxy prime status
cq codex proxy readiness
cq codex proxy readiness show
cq codex proxy reserve
cq codex proxy reserve clear
cq codex proxy reserve disable
cq codex proxy reserve enable
cq codex proxy reserve set
cq codex proxy reserve status
cq codex proxy reserve windows
cq codex proxy session
cq codex proxy session bind
cq codex proxy session digest
cq codex proxy session list
cq codex proxy session show
cq codex proxy session unbind
cq codex proxy trace
cq codex proxy validate
cq codex proxy validate http
cq codex proxy validate websocket
cq codex reset
cq codex reset list
cq codex reset recommend
cq codex reset use
cq completion
cq gemini
cq gemini account
cq gemini account show
cq help
cq models
cq models list
cq models overlay
cq models overlay add
cq models overlay prune
cq models overlay remove
cq models refresh
cq proxy
cq proxy candidate
cq proxy candidate client-safety
cq proxy candidate client-safety refresh
cq proxy candidate prepare
cq proxy candidate receipt
cq proxy candidate receipt show
cq proxy candidate release
cq proxy candidate release activate
cq proxy candidate release validate
cq proxy candidate remove
cq proxy candidate start
cq proxy candidate status
cq proxy candidate stop
cq proxy health
cq proxy operation
cq proxy operation status
cq proxy rescue
cq proxy rescue enter
cq proxy rescue exit
cq proxy rescue status
cq proxy serve
cq proxy state
cq proxy state initialise
cq proxy status
cq service
cq service install
cq service restart
cq service start
cq service status
cq service stop
cq service uninstall
cq version
```
<!-- public-command-index:end -->

</details>

## Development

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for branch, review, release, and Homebrew service rules.

## Licence

MIT
