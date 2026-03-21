<!-- Parent: ../AGENTS.md -->

# app

Application orchestration: concurrent fetching, report building, account management.

## Key Files

| File | Description |
|------|-------------|
| `runner.go` | `Runner.BuildReport`: concurrent provider fetch with WaitGroup, nil-safe cache |
| `report.go` | `buildReport`: assembles Report from results, computes aggregates, capitalises names |
| `accounts.go` | `RunLogin`, `RunAccounts`, `RunSwitch`, `PrintClaudeAccounts` — account management CLI handlers |

## For AI Agents

### Working In This Directory

- `Runner.Cache` can be nil (cache failure is non-fatal) — all cache access is guarded
- `buildReport` uses `capitalise()` which is safe for empty strings (unlike raw slice indexing)
- `PrintClaudeAccounts` requires `activeEmail != ""` for active-account matching to prevent false positives
- Account management functions write directly to stdout — these are CLI handlers, not library code
