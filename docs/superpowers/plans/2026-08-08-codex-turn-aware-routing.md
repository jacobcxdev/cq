# Codex Turn-Aware Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make CQ sole authority for automatic Codex account routing while preserving system auth for explicit user actions and pinning each admitted Codex agent turn to one account.

**Architecture:** Build permanent credential containment first, then one candidate inventory and single credential coordinator. Add bucket-aware capacity, explicit-account request execution, strict Responses protocol parsing, and a durable lane/lease core. Integrate leases in observe mode before separately enforcing HTTP and WebSocket routing behind versioned readiness markers. Keep one transport-independent `(session_id, thread_id, turn_id, "codex-responses")` namespace and bind WebSocket continuation to exact upstream socket generation.

**Tech Stack:** Go 1.26, standard library HTTP/JSON/crypto/filesystem packages, Gorilla WebSocket, `compress/zstd`, Unix-domain sockets on supported platforms, existing `fsutil` dependency injection, Go race detector.

---

## Execution contract

- Work stage-by-stage. Each stage ends green, reviewable, and committed before next begins.
- For every behaviour change: add smallest failing test, run named test and confirm expected failure, add minimum production code, rerun named test, then run stage package suite with `-race`.
- Never print or fixture real tokens, emails, account IDs, paths, turn IDs, thread IDs, response IDs, or prompt bodies. Test values stay synthetic.
- Never reintroduce automatic `~/.codex/auth.json` or registry active-state writes. `off` and rollback modes change routing only.
- Preserve unknown JSON fields and file permissions. Managed credential and journal commits use unique temp file, file sync, atomic rename, and parent-directory sync.
- Treat official Codex `rust-v0.146.0` fixtures as protocol authority. Copy only minimal synthetic protocol shapes into testdata and record source commit in fixture comments.
- Do not enable HTTP or WebSocket enforcement until matching readiness marker exists. Upgrade never creates marker or activates enforcement.
- Full Stage 15 promotion requires seven elapsed canary days and at least 100 admitted installed-service turns. Do not claim completion before both facts exist.

## Stage 0: Baseline and isolation

**Files:**

- Create implementation branch/worktree from current blueprint commit.
- Inspect: `go.mod`, `go.sum`, current `git status`.

- [x] Create branch `jacobcxdev/feat/codex-turn-routing` in isolated worktree.
- [x] Run `go mod download`.
- [x] Run `go test -race -count=1 ./...` and preserve baseline output outside Git.
- [x] Run `go build ./...` and `go vet ./...`.
- [x] Stop if baseline failure overlaps touched packages; diagnose before Stage 1.

## Stage 1: Permanent containment floor

**Files:**

