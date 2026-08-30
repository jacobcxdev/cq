# Linux Runtime Compatibility Implementation Plan

> **For Codex:** Execute this plan task by task with
> `superpowers:executing-plans`. Apply test-driven development for every
> behavioural slice. Keep packaging-owned unit mutation and service lifecycle
> outside this branch.

**Goal:** Give Linux the same owned proxy supervisor/worker runtime, exact
kernel-backed process and listener evidence, installed HTTP/SSE/WebSocket
validation, kernel-enforced acceptance confinement, and native systemd-user
proof already required by the approved design.

**Architecture:** Reuse CQ's existing runtime supervisor on Darwin and Linux
through a small Unix layer. Add bounded procfs collectors and compose them with
packaging-owned systemd manager facts. Run installed validation candidates as
ephemeral owned runtimes, and execute the installed Codex client in a Linux
user/network namespace with descriptor-only loopback relays, `no_new_privs`,
and Landlock write confinement. Prove behaviour in focused race tests and a
standard ephemeral Linux CI job.

**Tech stack:** Go, `golang.org/x/sys/unix`, procfs, cgroup v2, Linux user and
network namespaces, Unix descriptor passing, Landlock, systemd user services,
GitHub Actions Ubuntu runners.

**Integration contract:** Packaging branch
`jacobcxdev/feat/cross-platform-installer` at `839c3bb` owns product unit
rendering and lifecycle. Fixed service is `cq-proxy.service`, with exact
`ExecStart=<absolute-cq> proxy start`.

---

## Task 1: Extract portable Unix owned-runtime wiring

**Files:**

- Create: `cmd/cq/proxy_unix.go`
- Create: `cmd/cq/proxy_unix_test.go`
- Create: `cmd/cq/proxy_linux.go`
- Create: `cmd/cq/proxy_linux_test.go`
- Modify: `cmd/cq/proxy_darwin.go`

### Step 1: Add failing Unix lifecycle and launcher tests

Move only portable runtime behaviours into tests tagged `darwin || linux`:

```go
func TestUnixRuntimeLifecycleOpensExactHolder(t *testing.T) {
	// create secure lifecycle file, open supervisor holder, and require the
	// returned proof to validate against the still-open descriptor.
}

func TestUnixRuntimeLauncherUsesInheritedLifecycleDescriptor(t *testing.T) {
	// require exact current executable, runtime worker role arguments, and an
	// inherited lifecycle descriptor resolved through the platform fd root.
}
```

Add a Linux-only wiring test requiring non-stub platform hooks:

```go
func TestLinuxProxyStartWiresOwnedRuntime(t *testing.T) {
	if reflect.ValueOf(runProxyOwnedRuntimeFn).Pointer() == reflect.ValueOf(defaultRunProxyOwnedRuntime).Pointer() {
		t.Fatal("Linux owned runtime remains unavailable")
	}
}
```

Run:

```bash
go test -race -count=1 ./cmd/cq -run '^(TestUnixRuntime|TestLinuxProxyStart)'
```

Expected: FAIL because portable helpers and Linux hook registration do not
exist.

### Step 2: Extract portable runtime helpers

Create `proxy_unix.go` with build tag `darwin || linux`. Move these behaviours
without changing protocol or policy:

- owned listener bind and lifecycle initialisation;
- adopted supervisor setup;
- lifecycle holder open and proof construction;
- `RuntimeProcessWorkerLauncher` construction;
- inherited worker listener adoption;
- lifecycle file initialisation.

Use one platform-specific fd path helper:

```go
func runtimeDescriptorPath(fd uintptr) string {
	return filepath.Join(runtimeDescriptorRoot(), strconv.FormatUint(uint64(fd), 10))
}
```

Darwin returns `/dev/fd`; Linux returns `/proc/self/fd`. Keep existing Darwin
function names as thin wrappers so launchd behaviour and existing tests remain
unchanged.

### Step 3: Register Linux runtime hooks

In `proxy_linux.go`, register only runtime and inspection hooks:

