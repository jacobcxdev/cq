<!-- Parent: ../AGENTS.md -->

# cache

File-based JSON cache with TTL and atomic writes.

## Key Files

| File | Description |
|------|-------------|
| `cache.go` | `New`, `Get`, `Put` — cache keyed by provider ID, TTL-based expiry |
| `fs.go` | Type aliases for `fsutil.FileSystem` and `fsutil.OSFileSystem` |

## For AI Agents

### Working In This Directory

- Cache IDs are validated via `filepath.Base` to prevent path traversal
- `Get` treats read errors and JSON parse errors as cache misses (fail-open)
- `Put` uses atomic tmp+rename with `0o600` permissions
- `New` can fail (e.g., unwritable dir) — callers must handle nil cache gracefully
