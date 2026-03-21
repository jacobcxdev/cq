<!-- Parent: ../AGENTS.md -->

# internal

All business logic, organised by domain.

## Subdirectories

| Directory | Purpose |
|-----------|---------|
| `provider/` | Provider interface and per-provider implementations ([AGENTS.md](provider/AGENTS.md)) |
| `app/` | Runner, Report types, account management ([AGENTS.md](app/AGENTS.md)) |
| `output/` | TTY (lipgloss) and JSON renderers ([AGENTS.md](output/AGENTS.md)) |
| `aggregate/` | Multi-account weighted pace, sustainability, burndown ([AGENTS.md](aggregate/AGENTS.md)) |
| `quota/` | Domain types: Result, Window, Status, time helpers ([AGENTS.md](quota/AGENTS.md)) |
| `auth/` | OAuth PKCE flow, browser detection, JWT decode ([AGENTS.md](auth/AGENTS.md)) |
| `keyring/` | Credential discovery and persistence ([AGENTS.md](keyring/AGENTS.md)) |
| `cache/` | File-based JSON cache with atomic writes and TTL ([AGENTS.md](cache/AGENTS.md)) |
| `httputil/` | HTTP client with User-Agent, body limits, redirect safety ([AGENTS.md](httputil/AGENTS.md)) |
| `fsutil/` | FileSystem interface and MemFS for test injection ([AGENTS.md](fsutil/AGENTS.md)) |
