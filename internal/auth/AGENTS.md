<!-- Parent: ../AGENTS.md -->

# auth

OAuth PKCE login flow, browser detection, and JWT utilities.

## Key Files

| File | Description |
|------|-------------|
| `oauth.go` | `Login` function: PKCE flow, local callback server, token exchange, profile fetch |
| `browser.go` | Platform-specific browser detection and private-mode launch (Darwin/Linux/Windows) |
| `jwt.go` | `DecodeEmail`: extracts email from JWT payload without signature verification |

## For AI Agents

### Working In This Directory

- OAuth callback uses `sync.Once` that gates **only valid callbacks** — invalid requests (bad state, missing code) are rejected before the Once
- Listener binds `127.0.0.1` and redirect URI uses `127.0.0.1` (not `localhost`) to avoid IPv4/IPv6 mismatch
- All `exec.Command(...).Start()` calls must use `startAndReap` to prevent zombie processes
- `openBrowser` validates URL scheme (http/https only) before any exec
- `validBrowserName` regex is a security boundary for AppleScript interpolation
