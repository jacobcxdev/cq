# CQ CLI v2 Cutover and Delivery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the complete canonical CLI and prepare verifiable release acceptance.

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

## T27 — Assemble the complete CLI and migrate repository consumers

**Dependencies:** T01, T02, T03, T04, T05, T06, T07, T08, T09, T10, T11, T12, T13, T14, T15, T16, T17, T18, T19, T20, T21, T22, T23, T24, T25, T26.

**Files:** Create `cmd/cq/cli_v2.go`, `cmd/cq/cli_v2_integration_test.go`, `cmd/cq/cli_v2_consumers_test.go`; modify `cmd/cq/main.go`, `cmd/cq/help.go`, `cmd/cq/proxy_commands.go` and old CLI structs/manual parsers only after all adapters exist. Update `README.md`, `CONTRIBUTING.md`, affected command docs, shell snippets, `.github/workflows/ci.yml`, `.github/workflows/.goreleaser.yml`, `scripts/validate-codex-release`, `scripts/verify-proxy-cu`, `scripts/build-proxy-release`, package/service template consumers. Remove Kong from go.mod/go.sum only when unused.

**Interfaces:** Produces `runCLIV2(ctx context.Context, argv []string, session *cli.Session) int` and complete map of89 leaf paths to pure utility/operational dispatch. main executes hidden ABI classifier first, then runCLIV2; service/runtime launch generators retain frozen machine argv. No temporary public feature flag.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** full-registry fixture injects all real family adapters and no-access dependencies for help/errors/unavailable commands. consumer-script fixture installs fake cq/gh/git/service tools on isolated PATH, uses recorded schema2 rescue/status JSON and fails if any real network/service tool is reached.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2EveryGroupIsPure(t *testing.T) {
    raw, err := os.ReadFile("../../specs/cli-v2/commands.json")
    if err != nil { t.Fatal(err) }
    var spec struct { Commands []struct { Path, Kind string } }
    if err := json.Unmarshal(raw, &spec); err != nil { t.Fatal(err) }
    for _, command := range spec.Commands {
        if command.Kind != "group" { continue }
        t.Run(command.Path, func(t *testing.T) {
            var out, diagnostics bytes.Buffer
            session := &cli.Session{In: strings.NewReader(""), Out: &out, Err: &diagnostics}
            lookup := func(string) (cli.Handler, bool) { panic("group performed handler lookup") }
            exit := cli.Run(context.Background(), strings.Fields(command.Path), session, lookup)
            want, ok := cli.Help(command.Path)
            if !ok || exit != 0 || out.String() != want || diagnostics.Len() != 0 {
                t.Fatalf("impure group help: exit=%d out=%q err=%q", exit, out.String(), diagnostics.String())
            }
        })
    }
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
python3 specs/cli-v2/generate.py --check --go-output internal/cli/catalogue_gen.go
python3 specs/cli-v2/plan/check.py
go build ./...
go vet ./...
go test -race -count=1 ./...
```

- [ ] Change 1: Assemble every family handler and utility path; assert exact set equality with catalogue before exposing main. Keep global parser/presentation before environment/compatibility epoch work. Remove old global ensureCompatibilityEpoch/ensureAgent side effects; call epoch validation only inside operations that require it after preflight.

- [ ] Change 2: Replace public dispatch in one change. Delete only superseded parser/help code proven unused by rg/build; retain ABI parsers and actual domain engines. Do not retain two public grammars or fall back to old handlers on parse failure. Remove Kong dependency only if no remaining imports.

- [ ] Change 3: Migrate all repository-owned command invocations to canonical spellings except frozen launch/installer ABI. In validate-codex-release production_snapshot, request --json, parse schema2 .data.mode with a preflighted existing supported JSON parser, and reject errors/missing mode. Preserve production PID/mode comparison and no-publication-on-failure behaviour.

- [ ] Change 4: Add hermetic consumer tests using fake PATH tools and synthetic envelopes, covering malformed JSON, ok=false, missing mode, changed PID/mode and forbidden publish calls. Do not run the real release script during local testing; it writes GitHub status and performs live operations.

- [ ] Change 5: Update examples/docs/completion installation instructions and CLI/envelope version1.0.0/schema2 release notes. Include old-to-new examples for reserve/policy/fallback/accounts/services, alias retention until2.0.0 and all four unavailable candidate commands.

- [ ] Change 6: Run full catalogue/alias/environment/output acceptance, every invalid option before IO, all127 help goldens and89 leaf registrations. Run required build/vet/full-race once on the integrated tree; preserve Linux native no-skip gates and Windows/macOS package/lifecycle jobs. Re-run only affected checks after new fixes.

**Implementation sequence:**

```text
1. Assemble every family handler and utility path; assert exact set equality with catalogue before exposing main.
2. Replace public dispatch in one change.
3. Migrate all repository-owned command invocations to canonical spellings except frozen launch/installer ABI.
4. Add hermetic consumer tests using fake PATH tools and synthetic envelopes, covering malformed JSON, ok=false, missing mode, changed PID/mode and forbidden publish calls.
5. Update examples/docs/completion installation instructions and CLI/envelope version1.0.0/schema2 release notes.
6. Run full catalogue/alias/environment/output acceptance, every invalid option before IO, all127 help goldens and89 leaf registrations.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: switched to canonical CLI v2` and a concise unordered body describing significant changes. Do not push or merge.

T27 is complete only when all supported command contracts pass and reserved4 fail without IO. The plan coverage checker checks ownership completeness, not behaviour; it cannot replace executable acceptance. Repository consumer discovery uses rg for string literals, including tests/templates/workflows, because symbol graphs do not index shell commands reliably.

## T28 — Prepare release evidence and gate authorised rollout

**Dependencies:** T27, T19, T20, T21.

**Files:** Create `specs/cli-v2/release-evidence.md` during implementation with immutable SHA/artifact hashes and gate table. Update release/package test configuration only where needed in `.github/workflows/ci.yml`, `.github/workflows/.goreleaser.yml`, `packaging/windows/cq.wxs`, `packaging/winget/jacobcxdev.cq.installer.yaml.tmpl`, `internal/installer/` and `cmd/cq-install/main_test.go`. Do not generate a release tag, upload assets or run real release automation under this planning request.

**Interfaces:** Consumes integrated CLI and native adapters; produces local1.0.0 release-candidate artifacts, digest manifest and evidence per OS/architecture. Later rollout requires explicit scope/host/version authority. Machine ABI and schema2 consumer checks apply to the packaged binary, not only go run.

**Normative command ownership:**

Shared infrastructure or delivery task; command/parameter ownership is assigned in [coverage.json](coverage.json).

**Fixture arrangement:** Package fixtures install into disposable authorised CI users/roots only. Native runtime evidence must identify OS/arch, user-service domain, executable SHA/digest, previous installation snapshot and restoration target. No real reset Consume is required or permitted by CLI acceptance.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
python3 specs/cli-v2/generate.py --check --go-output internal/cli/catalogue_gen.go
python3 specs/cli-v2/plan/check.py
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
python3 specs/cli-v2/plan/check.py
python3 specs/cli-v2/generate.py --check
```

- [ ] Change 1: Build local release candidates for each existing supported release OS/architecture using current package tooling; record version1.0.0, source revision, dirty provenance, artifact SHA-256 and embedded127-help/command-schema check. Adapt patch-derived CI test-version logic deliberately for1.0.0 rather than accidentally minting0.32.x.

- [ ] Change 2: Keep native macOS launchd/quarantine/cask, Linux namespace/Landlock/descendant-cancellation and Windows scheduler/WiX/credential ownership gates. Cross-compilation proves build only. Mark unavailable native runners unverified and block a claim of platform runtime acceptance; do not relabel skipped tests as passes.

- [ ] Change 3: In explicitly authorised isolated native environments, execute lifecycle sequence and selected-component matrix below, including package upgrade failure rollback. Record definitions, enablement, process identity, lock/owner authority and new periodic-refresh completion evidence.

- [ ] Change 4: Prepare exact rollout/rollback commands and target artifact before requesting any missing live/publication authority. Preserve old installation/snapshot and show what will change. Do not push, merge, tag, publish, write GitHub status or mutate installed service until that later authority is explicit.

- [ ] Change 5: After authorised installation, prove installed executable revision and real HTTP200 plus WebSocket101, zero503 and zero continuity mismatch on the approved test flow. Health/status alone do not close transport acceptance. If restoration or validation fails, stop that rollout and retain evidence; do not consume a reset as a workaround.

**Implementation sequence:**

```text
1. Build local release candidates for each existing supported release OS/architecture using current package tooling; record version1.0.0, source revision, dirty provenance, artifact SHA-256 and embedded127-help/command-schema check.
2. Keep native macOS launchd/quarantine/cask, Linux namespace/Landlock/descendant-cancellation and Windows scheduler/WiX/credential ownership gates.
3. In explicitly authorised isolated native environments, execute lifecycle sequence and selected-component matrix below, including package upgrade failure rollback.
4. Prepare exact rollout/rollback commands and target artifact before requesting any missing live/publication authority.
5. After authorised installation, prove installed executable revision and real HTTP200 plus WebSocket101, zero503 and zero continuity mismatch on the approved test flow.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `docs: recorded CLI release acceptance` and a concise unordered body describing significant changes. Do not push or merge.

T28 has two checkpoints: local release candidate/evidence preparation, then separately authorised publication and installed acceptance. CLI implementation completion is T00–T27; do not leave those unfinished solely because publication authority is absent.

Native lifecycle sequence, run only against an approved disposable user installation with its built candidate:

```sh
cq service install --component all --json
cq service status --component all --strict --json
cq service stop --component proxy --json
cq service status --component proxy --json
cq service start --component proxy --json
cq service stop --component token-refresh --json
cq service restart --component token-refresh --json
cq service status --component token-refresh --json
cq service start --component token-refresh --json
cq service uninstall --component all --json
```

Assert unselected state after every selected operation. Disabled token-refresh restart must show a new successful completion while remaining disabled/unhealthy. Repeat with selective install/uninstall and injected second-component failure/rollback. Never run this sequence against the user's current installation as an ordinary unit test.