```go
func init() {
	defaultProxyInspectionTarget = linuxProxyInspectionTarget
	proxyInspectionTargetForRoot = linuxProxyInspectionTargetForRoot
	adoptProxyListenerFn = adoptUnixProxyListener
	newProxyRuntimeWorkerLauncherFn = newUnixProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runUnixProxyAdoptedRuntime
	runProxyOwnedRuntimeFn = runUnixProxyOwnedRuntime
}
```

Do not add product unit install, restart, stop, or remove operations.

### Step 4: Verify Darwin preservation and Linux compilation

Run:

```bash
go test -race -count=1 ./cmd/cq -run '^(TestUnixRuntime|TestLinuxProxyStart|TestProxy)'
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go test -c ./cmd/cq -o "$TMPDIR/cq-darwin.test"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./cmd/cq -o "$TMPDIR/cq-linux.test"
```

Expected: PASS. Remove both temporary test binaries.

### Step 5: Commit

Commit subject:

```text
feat: added Linux runtime ownership
```

Body:

```text
- shared supervisor and worker lifecycle behaviour across Unix platforms
- wired Linux startup to the owned runtime protocol
- preserved existing macOS service behaviour
```

---

## Task 2: Add bounded Linux procfs collectors

**Files:**

- Create: `internal/proxy/linux_process.go`
- Create: `internal/proxy/linux_process_test.go`
- Create: `internal/proxy/linux_procfs.go`
- Create: `internal/proxy/linux_procfs_test.go`
- Create: `internal/proxy/linux_listener.go`
- Create: `internal/proxy/linux_listener_test.go`

### Step 1: Add failing parser tests

Define fixtures for:

- `/proc/<pid>/stat` with spaces and `)` in command names;
- `Uid:` with exactly four equal current-user values;
- NUL-terminated exact command lines;
- cgroup v2 single-membership lines;
- TCP4/TCP6 listener rows and socket inodes;
- `/proc/<pid>/fd` `socket:[inode]` links.

Require rejection of oversized input, missing final NUL, duplicate fields,
numeric overflow, non-listen socket state, wildcard/non-loopback addresses,
extra matching listeners, and PID/start-time changes.

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^TestLinuxProc'
```

Expected: FAIL because collectors do not exist.

### Step 2: Implement bounded parsers and injected reader

Add package-private types:

```go
type linuxProcessIdentity struct {
	PID, ParentPID int
	StartTime      uint64
	UID            uint64
	Executable     codexInstalledExecutableProof
}

type linuxListenerIdentity struct {
	Address string
	Inode   uint64
	Owner   linuxProcessIdentity
}

type linuxProcFS interface {
	ReadFile(path string, max int64) ([]byte, error)
	Readlink(path string) (string, error)
	ReadDir(path string, maxEntries int) ([]os.DirEntry, error)
}
```

Apply explicit limits: 64 KiB per proc file, 4,096 process entries, 4,096
descriptor entries, 65,536 TCP rows, and no unbounded traversal. Parse fields
from bytes and reject ambiguity.

### Step 3: Bind executable and socket identity

Open `/proc/<pid>/exe` without following a caller-controlled path, `fstat` the
descriptor, hash through the descriptor, and compare path/device/inode with
the expected executable proof. Resolve expected listener inode from
`/proc/net/tcp{,6}` and require exactly one matching descriptor owner.

### Step 4: Add live self-process proof

Linux-only test opens a TCP4 loopback listener, captures current process and
listener identity, closes the listener, and requires revalidation to fail.
No external commands or root authority.

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^TestLinuxProc'
```

Expected: PASS.

### Step 5: Commit

Commit subject:

```text
feat: added Linux runtime collectors
```

Body:

```text
- parsed bounded process, cgroup, and socket kernel facts
- bound listeners to stable process and executable identities
- rejected malformed, ambiguous, and stale observations
```

---

## Task 3: Compose Linux inspection and installed process attestation

**Files:**

- Create: `cmd/cq/proxy_inspect_linux.go`
- Create: `cmd/cq/proxy_inspect_linux_test.go`
- Create: `internal/proxy/codex_installed_process_attestation_linux.go`
- Create: `internal/proxy/codex_installed_process_attestation_linux_test.go`
- Modify: `internal/proxy/codex_installed_process_attestation_other.go`
- Modify: `internal/proxy/codex_installed_process_attestation.go`
- Modify: `internal/proxy/codex_installed_acceptance_harness.go`
- Modify: `internal/proxy/codex_readiness.go`

