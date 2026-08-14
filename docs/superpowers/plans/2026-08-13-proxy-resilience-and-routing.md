# CQ Proxy Resilience and Capability-Aware Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** Adversarial plan review round 3 clean; ready for implementation.

**Goal:** Build the reviewed proxy-resilience, rescue, session-pool, capability-routing, candidate-validation, and primary-promotion design without disturbing the installed listener or external credential authorities.

**Architecture:** Implement the frozen blueprint as ten ordered construction units. CU-0 establishes canonical proof and release tooling; CU-1 through CU-5 establish non-mutating inspection, unified authority, supervisor/worker isolation, rescue protocol, and rescue lifecycle; CU-6 through CU-8 add policy schemas, hard routing enforcement, brokered candidate validation, capability routing, and an accepted rollback floor; CU-9 imports that floor and promotes a separately signed target through parent-owned release authority. Every unit is inert when a later unit is absent, persists only authenticated bounded state, and produces a deterministic signed report consumed by later release gates.

**Tech Stack:** Go 1.26.1, `net/http`, Gorilla WebSocket, Darwin launchd/socket activation, Unix domain sockets plus `SCM_RIGHTS`, Ed25519, X25519/HKDF-SHA256/AES-GCM, RFC 8785 JCS, owner-only durable filesystem primitives, race-enabled Go tests, shell verification wrappers.

**Authority:** Blueprint [`docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md`](../specs/2026-08-13-proxy-resilience-and-routing-blueprint.md), SHA-256 `bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38`; clean recursive review [`docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json`](../specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json), aggregate `3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be`; source baseline `9fe30df8d4101f69084d6487740ed324a5d0b59d`.

**Plan Review:** Terminal review is recorded in sibling [`docs/superpowers/plans/2026-08-13-proxy-resilience-and-routing.review.json`](2026-08-13-proxy-resilience-and-routing.review.json), which binds this file's final SHA-256, both authority digests, round-1/round-2 dispositions, and seven clean round-3 lens records.

## Global Constraints

- Keep blueprint and review sibling byte-identical. Every CU wrapper verifies both pinned digests before tests, build, or report publication.
- Implement CU-0 through CU-9 in order. Missing later semantics return `incompatible` or `feature_inactive` before authority mutation.
- Preserve installed listener `127.0.0.1:19280`. Construction and candidate validation use explicit isolated roots and `127.0.0.1:29280`; no test restarts, unloads, replaces, or signals installed service.
- Never mutate `~/.codex/auth.json`, Codex Bar, keychain, external provider credentials, or external account selection. Tests use synthetic credentials, fixed fake TLS origins, and injected filesystem/keychain/network fakes.
- Use `fsutil.DurableFileSystem` or narrower injected interfaces for every durable operation. Files are `0o600`; directories are `0o700`; every create uses no-follow, exclusive/no-replace semantics, file sync, directory sync, and same-description reopen where blueprint requires it.
- Keep caller authentication, provider credentials, route eligibility, account health, and capability evidence as distinct authorities. Never infer authority from paths, tiers, failure statuses, or post-effect state.
- Every goroutine calling external code retains panic recovery. Every HTTP body uses an explicit bounded reader from the blueprint contract.
- All Go test commands use `-race -count=1`. Each task adds happy-path, rejection, crash-boundary, exact-cap, and `+1` tests for its owned state.
- Each task commits only listed files. Existing unrelated work remains untouched.
- CU reports are out-of-band products. A unit cannot sign its own verifier result from the bytes it is verifying.

## File Map

### Proof, schemas, and shared durable primitives

- `scripts/verify-blueprint-review`: verifies frozen blueprint sibling before every CU.
- `scripts/verify-proxy-cu`: runs one construction-unit manifest and emits bounded result material.
- `scripts/build-proxy-release`: builds role-separated signed release artifacts after clean source and CU gates.
- `internal/tools/proxycu/main.go`: private executable used by both verification scripts; exposes no product CLI command.
- `internal/tools/proxyrelease/main.go`: private hermetic release builder invoked only by `scripts/build-proxy-release`.
- `internal/proxy/canonical_jcs.go`: strict RFC 8785 encoding, framed digests, lower-hex parsing, bounded JSON readers.
- `internal/proxy/authority_crypto.go`: purpose-separated Ed25519, HMAC, HKDF, identity, and set-digest helpers.
- `internal/proxy/authority_fs.go`: secure open, sync, no-replace publish, exact-prior selector CAS, and stable identity helpers.
- `internal/proxy/authority_lock.go`: lifecycle/mutation locks, same-open-description downgrade, holder attestation, retained release.
- `internal/proxy/proxy_cu_manifest.go`: closed CU manifest/report/release schemas and parsers.

### CLI, instance, and runtime

- `cmd/cq/proxy.go`: retain command entry; delegate to closed lexical classifier before any config read.
- `cmd/cq/proxy_commands.go`: canonical proxy command tree and typed dispatch rows.
- `cmd/cq/proxy_inspect.go`: non-creating inspection/status/doctor projections.
- `cmd/cq/proxy_release.go`: candidate stage, validate-release, activate, rollback, and removal orchestration.
- `internal/proxy/proxy_snapshot.go`: best-effort desired/service/listener/process/runtime/data-plane facts.
- `internal/proxy/instance_context.go`: immutable primary/candidate roots and secure capability paths.
- `internal/proxy/operation_coordinator_store.go`: `OperationCoordinatorStore` bootstrap, intent/anchor/child/receipt, recovery, and GC.
- `internal/proxy/instance_authority_store.go`: initialisation, composite authority, external-reference, staged-release, and feature-activation stores.
- `internal/proxy/runtime_supervisor.go`: socket-activated supervisor and traffic-mode state machine.
- `internal/proxy/runtime_worker.go`: replaceable normal-worker ABI and private protocol.
- `internal/proxy/runtime_control.go`: authenticated control channel, acknowledgements, and lifecycle barriers.
- `internal/proxy/runtime_evidence.go`: runtime identity, holder checkpoints, boot acknowledgements, marker/policy stores.

### Routing, broker, and release

- `internal/proxy/rescue_relay.go`: minimal Codex HTTP/SSE/WebSocket rescue protocol.
- `internal/proxy/routing_policy_store.go`: pools, delegations, bindings, capability evidence, desired/effective policy.
- `internal/proxy/caller_authority.go`: caller indexes, pre-body authentication, ingress capabilities, permit consumption.
- `internal/proxy/session_policy.go`: session HMAC, pool intersection, continuity-safe policy resolution.
- `internal/proxy/candidate_controller.go`: candidate controller lifecycle, validation source DAG, confinement, and evidence stores.
- `internal/proxy/candidate_broker.go`: controller-owned provider vault, request broker, durable journal, scanner, and retirement handoff.
- `internal/proxy/capability_routing.go`: evidence fusion, final scope, derived pools, and route choice.
- `internal/proxy/release_bundle.go`: signed ancestry, CU report sets, role artifacts, rollback floor, and target bundle.
- `internal/proxy/release_promotion.go`: promotion/export/import/finalisation, client barrier, RCV, and rollback.
- Adjacent `_test.go` files: focused schema, state-machine, crash, capacity, differential, and source-boundary tests.

## Construction Contract

Each task follows one interface rule:

```go
type ConstructionUnitVerifier interface {
	Verify(ctx context.Context, manifestPath string) (CUReportV1, error)
}

type ConstructionUnitGate interface {
	Require(ctx context.Context, unit string, blueprintDigest string, reviewAggregate string) error
}
```

