# Codex Backend-Window Priming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically start each backend-reported Codex quota window after reset with one account-pinned synthetic request, then verify exact reset-epoch advancement without manual traffic or duplicate admission.

**Architecture:** Preserve backend window descriptors, resolve each scoped window to a safe registry model, coalesce compatible due windows into activation targets, and run one durable scheduler owned by the proxy. Scheduler refreshes usage before dispatch, claims each account/window generation atomically, sends a minimal native Responses request through existing pinned candidate routing, and verifies advancement by polling usage only.

**Tech Stack:** Go 1.26.1, standard library HTTP/JSON/time/filesystem packages, existing Codex parser, model registry, proxy request router, credential coordinator, atomic journal patterns, and Go race detector.

---

## Execution contract

- Default disabled. No production model traffic before all fake-clock, crash, race, and integration tests pass.
- Backend defines windows. Configuration never contains duration or window-name lists.
- Synthetic dispatch never joins user leases, crosses accounts, activates system auth, or writes credential stores.
- One admitted or ambiguous generation gets no replay. Later work is usage verification only.
- Live acceptance uses untouched naturally reset account. Reads are allowed; no manual model request is allowed.
- Exact `ResetAt` advancement proves activation. Remaining percentage does not.

## Task 1: Preserve backend window descriptors

**Files:**

- Create: `internal/provider/codex/window_descriptor.go`
- Create: `internal/provider/codex/window_descriptor_test.go`
- Modify: `internal/provider/codex/parser.go`
- Modify: `internal/provider/codex/parser_test.go`

- [ ] Add failing parser tests for primary, secondary, additional Spark, missing reset epoch, duplicate raw names, changed period, and unknown future window duration.
- [ ] Add domain types:

```go
type WindowScopeKind uint8

const (
    WindowScopeShared WindowScopeKind = iota + 1
    WindowScopeModelFamily
)

type WindowDescriptor struct {
    RawLimitName string
    WindowName   quota.WindowName
    Period       time.Duration
    ScopeKind    WindowScopeKind
    Scope        string
    ResetAt      time.Time
    RemainingPct float64
}
```

