# Codex Request-Size Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove CQ's artificial HTTP request-size rejection while matching installed Codex's 64 MiB WebSocket logical-message limit.

**Architecture:** Keep unrelated bounded readers unchanged. Native HTTP intake buffers until EOF and passes an explicit unbounded request policy through zstd inspection, protocol parsing, frozen replay, compact handling, and Headroom. Native WebSocket paths share one 64 MiB logical-message constant across connection and frame validation.

**Tech Stack:** Go 1.26, `net/http`, Gorilla WebSocket, klauspost zstd, race-enabled Go tests, Codex CLI isolated-base-url validation.

---

## File map

- `internal/proxy/codex_request_limits.go`: transport-specific Codex request policies and helpers.
- `internal/proxy/codex_request_limits_test.go`: direct HTTP-unbounded and WebSocket-boundary policy tests.
- `internal/proxy/codex_native_http.go`: context-aware HTTP intake without a CQ byte ceiling.
- `internal/proxy/codex_zstd.go`: optional encoded, decoded, and expansion limits; zero means unbounded.
- `internal/proxy/codex_protocol.go`: protocol parsing with explicit optional size policy.
- `internal/proxy/codex_request_envelope.go`: immutable replay ownership without the removed 10 MiB ceiling.
- `internal/proxy/codex_frozen_request.go`: full admitted HTTP request inspection, transformation, and freezing.
- `internal/proxy/codex_transport.go`: full admitted HTTP model rewrite.
- `internal/proxy/codex_legacy_native_http.go`: legacy `/responses` fallback parity.
- `internal/proxy/codex_compact.go`: `/responses/compact` parity.
- `internal/proxy/headroom.go`: unbounded request/response line exchange while keeping diagnostic stderr bounded.
- `internal/proxy/codex_ws_request.go`, `codex_ws_broker.go`, `codex_live.go`, `server.go`: shared 64 MiB WebSocket limit.
- Existing focused `_test.go` files: regression coverage beside each changed production boundary.

### Task 1: Define separate request contracts

**Files:**
- Create: `internal/proxy/codex_request_limits.go`
- Create: `internal/proxy/codex_request_limits_test.go`
- Modify: `internal/proxy/server.go`

- [ ] **Step 1: Write failing transport-policy tests**

```go
func TestCodexHTTPRequestPolicyHasNoCQByteLimit(t *testing.T) {
	if codexHTTPRequestMaxBytes != 0 {
		t.Fatalf("HTTP request limit = %d, want unbounded", codexHTTPRequestMaxBytes)
	}
}

func TestCodexWebSocketPolicyMatchesInstalledClient(t *testing.T) {
	if codexWebSocketMessageMaxBytes != 64<<20 {
		t.Fatalf("WebSocket message limit = %d, want %d", codexWebSocketMessageMaxBytes, 64<<20)
	}
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run '^TestCodex(HTTP|WebSocket)RequestPolicy'`

Expected: compile failure because both policy constants are undefined.

- [ ] **Step 3: Add explicit policies**

```go
package proxy

const (
	codexHTTPRequestMaxBytes       = 0
	codexWebSocketMessageMaxBytes = 64 << 20
	codexDiagnosticLineMaxBytes    = 10 << 20
)
```

Keep `maxRequestBody` only where non-Codex or separately bounded behavior still needs it. Do not use it in native Responses request paths.

- [ ] **Step 4: Run focused tests and verify GREEN**

Run: `go test -race -count=1 ./internal/proxy -run '^TestCodex(HTTP|WebSocket)RequestPolicy'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/codex_request_limits.go internal/proxy/codex_request_limits_test.go internal/proxy/server.go
git commit -m "refactor: separated Codex request limits" -m $'- Split HTTP and WebSocket transport policies.\n- Preserved unrelated diagnostic bounds.'
```

### Task 2: Remove HTTP intake, decode, and replay ceilings

**Files:**
- Modify: `internal/proxy/codex_native_http.go`
- Modify: `internal/proxy/codex_native_http_test.go`
- Modify: `internal/proxy/codex_zstd.go`
- Modify: `internal/proxy/codex_zstd_test.go`
- Modify: `internal/proxy/codex_protocol.go`
- Modify: `internal/proxy/codex_protocol_test.go`
- Modify: `internal/proxy/codex_request_envelope.go`
- Modify: `internal/proxy/codex_request_envelope_test.go`
- Modify: `internal/proxy/codex_frozen_request.go`
- Modify: `internal/proxy/codex_frozen_request_test.go`
- Modify: `internal/proxy/codex_transport.go`
- Modify: `internal/proxy/codex_transport_test.go`
- Modify: `internal/proxy/codex_installed_http_probe_trace.go`
- Modify: `internal/proxy/codex_runtime_observability_test.go`