`Verify` consumes a closed manifest plus repository state and produces a secret-free report object. `Require` consumes already verified report bytes; it never invokes mutable runtime code. Each task below extends this common contract with exact owned schemas from the blueprint construction-unit table and ownership refinements.

Every implementation checkbox is executed as per-object microcycles. For each schema, transition row, digest domain, or fixture named by that checkbox: add one failing focused test, run that single test, add the minimum production case, rerun the single test, then move to the next named item. Each microcycle is 2–5 minutes; the enclosing checkbox is checked only after all explicitly named items complete. No implementation worker may batch multiple authority objects behind an unreviewed generic parser.

---

### Task 1: Build CU-0 canonical proof harness

**Files:**
- Create: `internal/proxy/canonical_jcs.go`
- Create: `internal/proxy/canonical_jcs_test.go`
- Create: `internal/proxy/proxy_cu_manifest.go`
- Create: `internal/proxy/proxy_cu_manifest_test.go`
- Create: `scripts/verify-blueprint-review`
- Create: `scripts/verify-proxy-cu`
- Create: `scripts/build-proxy-release`
- Create: `internal/tools/proxycu/main.go`
- Create: `internal/tools/proxycu/main_test.go`
- Create: `internal/tools/proxyrelease/main.go`
- Create: `internal/tools/proxyrelease/main_test.go`
- Modify: `cmd/cq/help.go`
- Test: `cmd/cq/help_test.go`

**Interfaces:** `CanonicalJSONV1(v any) ([]byte, error)`, `ParseCUManifestV1(io.Reader)`, `VerifyBlueprintReview(path, sibling string)`, and `CUReportV1` consume only bounded bytes and produce canonical secret-free objects. `internal/tools/proxycu` selects `blueprint-review|self-test|unit` and calls these APIs; `internal/tools/proxyrelease` accepts one closed release-build manifest. Shell wrappers only resolve repository root and `exec go run` for those private tools; they do not duplicate parser or build-authority logic.

- [ ] **Step 1: Write failing canonicalisation and review-sibling tests**

```go
func TestVerifyBlueprintReviewAcceptsFrozenRound44(t *testing.T) {
	err := VerifyBlueprintReview(
		"../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md",
		"../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json",
	)
	if err != nil { t.Fatal(err) }
}

func TestParseCUManifestRejectsUnknownMemberBeforeDispatch(t *testing.T) {
	_, err := ParseCUManifestV1(strings.NewReader(`{"schema_version":1,"unit":"CU-0","extra":true}`))
	if err == nil { t.Fatal("accepted unknown member") }
}
```

- [ ] **Step 2: Run CU-0 tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'CanonicalJSON|BlueprintReview|CUManifest|Help'`

Expected: compile failures for missing canonical and manifest APIs.

- [ ] **Step 3: Implement strict canonical encoders, bounded parsers, and pure help dispatch**

Implement exact blueprint digest domains, fixed lens order, record/result/aggregate reconstruction, JCS+LF validation, byte caps, task-label grammar, stale-digest rejection, and non-creating help/version/usage paths. Shell wrappers must use repository-relative inputs, a clean exact source commit where required, and deterministic environment projection.

- [ ] **Step 4: Add wrapper and corruption fixtures**

Cover every blueprint sibling positive/negative vector, absent HOME/XDG read-only operation, wrapper-output substitution, report/set/bundle cardinality, and one-byte streaming splits.

- [ ] **Step 5: Run CU-0 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'CanonicalJSON|BlueprintReview|CUManifest|Help'
./scripts/verify-proxy-cu --self-test
./scripts/verify-proxy-cu CU-0
```

Expected: PASS; no filesystem entry appears under an absent HOME/XDG fixture.

- [ ] **Step 6: Commit**

```bash
git add scripts internal/tools/proxycu/main.go internal/tools/proxycu/main_test.go internal/tools/proxyrelease/main.go internal/tools/proxyrelease/main_test.go internal/proxy/canonical_jcs.go internal/proxy/canonical_jcs_test.go internal/proxy/proxy_cu_manifest.go internal/proxy/proxy_cu_manifest_test.go cmd/cq/help.go cmd/cq/help_test.go
git commit -m "feat: added CU-0 proof harness" -m $'- verified canonical review and report artifacts\n- kept help and inspection paths non-creating'
```

### Task 2: Build CU-1 inspection and lexical command authority

**Files:**
- Create: `cmd/cq/proxy_commands.go`
- Create: `cmd/cq/proxy_commands_test.go`
- Create: `cmd/cq/proxy_inspect.go`
- Create: `cmd/cq/proxy_inspect_test.go`
- Create: `internal/proxy/proxy_snapshot.go`
- Create: `internal/proxy/proxy_snapshot_test.go`
- Modify: `cmd/cq/proxy.go:24-192,286-319,792-817`
- Modify: `cmd/cq/proxy_darwin.go`
- Modify: `cmd/cq/proxy_darwin_test.go`

**Interfaces:** `ClassifyProxyCommand(argv []string) (OrdinaryCommandAuthorityV1, error)` executes before configuration access. `InspectProxy(ctx, target) ProxySnapshot` returns typed facts with `known|unavailable|invalid`, never a synthesized success.

- [ ] **Step 1: Write failing lexical and non-mutation tests**

Table-test all thirteen ordinary rows, four refresh rows, operator recovery, help-anywhere, ignored tails, typed TTL/deadline arguments, candidate receipt lookup, and global help/version/usage. Run every read command with missing HOME/XDG and assert zero creates, keychain calls, network calls, or provider calls.

- [ ] **Step 2: Run CU-1 tests and verify RED**

Run: `go test -race -count=1 ./cmd/cq ./internal/proxy -run 'ProxyCommand|ProxySnapshot|ProxyInspect|ReadOnly'`

Expected: failures show current `runProxyStatus` creates/loads insufficient authority and lacks fact reconciliation.

- [ ] **Step 3: Implement closed command rows and fact reconciliation**

```go
type ProxySnapshot struct {
	Desired   Fact[DesiredProxyState] `json:"desired"`
	Service   Fact[ServiceState]      `json:"service"`
	Listener  Fact[ListenerState]     `json:"listener"`
	Process   Fact[ProcessState]      `json:"process"`
	Runtime   Fact[RuntimeIdentity]   `json:"runtime"`
	DataPlane Fact[DataPlaneProof]    `json:"data_plane"`
}
```

Reconcile Homebrew, CQ launchd, manual, foreign, stopped, crash-looping, and inspector-skew states. `/health` remains listener evidence only. Render exact human/JSON/doctor state, exit codes, privacy omissions, and deadlines from blueprint.

- [ ] **Step 4: Run CU-1 gate and verify GREEN**

```bash
go test -race -count=1 ./cmd/cq ./internal/proxy -run 'ProxyCommand|ProxySnapshot|ProxyInspect|ReadOnly'
./scripts/verify-proxy-cu CU-1
```

- [ ] **Step 5: Commit**

```bash
git add cmd/cq/proxy.go cmd/cq/proxy_darwin.go cmd/cq/proxy_commands.go cmd/cq/proxy_commands_test.go cmd/cq/proxy_inspect.go cmd/cq/proxy_inspect_test.go cmd/cq/proxy_darwin_test.go internal/proxy/proxy_snapshot.go internal/proxy/proxy_snapshot_test.go
git commit -m "feat: added authoritative proxy inspection" -m $'- classified commands before authority access\n- reconciled service listener process and runtime facts'
```

### Task 3: Build CU-2 secure filesystem, lock, and instance context

