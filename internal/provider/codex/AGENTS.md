<!-- Parent: ../AGENTS.md -->

# provider/codex

Multi-account Codex provider. Returns `auth_expired` on 401/403. Automatic code reads system auth but never refreshes, activates, removes, or rewrites it.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: discovers accounts, fetches usage, returns `auth_expired` on 401/403 |
| `parser.go` | `parseUsage`: parses Codex usage JSON, handles numeric/string reset_at |
| `refresh.go` | `fetchUsage` (HTTP call) |

## For AI Agents

### Working In This Directory

- No automatic token refresh — CQ shares `~/.codex/auth.json` with Codex and other account tools; Auth0 rotation can invalidate another writer's copy
- Automatic routing never writes `~/.codex/auth.json`, updates registry active state, or invokes account switching
- `parseNumericResetAt` only handles `float64` and `string` (standard `json.Unmarshal` types)
- Tests use `fakeFS` with injectable errors rather than `fsutil.MemFS` (needs error injection)
