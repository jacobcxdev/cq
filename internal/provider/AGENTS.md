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
| `codex/` | Multi-account provider with read-only automatic credential use ([AGENTS.md](codex/AGENTS.md)) |
| `gemini/` | Single-account provider using bounded structured Antigravity CLI output ([AGENTS.md](gemini/AGENTS.md)) |

## For AI Agents

### Working In This Directory

- Each provider implements `Provider.Fetch(ctx, now) ([]quota.Result, error)`
- HTTP providers use `httputil.Doer`; Gemini injects a direct command runner
- Panic recovery is mandatory in all fetch goroutines
- Error results use `quota.ErrorResult` with specific codes — never return bare errors to the runner
- Claude and Codex are multi-account providers; Codex system credentials remain read-only automatically
- Gemini account authority stays with Antigravity CLI; cq never reads or refreshes its credentials
