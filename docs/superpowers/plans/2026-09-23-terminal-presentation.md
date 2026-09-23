# CQ Terminal Presentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Independent family tasks may use superpowers:subagent-driven-development after the shared interfaces pass review. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the approved help and operational-output design for every CQ command, including semantic colour, concrete result layouts and truthful partial/failure states.

**Architecture:** Keep operations, quota arithmetic and machine resources unchanged. Each semantic adapter supplies a pure terminal renderer built from its existing typed result; `internal/cli` chooses terminal versus existing plain/JSON output and owns diagnostics. Shared formatting lives in `internal/cli` because `internal/output` already imports that package; reversing that dependency would create a cycle.

**Tech Stack:** Go 1.26.1, existing `charmbracelet/x/ansi`, `charmbracelet/x/term`, Lipgloss and Go testing with `-race`; Python 3 for existing fixture/catalogue checks. No new framework or runtime dependency.

**Spec:** `specs/cli-v2/presentation/README.md`, `colour.md`, `groups.md`, `accounts.md`, `routing.md`, `runtime.md`; concrete targets in `presentation/examples/{groups,accounts,routing,runtime}.md` and `examples/index.json`. Paths in this plan are relative to the repository root.

## Global Constraints

- Canonical grammar, defaults, exit codes, JSON resources, authority boundaries and mutation receipts remain unchanged.
- Maximum body width is 100 display cells; test 20, 40, 80 and 120 columns. No clipped IDs or dropped fields.
- Colour requires the destination stream to be a terminal, `TERM` not `dumb`, and nonempty `NO_COLOR` absent. Evaluate stdout and stderr separately.
- Existing redirected plain output, JSON/JSONL, completion scripts, hook payloads and frozen human trace protocols remain byte-compatible.
- One rendering clock per invocation. Fixture clock: `2026-09-23T15:30:00Z`; timezone: `Europe/London`.
- Unknown is not zero; a failed command is not proof of rollback; active is not healthy; configured is not applied live.
- Green means confirmed outcome, cyan identity/syntax, blue quantity, magenta time, amber attention and red failure. Never infer roles from untrusted text.
- Retain the quota dashboard and existing plan-weighted gauge semantics. No renderer-triggered provider requests.
- Preserve release-linked Gemini credentials. Never deploy a development binary lacking release link data.
- No disposable deployment environment is required. Candidate command presentation does not authorise enabling its four unavailable transitions.
- All Go tests use `-race`. Required final checks: `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`.
- This document authorises no implementation, push, PR, release or deployment by itself. Execute after the user's implementation instruction; apply existing authorisation to its actual scope and target.

## Review Focus

1. Mixed streams: stdout redirected with stderr attached to a terminal, and the inverse, must not leak ANSI or select the wrong layout. Owned by T02/T03.
2. Hostile identity text: an account named `failed@example.test`, embedded escape sequences and wide graphemes must not change colour meaning, inject controls or exceed width. Owned by T01/T04.
3. Indeterminate mutations: known committed work plus interrupted cleanup must preserve receipts, causes and same-credit retry identity. Owned by T05/T10/T11.
4. Time and numeric edges: DST/year boundaries, expired deadlines, tiny percentages, exact thresholds and uint64 generations must retain meaning and precision. Owned by T01/T05/T07/T12.
5. Frozen output and early dispatch: help/version must not read state, while hook/trace/completion/JSON must not gain banners or diagnostics in their payloads. Owned by T03/T14/T15.

## Starting state and authority

Planning checkout: `.worktrees/cli-v2`, branch `jacobcxdev/cli-v2`, HEAD `10a374094a99e74a5fb9ed194926c557de591a10`. It has uncommitted help prototype, discovery fixes and approved design artefacts. Preserve them. Do not work in or clean the independently dirty original checkout.

At execution, record current HEAD, diff and installed build identity; fetch/compare the repository's actual default branch and incorporate intervening fixes additively before final qualification. Never reset the checkout or force-push. Review changed semantic paths after integration; rerun affected acceptance tests.

There are 89 operational leaves, 37 groups and root help. `terminal-presentation-coverage.json` beside this plan assigns each leaf to exactly one family task and each authored scenario to its source fixture. Tasks T01–T03 are prerequisites. Family tasks T04–T14 can then proceed independently except the explicit dependencies below. T15 is the whole-product gate.