- Modify: `internal/provider/codex/accounts.go`
- Modify: `internal/provider/codex/provider.go`
- Modify: `internal/provider/codex/accounts_test.go`
- Modify: `internal/provider/codex/provider_test.go`
- Modify: `internal/proxy/codex_selector.go`
- Modify: `internal/proxy/codex_selector_test.go`
- Modify: `internal/proxy/codex_transport.go`
- Modify: `internal/proxy/codex_transport_test.go`
- Modify: `internal/proxy/codex_live.go`
- Modify: `internal/proxy/codex_live_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`
- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/proxy_test.go`
- Modify: `cmd/cq/refresh.go`
- Modify: `cmd/cq/refresh_test.go`
- Modify: `cmd/cq/registry_pipeline.go`
- Modify: `cmd/cq/registry_pipeline_test.go`
- Modify: `cmd/cq/local_registry.go`
- Modify: `cmd/cq/local_registry_test.go`
- Create: `internal/compat/epoch.go`
- Create: `internal/compat/epoch_test.go`
- Modify: `internal/provider/AGENTS.md`
- Modify: `internal/provider/codex/AGENTS.md`
- Modify: `AGENTS.md`

- [x] Add red discovery tests: fresh matching system candidate beats stale managed candidate; managed candidate remains available; malformed claims never form `"::"`; directory order cannot change chosen live copy.
- [x] Add red automatic-path tests. Inject counting HTTP client and write-recording filesystem. Assert quota fetch, `cq refresh`, registry construction/refresh, HTTP 401/429, WS upgrade 401/429, and Live failover make zero OAuth token calls and leave system auth, registry, and managed files byte-identical.
- [x] Add red diagnostics tests proving no identifier or hint falls back to access token.
- [x] Add red `/app-server` tests: HTTP and WebSocket `initialize` attempt fail explicitly and never dial upstream. Update compact error message to direct callers to local Codex app-server with CQ Responses base URL.
- [x] Add red compatibility-epoch tests: missing epoch creates current floor atomically; equal/newer starts; older executable epoch refuses; corrupt/permission failure fails closed.
- [x] Change `DiscoverAccounts` duplicate handling so matching live candidate remains selected. Do not overwrite its token/path with managed copy.
- [x] Make `DecodeCodexClaims(...).RecordKey()` result usable only when real identity components exist; keep malformed candidates separate and non-routable.
- [x] Change `codexAcctIdentifier` and `codexAccountHint` to stable non-secret identity only. Return empty/typed unroutable state when none exists.
- [x] Remove `CodexAccountSwitcher`, `Switcher`, `persistSwitch`, failover suppression, and all proxy wiring to `Accounts.Switch`.
- [x] Remove Codex refresh/exchange/persistence from provider `fetchAccount`, `runRefresh`, registry token lookup, local registry, and proxy startup. A 401/403 becomes `auth_expired` or tries an already-present same-identity candidate only after Stage 3; Stage 1 does no refresh.
- [x] Make `PersistCodexAccount` managed-file-only during compatibility. Remove `IsActive` mirroring into system auth.
- [x] Retire `proxyCodexAppServer`, its fake JSON-RPC parser/rewriter, and `/app-server` upstream dial path. Handler returns stable explanatory error without upgrade.
- [x] Add compatibility epoch startup check before proxy/provider automatic work. Persist only non-secret integer schema/floor under `0o700` state directory and `0o600` file.
- [x] Update architecture docs: Codex is multi-account, read-only automatically, no refresh until coordinator broker.
- [x] Run:

```bash
go test -race -count=1 ./internal/provider/codex ./internal/proxy ./cmd/cq ./internal/compat
go test -race -count=1 ./...
git diff --check
```

- [x] Secret-scan changed files with `rg -n '(access_token|refresh_token|Bearer )'` and inspect every match for schema-only/synthetic-safe use.
- [x] Commit `fix: contained Codex credential writes`.

## Stage 2: Explicit system authority

**Files:**

- Create: `internal/provider/codex/system_activator.go`
- Create: `internal/provider/codex/system_activator_test.go`
- Create: `internal/provider/codex/registry.go`
- Create: `internal/provider/codex/registry_test.go`
- Modify: `internal/provider/codex/accounts.go`
- Modify: `internal/provider/codex/accounts_test.go`
- Modify: `internal/app/accounts.go`
- Modify: `internal/app/accounts_test.go`

Define typed boundary:

```go
type ActivationResult struct {
    SystemCommitted bool
    ProjectionError error
}

type DeactivationResult struct {
    SystemRemoved   bool
    ProjectionError error
}

