# CQ CLI v2 Accounts and Models Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adapt account, reset, model and credential-refresh commands with precise outcomes.

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

## T07 — Expose read-only account inventory and stable references

**Dependencies:** T05.

**Files:** Create `cmd/cq/cli_v2_accounts.go`, `cmd/cq/cli_v2_accounts_test.go`. Modify `internal/app/accounts.go`, `internal/provider/codex/accounts.go`, `internal/provider/codex/account_reference.go`, `internal/provider/claude/accounts.go`, `internal/provider/gemini/provider.go` and corresponding tests only for read-only typed inventory seams.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2AccountInspection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Reuse existing ResolveAccountReference and complete-inventory APIs; T08–T10/T13/T15 consume their resolved identity rather than independently matching emails. Expose a typed AccountSummary DTO in cli_v2_accounts.go with the exact accounts.md fields.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `claude account list` | None | `--timeout` | [Help](../help/claude-account-list.txt) |
| `codex account list` | None | `--timeout` | [Help](../help/codex-account-list.txt) |
| `gemini account show` | None | `--timeout` | [Help](../help/gemini-account-show.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** accounts-readonly fixture provides duplicate email identities with distinct stable references, one system identity and one external read-only source. Deny all writes/recovery/service/network operations for plain inventory. accounts-unavailable returns a complete inventory discovery failure.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2AccountInspectionContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "inventory failure remains unavailable",
        Scenario: "accounts-unavailable",
        Args: []string{"codex", "account", "list", "--json"},
        Exit: 4,
        Command: "codex account list",
        Code: "account_inventory_unavailable",
        Forbid: []string{"filesystem-write", "service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/app ./internal/provider/codex ./internal/provider/claude ./internal/provider/gemini -run 'Test(CLIV2AccountInspection|.*AccountReference|.*DiscoverAccounts|.*AccountAlias)'
go vet ./cmd/cq ./internal/app ./internal/provider/...
```

- [ ] Change 1: Project existing RunAccounts/accountListManager inventory into AccountSummary without invoking credential coordinator recovery. Mark native default, activatable/removable and read-only source status from actual authority; do not equate active system account with proxy pin.

- [ ] Change 2: Keep Codex opaque references distinct from email. Resolve against complete inventory and preserve not-found/ambiguous/unstable distinctions. Claude uses exact unique email; Gemini reports one externally managed account and gains no login/activate/remove commands.

- [ ] Change 3: Implement deterministic sorting and exact null/false representation. Read-only inspection must not bootstrap dirs or persist anonymous reconciliation. Propagate deadline through inventory without inventing credentials.

- [ ] Change 4: Test all three inspection leaves, duplicate emails, missing metadata, incomplete inventory, read-only sources and no access to unrelated providers. Reuse dirty-work regression cases identified in T00 rather than applying whole patches.

**Implementation sequence:**

```text
1. Project existing RunAccounts/accountListManager inventory into AccountSummary without invoking credential coordinator recovery.
2. Keep Codex opaque references distinct from email.
3. Implement deterministic sorting and exact null/false representation.
4. Test all three inspection leaves, duplicate emails, missing metadata, incomplete inventory, read-only sources and no access to unrelated providers.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: exposed precise account inventory` and a concise unordered body describing significant changes. Do not push or merge.

## T08 — Adapt login, activation and removal transactions

**Dependencies:** T07.

**Files:** Create `cmd/cq/cli_v2_account_mutations.go`, `cmd/cq/cli_v2_account_mutations_test.go`. Modify `internal/app/accounts.go`; `internal/provider/codex/accounts.go`, `internal/provider/codex/credential_coordinator.go`, `internal/provider/codex/credential_control.go`, `internal/provider/codex/system_activator.go` and removal/registry coordinator files reached by SaveLogin/Activate/RemoveManaged; `internal/provider/claude/accounts.go`. Keep transaction journals and ownership formats.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2AccountMutation(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consumes T07 identity resolution and T04 Confirm/Budget. Call CredentialAdmin.SaveLogin/Activate, Accounts.switchThroughCoordinator/removeThroughCoordinator or ClaudeAccounts.Switch/Remove; never assign a new system account by editing files in the CLI.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `claude account activate` | `account` | `--timeout` | [Help](../help/claude-account-activate.txt) |
| `claude account login` | None | `--activate`, `--timeout` | [Help](../help/claude-account-login.txt) |
| `claude account remove` | `account` | `--yes`, `--timeout` | [Help](../help/claude-account-remove.txt) |
| `codex account activate` | `account` | `--timeout` | [Help](../help/codex-account-activate.txt) |
| `codex account login` | None | `--activate`, `--timeout` | [Help](../help/codex-account-login.txt) |
| `codex account remove` | `account` | `--yes`, `--timeout` | [Help](../help/codex-account-remove.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** login-activation-fails fixture completes OAuth and durable account save, then injects activation conflict; data must say credentials_saved=true and activated=false. remove-no-consent supplies a removable account but JSON mode without --yes. remove-external offers an unowned credential source.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2AccountMutationContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "saved login reports partial activation",
        Scenario: "login-activation-fails",
        Args: []string{"codex", "account", "login", "--activate", "--json"},
        Exit: 8,
        Command: "codex account login",
        Code: "account_login_partial",
        WantJSON: `{"credentials_saved":true,"activated":false}`,
        Forbid: []string{"service", "consume"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/app ./internal/provider/codex ./internal/provider/claude -run 'Test(CLIV2AccountMutation|.*SaveLogin|.*Activate|.*RemoveManaged|.*Removal|.*Switch)'
go vet ./cmd/cq ./internal/app ./internal/provider/codex ./internal/provider/claude
```

- [ ] Change 1: Adapt login without activation by default; --activate requests native-default selection only after save. Preserve OAuth browser/timeout rules, credential ownership, broker outcomes and source refresh rules. Report durable save even when subsequent activation fails.

- [ ] Change 2: For activate, resolve once against complete inventory then revalidate identity through the existing transaction coordinator. Preserve account_unstable/ambiguous/not_activatable and truthful activation_partial; do not modify proxy routing selections.

- [ ] Change 3: For remove, resolve and preview before terminal consent, require --yes in JSON/nonterminal modes, and revalidate selected identity after consent. Keep external sources intact, refuse unsupported system/read-only removal and use existing journalled cleanup. Do not choose a replacement account implicitly.

- [ ] Change 4: Test each provider/action with success, injected transaction failure and edge case: duplicate identity, vanished source, cancellation, partial credential removal, EOF and delayed confirmation. Verify caller deadline includes locks and post-save checks; consent wait alone pauses it.

**Implementation sequence:**

```text
1. Adapt login without activation by default; --activate requests native-default selection only after save.
2. For activate, resolve once against complete inventory then revalidate identity through the existing transaction coordinator.
3. For remove, resolve and preview before terminal consent, require --yes in JSON/nonterminal modes, and revalidate selected identity after consent.
4. Test each provider/action with success, injected transaction failure and edge case: duplicate identity, vanished source, cancellation, partial credential removal, EOF and delayed confirmation.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified account mutation outcomes` and a concise unordered body describing significant changes. Do not push or merge.

## T09 — Adapt reset inventory and advisory scheduling

**Dependencies:** T07.

**Files:** Create `cmd/cq/cli_v2_resets.go`, `cmd/cq/cli_v2_resets_test.go`. Modify `cmd/cq/codex_resets.go`, `internal/app/codex_resets.go`, `internal/provider/codex/reset_accounts.go`, `internal/provider/codex/reset_credits.go` and tests for typed projection/context only. Preserve frozen reset scheduling annex algorithms.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2ResetInspection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume CodexResetApp.List/Recommend, backendSnapshot/ListCredits, ProjectVisibleAccounts and publicCodexResetSchedule. T10 shares this inventory/selection projection without introducing cache-based consume authority.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex reset list` | `account` | `--timeout` | [Help](../help/codex-reset-list.txt) |
| `codex reset recommend` | None | `--timeout` | [Help](../help/codex-reset-recommend.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** reset-inventory-partial fixture has two visible accounts, one fresh credit list and one broker failure. reset-recommend-boundaries fixes time, pool quota and credit expiry to cover expiry/current-window ties. Neither fixture exposes Consume.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ResetInspectionContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "partial inventory exposes incompleteness",
        Scenario: "reset-inventory-partial",
        Args: []string{"codex", "reset", "list", "--json"},
        Exit: 8,
        Command: "codex reset list",
        Code: "codex_reset_inventory_partial",
        WantJSON: `{"complete":false}`,
        Forbid: []string{"consume", "credential-activate", "service"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/app ./internal/provider/codex -run 'Test(CLIV2ResetInspection|.*Reset.*(List|Recommend|Schedule|Inventory)|.*ProjectVisibleAccounts)'
go vet ./cmd/cq ./internal/app ./internal/provider/codex
```

- [ ] Change 1: Always fetch the required fresh reset/usage inputs; CQ_TTL does not authorise reset-cache reuse. Keep partial account/credit errors visible instead of dropping rows. Optional account selection uses T07 complete-inventory rules.

- [ ] Change 2: Project ResetAccountInventory and ResetSchedule exactly, including nullable deadlines, unavailable capacities and provider-reported credit eligibility. Preserve underlying recommendation arithmetic, ties and selected account ordering from the frozen annex.

- [ ] Change 3: Never consume a credit, switch native account, rearm reserve or write a fabricated reset observation from recommend/list. Surface advisory recommendations with explicit incomplete exit8 when any required input fails.

- [ ] Change 4: Test empty inventory, ambiguous selected account, expired/ineligible credits, multiple windows, partial broker failures, authentication precedence and total deadline. Retain pre-existing scheduling regression tests unchanged.

**Implementation sequence:**

```text
1. Always fetch the required fresh reset/usage inputs; CQ_TTL does not authorise reset-cache reuse.
2. Project ResetAccountInventory and ResetSchedule exactly, including nullable deadlines, unavailable capacities and provider-reported credit eligibility.
3. Never consume a credit, switch native account, rearm reserve or write a fabricated reset observation from recommend/list.
4. Test empty inventory, ambiguous selected account, expired/ineligible credits, multiple windows, partial broker failures, authentication precedence and total deadline.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: adapted reset inspection commands` and a concise unordered body describing significant changes. Do not push or merge.

## T10 — Preserve explicit, idempotent reset consumption

**Dependencies:** T09.

**Files:** Create `cmd/cq/cli_v2_reset_use.go`, `cmd/cq/cli_v2_reset_use_test.go`. Modify `internal/app/codex_resets.go`, `internal/provider/codex/reset_credits.go`, `internal/provider/codex/reset_accounts.go`, `internal/provider/codex/reset_attempts.go` and their matching test files. Keep the existing persistent attempt schema and credential affinity.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2ResetUse(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Consume existing PrepareUse/ExecuteUse and ResetAttemptStore.Pending/Ensure/Remove. The CLI never generates an unrelated retry attempt after an uncertain Consume result.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `codex reset use` | `account` | `--credit`, `--yes`, `--timeout` | [Help](../help/codex-reset-use.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** reset-no-consent fixture has one eligible credit and current windows; JSON without --yes stops before Consume. reset-indeterminate fixture persists an attempt then times out after upstream may have accepted; replay uses exactly the same credit/idempotency key/credential identity. reset-postcheck-fails returns reset then fails usage refresh.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ResetUseContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "JSON does not authorise consumption",
        Scenario: "reset-no-consent",
        Args: []string{"codex", "reset", "use", "alice@example.com", "--json"},
        Exit: 6,
        Command: "codex reset use",
        Code: "codex_reset_confirmation_required",
        Forbid: []string{"consume", "credential-activate", "service"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/app ./internal/provider/codex -run 'Test(CLIV2ResetUse|.*Reset.*(Use|Attempt|Consume|Retry|Postcheck))'
go vet ./cmd/cq ./internal/app ./internal/provider/codex
```

- [ ] Change 1: Prepare selection and display exact credit/account/window impact; honour --credit and --yes with T04 consent semantics. Recheck eligibility/identity after consent without changing selected credit behind the user.

- [ ] Change 2: Persist the existing attempt before Consume; fail closed when attempt persistence is unavailable or pending state is ambiguous. Reuse unresolved logical attempt exactly. Never automatically consume another credit or retry with another account/credential after ambiguity.

- [ ] Change 3: Report cancelled/reset/already_redeemed/nothing_to_reset/no_credit/indeterminate from actual provider outcomes. Postcheck failure preserves a successful reset with exit8 and truthful changed-window/retry fields; attempt cleanup failure cannot erase known upstream outcome.

- [ ] Change 4: Test timeout before send versus ambiguous after send, process-restart retry identity, missing/expired selected credit, EOF cancellation, failed confirmation, postcheck/cleanup failure and duplicate invocation. Assert Consume exact call counts and durable attempt content with synthetic secrets only.

**Implementation sequence:**

```text
1. Prepare selection and display exact credit/account/window impact; honour --credit and --yes with T04 consent semantics.
2. Persist the existing attempt before Consume; fail closed when attempt persistence is unavailable or pending state is ambiguous.
3. Report cancelled/reset/already_redeemed/nothing_to_reset/no_credit/indeterminate from actual provider outcomes.
4. Test timeout before send versus ambiguous after send, process-restart retry identity, missing/expired selected credit, EOF cancellation, failed confirmation, postcheck/cleanup failure and duplicate invocation.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: preserved explicit reset consumption` and a concise unordered body describing significant changes. Do not push or merge.

## T11 — Adapt model catalogue and overlay publication

**Dependencies:** T05.

**Files:** Create `cmd/cq/cli_v2_models.go`, `cmd/cq/cli_v2_models_test.go`. Modify `cmd/cq/models.go`, `cmd/cq/registry_pipeline.go`, `cmd/cq/local_registry.go`; `internal/modelregistry/merge.go`, `internal/modelregistry/infer.go`, `internal/modelregistry/validate.go`, `internal/modelregistry/overlay.go`, `internal/modelregistry/refresh.go`, `internal/modelregistry/projection_codex.go`, `internal/modelregistry/projection_claude.go`, `internal/modelregistry/projection_claudecode.go` and associated tests. Use T05 model-cache resolver.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Models(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Expose typed publication outcomes from the existing refresh pipeline; logs are diagnostics, never the success API. Define ModelEntry, ModelCloneSelectionResult and ModelPublication DTOs exactly in resources/models.md.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `models list` | None | `--provider` | [Help](../help/models-list.txt) |
| `models overlay add` | None | `--provider`, `--id`, `--clone-from` | [Help](../help/models-overlay-add.txt) |
| `models overlay prune` | None | None | [Help](../help/models-overlay-prune.txt) |
| `models overlay remove` | None | `--provider`, `--id` | [Help](../help/models-overlay-remove.txt) |
| `models refresh` | None | None | [Help](../help/models-refresh.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** models-native-shadow fixture has native and overlay entries with the same provider/id and distinct metadata; list must return native. models-publish-partial saves an overlay, publishes one client cache, fails another and keeps committed outcome. models-proxy-reachable-error distinguishes explicit server error from unreachable listener.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2ModelsContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "saved overlay survives publication failure",
        Scenario: "models-publish-partial",
        Args: []string{"models", "overlay", "add", "--provider", "codex", "--id", "local-example", "--clone-from", "native-example", "--json"},
        Exit: 8,
        Command: "models overlay add",
        Code: "models_refresh_partial",
        WantJSON: `{"overlay_saved":true}`,
        Forbid: []string{"consume", "credential-activate", "service"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/modelregistry -run 'Test(CLIV2Models|.*Model|.*Overlay|.*Registry)'
go vet ./cmd/cq ./internal/modelregistry
```

- [ ] Change 1: Change merge projection so native entries win over same-provider overlay collisions. Remove removeNativesShadowedByOverlays behaviour; preserve cross-provider identities. Clone exact provider/id when specified, otherwise follow the frozen selection/inference ordering and fill only missing metadata.

- [ ] Change 2: Apply strict model ID/provider validation before store/network access. Preserve atomic overlay writes and report overlay_saved/removal/prune mutation separately from publication. Prune only entries proven redundant by fresh native data; failed fetch cannot delete speculative stale overlays.

- [ ] Change 3: Expose explicit per-source/per-target publication statuses. For unreachable proxy, use documented direct fallback once; reachable error must remain an error, not trigger silent alternate authority. Report models_refresh_partial/failed according to committed outcomes.

- [ ] Change 4: Use identical reader/publisher cache paths from T05. Preserve phase-specific network/client deadlines with no new total --timeout. Test every option, native collision, exact/implicit clone, uncertain source ordering, partial publication, unavailable home and malformed native cache.

**Implementation sequence:**

```text
1. Change merge projection so native entries win over same-provider overlay collisions.
2. Apply strict model ID/provider validation before store/network access.
3. Expose explicit per-source/per-target publication statuses.
4. Use identical reader/publisher cache paths from T05.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: clarified model overlay publication` and a concise unordered body describing significant changes. Do not push or merge.

## T12 — Adapt explicit provider credential refresh

**Dependencies:** T05, T07.

**Files:** Create `cmd/cq/cli_v2_auth.go`, `cmd/cq/cli_v2_auth_test.go`. Modify `cmd/cq/refresh.go` and provider credential refresh seams used by refreshCodexAccounts, refreshManagedCodexAuthority, syncAnonymousToIdentifiedWithChange and resolveProfileEmail. Retain provider-owned persistence and broker interfaces.

**Interfaces:** Consumes T04 `cli.Handler`, `cli.Invocation`, `cli.Session`, `cli.Outcome` and T05 resolved paths. Produces `handleV2Auth(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome`; register only the canonical leaves assigned here. Resource DTO fields/types/nullability and human templates come from each linked command and its resource document. Return typed per-provider/per-account outcomes instead of inferring writes from final status. Service token-refresh process invokes the same refresh engine through its frozen process contract.

**Normative command ownership:**

| Canonical command | Positional parameters | Local options | Exact help |
| --- | --- | --- | --- |
| `auth refresh` | `providers` | None | [Help](../help/auth-refresh.txt) |

This task owns every listed command's defaults, required/repeatable flags, ranges, combinations, preconditions, effects, DTOs, exact help/examples, error precedence and acceptance cases in commands.json. Shared lexical checks are T03; this task owns file/account/authority checks and operational outcomes. Global help/json/version options apply to every row.

**Fixture arrangement:** auth-partial-write fixture commits one logical account reconciliation then fails a later token refresh for that account; changed_count must still be1. auth-boundary fixture supplies tokens with exactly30m and30m+1ns remaining. External/system Codex entries are read-only and never refreshed.

- [ ] Add the following concrete regression and the complete normative cases for this task. Use the [test harness contract](../plan.md#test-harness-contract); imports follow the shown identifiers. For non-Go baseline/release evidence tasks, execute the shown read/build checks instead of inventing a failing assertion.

```go
func TestCLIV2AuthContract(t *testing.T) {
    runV2Case(t, v2Case{
        Name: "committed credential change stays visible",
        Scenario: "auth-partial-write",
        Args: []string{"auth", "refresh", "codex", "--json"},
        Exit: 8,
        Command: "auth refresh",
        Code: "auth_refresh_partial",
        WantJSON: `{"credentials_changed":true,"changed_count":1}`,
        Forbid: []string{"service", "consume", "credential-activate"},
    })
}
```

- [ ] Run the focused gate below before changing behaviour. For implementation tasks, confirm a behavioural assertion fails on the old adapter; establish compiling declarations first. Baseline/package tasks record actual status.

```sh
go test -race -count=1 ./cmd/cq ./internal/provider/codex ./internal/provider/claude -run 'Test(CLIV2Auth|.*Refresh|.*AnonymousFresh)'
go vet ./cmd/cq ./internal/provider/codex ./internal/provider/claude
```

- [ ] Change 1: Accept only selected claude/codex providers, preserve requested order, default both and reject duplicates/Gemini before access. Refresh eligible credentials at <=30m remaining, keeping existing source-authority checks and reconciliation logic.

- [ ] Change 2: Count distinct logical accounts with committed credential changes, including anonymous reconciliation and writes before later failure. Include each candidate broker/store error rather than silently skipping failures.

- [ ] Change 3: Keep JSON free of browser launch. Interactive reauthentication uses existing flow and exact EOF/failure outcome; no overall timeout flag is added. Preserve10s HTTP and5min OAuth phase bounds and interruption130.

- [ ] Change 4: Remove implicit service registration from refresh callers. Test provider subset, boundary expiry, read-only sources, partial commits, inventory degradation, broker failures, reauthentication EOF and no token leakage.

**Implementation sequence:**

```text
1. Accept only selected claude/codex providers, preserve requested order, default both and reject duplicates/Gemini before access.
2. Count distinct logical accounts with committed credential changes, including anonymous reconciliation and writes before later failure.
3. Keep JSON free of browser launch.
4. Remove implicit service registration from refresh callers.
```

- [ ] Re-run the focused gate; require all new behavioural cases and affected existing tests to pass. Inspect the actual matched test names so an empty regex match does not masquerade as verification. Run the package vet command shown above; no service/live mutation is part of this gate.

- [ ] Review only this task's diff against its ownership and shared interfaces. Commit task files with subject `feat: exposed credential refresh outcomes` and a concise unordered body describing significant changes. Do not push or merge.
