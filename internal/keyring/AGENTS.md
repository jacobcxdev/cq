<!-- Parent: ../AGENTS.md -->

# keyring

Claude credential discovery and persistence across multiple storage backends.

## Key Files

| File | Description |
|------|-------------|
| `keyring.go` | `DiscoverClaudeAccounts`, `StoreCQAccount`, `BackfillCredentialsFile`, `PersistRefreshedToken`, manifest management |
| `keyring_darwin.go` | macOS keychain access via `security` CLI (credentials passed via stdin) |

## For AI Agents

### Working In This Directory

- Discovery order: credentials file → platform keychain → cq keyring. `mergeAnonymousFresh` only merges when exactly 1 identified account exists
- All file writes use atomic tmp+rename with `0o600` permissions (files) and `0o700` (directories)
- `BackfillCredentialsFile` and `PersistRefreshedToken` log errors to stderr and return void — they are best-effort
- `accountKey` falls back to hashed access token when no stable identifier exists — this is fragile after refresh
- Functions call real OS/keychain — unit tests that need accounts should mock at the provider level