- [ ] **Step 1: Write failing HTTP parity tests**

Add a helper returning valid strong-turn JSON whose `input` padding makes the body `10<<20 + 1` bytes. Add tests proving:

```go
func TestReadCodexNativeHTTPRequestAcceptsBodyOverLegacyLimit(t *testing.T) {
	body := codexProtocolRequestBodyAtSize(t, 10<<20+1)
	req := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
	got, err := readCodexNativeHTTPRequest(req)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read = %d bytes, %v; want %d bytes", len(got), err, len(body))
	}
}
```

Add companion tests for identity `DecodeCodexRequest`, protocol parsing, frozen inspection/freezing, envelope replay, and model rewrite using the same over-10-MiB body. For zstd, encode a valid over-10-MiB decoded body and prove decode/re-encode succeeds under `codexHTTPZstdLimits()`.

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
go test -race -count=1 ./internal/proxy -run 'OverLegacyLimit|HTTPZstdLimits'
```

Expected: failures naming the 10 MiB native, protocol, envelope, decoded, or encoded limit.

- [ ] **Step 3: Make zstd limits optional**

Use zero as an unbounded field value. Keep negative values invalid. Avoid preallocating an unbounded capacity.

```go
func codexLimitExceeded(size, limit int) bool {
	return limit > 0 && size > limit
}

func codexZstdDecodeCapacity(encodedBytes int, limits CodexZstdLimits) int {
	const initialCap = 64 << 10
	capacity := min(max(encodedBytes, initialCap), 1<<20)
	if limits.MaxDecodedBytes > 0 {
		capacity = min(capacity, limits.MaxDecodedBytes)
	}
	return capacity
}

func codexHTTPZstdLimits() CodexZstdLimits {
	return CodexZstdLimits{}
}
```

`DecodeCodexRequest` and `EncodeCodexRequest` must:

- reject negative fields;
- apply encoded/decoded checks only when the corresponding field is positive;
- apply expansion checks only when `MaxExpansion` is positive;
- omit zstd decoder max-window/max-memory options when decoded size is unbounded;
- use `codexZstdDecodeCapacity`, never `make(..., math.MaxInt)`.

- [ ] **Step 4: Make HTTP intake and protocol parsing unbounded**

Replace the body read with cancellation-safe `io.ReadAll(bodyReader)`. Remove local HTTP `413` mapping and obsolete `errCodexNativeHTTPRequestTooLarge`.

Change protocol parsing to apply an explicit optional limit:

```go
func ParseCodexProtocolRequest(body []byte, directMetadata string, handshake *CodexTurnMetadata) (CodexProtocolRequest, error) {
	return parseCodexProtocolRequest(body, directMetadata, handshake, codexHTTPRequestMaxBytes)
}

func parseCodexProtocolRequest(body []byte, directMetadata string, handshake *CodexTurnMetadata, maxBytes int) (CodexProtocolRequest, error) {
	if codexLimitExceeded(len(body), maxBytes) {
		return CodexProtocolRequest{}, errors.New("Codex protocol request exceeds limit")
	}
	// Existing metadata and JSON parsing, using maxBytes when positive and len(body) otherwise.
}
```

- [ ] **Step 5: Remove replay and transform ceilings**

Remove fixed-size rejection from `CodexRequestEnvelope`, frozen inspection, Headroom result acceptance, model splice, installed replay digest, and transport rewrite. Retain integer-overflow checks and lifecycle accounting. Pass `codexHTTPZstdLimits()` through the HTTP freeze/rewrite path. Update aggregate replay ownership proof so a large envelope acquires and releases its exact byte count.

- [ ] **Step 6: Run focused tests and verify GREEN**

Run:

```bash
go test -race -count=1 ./internal/proxy -run 'OverLegacyLimit|HTTPZstdLimits|CodexFrozenRequest|CodexRequestEnvelope|CodexTransport'
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/proxy/codex_native_http.go internal/proxy/codex_native_http_test.go internal/proxy/codex_zstd.go internal/proxy/codex_zstd_test.go internal/proxy/codex_protocol.go internal/proxy/codex_protocol_test.go internal/proxy/codex_request_envelope.go internal/proxy/codex_request_envelope_test.go internal/proxy/codex_frozen_request.go internal/proxy/codex_frozen_request_test.go internal/proxy/codex_transport.go internal/proxy/codex_transport_test.go internal/proxy/codex_installed_http_probe_trace.go internal/proxy/codex_runtime_observability_test.go
git commit -m "fix: removed native HTTP size ceiling" -m $'- Accepted full Codex HTTP request envelopes.\n- Preserved cancellation, codec validation, and replay ownership.\n- Avoided unbounded decoder preallocation.'
```

### Task 3: Make legacy and compact HTTP routes exact relays

**Files:**
- Modify: `internal/proxy/codex_legacy_native_http.go`
- Modify: `internal/proxy/codex_legacy_native_http_test.go`
- Modify: `internal/proxy/codex_compact.go`
- Modify: `internal/proxy/codex_compact_test.go`
- Modify: `internal/proxy/server_test.go`

- [ ] **Step 1: Write failing route tests**

Create fake upstream handlers that read the complete request and record status/body. Send valid identity and zstd bodies larger than 10 MiB through both legacy native and compact handlers.

```go
if gotStatus != http.StatusCreated || !bytes.Equal(gotUpstreamBody, wantBody) {
	t.Fatalf("relay status/body = %d/%d, want %d/%d", gotStatus, len(gotUpstreamBody), http.StatusCreated, len(wantBody))
}
```

Add a fake upstream returning `413`, a distinguishing response header, and JSON body. Assert CQ relays all three unchanged.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'LegacyNativeHTTP.*OverLegacyLimit|Compact.*OverLegacyLimit|RelaysUpstreamPayloadTooLarge'`

