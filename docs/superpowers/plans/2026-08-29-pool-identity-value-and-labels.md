# Pool Identity, Value, and Labels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Give routing pools stable hidden UUID identity, mutable name-only CLI labels, relative account-capacity value, and dynamically aligned TTY headers; migrate installed schema-v1 policy safely and deploy a verified Homebrew release.

**Architecture:** Persist authenticated `RoutingPolicyV2` objects keyed by `PoolID`, while exposing a separate name-based `RoutingPolicyDocument` through CLI and control APIs. Resolve names and allocate UUIDs only inside `RoutingPolicyStore`; use a dedicated rename mutation because name-only apply cannot preserve identity. Project an owned account-value map into the existing common frozen Codex route planner, inserting value after hard affinity/binding decisions but before capacity ordering. Compute one report-wide TTY label width before building headers.

**Tech Stack:** Go 1.21+, authenticated immutable authority objects, `fsutil`, injected `io.Reader`, HTTP loopback control, lipgloss TTY rendering, `go test -race`, GitHub Actions, GoReleaser, Homebrew services.

**Spec:** `docs/superpowers/specs/2026-08-29-pool-identity-and-labels.md`

**Global Constraints:** Keep UUIDs out of CLI arguments/output and quota JSON. Preserve exact bindings, caller continuity, capability restrictions, task affinity, existing equal-value ordering, and all quota arithmetic. No queues, scores, reservation thresholds, truncation, or unrelated refactors. Use red-green TDD. Run every Go test with `-race`. Never mutate production state before capturing authenticated v1 snapshot and proving copied-state migration plus isolated rollback.

---

### Task 1: Define internal v2 policy and public name document

**Files:**
- Modify: `internal/proxy/routing_policy_store.go`
- Test: `internal/proxy/routing_policy_store_test.go`
- Test: `internal/proxy/routing_policy_validation_test.go`

- [ ] **Step 1: Add failing boundary tests**

Cover valid Unicode names, exact casing, case-insensitive collision, case-only same-ID rename, control characters, malformed/duplicate UUIDs, dangling refs, duplicate members, and `uint32` value decoding. Public JSON must contain names/value and no `id`, `pool_id`, or UUID string.

```go
func TestRoutingPolicyDocumentHidesPoolIdentity(t *testing.T) {
    document := store.Document()
    body, _ := json.Marshal(document)
    if bytes.Contains(body, []byte(`"id"`)) || bytes.Contains(body, []byte(poolID)) {
        t.Fatalf("public policy leaked pool identity: %s", body)
    }
}
```

- [ ] **Step 2: Run focused tests and confirm red**

Run: `go test -race -count=1 ./internal/proxy -run 'RoutingPolicy(Document|Validation|Name|Value)'`

Expected: compile/test failure because v2 types, document projection, and validation do not exist.

- [ ] **Step 3: Add minimum domain model**

Add `PoolID string`, `PoolValue uint32`, internal `AccountPoolV2`, `SessionBindingV2`, and `RoutingPolicyV2`. Keep legacy v1 decoding private to migration. Use lowercase RFC 4122 UUID syntax and a small injected-random v4 generator.

```go
type AccountPoolV2 struct {
    ID      PoolID                     `json:"id"`
    Name    string                     `json:"name"`
    Value   PoolValue                  `json:"value,omitempty"`
    Members []providerCodex.AccountKey `json:"members"`
}

type PolicyPoolDocument struct {
    Name    string                     `json:"name"`
    Value   PoolValue                  `json:"value,omitempty"`
    Members []providerCodex.AccountKey `json:"members"`
}
```

Validate names by UTF-8, trimmed non-empty content, no control runes, and case-folded uniqueness. Preserve supplied spelling. Build ID/name indexes once per validation/conversion.

- [ ] **Step 4: Add document projection and compilation**

`Document()` joins internal IDs to names. `PublishDocument()` resolves same-name pools case-insensitively and retains their IDs/value, allocates IDs for new names, resolves session/capability names, and treats removed-plus-added names as new identities. Return name-only errors.

- [ ] **Step 5: Run focused tests and confirm green**

Run: `go test -race -count=1 ./internal/proxy -run 'RoutingPolicy(Document|Validation|Name|Value)'`

- [ ] **Step 6: Commit**

