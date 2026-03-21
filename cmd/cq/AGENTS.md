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
- Provider IDs come from `provider.Ordered`; the enum constraint on `CheckCmd.Providers` must match
- `dispatch` routes kong commands to the appropriate handler function
