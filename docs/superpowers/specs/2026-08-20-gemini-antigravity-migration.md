# Gemini Antigravity Migration Design

- **Status:** Approved
- **Date:** 2026-08-20
- **Scope:** Gemini provider quota fetch and local account discovery

## Outcome

`cq check gemini` keeps provider ID `gemini` while sourcing quota from authenticated Antigravity CLI. Existing CLI commands, JSON provider keys, cache keys, and provider ordering remain compatible. Legacy Gemini CLI credentials and retired Code Assist consumer endpoints are no longer read or called.

## Runtime authority

Installed Antigravity CLI 1.1.16 exposes quota through structured print-mode output:

```bash
agy -p /usage --output-format json --print-timeout 15s
```

Observed successful output identifies `command.name` as `usage`, reports `num_turns: 0`, consumes zero tokens, and supplies quota groups under `command.data.groups`. Gemini quota uses stable bucket IDs `gemini-5h` and `gemini-weekly`. Third-party models use separate `3p-*` buckets.

Direct calls to `daily-cloudcode-pa.googleapis.com` returned `403 PERMISSION_DENIED` without Antigravity's internal entitlement context. Reading Keychain tokens or reverse-engineering private RPCs would couple CQ to undocumented credential and API contracts. Official CLI output is therefore provider boundary.

## Compatibility

- Provider ID stays `gemini`.
- `cq`, `cq check gemini`, JSON provider keys, cache keys, icons, and display name stay unchanged.
- Gemini remains single-account.
- Discovered account uses stable account ID `antigravity-cli`, label `Antigravity CLI`, and `active: true` when `agy` exists on `PATH`.
- Email and tier remain empty because structured `/usage` output does not expose them.

## Command boundary

CQ resolves `agy` with `exec.LookPath` and executes its resolved path directly with `exec.CommandContext`. No shell participates. Provider passes only exact arguments listed above.

Command receives caller context plus 20-second safety timeout. Stdout is limited to 1 MiB. Stderr is discarded and raw command output never appears in CQ errors, preventing accidental disclosure of account or diagnostic data.

CQ accepts output only when all safety invariants hold:

- process exits successfully;
- top-level `status` equals `SUCCESS`;
- `num_turns` equals zero;
- `usage.total_tokens` equals zero;
- `command.name` equals `usage`;
- exactly one `gemini-5h` bucket exists;
- exactly one `gemini-weekly` bucket exists.

Any changed slash-command behavior therefore fails closed instead of dispatching model work through CQ.

## Quota mapping

CQ ignores non-Gemini groups and maps exact bucket IDs:

| Antigravity bucket | CQ window |
|---|---|
| `gemini-5h` | `5h` |
| `gemini-weekly` | `7d` |

`remaining_fraction` is multiplied by 100, rounded to nearest integer, and clamped to 0-100. `reset_time` uses existing RFC3339 quota parser. Result uses stable account ID `antigravity-cli`, `active: true`, and status derived from both windows.

Unknown groups and bucket IDs are ignored. Missing or duplicate required Gemini buckets reject complete response; CQ never reports partial quota as complete.

## Error behaviour

- Missing `agy`: `not_configured`, with guidance to install and authenticate Antigravity CLI.
- Command start, exit, timeout, or bounded-read failure: `fetch_error` with privacy-safe message.
- Malformed JSON or violated safety invariants: `parse_error` with privacy-safe message.
- Missing or duplicate Gemini buckets: `parse_error` with privacy-safe message.

Provider continues returning `quota.ErrorResult` rows rather than bare errors. Runner cache backfill behaviour remains unchanged.

## Code shape

- `provider.go` owns provider orchestration and result classification.
- `cli.go` owns executable lookup, bounded exact command execution, and injectable command interface.
- `parser.go` owns structured `/usage` decoding, safety validation, and quota mapping.
- Legacy OAuth refresh and Code Assist HTTP helpers are deleted.
- Command wiring constructs Gemini provider without HTTP dependency.

## Verification

1. Write failing tests for exact command invocation, account discovery, structured success mapping, zero-turn safety, malformed output, missing/duplicate buckets, command failure, clamping, and cancellation.
2. Run focused Gemini tests with `-race -count=1` after each TDD cycle.
3. Run `go test -race -count=1 ./...`, `go vet ./...`, and `go build ./...`.
4. Build isolated CQ binary and run `check gemini --json --refresh` against installed authenticated Antigravity CLI.
5. Confirm live result contains provider key `gemini`, windows `5h` and `7d`, no API 403, and no model-token use reported by command contract.

## Non-goals

- Renaming provider to `antigravity`.
- Reading or refreshing Antigravity Keychain credentials.
- Calling undocumented Antigravity HTTP or local RPC endpoints.
- Reporting Claude/GPT Antigravity quota.
- Adding Gemini multi-account management.
- Changing global cache, renderer, aggregation, or availability semantics.
