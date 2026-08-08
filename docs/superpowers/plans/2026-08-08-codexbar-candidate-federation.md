# CodexBar Candidate Federation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let CQ route through fresh CodexBar-managed credentials without copying, refreshing, activating, removing, or otherwise mutating CodexBar state.

**Architecture:** Add a generic read-only external credential source to the Codex inventory. A CodexBar adapter reads its manifest, validates each declared managed home and credential revision, and contributes exact candidates to existing logical-account reconciliation. Candidate secrets remain behind the coordinator and are resolved only for a pinned request attempt.

**Tech Stack:** Go 1.26.1, standard library JSON/filesystem/crypto packages, existing `fsutil`, Codex inventory, credential coordinator/control, request router, and Go race detector.

---

## Execution contract

- Work test-first. Confirm each named test fails for expected missing behaviour before production edits.
- Never fixture, log, or persist real tokens, emails, account IDs, home paths, or manifest UUIDs.
- Treat CodexBar files as external authority. CQ performs reads only.
- Reject symlinks, non-regular files, unsafe permissions, untrusted ownership, escaped managed-home paths, manifest/auth identity conflicts, and manifest fingerprint conflicts.
- Re-resolve exact candidate revision before every attempt. Stale references fail closed and trigger inventory refresh; they never silently bind a different revision.
- Existing system and CQ-managed candidates retain their current semantics.

## Task 1: Define external source boundary

**Files:**

- Create: `internal/provider/codex/external_source.go`
- Create: `internal/provider/codex/external_source_test.go`
- Modify: `internal/provider/codex/inventory.go`

- [ ] Add a failing compile-time/domain test proving an external source can enumerate metadata-only candidates and resolve one exact revision without exposing credential material in list results.
- [ ] Add source enum and contracts:

```go
const (
    SourceSystem CredentialSource = iota + 1
    SourceManaged
    SourceExternal
)

type ExternalCandidateRef struct {
    Source      string
    RecordID    string
    Revision    Revision
}

type ExternalCredentialSource interface {
    Name() string
    List(context.Context) ([]ExternalCandidate, error)
    Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error)
}
```

- [ ] Keep source-specific locator private to coordinator-facing candidate data. Diagnostic/account output exposes source name, health, and non-secret candidate ID only.
- [ ] Add typed errors for unavailable source, invalid manifest, unsafe path, stale revision, identity mismatch, and fingerprint mismatch.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex -run 'ExternalSource|ExternalCandidate'
```

## Task 2: Parse and validate CodexBar manifest

**Files:**

- Create: `internal/provider/codex/codexbar_source.go`
- Create: `internal/provider/codex/codexbar_source_test.go`

- [ ] Add table-driven failing tests using `t.TempDir` for:
  - missing manifest: source unavailable, not fatal to inventory;
  - valid manifest plus `0o600` regular auth file;
  - duplicate manifest record IDs;
  - relative, empty, or escaped managed-home path;
  - symlinked managed home, auth file, or manifest;
  - group/world-readable manifest or auth file;
  - non-regular auth file;
  - current-user ownership rejection through injected ownership checker;
  - malformed JSON, missing declared identity, and missing auth file;
  - manifest fingerprint mismatch;
  - manifest identity conflicting with decoded credential claims;
  - changed auth bytes between list and resolve producing `ErrStaleRevision`.
- [ ] Parse only required manifest fields and preserve no external data:

```go
type codexBarManifestRecord struct {
    ID                string `json:"id"`
    ManagedHomePath   string `json:"managedHomePath"`
    ProviderAccountID string `json:"providerAccountID"`
    WorkspaceAccountID string `json:"workspaceAccountID"`
    AuthFingerprint   string `json:"authFingerprint"`
}
```

- [ ] Default root to current user's `Library/Application Support/CodexBar`. Accept test override through constructor, not environment-variable production behaviour.
- [ ] Resolve each declared `managedHomePath/auth.json` only after lexical containment and evaluated-path containment under CodexBar application-support root.
- [ ] Use `Lstat` before reads and `EvalSymlinks` containment. Validate regular file, `perm&0o077 == 0`, and current-user ownership before parsing.
- [ ] Decode existing Codex auth schema and claims with current inventory helpers. Require strong account identity and reconcile manifest IDs with token claims.
- [ ] Compute external candidate revision from exact credential material and file identity. Validate current CodexBar 64-character hexadecimal SHA-256 fingerprint against the exact auth bytes; reject unknown non-empty formats until explicitly supported.
- [ ] Return metadata-only candidates from `List`; reopen, revalidate, and revision-check in `Resolve`.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex -run 'CodexBar'
```

