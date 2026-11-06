<!-- Parent: ../../AGENTS.md -->

# cmd/cq

CLI entry point using kong for command parsing.

## Key Files

| File | Description |
|------|-------------|
| `main.go` | CLI struct, kong wiring, `runCheck` orchestration, cache/renderer setup |
| `main_test.go` | Tests for CLI configuration helpers (cacheTTL, cacheDir, provider resolution) |

## For AI Agents

### Working In This Directory

- `CLI` struct defines the kong command tree; `CheckCmd` is the default command
- `runCheck` wires together cache, runner, and renderer — cache failure is non-fatal (nil cache)
- `runCheck` passes one shared HTTP client and release-linked Gemini OAuth client secret to providers
- Provider IDs are the four constants in `internal/provider/provider.go`; keep `CheckCmd.Providers` and the provider wiring consistent
- `dispatch` routes kong commands to the appropriate handler function

- `service` coordinates proxy and refresh lifecycle through platform adapters; keep direct component repair commands distinct from complete installation.
- `proxy`, `models`, `codex dots`, `codex network`, `codex harness` and reset scheduling have separate state owners. Follow [the architecture guide](../../docs/architecture.md) before changing their boundaries.
- A stored setting, successful package hook or `/health` result does not prove runtime adoption or real affected traffic. Use the existing installed validation gates in [the release guide](../../docs/releases.md).
