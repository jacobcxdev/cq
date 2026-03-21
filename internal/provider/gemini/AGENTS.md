<!-- Parent: ../AGENTS.md -->

# provider/gemini

Single-account Gemini provider. Returns `auth_expired` when token is expired (no token refresh — cq must not mutate shared credentials).

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: reads oauth_creds.json, checks expiry, parallel tier+quota fetch |
| `parser.go` | `parseTier`, `parseQuota`: JSON parsing for tier and quota responses |
| `refresh.go` | `fetchTier`, `fetchQuota` — HTTP calls |

## For AI Agents

### Working In This Directory

- No token refresh — cq shares `~/.gemini/oauth_creds.json` with gemini CLI; refreshing could invalidate gemini's cached credentials
- Parallel fetch has separate error tracking: `quotaPanic` (bool) vs `quotaErr` (error) vs `quotaCode` (int)