The design fixtures are independent expected outputs. Never regenerate their words from the implementation to make tests pass. If fixture/source conflict reveals a real domain constraint, correct the design explicitly, document the reason and preserve semantics. Existing examples cover representative states, not every reachable error: retain all declared-error tests too.

## File and interface map

| Files | Responsibility |
|---|---|
| `internal/cli/terminal.go`, `terminal_format.go`, matching tests | Typed colour roles, safe spans, fields/tables, numerical/time formatting |
| `internal/cli/types.go`, `run.go`; `cmd/cq/main.go` | Per-stream terminal capabilities and pure terminal-renderer selection |
| `internal/cli/terminal_help.go`, `help_gen.go`; catalogue generator | Shared help typography and discovery, without manual generated-file edits |
| `cmd/cq/cli_v2_terminal_{accounts,resets,models,routing,validation,endpoint,runtime,service,candidate}.go` and matching tests | Pure family presentation, using existing typed results in the same package |
| Existing `cmd/cq/cli_v2_*.go` semantic adapters | Attach renderer callbacks at result construction; no operation changes |
| `internal/output/tty_*.go`, `cmd/cq/cli_v2_check.go` | Quota presentation while retaining the existing aggregate computation |
| `cmd/cq/cli_v2_terminal_contract_test.go` | Scenario coverage, fixture comparison, width/colour/stream matrix |

### Shared interface, defined by T01/T02

```go
// internal/cli/terminal.go
// All text passed as a Span is untrusted display data and is escaped once.
type Role uint8
const (
    Body Role = iota
    Heading
    Identity
    Quantity
    TimeValue
    Confirmed
    Attention
    Failed
    Secondary
)
type Span struct { Text string; Role Role }
type Field struct { Label string; Value []Span }
type TerminalOptions struct {
    Width int // Zero means redirected/non-terminal; never enable terminal layout.
    Colour bool
    Now time.Time
    Location *time.Location
}
type TerminalRenderer func(TerminalOptions) string

func TerminalLine(options TerminalOptions, indent int, spans ...Span) string
func TerminalFields(options TerminalOptions, indent int, fields []Field) string
func TerminalTable(options TerminalOptions, headers []string, rows [][]Span) string
func TerminalCount(value uint64) Span
func TerminalPercent(value float64, exactSetting bool) Span
func TerminalTime(value time.Time, options TerminalOptions, deadline bool) Span
```

Add `Terminal TerminalRenderer` to `cli.Outcome`; keep `Human`, `Data`, `Errors`, `Warnings` and `Streamed`. Replace the local prototype's `Session.TerminalWidth`/`Colour` with `Session.OutTerminal`/`ErrTerminal` of type `TerminalOptions`, migrating all callers/tests in T02. Capture `Now` once and use the same location in both. A callback captures a completed result snapshot, performs no I/O, does not decode JSON, and cannot change exit status or operation state.

Do not add a rendering registry parallel to the semantic registry, a document DSL, reflection over arbitrary resources or a second JSON schema. Family callbacks compose the small formatting functions directly. Unknown/absent values are decided by the adapter's typed field, not by generic zero-value guessing.

## Task 1: Safe semantic formatting primitives

**Files:** Create `internal/cli/terminal.go`, `terminal_format.go`, `terminal_test.go`, `terminal_format_test.go`. Read existing `HumanValue` and ANSI helpers before sharing escaping logic.

**Interfaces:** Produces the shared types and functions above. `TerminalTable` uses a table only when every required cell fits; otherwise emits labelled records in column order. An empty table is handled by its caller's explicit empty-state message.

- [ ] Add failing tests for zero/unknown separation, exact `2.05%`, `<0.1%`, `>99.9%`, uint64 counts versus identity generations, hostile text, grapheme width, narrow records and one failed colour-role assignment. Use real fixture lines as expected values.

```go
func TestTerminalQuantityRole(t *testing.T) {
    options := TerminalOptions{Width: 100, Colour: true}
    got := TerminalLine(options, 0, TerminalCount(0))
    if !strings.Contains(got, "\x1b[1;34m") || strings.Contains(got, "\x1b[31m") {
        t.Fatalf("zero must be a blue quantity, not failure: %q", got)
    }
    if ansi.Strip(got) != "0\n" { t.Fatalf("visible output: %q", got) }
}
```