**Files:**
- Create: `internal/proxy/authority_crypto.go`
- Create: `internal/proxy/authority_crypto_test.go`
- Create: `internal/proxy/authority_fs.go`
- Create: `internal/proxy/authority_fs_test.go`
- Create: `internal/proxy/authority_lock.go`
- Create: `internal/proxy/authority_lock_test.go`
- Create: `internal/proxy/instance_context.go`
- Create: `internal/proxy/instance_context_test.go`

**Interfaces:** `OpenInstanceContext(root, expectedID)`, `PublishImmutable`, `ReplaceSelectorExactPrior`, and `AcquireLifecycle` accept injected durable filesystem/identity/clock/random sources. Returned handles retain open-description identity until explicit release.

- [ ] **Step 1: Write failing security-boundary tests**

Cover symlink, hard-link, owner/mode mismatch, path traversal, replaced-parent descriptor, create-before-write, partial temporary, no-replace collision, same-description downgrade, distinct-holder proof, lock-order inversion, and primary/candidate root confusion.

- [ ] **Step 2: Run focused tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'Authority(Crypto|FS|Lock)|InstanceContext'`

- [ ] **Step 3: Implement narrow primitives**

```go
type DurableObjectPublisher interface {
	PublishImmutable(ctx context.Context, dir fsutil.SecureDirectory, name string, body []byte, mode fs.FileMode) (StableObjectIdentity, error)
	ReplaceSelectorExactPrior(ctx context.Context, dir fsutil.SecureDirectory, name string, prior *StableObjectIdentity, body []byte) (StableObjectIdentity, error)
}
```

Use blueprint domains, framed lengths, exact path grammars, no-follow opens, exclusive temporaries, sync ordering, same-description reopen, and constant-time MAC comparison.

- [ ] **Step 4: Run focused tests and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run 'Authority(Crypto|FS|Lock)|InstanceContext'
```

The full CU-2 gate remains unavailable until Task 4 completes every CU-2 owner.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/authority_crypto.go internal/proxy/authority_crypto_test.go internal/proxy/authority_fs.go internal/proxy/authority_fs_test.go internal/proxy/authority_lock.go internal/proxy/authority_lock_test.go internal/proxy/instance_context.go internal/proxy/instance_context_test.go
git commit -m "feat: added secure authority primitives" -m $'- enforced durable no-replace publication and CAS\n- isolated instance roots and lock ownership'
```

### Task 4: Complete CU-2 coordinator, unified authority, and credential ownership

**Files:**
- Create: `internal/proxy/operation_coordinator_store.go`
- Create: `internal/proxy/operation_coordinator_store_test.go`
- Create: `internal/proxy/instance_authority_store.go`
- Create: `internal/proxy/instance_authority_store_test.go`
- Create: `internal/provider/codex/credential_owner_store.go`
- Create: `internal/provider/codex/credential_owner_store_test.go`
- Create: `internal/provider/codex/refresh_mutation_store.go`
- Create: `internal/provider/codex/refresh_mutation_store_test.go`
- Modify: `internal/provider/codex/credential_coordinator.go`
- Modify: `internal/provider/codex/credential_coordinator_test.go`
- Modify: `internal/provider/codex/refresh_broker.go`
- Modify: `internal/provider/codex/refresh_broker_test.go`
- Modify: `internal/auth/oauth.go`
- Modify: `internal/auth/oauth_test.go`

**Interfaces:** coordinator store owns parent mutation ordering and terminal receipts; credential owner store owns credential effects; refresh mutation store selects every action before effect. Cross-store completion binds exact digests without sharing keys or lock ownership.

- [ ] **Step 1: Write failing store/state-machine tests**

Cover bootstrap seed/verifier creation and recovery, seven child-selection absence variants, intent/anchor/receipt ordering, external-reference rows, controller initialisation, staged release, feature activation, credential-owner commit/receipt, signed source-action mapping, 3,825-unit refresh capacity, OAuth two-key query grammar, and every exact/`+1` bound named by CU-2.

- [ ] **Step 2: Run CU-2 store tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./internal/provider/codex ./internal/auth -run 'Coordinator|InstanceAuthority|CredentialOwner|RefreshMutation|OAuthAcceptedCallback'`

- [ ] **Step 3: Implement coordinator and child stores in dependency order**

Implement key bootstrap first, then coordinator object/anchor/receipt, initialisation/composite authority, external-reference ledger, controller store, quarantine, staged release, feature activation, credential owner, refresh reservation/lease/receipt, and OAuth carrier recomputation. Each child selects authenticated pre-effect authority before opening mutable provider state.

- [ ] **Step 4: Add exhaustive crash and capacity corpus**

Use phase hooks after every write, file sync, rename, directory sync, reopen, selector CAS, effect, receipt publication, and GC removal. Assert recovery chooses exactly one resume/abort/conflict branch and never reconstructs authority from a basename or post-effect observation.

- [ ] **Step 5: Run full CU-2 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./internal/provider/codex ./internal/auth -run 'Coordinator|InstanceAuthority|CredentialOwner|RefreshMutation|OAuthAcceptedCallback'
./scripts/verify-proxy-cu CU-2
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/operation_coordinator_store.go internal/proxy/operation_coordinator_store_test.go internal/proxy/instance_authority_store.go internal/proxy/instance_authority_store_test.go internal/provider/codex/credential_owner_store.go internal/provider/codex/credential_owner_store_test.go internal/provider/codex/refresh_mutation_store.go internal/provider/codex/refresh_mutation_store_test.go internal/provider/codex/credential_coordinator.go internal/provider/codex/credential_coordinator_test.go internal/provider/codex/refresh_broker.go internal/provider/codex/refresh_broker_test.go internal/auth/oauth.go internal/auth/oauth_test.go
git commit -m "feat: added unified proxy authority stores" -m $'- persisted coordinator and credential-owner authority\n- closed refresh and OAuth recovery ordering'
```

### Task 5: Build CU-3 socket supervisor and replaceable normal worker

**Files:**
- Create: `internal/proxy/runtime_supervisor.go`
- Create: `internal/proxy/runtime_supervisor_test.go`
- Create: `internal/proxy/runtime_worker.go`
- Create: `internal/proxy/runtime_worker_test.go`
- Create: `internal/proxy/runtime_control.go`
- Create: `internal/proxy/runtime_control_test.go`
- Modify: `internal/proxy/server.go:84-372`
- Modify: `internal/proxy/server_test.go`
- Modify: `internal/proxy/serving_attestor.go`
- Modify: `internal/proxy/serving_attestor_test.go`
- Modify: `cmd/cq/proxy.go:320-707`
- Modify: `cmd/cq/proxy_darwin.go`

**Interfaces:** launcher inherits launchd TCP listener and starts `RuntimeSupervisor`; supervisor owns public accept/auth/control and passes accepted normal work over owner-only Unix transport; worker owns current normal semantics but no public listener. Frozen exec ABI contains role, manifest digest, instance identity, listener/control descriptors, lifecycle holder identity, and one-shot channel material only.

- [ ] **Step 1: Write failing role and listener-ownership tests**

Cover launchd socket adoption, exact role argv, forbidden ambient helper launch, distinct supervisor/worker shared lifecycle descriptions, full holder checkpoint before admission, worker crash/replacement, prior-worker reap, inherited listener continuity, malformed private frames, cancellation/backpressure, and secret-zeroisation.

- [ ] **Step 2: Run CU-3 runtime tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Runtime(Supervisor|Worker|Control)|ServingAttestor|ListenerInheritance'`

- [ ] **Step 3: Implement frozen two-role runtime**