- [ ] Extend parsed Codex usage with descriptors while preserving existing `quota.Result` JSON and display behaviour.
- [ ] Derive primary/secondary as shared. Derive every `additional_rate_limits[].limit_name` as model-family scope while preserving exact raw backend spelling.
- [ ] Exclude malformed descriptors from scheduling but keep existing quota error/display behaviour.
- [ ] Do not infer scheduler scope from canonical `WindowName` text.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex -run 'WindowDescriptor|ParseUsage'
```

## Task 2: Resolve models and coalesce activation targets

**Files:**

- Create: `internal/proxy/codex_primer_model.go`
- Create: `internal/proxy/codex_primer_model_test.go`
- Create: `internal/proxy/codex_primer_plan.go`
- Create: `internal/proxy/codex_primer_plan_test.go`
- Inspect: `internal/modelregistry/entry.go`
- Inspect: `internal/modelregistry/catalog.go`

- [ ] Add failing resolver tests for exact ID, alias, display name, case folding, unique token-boundary family match, ambiguous family, invisible model, explicit override, invalid override, and unresolved scope.
- [ ] Add failing planner tests proving:
  - shared 5h plus shared 7d at same reset epoch becomes one target;
  - due Spark plus shared windows at same epoch becomes one Spark target;
  - Spark and another family become separate targets;
  - equal duration with different reset epochs stays separate;
  - current Codex shared-only policy prefers visible registry Spark;
  - shared-only fallback uses deterministic registry preference;
  - one unresolved scoped window fails closed without blocking unrelated resolvable targets.
- [ ] Resolve in strict order: exact configured raw-scope override, exact case-folded ID/alias/display, unique token-boundary family, explicit provider adapter, typed unresolved result.
- [ ] Reject arbitrary substring matching and ambiguous families.
- [ ] Use registry priority/version ordering only as deterministic preference. Do not describe selection as cheapest because registry has no price authority.
- [ ] Key activation groups by exact reset epoch plus selected model family. Record every descriptor expected to advance.
- [ ] Run:

```bash
go test -race -count=1 ./internal/proxy -run 'PrimerModel|PrimerPlan'
```

## Task 3: Add durable generation journal

**Files:**

- Create: `internal/proxy/codex_primer_store.go`
- Create: `internal/proxy/codex_primer_store_test.go`

- [ ] Add failing state-machine tests for observed, due, claimed, pre-admission-rejected, admitted, ambiguous, verifying, verified, primed-externally, unresolved, and terminal failure.
- [ ] Add failing crash tests for temp write, file sync, rename, directory sync, restart recovery, duplicate owner, corrupt state, unknown enum, stale generation, and concurrent claim.
- [ ] Persist only:

```go
type PrimerRecord struct {
    AccountHash  string
    ScopeHash    string
    ResetAt      time.Time
    ModelID      string
    State        PrimerState
    Generation   uint64
    NextCheckAt  time.Time
    ResultCode   string
}
```

- [ ] Use installation HMAC for account/scope identity. Store no email, account ID, token, external path, prompt, response, or raw backend scope.
- [ ] Commit under CQ state directory with `0o700` directory, `0o600` file, unique temp, file sync, atomic rename, and directory sync.
- [ ] Make claim compare-and-set generation-fenced. `admitted` and `ambiguous` states can transition only to verification/result states, never back to dispatchable.
- [ ] Keep verified records long enough to distinguish restart from new backend reset epoch; compact only generations superseded by a newer exact epoch.
- [ ] Run:

```bash
go test -race -count=100 ./internal/proxy -run 'PrimerStore'
```

## Task 4: Build pinned usage reader

**Files:**

- Create: `internal/proxy/codex_primer_usage.go`
- Create: `internal/proxy/codex_primer_usage_test.go`
- Modify: `internal/provider/codex/parser.go`
- Modify: `internal/provider/codex/parser_test.go`

- [ ] Add failing tests for exact-account success, same-identity candidate fallback, eligible CQ refresh, 401 exhaustion, malformed response, body limit, timeout, and forbidden cross-account failover.
- [ ] Export a narrow safe Codex usage parser returning quota result plus window descriptors; keep HTTP ownership in proxy.
- [ ] Fetch backend usage through `CodexRequestRouter.DoPinned` using exact `RouteChoice` and bounded response reads.
- [ ] Preserve account pin through every candidate retry. Never call selector for another account.
- [ ] Return typed observation with exact reset epochs and source generation.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex ./internal/proxy -run 'PrimerUsage|ParseUsage'
```

## Task 5: Build synthetic Responses executor

**Files:**

- Create: `internal/proxy/codex_primer_request.go`
- Create: `internal/proxy/codex_primer_request_test.go`
- Reuse: `internal/proxy/codex_sse.go`
- Reuse: `internal/proxy/codex_attempt.go`

- [ ] Add failing request-shape tests proving native `/responses`, exact selected model, `store:false`, empty tools, bounded minimal `ping` input, CQ synthetic metadata, no continuation, no task identifiers, and no user lease acquisition.
- [ ] Add failing lifecycle tests for definite pre-admission rejection, parsed `response.created` admission, completion, stream error after admission, EOF after bytes with ambiguous admission, timeout, same-identity 401 fallback, and no cross-account retry.
- [ ] Construct minimal request:

```json
{
  "model": "selected-registry-id",
  "instructions": "Reply with pong.",
  "input": [{"role":"user","content":[{"type":"input_text","text":"ping"}]}],
  "tools": [],
  "tool_choice": "auto",
  "parallel_tool_calls": false,
  "store": false,
  "stream": true,
  "client_metadata": {"cq.synthetic":"window-primer-v1"}
}
```

- [ ] Send through exact-account `CodexRequestRouter.DoPinned`. Use bounded request, response, event, and total time limits.
- [ ] Classify admission conservatively: well-formed `response.created` admits; bytes/transport ambiguity after dispatch becomes ambiguous; explicit rejection before admission may be retryable under scheduler policy.
- [ ] Discard bounded response content. Persist no prompt or output.
- [ ] Run:

```bash
go test -race -count=1 ./internal/proxy -run 'PrimerRequest'
```

## Task 6: Implement scheduler and verification loop

**Files:**

- Create: `internal/proxy/codex_primer.go`
- Create: `internal/proxy/codex_primer_test.go`