```text
feat: added hidden pool identity

- added schema-v2 pool identity and value types
- added name-only policy projection and validation
```

### Task 2: Migrate authenticated schema v1 atomically

**Files:**
- Modify: `internal/proxy/routing_policy_store.go`
- Modify: `internal/proxy/resilience_state.go`
- Test: `internal/proxy/routing_policy_store_test.go`
- Test: `internal/proxy/resilience_state_test.go`

- [ ] **Step 1: Add failing migration fixtures**

Publish a valid v1 object and anchor, reopen with deterministic random bytes, then prove:

- `cyber` becomes `Cyber` with value zero;
- membership, evidence, predicates, delegations, and generations stay unchanged;
- binding/capability names become one matching `PoolID`;
- reopen keeps same ID;
- invalid v1 MAC/refs fail before migration;
- injected failures before object write, after object write, and around anchor CAS leave one complete authoritative policy.

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./internal/proxy -run 'RoutingPolicy.*(Migrate|Migration|Reopen)|Resilience.*RoutingPolicy'`

- [ ] **Step 3: Pass resilience random into policy store**

Change `OpenRoutingPolicyStore` to receive `io.Reader`. Reuse `ProxyResilienceStateOptions.Random`; do not read global randomness inside store.

- [ ] **Step 4: Support v1 and v2 anchor/object authentication**

Read anchor schema first. For v1, authenticate anchor and object with v1 domain separators, validate v1, convert, validate/seal v2, publish immutable v2 object, then exact-prior CAS v2 anchor while lock remains held. Preserve generations. For v2, authenticate and load directly. Keep old immutable v1 object.

- [ ] **Step 5: Confirm migration green**

Run: `go test -race -count=1 ./internal/proxy -run 'RoutingPolicy.*(Migrate|Migration|Reopen)|Resilience.*RoutingPolicy'`

- [ ] **Step 6: Commit**

```text
feat: migrated routing pools to UUIDs

- migrated authenticated schema-v1 policies atomically
- preserved routing meaning and stable pool identity
```

### Task 3: Expose name-only control projection and mutations

**Files:**
- Modify: `internal/proxy/policy_control.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/normal_route_catalog.go`
- Test: `internal/proxy/policy_control_test.go`
- Test: `internal/proxy/normal_route_catalog_test.go`

- [ ] **Step 1: Add failing control tests**

GET/PUT must use `RoutingPolicyDocument`; response contains values but no UUID. Add authenticated mutation endpoint accepting only:

```go
type PoolMutationRequest struct {
    Operation string    `json:"operation"`
    Name      string    `json:"name"`
    NewName   string    `json:"new_name,omitempty"`
    Value     PoolValue `json:"value,omitempty"`
}
```

Prove rename retains ID, membership, bindings, capability reference, and value; value changes only value; failures return generic name-only errors.

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./internal/proxy -run 'PolicyControl|NormalRouteCatalog.*Policy'`

- [ ] **Step 3: Implement store-owned mutations**

Add `RenamePool(oldName, newName)` and `SetPoolValue(name, value)`. Clone current policy, resolve names case-insensitively, advance authority/routing/effective generations once, publish via existing CAS, then replace session resolver snapshot.

- [ ] **Step 4: Implement control endpoint and allowlist route**

Add `RuntimePolicyPoolPath`. Bound body to 1 MiB, disallow unknown JSON, reject trailing values, and return current public document.

- [ ] **Step 5: Confirm green and commit**

Run: `go test -race -count=1 ./internal/proxy -run 'PolicyControl|NormalRouteCatalog.*Policy'`

```text
feat: added name-only pool mutations

- added authenticated rename and value operations
- kept UUIDs behind control projection
```

### Task 4: Update pool and session CLI

**Files:**
- Modify: `cmd/cq/proxy_policy.go`
- Modify: `cmd/cq/proxy_commands.go`
- Modify: `cmd/cq/help.go`
- Test: `cmd/cq/proxy_policy_test.go`
- Test: `cmd/cq/proxy_commands_test.go`

- [ ] **Step 1: Add failing CLI lifecycle tests**

Cover:

- `pool set Cyber --account A --value 10`;
- later `pool set cyber --account B` preserving spelling, identity, and value;
- new pool without value defaults zero;
- `pool value Cyber 12`;
- `pool rename Cyber "Security Research"` and case-only rename;
- quoted names in session bind/show/list;
- malformed/negative/fractional/overflow values;
- missing/colliding names and unknown flags;
- status/apply/session output never exposing UUIDs.

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./cmd/cq -run 'ProxyPolicy|ProxyCommands.*Policy'`

- [ ] **Step 3: Switch CLI transport to public document**

Change `proxyPolicyControl` and apply/status readers to `RoutingPolicyDocument`. Preserve current generations. Resolve name matches with shared boundary helper; avoid duplicating validation rules in command package.

- [ ] **Step 4: Add set/value/rename grammar**

Parse values with `strconv.ParseUint(raw, 10, 32)`. Make `--value` optional with explicit presence bit. Send rename/value through pool mutation path. Update manual help and lexical grammar.

- [ ] **Step 5: Confirm green and commit**

Run: `go test -race -count=1 ./cmd/cq -run 'ProxyPolicy|ProxyCommands.*Policy'`

```text
feat: added pool rename and value CLI

- added name-based pool rename and value commands
- preserved values when membership changed without a value
```

### Task 5: Move runtime policy references and permits to PoolID

**Files:**
- Modify: `internal/proxy/session_policy.go`
- Modify: `internal/proxy/caller_dispatch_permit.go`
- Modify: `internal/proxy/codex_http_request_plan.go`
- Test: `internal/proxy/session_policy_enforcement_test.go`
- Test: `internal/proxy/caller_dispatch_permit_test.go`
- Test: `internal/proxy/codex_http_request_plan_test.go`
- Test: `internal/proxy/codex_responses_http_test.go`

- [ ] **Step 1: Add failing identity-continuity tests**

Resolve same raw session before/after rename and prove selected ID/allowed accounts unchanged. Prove capability snapshot and permit carry ID, not name. Reject schema-v1 five-second permit. Prove HTTP and WebSocket pool restrictions survive rename.

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./internal/proxy -run 'SessionPolicy|CallerDispatchPermit|Rename.*(HTTP|WebSocket)'`

- [ ] **Step 3: Convert resolver and capability snapshot**

Index `RoutingPolicyV2` by `PoolID`. Add internal `PoolID` to decisions/snapshots while resolving names only for public inspection. Return owned slices/maps. Preserve hard restriction order.

- [ ] **Step 4: Introduce permit schema v2**

Carry `PoolID`, update MAC/digest domain separators to v2, require schema 2, and keep one-use/5-second/routing-generation fences. Do not migrate old permit files.

- [ ] **Step 5: Confirm green and commit**

Run: `go test -race -count=1 ./internal/proxy -run 'SessionPolicy|CallerDispatchPermit|Rename.*(HTTP|WebSocket)'`

```text
feat: bound runtime policy to pool IDs

- moved session and capability references to immutable IDs
- upgraded caller dispatch permits to schema v2
```

### Task 6: Add pool value to common frozen routing order

**Files:**
- Modify: `internal/proxy/session_policy.go`
- Modify: `internal/proxy/codex_route_policy.go`
- Modify: `internal/proxy/codex_frozen_dispatch_plan.go`
- Modify: `internal/proxy/codex_http_request_plan.go`
- Test: `internal/proxy/codex_route_policy_test.go`
- Test: `internal/proxy/codex_frozen_dispatch_plan_test.go`
- Test: `internal/proxy/codex_http_request_plan_test.go`
- Test: `internal/proxy/codex_responses_http_test.go`

- [ ] **Step 1: Add failing route-order matrix**