### Step 1: Add failing systemd-user proof tests

Model manager facts as injected, already-inspected packaging authority:

```go
type linuxSystemdUserServiceProof struct {
	Unit          string
	UnitPath      string
	MainPID       int
	ControlGroup  string
	ExecStart     []string
	Executable    codexInstalledExecutableProof
	UnitSHA256    [sha256.Size]byte
}
```

Tests require exactly `cq-proxy.service`, absolute unit path under effective
user config, one main PID, exact two-argument suffix `proxy start`, matching
current CQ executable, and canonical cgroup v2 membership. Near-match unit
names, shell indirection, extra flags, replaced unit/executable, PID reuse,
wrong UID/cgroup/listener, and multiple service matches must fail.

Run:

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run '^TestLinux.*(Inspection|Attestation|Systemd)'
```

Expected: FAIL.

### Step 2: Add `systemd-user` service kind

Add:

```go
const codexInstalledListenerServiceSystemdUser codexInstalledListenerServiceKind = "systemd-user"
```

Accept it only in installed-artifact, readiness, and persistent-listener
switches. Do not make ephemeral proofs persistent.

### Step 3: Implement Linux verifier

Narrow unsupported verifier build tag to `!darwin && !linux`. Linux verifier:

1. captures exact current executable proof;
2. consumes injected packaging service proof;
3. captures procfs PID/start/UID/executable/cgroup facts;
4. binds exact loopback listener ownership;
5. hashes canonical service, executable, process, cgroup, and listener fields;
6. repeats mutable captures before returning;
7. emits generic `errCodexInstalledProcessAttestation` for any mismatch.

No service mutation and no human-readable `systemctl status` parsing.

### Step 4: Wire command inspection

`linuxProxyInspectionTargetForRoot` combines packaging-owned manager collector
with runtime-owned process/listener/data-plane collectors. Runtime collector
must remain independently useful when manager evidence is unavailable, while
reconciliation stays fail closed for installed validation.

### Step 5: Verify

Run:

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run '^TestLinux.*(Inspection|Attestation|Systemd)'
go test -race -count=1 ./internal/proxy -run '^(TestCodexInstalledProcess|TestCodexReadiness)'
```

Expected: PASS.

### Step 6: Commit

Commit subject:

```text
feat: attested Linux proxy identity
```

Body:

```text
- composed systemd user-service facts with kernel process evidence
- required exact executable, cgroup, and listener ownership
- extended installed readiness to persistent Linux services
```

---

## Task 4: Add Linux installed candidate authority

**Files:**

- Create: `cmd/cq/proxy_http_validation_service_linux.go`
- Create: `cmd/cq/proxy_http_validation_service_linux_test.go`
- Modify: `cmd/cq/proxy_http_validation_service_stub.go`
- Modify: `cmd/cq/proxy_http_validation_cli.go`
- Modify: `cmd/cq/proxy_candidate_runtime.go`

### Step 1: Add failing exact-binding tests

Tests cover persistent service binding and isolated candidate binding. Require:

- persistent label `cq-proxy.service` and default port;
- candidate identity generated from exact current executable, isolated port,
  owned supervisor manifest, process start time, executable digest, and
  listener inode;
- one active supervisor and one ready worker;
- revalidation after traffic;
- no product unit creation or mutation.

Near-match command vectors, current-executable mismatch, extra listeners,
stale PID, stopped worker, or changed binding digest fail.

Run:

```bash
go test -race -count=1 ./cmd/cq -run '^TestLinuxInstalledHTTPValidation'
```

Expected: FAIL.

### Step 2: Implement persistent binding resolver

Narrow stub tag to `!darwin && !linux`. Resolve fixed manager proof through
the same injected contract as Task 3. Read and hash exact CQ executable and
service identity with existing safe file helpers. Return existing
`installedHTTPValidationServiceBinding`.

### Step 3: Implement isolated candidate runtime authority

Use existing owned-runtime protocol directly. Candidate owns temporary
lifecycle state and isolated TCP4 loopback listener. Return cleanup closure
that cancels supervisor, waits for descendants, closes relay/listener files,
removes temporary state, and joins cleanup failures.

