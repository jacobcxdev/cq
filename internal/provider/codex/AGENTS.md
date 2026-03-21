<!-- Parent: ../AGENTS.md -->

# provider/codex

Single-account Codex provider. Returns `auth_expired` on 401/403 (no token refresh — cq must not mutate shared credentials).

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: reads auth.json, fetches usage, returns `auth_expired` on 401/403 |
| `parser.go` | `parseUsage`: parses Codex usage JSON, handles numeric/string reset_at |
| `refresh.go` | `fetchUsage` (HTTP call) |

## For AI Agents

### Working In This Directory

- No token refresh — cq shares `~/.codex/auth.json` with codex CLI; Auth0 refresh token rotation means refreshing would invalidate codex's copy
- `parseNumericResetAt` only handles `float64` and `string` (standard `json.Unmarshal` types)
- Tests use `fakeFS` with injectable errors rather than `fsutil.MemFS` (needs error injection)
