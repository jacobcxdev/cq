# CQ CLI v2 Runtime and Services Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adapt shared proxy, native component lifecycle and candidate/endpoint state boundaries.

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

## T17 — Adapt shared proxy runtime, inspection and state creation

**Dependencies:** T01, T05.

**Files:** Create `cmd/cq/cli_v2_proxy.go`, `cmd/cq/cli_v2_proxy_test.go`. Modify `cmd/cq/proxy.go`, `cmd/cq/proxy_inspect.go`, `cmd/cq/proxy_inspect_linux.go`, `cmd/cq/proxy_policy.go` state-init seam and `cmd/cq/proxy_unix.go` only as required for typed outcomes/context. Retain existing supervisor and resilience store.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Proxy(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume InspectProxy(ctx, ProxyInspectionTarget) proxy.ProxySnapshot, RenderProxySnapshot/projectProxySnapshot and existing supervisor start/state-init APIs. T18 status consumes the same underlying observations, not independent health guesses.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `proxy health` | None | `--port`, `--timeout` | [Help](../help/proxy-health.txt) |
| `proxy serve` | None | `--port`, `--migrate-legacy-managed` | [Help](../help/proxy-serve.txt) |
| `proxy state initialise` | None | `--state-dir`, `--timeout` | [Help](../help/proxy-state-initialise.txt) |
| `proxy status` | None | `--state-dir`, `--strict`, `--timeout` | [Help](../help/proxy-status.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** health-explicit-port fixture serves bounded public health JSON on an httptest loopback port and panics on config read. proxy-status-absent fixture returns coherent absent observations. state-conflict fixture has an existing configured authority root different from requested root.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ProxyContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "state initialise cannot silently retarget",
        Scenario: "state-conflict",
        Args: []string{"proxy", "state", "initialise", "--state-dir", "/tmp/cq-v2-other-state", "--json"},
        Exit: 6,
        Command: "proxy state initialise",
        Code: "proxy_state_conflict",
        Forbid: []string{"filesystem-write", "service"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Proxy|ProxyInspect|ProxyStatus|ProxyHealth|ProxyPolicy.*Initial|.*RuntimeSupervisor|.*ResilienceState)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Serve stays foreground, starts only after config validation/authority acquisition, emits ready only after bind and stopped after drain. --port override is ephemeral; explicit migration flag alone authorises its documented config migration. Preserve listener conflict and native worker cleanup, without service registration.

- [ ] Change 2: Health with explicit --port bypasses config entirely; otherwise resolve existing config/default without creating it. Enforce loopback, no redirects, bounded1MiB response and typed health resource; reject malformed upstream response.

- [ ] Change 3: Status always projects the same seven typed facts in human and JSON output. --strict alters exit policy only. Explicit --state-dir selects coherent candidate target; no canonical --port. T03 alone translates old status --port to health.

- [ ] Change 4: Extract state initialise from old policy path. Same configured directory is authenticated/idempotent, a different one conflicts, and only this explicit operation creates/persists resilience authority. Ordinary reads/policy writes cannot bootstrap it.

- [ ] Change 5: Test serve ready/drain/interrupt order, no token exposure, health read-only resolution, status JSON/human scope equality, strict absent/indeterminate/unhealthy exits and initialise repeat/conflict/cancellation.

**Implementation sequence:**

```text
1. Serve stays foreground, starts only after config validation/authority acquisition, emits ready only after bind and stopped after drain.
2. Health with explicit --port bypasses config entirely; otherwise resolve existing config/default without creating it.
3. Status always projects the same seven typed facts in human and JSON output.
4. Extract state initialise from old policy path.
5. Test serve ready/drain/interrupt order, no token exposure, health read-only resolution, status JSON/human scope equality, strict absent/indeterminate/unhealthy exits and initialise repeat/conflict/cancellation.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: unified shared proxy lifecycle output` and a concise unordered body describing significant changes. Do not push or merge.

## T18 — Implement selected-component service semantics

**Dependencies:** T01, T05, T17.

**Files:** Create `cmd/cq/cli_v2_service.go`, `cmd/cq/cli_v2_service_test.go`. Modify `cmd/cq/service.go`, `cmd/cq/service_command.go`, `internal/installstate/state.go` and tests for selected ownership updates; keep frozen package snapshot/restore format unchanged.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Service(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Extend existing servicePlatform with StartProxy(ctx context.Context) error, StopProxy(ctx context.Context) error, StartRefresh(ctx context.Context) error, StopRefresh(ctx context.Context) error. Add `serviceSelection` string enum values all/proxy/token-refresh and typed lifecycle action dispatch; preserve old both-component methods as wrappers for frozen machine callers. Durable enablement lives in the existing native platform definition/manager state: launchd disabled override, systemd enabled timer/service, Windows task Enabled. Inspect/Snapshot/Restore expose and preserve that state; no second CQ enablement file is introduced.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `service install` | None | `--component`, `--timeout` | [Help](../help/service-install.txt) |
| `service restart` | None | `--component`, `--timeout` | [Help](../help/service-restart.txt) |
| `service start` | None | `--component`, `--timeout` | [Help](../help/service-start.txt) |
| `service status` | None | `--component`, `--strict`, `--timeout` | [Help](../help/service-status.txt) |
| `service stop` | None | `--component`, `--timeout` | [Help](../help/service-stop.txt) |
| `service uninstall` | None | `--component`, `--timeout` | [Help](../help/service-uninstall.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** service-selective-stop fixture installs both components, sets both enabled/running and records per-component manager calls. service-disabled-refresh fixture is installed/disabled; forced restart finishes a new successful run but stays disabled. service-rollback fixture fails second selected component and then either restores or fails rollback.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ServiceContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "stop affects selected component only",
        Scenario: "service-selective-stop",
        Args: []string{"service", "stop", "--component", "proxy", "--json"},
        Exit: 0,
        Command: "service stop",
        WantJSON: `{"component":"proxy","action":"stop"}`,
        Forbid: []string{"refresh-manager", "credential-activate", "consume"},
        Calls: map[string]int{"proxy-stop": 1},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/installstate -run 'Test(CLIV2Service|RunService|Service.*(Install|Restart|Rollback|Ownership|Snapshot|Restore))'
go vet ./cmd/cq ./internal/installstate
```

- [ ] Change 1: Add real Start/Stop semantics: start requires installation and durably enables automatic activation; stop durably disables it without removing definitions. Install/start order proxy then refresh; stop/uninstall reverse order. Preserve unselected definitions, enabled state, process and ownership records.

- [ ] Change 2: Use the existing lifecycle lock around selected mutation and verification. Snapshot selected native definitions/enabled/running state and existing ownership before mutation; on failure restore them and report rollback=restored/failed truthfully. Snapshot/restore machine ABI remains schema1 and both-component authority. Native persisted enablement is the single source of desired state, including after package rollback.

- [ ] Change 3: Preserve explicit disabled native state; remove implicit ensureAgent/install calls from quota/account/auth commands so they cannot silently re-enable it. Uninstall selected component removes its native definition and selected ownership only, retaining user data. Package ownership conflicts gate install/uninstall, while authorised user start/stop/restart use existing package registration.

- [ ] Change 4: Implement service status and --strict from actual observations. Periodic token-refresh is healthy only if enabled and most recent successful completion <=35m old; idle is valid. Restart of disabled refresh succeeds only after a new successful forced run, while enabled=false/healthy=false remain truthful.

- [ ] Change 5: Test all6 actions x3 selections, missing registration, idempotent start/stop, disabled restart, partial failure/rollback, lock timeout and unknown ownership. Native platform methods are initially test fakes; T19–T21 complete real adapters before T27 cutover.

**Implementation sequence:**

```text
1. Add real Start/Stop semantics: start requires installation and durably enables automatic activation; stop durably disables it without removing definitions.
2. Use the existing lifecycle lock around selected mutation and verification.
3. Preserve explicit disabled native state; remove implicit ensureAgent/install calls from quota/account/auth commands so they cannot silently re-enable it.
4. Implement service status and --strict from actual observations.
5. Test all6 actions x3 selections, missing registration, idempotent start/stop, disabled restart, partial failure/rollback, lock timeout and unknown ownership.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: added explicit service enablement` and a concise unordered body describing significant changes. Do not push or merge.

Native adapter interface additions must compile on all targets in this task. Add platform method declarations forwarding to an explicit unsupported error until the corresponding native task lands; they are never exposed by main before T27. Do not treat that temporary internal error as finished native support.

## T19 — Implement macOS service enable and disable

**Dependencies:** T18.

**Files:** Modify `cmd/cq/service_darwin.go`, `cmd/cq/service_darwin_test.go`, `cmd/cq/launchagent_darwin.go`, `cmd/cq/launchagent_darwin_test.go`, `cmd/cq/proxy_darwin_test.go`. Native disabled state is the single enablement authority; no CQ sidecar store.

**Interfaces:** Implements the four servicePlatform methods fixed by T18 on darwinServicePlatform. Existing Install/Restart/Remove/Inspect/Snapshot/Restore methods retain their signatures and selected-component authority.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** mac-stop fixture uses a fake launchctl runner with owned proxy/refresh plists. Proxy is KeepAlive-enabled and running. Stop must persist disabled launchd state before verified process shutdown; an unselected refresh plist/task stays byte-identical.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2MacServiceStop(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "macOS persistent proxy stop", Scenario: "mac-stop",
        Args: []string{"service", "stop", "--component", "proxy", "--json"},
        Exit: 0, Command: "service stop", WantJSON: `{"component":"proxy"}`,
        Forbid: []string{"refresh-manager"}, Calls: map[string]int{"proxy-disable":1,"proxy-stop":1},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq -run 'Test(CLIV2MacService|DarwinService|.*LaunchAgent)'
go vet ./cmd/cq
```

- [ ] Change 1: Implement launchctl enable/bootstrap/kickstart and disable/bootout using existing GUI/user domain and owned plist identity. A bootout alone is insufficient persistent stop; verify disabled state and stopped process with the shared deadline.

- [ ] Change 2: Preserve plist escaping, executable same-file/owner checks, logs/root binding, snapshot restore and rollback. Start restores selected automatic activation; restart of disabled refresh forces one run without enabling future triggers.

- [ ] Change 3: Test manager call order, already-loaded/unloaded cases, disabled persistence, missing plist, second-step failure and rollback. Keep fake manager tests hermetic; no ordinary test invokes the user launchd domain.

- [ ] Change 4: On an explicitly authorised isolated macOS test user, run the lifecycle matrix in T28 and capture plist, launchctl enabled state, owner identity, process and new refresh completion evidence. Build-only evidence cannot satisfy this native gate.

**Implementation sequence:**

```text
1. Implement launchctl enable/bootstrap/kickstart and disable/bootout using existing GUI/user domain and owned plist identity.
2. Preserve plist escaping, executable same-file/owner checks, logs/root binding, snapshot restore and rollback.
3. Test manager call order, already-loaded/unloaded cases, disabled persistence, missing plist, second-step failure and rollback.
4. On an explicitly authorised isolated macOS test user, run the lifecycle matrix in T28 and capture plist, launchctl enabled state, owner identity, process and new refresh completion evidence.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: implemented macOS service controls` and a concise unordered body describing significant changes. Do not push or merge.

## T20 — Implement Linux service and timer enablement

**Dependencies:** T18.

**Files:** Modify `cmd/cq/service_systemd.go`, `cmd/cq/service_systemd_test.go`, `cmd/cq/service_linux.go`, `cmd/cq/service_linux_test.go`; update `cmd/cq/testdata/systemd/cq-proxy.service`, `cmd/cq/testdata/systemd/cq-refresh.service`, `cmd/cq/testdata/systemd/cq-refresh.timer` only if generated definitions change. Retain namespace/runtime authority.

**Interfaces:** Implements T18 Start/Stop methods on systemdServicePlatform; token-refresh timer+service are one selected component. Existing manager injection and renderer interfaces remain the boundary.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** linux-stop-refresh fixture has enabled cq-refresh.timer and running cq-refresh.service alongside healthy proxy. Stop disables/stops timer before stopping refresh job; subsequent timer activity cannot restart it. Proxy unit remains untouched.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2LinuxServiceStop(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "Linux timer and job stop together", Scenario: "linux-stop-refresh",
        Args: []string{"service", "stop", "--component", "token-refresh", "--json"},
        Exit: 0, Command: "service stop", WantJSON: `{"component":"token-refresh"}`,
        Forbid: []string{"proxy-manager"}, Calls: map[string]int{"refresh-disable-timer":1,"refresh-stop-job":1},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq -run 'Test(CLIV2LinuxService|SystemdService|LinuxService|.*Systemd.*Definition)'
go vet ./cmd/cq
```

- [ ] Change 1: Implement user-manager enable/start and disable/stop for selected units. Treat refresh.timer and refresh.service as one unit of rollback/selection, with timer disabled before stopping its active job. Preserve proxy runtime ownership and listener authority.

- [ ] Change 2: Persist disabled intent in systemd unit/timer enablement and report it through T18. Forced restart of disabled refresh runs service once without enabling timer. Capture successful completion time and distinguish idle from failed periodic service.

- [ ] Change 3: Test unit escaping, XDG root binding, daemon reload failure, absent user manager, timer/job ordering, selected rollback and unselected byte/state preservation with fake manager output.

- [ ] Change 4: Run native lifecycle matrix only in an authorised isolated Linux user manager. Preserve required namespace/Landlock/cancellation/HTTP/WebSocket CI tests, and fail the native gate when they skip or lack required kernel capability.

**Implementation sequence:**

```text
1. Implement user-manager enable/start and disable/stop for selected units.
2. Persist disabled intent in systemd unit/timer enablement and report it through T18.
3. Test unit escaping, XDG root binding, daemon reload failure, absent user manager, timer/job ordering, selected rollback and unselected byte/state preservation with fake manager output.
4. Run native lifecycle matrix only in an authorised isolated Linux user manager.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: implemented Linux service controls` and a concise unordered body describing significant changes. Do not push or merge.

## T21 — Implement Windows selected task enablement

**Dependencies:** T18.

**Files:** Modify `cmd/cq/service_windows_definition.go`, `cmd/cq/service_windows_definition_test.go`, `cmd/cq/service_windows.go`, `cmd/cq/service_windows_test.go`, `cmd/cq/service_windows_identity_windows.go`, `cmd/cq/service_windows_identity_other.go` only as required. Preserve installer/SID/known-folder authority.

**Interfaces:** Implements T18 Start/Stop methods on windowsTaskServicePlatform. Use existing runWindowsTaskMutation/queryWindowsTaskState/current-user identity APIs; task Enabled property is durable native state, separate from ownership.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** windows-stop fixture has current-subject-owned proxy and refresh scheduled tasks, enabled/running. Stop disables selected task then ends its process; different SID or task folder conflicts before mutation.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2WindowsServiceStop(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "Windows selected task remains disabled", Scenario: "windows-stop",
        Args: []string{"service", "stop", "--component", "proxy", "--json"},
        Exit: 0, Command: "service stop", WantJSON: `{"component":"proxy"}`,
        Forbid: []string{"refresh-manager"}, Calls: map[string]int{"proxy-disable":1,"proxy-stop":1},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq -run 'Test(CLIV2WindowsService|Windows.*Service|Windows.*Task|RunService|ServiceSnapshot)'
go vet ./cmd/cq
```

- [ ] Change 1: Implement persistent task Enabled transitions and verified Run/End semantics for selected current-user task. Preserve full XML, SID/folder identity and registered executable/root authority; never trust USERPROFILE to establish owner.

- [ ] Change 2: Keep restart of disabled periodic refresh a single forced run without enabling task triggers. Reflect task completion code/time in shared35m health rule. Restore selected definitions and enabled state on rollback.

- [ ] Change 3: Test fake scheduler XML/query transitions, escaped executable paths, SID mismatch, disabled task run, partial failures and unselected task preservation. Verify Windows build tags keep non-Windows tests free of native scheduler calls.

- [ ] Change 4: Run actual scheduler lifecycle/rollback under CQ_NATIVE_WINDOWS_SCHEDULER_TEST=1 only on an authorised isolated Windows CI user, then retain existing WiX install/upgrade/uninstall and credential endpoint native gates. Cross-builds are separate evidence.

**Implementation sequence:**

```text
1. Implement persistent task Enabled transitions and verified Run/End semantics for selected current-user task.
2. Keep restart of disabled periodic refresh a single forced run without enabling task triggers.
3. Test fake scheduler XML/query transitions, escaped executable paths, SID mismatch, disabled task run, partial failures and unselected task preservation.
4. Run actual scheduler lifecycle/rollback under CQ_NATIVE_WINDOWS_SCHEDULER_TEST=1 only on an authorised isolated Windows CI user, then retain existing WiX install/upgrade/uninstall and credential endpoint native gates.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: implemented Windows service controls` and a concise unordered body describing significant changes. Do not push or merge.

## T22 — Adapt supported candidate state and fence unavailable transitions

**Dependencies:** T01, T05.

**Files:** Create `cmd/cq/cli_v2_candidate.go`, `cmd/cq/cli_v2_candidate_test.go`. Modify `cmd/cq/proxy_candidate.go`, `cmd/cq/proxy_candidate_runtime.go`, `cmd/cq/proxy_candidate_release.go`, `cmd/cq/proxy_operation.go` typed inspection/stop/remove seams; retain `internal/proxy/candidate_lifecycle.go` and receipt stores. Preserve candidate-annex-v1 files as documentation.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Candidate(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Supported5 actions call prepareCandidateInput/readCandidateInputFile/digestCandidateExecutable, inspectCandidateRuntime/stopCandidateRuntime, removeCandidateStateRoot/removeCandidateTree and runCandidateReceiptLookup through existing typed engines. Reserved4 return their fixed Outcome before resolving environment or opening files.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `proxy candidate client-safety refresh` | None | `--state-dir`, `--validation-run-id`, `--timeout` | [Help](../help/proxy-candidate-client-safety-refresh.txt) |
| `proxy candidate prepare` | None | `--state-dir`, `--port`, `--source-config`, `--target-release-bundle`, `--release-digest`, `--client-build`, `--client-executable`, `--client-registry`, `--credential-mode`, `--credential-manifest`, `--confirm-read-only-credentials`, `--policy-snapshot`, `--confirm-payload-capture`, `--timeout` | [Help](../help/proxy-candidate-prepare.txt) |
| `proxy candidate receipt show` | None | `--state-dir`, `--attempt-id`, `--timeout` | [Help](../help/proxy-candidate-receipt-show.txt) |
| `proxy candidate release activate` | None | `--state-dir`, `--release-digest`, `--validation-run-id`, `--confirm-artifact-switch`, `--timeout` | [Help](../help/proxy-candidate-release-activate.txt) |
| `proxy candidate release validate` | None | `--state-dir`, `--target-release-bundle`, `--rollback-bundle`, `--rollback-receipt`, `--rollback-receipt-digest`, `--client-build`, `--client-executable`, `--validation-run-id`, `--receipt-file`, `--confirm-control-health` | [Help](../help/proxy-candidate-release-validate.txt) |
| `proxy candidate remove` | None | `--state-dir`, `--confirm-candidate-state-loss` | [Help](../help/proxy-candidate-remove.txt) |
| `proxy candidate start` | None | `--state-dir`, `--timeout` | [Help](../help/proxy-candidate-start.txt) |
| `proxy candidate status` | None | `--state-dir`, `--timeout` | [Help](../help/proxy-candidate-status.txt) |
| `proxy candidate stop` | None | `--state-dir`, `--confirm-client-stopped` | [Help](../help/proxy-candidate-stop.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** no-access for start uses a syntactically valid absolute state-dir whose parent does not exist. Supported fixtures use temp-owned state/receipts and synthetic executable bytes; root-owned executable ownership is simulated, never requires changing real ownership. candidate-receipt-conflict has two contradictory retained receipts.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2CandidateContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "unavailable start does not read state",
        Scenario: "no-access",
        Args: []string{"proxy", "candidate", "start", "--state-dir", "/tmp/cq-v2-unopened-candidate", "--json"},
        Exit: 4,
        Command: "proxy candidate start",
        Code: "candidate_evidence_unavailable",
        Forbid: []string{"filesystem-read", "filesystem-write", "credentials", "network", "service", "child", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'Test(CLIV2Candidate|ProxyCandidate|CandidateRuntime|.*CandidateLifecycle|.*CandidateReceipt)'
go vet ./cmd/cq ./internal/proxy
```

- [ ] Change 1: Implement prepare as byte/digest/build-bound state preparation only. It does not install source configuration, authenticate absent client provenance or qualify release safety. Preserve strict file size, symlink/inode/ownership/phase/generation protections and semantic confirmations.

- [ ] Change 2: Implement status/receipt read-only, stop for existing instances and remove only permitted owned-state phases with required confirmations. Stop/remove use fixed30s total with15s cleanup inside it; no new timeout flag. Failed cleanup leaves inspectable truthful state.

- [ ] Change 3: For start, client-safety refresh, release activate and release validate: after pure syntax checks return exit4, data=null, exact candidate_evidence_unavailable message. Do not resolve paths/env, open files, lock state, launch child, advance phase or write receipt. Keep all reserved grammar/help and aliases; do not invoke startCandidateRuntime/refreshCandidateBearerBarrier/switchCandidateRuntimeArtifact/validateCandidateRelease.

- [ ] Change 4: Split old broad prepare/start/release success tests so injected test helpers do not imply public capability. Test all four canonical unavailable paths and every alias with panic dependencies, plus valid/invalid option syntax. Test the five supported paths with digest mismatch, trusted root executable versus forbidden root state, inode race, cancellation, conflict receipt and wrong-root removal.

**Implementation sequence:**

```text
1. Implement prepare as byte/digest/build-bound state preparation only.
2. Implement status/receipt read-only, stop for existing instances and remove only permitted owned-state phases with required confirmations.
3. For start, client-safety refresh, release activate and release validate: after pure syntax checks return exit4, data=null, exact candidate_evidence_unavailable message.
4. Split old broad prepare/start/release success tests so injected test helpers do not imply public capability.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: defined candidate capability boundaries` and a concise unordered body describing significant changes. Do not push or merge.

The release validate16min budget and reserved_success_contract fields describe an unavailable future capability; no operational timer or proof engine is created here. Public exit4 is the complete specified implementation. Independent proof backend requires a separate approved specification.

## T23 — Adapt legacy credential-endpoint maintenance

**Dependencies:** T01, T05.

**Files:** Create `cmd/cq/cli_v2_endpoint.go`, `cmd/cq/cli_v2_endpoint_test.go`. Modify `cmd/cq/proxy_endpoint_maintenance.go` typed seams; retain `internal/provider/codex/credential_endpoint_maintenance.go` and platform/transition/journal companion files/tests.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Endpoint(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume inspectLegacyEndpointCommand/transitionLegacyEndpointCommand and existing maintenance journal. Preserve drain authority and candidate-health authority as distinct input types/paths; T04 supplies deadline and interactive confirmation mechanics.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex proxy credential-endpoint legacy activate` | None | `--ticket-file`, `--confirm-stopped-and-drained`, `--non-interactive`, `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-activate.txt) |
| `codex proxy credential-endpoint legacy finalise` | None | `--ticket-file`, `--confirm-candidate-healthy`, `--non-interactive`, `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-finalise.txt) |
| `codex proxy credential-endpoint legacy inspect` | None | `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-inspect.txt) |
| `codex proxy credential-endpoint legacy prepare` | None | `--snapshot-file`, `--confirm-stopped-and-drained`, `--non-interactive`, `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-prepare.txt) |
| `codex proxy credential-endpoint legacy resume` | None | `--ticket-file`, `--confirm-stopped-and-drained`, `--non-interactive`, `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-resume.txt) |
| `codex proxy credential-endpoint legacy rollback` | None | `--ticket-file`, `--confirm-stopped-and-drained`, `--non-interactive`, `--timeout` | [Help](../help/codex-proxy-credential-endpoint-legacy-rollback.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** endpoint-finalise-drain-only fixture contains a prepared/activated ticket with valid stopped-and-drained evidence but no live-owner health proof. endpoint-inspect fixture has retained journal and panic-on-write dependencies. All paths point to temp state.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2EndpointContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "finalise needs candidate health authority",
        Scenario: "endpoint-finalise-drain-only",
        Args: []string{"codex", "proxy", "credential-endpoint", "legacy", "finalise", "--ticket-file", "/tmp/cq-v2-ticket.json", "--confirm-candidate-healthy", "--non-interactive", "--json"},
        Exit: 6,
        Command: "codex proxy credential-endpoint legacy finalise",
        Code: "endpoint_conflict",
        Forbid: []string{"credential-publish"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/provider/codex -run 'Test(CLIV2Endpoint|ProxyEndpoint|.*CredentialEndpointMaintenance)'
go vet ./cmd/cq ./internal/provider/codex
```

- [ ] Change 1: Adapt inspect/prepare/resume/activate/finalise/rollback with exact file/confirmation flags and total timeout. Keep prepare, activation and finalisation separate journalled transitions; no legacy commit shortcut maps to finalise.

- [ ] Change 2: Use stopped-and-drained confirmation only for its authorised phases. Finalise validates live-owner candidate health and retains rollback authority until proof succeeds. --non-interactive never replaces semantic confirmation flags.

- [ ] Change 3: Preserve crash/resume/rollback rules and endpoint authority formats; expose only typed receipt/state fields, never credential/proof contents. Inspect does not repair or recover state.

- [ ] Change 4: Retain TestProxyEndpointTransitionRequiresConfirmationAndKeepsRollbackUntilFinalise and TestProxyEndpointFinaliseRequiresHealthAuthorityNotDrainAuthority. Add canonical/alias cases, malformed ticket/snapshot, wrong authority, timeout/interruption and immutable inspection inventory.

**Implementation sequence:**

```text
1. Adapt inspect/prepare/resume/activate/finalise/rollback with exact file/confirmation flags and total timeout.
2. Use stopped-and-drained confirmation only for its authorised phases.
3. Preserve crash/resume/rollback rules and endpoint authority formats; expose only typed receipt/state fields, never credential/proof contents.
4. Retain TestProxyEndpointTransitionRequiresConfirmationAndKeepsRollbackUntilFinalise and TestProxyEndpointFinaliseRequiresHealthAuthorityNotDrainAuthority.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified endpoint maintenance stages` and a concise unordered body describing significant changes. Do not push or merge.
