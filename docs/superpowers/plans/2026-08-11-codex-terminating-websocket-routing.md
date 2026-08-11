# Codex Terminating WebSocket Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete Stage 13 and Stage 14 with a frame-aware CQ WebSocket broker that durably routes each turn, safely replaces provisional upstream sockets behind one downstream socket, and never migrates admitted or generation-bound state.

**Architecture:** CQ terminates the downstream Codex WebSocket first. It owns one bounded `response.create` until route, lease, and upstream generation authority are committed; then it forwards that frame to one account-bound upstream. Definite pre-admission auth/hard-quota failures may rotate only a portable never-admitted frame. Admission destroys replay ownership and makes account/generation immutable.

**Tech Stack:** Go 1.26, Gorilla WebSocket, existing schema-v3 lease runtime, frozen request inspection/dispatch policy, race detector, real Codex CLI custom provider on isolated port only.

---

## File structure

- `internal/proxy/codex_responses_ws.go`: bounded frame parsing, portability, upstream event/error classification, existing rotation primitives.
- `internal/proxy/codex_ws_request.go`: one pending-frame owner and durable request lifecycle adapter.
- `internal/proxy/codex_ws_broker.go`: terminating downstream/upstream state machine and generation fencing.
- `internal/proxy/codex_ws_relay.go`: connection I/O primitives only; no route selection.
- `internal/proxy/server.go`: upgrade, dependency selection, diagnostics delegation.
- `internal/proxy/codex_readiness.go`: exact WS gate tuple and effective-mode inhibition.
- `cmd/cq/codex_validate.go`: isolated installed WS acceptance trigger only.
- Matching `_test.go` files: RED/GREEN behaviour, race, privacy, and installed fixtures.

### Task 1: Frame authority and bounded replay ownership

**Files:**
- Modify: `internal/proxy/codex_responses_ws.go`
- Create: `internal/proxy/codex_ws_request.go`
- Create: `internal/proxy/codex_ws_request_test.go`
- Modify: `internal/proxy/codex_resync_test.go`

- [x] **Step 1: Write failing frame-authority tests**

Add `TestCodexWSPendingFrameUsesStrongFrameAuthorityWithoutHandshake`, `TestCodexWSPendingFrameRejectsOversizeAndNonResponseCreate`, and `TestCodexWSPendingFrameClassifiesPortability`. Assert strong nested metadata and model produce canonical lane/lease identity; `previous_response_id`, encrypted state, missing strong metadata, and unknown request kind are non-portable/fail closed.

- [x] **Step 2: Run RED**

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexWSPendingFrame'
```

Expected: compile failure for missing `newCodexWSPendingFrame` and `Release`.

- [x] **Step 3: Implement minimal owner**

Implement package-private ownership:

```go
type codexWSPendingFrame struct {
    messageType int
    encoded     []byte
    request     CodexProtocolRequest
    key         LeaseKey
    portable    bool
    released    bool
}

func newCodexWSPendingFrame(messageType int, frame []byte) (*codexWSPendingFrame, error)
func (frame *codexWSPendingFrame) Release()
```

Copy at most `maxRequestBody`, require text `response.create`, strong valid metadata, non-empty model, and exact lease key. `portable` requires no `previous_response_id`, no encrypted content, and no generation-bound turn state. `Release` is idempotent and clears owned bytes/references.

- [x] **Step 4: Run GREEN and privacy scan**

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexWSPendingFrame'
rg -n 'pending.*(journal|persist)|frame.*(journal|persist)' internal/proxy
```

Expected: tests pass; no persistence call owns frame bytes.

### Task 2: WebSocket admission lifecycle over schema-v3 leases

**Files:**
- Modify: `internal/proxy/codex_lease_v2_runtime.go`
- Modify: `internal/proxy/codex_lease_v2_runtime_test.go`
- Create: `internal/proxy/codex_ws_lifecycle.go`
- Create: `internal/proxy/codex_ws_lifecycle_test.go`

- [x] **Step 1: Write failing lifecycle tests**

Add tests proving: prepared/dispatched frame remains provisional; upstream 101 without turn state does not admit; stateful 101 or valid `response.created` admits synchronously; exact error before admission can reject/prepare next slot; malformed/ambiguous event becomes indeterminate; later hard 429 on admitted turn cannot change account; stale upstream generation cannot mutate journal.

- [x] **Step 2: Run RED**

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexWSLifecycle'
```

Expected: compile failure for missing `codexWSLifecycle`/`AdmitWebSocketContext`.

- [x] **Step 3: Add transport-neutral admission mutation**

Add exact evidence type and method while sharing existing durable mutation code:

```go
type CodexWebSocketAdmissionEvidence struct {
    UpstreamGeneration uint64
    TurnState           string
    ResponseID          string
}

func (handle *CodexLeaseRequestHandle) AdmitWebSocketContext(
    ctx context.Context,
    evidence CodexWebSocketAdmissionEvidence,
) (*CodexLeaseRequestHandle, error)
```

Validate generation and durable identity before mutation. Preserve first-admission monotonicity, cache-affinity evidence, `NonMigratable`, and native admission sink semantics. Never treat downstream 101 alone as admission.

- [x] **Step 4: Implement lifecycle adapter and run GREEN**

`codexWSLifecycle` owns one `CodexLeaseRequestHandle`, selected slot, and upstream generation. It maps valid `response.created`, completion, terminal error, close, cancellation, and rate-limit events to existing durable transitions.

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexWSLifecycle|^TestCodexLease.*WebSocket'
```

### Task 3: Terminating broker and safe upstream rotation