type SystemActivator interface {
    Active(context.Context) (SystemSnapshot, error)
    Activate(context.Context, CandidateRef, Revision) (ActivationResult, error)
    Deactivate(context.Context, AccountKey, Revision) (DeactivationResult, error)
}
```

- [x] Add red tests for non-activating login preserving system auth and `active_account_key`, repeated login preserving unknown managed/registry fields, exact-candidate activation, ambiguous email refusal, active removal, inactive removal, and registry projection failure after successful system commit.
- [x] Split registry operation into `UpsertAccount` and `ProjectActive`; account upsert never changes active projection.
- [x] Implement concrete filesystem `SystemActivator`. Preserve unknown system-auth fields by merging only auth/token fields; strip `_cq` before system write.
- [x] Route explicit Codex switch/remove/login `--activate` through injected activator. Pass exact candidate returned by login, never rediscover by email.
- [x] Keep compatibility `Accounts` methods as thin explicit-only adapters. No automatic package imports `SystemActivator`.
- [x] Run `go test -race -count=1 ./internal/provider/codex ./internal/app ./cmd/cq`.
- [x] Commit `refactor: isolated Codex activation`.

## Stage 3: Read-only logical inventory

**Files:**

- Create: `internal/provider/codex/inventory.go`
- Create: `internal/provider/codex/inventory_test.go`
- Create: `internal/provider/codex/testdata/inventory/*.json`
- Modify: `internal/provider/codex/accounts.go`
- Modify: `internal/provider/codex/provider.go`
- Modify: `internal/provider/codex/accounts_test.go`
- Modify: `internal/provider/codex/provider_test.go`
- Modify: `internal/proxy/codex_selector.go`
- Modify: `internal/proxy/codex_selector_test.go`

Add domain types:

```go
type AccountKey string
type CandidateID string
type LineageID string
type Revision string

type CredentialSource uint8
const (
    SourceSystem CredentialSource = iota + 1
    SourceManaged
)

type LogicalAccount struct {
    Key        AccountKey
    Identity   AccountIdentity
    Candidates []CredentialCandidate
    Active     bool
    Routable   bool
}
```

- [x] Add red table tests for live/managed inverse freshness, two retained candidates, partial-to-rich claims, conflicting strong claims, weak-only ambiguity, malformed claims, stable candidate ordering independent from `ReadDir`, and one-row compatibility output.
- [x] Compute `CandidateID` from source namespace plus stable record identity; compute secret `Revision` from exact credential material but never expose/log it.
- [x] Reconcile only ordered strong evidence. Emit typed association/adoption intents without writing. Do not persist `AccountKey` yet; use generation-local opaque keys and mark them unstable.
- [x] Add candidate resolver ordering: accepted revision, unexpired, unknown expiry, expired probe; CQ-authored/later expiry/stable ID tie-break.
- [x] Make `DiscoverAccounts` compatibility wrapper flatten one preferred candidate per logical account while preserving `Active` meaning. Make provider fetch one logical row and try candidate revisions within identity before `auth_expired`.
- [x] Change selector exclusions to typed `AccountKey`/`CandidateID` sets. Delete email/account/token string exclusion logic.
- [x] Run `go test -race -count=1 ./internal/provider/codex ./internal/proxy -run 'Inventory|Candidate|Discover|Selector'`.
- [x] Commit `refactor: added Codex account inventory`.

## Stage 4: Single credential coordinator

**Files:**

- Create: `internal/provider/codex/managed_store.go`
- Create: `internal/provider/codex/managed_store_test.go`
- Create: `internal/provider/codex/credential_coordinator.go`
- Create: `internal/provider/codex/credential_coordinator_test.go`
- Create: `internal/provider/codex/credential_control.go`
- Create: `internal/provider/codex/credential_control_unix.go`
- Create: `internal/provider/codex/credential_control_other.go`
- Create: `internal/provider/codex/credential_control_test.go`
- Create: `internal/provider/codex/removal_journal.go`
- Create: `internal/provider/codex/removal_journal_test.go`
- Modify: `internal/provider/codex/inventory.go`
- Modify: `internal/provider/codex/system_activator.go`
- Modify: `internal/app/accounts.go`
- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/main.go`
- Modify: `internal/compat/epoch.go`

Persist closed metadata inside each managed record:

```go
type Provenance string
const (
    ProvenanceCQOAuth        Provenance = "cq_oauth"
    ProvenanceSystemBorrowed Provenance = "system_borrowed"
    ProvenanceLegacyUnknown  Provenance = "legacy_unknown"
)

type RefreshOwnership string
type OperationState string
```

- [x] Add red managed-store crash matrix: write, file sync, rename, directory sync, restart recovery, stale revision, bad permissions, unknown enum, corrupt `_cq`, and unknown-field round trip.
- [x] Extend `fsutil.FileSystem` only with minimum operations required for unique temp files and durable sync. Add OS and memory/test implementations.
- [x] Implement persisted random opaque `AccountKey`, `CandidateID`, lineage, generation, revision, provenance, ownership, operation state, and credential material as one JSON commit. Existing files load as refresh-suspended `legacy_unknown` without rewrite.
- [x] Add red ownership tests: first coordinator binds fixed socket; second delegates; simultaneous startup has one owner; stale/unreachable endpoint fails closed; unsupported platforms disable mutation/refresh; RPC uses current-user-only socket permissions.
- [x] Implement typed local RPC split: read/automatic broker versus administrative mutation methods. No raw credentials cross diagnostic/log surfaces.
- [x] Add red adoption/race tests: unseen system identity yields intent; coordinator adopts once; external system rotation rolls forward `system_borrowed`; CQ-owned candidate is never overwritten; activate/adopt/remove races have one revision-fenced result.
- [x] Implement durable removal journal before deleting exact candidate set/system projection. Recovery resumes exact IDs/revisions only. Return `RemovalResult` with managed/system/projection/recovery fields.
- [x] Route Codex login, switch, remove, and adoption through coordinator RPC. If no owner exists, command binds endpoint for one ephemeral transaction; existing unreachable endpoint never triggers direct-write fallback.
- [x] Advance compatibility epoch atomically before first new managed-record commit.
- [x] Run:

```bash
go test -race -count=100 ./internal/provider/codex -run 'Coordinator|ManagedStore|Adopt|Activate|Remove|Crash|Epoch'
go test -race -count=1 ./internal/app ./cmd/cq
```

- [x] Commit `feat: added Codex credential coordinator`.

## Stage 5: Managed refresh broker

**Files:**

- Create: `internal/provider/codex/refresh_broker.go`
- Create: `internal/provider/codex/refresh_broker_test.go`
- Modify: `internal/provider/codex/credential_coordinator.go`
- Modify: `internal/provider/codex/managed_store.go`
- Modify: `internal/provider/codex/provider.go`
- Modify: `cmd/cq/refresh.go`
- Modify: `cmd/cq/registry_pipeline.go`
- Modify: `cmd/cq/local_registry.go`
- Modify corresponding tests.

- [x] Add red eligibility table: only `cq_oauth + cq_owned_never_exported + ready` refreshes. Borrowed, legacy, exported, unknown, activation-pending, refreshing, and uncertain never call OAuth.
- [x] Add red equality/inequality tests: equal refresh token marks sharing/exported; unequal tokens never restore independence; activation makes ownership permanently exported; new CQ login creates new lineage.
- [x] Add red two-process race tests: refresh vs refresh exchanges once; refresh vs activate/remove has one coordinator outcome; stale expected revision rejects before exchange.
- [x] Add red crash tests: commit `refreshing` before exchange; uncertain network keeps old token unusable; success plus final-commit failure becomes `rotation_uncertain`; live process may retry commit only from retained response; restart never reuses uncertain token.
- [x] Implement per-lineage coordinator lock plus RPC revision fence. Singleflight coalesces same request inside owner but never substitutes for owner exclusivity.
- [x] Expose broker call to provider/quota/registry paths. These callers request eligible refresh; they never call `auth.RefreshCodexToken` or persist directly.
- [x] Require durable refreshed credential commit before returning candidate usable for dispatch.
- [x] Run `go test -race -count=100 ./internal/provider/codex -run 'Refresh|Lineage|Rotation|Coordinator'` and `go test -race -count=1 ./cmd/cq`.
- [x] Commit `feat: brokered Codex credential refresh`.

## Stage 6: Capacity ledger and indivisible route choice

**Files:**

- Create: `internal/proxy/codex_capacity.go`
- Create: `internal/proxy/codex_capacity_test.go`
- Modify: `internal/proxy/quota_cache.go`
- Modify: `internal/proxy/codex_selector.go`
- Modify: `internal/proxy/codex_selector_test.go`

```go
type RouteChoice struct {
    AccountKey      codex.AccountKey
    RequestedModel  string
    EffectiveModel  string
    RequiredBuckets []CapacityBucket
}
```

- [x] Add fake-clock red tests for source precedence, monotonic observation sequence, socket generation fence, reset epochs, hard-zero fence, newer positive live lift, stale facts, out-of-order updates, base/Spark scoped buckets, and no-scoped-data fallback.
- [x] Add red selection tests: known positive before unknown, known zero ineligible, all-zero returns typed cached limit, remaining percentage rank, active lease count tie-break, no system-active preference, Spark rewrite evaluates effective base bucket, pre-turn compaction requires both buckets.
- [x] Implement ledger keyed by opaque account key and canonical bucket. Keep bounded facts with source/generation/observed/reset/confidence.
- [x] Replace selector return with atomic `RouteChoice`; model rewrite consumes `EffectiveModel` and never reruns selection.
- [x] Run `go test -race -count=100 ./internal/proxy -run 'CodexCapacity|CodexSelector|RouteChoice'`.
- [x] Commit `feat: added Codex capacity choices`.

## Stage 7: Config modes, epochs, and readiness floor

**Files:**

- Modify: `internal/proxy/config.go`
- Modify: `internal/proxy/config_test.go`
- Create: `internal/proxy/codex_readiness.go`
- Create: `internal/proxy/codex_readiness_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`
- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/proxy_pin_test.go`

- [x] Add raw unknown-field storage to config load/save. Red N/N-1 round-trip tests cover `proxy pin`, reload, default generation, both new fields, and arbitrary future objects.
- [x] Add `off|observe|enforce` primary and WS modes, persistent mode epoch, shadow/authoritative epoch distinction, and restart-only application.
- [x] Add versioned readiness marker containing transport, CQ build, parser/lease schemas, exact client build, retry budget, fixture hash, installed result, and completed gates.
- [x] Add red validation tests: `enforce` rejected before implementation/marker; stale dimension invalidates marker; upgrade never creates marker; `observe -> enforce` starts fresh authoritative epoch; `enforce -> off -> enforce` preserves authoritative exact-turn fences and never promotes shadow state.
- [x] Extend `/health` with configured/effective mode and inhibition reason.
- [x] Run `go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Config|Mode|Readiness|Health|Pin'`.
- [x] Commit `feat: added Codex routing modes`.

## Stage 8: Explicit-account execution and relay decomposition

**Files:**

- Create: `internal/proxy/codex_request_scope.go`
- Create: `internal/proxy/codex_request_scope_test.go`
- Create: `internal/proxy/codex_attempt.go`
- Create: `internal/proxy/codex_attempt_test.go`
- Create: `internal/proxy/codex_http_relay.go`
- Create: `internal/proxy/codex_ws_relay.go`
- Create: `internal/proxy/codex_ws_relay_test.go`
- Modify: `internal/proxy/codex_transport.go`
- Modify: `internal/proxy/codex_transport_test.go`
- Modify: `internal/proxy/codex_handler.go`
- Modify: `internal/proxy/codex_compact.go`
- Modify: `internal/proxy/codex_live.go`
- Modify: `internal/proxy/server.go`
- Modify corresponding route tests.

```go
type ExplicitAccountExecutor interface {
    Do(context.Context, RouteChoice, CandidateAttempt, *http.Request) (*http.Response, error)
}
```

- [x] Add red call-site inventory tests proving translated Anthropic, count tokens, compact, images/search, native Responses, Live call, and Live sideband use explicit account choice. Each returned attempt contains typed account/candidate refs only.
- [x] Reduce `CodexTokenTransport` to request cloning, explicit credential injection, and selected model rewrite. Delete selection, account failover, suppression, persistence, and goroutines.
- [x] Implement request-scoped executor for non-turn-aware routes with bounded same-identity candidate recovery and pre-admission hard failure policy. Preserve Live call-ID affinity.
- [x] Split HTTP relay and one-to-one supervised WS relay shells from `server.go`; no leases yet. Add panic recovery, joined pumps, cancellation/deadline/close propagation, and generation fencing.
- [x] Add 1,000 connect/cancel/shutdown leak/race test. Preserve route status/body/model/headroom behaviour.
- [x] Run `go test -race -count=1 ./internal/proxy` and focused lifecycle test with `-count=100`.
- [x] Commit `refactor: decomposed Codex request routing`.

## Stage 9: Strict Responses protocol parsers

**Files:**

- Create: `internal/proxy/codex_turn_metadata.go`
- Create: `internal/proxy/codex_turn_metadata_test.go`
- Create: `internal/proxy/codex_protocol.go`
- Create: `internal/proxy/codex_protocol_test.go`
- Create: `internal/proxy/codex_sse.go`
- Create: `internal/proxy/codex_sse_test.go`
- Create: `internal/proxy/codex_zstd.go`
- Create: `internal/proxy/codex_zstd_test.go`
- Create: `internal/proxy/testdata/codex-0.146/*.json`
- Modify: `go.mod`
- Modify: `go.sum`

- [x] Add dependency `github.com/klauspost/compress/zstd` (or standard-library equivalent if available) only after red compressed fixtures require it.
- [x] Add exact synthetic 0.146 fixture cases: canonical nested metadata JSON string/object; flat compatibility; request kinds; compaction phases; memory without turn; typed empty prewarm; malformed/incomplete/oversized metadata.
- [x] Add exact installed 0.147.0-alpha.6.5 cases: object-shaped compaction metadata and canonical nested metadata larger than 64 KiB because of unbounded tool metadata. Bound nested metadata by protocol request size while retaining the 64 KiB direct-header and turn-state limits.
- [x] Parser priority: nested client metadata, direct HTTP header, flat fields, handshake hint. Validate complete session/thread/turn tuple; compare IDs as opaque strings only.
- [x] Add bounded zstd tests for decoded-size limit, expansion ratio, malformed streams, exact original-byte replay, and no decoder allocation before header bounds.
- [x] Add SSE parser tests for split/coalesced chunks, CRLF, multiline data, malformed/unknown/oversized events, `response.metadata`, well-formed `response.created` with response object, deltas, `response.completed.end_turn` tri-state, errors, and `codex.rate_limits`.
- [x] Add WS parser tests for wrapped 401/403 and exact hard-429 predicate: top-level error, 429 status, nested `usage_limit_reached`; every near-match stays soft.
- [x] Add unary compact parser and response-header turn-state parser.
- [x] Parsers remain observation-only. Relays still use Stage 8 behaviour.
- [x] Run `go test -race -count=100 ./internal/proxy -run 'TurnMetadata|CodexProtocol|CodexSSE|CodexZstd'`.
- [x] Commit `feat: parsed Codex Responses lifecycle`.

## Stage 10: Durable lane and lease core

**Files:**

- Create: `internal/proxy/codex_turn_lease.go`
- Create: `internal/proxy/codex_turn_lease_test.go`
- Create: `internal/proxy/codex_lease_store.go`
- Create: `internal/proxy/codex_lease_store_test.go`
- Create: `internal/proxy/codex_prewarm.go`
- Create: `internal/proxy/codex_prewarm_test.go`
- Modify: `internal/proxy/config.go`

Core identifiers and transitions:

```go
type LaneKey struct { Session, Thread, Namespace string }
type LeaseKey struct { Lane LaneKey; Turn string }

type LeaseState uint8
const (
    LeaseReserving LeaseState = iota + 1
    LeaseProvisional
    LeaseBoundActive
    LeaseContinuationPending
    LeaseBoundQuiescent
    LeaseOrphaned
    LeaseSuperseded
    LeaseExpired
    LeaseFailedUnadmitted
)
```

- [x] Add red state-table tests for every allowed/forbidden logical and attempt transition. `response.completed` never releases. `end_turn=false` becomes continuation-pending; true/absent becomes quiescent.
- [x] Add red lane tests: concurrent first acquisition selects once; same key reuses; different unseen ID supersedes only after predecessor routing refs drain; retained historical ID fails stale; opaque inequality only; root/subagent same session remain independent; failed successor never resurrects predecessor.
- [x] Add red prewarm tests: empty ID allowed only typed prewarm; admission fixes account/socket; one matching real turn atomically adopts response anchor/turn state/socket; mismatch/restart cannot inherit extinct lineage.
- [x] Add red continuation tests: first turn state wins; later mismatch fails; normal turns never inherit turn state; `previous_response_id` requires exact live `(AccountKey, UpstreamSocketGeneration)`; encrypted content retains account affinity.
- [x] Implement keyed HMAC journal with separate hashes for session/thread/turn/account/correlation and queryable lane components. Never hash whole composite into one unqueryable field.
- [x] Add red journal crash/corruption tests: key generation, `0o600`/`0o700`, write/sync/rename/dir-sync, ENOSPC, corrupt record, HMAC key loss, generation fence, compaction, restart orphaning, socket extinction, seven-day retention, active-ref no expiry, late-resume block.
- [x] Keep shadow and authoritative records distinct by mode epoch. Once in-memory admission occurs, journal failure leaves non-migratable fail-closed lease.
- [x] Run:

```bash
go test -race -count=100 ./internal/proxy -run 'CodexTurn|CodexLease|CodexPrewarm|CodexJournal'
```

- [x] Commit `feat: added durable Codex turn leases`.

## Stage 11: Observe-only integration

**Files:**

- Modify: `internal/proxy/codex_responses_http.go`
- Modify: `internal/proxy/codex_responses_ws.go`
- Modify: `internal/proxy/codex_attempt.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/diag.go`
- Modify: `internal/proxy/diag_test.go`
- Create: `internal/proxy/codex_observe_test.go`
- Modify: `cmd/cq/proxy.go`

- [x] Feed decoded HTTP requests, compact calls, WS handshakes/frames, SSE/WS events, capacity events, disconnects, and attempts into shadow lane/lease manager. Legacy selection stays authoritative for an unseen strong turn; its first actual admitted route becomes the same-turn continuity floor, while prospective shadow choices remain unused.
- [x] Add safe diagnostics: keyed turn hint, request kind, lease phase/generation, decision, reason, bucket, account hint, continuity. Assert raw fixtures and secrets absent.
- [x] Extend `/health` with shadow lease counts/failovers/quota/resync/unknown/late/stale/refresh-suspended counters.
- [x] Build deterministic 1,000-turn corpus covering simple, tool-loop, same-lane succession, parallel threads, subagents, prewarm, compaction, reconnect, HTTP/WS crossover, delayed stale traffic, and malformed metadata. Assert shadow decision never changes actual routing.
- [x] Add installed-fixture capture command that stores only schema-safe sanitised envelopes/hashes after explicit test invocation; never enable raw payload diagnostics automatically.
- [x] Run corpus under race detector and record fixture corpus hash.
- [x] Run 20 compiled-listener turns against a local synthetic upstream. Record zero strong-key/account mismatch and zero raw-ID/secret leak. Do not create readiness marker if any unknown lifecycle event remains.
- [x] Commit `feat: observed Codex turn routing`.

Installed 0.147.0-alpha.6.5 observe acceptance on 2026-08-09 found and fixed two parser gaps without enabling payload diagnostics: current object-shaped compaction metadata and canonical nested metadata above 64 KiB. Build `d6a7d31` then observed 3/3 zstd requests with metadata headers as strong keys, zero request-decode errors, zero metadata-parse errors, and independent active/quiescent leases. This is live parser evidence, not the Stage 15 elapsed-time soak or an enforcement marker.

Later live observe evidence found four continuity mismatches. A fresh quota-cache update changed the legacy selector's account while one exact turn ID continued sampling, proving that request-scoped legacy selection was not a safe observe authority. No readiness marker existed and enforcement remained disabled. Observe now reuses only the first actual route for an already-seen exact strong turn, blocks a successor until predecessor attempts drain, rejects retained stale IDs before upstream dispatch, and still lets the selector choose independently for an unseen successor or parallel lane. This safety floor does not promote or consume prospective shadow choices.

## Stage 12: HTTP enforcement

**Files:**

- Modify: `internal/proxy/codex_responses_http.go`
- Modify: `internal/proxy/codex_attempt.go`
- Modify: `internal/proxy/codex_readiness.go`
- Create: `internal/proxy/codex_responses_http_test.go`
- Modify: `cmd/cq/proxy.go`

- [x] Add red strong-metadata HTTP tests: repeated sampling pins account; mid-turn zero only affects future turns; parallel/new turns select independently; exact changed turn supersedes lane; stale ID fails; compact shares namespace; pre-turn compact binds both buckets.
- [x] Implement replay using original encoded bytes. Select provisional account once; try same-identity candidates; refresh eligible lineage; only pre-admission 401/403/exact hard 429 may change account.
- [x] Admit on accepted 2xx headers. Bind in memory, journal synchronously, then forward headers. Journal failure fails closed and cannot replay elsewhere.
- [x] Observe SSE without changing bytes/order. Classify clean terminal, premature EOF, read error, early close, cancellation. Never treat `response.completed` as whole-turn completion.
- [x] Enforce continuation: turn state stays exact account/lease; `previous_response_id` and encrypted content require proven affinity or typed continuity failure. No account migration after admission or after client-visible bytes.
- [x] Add `-race -count=100` no-post-admission-migration tests including compressed request replay and rate-limit/error ordering.
- [x] Add explicit validation command/path that writes HTTP readiness marker only after Stage 11 corpus hash, installed build, parser/lease schema, and HTTP gate results match.
- [x] Run full HTTP gate; then opt in explicitly and verify `/health` effective HTTP `enforce`.
- [x] Commit `feat: enforced Codex HTTP turn leases`.

## Stage 13: WebSocket resynchronisation proof

**Files:**

- Modify: `internal/proxy/codex_responses_ws.go`
- Modify: `internal/proxy/codex_protocol.go`
- Modify: `internal/proxy/codex_turn_lease.go`
- Create: `internal/proxy/codex_resync_test.go`
- Create: `cmd/cq/codex_validate.go`
- Create: `cmd/cq/codex_validate_test.go`

- [x] Implement shadow-only rotation intent keyed to exact turn, mode epoch, downstream generation, upstream generation, client build, and retry budget.
- [x] Require model-bearing handshake and first-frame model match before any model-aware prospective decision. Stale handshake metadata loses to current frame.
- [x] Preserve upstream 101 semantics, required beta/header/subprotocol values, and permessage-deflate across two legs. Pass pre-upgrade final 401/403/429/426 status/body/safe headers without downstream upgrade.
- [x] Add exact wrapped `previous_response_not_found` event. Do not require graceful close: client receives error, invalidates socket, then reconnects or crosses to HTTP with portable full request.
- [x] Never transparently replay incremental frame or open replacement upstream behind same downstream socket. Same-account reconnect also invalidates predecessor response ID.
- [ ] Run 100 trials for each explicitly supported CLI/Desktop build and retry budget 0, 1, exhausted. Prove error -> client invalidation -> new WS generation or HTTP crossover -> full portable request before replacement dispatch.
- [x] Test full new-turn rotation trigger separately. Keep unsupported trigger account-affine/fail-closed until fixture proves signal.
- [x] Record proof result but keep WS `observe`; no WS readiness marker yet.
- [x] Commit Stage 13 proof with unsupported-build blocker recorded.

## Stage 14: WebSocket enforcement

**Promotion blocker:** Codex CLI/Desktop 0.146.0 and installed Desktop `0.147.0-alpha.6.5` send turn metadata but no model in the WebSocket HTTP handshake. Exact `0.147.0-alpha.6.5` source at `618b8e9111da9f57fe380b09d0f6516e3f343536` builds handshake headers from Responses compatibility metadata, session headers, attestation, and beta headers; it defines no `x-codex-model` projection. Model first appears in `response.create` after downstream `101`, while approved model-aware account selection and stateful upstream `101` journalling must finish before downstream `101`. No WS readiness marker may be written without a client change or approved architecture revision.

**Files:**

- Modify: `internal/proxy/codex_responses_ws.go`
- Modify: `internal/proxy/codex_readiness.go`
- Modify: `internal/proxy/server.go`
- Modify: `cmd/cq/codex_validate.go`
- Create: `internal/proxy/codex_responses_ws_test.go`

- [ ] Add red broker tests for multiple turns on one socket, repeated sampling, prewarm adoption, rate-limit buffering, exact admission events, malformed commit-to-current behaviour, changed turn while active, cancellation drain, and late-generation fencing.
- [ ] Before downstream 101, dial provisional upstream; apply bounded pre-upgrade recovery; bind/journal any stateful 101 before sending downstream 101.
- [ ] Supervise exactly one reader and serialised writer per connection. Join pumps, propagate close/error/deadline/shutdown, recover panics, and reject changed turn until predecessor routing work drains.
- [ ] On new turn/account/socket change, record intent and use only Stage 13 proven reconnect path. Consume intent once on same-turn portable full request. Never cross upstream generation with `previous_response_id`.
- [ ] Run installed listener tests: same-turn sampling, parallel turns, quota exhaustion, 60-minute/restart reconnect, full-history account change, exact bytes/error propagation, retry budgets, and zero late mutations.
- [ ] Confirm remaining blocking gates: Desktop frame metadata, canonical live bucket names, zero unknown lifecycle, 101 semantics, zstd/compact, remote encrypted-content portability or permanent affinity, and model-bearing handshake.
- [ ] Atomically write WS readiness marker only when every exact build/schema/retry tuple gate passes. Explicitly opt in; restart/drain; verify `/health` effective WS `enforce`.
- [ ] Run `go test -race -count=100 ./internal/proxy -run 'CodexResponsesWS|CodexResync|CodexTurn|CodexLease'` plus full suite.
- [ ] Commit `feat: enforced Codex WebSocket leases`.

## Stage 15: Installed-service soak and default policy

**Promotion blocker:** deterministic/local-fake acceptance has one admitted turn on one observed day. Promotion still requires seven consecutively observed UTC days, at least 100 admitted installed-service turns, and all zero-failure counters.

**Files:**

- Create: `docs/operations/codex-routing-canary.md`
- Create: `cmd/cq/codex_canary.go`
- Create: `cmd/cq/codex_canary_test.go`
- Modify: `internal/proxy/codex_readiness.go`
- Modify: `internal/proxy/codex_selector.go`
- Modify: `internal/proxy/codex_transport.go`
- Modify: `cmd/cq/proxy.go`
- Modify related docs/help/tests.

- [x] Add privacy-safe canary recorder: admitted-turn count, keyed mismatch count, automatic auth/registry hash-change count, secret-leak count, unexplained lifecycle count, start/end time, build/schema/marker tuple. No raw identifiers or credentials.
- [ ] Run installed service for seven consecutive days and at least 100 admitted turns. Include long turn past quota depletion, parallel short turns, next-turn reselection, same-lane supersession, restart during quiescence, explicit switch, and Codex Bar observation.
- [ ] Verify automatic activity leaves system-auth and registry hashes unchanged; explicit switch alone changes system hash.
- [x] Rehearse rollback in deterministic restart tests: transition `enforce -> off`, retain exact authoritative account fence, route unseen turns through legacy path, and prove shadow epochs never promote. Installed-service drain remains part of soak acceptance.
- [x] Only in later release, document enforcement as recommended after validation. Never default existing install to effective enforce and never synthesise readiness marker during upgrade.
- [ ] After rollback window, delete dead request-scoped Codex selector failover/suppression/switch code proven unreachable. Keep `off/observe` executor path.
- [x] Run final gates:

```bash
go test -race -count=100 ./internal/proxy -run 'CodexTurn|CodexLease|CodexResponses'
go test -race -count=100 ./internal/provider/codex -run 'Inventory|Candidate|Refresh|System'
go build ./...
go vet ./...
go test -race -count=1 ./...
git diff --check
```

- [x] Audit current source for forbidden paths:

```bash
rg -n 'Accounts\.Switch|CodexAccountSwitcher|PersistCodexAccount|RefreshCodexToken|active_account_key|auth\.json' internal cmd
```

Every remaining match must be explicit coordinator/activator code, auth protocol code, or synthetic test fixture.

- [x] Run installed-listener acceptance from approved blueprint, save privacy-safe evidence, and verify current readiness marker dimensions.
- [ ] Commit `feat: completed Codex routing canary`.

Installed binary `0.20.3+6c03ebe` (SHA-256 `f63be75789e5b49f5998ad50afffa921d9fb75256eda2304ce6fecd87383ef10`) passed self-verifying loopback acceptance on 2026-08-09 after the shared transport-lease fix: 20 strong-key turns, 40 upstream requests, 20 selector calls, same-account repeated sampling, mixed plain/zstd bodies, zero continuity errors, and zero unknown lifecycle events. It wrote a mode-`0600` temporary HTTP marker matching parser schema 1, lease schema 1, retry budget 1, Desktop build `0.147.0-alpha.6.5`, and fixture hash `618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1`. Synthetic credentials and a loopback upstream prevented user-auth or raw-identifier capture. The live marker/config remained unchanged, and launchd PID 30030 stayed at one run with no restart during validation.

## Final completion audit

- [ ] Read complete diff from Stage 1 base to Stage 15 head.
- [ ] Trace every system-auth/registry/managed writer to coordinator capability. Trace every native/translated/Live route to explicit account executor.
- [x] Prove HTTP and WS share one lease manager and namespace; prove journal restart resolves account by opaque `AccountKey` and never token/path/email.
- [ ] Prove `response.completed` only changes sampling state; changed unseen turn ID after drained work is successor boundary; retained history blocks stale traffic.
- [ ] Prove no account identity migrates after admission and no incremental input crosses upstream generation.
- [ ] Confirm worktree contains no credential snapshots, raw installed fixtures, payload logs, canary raw IDs, or temp journals.
- [ ] Run full commands once more on clean tree. Record exact commit, Go version, Codex client/Desktop build, fixture hash, and installed-service marker in final report.
- [ ] Do not push or open PR without explicit user approval.

Final audit found HTTP enforcement and WebSocket observation initially shared only a journal and namespace, not live lease state. Mode-specific views now share one synchronised lease-map core, while retaining independent mode epochs and authority flags. A 100-run race test proves WS-observed exact-turn state remains on its first account when traffic crosses to HTTP enforcement. Existing restart tests resolve freshly discovered opaque `AccountKey` values from HMAC journal fields and assert raw session, thread, turn, account, response, and turn-state values never persist.
