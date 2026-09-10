# CQ CLI v2 Codex Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Place provider routing controls under explicit providers and preserve routing authority.

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

## T13 — Separate routing selections from account activation

**Dependencies:** T05, T07.

**Files:** Create `cmd/cq/cli_v2_selections.go`, `cmd/cq/cli_v2_selections_test.go`. Modify `cmd/cq/proxy.go` routing helper seams and `cmd/cq/proxy_codex_default.go`. Retain `internal/proxy/codex_route_policy.go`, `internal/proxy/codex_primer.go` algorithms. Extend `cmd/cq/proxy_pin_test.go`, `cmd/cq/proxy_codex_default_test.go`, `cmd/cq/proxy_prime_test.go`, `cmd/cq/proxy_primer_runtime_test.go`.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Selection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume inv.LegacySelector only for legacy Claude UUID resolution. Call existing runProxyPinWithDependencies/runProxyCodexDefaultWithDependencies/reloadProxyConfig through extracted typed intent; do not rebuild argv.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `claude proxy pin clear` | None | `--timeout` | [Help](../help/claude-proxy-pin-clear.txt) |
| `claude proxy pin set` | `account` | `--timeout` | [Help](../help/claude-proxy-pin-set.txt) |
| `claude proxy pin show` | None | `--timeout` | [Help](../help/claude-proxy-pin-show.txt) |
| `codex proxy fallback clear` | None | `--timeout` | [Help](../help/codex-proxy-fallback-clear.txt) |
| `codex proxy fallback set` | `account` | `--timeout` | [Help](../help/codex-proxy-fallback-set.txt) |
| `codex proxy fallback show` | None | `--timeout` | [Help](../help/codex-proxy-fallback-show.txt) |
| `codex proxy pin clear` | None | `--timeout` | [Help](../help/codex-proxy-pin-clear.txt) |
| `codex proxy pin set` | `account` | `--timeout` | [Help](../help/codex-proxy-pin-set.txt) |
| `codex proxy pin show` | None | `--timeout` | [Help](../help/codex-proxy-pin-show.txt) |
| `codex proxy prime disable` | None | `--timeout` | [Help](../help/codex-proxy-prime-disable.txt) |
| `codex proxy prime enable` | None | `--timeout` | [Help](../help/codex-proxy-prime-enable.txt) |
| `codex proxy prime status` | None | `--timeout` | [Help](../help/codex-proxy-prime-status.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** pin-ambiguous fixture supplies two Codex accounts sharing an email, with an existing proxy config containing unknown future fields. prime-enable fixture has disabled prime and a request-counting upstream; enable changes config without priming immediately.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2SelectionContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "ambiguous routing target does not write",
        Scenario: "pin-ambiguous",
        Args: []string{"codex", "proxy", "pin", "set", "alice@example.com", "--json"},
        Exit: 6,
        Command: "codex proxy pin set",
        Code: "routing_account_ambiguous",
        Forbid: []string{"filesystem-write", "credential-activate", "service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Selection|ProxyPin|ProxyCodexDefault|ProxyPrime|ProxyConfigReload|.*CodexRoutePolicy)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Implement show/set/clear as separate typed actions, with explicit provider. Resolve full Codex inventory before write. Canonical Claude pin accepts unique email; translated legacy UUID resolves uniquely to email with the same missing/ambiguous errors.

- [ ] Change 2: Preserve atomic configuration updates and unknown future config fields. Show never creates configuration. Pin/fallback change proxy selection only; fallback remains terminal fallback and is never promoted to first-choice preference.

- [ ] Change 3: Preserve Claude live reload versus Codex configured-only/restart-required semantics. Prime enable/disable writes configuration only; status reports the stored model override map and never sends a priming request.

- [ ] Change 4: Test all12 leaves plus alias arity, UUID translation, case-sensitive account identity, future fields, config reload failure and exact application/restart_required output. Run existing route-policy/continuity tests to prove unchanged account choice algorithm.

**Implementation sequence:**

```text
1. Implement show/set/clear as separate typed actions, with explicit provider.
2. Preserve atomic configuration updates and unknown future config fields.
3. Preserve Claude live reload versus Codex configured-only/restart-required semantics.
4. Test all12 leaves plus alias arity, UUID translation, case-sensitive account identity, future fields, config reload failure and exact application/restart_required output.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: separated provider routing selections` and a concise unordered body describing significant changes. Do not push or merge.

## T14 — Expose Codex system-account reserve precisely

**Dependencies:** T05, T07.

**Files:** Create `cmd/cq/cli_v2_reserve.go`, `cmd/cq/cli_v2_reserve_test.go`. Modify `cmd/cq/proxy_reserve.go` typed control seam; retain `internal/proxy/codex_reserve.go` Control/Status/reconciliation. Extend `cmd/cq/proxy_reserve_test.go`, `cmd/cq/proxy_reserve_help_test.go`, `internal/proxy/codex_reserve_test.go`.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Reserve(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Reuse authenticated loopback control transport and CodexReserve.Control/Status. RoutingReserveStatus DTO must preserve fresh/stale evidence and nullable reset instants, not derive eligibility from formatted output.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy reserve clear` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-clear.txt) |
| `codex proxy reserve disable` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-disable.txt) |
| `codex proxy reserve enable` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-enable.txt) |
| `codex proxy reserve set` | None | `--window`, `--percent`, `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-set.txt) |
| `codex proxy reserve status` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-status.txt) |
| `codex proxy reserve windows` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-reserve-windows.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** reserve-stale-disable fixture has configured7d reserve, current system account and stale/missing reset observation. reserve-equality fixture has2% remaining and threshold2%; another account has identical remaining but is not system.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ReserveContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "disable requires fresh reset evidence",
        Scenario: "reserve-stale-disable",
        Args: []string{"codex", "proxy", "reserve", "disable", "--json"},
        Exit: 6,
        Command: "codex proxy reserve disable",
        Code: "reserve_evidence_required",
        Forbid: []string{"filesystem-write", "consume", "credential-activate"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Reserve|ProxyReserve|CodexReserve)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Adapt all six actions with exact window/percent validation and control-port/default timeout. Percent is full-window percentage points; threshold equality blocks only current system account. No account-selection option is added.

- [ ] Change 2: Keep temporary disable/reset rearming governed by verified reset evidence. Stale observations, failed refresh or expired bypass fail closed; do not persist an optimistic bypass or auto-consume credits.

- [ ] Change 3: Expose configured/enabled/effective state, reason, threshold/windows and timestamps exactly in resource DTO. Distinguish not configured from unavailable control and authentication failure.

- [ ] Change 4: Test threshold equality, non-system eligibility, system account replacement, custom windows, unavailable window, stale disable, verified rearm, idempotent clear and no implicit service start. Canonical provider scope and old aliases must produce equal resources.

**Implementation sequence:**

```text
1. Adapt all six actions with exact window/percent validation and control-port/default timeout.
2. Keep temporary disable/reset rearming governed by verified reset evidence.
3. Expose configured/enabled/effective state, reason, threshold/windows and timestamps exactly in resource DTO.
4. Test threshold equality, non-system eligibility, system account replacement, custom windows, unavailable window, stale disable, verified rearm, idempotent clear and no implicit service start.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: scoped reserve controls to Codex` and a concise unordered body describing significant changes. Do not push or merge.

## T15 — Adapt policy, pools and session bindings

**Dependencies:** T05, T07.

**Files:** Create `cmd/cq/cli_v2_policy.go`, `cmd/cq/cli_v2_policy_test.go`. Modify typed seams in `cmd/cq/proxy_policy.go`; preserve `internal/proxy/routing_policy_store.go`, `internal/proxy/routing_policy_store_test.go`, `internal/proxy/routing_policy_validation_test.go` storage/authority algorithms and tests.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Policy(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Reuse RoutingPolicyDocument, RenamePool, SetPoolValue, proxyPolicyControl and proxyPolicySessionDigest. State bootstrap belongs exclusively to T17; this adapter requires existing authority.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy policy apply` | None | `--file`, `--state-dir`, `--port`, `--timeout` | [Help](../help/codex-proxy-policy-apply.txt) |
| `codex proxy policy show` | None | `--state-dir`, `--port`, `--timeout` | [Help](../help/codex-proxy-policy-show.txt) |
| `codex proxy pool rename` | `old-name`, `new-name` | `--port`, `--timeout` | [Help](../help/codex-proxy-pool-rename.txt) |
| `codex proxy pool set` | `name` | `--account`, `--value`, `--port`, `--timeout` | [Help](../help/codex-proxy-pool-set.txt) |
| `codex proxy pool value` | `name`, `value` | `--port`, `--timeout` | [Help](../help/codex-proxy-pool-value.txt) |
| `codex proxy session bind` | None | `--pool`, `--session-id`, `--session-id-stdin`, `--digest`, `--port`, `--timeout` | [Help](../help/codex-proxy-session-bind.txt) |
| `codex proxy session digest` | None | `--session-id`, `--session-id-stdin`, `--digest`, `--port`, `--timeout` | [Help](../help/codex-proxy-session-digest.txt) |
| `codex proxy session list` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-session-list.txt) |
| `codex proxy session show` | None | `--session-id`, `--session-id-stdin`, `--digest`, `--port`, `--timeout` | [Help](../help/codex-proxy-session-show.txt) |
| `codex proxy session unbind` | None | `--session-id`, `--session-id-stdin`, `--digest`, `--port`, `--timeout` | [Help](../help/codex-proxy-session-unbind.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** policy-no-root fixture supplies valid public schema1 policy and a missing offline authority root. session-stdin-invalid fixture offers two selector options and panic-on-read stdin. session-digest-pass fixture supplies64 lowercase hex and no state dependencies. pool-unicode fixture uses `Research Ω; "night"` literally.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2PolicyContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "digest pass-through needs no control",
        Scenario: "session-digest-pass",
        Args: []string{"codex", "proxy", "session", "digest", "--digest", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--json"},
        Exit: 0,
        Command: "codex proxy session digest",
        WantJSON: `{"session_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
        Forbid: []string{"network", "credentials", "filesystem-write"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Policy|ProxyPolicy|.*RoutingPolicy|.*Pool|.*SessionDigest)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Apply/show use strict public schema1 complete replacement nested inside envelope2. Reject unknown/trailing JSON, generation conflicts and oversized inputs. Live --port and offline --state-dir are exclusive; offline mode requires existing state and exclusive ownership, with no implicit initialise.

- [ ] Change 2: Pool set resolves every repeated account before mutation, rejects duplicate resolved identities, preserves uint32 values and omission semantics. Rename/value reuse existing generation and case-insensitive Unicode name rules; never narrow pool names to shell-safe ASCII.

- [ ] Change 3: For session commands, enforce exact selector count before reading stdin. Validate raw UTF-8 length1..4096 bytes and preserve newline. Digest pass-through performs no state access; raw ID digest uses authenticated state. Preserve missing pool/binding3 and list ordering.

- [ ] Change 4: Test all10 leaves, generation races, unknown fields, root absence, duplicate references, Unicode/quotes, stdin exclusivity, byte4096/4097, digest case and idempotent binding changes. Output one envelope per mutation and retain internal storage schema unchanged.

**Implementation sequence:**

```text
1. Apply/show use strict public schema1 complete replacement nested inside envelope2.
2. Pool set resolves every repeated account before mutation, rejects duplicate resolved identities, preserves uint32 values and omission semantics.
3. For session commands, enforce exact selector count before reading stdin.
4. Test all10 leaves, generation races, unknown fields, root absence, duplicate references, Unicode/quotes, stdin exclusivity, byte4096/4097, digest case and idempotent binding changes.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: adapted policy and session commands` and a concise unordered body describing significant changes. Do not push or merge.

## T16 — Adapt lease invalidation and trace streaming

**Dependencies:** T05.

**Files:** Create `cmd/cq/cli_v2_diagnostics.go`, `cmd/cq/cli_v2_diagnostics_test.go`. Modify `cmd/cq/proxy_leases.go`, `cmd/cq/proxy_trace.go` typed output/context seams. Retain `internal/proxy/codex_lease_invalidation.go`, `internal/proxy/diag.go`, `internal/proxy/codex_trace.go` domain/log formats; extend matching tests.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2RoutingDiagnostics(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Trace uses cli.WriteJSON per event and terminal record, then returns Streamed=true; lease invalidation returns a normal Outcome. Keep follow checkpoint/rotation helpers and durable lease generation logic.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy lease invalidate` | None | `--port`, `--timeout` | [Help](../help/codex-proxy-lease-invalidate.txt) |
| `codex proxy trace` | None | `--session`, `--trace`, `--since`, `--tail`, `--follow`, `--payload`, `--timeout` | [Help](../help/codex-proxy-trace.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** lease-no-journal fixture has live control but unavailable durable journal. trace-empty fixture has configured empty retained logs. trace-timeout fixture adds one record then blocks follow until budget expires; use a temporary log and fake clock, not installed logs.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2RoutingDiagnosticsContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "missing lease authority is explicit",
        Scenario: "lease-no-journal",
        Args: []string{"codex", "proxy", "lease", "invalidate", "--json"},
        Exit: 4,
        Command: "codex proxy lease invalidate",
        Code: "lease_journal_unavailable",
        Forbid: []string{"credential-activate", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2RoutingDiagnostics|ProxyLeases|ProxyTrace|.*LeaseInvalidation)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Invalidate reusable task affinities through existing durable generation control. Preserve active request authority and hard continuity; missing/poisoned/legacy journal is not success. Repeated invalidation reports actual counts and generation semantics.

- [ ] Change 2: Preserve trace tail, AND filters, retained history, bridge/checkpoint and rotation behaviour. Reject normalised empty session filter; --payload exposes only requested captured content. Known authentication keys/headers receive recursive redaction; arbitrary prompt text remains unchanged.

- [ ] Change 3: JSONL writes zero or more event envelopes and exactly one terminal envelope for end, gap, timeout7 or interrupt130. Propagate output writer errors; never emit duplicate terminal errors or enable payload capture as a read side effect.

- [ ] Change 4: Test empty success terminal, tail0/duplicates, timestamp bounds, partial history8, first-poll rotation, write failures, redaction and interruption. Use one helper-process signal test and deterministic context tests for the rest.

**Implementation sequence:**

```text
1. Invalidate reusable task affinities through existing durable generation control.
2. Preserve trace tail, AND filters, retained history, bridge/checkpoint and rotation behaviour.
3. JSONL writes zero or more event envelopes and exactly one terminal envelope for end, gap, timeout7 or interrupt130.
4. Test empty success terminal, tail0/duplicates, timestamp bounds, partial history8, first-poll rotation, write failures, redaction and interruption.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified routing diagnostics output` and a concise unordered body describing significant changes. Do not push or merge.