Prove lower value wins despite less remaining quota; known-zero spills; unknown lower-value stays first; equal values retain exact current ordering; overlap uses max; unpooled is zero. Add hard override cases for bound account, authenticated continuity, session/capability pool, and affinity. Prove frozen plan does not reorder after policy mutation.

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./internal/proxy -run 'Codex(RoutePolicy|FrozenDispatch|HTTPRequest).*(Value|Affinity|Bound|Pool)'`

- [ ] **Step 3: Project effective values**

Have session resolver snapshot build owned `map[AccountKey]PoolValue`, taking max across pools and zero for absent accounts. Pass map into every HTTP and WebSocket/prewarm `CodexFrozenDispatchInput` build and capability-narrowed rebuild.

- [ ] **Step 4: Freeze value on candidates**

Add `Value PoolValue` to projected route candidates. Preserve it in frozen accounts and dispatch probe facts. Comparator order:

```go
if left.Affinity != right.Affinity { return left.Affinity }
if left.Value != right.Value { return left.Value < right.Value }
// existing capacity, remaining, native, load, account, choice ordering unchanged
```

Bound selection remains early exact-return path. Affinity remains stronger. Eligibility filtering happens before value comparison.

- [ ] **Step 5: Confirm green and commit**

Run: `go test -race -count=1 ./internal/proxy -run 'Codex(RoutePolicy|FrozenDispatch|HTTPRequest).*(Value|Affinity|Bound|Pool)'`

```text
feat: preserved valuable pool capacity

- routed ordinary work through lower-value accounts first
- retained hard bindings, affinity, and frozen-plan order
```

### Task 7: Project renamed pools and align TTY labels

**Files:**
- Modify: `cmd/cq/codex_proxy_eligibility.go`
- Modify: `internal/app/report.go`
- Modify: `internal/output/tty_build.go`
- Test: `cmd/cq/codex_proxy_eligibility_test.go`
- Test: `internal/output/tty_build_test.go`
- Test: `internal/output/tty_renderer_test.go`

- [ ] **Step 1: Add failing report/TTY tests**

Use public policy document with `Cyber`. Prove bound subsets keyed by resolved pool identity, name-only JSON, no value in quota JSON, and renamed display on next report. Reproduce misalignment and assert common summary start column for account/provider aggregate/pool aggregate with a name longer than seven cells. Assert no truncation and wider separator. Keep unnamed proper subset as `Proxy`.

```go
if got, want := stripANSI(section.ProxyPools[0].Header), "    Cyber 2 × pro 20x = 40x"; got != want {
    t.Fatalf("pool header = %q, want %q", got, want)
}
```

- [ ] **Step 2: Confirm red**

Run: `go test -race -count=1 ./cmd/cq ./internal/output -run 'ProxyEligibility|ProxyPool|TTY.*(Align|Label|Aggregate)'`

- [ ] **Step 3: Project names without values**

Change live policy loader to name-only document. Determine bound named subsets by document names. Keep quota report schema unchanged except already-existing pool name.

- [ ] **Step 4: Compute shared visible label width first**

Scan provider account labels, aggregate provider names, generic `Proxy`, and pool names before header creation. Minimum width seven; no maximum. Pass width to all header builders and right-align by visible rune width. Named pool header label is exactly pool name.

- [ ] **Step 5: Confirm green and commit**

Run: `go test -race -count=1 ./cmd/cq ./internal/output -run 'ProxyEligibility|ProxyPool|TTY.*(Align|Label|Aggregate)'`

```text
fix: aligned name-only pool headers

- rendered pool names without proxy prefix
- aligned all labels against longest visible name
```

### Task 8: Complete regression and security verification

**Files:**
- Modify only failing scope-owned files
- Verify: all Go packages

- [ ] **Step 1: Format changed Go files**

Run: `gofmt -w <changed-go-files>`

- [ ] **Step 2: Run package gates**

Run:

```bash
go test -race -count=1 ./internal/proxy
go test -race -count=1 ./internal/output ./internal/app
go test -race -count=1 ./cmd/cq
```

Expected: all pass.

- [ ] **Step 3: Run repository gates**

Run:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Expected: all pass; no UUID in golden CLI/quota output.

- [ ] **Step 4: Inspect diff and source graph impact**

Run: `git diff --check && git status --short && git diff --stat origin/main...HEAD`

Use codebase graph `detect_changes` if available; otherwise inspect exact changed callers with `trace_path` and current source. Confirm no placeholder, TODO, v1 runtime reference, pool-name permit field, or `"Proxy "+pool.Name` remains.

- [ ] **Step 5: Commit any verification-only fixes**

```text
test: covered pool identity migration