- [ ] Run `go test -race -count=1 ./internal/cli -run 'TestTerminal'`; confirm the new contract fails before implementation.
- [ ] Implement fixed role-to-SGR mapping: Body reset/default, Heading bold, Identity 36, Quantity 1;34, TimeValue 35, Confirmed 32, Attention 33, Failed 31, Secondary 2. Escape external controls before adding owned SGR. Wrap using display-cell-aware ANSI utilities, not byte or rune counts. Every coloured span closes its style.
- [ ] Implement time formatting against `options.Now` and `options.Location`; preserve year, timestamp-specific UTC offset and audit precision. Round remaining time up and elapsed age down. No arbitrary urgency thresholds. Retain exact setting decimals separately from observed percentage rounding.
- [ ] Add DST fixtures across the London clock change, year rollover, past deadline, nil handling in callers and long numeric identifiers. Run the targeted tests and `go vet ./internal/cli`.

## Task 2: Per-stream dispatch and compatibility boundary

**Files:** Modify `internal/cli/types.go`, `run.go`, `cmd/cq/main.go`; extend `internal/cli/run_test.go`, `cmd/cq/cli_v2_integration_test.go`.

**Interfaces:** Consumes T01. Produces `Outcome.Terminal`, `Session.OutTerminal` and `Session.ErrTerminal`. No adapter must know whether the other stream is redirected.

- [ ] Add a renderer-selection table covering stdout/stderr TTY independently, NO_COLOR nonempty/empty, TERM=dumb, JSON, raw hook, completion, help and streamed outcomes. Inject writers and terminal options; do not depend on the test runner's terminal.
- [ ] Run `go test -race -count=1 ./internal/cli -run 'TestTerminalDispatch'` and observe the missing terminal-callback behaviour.
- [ ] Insert the terminal branch only after existing stream/JSON/raw-hook exclusions. Keep completion and already-rendered help on their established branches.

```go
case outcome.Terminal != nil && session.OutTerminal.Width > 0:
    err = writeBytes(session.Out, []byte(outcome.Terminal(session.OutTerminal)))
```

- [ ] Detect terminal width and colour independently for each destination in `main`, without global Lipgloss/termenv profile mutation. Replace all prototype field usages. Capture the rendering instant once.
- [ ] Assert redirected bytes and JSON envelopes are unchanged. Inject a short/failing writer: never emit a second stdout document; preserve existing safe write-failure exit behaviour. Run `go test -race -count=1 ./internal/cli ./cmd/cq -run 'TerminalDispatch|Output|CLIV2'`.

## Task 3: Help, diagnostics and pure built-ins

**Files:** Modify `internal/cli/terminal_help.go`, `run.go`, `presentation.go`; extend help/run/presentation tests. Modify `specs/cli-v2/generate.py` and `commands.json` only when metadata is missing; regenerate generated files.

**Interfaces:** Consumes T01/T02. Add unexported `terminalDiagnostics(path string, outcome Outcome, options TerminalOptions) string`. It receives structured errors/warnings and builds one diagnostic block with subordinate causes; it never infers success from exit 0 alone.

- [ ] Add fixtures for all 127 help surfaces, including direct reserve/policy links from shared proxy help and no self-listed groups. Add parse errors and a partial outcome with both aggregate code and specific cause.
- [ ] Run `go test -race -count=1 ./internal/cli -run 'Help|Presentation|Diagnostic'` before changing output; verify target fixture differences.
- [ ] Use shared roles in help; keep descriptions readable, constraints adjacent to options, Related before Examples. Retain all flags/defaults and pure early dispatch. Build version terminal output from `versionData`; do not embellish raw completion output.
- [ ] Route terminal diagnostics through `ErrTerminal`; preserve non-terminal and machine diagnostics exactly. Keep deprecation warnings distinct and preserve all specific warning causes. Cancellation/unknown state never becomes a fabricated rollback.
- [ ] Exercise a lookup function that fails the test if called by help/version/invalid syntax. Test exact completion bytes for Bash/Zsh/Fish and raw hook dispatch. Run generator and fixture checks from T15 plus `go test -race -count=1 ./internal/cli`.

## Family task pattern and fixture harness

T04–T13 each define their family renderer as a pure callback attached where the adapter already holds the typed result. `Human` remains unchanged. Avoid rebuilding the outcome from JSON. The family test constructs that typed result or uses the adapter's existing dependency fakes, then calls `cli.Run` with fixed terminal options.

T04 creates this shared test helper in `cmd/cq/cli_v2_terminal_contract_test.go`; later tasks use it unchanged:

```go
func assertTerminalFixture(t *testing.T, relative string, got string) {
    t.Helper()
    path := filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", relative)
    want, err := os.ReadFile(path)
    if err != nil { t.Fatal(err) }
    if ansi.Strip(got) != string(want) {
        t.Fatalf("terminal fixture %s differs\nwant:\n%s\ngot:\n%s", relative, want, ansi.Strip(got))
    }
}
```

Use fixture paths from `examples/index.json`, not guessed ordinal filenames. Register every authored operational scenario with its command, scenario title and stream; raw fixtures use their explicit protocol exemption. The final coverage test compares this registration with the index and fails on omissions. Add typed role assertions alongside plain-text equality; stripping ANSI alone cannot prove the colour design.

Each family task follows this independent cycle: add its typed fixtures and failing output test; run the listed command to establish the mismatch; implement its concrete renderer and callback attachment; run the tests again; inspect 40- and 100-column outputs; review the diff for semantic changes. Exact responsibilities and acceptance states follow.

## Task 4: Accounts and authentication

**Files:** Add `cmd/cq/cli_v2_terminal_accounts.go` and its `_test.go`; modify `cli_v2_accounts.go`, `cli_v2_account_mutations.go`, `cli_v2_auth.go`; create the shared contract test helper above. **Depends on:** T01–T03.

- [ ] Register all account/auth fixtures, including Gemini's externally managed account. Run `go test -race -count=1 ./cmd/cq -run 'TerminalAccounts|TerminalAuth'` and confirm failures.
- [ ] Attach callbacks using `AccountSummary` and authentication result structs. Render account identity cyan; active markers green only for confirmed native selection; numeric counts blue; credential expiry magenta. Show source/alias details as secondary metadata.
- [ ] Preserve login progress, cancellation, partial refresh and remove/activate confirmation semantics. Native activation does not prove quota health or proxy selection. Never expose credentials.
- [ ] Cover empty lists, duplicate selectors, hostile display names, missing Gemini state, partial provider refresh, cancelled login and failed removal. Compare each stream with its authored fixture; rerun the command above and existing account/auth tests with `-race`.

## Task 5: Reset-credit inventory, recommendations and redemption

**Files:** Add `cmd/cq/cli_v2_terminal_resets.go` and its `_test.go`; modify `cli_v2_resets.go`, `cli_v2_reset_use.go`. **Depends on:** T04's helper and T01–T03.

- [ ] Register every `codex reset` fixture. Run `go test -race -count=1 ./cmd/cq -run 'TerminalReset'` to expose current mismatches.
- [ ] Build inventory rows directly from `ResetAccountInventory`/`ResetCredit`; recommendation rows from `ResetSchedule`. Use account sections, credit counts, grant/expiry dates and explicit unavailable reasons. Known zero credits is a blue quantity, never a red error.
- [ ] Attach pure render callbacks after existing operations complete. Example composition, with already-resolved strings from the typed result:

```go
fields := []cli.Field{
    {Label: "Account", Value: []cli.Span{{Text: accountLabel, Role: cli.Identity}}},
    {Label: "Credits", Value: []cli.Span{cli.TerminalCount(creditCount)}},
    {Label: "Expires", Value: []cli.Span{cli.TerminalTime(expiresAt, options, true)}},
}
text := cli.TerminalFields(options, 2, fields)
```

Resolve `accountLabel`, `creditCount` and `expiresAt` from the existing inventory result before constructing this block; do not fetch or redeem inside a renderer.

- [ ] Test all-account and selected-account inventories, expired credits, unavailable inventory, no recommendation, successful redemption, quota-refetch warning and indeterminate redemption. An indeterminate result must never invite consuming another credit; preserve same-account/same-credit retry instructions exactly.
- [ ] Check timezone offsets across DST and exact cause codes. Run terminal tests plus existing reset tests with `-race`; assert fake redemption call counts did not change.

## Task 6: Model registries and overlays

**Files:** Add `cmd/cq/cli_v2_terminal_models.go` and its `_test.go`; modify `cli_v2_models.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register all five model-command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalModels'` before implementation.
- [ ] Render typed `ModelEntry` and selection results as aligned model identities, source metadata and blue counts. Mutation summaries distinguish persisted local changes from upstream registry state.
- [ ] Cover empty registry, refresh partial failure, overlay add/remove/prune success, unchanged results, invalid selection and local write failure. Preserve explicit paths and follow-up commands; style command syntax cyan without colouring whole prose paragraphs.
- [ ] Rerun terminal and existing model tests with `-race`; verify model ordering and machine resources remain unchanged.

