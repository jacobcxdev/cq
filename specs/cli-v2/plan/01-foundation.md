# CQ CLI v2 Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the pure public CLI boundary, compatibility firewall and shared execution rules.

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

## T00 — Select and prove the implementation baseline

**Dependencies:** None.

**Files:** Create `specs/cli-v2/implementation-baseline.md` in the implementation worktree only. Read `source-baseline.json`, `source-inventory.json`, `source-differences.md`, root/child `AGENTS.md`, `CONTRIBUTING.md`, `go.mod` and all ten dirty paths listed in `source-baseline.json`. No current source edits.

**Interfaces:** Produces one documented implementation worktree, immutable starting SHA and baseline check results. T01–T28 use its repository-relative paths. No runtime API.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** Use the recorded SHA and hashes, not the current branch name as a proxy for source identity. Capture each existing dirty diff without printing credentials; classify each changed behaviour as present in release, required by this spec, or independent work retained in the original checkout.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```sh
git rev-parse HEAD
git status --short
git cat-file -e debcb2cb19b61fd0cb2983554299d0edaaa052c5^{commit}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
python3 specs/cli-v2/generate.py --check
go build ./...
go vet ./...
go test -race -count=1 ./...
```

- [ ] Change 1: Use `superpowers:using-git-worktrees` at execution time. Create a sibling worktree on `jacobcxdev/cli-v2` from `debcb2cb19b61fd0cb2983554299d0edaaa052c5`; if that branch/path already exists, inspect it and choose an unused `jacobcxdev/cli-v2-N` suffix rather than overwriting it. Copy only `specs/cli-v2/`, excluding implementation evidence from another run.

- [ ] Change 2: Record `git rev-parse HEAD`, `git status --short`, Go version and the SHA-256 of every preserved dirty file. Compare the released implementations with the ten original dirty diffs. Assign any required regression to T07–T12 without applying unrelated patches wholesale.

- [ ] Change 3: Run the baseline gates below before CLI edits. Retain failing test names/logs; fix only baseline defects that prevent meaningful CLI verification in separate reviewed prerequisite commits. Do not weaken a test or call a pre-existing failure a CLI regression.

- [ ] Change 4: If using a newer integration base, rerun full command/flag/source inventory and compare against both manifests before proceeding. Record every delta and amend the canonical spec and coverage together; never silently omit new commands.

**Implementation sequence:**

```text
1. Use `superpowers:using-git-worktrees` at execution time.
2. Record `git rev-parse HEAD`, `git status --short`, Go version and the SHA-256 of every preserved dirty file.
3. Run the baseline gates below before CLI edits.
4. If using a newer integration base, rerun full command/flag/source inventory and compare against both manifests before proceeding.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `docs: recorded CLI migration baseline` and a concise unordered body describing significant changes. Do not push or merge.

This task is a baseline evidence task, not a manufactured failing-test exercise. Success means a preserved original checkout, an identified complete source baseline, and known baseline test status.

## T01 — Fence frozen machine protocols

**Dependencies:** T00.

**Files:** Create `cmd/cq/cli_v2_internal.go`, `cmd/cq/internal_abi_test.go`, `cmd/cq/testdata/cli-v2/internal-abi.json`. Modify only necessary typed seams in `cmd/cq/proxy.go`, `cmd/cq/proxy_candidate_runtime.go`, `cmd/cq/service_command.go`, `cmd/cq/service.go` and `internal/proxy/runtime_control.go`. Extend `cmd/cq-install/main_test.go`; do not change public `cmd/cq-install/main.go` dispatch yet.

**Interfaces:** Produces `dispatchMachineABI(argv []string) (handled bool, exitCode int)` with frozen handlers. Add pure `isMachineABI(argv []string) bool` for classifier tests. T27 calls dispatch before public parsing. Internal role/FD structs remain existing types; do not replace their wire format.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** Use exact argv fixtures from all eight frozen migration entries plus Linux namespace/candidate child and installer cases in internal-abi.md. Dependency fakes reject service/credential access; inherited-lock/descriptor tests use helper processes and synthetic FDs only.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2InternalBoundary(t *testing.T) {
    if isMachineABI([]string{"proxy", "start", "--help"}) {
        t.Fatal("public help entered machine dispatch")
    }
    if isMachineABI([]string{"proxy", "serve", "--runtime-role=worker"}) {
        t.Fatal("canonical public path accepted internal flags")
    }
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./cmd/cq-install ./internal/proxy -run 'Test(CLIV2Internal|Service.*(Snapshot|Restore)|CandidateRuntime|RuntimeRole|.*Inherited.*Lock)'
go vet ./cmd/cq ./cmd/cq-install ./internal/proxy
```

