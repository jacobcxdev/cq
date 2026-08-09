<!-- Parent: ../AGENTS.md -->

# provider/codex

Multi-account Codex provider. Returns `auth_expired` on 401/403. Automatic code never refreshes, activates, removes, or rewrites system auth. Eligible CQ-owned managed lineages refresh only through the credential coordinator.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: discovers accounts, fetches usage, returns `auth_expired` on 401/403 |
| `parser.go` | `parseUsage`: parses Codex usage JSON, handles numeric/string reset_at |
| `refresh.go` | `fetchUsage` (HTTP call) |

## For AI Agents

### Working In This Directory

- Never refresh system, borrowed, legacy, exported, or uncertain credentials. Managed refresh requires `cq_oauth + cq_owned_never_exported + ready` through the coordinator broker.
- Automatic routing never writes `~/.codex/auth.json`, updates registry active state, or invokes account switching
- External sources, including CodexBar, are read-only authorities. CQ may enumerate declared metadata and resolve exact pinned revisions only. CQ never mutates, refreshes, activates, removes, adopts, scans beyond declared paths, or projects external material.
- `parseNumericResetAt` only handles `float64` and `string` (standard `json.Unmarshal` types)
- Tests use `fakeFS` with injectable errors rather than `fsutil.MemFS` (needs error injection)