- added policy migration and downgrade fixtures
- covered value ordering and name-only rendering
```

### Task 9: Prove copied-state migration and rollback

**Files:**
- Create only temporary directories outside repository
- Preserve: installed production resilience state

- [ ] **Step 1: Resolve exact installed binary, state root, anchor, selected v1 object, and version**

Use read-only `cq --version`, `cq proxy status --json`, launchd/Homebrew service inspection, and authenticated state inspection. Record hashes; do not print secrets or raw policy.

- [ ] **Step 2: Snapshot v1 authority recoverably**

Create `mktemp -d`, copy only resolved routing-policy anchor/object plus required authority material with metadata preserved, and hash copied files. Keep production listener unchanged.

- [ ] **Step 3: Run new binary against copied state**

Build unique candidate path. Open copied root on isolated port, verify automatic migration, `Cyber`, value zero/current configured value, stable ID across restart through internal test/inspection only, and unchanged membership/binding behaviour.

- [ ] **Step 4: Prove isolated downgrade recovery**

Restore pre-migration snapshot into a second temporary root. Start prior installed binary on another isolated port and verify it opens v1 policy and routes bound session. Never point old binary at migrated v2 root.

- [ ] **Step 5: Run release validation**

Run: `scripts/validate-codex-release`

Expected: normal WebSocket, repeated HTTP, compaction, rescue, and handoff gates pass; commit-bound `cq/live-normal-routing` status succeeds; production listener unchanged.

### Task 10: Review, merge, release, and deploy

**Files:**
- Remote: `jacobcxdev/cq`
- Remote: `jacobcxdev/homebrew-tap`
- Installed: Homebrew `cq` service

- [ ] **Step 1: Push branch and open assigned PR**

Push `jacobcxdev/feat/pool-identity`. Create PR against `main`, assign `jacobcxdev`, summarise schema migration/value semantics/TTY result, and include exact tests plus copied-state rollback evidence.

- [ ] **Step 2: Wait for CI and review state**

Use `gh pr checks --watch`. Resolve failures within scope. Re-run affected race tests. Verify head SHA before merge.

- [ ] **Step 3: Squash-merge and verify main**

Merge only after required checks pass. Fetch main, verify merge SHA and clean tree. Run focused migration/routing/TTY race tests at merged SHA.

- [ ] **Step 4: Release next minor version**

Current installed line is `v0.24.x`; feature release target is `v0.25.0`, unless remote tags show a newer version. Tag exact green merge SHA, push tag, watch Release workflow, verify published release/assets and generated Homebrew PR.

- [ ] **Step 5: Merge Homebrew formula PR and install**

Wait for tap checks, merge formula PR, then run:

```bash
brew update
brew upgrade jacobcxdev/tap/cq
brew services restart cq
```

Verify installed `cq --version`, executable path, Homebrew service PID, listener ownership, `/health`, `cq proxy status --json`, and selected migrated policy document.

- [ ] **Step 6: Verify installed CLI output and routing**

Using name-only commands, preserve/configure current `Cyber` pool and higher value. Run `cq check codex`; assert provider and `Cyber` aggregate labels align and no named `Proxy` prefix appears. Confirm policy/session/quota JSON contain no UUID and quota JSON contains no values.

Send real version-matched Codex HTTP and WebSocket traffic through `127.0.0.1:19280`. Prove ordinary unbound work selects viable lower-value capacity; Cyber-bound work stays within Cyber before/after a reversible rename; normal, rescue, and handoff requests complete. Correlate listener PID/socket and routing evidence. Restore intended display name/value after proof.

- [ ] **Step 7: Final cleanliness and evidence**

Confirm repository clean, service healthy, no temporary proxy/listener remains, production config matches intended `Cyber` name/value/members, and rollback snapshot location/recovery procedure is recorded without secrets.

---

## Plan self-review

- Every approved requirement maps to one task: UUID/name projection (1–5), atomic v1 migration/downgrade (2, 9), value semantics (4, 6), name-only aligned TTY (7), deployment/live proof (9–10).
- Public/internal type boundary stays explicit: `RoutingPolicyV2` never becomes CLI JSON; `RoutingPolicyDocument` never becomes runtime authority.
- Rename uses dedicated mutation; apply remove-plus-add semantics remain unambiguous.
- Value comparator preserves hard binding and affinity before value, then existing ordering within equal tiers.
- HTTP and WebSocket share frozen planner; value map reaches initial, narrowed, and prewarm builds.
- No placeholder implementation, speculative scheduler, extra policy dimension, pool-name length cap, or UUID CLI selector included.