Expected: local `413` and zero fake-upstream calls.

- [ ] **Step 3: Use shared HTTP intake and HTTP zstd policy**

Replace each legacy `LimitReader` block with `readCodexNativeHTTPRequest`. Decode and re-encode with `codexHTTPZstdLimits()`. Remove local size-specific error text while keeping malformed-body and unsupported-encoding failures.

- [ ] **Step 4: Run route tests and verify GREEN**

Run: `go test -race -count=1 ./internal/proxy -run 'LegacyNativeHTTP.*OverLegacyLimit|Compact.*OverLegacyLimit|RelaysUpstreamPayloadTooLarge'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/codex_legacy_native_http.go internal/proxy/codex_legacy_native_http_test.go internal/proxy/codex_compact.go internal/proxy/codex_compact_test.go internal/proxy/server_test.go
git commit -m "fix: relayed large Codex HTTP requests" -m $'- Applied one request contract to native and compact routes.\n- Preserved upstream payload-too-large responses.'
```

### Task 4: Make Headroom preserve admitted HTTP requests

**Files:**
- Modify: `internal/proxy/headroom.go`
- Modify: `internal/proxy/headroom_test.go`
- Modify: `internal/proxy/codex_frozen_request_test.go`

- [ ] **Step 1: Write failing Headroom tests**

Send a JSON-line bridge response larger than 10 MiB and prove it is returned. Add a failure response from the bridge for an over-10-MiB request and prove the frozen request forwards original encoded bytes with one privacy-safe error and no local `413`.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'Headroom.*OverLegacyLimit|FrozenRequest.*HeadroomFailOpen'`

Expected: `bufio.Scanner: token too long` or transformed-request limit failure.

- [ ] **Step 3: Replace request transport scanner**

Store `stdout` as `*bufio.Reader` and read exactly one newline-delimited response:

```go
response, err := b.stdout.ReadBytes('\n')
if err != nil {
	operationErr = fmt.Errorf("read from bridge: %w", err)
} else {
	response = bytes.TrimSuffix(response, []byte{'\n'})
}
```

Keep stderr on a scanner with `codexDiagnosticLineMaxBytes`; stderr is diagnostic output and must not allocate without bound. Preserve operation serialisation, cancellation shutdown, and response validation.

Make the live adapter fail open before `Freeze` sees a transform failure. Keep cancellation and deadline errors terminal; replace other bridge errors with unchanged owned input, zero savings, and one fixed diagnostic:

```go
transformed, saved, err := adapter.bridge.CompressResponsesContext(ctx, body, mode)
if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
	return transformed, saved, err
}
fmt.Fprintln(os.Stderr, "cq: headroom: compression unavailable")
return bytes.Clone(body), 0, nil
```

Do not include bridge error text because external stderr can contain private data. Do not freeze partial bridge output.

- [ ] **Step 4: Run Headroom tests and verify GREEN**

Run: `go test -race -count=1 ./internal/proxy -run 'Headroom.*OverLegacyLimit|FrozenRequest.*HeadroomFailOpen|HeadroomBridge'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/headroom.go internal/proxy/headroom_test.go internal/proxy/codex_frozen_request_test.go
git commit -m "fix: preserved large Headroom requests" -m $'- Removed request-line truncation from bridge exchange.\n- Kept diagnostic stderr bounded and fail-open forwarding intact.'
```

### Task 5: Match installed Codex WebSocket size policy

**Files:**
- Modify: `internal/proxy/codex_ws_request.go`
- Modify: `internal/proxy/codex_ws_request_test.go`
- Modify: `internal/proxy/codex_ws_broker.go`
- Modify: `internal/proxy/codex_ws_relay_test.go`
- Modify: `internal/proxy/codex_live.go`
- Modify: `internal/proxy/codex_live_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`

- [ ] **Step 1: Write failing logical-message boundary tests**

Build valid `response.create` JSON padded to exactly `codexWebSocketMessageMaxBytes` and one byte larger. Assert exact-limit acceptance and over-limit `ErrCodexWSInvalidFrame`. Add broker/relay tests proving every Gorilla connection receives the shared 64 MiB read limit through observed exact/over-limit behavior.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'WebSocket.*MessageLimit|WSPendingFrame.*Limit'`

