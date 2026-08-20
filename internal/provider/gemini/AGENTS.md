<!-- Parent: ../AGENTS.md -->

# provider/gemini

Single-account Gemini provider backed by authenticated Antigravity CLI. Provider identity remains `gemini`; account authority remains external.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | Discovers `agy`, applies timeout, invokes exact structured usage command, classifies errors |
| `cli.go` | Direct no-shell command execution with bounded stdout and cancellation |
| `parser.go` | Validates zero-token usage envelope and maps required Gemini quota buckets |

## For AI Agents

### Working In This Directory

- Execute only `agy -p /usage --output-format json --print-timeout 15s`; command changes require safety review and tests
- Accept output only when status is `SUCCESS`, turns and total tokens are zero, command name is `usage`, and required buckets occur exactly once
- Keep stdout capped at 1 MiB, discard stderr, and never include raw command output in errors
- Never read, refresh, write, log, or persist Antigravity credentials
- Ignore `3p-*` buckets; map `gemini-5h` to `5h` and `gemini-weekly` to `7d`