Replace candidate restart callback with owned-runtime worker replacement; do
not call `systemctl` or install a fourth service.

### Step 4: Verify request binding and restart

Run:

```bash
go test -race -count=1 ./cmd/cq -run '^(TestLinuxInstalledHTTPValidation|TestRunProxyHTTPValidation|TestInstalledHTTPValidation)'
```

Expected: PASS.

### Step 5: Commit

Commit subject:

```text
feat: added Linux validation authority
```

Body:

```text
- bound installed validation to exact Linux service evidence
- ran isolated candidates through owned supervisor control
- revalidated executable, process, and listener facts around traffic
```

---

## Task 5: Extract acceptance confinement hook without Darwin regression

**Files:**

- Create: `internal/proxy/codex_acceptance_confinement.go`
- Create: `internal/proxy/codex_acceptance_confinement_darwin.go`
- Create: `internal/proxy/codex_acceptance_confinement_darwin_test.go`
- Create: `internal/proxy/codex_acceptance_confinement_other.go`
- Modify: `internal/proxy/codex_acceptance_installed.go`
- Modify: `internal/proxy/codex_installed_http_client_exercise_test.go`

### Step 1: Characterise Darwin command preparation

Add tests proving identical sandbox profile, executable materialisation,
arguments, environment, directory, output, timeout, and post-run identity
checks through a platform preparation interface:

```go
type codexAcceptanceConfinement interface {
	Prepare(context.Context, codexAcceptanceCommand, codexInstalledExecutableProof) (codexAcceptanceExecution, error)
}
```

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^(TestCodexAcceptanceSandbox|TestDarwinCodexAcceptanceConfinement)'
```

Expected: FAIL before extraction.

### Step 2: Move Darwin-only logic behind build tag

Common runner retains executable identity/materialisation and process
execution. Darwin implementation prepares `/usr/bin/sandbox-exec` with exact
existing profile. Unsupported platforms fail closed. Remove common
`runtime.GOOS` branch.

### Step 3: Verify existing acceptance suite

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^(TestCodexAcceptance|TestCodexInstalledHTTPClientExercise)'
```

Expected: PASS with unchanged Darwin semantics.

### Step 4: Commit

Commit subject:

```text
refactor: isolated acceptance confinement
```

Body:

```text
- moved platform sandbox preparation behind a narrow interface
- preserved executable identity and cleanup checks
- kept unsupported platforms fail closed
```

---

## Task 6: Implement Linux namespace relay and Landlock confinement

**Files:**

- Create: `internal/proxy/codex_acceptance_confinement_linux.go`
- Create: `internal/proxy/codex_acceptance_confinement_linux_test.go`
- Create: `internal/proxy/linux_namespace_helper.go`
- Create: `internal/proxy/linux_namespace_helper_test.go`
- Create: `internal/proxy/linux_relay.go`
- Create: `internal/proxy/linux_relay_test.go`
- Create: `internal/proxy/linux_landlock.go`
- Create: `internal/proxy/linux_landlock_test.go`
- Modify: `internal/proxy/candidate_confinement_linux.go`
- Modify: `internal/proxy/candidate_confinement_other.go`
- Modify: `internal/proxy/candidate_confinement_test.go`

### Step 1: Add failing namespace protocol tests