Expected: exact 64 MiB request rejected by old 10 MiB checks.

- [ ] **Step 3: Replace every Codex WebSocket request limit**

Use `codexWebSocketMessageMaxBytes` in `newCodexWSPendingFrame` and all downstream/upstream `SetReadLimit` calls. Parse WS requests with the explicit 64 MiB limit so HTTP remains unbounded.

- [ ] **Step 4: Run WS tests and verify GREEN**

Run: `go test -race -count=1 ./internal/proxy -run 'WebSocket.*MessageLimit|WSPendingFrame.*Limit|CodexWS'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/codex_ws_request.go internal/proxy/codex_ws_request_test.go internal/proxy/codex_ws_broker.go internal/proxy/codex_ws_relay_test.go internal/proxy/codex_live.go internal/proxy/codex_live_test.go internal/proxy/server.go internal/proxy/server_test.go
git commit -m "fix: matched Codex WebSocket limits" -m $'- Accepted logical messages through 64 MiB.\n- Rejected larger messages before account dispatch.'
```

### Task 6: Verify candidate without disrupting live work

**Files:**
- Modify only if verification exposes a defect; return to the matching task's RED→GREEN cycle.

- [ ] **Step 1: Run focused race suite**

```bash
go test -race -count=1 ./internal/proxy -run 'Codex.*(Request|HTTP|Compact|Zstd|Frozen|Envelope|Transport|WebSocket)|Headroom'
```

Expected: PASS.

- [ ] **Step 2: Run full repository gates**

```bash
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 3: Prove large-byte relay against an isolated fake upstream**

Start a loopback fake upstream that records request bytes and returns a distinguishing response. Start CQ candidate on a free non-`19280` port with its Codex upstream set to that fake. Send a valid request larger than 10 MiB to candidate and verify byte-exact upstream receipt and unchanged response relay.

- [ ] **Step 4: Start isolated real-upstream candidate**

Choose a free loopback port other than `19280`, generate isolated state/config directories with `mktemp -d`, and launch the candidate in a persistent terminal session. Do not kill, signal, restart, unload, or rewrite the live service.

- [ ] **Step 5: Run installed Codex CLI against candidate only**

Set Codex's base URL only in that process environment and run one normal short task. Verify candidate logs one routed upstream dispatch and Codex receives the response. Never change global Codex configuration or system credentials. The fake-upstream test owns byte-exact large-body proof so installed validation does not spend a huge real request.

- [ ] **Step 6: Stop candidate and clean temporary state**

Terminate only the recorded candidate PID/session. Move exact temporary directories to Trash and verify they no longer exist. Confirm live port `19280` PID and `/health` response stayed unchanged throughout.

- [ ] **Step 7: Run final diff review**

```bash
git status --short
git diff origin/main...HEAD --stat
git diff origin/main...HEAD --check
```

Expected: only planned source, tests, spec, and plan changes.

- [ ] **Step 8: Confirm no uncommitted verification edits**

Run: `git status --short`

Expected: clean worktree. If installed verification exposes a defect, return to the owning task, capture a focused RED, implement the minimal fix, rerun that task's gates, and commit only its listed files before repeating Task 6.

## Self-review

- Spec coverage: HTTP, compact, zstd, replay, Headroom, upstream `413`, WebSocket, isolated candidate, account-switch gate all map to tasks.
- Placeholder scan: no `TBD`, deferred implementation, or unspecified test action remains.
- Type consistency: `codexHTTPRequestMaxBytes`, `codexWebSocketMessageMaxBytes`, `codexDiagnosticLineMaxBytes`, `codexHTTPZstdLimits`, and `codexLimitExceeded` use one spelling throughout.
