<!-- Parent: ../AGENTS.md -->

# provider

Shared interfaces and per-provider implementations.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Provider` interface, five `ID` constants, `Services` struct |

## Subdirectories

| Directory | Purpose |
|-----------|---------|
| `claude/` | Multi-account provider with parallel profile+usage fetch, dedup, token refresh ([AGENTS.md](claude/AGENTS.md)) |
| `codex/` | Multi-account provider with read-only automatic credential use ([AGENTS.md](codex/AGENTS.md)) |
| `gemini/` | Single-account provider using direct Antigravity OAuth and quota HTTP ([AGENTS.md](gemini/AGENTS.md)) |
| `zai/` | Multi-account Coding Plan, Start and ZCode authentication/quota; [routing guide](../../docs/zcode-routing.md) |
| `grok/` | Multi-account subscription OAuth, included quota, account-scoped models and borrowed CodexBar discovery; [usage and limits](../../docs/usage.md#grok-subscriptions) |
| `codexbar/` | Provider-scoped read-only external configuration for Grok and Z.ai; credential ownership stays with its source |

## For AI Agents

### Working In This Directory

- Each provider implements `Provider.Fetch(ctx, now) ([]quota.Result, error)`
- HTTP providers use `httputil.Doer`; Gemini also injects filesystem and Keychain readers
- Panic recovery is mandatory in all fetch goroutines
- Error results use `quota.ErrorResult` with specific codes — never return bare errors to the runner
- Claude, Codex, Z.ai and Grok are multi-account providers; Codex system credentials remain read-only automatically
- Gemini account authority stays with Antigravity; cq reads credentials and refreshes in memory but never writes Antigravity-owned state