## Task 7: Provider routing, policy, pools, sessions and reserve

**Files:** Add `cmd/cq/cli_v2_terminal_routing.go` and its `_test.go`; modify `cli_v2_selections.go`, `cli_v2_policy.go`, `cli_v2_reserve.go`. **Scope:** The 29 commands assigned T07 in the coverage manifest. **Depends on:** T04 helper, T01–T03.

- [ ] Register all pin/fallback/prime, policy/pool/session/lease and reserve scenarios; run `go test -race -count=1 ./cmd/cq -run 'TerminalRouting|TerminalReserve'` to establish failures.
- [ ] Render typed selection and policy documents before JSON encoding. Keep account/pool/session identities cyan, quantities blue, deadlines magenta and revision/source metadata secondary. Show desired configuration and observed live state as separate labelled facts when both exist.
- [ ] Preserve `7d:gpt-reserve` spelling. Use exact-setting percentage formatting for thresholds such as `2.05%`; observed values use the approved display precision. Render unknown/stale eligibility as attention with its reason, including successful read-only observations that are not healthy.
- [ ] Cover absent selections, unchanged mutations, protected pool settings, partial policy evidence, stale quota, expired leases, unknown sessions, clear/disable and write failures. A persisted policy must not imply live acknowledgement.
- [ ] For the six Codex pin, fallback and priming mutations, preserve a local configuration receipt on terminal stdout when `SaveConfig` returns successfully before timeout or interruption. Keep warning diagnostics on stderr, state `Live application unconfirmed`, and retain the no-receipt cases before save or on save failure. Compare both streams with the authored scenarios.
- [ ] Test fresh reserve data at five seconds and stale data beyond its existing freshness rule without modifying thresholds or eligibility logic. Run terminal tests and existing selection/policy/reserve tests with `-race`; assert existing policy generation and persistence calls are unchanged.

## Task 8: Readiness, validation, canaries and fixtures

**Files:** Add `cmd/cq/cli_v2_terminal_validation.go` and its `_test.go`; modify `cli_v2_validation.go` (including readiness/fixture handlers) and `cli_v2_canary.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register the seven assigned command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalValidation|TerminalCanary|TerminalReadiness|TerminalFixture'`.
- [ ] Present typed evidence as labelled checks with explicit passed/failed/unavailable states, transport, observation time and receipt identity. Counts stay blue even in failed results; only the failed state/cause is red.
- [ ] Preserve exact HTTP/WebSocket semantics identifiers and fixture digests from authored examples. Canary run IDs remain valid identifiers. Render bucketed timestamps as **Last observed day**, never heartbeat freshness; stopping is not equivalent to drained/finalised.
- [ ] Cover readiness blocked/unknown, HTTP and WebSocket success/failure, canary starting/running/stopping/stopped, missing evidence, fixture write failure and mixed validation checks. Use injected transports and clocks; no paid/live probes for screenshot generation.
- [ ] Rerun terminal and existing validation/canary tests with `-race`. Assert transport call counts, proof contents and admission decisions unchanged.

## Task 9: Legacy credential-endpoint migration

**Files:** Add `cmd/cq/cli_v2_terminal_endpoint.go` and its `_test.go`; modify `cli_v2_endpoint.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register all six lifecycle command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalEndpoint'` before edits.
- [ ] Render typed migration phase, endpoint identity, receipts and next action. Separate recorded intent from confirmed activation/finalisation. Keep addresses cyan, timestamps magenta, phase explanation readable without colour.
- [ ] Cover inspect/prepare/activate/resume/finalise/rollback success, unavailable state, conflicting generation, interruption and failed persistence. Do not claim rollback happened when effects are uncertain; redact secrets using existing safe fields only.
- [ ] Run terminal and existing endpoint tests with `-race`; check exact persisted bytes and transition call counts remain unchanged.

## Task 10: Shared proxy runtime, operations and rescue