## Task 3: Federate external candidates into logical inventory

**Files:**

- Modify: `internal/provider/codex/inventory.go`
- Modify: `internal/provider/codex/inventory_test.go`
- Modify: `internal/provider/codex/accounts.go`
- Modify: `internal/provider/codex/accounts_test.go`

- [ ] Add failing inventory fixtures proving:
  - fresh external plus stale managed becomes one logical account with both candidates;
  - external candidate can be preferred for an attempt without changing `Active`;
  - same email but conflicting strong provider account ID remains separate/unroutable;
  - source enumeration order does not change account or candidate IDs;
  - unavailable/invalid external source produces typed source health while system/CQ inventory still loads.
- [ ] Change inventory construction to accept zero or more injected `ExternalCredentialSource` values.
- [ ] Reconcile external candidates only through existing strong identity evidence. Never use email alone to merge conflicting accounts.
- [ ] Keep one compatibility account row and retain all same-identity candidate revisions behind it.
- [ ] Rank usable same-identity candidates by accepted revision/freshness, not source ownership. Keep deterministic source/candidate tie-breaks.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex -run 'Inventory|Discover|External'
```

## Task 4: Resolve external secrets through coordinator only

**Files:**

- Modify: `internal/provider/codex/credential_coordinator.go`
- Modify: `internal/provider/codex/credential_coordinator_test.go`
- Modify: `internal/provider/codex/credential_control.go`
- Modify: `internal/provider/codex/credential_control_unix_test.go`
- Modify: `internal/proxy/codex_attempt_test.go`

- [ ] Add failing tests proving:
  - local coordinator resolves exact external revision for attempt execution;
  - delegated control client receives no token in list/diagnostic RPC;
  - stale external revision is not substituted;
  - external 401 tries next same-identity candidate before any cross-account selection;
  - refresh broker never refreshes an external candidate;
  - activate, adopt, remove, and registry projection reject external mutation or operate only on explicitly chosen CQ/system material without touching external files.
- [ ] Inject external sources into coordinator owner. Keep source instances out of RPC clients.
- [ ] Extend resolver dispatch for `SourceExternal`; call source `Resolve` only inside secret-bearing request execution.
- [ ] Return typed retryable stale-revision result so caller refreshes inventory once and reselects within same logical account.
- [ ] Preserve existing 401 order: remaining same-identity candidates, one eligible CQ-managed refresh, refreshed CQ candidate, then caller-controlled cross-account decision.
- [ ] Run:

```bash
go test -race -count=1 ./internal/provider/codex ./internal/proxy -run 'Coordinator|Control|External|CodexRequestRouter'
```

## Task 5: Wire production discovery and diagnostics

**Files:**

- Modify: `cmd/cq/proxy.go`
- Modify: `cmd/cq/proxy_test.go`
- Modify: `internal/provider/codex/provider.go`
- Modify: `internal/provider/codex/provider_test.go`
- Modify: `internal/provider/codex/AGENTS.md`

- [ ] Add failing command/provider tests proving default production construction discovers CodexBar when present, degrades when absent, and reports source health without paths or identifiers.
- [ ] Construct one CodexBar source at the credential-coordinator owner and pass it to inventory/provider consumers.
- [ ] Add safe diagnostics: source name, candidate count, and typed health only. Never emit managed-home path, manifest record ID, email, account ID, token fingerprint, or revision.
- [ ] Document read-only ownership boundary in provider instructions.
- [ ] Run:

```bash
go test -race -count=1 ./cmd/cq ./internal/provider/codex ./internal/proxy
go test -race -count=1 ./...
go build ./...
go vet ./...
git diff --check
```

## Task 6: Reproduce fixed failure without writes

**Files:**

- No repository changes required.

- [ ] Snapshot hashes, mtimes, sizes, and modes for system, CQ-managed, CodexBar manifest, and CodexBar auth files. Do not print content or identifiers.
- [ ] Run installed-candidate discovery and account-pinned usage fetch through tested build.
- [ ] Prove fresh CodexBar candidate succeeds for same logical account where stale CQ candidate returns 401.
- [ ] Re-snapshot and prove every external/system/CQ credential file remains byte-identical.
- [ ] Record privacy-safe counts and typed outcomes in local test evidence only.
