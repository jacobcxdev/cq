<!-- Parent: ../AGENTS.md -->

# provider/claude

Multi-account Claude provider with parallel profile+usage fetch and dedup.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: discovers accounts, parallel fetch per account with panic recovery |
| `client.go` | `FetchProfile`, `FetchUsage` — HTTP calls to Claude API |
| `parser.go` | `parseProfile`, `parseUsage`, `dedup` — JSON parsing and account deduplication |
| `refresh.go` | `RefreshToken` — OAuth token refresh via platform.claude.com |
| `accounts.go` | `Accounts.Discover`, `Accounts.Switch` — account management |
| `credentials.go` | `persistRefreshedToken`, `backfillCredentialsFile` — credential persistence helpers |

## For AI Agents

### Working In This Directory

- `Fetch` spawns one goroutine per account; each has panic recovery
- `fetchAccount` spawns two inner goroutines (profile + usage) — both have panic recovery
- `dedup` prefers usable results over errors on key collision (not first-occurrence wins)
- Profile fetch errors are non-fatal (falls back to keychain metadata)
- Usage fetch errors are fatal for that account (returns `fetch_error`)
- `parseUsage` clamps percentages to [0, 100] and includes parse error details in ErrorResult