**Files:** Add `cmd/cq/cli_v2_terminal_runtime.go` and its `_test.go`; modify `cli_v2_proxy.go`, `cli_v2_rescue.go`, `cli_v2_diagnostics.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register eight assigned command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalProxy|TerminalOperation|TerminalRescue'`.
- [ ] Render endpoint, runtime identity, health observations and operation evidence from typed results. Distinguish process liveness from routing readiness. Preserve actual operation states such as `result_available` and `terminal`.
- [ ] For `proxy serve`, style only human lifecycle messages at their existing emission points; preserve request/log stream formats and shutdown behaviour. Never buffer an unbounded stream into `Outcome.Terminal`.
- [ ] Cover normal stopped runtime, healthy idle runtime, failed health check, rescue entered/exited/absent, existing state initialisation, operation pending/result available and interrupted serve. A normal stopped state is not automatically an error.
- [ ] Run terminal and existing proxy/rescue/diagnostic tests with `-race`; verify signals, lifecycle ordering and exit codes unchanged.

## Task 11: Background services

**Files:** Add `cmd/cq/cli_v2_terminal_service.go` and its `_test.go`; modify `cli_v2_service.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register all six service command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalService'` before implementation.
- [ ] Render `v2ServiceData`/`v2ServiceComponent` using separate component rows, configured/running state, identity and last observation. Use confirmed green only for verified action results; blue PIDs/counts and magenta elapsed times.
- [ ] Cover combined components, partial install/start/stop/restart/uninstall, already satisfied state, unsupported host and manager failure. Display specific failed components without implying successful components rolled back.
- [ ] Run terminal and existing service tests with `-race` using fake managers. Do not install, stop or restart real services during renderer testing.

## Task 12: Quota display

**Files:** Modify `cmd/cq/cli_v2_check.go`, `internal/output/tty_renderer.go`, `tty_build.go` and their relevant tests; add `cmd/cq/cli_v2_terminal_check_test.go`. **Depends on:** T01–T03 and T04 helper.

- [ ] Register every `check` fixture, including root invocation; run `go test -race -count=1 ./cmd/cq ./internal/output -run 'TerminalCheck|TTY'` to establish differences.
- [ ] Attach the terminal callback to the completed report. Adapt the existing quota renderer to explicit width/colour/time options; keep its aggregate arithmetic and gauge model. Maintain the existing `internal/output` → `internal/cli` dependency direction.
- [ ] Preserve capacity-weighted depleted accounts, window values, reserve nesting and correction-deadline gauge semantics. Apply colour to typed identities, quantities, dates and explicit risk states; do not derive urgency from percentage alone.
- [ ] Cover missing provider configuration, mixed provider failures, cached fallback, depleted/healthy accounts, unknown reset, reserve-only windows, aggregate waste and overburn, Unicode names and 20-column output. Preserve specific causes underneath partial-result summaries.
- [ ] Assert an unconfigured provider alone does not fail an otherwise usable check. Run existing aggregate tests plus terminal/output/check tests with `-race`; unchanged numerical assertions are mandatory.

## Task 13: Candidate lifecycle and receipts

**Files:** Add `cmd/cq/cli_v2_terminal_candidate.go` and its `_test.go`; modify `cli_v2_candidate.go`. **Depends on:** T04 helper, T01–T03.

- [ ] Register all nine candidate-command fixture sets; run `go test -race -count=1 ./cmd/cq -run 'TerminalCandidate'` before edits.
- [ ] Render typed candidate state, generations, receipt identity, timestamps and confirmed effects. Follow authored historical/prepared/stopped/removed states exactly; never fabricate `ActiveReleaseDigest` when the projector leaves it absent.
- [ ] Keep client-safety refresh, start, release activate and release validate unavailable where currently unavailable. Their designed failure output does not authorise implementing those domain transitions.
- [ ] Cover prepare/status/receipt/stop/remove success and failures, stale generations, pending stop, missing receipt and unavailable transitions. Non-null known effect receipts must remain visible; uncertainty must remain explicit.
- [ ] Run terminal and existing candidate tests with `-race`; verify no new candidate transitions, filesystem writes or lifecycle effects.

## Task 14: Raw trace and hook protocol boundaries

**Files:** Modify only tests unless a boundary defect is found: `cmd/cq/proxy_trace_test.go`, `cli_v2_hook_test.go`, `internal/cli/run_test.go`; production boundaries are `proxy_trace.go`, `cli_v2_hook.go`, `internal/cli/run.go`. **Depends on:** T02/T03/T04.

