# cq

A CLI tool to check AI provider quota usage at a glance. Supports **Claude**, **Codex**, and **Gemini**, with optional local proxy support for quota-aware routing and model registry publishing.

## Install

```bash
brew install jacobcxdev/tap/cq
```

Or with Go:

```bash
go install github.com/jacobcxdev/cq/cmd/cq@latest
```

## Usage

```bash
cq                         # Check all providers
cq check claude codex      # Check specific providers
cq --json                  # JSON output
cq --refresh               # Bypass cached quota results
cq --version               # Print version
```

`check` accepts `claude`, `codex`, and `gemini` provider names.

### JSON availability

`cq --json` includes a provider-level `availability` object for agents and other automated consumers. Use `availability.state` and `availability.guidance` to decide whether to send new work to a provider:

- `available`: provider is available for normal work.
- `limited`: provider can route work, but quota is low; conserve it for small, necessary, or user-approved work.
- `exhausted`: provider is exhausted or unavailable for new work.

Account-level `active` fields are retained for compatibility and mean credential default/current account. They are not proxy routing decisions and should not be used as provider availability signals; the proxy may route differently because of quota, manual pins, or failover.

## Account Management

Claude and Codex support stored account management:

```bash
cq claude login --activate # Add Claude account via OAuth and make it active
cq claude accounts         # List Claude accounts
cq claude switch EMAIL     # Switch active Claude account
cq claude remove EMAIL     # Remove Claude account

cq codex login --activate  # Add Codex account via OAuth and make it active
cq codex accounts          # List Codex accounts
cq codex switch EMAIL      # Switch active Codex account
cq codex remove EMAIL      # Remove Codex account
```

Gemini currently exposes account discovery only:

```bash
cq gemini accounts         # Show Gemini account
```

> **Note:** After switching accounts, MCP servers that use the provider's credentials (for example Codex MCP) may need to be reconnected to pick up the new active account.

## Proxy

`cq proxy` runs a local Anthropic-compatible proxy for Claude Code and other clients. It can route runtime traffic through cq while preserving provider credentials and quota awareness.

```bash
cq proxy start             # Start the proxy
cq proxy start --port 19280
cq proxy status            # Check proxy health
cq proxy status --port 19280
cq proxy install           # Install the user launch agent
cq proxy uninstall         # Remove the user launch agent
cq proxy restart           # Restart the user launch agent
cq proxy pin               # Show the pinned Claude account, if any
cq proxy pin <email-or-account-uuid>
cq proxy pin --clear       # Clear the pinned Claude account
```

Use `cq proxy pin --clear` to clear a pin. `clear` and `remove` are reserved words, not valid literal pin values.

The proxy config is stored at `$XDG_CONFIG_HOME/cq/proxy.json`, or `~/.config/cq/proxy.json` when `XDG_CONFIG_HOME` is not set. If it does not exist, `cq proxy start` creates it with a random local token.

Important `proxy.json` fields:

| JSON field | Default | Description |
|------------|---------|-------------|
| `port` | `19280` | Local proxy listen port. |
| `claude_upstream` | `https://api.anthropic.com` | Anthropic API upstream. |
| `codex_upstream` | `https://chatgpt.com/backend-api/codex` | Codex backend upstream. |
| `local_token` | generated | Required bearer token for local proxy requests. |
| `pinned_claude_account` | unset | Optional Claude account email or UUID to force proxy selection. |
| `diagnostics_log` | unset | Optional JSONL routing diagnostics log path for advanced local debugging. |
| `headroom` | `false` | Enables the headroom compression bridge when true. |
| `headroom_mode` | `cache` | Compression strategy when set; valid values are `cache` and `token`. |

Routing diagnostics are disabled by default. To enable them, set `diagnostics_log` in `proxy.json` to a local file path and restart the proxy. The log is append-only JSONL containing redacted route metadata such as method, path, provider, route kind, status, and latency. It is intended for advanced local debugging and UAT, and enabling it does not change routing policy.

## Model Registry

`cq models` manages the local model registry used by the proxy, Claude Code model caches, and Codex model cache integration.

```bash
cq models refresh                         # Refresh registry data and publish caches
cq models list                            # List active registry models
cq models list --json                     # JSON model list
cq models list --provider codex           # Filter by provider
cq models list --provider anthropic

cq models overlay add --provider codex --id gpt-5.5 --clone-from gpt-5.4
cq models overlay remove --provider codex --id gpt-5.5
cq models overlay prune                   # Remove overlays shadowed by native models
```

Registry overlays are stored at `$XDG_CONFIG_HOME/cq/models.json`, or `~/.config/cq/models.json` when `XDG_CONFIG_HOME` is not set.

A registry refresh publishes provider-specific caches where supported:

- Codex model cache: `$CODEX_HOME/models_cache.json`, or `~/.codex/models_cache.json`.
- Claude Code model capabilities: `$CLAUDE_CONFIG_DIR/cache/model-capabilities.json`, or `~/.claude/cache/model-capabilities.json`.
- Claude Code picker options: `additionalModelOptionsCache` in `~/.claude.json`.

Claude Code still needs `ANTHROPIC_BASE_URL` pointed at the running proxy for runtime API traffic. The `/model` picker is populated from Claude Code config/cache files, so `cq models refresh` and the proxy publish registry-backed picker entries there. The proxy also re-publishes picker entries automatically when it detects drift.

## Background Agent

```bash
cq agent install           # Install the quota refresh launch agent
cq agent uninstall         # Remove the quota refresh launch agent
cq refresh                 # Run a one-shot refresh
```

## What It Shows

For each provider, cq displays remaining quota as a percentage bar, pace indicator, and burndown estimate for each rate-limit window. Requires a [Nerd Font](https://www.nerdfonts.com/) for icons to render correctly. Recommended: [`jacobcxdev/tap/liga-sf-mono-nerd-font`](https://github.com/jacobcxdev/homebrew-tap).

![cq output](docs/screenshot.png)

## Configuration

### Environment variables

| Environment variable | Default | Description |
|----------------------|---------|-------------|
| `CQ_TTL` | `30` | Quota cache duration in seconds, e.g. `60`, `300`. |
| `XDG_CONFIG_HOME` | `~/.config` | Base directory for cq config files. |
| `XDG_CACHE_HOME` | platform cache dir | Base directory for cq quota cache files. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Claude Code config directory for model capability cache publication. |
| `CODEX_HOME` | `~/.codex` | Codex config directory for model cache reads/writes. |

### Config and cache files

| Path | Purpose |
|------|---------|
| `~/.config/cq/proxy.json` | Local proxy port, upstreams, token, and headroom settings. |
| `~/.config/cq/models.json` | User-managed model registry overlays. |
| `~/.cache/cq/*.json` | Cached quota results and account metadata. |
| `~/.claude/.credentials.json` | Claude credentials read/written for account management. |
| `~/.claude.json` | Claude Code global config; cq writes managed model picker entries. |
| `~/.codex/models_cache.json` | Codex model cache populated by registry refresh. |
| `~/Library/Logs/cq/proxy.log` | macOS launch agent log for the proxy service. |
| `~/Library/Logs/cq/refresh.log` | macOS launch agent log for quota refresh. |

## Licence

MIT
