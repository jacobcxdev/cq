<!-- Parent: ../AGENTS.md -->

# keyring

Claude credential discovery and persistence across multiple storage backends.

## Key Files

| File | Description |
|------|-------------|
| `keyring.go` | `DiscoverClaudeAccounts`, `StoreCQAccount`, `BackfillCredentialsFile`, `PersistRefreshedToken`, manifest management |
| `keyring_darwin.go` | Legacy macOS keychain access via `security`; `UpdateKeychainEntry` passes credential JSON as the `-w` argument |

## For AI Agents

### Working In This Directory

- Discovery order: credentials file → platform keychain → cq keyring. `mergeAnonymousFresh` uses token affinity (`sameStoredAccount`) to match anonymous keychain entries to identified accounts — never merges blindly
- All file writes use atomic tmp+rename with `0o600` permissions (files) and `0o700` (directories)
- `BackfillCredentialsFile` and `PersistRefreshedToken` log errors to stderr and return void — they are best-effort
- `accountKey` falls back to hashed access token when no stable identifier exists — this is fragile after refresh
- Functions call real OS/keychain — unit tests that need accounts should mock at the provider level

- The legacy command argument is a credential exposure surface. Never log, print or retain its arguments or values. Do not describe it as stdin-based or copy it into new credential paths.
- Proxy worker execution and credential-broker restrictions are independent; this legacy backend does not authorise ambient helper execution there.
