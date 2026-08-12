# Codex Request-Size Parity Design

- **Status:** Proposed
- **Date:** 2026-08-12
- **Scope:** Native Codex Responses HTTP requests, compact requests, WebSocket request messages, request replay, and Headroom transport

## Outcome

CQ mediates the request contract exposed by installed Codex without inventing a smaller transport limit.

- Native HTTP `/responses` and `/responses/compact` add no CQ-specific encoded or decoded body ceiling. CQ forwards any backend `413 Payload Too Large` response unchanged.
- Native WebSocket request messages use the installed Codex client's 64 MiB logical-message ceiling.
- Routing, metadata extraction, model rewriting, durable request freezing, replay, and Headroom accept every request admitted by the matching transport boundary.
- Response events, diagnostics, rejected-response parsing, executable inspection, and other unrelated bounded readers keep their existing limits.
- Live port `19280` is never used for candidate validation. Codex CLI points at an isolated candidate listener.

## Protocol authority

Installed Codex CLI `0.146.0` corresponds to official tag `rust-v0.146.0`, commit `e363b08c9175ac1cbe5893615dd2cb9ddf95043b`.

- HTTP serialises the full request with `serde_json::to_vec`, optionally zstd-encodes it, and gives the resulting bytes to `reqwest`. No client-side request byte ceiling exists. Backend policy therefore owns HTTP size rejection.
- WebSocket uses the OpenAI `tokio-tungstenite` fork's default `WebSocketConfig`. That exact dependency caps one logical message at `64 << 20` bytes.
- CQ currently reuses `maxRequestBody = 10 << 20` across both transports and multiple internal consumers. A request larger than 10 MiB is rejected locally before the backend can decide.

## Design

### Separate transport contracts

Replace the shared request constant with explicit policy:

- HTTP request size is represented as unbounded at CQ's protocol layer. HTTP intake reads until EOF or request-context cancellation.
- WebSocket request size is exactly 64 MiB. Downstream and upstream connection read limits, pending-frame validation, and frame parsing use the same constant.
- Internal helpers receive the transport policy explicitly. They do not recover a hidden 10 MiB limit through protocol parsing, zstd rewriting, frozen replay, or Headroom.

### HTTP buffering and replay

CQ still needs one complete canonical request for routing and deterministic replay. HTTP intake therefore buffers the request in memory, closes it once, honours cancellation, and clears owned byte slices at existing lifecycle boundaries.

This matches Codex, which already creates the complete encoded request in memory. It does increase CQ's exposure to a malicious local process because native Codex routes are loopback-only and intentionally unauthenticated. That local trust boundary is unchanged by this fix and is not widened beyond the loopback listener.

### Compression

Identity bodies have no CQ-specific encoded or decoded byte ceiling.

Zstd bodies retain structural safety checks and single-threaded decoding, but the old fixed encoded/decoded byte ceilings cannot reject a request that Codex would send. Expansion handling must be based on representable Go sizes and decoder failures, not the removed 10 MiB transport constant. Exact original encoded bytes remain the replay source when no rewrite occurs.

### Headroom

Headroom remains fail-open. Bridge request and response framing must accept the full admitted HTTP request. Bridge failure logs one privacy-safe diagnostic and forwards the original request unchanged. Stderr capture remains separately bounded because it is diagnostic output, not request transport.

### WebSocket

CQ applies 64 MiB to complete logical messages, not individual network frames. A 64 MiB message is accepted; a message larger than 64 MiB is rejected and never dispatched. CQ keeps existing turn/account routing and upstream relay semantics.

### Account routing

Request-size parity does not change account selection. Once readiness is valid and HTTP routing reports `effective=enforce`, CQ considers all three installed Codex accounts. System-active identity is not a selector preference. Existing task affinity keeps warm tasks on their admitted account; capacity fairness chooses among accounts for eligible new tasks.

Changing system-active identity remains a separate explicit user action. It is safe only after isolated candidate validation, release installation, live readiness validation, and observed nonzero routing decisions.

## Error behaviour

- CQ no longer emits its own HTTP `413` for native Responses request size.
- Backend `413` status, headers, and body follow normal upstream relay behaviour.
- Malformed JSON, unsupported content encoding, cancellation, zstd decode failure, and WebSocket messages over 64 MiB remain local typed failures.
- Headroom failure never changes request acceptance; original encoded bytes continue upstream.

## Verification

1. Reproduce current HTTP failure with an identity request larger than 10 MiB and prove zero upstream dispatch.
2. Prove the same request reaches a fake upstream byte-exactly after the fix.
3. Repeat for zstd, model rewriting, compact, frozen replay, and Headroom fail-open paths.
4. Prove backend `413` relays unchanged.
5. Prove a 64 MiB WebSocket logical request reaches parsing and that 64 MiB plus one byte is rejected before dispatch.
6. Run focused race tests, full proxy race tests, `go vet ./...`, `go build ./...`, and diff checks.
7. Run installed Codex CLI against a separate candidate port. Do not stop, replace, or reconfigure live port `19280`.

## Non-goals

- Changing response/event/log limits.
- Authenticating native loopback routes.
- Changing quota selection or task-affinity policy.
- Switching system credentials.
- Restarting or replacing the live proxy during candidate tests.