- [ ] Change 1: Write the positive/negative corpus with exact argv, raw stdout, exit and side-effect expectations for every internal-abi.md form. Cover omitted/explicit owner, equals-only fields, invalid lock markers, ordered descriptors, wrong roles, noncanonical numeric spelling, unknown/trailing snapshot fields and help interception.

- [ ] Change 2: Implement classifier precedence as `frozen exact machine grammar -> unchanged machine handler`, otherwise return `handled=false`. Public help must resolve without launching a child. Invalid internal-looking forms must fail their documented ABI grammar or public parser without falling into a permissive alias.

- [ ] Change 3: Keep generator/parser pairs and inherited authority validation together. Preserve package transaction schema1, silent mutation stdout, raw JSON and exit1 conventions. Public --json is not a new internal option.

- [ ] Change 4: Run the existing service snapshot/restore and runtime descriptor helper-process tests with fake targets. Confirm all eight frozen entries have both positive and rejection cases; no global v2 envelope appears on their stdout.

**Implementation sequence:**

```text
1. Write the positive/negative corpus with exact argv, raw stdout, exit and side-effect expectations for every internal-abi.md form.
2. Implement classifier precedence as `frozen exact machine grammar -> unchanged machine handler`, otherwise return `handled=false`.
3. Keep generator/parser pairs and inherited authority validation together.
4. Run the existing service snapshot/restore and runtime descriptor helper-process tests with fake targets.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `test: fenced internal command protocols` and a concise unordered body describing significant changes. Do not push or merge.

## T02 — Generate the command catalogue, help and utility output

**Dependencies:** T00.

**Files:** Create `internal/cli/types.go`, `internal/cli/catalogue.go`, `internal/cli/catalogue_gen.go`, `internal/cli/catalogue_test.go`, `internal/cli/presentation.go`, `internal/cli/presentation_test.go`. Extend `specs/cli-v2/generate.py` with explicit Go-output generation/checking. Create `cmd/cq/cli_v2_presentation_test.go`. All 127 existing help files remain canonical goldens.

**Interfaces:** Produces shared types from coordinator and `Help(path string) (string, bool)`. `CommandSpec` has `Path`, `Kind` strings and `Options`, `Positionals []ParameterSpec`; `ParameterSpec` has `Name`, `Type`, `Short` strings, `Choices []string`, `Default []string`, `Required`, `Repeatable bool`. Generated package-local `catalogue []CommandSpec`, `helpByPath map[string]string`, `completionByShell map[string]string` feed T03/T04. Define `BuildInfo{Version string; Revision *string; Dirty *bool}` and `VersionOutcome(info BuildInfo) Outcome`; T27 supplies linker/build-info evidence.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `completion` | `shell` | None | [Help](../help/completion.txt) |
| `help` | `command-path` | None | [Help](../help/help.txt) |
| `version` | None | None | [Help](../help/version.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** Help test reads only the repository golden. Use an empty operational handler registry and invalid HOME to prove pure presentation. Completion fixtures include canonical commands/flags and a quoted Unicode pool-like word; no actual account/network lookup.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2GeneratedHelp(t *testing.T) {
    want, err := os.ReadFile("../../specs/cli-v2/help/codex-proxy-reserve-set.txt")
    if err != nil { t.Fatal(err) }
    got, ok := Help("codex proxy reserve set")
    if !ok || got != string(want) { t.Fatal("help differs from canonical specification") }
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
python3 specs/cli-v2/generate.py --check --go-output internal/cli/catalogue_gen.go
go test -race -count=1 ./internal/cli -run 'TestCLIV2(GeneratedHelp|Presentation|Completion|Version)'
go vet ./internal/cli
```