```go
type RuntimeRole string
const (
	RuntimeRoleSupervisor RuntimeRole = "supervisor"
	RuntimeRoleWorker     RuntimeRole = "worker"
)

type RuntimeControl interface {
	BeginDrain(context.Context, TrafficMode, uint64) error
	AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error)
	ReplaceWorker(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error)
}
```

Capture ingress mode before body read. Worker replacement never closes inherited public listener. Reject private-state access before role-local lock attestation and checkpoint selection.

- [ ] **Step 4: Add process and crash sentinels**

Prove supervisor/worker never discover or launch Python/Headroom, worker has no public descriptor, hostile caller reaches zero worker inventory, and crash/restart preserves exact listener generation while extinguishing old worker authority.

- [ ] **Step 5: Run runtime tests and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Runtime(Supervisor|Worker|Control)|ServingAttestor|ListenerInheritance'
```

The full CU-3 gate remains unavailable until Task 6 completes normal caller/control ownership.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/runtime_supervisor.go internal/proxy/runtime_supervisor_test.go internal/proxy/runtime_worker.go internal/proxy/runtime_worker_test.go internal/proxy/runtime_control.go internal/proxy/runtime_control_test.go internal/proxy/server.go internal/proxy/server_test.go internal/proxy/serving_attestor.go internal/proxy/serving_attestor_test.go cmd/cq/proxy.go cmd/cq/proxy_darwin.go
git commit -m "feat: added socket-activated proxy runtime" -m $'- separated supervisor and normal-worker authority\n- preserved listener ownership across worker replacement'
```

### Task 6: Complete CU-3 caller authentication, control, and normal corpora

**Files:**
- Create: `internal/proxy/caller_authority.go`
- Create: `internal/proxy/caller_authority_test.go`
- Create: `internal/proxy/normal_route_catalog.go`
- Create: `internal/proxy/normal_route_catalog_test.go`
- Create: `internal/proxy/normal_worker_control.go`
- Create: `internal/proxy/normal_worker_control_test.go`
- Modify: `internal/proxy/server.go:373-1745`
- Modify: `internal/proxy/codex_http_request_plan.go`
- Modify: `internal/proxy/codex_http_request_plan_test.go`
- Modify: `internal/proxy/codex_ws_broker.go`
- Modify: `internal/proxy/codex_ws_broker_test.go`
- Modify: `internal/provider/claude/refresh.go`
- Create: `internal/provider/claude/refresh_test.go`
- Modify: `internal/provider/codex/refresh.go`
- Modify: `internal/provider/codex/refresh_test.go`
- Modify: `internal/provider/gemini/refresh.go`
- Create: `internal/provider/gemini/refresh_test.go`

**Interfaces:** supervisor constructs route-independent pre-body authentication and one-use admission; worker receives complete authenticated request authority plus consumption receipt. Structured control travels only through authenticated encrypted worker frames and returns typed terminal results.

- [ ] **Step 1: Write failing caller/control/catalogue tests**

Cover exact Claude/Codex caller indexes, local-token rejection, provider-branch admission, one-use consumption, cross-provider substitution, complete Codex/Claude route catalogues, status `401|403|429` semantics, origin-policy call-site inventory, OAuth carrier handling, account commands, refresh, Gemini persistence failure, and external projection degradation.

- [ ] **Step 2: Run normal-boundary tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./internal/provider/... -run 'CallerAuthority|NormalRouteCatalog|NormalWorkerControl|Terminal401|CredentialOrigin'`

- [ ] **Step 3: Implement supervisor admission and worker control**

Persist admission consumption before private worker dispatch. Keep refresh evidence future-ingress-only for provider bearer and every Claude request; permit same-request refreshed dispatch only for independently current local-owner Codex V4 authority. Zeroise typed OAuth/control plaintext at terminal boundary.

- [ ] **Step 4: Generate differential corpora**

Generate every baseline mux route, method-independent catch-all, count-token branch, provider partition, query/header mapping, origin, downstream transformation, and terminal classification from current source fixtures. Assert each maps once and candidate publisher remains absent.

- [ ] **Step 5: Run full CU-3 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./internal/provider/... -run 'CallerAuthority|NormalRouteCatalog|NormalWorkerControl|Terminal401|CredentialOrigin'
./scripts/verify-proxy-cu CU-3
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/caller_authority.go internal/proxy/caller_authority_test.go internal/proxy/normal_route_catalog.go internal/proxy/normal_route_catalog_test.go internal/proxy/normal_worker_control.go internal/proxy/normal_worker_control_test.go internal/proxy/server.go internal/proxy/codex_http_request_plan.go internal/proxy/codex_http_request_plan_test.go internal/proxy/codex_ws_broker.go internal/proxy/codex_ws_broker_test.go internal/provider/claude/refresh.go internal/provider/claude/refresh_test.go internal/provider/codex/refresh.go internal/provider/codex/refresh_test.go internal/provider/gemini/refresh.go internal/provider/gemini/refresh_test.go
git commit -m "feat: added authenticated normal-worker ingress" -m $'- consumed caller admission before worker dispatch\n- preserved source-exact provider route semantics'
```

### Task 7: Build CU-4 minimal rescue protocol

**Files:**
- Create: `internal/proxy/rescue_relay.go`
- Create: `internal/proxy/rescue_relay_test.go`
- Create: `internal/proxy/rescue_http_test.go`
- Create: `internal/proxy/rescue_websocket_test.go`
- Modify: `internal/proxy/runtime_supervisor.go`
- Modify: `internal/proxy/runtime_supervisor_test.go`

**Interfaces:** `RescueRelay.ServeHTTP` consumes one captured rescue ingress commitment, caller-supplied ChatGPT bearer, fixed route catalogue, and bounded resource reservation. It has no account inventory, keyring, refresh, retry, routing policy, projection, or normal-worker dependency.

- [ ] **Step 1: Write failing protocol and isolation tests**

Cover opaque bearer forwarding, local proxy token rejection, exact methods/routes/queries/headers, one-shot HTTP/SSE, WebSocket V1 frames, unsupported custom origin, body/frame caps, cancellation/backpressure, global unverified pre-body bucket, owner-reserved capacity, and normal-subsystem panic sentinels.

- [ ] **Step 2: Run CU-4 tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run '^TestRescue'`

- [ ] **Step 3: Implement fixed relay**

```go
type RescueRelay struct {
	Transport httputil.Doer
	DialWS    RescueWebSocketDialer
	Budget    RescueBudget
	Origin    *url.URL
}
```

Preserve admitted request/response bytes and streaming semantics. Never retry, refresh, select account, inspect credentials, fall back into normal routing, or persist bearer/session data.

- [ ] **Step 4: Run source differential and CU-4 gate; verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run '^TestRescue'
./scripts/verify-proxy-cu CU-4
```

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/rescue_relay.go internal/proxy/rescue_relay_test.go internal/proxy/rescue_http_test.go internal/proxy/rescue_websocket_test.go internal/proxy/runtime_supervisor.go internal/proxy/runtime_supervisor_test.go
git commit -m "feat: added isolated Codex rescue relay" -m $'- relayed fixed HTTP SSE and WebSocket routes\n- excluded normal routing and credential authority'
```

### Task 8: Build CU-5 rescue lifecycle and runtime artifacts

**Files:**
- Create: `internal/proxy/runtime_evidence.go`
- Create: `internal/proxy/runtime_evidence_test.go`
- Create: `internal/proxy/runtime_bundle_switch.go`
- Create: `internal/proxy/runtime_bundle_switch_test.go`
- Modify: `internal/proxy/runtime_supervisor.go`
- Modify: `internal/proxy/runtime_supervisor_test.go`
- Modify: `internal/proxy/runtime_control.go`
- Modify: `cmd/cq/proxy_commands.go`
- Modify: `cmd/cq/proxy_commands_test.go`
- Modify: `cmd/cq/proxy_darwin.go`
- Modify: `cmd/cq/proxy_darwin_test.go`

**Interfaces:** composite traffic-mode mutation uses coordinator intent plus runtime control acknowledgement. Runtime bundle switch consumes signed manifests and produces boot/stage evidence; it never issues installed markers, policy acknowledgements, floor acceptance, or promotion authority.

- [ ] **Step 1: Write failing lifecycle/recovery tests**

Cover `normal -> rescue_draining -> rescue` and reverse transition, explicit session-policy suspension, quiescence, admitted-count drain, bounded worker kill, runtime bundle switch, launcher handoff, target/floor/rollback/restart boot chains, holder release, crash at every journal boundary, and exact primary/candidate evidence caps.

- [ ] **Step 2: Run CU-5 tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'RescueLifecycle|RuntimeBundle|RuntimeEvidence|TrafficMode'`