- [ ] Add fake-clock failing tests for future reset wake, already-due untouched generation, external priming before wake, coalesced dispatch, restart before claim, restart after admission, ambiguous outcome, verification lag, exact epoch advance, percentage-only change, account removal, model disappearance, source auth rotation, graceful shutdown, and two scheduler instances.
- [ ] Scheduler flow:
  1. list routable logical accounts;
  2. read each account's usage;
  3. persist observed descriptors/plans;
  4. wake at earliest due epoch;
  5. reread exact account usage;
  6. record `primed_externally` when epoch advanced;
  7. claim generation atomically;
  8. dispatch once;
  9. transition admitted/ambiguous to verification-only;
  10. poll usage with bounded backoff until every target descriptor advances.
- [ ] Allow bounded retry only for typed definite pre-admission rejection. Record attempt count and deadline in generation state.
- [ ] Start scheduler only in credential-coordinator owner process. Delegates and second proxy instances remain read-only observers.
- [ ] Recover journal before observing current usage. Never let a stale usage response regress epoch or state.
- [ ] Emit privacy-safe metrics/status: counts, state, model ID, reset time, typed error. No account/scope identifiers.
- [ ] Run:

```bash
go test -race -count=100 ./internal/proxy -run 'PrimerScheduler|PrimerStore|PrimerPlan'
```

## Task 7: Add config, command, and service wiring

**Files:**

- Modify: `internal/proxy/config.go`
- Modify: `internal/proxy/config_test.go`
- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/proxy_test.go`
- Modify: `cmd/cq/help.go`
- Modify: `cmd/cq/help_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`

- [ ] Add failing config round-trip tests for omitted/default-disabled, enabled, exact raw-scope overrides, invalid model ID, and unknown-field preservation.
- [ ] Add additive config:

```go
type CodexWindowPrimingConfig struct {
    Enabled        bool              `json:"enabled"`
    ModelOverrides map[string]string `json:"model_overrides,omitempty"`
}
```

- [ ] Add `cq proxy prime status`, `cq proxy prime enable`, and `cq proxy prime disable`. Commands update config atomically and report restart requirement consistently with existing proxy lifecycle.
- [ ] Validate override model IDs against current provider registry at startup. Keep unresolved backend scopes as runtime typed status, not startup failure.
- [ ] Start primer after credential coordinator, inventory, registry, and request router are ready. Stop and await it during proxy shutdown.
- [ ] Expose primer summary in local status/health without secret or account identity.
- [ ] Run:

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Primer|Config|Help|Proxy'
```

## Task 8: Full verification before enablement

**Files:**

- No new production files.

- [ ] Run:

```bash
go test -race -count=100 ./internal/proxy -run 'Primer'
go test -race -count=1 ./internal/provider/codex ./internal/proxy ./cmd/cq
go test -race -count=1 ./...
go build ./...
go vet ./...
git diff --check
```

- [ ] Secret-scan changed files and inspect every credential schema match.
- [ ] Snapshot credential/system/registry/CodexBar file hashes and metadata. Preserve no content in Git.
- [ ] Confirm installed config still has priming disabled.

## Task 9: Automatic-only installed acceptance

**Files:**

- No repository changes required unless a diagnosed failure needs a test-first fix.

- [ ] Read untouched account usage through usage endpoint only. Record privacy-safe exact reset-epoch hash/state; do not issue Responses traffic.
- [ ] Install tested CQ build and restart owned proxy service with priming still disabled.
- [ ] Enable priming through CQ config command and restart service. Do not call primer request executor directly and do not send any manual Codex message on target account.
- [ ] Observe journal/status until scheduler itself claims target generation.
- [ ] If scheduler fails before dispatch, preserve untouched generation, add failing regression test, fix, reinstall, and let scheduler retry under tested pre-admission policy.
- [ ] If admission or ambiguity occurs, never replay. Diagnose with journal/status and continue verification polling only.
- [ ] Prove exact backend reset epoch advances and countdown starts for scheduler-originated generation.
- [ ] Re-snapshot credential/system/registry/CodexBar files and prove byte identity.
- [ ] Record build SHA, scheduler state transitions, exact-epoch advancement result, and no-manual-request attestation in privacy-safe local evidence.