- [ ] Register trace and hook fixtures as raw protocol cases; run `go test -race -count=1 ./cmd/cq ./internal/cli -run 'Trace|Hook|TerminalDispatch'`.
- [ ] Assert byte equality with raw fixtures for TTY/non-TTY, colour/no-colour and every width. Preserve the 105-column trace line and 155-column hook JSON; no wrapping, padding, ANSI or extra success banner.
- [ ] Preserve streaming cancellation and existing terminal-envelope exceptions. Only eligible human stderr diagnostics use the shared renderer; raw stdout never does.
- [ ] Add negative assertions for ESC bytes and extra stdout documents. Rerun tests above; inspect production diff and reject unrelated protocol changes.

## Task 15: Whole-product verification and release handoff

**Files:** Complete `cmd/cq/cli_v2_terminal_contract_test.go`; maintain `internal/cli` help coverage tests. Use the sibling coverage manifest and existing specification validators. **Depends on:** T01–T14.

- [ ] Reconcile the fixture index against registered tests: every fixture file appears exactly once in coverage; every command has its authored scenarios; all 127 help surfaces have tests. Fail when a new catalogue command or example lacks an owner/test.
- [ ] Run each human scenario with colour on/off and widths 20, 40, 80 and 120. The nominal authored width is exact text equality; other widths assert bounded visible cells, preserved content/order and no broken ANSI or graphemes. Raw protocols retain their documented width exemption.
- [ ] Check stdout/stderr independently, redirected output, NO_COLOR, TERM=dumb, invalid UTF-8/control sequences in external labels and partial writer failure. Validate role selection from semantic fields, not regex matches or ANSI-stripped output alone.
- [ ] Execute specification checks:

```sh
python3 specs/cli-v2/generate.py --check
python3 specs/cli-v2/presentation/check.py
python3 specs/cli-v2/presentation/examples/export.py --check
```

- [ ] Execute required Go checks after all family changes:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```

- [ ] Review terminal captures for reset inventory, reserve status, mixed service results, candidate uncertainty and quota aggregates at 40 and 100 columns. Check both dark and light terminal themes. Record actual visual review separately from fixture/HTML validation; the current browser preview has not been visually verified through automation.
- [ ] Refresh the default-branch comparison and integrate intervening bug fixes additively before final qualification. If integration changes code, rerun affected checks and the required final suite. Record exact baseline and candidate SHAs; do not reset dirty work or force-push.
- [ ] Produce the review handoff: changed families, complete fixture coverage, verification commands/results, raw/plain/JSON compatibility evidence and remaining limitations. No implementation, deployment or release is performed by writing this plan.

### Release gate when execution and release are authorised

Use the existing release pipeline and its release-time Gemini client-secret link data. Never substitute a locally built binary without that data, scrape secrets from a binary, or add a runtime secret fallback. Resolve and record the exact known-good installed release/artifact and candidate artifact hashes before replacing anything. Preserve unrelated configuration and credentials.

Deploy directly through the established process; the user explicitly rejected disposable installations. Verify the installed executable's build identity, help, default quota view, reset inventory and read-only service/proxy status. If deployment restarts or changes the proxy, require actual HTTP `200` and WebSocket `101` traffic, zero `503` and zero continuity mismatches; health alone is insufficient. Use existing authorised release traffic checks, not invented paid requests. If verification fails, restore the recorded known-good artifact through the existing release mechanism and report the exact failure. New remote writes or release targets require their own authorisation when not already covered.

### Signed checkpoints during execution

Keep each task reviewable. Before committing, inspect staged paths and exclude unrelated pre-existing changes. Use signed conventional commits with a short past-tense subject and a detailed body, for example:

```sh
git commit -S -m 'feat: added terminal presentation roles' -m '- Added typed colour and layout helpers.
- Preserved plain and machine output contracts.
- Verified terminal formatting with race-enabled tests.'
```

Do not create a PR, push, tag or deploy as part of this planning task. During execution, follow the repository's authorised review/release workflow and preserve published history.

## Completion criteria and execution recommendation

Implementation is complete only when all 89 commands, 37 groups and root help are accounted for; all authored fixtures and semantic role assertions pass; raw/plain/JSON contracts and effects remain unchanged; required Go checks pass; and visual review is recorded. Release completion additionally requires the installed evidence above.

Execute T01–T03 sequentially, then T04 to establish the shared fixture helper. Delegate independent family renderers after that foundation is stable; give each worker only its assigned files and manifest entries. Shared adapters such as `cli_v2_diagnostics.go` must have one writer at a time. Finish with T15 as a single integration/review task. This keeps semantic decisions central while allowing the repetitive family work to proceed independently.