- [ ] **Step 3: Implement composite lifecycle and evidence stores**

Acquire lifecycle lock before reading selected runtime state. Publish desired intent, drain acknowledgement, mode barrier, boot acknowledgement, holder checkpoint, stage source/evidence graph, and terminal receipt in blueprint order. Preserve exact rollback bundle and refuse ambiguous lineage.

- [ ] **Step 4: Verify candidate-only runtime rehearsal**

Run candidate fake listener on `127.0.0.1:29280` with synthetic credentials and injected launchd/process fakes. Assert no connect, signal, plist write, or service action targets `19280`.

- [ ] **Step 5: Run CU-5 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'RescueLifecycle|RuntimeBundle|RuntimeEvidence|TrafficMode'
./scripts/verify-proxy-cu CU-5
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/runtime_evidence.go internal/proxy/runtime_evidence_test.go internal/proxy/runtime_bundle_switch.go internal/proxy/runtime_bundle_switch_test.go internal/proxy/runtime_supervisor.go internal/proxy/runtime_supervisor_test.go internal/proxy/runtime_control.go cmd/cq/proxy_commands.go cmd/cq/proxy_commands_test.go cmd/cq/proxy_darwin.go cmd/cq/proxy_darwin_test.go
git commit -m "feat: added rescue lifecycle evidence" -m $'- journalled explicit traffic-mode transitions\n- retained verified rollback runtime artifacts'
```

### Task 9: Build CU-6 routing-policy and marker infrastructure

**Files:**
- Create: `internal/proxy/routing_policy_store.go`
- Create: `internal/proxy/routing_policy_store_test.go`
- Create: `internal/proxy/session_policy.go`
- Create: `internal/proxy/session_policy_test.go`
- Create: `internal/proxy/installed_marker_store.go`
- Create: `internal/proxy/installed_marker_store_test.go`
- Modify: `internal/proxy/config.go`
- Modify: `internal/proxy/config_test.go`
- Modify: `internal/proxy/codex_turn_metadata.go`
- Modify: `internal/proxy/codex_turn_metadata_test.go`
- Modify: `cmd/cq/proxy_commands.go`
- Modify: `cmd/cq/proxy_commands_test.go`

**Interfaces:** `RoutingPolicyStore` owns desired objects, runtime acknowledgements, pools, session bindings, delegations, and capability evidence. `SessionPolicyResolver` consumes exact-byte session metadata and global account authority, then returns a narrowing decision with no raw session persistence.

- [ ] **Step 1: Write failing schema and policy-store tests**

Cover pool grammar/membership, keyed session digests, delegation uniqueness/expiry/revocation, tri-state capability evidence and scope, desired/effective generations, composite mutation, marker sets, policy acknowledgements, settings shadow, unknown/unreadable state, and inactive-feature command rejection.

- [ ] **Step 2: Run CU-6 policy tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'RoutingPolicyStore|SessionPolicy|InstalledMarker|PolicyAck|CapabilityEvidence'`

- [ ] **Step 3: Implement schemas and inert mutations**

```go
type SessionPolicyDecision struct {
	SessionDigest  string
	Pool           string
	Allowed        []codex.AccountKey
	PolicyRevision uint64
	Status         PolicyDecisionStatus
}
```

Persist only keyed session digest and opaque account keys. Intersect pool, delegation, global allowlist, and capability constraints; never broaden. Until CU-7 marker closure is active, delegation mutations return `feature_inactive` before store mutation.

- [ ] **Step 4: Add migration and privacy tests**

Reject legacy unkeyed/raw-session objects, digest-only authority, unknown members, stale revisions, provider-subject/local-owner nullability violations, and sensitive identifiers in human/JSON/log output.

- [ ] **Step 5: Run policy tests and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'RoutingPolicyStore|SessionPolicy|InstalledMarker|PolicyAck|CapabilityEvidence'
```

The full CU-6 gate remains unavailable until Task 10 completes controller/broker primitives.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/routing_policy_store.go internal/proxy/routing_policy_store_test.go internal/proxy/session_policy.go internal/proxy/session_policy_test.go internal/proxy/installed_marker_store.go internal/proxy/installed_marker_store_test.go internal/proxy/config.go internal/proxy/config_test.go internal/proxy/codex_turn_metadata.go internal/proxy/codex_turn_metadata_test.go cmd/cq/proxy_commands.go cmd/cq/proxy_commands_test.go
git commit -m "feat: added durable routing policy" -m $'- persisted privacy-preserving session constraints\n- gated inactive delegation authority'
```

### Task 10: Complete CU-6 candidate controller and broker primitives

**Files:**
- Create: `internal/proxy/candidate_controller.go`
- Create: `internal/proxy/candidate_controller_test.go`
- Create: `internal/proxy/candidate_validation_source.go`
- Create: `internal/proxy/candidate_validation_source_test.go`
- Create: `internal/proxy/candidate_broker_store.go`
- Create: `internal/proxy/candidate_broker_store_test.go`
- Create: `internal/proxy/candidate_confinement.go`
- Create: `internal/proxy/candidate_confinement_darwin.go`
- Create: `internal/proxy/candidate_confinement_other.go`
- Create: `internal/proxy/candidate_confinement_test.go`
- Modify: `internal/proxy/instance_authority_store.go`
- Modify: `internal/proxy/instance_authority_store_test.go`

**Interfaces:** CU-6 exposes permit-independent parsers, stores, publishers, reopen/recovery logic, and offline verifiers. It cannot create route semantics, consume mutable route budgets, issue request capability, open provider socket, produce live scan evidence, or accept a validation run.

- [ ] **Step 1: Write failing controller/store/confinement tests**

Cover prepared manifest, capability manifest, seventeen-slot pool, runtime resource envelope, controller key ID, source DAG/direct dependency matrix, synthetic ingress/index/materialisation, validation catalogue framing, credential vault, broker policy, journal records, generated-control store, scanner evidence store, run-retirement manifest/state/anchor/receipt, controller loss/invalidation, and child-FD/network/filesystem confinement.

