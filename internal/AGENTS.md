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

Additional domains are described in [the architecture guide](../docs/architecture.md):

| Directory | Purpose |
| --- | --- |
| `proxy/` | Owned supervisor/worker, routing, continuity and private control |
| `modelregistry/` | Account-scoped discovery/cache, overlays and projections |
| `dots/` | Optional native multi-account adapter, relay and launcher |
| `harness/` | Layered capability policy, complete inventory and apply receipts |
| `history/` | Burn estimates and reset corrections |
| `installer/`, `installstate/` | Verified official installation and ownership |
| `userdirs/` | Platform config, state, cache, runtime and log roots |
| `tools/` | Release/package/installed-client verification tools |

Provider IDs include `claude`, `codex`, `gemini` and `zai`. Keep account ownership,
routing eligibility, native discovery identity and runtime activation separate.