- [ ] Change 1: Generate immutable Go literals from commands.json and help/*.txt; runtime reads no specification files and requires no Python. Add `--go-output internal/cli/catalogue_gen.go`; with --check compare exact bytes and fail on drift. Retain the existing documentation-only --check behaviour.

- [ ] Change 2: Generate Bash/Zsh/Fish scripts from the same tree and parameter names. Offer canonical paths only; stop option completion after --, support repeatable options and positional enums, and quote shell words correctly. No invented dynamic account completion or network access.

- [ ] Change 3: Implement bare-group/help presentation for all 37 groups plus root. Render version SemVer/dev and nullable build provenance exactly; never invent a revision or dirty=false when unknown. --version and version share output, with JSON schema2 when requested.

- [ ] Change 4: Test all 127 help pages byte-for-byte, no ANSI under JSON, completion syntax using each available shell, and source-only output versus JSON script resource. Unavailable shells remain an explicit native/CI gate, not a skipped claim of compatibility.

**Implementation sequence:**

```text
1. Generate immutable Go literals from commands.json and help/*.txt; runtime reads no specification files and requires no Python.
2. Generate Bash/Zsh/Fish scripts from the same tree and parameter names.
3. Implement bare-group/help presentation for all 37 groups plus root.
4. Test all 127 help pages byte-for-byte, no ANSI under JSON, completion syntax using each available shell, and source-only output versus JSON script resource.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: generated canonical command help` and a concise unordered body describing significant changes. Do not push or merge.

Group and utility presentation is handled by cli.Run before operational lookup. T02 owns all 37 groups even though only the three leaf utilities appear in the command table below. Completion scripts have no runtime CQ state dependency.

## T03 — Implement pure parsing and all compatibility translations

**Dependencies:** T01, T02.

**Files:** Create `internal/cli/parse.go`, `internal/cli/parse_test.go`, `internal/cli/values.go`, `internal/cli/values_test.go`, `internal/cli/aliases.go`, `internal/cli/aliases_test.go`, `internal/cli/constraints.go`, `internal/cli/constraints_test.go` and `testdata/compatibility.json`. Keep command-specific rules here when they depend only on argv; file content/account resolution remains in family handlers.

**Interfaces:** Produces `Parse(argv []string) (Invocation, *ParseError)` and `ParseError` exactly as coordinator defines. Consumes catalogue and returns canonical Path, ordered argument/option slices, Supplied map, JSON/Presentation and deprecation warnings. Alias translation occurs once and cannot call account/filesystem dependencies.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** no-access parsing corpus: unknown --dry-run, duplicate switches, --flag=value, single short flag vs forbidden bundles, flags between command tokens, --, exact lowercase provider/enums, invalid UTF-8, min/max integer/duration and positional arity. For legacy Claude UUID, parsing retains a typed legacy-selector annotation for the handler; resolution requires inventory in T13.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2DuplicateOption(t *testing.T) {
    _, err := Parse([]string{"codex", "proxy", "trace", "--tail", "1", "--tail", "2"})
    if err == nil || err.ExitCode != 2 || err.Path != "codex proxy trace" {
        t.Fatalf("duplicate tail accepted: %#v", err)
    }
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./internal/cli -run 'TestCLIV2(Parse|Duplicate|Value|Constraint|Alias|Retired)'
go vet ./internal/cli
```

- [ ] Change 1: Implement token scanning in the exact README.md order: identify globals before -- without swallowing option values; resolve complete command path; apply local options only at that leaf; bind positionals; validate types and combinations. Parse help/version according to documented inspection precedence while preserving malformed-token errors.

- [ ] Change 2: Implement duration, integer, uint32, percentage, digest, timestamp, account-reference, path and UTF-8 byte checks from each parameter definition. Use strconv/time/json parsing with explicit bounds, no permissive coercion. Preserve raw session newline and Unicode pool names. Materialise only literal defaults; never read environment in Parse.

- [ ] Change 3: Encode cross-option constraints as a table keyed by canonical path with Go predicates. For every catalogue validation/constraint sentence, add a fixture identifier to the parser acceptance table or explicitly assign its runtime check to the semantic owner in coverage.json. Do not execute English validation prose.

- [ ] Change 4: Translate all 92 migration entries and documented variants: 62 aliases, 20 canonical entries, two retired forms, eight machine ABIs delegated to T01. Keep --refresh scoped to root/check; old proxy status --port routes to health; old bare proxy pin produces provider guidance. Preserve exact deprecation once, parameter values and canonical error path.

- [ ] Change 5: Test canonical/alias pairs against the same Invocation; cover every legacy flag/positional mapping and rejected extra tails. Mark internal annotation only for legacy Claude UUID with the internal-only Invocation.LegacySelector string annotation; never expose it as a public option or infer it from canonical account text.

**Implementation sequence:**

```text
1. Implement token scanning in the exact README.md order: identify globals before -- without swallowing option values; resolve complete command path; apply local options only at that leaf; bind positionals; validate types and combinations.
2. Implement duration, integer, uint32, percentage, digest, timestamp, account-reference, path and UTF-8 byte checks from each parameter definition.
3. Encode cross-option constraints as a table keyed by canonical path with Go predicates.
4. Translate all 92 migration entries and documented variants: 62 aliases, 20 canonical entries, two retired forms, eight machine ABIs delegated to T01.
5. Test canonical/alias pairs against the same Invocation; cover every legacy flag/positional mapping and rejected extra tails.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: parsed canonical commands and aliases` and a concise unordered body describing significant changes. Do not push or merge.

Interface amendment fixed here: add `LegacySelector string` to Invocation in types.go, empty normally and `claude_uuid` only for the explicitly translated old Claude pin selector. This is the sole legacy-only semantic annotation; T13 consumes it. Both coordinator and this task must remain consistent.

T03 parser acceptance is recorded in `internal/cli/testdata/parser-cases.json`, `compatibility.json` and `parser-coverage.json`. The coverage test compares all current catalogue clauses verbatim, executes each referenced parser fixture and checks each deferred semantic owner against `coverage.json`; unknown or unclassified clauses fail. Mixed lexical/state clauses retain both responsibilities. Required confirmation booleans remain semantic exit-6 preconditions, while unavailable candidate commands stop after syntax validation. The retired recovery route returns its mandated exit 4 through `ParseError`, including help. Compatibility-only pin guidance does not extend the executable catalogue. Unchanged legacy paths with consumed duration positionals do not acquire an invented deprecation warning; actual old options and changed paths retain the exact migration warnings.

## T04 — Implement output, consent, operation budgets and contract fixtures

**Dependencies:** T03.

**Files:** Create `internal/cli/run.go`, `internal/cli/json.go`, `internal/cli/budget.go`, `internal/cli/consent.go` and corresponding `internal/cli/_test.go` files. Create `cmd/cq/cli_v2_contract_test.go`. Define family fixture registration in that test helper; real production composition remains T27.

**Interfaces:** Produces Run, WriteJSON, EncodeData, BeginBudget/Budget and Confirm with exact coordinator signatures, plus runV2Case and v2Case test contract. Existing family engines remain dependency injected. BuildInfo feeds VersionOutcome; lookup is not consulted for pure presentation.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** no-access rejects every dependency. Budget fixture injects monotonic clock/timers with a 30s total/5s cleanup reserve; spends 20s preparing, pauses 60s only for consent, resumes, spends 5s work and leaves 5s cleanup. A parent deadline expiring during consent remains expired. Output fixture uses a writer failing after a selected byte count.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2RejectsMutationTail(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "unknown tail before access", Scenario: "no-access",
        Args: []string{"codex", "account", "remove", "alice@example.com", "--dry-run", "--json"},
        Exit: 2, Code: "unknown_option", Command: "codex account remove", WantJSON: `null`,
        Forbid: []string{"credentials", "network", "filesystem-write", "service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./internal/cli ./cmd/cq -run 'TestCLIV2(Output|Budget|Consent|RejectsMutationTail|Interrupt)'
go vet ./internal/cli ./cmd/cq
```

- [ ] Change 1: Implement one terminal envelope with explicit [] errors/warnings and data null when no resource exists, canonical command string, no ANSI and LF termination. Preserve exact stderr diagnostic/deprecation format. Output encode/write failure exits1 and must not retry a mutation or print a second success document.

- [ ] Change 2: Render human templates with exact lowercase booleans, em-dash nulls, deterministic array order and Unicode Cc control escaping as literal \uNNNN. Test C0/C1/DEL, ordinary Unicode, locale-independent decimals, path/option deprecation suppression and warning order. JSON and human output must describe the same operational scope.

- [ ] Change 3: Implement Run ordering: Parse -> pure help/version/group/completion -> handler lookup -> handler -> one renderer. Use Outcome.Streamed only when the handler already wrote its terminal record; intermediate JSONL records do not mark completion. Preserve hook raw protocol as its explicit renderer branch.

- [ ] Change 4: Implement Budget as remaining monotonic duration plus original parent context. Work deadline reserves cleanup inside the total; Cleanup deadline is total remaining. Pause requires quiescent work, stops timers, preserves remainder; Resume creates fresh contexts bounded by original parent. No cleanup extension after expiry.

- [ ] Change 5: Implement Confirm with exact per-command prompt supplied by handler, stderr only, one normalised yes/no response per specification, EOF=false. JSON/noninteractive checks happen before calling it. Interrupt maps to130 with truthful partial data, even if a terminal operation receipt exists.

- [ ] Change 6: Implement runV2Case using registered fixture constructors that inject real engines and tally boundary calls. Assert recursive JSON subsets plus exact envelope keys, one terminal output, matching canonical command, error code and no fixture secret leakage. Fixture constructors must fail loudly if unavailable rather than silently skip.

**Implementation sequence:**

```text
1. Implement one terminal envelope with explicit [] errors/warnings and data null when no resource exists, canonical command string, no ANSI and LF termination.
2. Render human templates with exact lowercase booleans, em-dash nulls, deterministic array order and Unicode Cc control escaping as literal \uNNNN.
3. Implement Run ordering: Parse -> pure help/version/group/completion -> handler lookup -> handler -> one renderer.
4. Implement Budget as remaining monotonic duration plus original parent context.
5. Implement Confirm with exact per-command prompt supplied by handler, stderr only, one normalised yes/no response per specification, EOF=false.
6. Implement runV2Case using registered fixture constructors that inject real engines and tally boundary calls.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: unified command output and deadlines` and a concise unordered body describing significant changes. Do not push or merge.

## T05 — Unify lazy environment and client path resolution

**Dependencies:** T04.

**Files:** Modify `internal/userdirs/dirs.go`, `internal/userdirs/dirs_unix.go`, `internal/userdirs/dirs_windows.go`, related tests; `internal/compat/epoch_path_unix.go`, `internal/compat/epoch_path_windows.go`; `internal/cache/dir.go`; `internal/fsutil/fs.go`; `internal/proxy/config.go`, `internal/proxy/models.go`; `cmd/cq/models.go`, `cmd/cq/registry_pipeline.go`, `cmd/cq/codex_version.go`. Create `internal/userdirs/client_paths.go`, `internal/userdirs/client_paths_test.go` for shared reader/publisher paths.

**Interfaces:** Consumes existing userdirs.Resolver.Resolve and authenticated Windows subject-anchor APIs. Produces `ClientPaths{CodexModels string; CodexVersion string; ClaudeCapabilities string}` and `ResolveClientPaths(provider string, cwd string, home string, getenv func(string) string) (ClientPaths, error)`. Provider is exactly claude or codex; resolve only that provider, leaving unrelated fields empty. Existing Resolver remains root authority. No caller may resolve unused provider roots.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** roots-invalid fixture supplies relative XDG_CONFIG_HOME and records root/credential/network calls; model-path fixture supplies a relative CLAUDE_CONFIG_DIR under a temporary command-start cwd and a distinct native credential home. Windows fixture poisons USERPROFILE while the authenticated subject anchor points to a safe temp root.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2EnvironmentIsLazy(t *testing.T) {
    t.Setenv("HOME", "relative-home")
    t.Setenv("XDG_CONFIG_HOME", "relative-config")
    got, ok := cli.Help("codex account list")
    if !ok || got == "" { t.Fatal("help required valid environment") }
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./internal/userdirs ./internal/compat ./internal/cache ./internal/proxy ./cmd/cq -run 'Test(CLIV2Environment|.*UserDirs|.*ClientPath|.*Cache.*TTL|.*Continuity.*Dir|.*Windows.*Anchor)'
go vet ./internal/userdirs ./internal/compat ./internal/cache ./internal/proxy ./cmd/cq
```

- [ ] Change 1: Implement environment.md precedence exactly, including empty/unset, literal spaces, no shell expansion, relative public paths resolved once, strict owned-state roots, and unavailable HOME without temporary persistent-state fallback. Return environment_path_invalid2 or environment_root_unavailable4 without echoing values.

- [ ] Change 2: Route cache/config/state/runtime/log and compatibility epoch resolution through the same root result. Default continuity/readiness state is S=C/state on Unix, never C. Leave resilience root uninitialised until explicit state initialise. Reject unusable upstream schemes/hosts before serve.

- [ ] Change 3: Use shared client paths in both model readers and publishers. CODEX_HOME relocates model/version inputs only, not auth.json/accounts/credential ownership. CLAUDE_CONFIG_DIR relocates capability cache only, not credentials or global settings. Resolve one provider without touching the other provider root.

- [ ] Change 4: Preserve CQ_TTL Atoi/clamp semantics with table cases invalid, negative,0,3600,3601,+30 and overflow; do not substitute command timeout rules. Preserve OS subject-user anchors, Go destination validation, service environment authority and loopback transport rules.

- [ ] Change 5: Assert every derived-root row and all explicitly supported environment variables from environment.md using fake env/OS anchors. Test read-only paths create no directories; account/model/service fixtures consume the same resolved paths.

**Implementation sequence:**

```text
1. Implement environment.md precedence exactly, including empty/unset, literal spaces, no shell expansion, relative public paths resolved once, strict owned-state roots, and unavailable HOME without temporary persistent-state fallback.
2. Route cache/config/state/runtime/log and compatibility epoch resolution through the same root result.
3. Use shared client paths in both model readers and publishers.
4. Preserve CQ_TTL Atoi/clamp semantics with table cases invalid, negative,0,3600,3601,+30 and overflow; do not substitute command timeout rules.
5. Assert every derived-root row and all explicitly supported environment variables from environment.md using fake env/OS anchors.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `fix: unified command storage paths` and a concise unordered body describing significant changes. Do not push or merge.

## T06 — Adapt quota reporting without changing quota arithmetic

**Dependencies:** T05.

**Files:** Create `cmd/cq/cli_v2_check.go`, `cmd/cq/cli_v2_check_test.go`. Read `cmd/cq/main.go` runCheck as the existing composition reference; build the new adapter around the same Runner without modifying main until T27. Modify `internal/app/runner.go`, `internal/app/report.go` and `internal/output/tty_renderer.go`, `internal/output/tty_build.go`, `internal/output/tty_format.go`, `internal/output/json.go` only for context/outcome/rendering seams; retain frozen quota/aggregate algorithms.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Check(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `check` | `providers` | `--fresh`, `--timeout` | [Help](../help/check.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** quota-partial fixture injects one successful Codex account, one fetch_error account and deterministic time/cache. quota-exhausted gives a valid zero-remaining window. quota-timeout retains one completed row and blocks the second provider until budget cancellation.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2CheckContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "partial report is not success",
        Scenario: "quota-partial",
        Args: []string{"check", "codex", "--json"},
        Exit: 8,
        Command: "check",
        Code: "check_partial",
        Forbid: []string{"service", "consume", "credential-activate"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/app ./internal/output ./internal/aggregate -run 'Test(CLIV2Check|Runner|.*Report|.*Aggregate|.*Gauge)'
go vet ./cmd/cq ./internal/app ./internal/output ./internal/aggregate
```

- [ ] Change 1: Extract typed report production from runCheck without capturing stdout. Forward one budget to all provider workers and maintain panic recovery. Preserve user provider order, deterministic account sorting and unchanged frozen report metrics.

- [ ] Change 2: Apply --fresh to initial cache lookup only; stale fallback remains visible with partial exit8. Exhausted quota remains successful observation. Classify no-usable rows with timeout precedence, then authentication, operational failure, unavailable as specified.

- [ ] Change 3: Render QuotaReportHumanV2 exactly, including empty/missing windows and aggregate gauges. Preserve QuotaReportV1 nested inside data.report. Never call ensureAgent or start/install services from check.

- [ ] Change 4: Test bare cq/check all-three equivalence, explicit subset ordering, every error precedence, stale cache/fresh interaction and cancellation. Compare frozen numeric report fixtures without rounding or scheduling changes.

**Implementation sequence:**

```text
1. Extract typed report production from runCheck without capturing stdout.
2. Apply --fresh to initial cache lookup only; stale fallback remains visible with partial exit8.
3. Render QuotaReportHumanV2 exactly, including empty/missing windows and aggregate gauges.
4. Test bare cq/check all-three equivalence, explicit subset ordering, every error precedence, stale cache/fresh interaction and cancellation.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: adapted quota command contracts` and a concise unordered body describing significant changes. Do not push or merge.