Define bounded versioned control messages and relay IDs. Tests require one
control descriptor, reject unknown/duplicate relay IDs and extra passed
descriptors, and prove helper readiness arrives only after namespace,
`no_new_privs`, loopback, descriptor closure, and Landlock succeed.

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^TestLinux(Namespace|Relay|Landlock|AcceptanceConfinement|CandidateConfinement)'
```

Expected: FAIL.

### Step 2: Implement descriptor-only loopback relays

Trusted parent opens real validation endpoint and deny-only egress guard.
Helper owns fresh network namespace and loopback relay ports. For each accepted
connection it sends exactly one relay ID plus one connection descriptor via
`SCM_RIGHTS`. Parent connects descriptor only to pre-enumerated destination.
Apply byte, connection, message, descriptor, and idle limits. Recover panics in
relay goroutines and cancel full group on protocol failure.

### Step 3: Implement ordered namespace helper setup

Spawn exact current executable in hidden helper role with:

```go
SysProcAttr: &syscall.SysProcAttr{
	Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWNS,
	UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
	GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
	GidMappingsEnableSetgroups: false,
}
```

Helper calls `PR_SET_NO_NEW_PRIVS`, brings loopback up using netlink syscalls,
closes all unlisted descriptors, applies Landlock, reports ready, then executes
the exact retained client descriptor. Any partial setup exits without running
client.

### Step 4: Apply Landlock write policy

Negotiate ABI with `LANDLOCK_CREATE_RULESET_VERSION`. Handle all supported
write rights, including `REFER` and `TRUNCATE` when available. Add path-beneath
rules only for unique validation root and required null-device access. Close
external writable descriptors before enforcement. If kernel cannot enforce
base write restriction, fail closed.

### Step 5: Bind capability check to actual launcher

Linux `candidatePlatformConfinementAvailable` runs a cached, side-effect-free
kernel capability probe using same namespace and Landlock primitives as
launcher. Candidate launch calls full confinement preparation; passing
`ValidateCandidateConfinement` alone never authorises unconfined execution.

### Step 6: Add native kernel tests

On Linux, spawn helper and prove:

- allowed file creation beneath validation root;
- denied write and truncate outside root;
- allowed HTTP/SSE and WebSocket relay traffic to enumerated destinations;
- denied direct external socket connection;
- deny-only egress relay increments attempts;
- unknown relay, extra descriptor, disabled namespace, Landlock failure,
  timeout, client crash, and cleanup failure all fail closed;
- no helper, relay, socket, mount, namespace, or temporary root remains.

Tests skip only when kernel itself denies unprivileged user namespaces, and CI
must treat such skip as failure in native proof job.

Run:

```bash
go test -race -count=1 ./internal/proxy -run '^TestLinux(Namespace|Relay|Landlock|AcceptanceConfinement|CandidateConfinement)'
```

Expected: PASS on supported Linux; parser/unit tests pass everywhere selected
by build tags.

### Step 7: Commit

Commit subject:

```text
feat: confined Linux acceptance clients
```

Body:

```text
- isolated clients in user and network namespaces
- relayed only enumerated loopback traffic through descriptors
- denied external writes with Landlock and fail-closed setup
```

---

## Task 7: Prove installed HTTP, SSE, and WebSocket traffic on Linux

**Files:**

- Create: `internal/proxy/codex_installed_linux_acceptance_test.go`
- Modify: `internal/proxy/codex_installed_http_client_exercise_test.go`
- Modify: `internal/proxy/codex_installed_task_affinity_live_test.go`
- Modify: `cmd/cq/proxy_http_validation_service_linux_test.go`

### Step 1: Add installed candidate end-to-end test

Use exact built CQ test executable, exact installed Codex executable proof,
isolated owned runtime, synthetic backend, and Linux confinement. Require:

- HTTP request and streamed SSE frames cross CQ;
- WebSocket prewarm and repeated messages cross CQ;
- backend receipts match requests;
- selector, continuity, headroom, quiescent lease, pong, and egress counters
  match existing installed gate;
- executable, process, cgroup, and listener proof revalidate afterward;
- readiness marker publishes only after every proof.

Run:

```bash
CQ_RUN_CODEX_INSTALLED_ACCEPTANCE=1 go test -race -count=1 -v ./internal/proxy ./cmd/cq -run '^TestLinuxInstalled.*(HTTP|SSE|WebSocket)'
```

Expected: FAIL until client exercises and candidate authority are fully wired.

### Step 2: Remove Darwin-only test gating where confinement exists

Replace `runtime.GOOS == "darwin"` checks with platform capability checks.
Keep current explicit environment opt-in and exact-client requirements.

### Step 3: Prove crash and cleanup behaviour

Terminate active worker. Require supervisor replacement, one listener owner,
no overlapping request authority, then run second HTTP and WebSocket exchange.
Cancel candidate and require all descendant PIDs, listeners, relay sockets,
and temporary files disappear.

### Step 4: Verify

Run:

```bash
CQ_RUN_CODEX_INSTALLED_ACCEPTANCE=1 go test -race -count=1 -v ./internal/proxy ./cmd/cq -run '^TestLinuxInstalled.*(HTTP|SSE|WebSocket|Recovery|Cleanup)'
```

Expected: PASS.

### Step 5: Commit

Commit subject:

```text
test: proved Linux installed traffic
```

Body:

```text
- exercised real client HTTP, streaming, and WebSocket transport
- proved worker recovery without duplicate authority
- required complete candidate and confinement cleanup
```

---

## Task 8: Add ephemeral native systemd-user proof

**Files:**

- Create: `scripts/test-linux-runtime.sh`
- Create: `scripts/test-linux-runtime_test.go`
- Modify: `.github/workflows/ci.yml`

### Step 1: Add failing harness contract test

Test script text and invoked helper modes. Require temporary HOME and
`XDG_CONFIG_HOME`, exact unit filename `cq-proxy.service`, exact absolute
`ExecStart=<binary> proxy start`, disposable user manager setup, cleanup trap,
and no install/restart/status/remove product command calls.

Run:

```bash
go test -race -count=1 ./scripts -run '^TestLinuxRuntimeHarnessContract$'
```

Expected: FAIL because harness does not exist.

### Step 2: Implement disposable proof harness

Script builds one release-like CQ binary, creates temporary roots, starts a
disposable user manager only when current runner lacks one, writes fixture unit
matching packaging contract, then proves:

1. service main PID is CQ supervisor;
2. ready worker owns default TCP4 loopback listener;
3. manager, cgroup, executable digest, process, and listener evidence agree;
4. HTTP/SSE and WebSocket test traffic crosses service;
5. worker termination recovers;
6. stop removes descendants and listeners;
7. trap removes unit, runtime roots, bus, namespaces, relays, sockets, markers,
   and temporary directories.

Use `mktemp -d`; never depend on persistent runner state.

### Step 3: Add standard Linux CI job

Add Ubuntu job running focused native tests and harness. Fail if namespace or
Landlock capability tests skip. Keep existing Ubuntu full suite unchanged.

### Step 4: Run native proof

On Linux:

```bash
bash scripts/test-linux-runtime.sh
```

Expected: PASS with cleanup audit printed at end.

### Step 5: Commit

Commit subject:

```text
ci: proved native Linux runtime
```

Body:

```text
- exercised owned runtime under an ephemeral user service
- verified kernel confinement and real proxy transports
- audited service, process, listener, and temporary-state cleanup
```

---

## Task 9: Full verification and integration handoff

**Files:**

- Modify only files needed to fix failures introduced by Tasks 1–8
- Review: approved design and packaging contract

### Step 1: Format and inspect scope

Run:

```bash
gofmt -w <changed-go-files>
git diff --check
git status --short
```

Confirm no installer, package-manager, product unit renderer, installation
documentation, or aggregate service transaction changed.

### Step 2: Run focused Linux tests

Run:

```bash
go test -race -count=1 ./internal/proxy ./cmd/cq -run 'Linux|Installed|RuntimeSupervisor'
```

Expected: PASS.

### Step 3: Run repository gates

Run:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Expected: PASS.

### Step 4: Cross-build and compile tests

Use a temporary directory created by `mktemp -d` and remove it afterward:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/proxy -o "$proof_dir/proxy-amd64.test"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./internal/proxy -o "$proof_dir/proxy-arm64.test"
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Expected: PASS; remove `proof_dir` afterward.

### Step 5: Run native proof and review evidence

Run native Linux harness. Verify actual client/backend receipts for HTTP, SSE,
and WebSocket; health or listener-only evidence cannot substitute.

### Step 6: Commit final fixes

If verification required code changes, commit with a scoped conventional
message following repository and app rules. Leave worktree clean.

### Step 7: Handoff packaging integration

Report branch and exact commit SHA to task
`019ff8ae-9d9d-7601-b843-8700c0eeba17`. Packaging integration must then prove
all three product units, install/restart/status/refresh/uninstall, exact
runtime evidence, real HTTP/SSE/WebSocket traffic, and cleanup before overall
Linux compatibility is complete.