**Files:**
- Create: `internal/proxy/codex_ws_broker.go`
- Create: `internal/proxy/codex_ws_broker_test.go`
- Modify: `internal/proxy/codex_ws_relay.go`
- Modify: `internal/proxy/codex_ws_relay_test.go`

- [x] **Step 1: Write failing broker tests**

Use fake downstream/upstream connections and dial authority. Cover:

- first frame selects account from durable lane after downstream upgrade;
- same-turn repeated sampling reuses admitted account/upstream;
- successor turn reuses warm account before 30-minute floor;
- successor hard 429 before admission closes A, opens B, and forwards identical frame once to B;
- 401/403 exhaust same-account candidates before terminal translation;
- incremental/encrypted/admitted/ambiguous request never rotates;
- one active request only; changed turn while active returns typed concurrency error;
- late old-generation event cannot admit, complete, update capacity, or write downstream;
- cancellation joins reader/writer and releases pending frame.

- [x] **Step 2: Run RED**

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexTerminatingWSBroker'
```

Expected: compile failure for missing broker/config/run methods.

- [x] **Step 3: Implement minimal broker interfaces**

```go
type codexWSUpstreamDialer interface {
    Dial(context.Context, RouteChoice, CandidateAttempt, string, http.Header) (*websocket.Conn, *http.Response, []byte, error)
}

type codexTerminatingWSBrokerConfig struct {
    Plans      *CodexHTTPRequestPlanFactory
    Executor   ExplicitWebSocketExecutor
    UpstreamURL string
    Headers    http.Header
}

func (broker *codexTerminatingWSBroker) Serve(
    ctx context.Context,
    downstream websocketRelayConn,
) error
```

Use one downstream reader, one serial downstream writer, and at most one active upstream reader generation. Retain only one pending frame. Rotate upstream only while lifecycle proves definite pre-admission and frame proves portable. Close and join old generation before installing next.

- [x] **Step 4: Add canonical terminal translation**

Translate bounded upstream handshake response/body to Codex `type:error` event with exact status/status_code and nested safe error fields, then send fixture-proven close code. Strip credentials, cookies, hop-by-hop, arbitrary panic/error text, and bodies over limit.

- [x] **Step 5: Run GREEN and stress**

```bash
go test -race -count=1 ./internal/proxy -run '^TestCodexTerminatingWSBroker'
go test -race -count=100 ./internal/proxy -run '^TestCodexTerminatingWSBroker.*(Rotate|Generation|Concurrent|Cancel)'
```

### Task 4: Production wiring and readiness gate

**Files:**
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`
- Modify: `internal/proxy/codex_readiness.go`
- Modify: `internal/proxy/codex_readiness_test.go`
- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/codex_validate.go`
- Modify: `cmd/cq/codex_validate_test.go`

- [x] **Step 1: Write failing production-boundary tests**

Prove effective WS `enforce` delegates to terminating broker after downstream upgrade; `observe/off` preserve legacy relay; missing/invalid marker inhibits enforcement; generic `1011 upstream error` path is unreachable under enforce; no automatic credential/admin/system mutation call occurs.

- [x] **Step 2: Run RED**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Codex.*WebSocket.*(Enforce|Readiness|Authority)'
```

- [x] **Step 3: Wire private broker authority**

Construct broker from existing inventory, exact secret resolver, capacity ledger, schema-v3 route snapshot/runtime, default account, and installed build tuple. Keep API package-private. `server.go` owns upgrade/diagnostics only.

- [x] **Step 4: Advance WS readiness tuple**

Require exact gates for frame authority, portability, admission, auth recovery, hard-429 rotation, terminal translation, generation fencing, compression/subprotocol, privacy, and installed CLI acceptance. Marker creation remains explicit and isolated; ordinary startup cannot mint it.

- [x] **Step 5: Run GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Codex.*WebSocket.*(Enforce|Readiness|Authority)'
```

### Task 5: Installed isolated acceptance and final promotion audit

**Files:**
- Modify: `internal/proxy/codex_installed_acceptance_harness.go`
- Modify: `internal/proxy/codex_installed_acceptance_harness_test.go`
- Modify: `docs/superpowers/plans/2026-08-08-codex-turn-aware-routing.md`
- Create: `docs/superpowers/evidence/2026-08-11-codex-websocket-enforcement.json`

- [x] **Step 1: Add deterministic broker and installed-harness cases**

Synthetic upstream and broker tests prove successor warm affinity, exact hard-429 pre-admission rotation, admitted hard-429 non-migration, same-account auth recovery, terminal errors, subprotocol/compression, zero late-generation mutation, and bounded frame ownership. Installed Codex CLI proves dynamic strong-frame routing and exact PONG against isolated candidate.

- [x] **Step 2: Run focused gates**

```bash
go test -race -count=100 ./internal/proxy -run 'TestCodexTerminatingWSBroker|TestCodexWSLifecycle|TestCodexWSPendingFrame|TestServerCodexWebSocketEnforce'
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Codex.*WebSocket|Proxy.*WebSocket'
```

- [x] **Step 3: Run full repository gates**

```bash
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

- [x] **Step 4: Run real Codex CLI only against isolated candidate**

Use explicit custom-provider base URL on unused candidate port. Record live proxy PID/listener before and after; they must remain byte-for-byte unchanged. Do not kill, restart, rebind, bootout, or repair live port `19280`.

- [x] **Step 5: Promote and audit all 15 stages**

Write WS marker only after exact isolated evidence passes. Re-read every stage checkbox and named gate against current source/test/runtime evidence. Keep unchecked anything missing. Update concise report, verify clean worktree after local commits, and do not push or open a PR.