- [ ] **Step 2: Run CU-6 candidate tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'Candidate(Controller|ValidationSource|BrokerStore|Confinement)|BrokerRetirement'`

- [ ] **Step 3: Implement acyclic permit-independent primitives**

```go
type CandidateBrokerPrimitives interface {
	PublishSource(context.Context, CandidateValidationSourceV1) (StableObjectIdentity, error)
	AppendJournal(context.Context, CandidateBrokerRecordV1) (JournalPositionV1, error)
	PublishScanEvidence(context.Context, CandidateCredentialEchoScanEvidenceV1) (StableObjectIdentity, error)
	VerifySealedRun(context.Context, CandidateBrokerJournalSealV1) error
}
```

Keep controller-only signing key process-private. Candidate worker receives no provider bearer, provider-origin descriptor, external credential descriptor, authority key, or direct egress. Publish source DAG before dependent objects; enforce exact per-run/global caps and reverse-DAG GC.

- [ ] **Step 4: Add malicious-candidate and recovery fixtures**

Prove candidate direct official-origin egress, off-plan IPC, inherited descriptor, arbitrary file open, executable launch, authority substitution, stale controller key, malformed temporary, and fourth-run residue all fail before provider bytes or candidate exposure.

- [ ] **Step 5: Run full CU-6 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run 'Candidate(Controller|ValidationSource|BrokerStore|Confinement)|BrokerRetirement'
./scripts/verify-proxy-cu CU-6
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/candidate_controller.go internal/proxy/candidate_controller_test.go internal/proxy/candidate_validation_source.go internal/proxy/candidate_validation_source_test.go internal/proxy/candidate_broker_store.go internal/proxy/candidate_broker_store_test.go internal/proxy/candidate_confinement.go internal/proxy/candidate_confinement_darwin.go internal/proxy/candidate_confinement_other.go internal/proxy/candidate_confinement_test.go internal/proxy/instance_authority_store.go internal/proxy/instance_authority_store_test.go
git commit -m "feat: added candidate proof infrastructure" -m $'- isolated controller sources and broker stores\n- denied candidate credential and provider authority'
```

### Task 11: Build CU-7 hard session, delegation, and continuity enforcement

**Files:**
- Modify: `internal/proxy/caller_authority.go`
- Modify: `internal/proxy/caller_authority_test.go`
- Modify: `internal/proxy/session_policy.go`
- Modify: `internal/proxy/session_policy_test.go`
- Create: `internal/proxy/caller_dispatch_permit.go`
- Create: `internal/proxy/caller_dispatch_permit_test.go`
- Create: `internal/proxy/codex_lease_v4.go`
- Create: `internal/proxy/codex_lease_v4_test.go`
- Modify: `internal/proxy/codex_lease_v2_route_snapshot.go`
- Modify: `internal/proxy/codex_lease_v2_route_snapshot_test.go`
- Modify: `internal/proxy/codex_http_request_plan.go`
- Modify: `internal/proxy/codex_http_request_plan_test.go`
- Modify: `internal/proxy/codex_turn_lease.go`
- Modify: `internal/proxy/codex_turn_lease_test.go`
- Modify: `internal/proxy/codex_lease_v2_prewarm.go`
- Modify: `internal/proxy/codex_lease_v2_prewarm_test.go`

**Interfaces:** one `CallerRequestAuthorityV1` embeds complete pre-body authentication, ingress capability, caller/delegation branch, desired/effective resolver, route-budget purpose, and permit. Journal V4 is sole dispatch linearisation authority. Continuity loads before pool filtering and then narrows; it never escapes frozen account authority.

- [ ] **Step 1: Write failing enforcement tests**

Cover HTTP/compact/retained/WebSocket/retry/prewarm permits, provider-subject versus local-owner nullability, session/pool/delegation intersection, endpoint-minimum expiry, historical consumption time, route-budget base/send branches, revocation, continuation conflicts, V4 recovery, terminal `401|403|429`, and active-non-V4 frozen transitions.

- [ ] **Step 2: Run routing-enforcement tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'CallerDispatchPermit|LeaseV4|SessionPolicyEnforcement|Prewarm.*Authority|Terminal(401|403|429)'`

- [ ] **Step 3: Implement immutable authority and one-use CAS flow**

Resolve caller and desired/effective policy, reopen continuity, calculate intersection, freeze plan, reserve route-budget successor, consume dispatch permit, append Journal V4 transaction, then release immutable dispatch. Pre-journal recovery abandons with zero dispatch; post-journal recovery never abandons, replays, or changes account.

- [ ] **Step 4: Implement native prewarm authority**

Use pinned full startup builder and metadata/window fixture. Persist distinct volatile dial lineage and durable real lineage, bridge only blueprint-listed identity fields, require complete response ID and incremental input for adoption, and consume distinct probe/adopted-send authorities on same live socket generation.

- [ ] **Step 5: Run enforcement tests and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run 'CallerDispatchPermit|LeaseV4|SessionPolicyEnforcement|Prewarm.*Authority|Terminal(401|403|429)'
```

The full CU-7 gate remains unavailable until Task 12 completes broker/scanner ownership.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/caller_authority.go internal/proxy/caller_authority_test.go internal/proxy/session_policy.go internal/proxy/session_policy_test.go internal/proxy/caller_dispatch_permit.go internal/proxy/caller_dispatch_permit_test.go internal/proxy/codex_lease_v4.go internal/proxy/codex_lease_v4_test.go internal/proxy/codex_lease_v2_route_snapshot.go internal/proxy/codex_lease_v2_route_snapshot_test.go internal/proxy/codex_http_request_plan.go internal/proxy/codex_http_request_plan_test.go internal/proxy/codex_turn_lease.go internal/proxy/codex_turn_lease_test.go internal/proxy/codex_lease_v2_prewarm.go internal/proxy/codex_lease_v2_prewarm_test.go
git commit -m "feat: enforced session-bound routing" -m $'- consumed one-use route authority before dispatch\n- preserved continuity within hard pool boundaries'
```

### Task 12: Complete CU-7 brokered provider protocol and scanner

**Files:**
- Create: `internal/proxy/candidate_broker.go`
- Create: `internal/proxy/candidate_broker_test.go`
- Create: `internal/proxy/candidate_broker_ipc.go`
- Create: `internal/proxy/candidate_broker_ipc_test.go`
- Create: `internal/proxy/credential_echo_scanner.go`
- Create: `internal/proxy/credential_echo_scanner_test.go`
- Modify: `internal/proxy/codex_http_relay.go`
- Create: `internal/proxy/codex_http_relay_test.go`
- Modify: `internal/proxy/codex_ws_broker.go`
- Modify: `internal/proxy/codex_ws_broker_test.go`
- Modify: `internal/proxy/codex_ws_relay.go`
- Modify: `internal/proxy/codex_ws_relay_test.go`
- Modify: `internal/proxy/codex_sse.go`
- Modify: `internal/proxy/codex_sse_test.go`
- Modify: `internal/proxy/candidate_broker_store.go`
- Modify: `internal/proxy/candidate_broker_store_test.go`

**Interfaces:** worker proposes; controller validates fixed case and mutable budget; controller publishes routing authority; broker signs one-use capability; durable issued record is sole capability release CAS; consumption precedes provider bytes. Broker alone holds bearer and provider socket. Scanner clears both exact encoded wire and decoded logical representations before any candidate delivery.

- [ ] **Step 1: Write failing IPC, broker, and scanner tests**

Cover Acquire→Granted/refused, HTTP/SSE/WebSocket order, handshake versus repeated application-send authority, actual connection identity/generation, cancellation/backpressure/deadline, source-exact post-upgrade JSON Text then Close errors, issued/consumed/terminal rows, ambiguous writes, no replay, route budgets, generated-control observation, header-name/value scanning, gzip/zstd encoded+decoded views, cross-chunk matches, clear-slot reservation, and terminal sentinel.

- [ ] **Step 2: Run broker tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'CandidateBroker|CredentialEcho|BrokerIPC|PostUpgradeHandshake|WebSocketApplicationSend'`

