<!-- Parent: ../AGENTS.md -->

# fsutil

Filesystem abstraction for dependency injection in tests.

## Key Files

| File | Description |
|------|-------------|
| `fs.go` | `FileSystem` interface: ReadFile, WriteFile, Rename, Remove, Stat, MkdirAll, UserHomeDir, ReadDir |
| `memfs.go` | `MemFS`: in-memory implementation for tests (MkdirAll is a no-op, ReadDir not implemented) |

## For AI Agents

### Working In This Directory

- `OSFileSystem` is the production implementation (delegates to `os` package)
- `MemFS` supports injectable errors via `HomeDir`/`HomeDirErr` fields
- `MemFS.MkdirAll` is a no-op and `ReadDir` returns an error — only file operations are implemented
- Used by cache, Gemini provider, and Codex provider for testable file I/O
