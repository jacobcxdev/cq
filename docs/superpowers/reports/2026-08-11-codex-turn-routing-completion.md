# Codex turn-aware routing completion

## Outcome

All 15 migration stages are implemented and locally verified. HTTP and WebSocket routing share durable schema-3 lane authority keyed by `(session_id, thread_id, "codex-responses")`. Same-turn admission is immutable. Successor turns retain task affinity through the GPT-5.6 cache floor, then become fairness-eligible. Only a definite hard quota before admission may rotate a portable request.

## WebSocket design

CQ terminates downstream WebSocket locally. Downstream `101` is transport acceptance only. CQ reads one bounded strong `response.create`, acquires durable account/turn authority, then opens or reuses an account-bound upstream. Provider admission is recorded only from stateful upstream authority or `response.created`. Admitted, incremental, encrypted, ambiguous, or generation-bound requests never cross accounts.

## Installed evidence

Installed Homebrew Codex CLI `0.146.0` used an explicit custom provider against random isolated loopback ports and returned exact `PONG`. Candidate path produced two downstream WebSocket connections, nonzero routed requests/upstream dials, zero unexpected routes, and zero egress attempts. Secure temporary readiness marker was deleted after proof. Live proxy stayed PID `41342` on `127.0.0.1:19280` before and after.

## Verification

- Focused broker/lifecycle/server race soak: 100/100 passed in 46.467 seconds.
- Full `go test -race -count=1 ./...`: passed; proxy package 175.456 seconds.
- `go vet ./...`, `go build ./...`, and `git diff --check`: passed.
- Exact installed CLI SHA-256: `ae1d3ffe6d48aec6a4dc3f50e7eb8e0d11962485a6a9406c5a7012139383da02`.

## Operational boundary

No live cutover occurred. No live proxy stop/restart/rebind occurred. No system Codex auth, registry, global active-account state, or user credential changed. Enforcement activation remains explicit: persist current WS marker, set configured WS mode to `enforce`, then start selected build through user-approved rollout tooling.