- [ ] **Step 3: Implement controller-owned broker**

```go
type CandidateProviderBroker interface {
	Acquire(context.Context, CandidateProviderCapabilityAcquireV1) (CandidateProviderCapabilityGrantedV1, error)
	Exchange(context.Context, CandidateProviderRequestCapabilityV1, CandidateProviderExchange) (CandidateProviderTerminalReceiptV1, error)
}
```

Journal private signed capability before release; persist consumption before provider connect/write; record delivered-prefix and Close materialisation; seal exact issued/consumed/terminal sets. Request capability binds origin, method, route, query, headers, body digest/limits, nonce, deadline, account/revision, and route budget.

- [ ] **Step 4: Implement exact credential-echo scanner**

Derive per-run scan subkeys with blueprint HKDF domains. Scan complete candidate-visible header names/values, encoded wire bytes, decoded identity/gzip/zstd representations, SSE cumulative content, WebSocket logical/control payloads, handshake-error Text/Close, and delivered prefixes. Any match or indeterminate result suppresses whole response before candidate exposure.

- [ ] **Step 5: Run full CU-7 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run 'CandidateBroker|CredentialEcho|BrokerIPC|PostUpgradeHandshake|WebSocketApplicationSend'
./scripts/verify-proxy-cu CU-7
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/candidate_broker.go internal/proxy/candidate_broker_test.go internal/proxy/candidate_broker_ipc.go internal/proxy/candidate_broker_ipc_test.go internal/proxy/credential_echo_scanner.go internal/proxy/credential_echo_scanner_test.go internal/proxy/codex_http_relay.go internal/proxy/codex_http_relay_test.go internal/proxy/codex_ws_broker.go internal/proxy/codex_ws_broker_test.go internal/proxy/codex_ws_relay.go internal/proxy/codex_ws_relay_test.go internal/proxy/codex_sse.go internal/proxy/codex_sse_test.go internal/proxy/candidate_broker_store.go internal/proxy/candidate_broker_store_test.go
git commit -m "feat: added brokered candidate provider traffic" -m $'- kept bearer and provider sockets outside candidate\n- blocked credential echoes before downstream delivery'
```

### Task 13: Build CU-8 capability routing and accepted rollback floor

**Files:**
- Create: `internal/proxy/capability_routing.go`
- Create: `internal/proxy/capability_routing_test.go`
- Create: `internal/proxy/release_bundle.go`
- Create: `internal/proxy/release_bundle_test.go`
- Create: `internal/proxy/release_build_authority.go`
- Create: `internal/proxy/release_build_authority_test.go`
- Modify: `scripts/build-proxy-release`
- Modify: `scripts/verify-proxy-cu`
- Modify: `internal/proxy/codex_route_policy.go`
- Modify: `internal/proxy/codex_route_policy_test.go`
- Modify: `internal/proxy/codex_route_projection.go`
- Modify: `internal/proxy/codex_route_projection_test.go`

**Interfaces:** capability resolver consumes authenticated evidence and policy generation, emits exact final scope/predicate/route choice, and can only narrow existing account authority. Release builder consumes clean CU-0…CU-8 reports and pinned source/toolchain identity, then emits signed floor-only artifacts plus `RollbackFloorAcceptanceReceiptV1`.

- [ ] **Step 1: Write failing capability and release-schema tests**

Cover evidence sort/order, scope equality, explicit/global sentinel domains, eligible/ineligible/unknown intersections, model/effective-outbound equality, derived pools, inactive marker closure, signed build authority, command/result digests, artifact/manifest stores, report sets, bundle signatures, ABI, and exact nine-slot/aggregate caps.

- [ ] **Step 2: Run CU-8 tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy -run 'CapabilityRouting|FinalScope|Release(Build|Bundle|Authority)|RollbackFloor'`

- [ ] **Step 3: Implement capability resolver**

```go
func ResolveCapabilityRoute(
	policy RoutingPolicySnapshotV1,
	evidence []CapabilityEvidenceV1,
	request CallerRequestAuthorityV1,
) (FinalRouteChoiceV1, error)
```

Require exact account/workspace/capability/product/access/auth/model scope. Finite expiry sorts before null; unknown/conflicting/stale evidence excludes account. Inactive normal Codex closure performs zero inventory, credential, network, receipt, or evidence work.

- [ ] **Step 4: Build and verify accepted floor in isolated output root**

Run builder with pinned Go 1.26.1, GOOS/GOARCH/CGO/flags, clean exact source commit, explicit launcher/supervisor/worker roles, and injected signing authority. Build only rollback-floor roles. Verify CU-0…CU-8 report set, build/vet/race reports, artifact set, bundle, and acceptance receipt; acceptance receipt contains no target ancestry or launcher payload.

- [ ] **Step 5: Run CU-8 gate and verify GREEN**

```bash
go test -race -count=1 ./internal/proxy -run 'CapabilityRouting|FinalScope|Release(Build|Bundle|Authority)|RollbackFloor'
./scripts/verify-proxy-cu CU-8
go vet ./...
go test -race -count=1 ./...
```

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/capability_routing.go internal/proxy/capability_routing_test.go internal/proxy/release_bundle.go internal/proxy/release_bundle_test.go internal/proxy/release_build_authority.go internal/proxy/release_build_authority_test.go internal/proxy/codex_route_policy.go internal/proxy/codex_route_policy_test.go internal/proxy/codex_route_projection.go internal/proxy/codex_route_projection_test.go scripts/build-proxy-release scripts/verify-proxy-cu
git commit -m "feat: added capability routing floor" -m $'- resolved scoped evidence without pool escape\n- signed an independently accepted rollback floor'
```

### Task 14: Build CU-9 primary promotion, import, and rollback

**Files:**
- Create: `internal/proxy/release_promotion.go`
- Create: `internal/proxy/release_promotion_test.go`
- Create: `internal/proxy/release_import.go`
- Create: `internal/proxy/release_import_test.go`
- Create: `internal/proxy/real_client_validation.go`
- Create: `internal/proxy/real_client_validation_test.go`
- Create: `internal/proxy/client_bearer_barrier.go`
- Create: `internal/proxy/client_bearer_barrier_test.go`
- Create: `cmd/cq/proxy_release.go`
- Create: `cmd/cq/proxy_release_test.go`
- Modify: `cmd/cq/proxy_commands.go`
- Modify: `cmd/cq/proxy_commands_test.go`
- Modify: `scripts/build-proxy-release`
- Modify: `scripts/verify-proxy-cu`

**Interfaces:** target builder consumes signed strict-descendant source and accepted CU-8 floor without rebuilding floor. Parent promotion owns candidate release receipt/export. Canonical import publishes destination inner receipt `R` before outer promotion `Q`, preserving distinct source and destination identities. Real-client validation owns one durable dispatch CAS and quarantine result.

- [ ] **Step 1: Write failing promotion/import/barrier tests**

Cover strict ancestry, exact CU-0…CU-9 report set, target-only roles, legacy live/stopped import, Headroom compatibility proof, writer/source quiescence, local-token registry/handoff, universal stateful/stateless sender enumeration, authorisation-domain/transport set, cold login/reconnect, foreign-bind zero-byte proofs, candidate stage/status/clear, promotion/export, canonical Q/R import, finalisation references, RCV preparation/grant/consumption/observation/quarantine, and candidate removal.

Candidate status selection is authority-driven, never directory recency: a selected nonterminal export anchor wins and projects `active_attempt` non-null with `compact_terminal_receipt:null`, even beside one or two retained compact receipts; only the exact row-three terminal relation can select current `compact_terminal_receipt` after terminal CAS/anchor retirement, with `active_attempt:null`. Cross-candidate, cross-run, generation mismatch, multiple selected authority, unavailable reopen, or receipt mismatch projects the blueprint's conflict/indeterminate branch.

- [ ] **Step 2: Run CU-9 tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Release(Promotion|Import)|ClientBearerBarrier|RealClientValidation|Candidate(Stage|Remove|ValidateRelease)'`

