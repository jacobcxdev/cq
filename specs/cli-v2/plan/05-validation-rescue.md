# CQ CLI v2 Validation and Rescue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adapt retained/active validation, canary, hook, rescue and operation inspection.

**Architecture:** Use the shared interfaces and staged registry from the [coordinator](../plan.md). Adapt existing domain engines; introduce only the seams required by the canonical contract. Do not expose a partial public CLI.

**Tech Stack:** Go 1.26.1, existing CQ packages, standard library, Python 3 build-time catalogue generation.

**Spec:** [Canonical contract](../README.md), [command catalogue](../commands.json), [acceptance cases](../acceptance.md), [resource schemas](../resources/root.md), [environment](../environment.md), [migration](../migration.md), [machine ABI](../internal-abi.md).

## Global Constraints

- “Every intermediate group prints help on bare invocation, exits 0 and reads no state.”
- “No mutation silently chooses Codex because the user happens to use Codex most often.”
- “They never grant consent.” JSON/nonterminal input must not authorise a mutation.
- “Remove aliases no earlier than 2.0.0.” Target CQ1.0.0, CLI/envelope schema2.
- One total operation budget includes cleanup; terminal consent waiting alone is excluded.
- Every Go test uses `-race`; preserve exact resource schemas, messages, human templates, machine protocols and frozen algorithms.
- All [coordinator constraints and interfaces](../plan.md#shared-interfaces-fixed-before-parallel-work) apply. Source paths below are relative to the T00 implementation baseline; proposed files are explicitly marked Create.
- Planning and local hermetic verification do not authorise installed service changes, reset consumption, publication or remote writes.

---

## T24 — Adapt fixture, readiness and transport validation

**Dependencies:** T01, T05, T17, T18.

**Files:** Create `cmd/cq/cli_v2_validation.go`, `cmd/cq/cli_v2_validation_test.go`. Modify `cmd/cq/codex_validate.go`, `cmd/cq/proxy_http_validation_cli.go`, `cmd/cq/proxy_http_validation_request_test.go` and platform proxy_http_validation_service adapters/tests. Retain `internal/proxy/codex_fixture.go`, `internal/proxy/codex_turn_metadata.go`, `internal/proxy/codex_zstd.go`, `internal/proxy/codex_readiness.go`, `internal/proxy/codex_installed_ws_validation.go` engines with explicit spec deltas only.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Validation(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume BuildSanitisedCodexFixture/WriteSanitisedCodexFixture, ValidateCodexReadinessMarker, runProxyValidateHTTP and RunCodexInstalledWebSocketValidation with caller budget. HTTP accepted response is not a completion receipt.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy fixture create` | None | `--input`, `--output`, `--content-encoding`, `--metadata-json` | [Help](../help/codex-proxy-fixture-create.txt) |
| `codex proxy readiness show` | None | `--client-build`, `--state-dir` | [Help](../help/codex-proxy-readiness-show.txt) |
| `codex proxy validate http` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-validate-http.txt) |
| `codex proxy validate websocket` | None | `--client-build`, `--client-executable`, `--state-dir`, `--timeout` | [Help](../help/codex-proxy-validate-websocket.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** fixture-output-exists fixture has a valid sanitised-input candidate and existing output with sentinel bytes. readiness-missing fixture has no retained marker. validation-http-accepted uses synthetic attested candidate/service binding and records an accepted request only. WS fixture uses isolated synthetic upstream/listener and injected cleanup.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ValidationContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "fixture cannot overwrite existing file",
        Scenario: "fixture-output-exists",
        Args: []string{"codex", "proxy", "fixture", "create", "--input", "request.json", "--output", "fixture.json", "--json"},
        Exit: 6,
        Command: "codex proxy fixture create",
        Code: "fixture_output_exists",
        Forbid: []string{"filesystem-write", "service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Validation|.*CodexFixture|.*TurnMetadata|.*Zstd|.*Readiness|ProxyHTTPValidation|CodexInstalledWebSocketValidation)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Enforce fixture encoded2MiB, decoded8MiB and expansion128 bounds, duplicate/unknown/trailing metadata rejection, representation-conflict checks and invalid UTF-8 rejection. Publish atomically without clobbering existing output or aliasing input. Scan generated fixture for synthetic secrets/raw IDs.

- [ ] Change 2: Readiness reports retained compatible evidence only. Exact client-build matching, stale/missing/malformed marker errors and state-dir resolution remain read-only; do not infer live health.

- [ ] Change 3: HTTP validation rejects production port19280 before state/manager access, attests candidate identity before restart and invalidates/cancels on failure. Return accepted with validation_complete=false; no invented polling ID or claim that an old marker proves this request passed.

- [ ] Change 4: WebSocket validation executes existing isolated exercise with one total budget including preparation and5s reserved cleanup. Propagate cancellation, verify executable/build binding, invalidate stale success marker on failure and never restart production listener.

- [ ] Change 5: Test boundary±1 sizes/ratio, overwrite, metadata conflicts, attestation race, accepted-versus-complete output, cleanup budget exhaustion, failed client version and interruption. Keep installed acceptance separate and opt-in.

**Implementation sequence:**

```text
1. Enforce fixture encoded2MiB, decoded8MiB and expansion128 bounds, duplicate/unknown/trailing metadata rejection, representation-conflict checks and invalid UTF-8 rejection.
2. Readiness reports retained compatible evidence only.
3. HTTP validation rejects production port19280 before state/manager access, attests candidate identity before restart and invalidates/cancels on failure.
4. WebSocket validation executes existing isolated exercise with one total budget including preparation and5s reserved cleanup.
5. Test boundary±1 sizes/ratio, overwrite, metadata conflicts, attestation race, accepted-versus-complete output, cleanup budget exhaustion, failed client version and interruption.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified transport validation results` and a concise unordered body describing significant changes. Do not push or merge.

## T25 — Adapt canary lifecycle and preserve Stop hook protocol

**Dependencies:** T01, T05, T15, T24.

**Files:** Create `cmd/cq/cli_v2_canary.go`, `cmd/cq/cli_v2_canary_test.go`, `cmd/cq/cli_v2_hook.go`, `cmd/cq/cli_v2_hook_test.go`. Modify `cmd/cq/codex_canary.go`, `cmd/cq/codex_canary_protection.go`, `cmd/cq/proxy_codex_hook.go`; retain `internal/proxy/codex_canary.go`, `internal/proxy/codex_canary_stop.go` state/protection algorithms and matching tests.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Canary(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Also produces `handleV2Hook(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`. Hook raw protocol renderer uses real JSON encoding; with explicit --json it returns canonical envelope. T01 retains old machine hook grammar/output without stdout deprecation.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy canary start` | None | None | [Help](../help/codex-proxy-canary-start.txt) |
| `codex proxy canary status` | None | None | [Help](../help/codex-proxy-canary-status.txt) |
| `codex proxy canary stop` | None | None | [Help](../help/codex-proxy-canary-stop.txt) |
| `codex proxy hook stop` | None | None | [Help](../help/codex-proxy-hook-stop.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** canary-missing fixture has no retained run. hook-unicode fixture has a valid receipt for a pool named `Research Ω; "night"`, with raw session/turn IDs exactly4096 UTF-8 bytes; extra event fields contain a secret sentinel that must not appear in output. hook-missing-receipt emits raw {} without --json.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2CanaryContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "missing canary is explicit",
        Scenario: "canary-missing",
        Args: []string{"codex", "proxy", "canary", "status", "--json"},
        Exit: 3,
        Command: "codex proxy canary status",
        Code: "canary_missing",
        Forbid: []string{"service", "consume", "filesystem-write"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Canary|CLIV2Hook|CodexCanary|ProxyCodex.*(Canary|Hook)|.*CanaryStop)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Canary start validates retained readiness and protected source identity without attaching/restarting the service. Preserve active-run conflicts, stop request versus finalisation distinction and repeated stop idempotence.

- [ ] Change 2: Hook strictly validates required event fields, duplicates and UTF-8 byte1..4096 selectors. Ignore unrelated input without echoing it. Keep missing receipt raw {} and frozen default JSON protocol; explicit --json wraps the manual result only.

- [ ] Change 3: Remove legacy ASCII pool regex; quote the pool name as an inner JSON string in the message, then JSON-encode the outer protocol. Preserve Unicode/spaces/quotes/semicolons/control escapes literally without shell execution or narrowing PoolName.

- [ ] Change 4: Test active/stopped/unfinalised canaries, source drift, pending stop, duplicate keys, byte4096/4097, missing receipt, malformed event and raw-versus-envelope/deprecation behaviour. No hook response may expose fixture credentials.

**Implementation sequence:**

```text
1. Canary start validates retained readiness and protected source identity without attaching/restarting the service.
2. Hook strictly validates required event fields, duplicates and UTF-8 byte1..4096 selectors.
3. Remove legacy ASCII pool regex; quote the pool name as an inner JSON string in the message, then JSON-encode the outer protocol.
4. Test active/stopped/unfinalised canaries, source drift, pending stop, duplicate keys, byte4096/4097, missing receipt, malformed event and raw-versus-envelope/deprecation behaviour.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: adapted canary and hook contracts` and a concise unordered body describing significant changes. Do not push or merge.

## T26 — Adapt rescue controls and read-only operation inspection

**Dependencies:** T01, T05, T17.

**Files:** Create `cmd/cq/cli_v2_rescue.go`, `cmd/cq/cli_v2_rescue_test.go`. Modify `cmd/cq/proxy_rescue.go`, `cmd/cq/proxy_operation.go`, `cmd/cq/proxy_commands.go` typed seams; preserve `internal/proxy/runtime_supervisor.go`, `internal/proxy/runtime_rescue_control_test.go` transition/operation engines.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Rescue(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Handles rescue and operation inspection only. Reuse runProxyRescueWithDependencies, EnterRescue/ExitRescue, defaultInspectOperatorOperation; operation recover retirement belongs to T03 before any handler.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `proxy operation status` | `operation-id` | None | [Help](../help/proxy-operation-status.txt) |
| `proxy rescue enter` | None | `--port`, `--timeout` | [Help](../help/proxy-rescue-enter.txt) |
| `proxy rescue exit` | None | `--port`, `--timeout` | [Help](../help/proxy-rescue-exit.txt) |
| `proxy rescue status` | None | `--port`, `--timeout` | [Help](../help/proxy-rescue-status.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** operation-pending fixture contains a valid retained intent and no final receipt. operation-failed-receipt contains an inspectable completed failure. rescue-timeout injects accepted transition then lost response; no automatic retry may reverse or duplicate it.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2RescueContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "pending operation is successful inspection",
        Scenario: "operation-pending",
        Args: []string{"proxy", "operation", "status", "--json"},
        Exit: 0,
        Command: "proxy operation status",
        WantJSON: `{"state":"pending","result_available":false,"recovery_supported":false}`,
        Forbid: []string{"filesystem-write", "service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Rescue|ProxyRescue|ProxyOperation|.*RuntimeRescue)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Normalise enter/exit/status through authenticated shared rescue control with exact default30s total budget. Preserve durable operation, drain state, idempotency and transition conflict semantics; malformed/oversized reply is not success.

- [ ] Change 2: Operation status is read-only and reports idle/pending/result_available independently of retained action success. An explicit missing record returns3; pending inspection returns0 with recovery_supported=false.

- [ ] Change 3: Retired recover produces the specified unsupported exit4 even when a receipt exists; no recovery engine, mutation, retry or implicit cleanup. Keep old hidden ABI cases fenced by T01.

- [ ] Change 4: Test every rescue mode, repeated transition, drain, auth/timeout/malformed response and no retry after ambiguity; test idle/pending/failed action receipt, missing selected ID and immutable state inventory.

**Implementation sequence:**

```text
1. Normalise enter/exit/status through authenticated shared rescue control with exact default30s total budget.
2. Operation status is read-only and reports idle/pending/result_available independently of retained action success.
3. Retired recover produces the specified unsupported exit4 even when a receipt exists; no recovery engine, mutation, retry or implicit cleanup.
4. Test every rescue mode, repeated transition, drain, auth/timeout/malformed response and no retry after ambiguity; test idle/pending/failed action receipt, missing selected ID and immutable state inventory.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified rescue operation inspection` and a concise unordered body describing significant changes. Do not push or merge.
