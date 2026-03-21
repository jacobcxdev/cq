<!-- Parent: ../AGENTS.md -->

# provider

Shared interfaces and per-provider implementations.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Provider` interface, `ID` type, `Ordered` list, `Services` struct |

## Subdirectories

| Directory | Purpose |
|-----------|---------|
| `claude/` | Multi-account provider with parallel profile+usage fetch, dedup, token refresh ([AGENTS.md](claude/AGENTS.md)) |
| `codex/` | Single-account provider with reactive 401/403 token refresh ([AGENTS.md](codex/AGENTS.md)) |
| `gemini/` | Single-account provider with expiry-based refresh, PATH-based credential discovery ([AGENTS.md](gemini/AGENTS.md)) |

## For AI Agents

### Working In This Directory

- Each provider implements `Provider.Fetch(ctx, now) ([]quota.Result, error)`
- All providers use `httputil.Doer` for HTTP — tests inject `urlRewriter` transports
- Panic recovery is mandatory in all fetch goroutines
- Error results use `quota.ErrorResult` with specific codes — never return bare errors to the runner
- Claude is the only multi-account provider; its parser includes dedup logic
