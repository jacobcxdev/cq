<!-- Parent: ../AGENTS.md -->

# httputil

HTTP client construction with security defaults.

## Key Files

| File | Description |
|------|-------------|
| `client.go` | `NewClient` (User-Agent, timeouts, redirect safety), `Doer` interface, `ReadBody` (1 MiB limit) |

## For AI Agents

### Working In This Directory

- `Doer` interface is the HTTP abstraction used by all providers — tests inject fakes here
- `ReadBody` limits responses to 1 MiB — all provider code must use this, not `io.ReadAll`
- `CheckRedirect` strips `Authorization` header on cross-host redirects to prevent credential leaks
- `uaTransport` clones the request before modifying headers — never mutates the caller's request