- [ ] **Step 3: Implement barrier and release admission before effects**

Enumerate every registered request sender and transport, including `cq_config_read_per_call_v1`; stop clients; verify foreign-bind zero application bytes; reopen source descriptors; copy bounded floor/receipt under one C0 deadline; publish one release-input manifest; require exact broker seal/confinement/preflight; then admit promotion. No acknowledgement waives missing hook, sender, domain, or transport.

- [ ] **Step 4: Implement stage, promotion, export, and canonical import**

```go
type CanonicalCandidateReleaseImportedObjectIdentityV1 struct {
	SourceDigest             string `json:"source_digest"`
	SourceObjectIdentity     string `json:"source_object_identity_digest"`
	CanonicalDigest          string `json:"canonical_digest"`
	CanonicalObjectIdentity  string `json:"canonical_object_identity_digest"`
}
```

Publish canonical `R` then `Q`, selector, and compact receipt under parent authority. Preserve source/canonical identity pairs, generated-control run-set, credential-owner fingerprint, broker aggregates, and deterministic captured completion time through stage, rehearsal, finalisation, history, and GC.

- [ ] **Step 5: Implement four-stage rehearsal and RCV**

Exercise target→floor→failed-target/rollback-floor→target with distinct runtime IDs and exact boot/stage proofs. RCV prepares immutable dispatch, appends sole V4 journal transaction, consumes one grant, records observation/receipt, and publishes quarantine before terminal failure. Ambiguous dispatch never replays.

- [ ] **Step 6: Run CU-9 and release gates; verify GREEN**

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Release(Promotion|Import)|ClientBearerBarrier|RealClientValidation|Candidate(Stage|Remove|ValidateRelease)'
./scripts/verify-proxy-cu CU-9
go vet ./...
go test -race -count=1 ./...
```

- [ ] **Step 7: Commit**

```bash
git add internal/proxy/release_promotion.go internal/proxy/release_promotion_test.go internal/proxy/release_import.go internal/proxy/release_import_test.go internal/proxy/real_client_validation.go internal/proxy/real_client_validation_test.go internal/proxy/client_bearer_barrier.go internal/proxy/client_bearer_barrier_test.go cmd/cq/proxy_release.go cmd/cq/proxy_release_test.go cmd/cq/proxy_commands.go cmd/cq/proxy_commands_test.go scripts/build-proxy-release scripts/verify-proxy-cu
git commit -m "feat: added verified primary promotion" -m $'- imported accepted floor with stable provenance\n- gated release on client and real-traffic proof'
```

### Task 15: Run full source-differential, crash, capacity, and release acceptance

**Files:**
- Create: `internal/proxy/proxy_resilience_acceptance_test.go`
- Create: `internal/proxy/proxy_resilience_crash_test.go`
- Create: `internal/proxy/proxy_resilience_capacity_test.go`
- Create: `internal/proxy/proxy_resilience_privacy_test.go`
- Create: `cmd/cq/proxy_resilience_cli_test.go`
- Create: `testdata/proxy-resilience/manifest.json`
- Modify: `scripts/verify-proxy-cu`
- Modify: `scripts/build-proxy-release`

**Interfaces:** acceptance harness consumes synthetic roots, fake service/process/kernel facts, fake TLS origins, signed test release authority, and checked-in fixtures. It produces only secret-free CU/release receipts and never contacts installed service or real providers.

- [ ] **Step 1: Write failing end-to-end acceptance matrix**

Map every blueprint acceptance criterion 1–20 to named tests and fixture IDs. Include normal no-policy parity, broken-config inspection, explicit rescue, hard pool/delegation routing on every transport, continuity conflict, capability-derived pools, provider rejection contraction, prewarm adoption, credential mutation recovery, candidate confinement/broker, rollback, promotion, import/finalisation, and OAuth query ownership.

- [ ] **Step 2: Run acceptance tests and verify RED**

Run: `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'ProxyResilience'`

- [ ] **Step 3: Complete fixture coverage without production shortcuts**

Add missing deterministic fixtures only. Do not add test-only authority branches, runtime fallbacks, sleeps, retries, process suspension, or network access. Each fixture declares owning CU, source boundary, expected authority transition, expected durable files, and privacy projection.

- [ ] **Step 4: Run every CU and full repository verification; verify GREEN**

```bash
./scripts/verify-blueprint-review
./scripts/verify-proxy-cu --self-test
./scripts/verify-proxy-cu CU-0
./scripts/verify-proxy-cu CU-1
./scripts/verify-proxy-cu CU-2
./scripts/verify-proxy-cu CU-3
./scripts/verify-proxy-cu CU-4
./scripts/verify-proxy-cu CU-5
./scripts/verify-proxy-cu CU-6
./scripts/verify-proxy-cu CU-7
./scripts/verify-proxy-cu CU-8
./scripts/verify-proxy-cu CU-9
go vet ./...
go test -race -count=1 ./...
```

Expected: every command passes from clean source; wrappers prove blueprint/review pins; test harness reports zero attempted connection to `19280`, zero real provider DNS/connect, and zero external credential mutation.

- [ ] **Step 5: Build target and rehearse only in isolated candidate environment**

Build signed target under isolated artifact root. Reopen accepted floor, run candidate listener on `29280`, execute synthetic HTTP/SSE/WebSocket and malicious-candidate corpus, run four-stage candidate rehearsal, and verify release receipts. Stop before installed listener migration unless separately authorised by operator.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/proxy_resilience_acceptance_test.go internal/proxy/proxy_resilience_crash_test.go internal/proxy/proxy_resilience_capacity_test.go internal/proxy/proxy_resilience_privacy_test.go cmd/cq/proxy_resilience_cli_test.go testdata/proxy-resilience/manifest.json scripts/verify-proxy-cu scripts/build-proxy-release
git commit -m "test: added proxy resilience acceptance" -m $'- covered every blueprint acceptance criterion\n- proved isolated candidate and release boundaries'
```

## Implementation Review Checkpoints

After each CU commit:

1. Reopen blueprint and review sibling and verify pinned digests.
2. Run owning CU gate plus all earlier CU gates.
3. Review changed diff through architecture, CLI/operations, routing/continuity, security/privacy, protocol fidelity, verification/release, and coverage/source-consistency lenses.
4. If any lens reports a finding, fix it in owning CU, rerun every lens against new commit, and replace no prior clean evidence silently.
5. Publish CU report only after all seven lenses return clean against same source commit.

## Plan Completion Evidence

Plan implementation is complete only when all evidence below exists and byte-links:

- frozen blueprint and round-44 sibling verify;
- CU-0…CU-9 reports verify in exact order against one signed target lineage;
- CU-8 accepted floor is independently built, signed, and retained;
- CU-9 target is a signed strict descendant and does not rebuild floor;
- `go vet ./...` and `go test -race -count=1 ./...` pass from clean source;
- candidate-only validation uses `29280` and synthetic providers;
- installed listener `19280`, system credentials, and external account state remain untouched until separately authorised release;
- full acceptance matrix maps every blueprint criterion to direct evidence;
- final seven-lens implementation review is clean against exact release commit and artifacts.
