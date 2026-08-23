<!-- Parent: ../../AGENTS.md -->

# provider/gemini

Single-account Gemini provider backed by direct Antigravity HTTP calls. Provider identity remains `gemini`; credential and project authority remains external.

## Key Files

| File | Description |
|------|-------------|
| `provider.go` | Reads independent local inputs concurrently, refreshes token in memory, resolves project, and classifies results |
| `credentials.go` | Read-only Antigravity Keychain and bounded project-cache decoding |
| `client.go` | Bounded OAuth, `loadCodeAssist`, and quota-summary HTTP requests |
| `parser.go` | Maps required direct Gemini quota buckets |

## For AI Agents

### Working In This Directory

- Never invoke `agy`, scrape its binary at runtime, or add a CLI fallback
- Read Keychain service `gemini`, account `antigravity`, and Antigravity project cache without writing either
- Keep public OAuth client ID in source; inject client secret only at release link time
- Retain refreshed access and refresh tokens only in process memory
- Send exact `User-Agent: antigravity/cli/cq` on private Antigravity endpoints
- Read all HTTP responses through `httputil.ReadBody`
- Keep panic recovery around concurrent credential and project reads
- Ignore `3p-*` buckets; map `gemini-5h` to `5h` and `gemini-weekly` to `7d`
- Never include credentials, project IDs, raw response bodies, or transport details in errors
