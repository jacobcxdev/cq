<!-- Parent: ../AGENTS.md -->

# provider/codex

Multi-account Codex provider. Returns `auth_expired` on 401/403. Automatic code never refreshes, activates, removes, or rewrites system auth. Eligible CQ-owned managed lineages and explicitly declared CodexBar managed accounts renew only through the credential coordinator owner.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | `Fetch`: discovers accounts, fetches usage, returns `auth_expired` on 401/403 |
| `parser.go` | `parseUsage`: parses Codex usage JSON, handles numeric/string reset_at |
| `refresh.go` | `fetchUsage` (HTTP call) |

## For AI Agents

### Working In This Directory

- Never refresh system, borrowed, legacy, exported, or uncertain credentials outside the declared CodexBar owner capability below. Managed refresh requires `cq_oauth + cq_owned_never_exported + ready` through the coordinator broker.
- Automatic routing never writes `~/.codex/auth.json`, updates registry active state, or invokes account switching
- External sources remain read-only authorities, with one explicit exception: the live coordinator owner may renew declared CodexBar managed accounts within 60 seconds of access expiry. Renewal shares CodexBar's manifest lock, fences exact identity/home/revision, journals rotating results privately, and publishes auth plus its manifest fingerprint back to the owner. Newer owner login wins. Ambiguous exchanges are never replayed.
- Other external sources and system credentials remain read-only. Renewal never activates, removes, adopts, scans beyond declared paths, or projects external material. Healthy inventory never waits for another account's renewal; all-unavailable reads wait at most one second for fast repair.
- `parseNumericResetAt` only handles `float64` and `string` (standard `json.Unmarshal` types)
- Tests use `fakeFS` with injectable errors rather than `fsutil.MemFS` (needs error injection)
