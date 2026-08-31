# Linux Runtime Compatibility Design

- **Status:** Approved
- **Date:** 2026-08-30
- **Scope:** Linux proxy runtime parity, inspection, attestation, installed
  transport validation, confinement, and native proof
- **Integration contract:** Packaging branch
  `jacobcxdev/feat/cross-platform-installer` at `839c3bb`

## Outcome

Linux runs CQ's full proxy supervisor and worker architecture under the current
user's systemd manager. Runtime status binds the configured service, exact CQ
executable, supervisor and worker processes, and TCP listener into one
fail-closed identity. Installed validation exercises HTTP, SSE, and WebSocket
traffic with the installed Codex client while preventing provider egress and
writes outside the validation root.

The packaging branch owns service installation. This branch owns runtime
behaviour and evidence. Neither branch duplicates the other's authority.

## Fixed packaging boundary

Packaging owns:

- `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/cq-proxy.service`;
- `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/cq-refresh.service`;
- `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/cq-refresh.timer`;
- service-manager preflight, inspection, installation, lifecycle, rollback,
  and removal;
- exact unit rendering with no shell indirection;
- installer, package-manager, and installation-documentation work.

Runtime consumes these fixed command forms:

```text
<absolute-cq> proxy start
<absolute-cq> refresh
```

Runtime does not write unit files, call product service lifecycle operations,
enable lingering, or infer package ownership from process names or alternative
paths. Packaging inspection is the sole source of systemd manager state.
Runtime inspection independently proves kernel process and listener facts.

## Platform structure

Linux behaviour is added through narrowly tagged files:

- `cmd/cq/proxy_linux.go` wires supervisor, worker, and Linux inspection
  dependencies;
- `cmd/cq/proxy_http_validation_service_linux.go` resolves and validates the
  Linux installed runtime without managing product units;
- `internal/proxy/codex_installed_process_attestation_linux.go` binds the
  running process, service identity, executable, and listener;
- `internal/proxy/candidate_confinement_linux.go` provides the Linux candidate
  capability and fail-closed launcher;
- additional `_linux.go` files isolate procfs, socket, namespace, and Landlock
  code where a focused file keeps testing simpler.

Existing non-Darwin stubs narrow to `!darwin && !linux`. Shared files change
only where a platform hook, systemd service kind, or portable helper is
required. Mature Darwin behaviour remains unchanged.

## Supervisor and worker parity

`cq proxy start` becomes the Linux owned-runtime entrypoint. systemd supervises
the CQ supervisor. CQ's existing runtime protocol supervises the worker.

The Linux launcher:

1. opens the lifecycle authority before spawning external code;
2. creates inherited listener, control, lifecycle, and readiness descriptors;
3. starts the exact current CQ executable in worker role;
4. closes parent and child descriptor copies deterministically;
5. waits for signed worker readiness before reporting ownership;
6. recovers worker exit, boot failure, and panic through existing owned-runtime
   policy;
7. drains and replaces workers without allowing predecessor and successor to
   own the same request authority concurrently;
8. exits when the service context ends so systemd can apply its configured
   restart policy.

Every goroutine that calls injected or external behaviour retains mandatory
panic recovery. Linux does not add a second supervisor or service loop.

## Linux runtime inspection

Runtime collectors read bounded kernel interfaces, not human-formatted command
output:

- `/proc/<pid>/stat` for parent PID and process start time;
- `/proc/<pid>/status` for effective UID;
- `/proc/<pid>/cmdline` for exact argument vectors;
- `/proc/<pid>/exe` plus `fstat` for mapped executable device and inode;
- `/proc/<pid>/cgroup` and the matching cgroup v2 directory for service
  membership;
- `/proc/net/tcp` and `/proc/net/tcp6` for listening socket inodes;
- `/proc/<pid>/fd` for socket ownership.

All reads have explicit size, field-count, PID-count, and traversal limits.
Parsers reject NUL ambiguity, duplicate facts, malformed numeric fields,
unsupported address families, and unexpected extra listeners.

Process identity is `(pid, start-time, uid, executable device, executable
inode)`, not PID alone. Listener identity adds socket inode and canonical
TCP4 loopback address. Service membership adds the expected user-service
cgroup path. Revalidation repeats all mutable facts before evidence is used.

## Installed process and service attestation

Linux extends installed service kinds with systemd-user ownership. A valid
proof requires exactly one match for `cq-proxy.service` and all of:

- packaging inspection reports the fixed unit active with one main PID and the
  exact `ExecStart` vector;
- runtime main PID equals the inspected supervisor PID;
- supervisor and worker use the expected effective UID;
- supervisor executable matches the fixed absolute CQ executable by path,
  device, inode, size, mode, link count, owner, and SHA-256 digest;
- `/proc/<pid>/exe` still maps that executable;
- command line is exactly `cq proxy start` for supervisor or the internal CQ
  worker role for worker;
- supervisor and worker belong to the inspected unit's cgroup;
- expected loopback listener socket belongs to the attested runtime process;
- configuration, executable, cgroup, process, and listener facts remain
  unchanged after capture.

Unit-file and executable reads use no-follow opens, owner checks, safe mode
checks, one-link requirements, bounded hashing, and before/after identity
checks. Missing, stale, replaced, deleted, permissive, near-match, or ambiguous
facts return the existing generic attestation error without leaking paths or
process details into readiness evidence.

Packaging may combine its manager proof with runtime's process proof. Runtime
does not create a competing `systemctl` adapter.

## Installed HTTP and SSE validation

Linux reuses the existing installed HTTP validation core and exact installed
Codex executable proof. The candidate runs the exact CQ executable under the
Linux owned supervisor on an isolated loopback port. It is not installed as a
fourth product unit.

Validation requires:

- exact CQ and Codex build identities;
- installed service and process attestation before capture;
- real Codex client requests through CQ's HTTP transport;
- streamed SSE responses through CQ and the synthetic upstream;
- routing, account selection, backend receipt, continuity, headroom, and
  quiescent-lease evidence already required by the current gate;
- listener and executable revalidation after traffic;
- durable readiness markers only after every gate passes.

Failure invalidates prior markers before returning. Candidate shutdown closes
listeners, workers, relays, temporary files, and namespace helpers even when a
test, client, or injected dependency panics.

## Installed WebSocket validation

Linux reuses the current WebSocket acceptance flow and installed Codex client
exercise. The exact installed client connects through the isolated CQ
candidate. Acceptance retains prewarm, persistent-account, upstream-dial,
generation-fence, and readiness-marker requirements.

Transport validation is data-plane proof. Health, unit state, PID presence, or
listener presence alone cannot satisfy HTTP, SSE, or WebSocket acceptance.

## Linux acceptance confinement

The installed Codex client runs beneath a CQ-owned helper created with:

- a new user namespace mapping only the invoking effective UID and GID;
- a new network namespace owned by that user namespace;
- a new mount namespace where needed for safe helper setup;
- `PR_SET_NO_NEW_PRIVS` before applying restrictions and executing the client;
- a Landlock filesystem ruleset permitting writes only beneath the unique
  validation root and to required null-device behaviour;
- closed inherited descriptors except explicitly enumerated control, relay,
  output, and executable descriptors.

User namespace creation precedes network and mount namespace creation. The
helper gains capabilities only inside its new namespaces and cannot change the
host network namespace.

The new network namespace contains loopback only and no host interface or
default route. The helper binds loopback relay listeners for the allowed proxy
endpoint and egress guard. Accepted connections pass to the trusted parent over
an inherited Unix socket with `SCM_RIGHTS`. The parent forwards only an
enumerated relay identity to its already-open validation endpoint. Unknown
relay identities, extra descriptors, non-loopback addresses, and unexpected
ports are denied.

Direct provider traffic has no route. Proxy-environment attempts reach the
deny-only egress guard and are counted. External filesystem writes fail under
Landlock. The helper reports capability setup through a bounded, versioned
control message before the client starts.

Landlock negotiates the running kernel ABI and requests every supported write
right. Base write restriction is mandatory. When the ABI lacks standalone
truncate control, the helper closes external writable descriptors and denies
standalone truncate syscalls before execution. Any missing namespace,
`no_new_privs`, Landlock, descriptor-passing, or loopback setup capability
fails closed as `Codex acceptance ... confinement unavailable`.

No best-effort mode, environment-only proxy sandbox, external sandbox binary,
root requirement, or systemd IP firewall fallback is permitted.

## Candidate confinement

`ValidateCandidateConfinement` continues rejecting credentials, provider
origins, authority keys, direct networking, external paths, executable
overrides, and extra inherited descriptors. Linux reports platform confinement
available only when the namespace helper can establish and prove the complete
policy above.

Validation of a launch specification is not enforcement. Candidate launch
must use the confined helper, receive only one controller IPC descriptor, and
publish a ready message only after namespace and Landlock setup succeeds.

## Error behaviour

- Unsupported or disabled user namespaces: fail closed, no readiness marker.
- Missing Landlock or insufficient rights: fail closed, no weaker fallback.
- Procfs race, PID reuse, cgroup mismatch, or executable replacement: generic
  installed-process attestation failure.
- Socket ambiguity or listener ownership mismatch: candidate/runtime authority
  failure.
- Relay protocol violation or unexpected egress: acceptance failure and helper
  termination.
- Context cancellation: terminate descendants, close relays, invalidate
  partial evidence, and return the existing stage-specific cancellation error.
- Cleanup failure joins the primary error and prevents a passing result.

Errors remain privacy-safe. Raw `/proc` content, home paths, command
environments, credentials, bearer tokens, readiness keys, and synthetic auth
do not enter diagnostics.

## Deterministic verification

Every Go test uses `-race -count=1`.

Focused tests cover:

- Linux supervisor boot, readiness, handoff, crash, restart, cancellation, and
  descriptor cleanup;
- procfs parsers with truncation, duplicate, malformed, oversized, PID-reuse,
  cgroup, executable, and socket near-matches;
- exact systemd inspection-to-runtime proof composition without product unit
  mutation;
- HTTP and SSE traffic through the installed candidate;
- WebSocket prewarm and repeated traffic through the installed candidate;
- user/network namespace creation and loopback-only relay behaviour;
- allowed validation-root writes and denied external writes;
- denied direct egress, denied unknown relays, and counted proxy egress;
- disabled user namespaces, missing Landlock, partial setup, client crash,
  timeout, and cleanup failure;
- Linux amd64 and arm64 compilation of every package and test binary.

Fakes inject filesystem, procfs, namespace setup, relay, executable capture,
clock, process, and service-inspection facts. Native tests separately prove
kernel behaviour.

## Native Linux proof

CI uses an ephemeral standard Linux job, not a persistent or bespoke runner.
It builds one release-like CQ binary, creates temporary HOME/XDG roots, and
starts a disposable systemd user manager when the hosted session lacks one.
Test fixtures use the packaging contract's exact unit names and `ExecStart`
forms without adding product unit-rendering code to this branch.

Runtime acceptance proves:

1. `cq-proxy.service` starts the exact binary as CQ supervisor;
2. one worker becomes ready and owns the expected TCP4 loopback listener;
3. manager inspection, procfs facts, cgroup membership, executable digest, and
   listener ownership agree;
4. real HTTP and streamed SSE traffic cross the service transport;
5. real WebSocket traffic crosses CQ with prewarm and repeated messages;
6. installed Codex acceptance cannot reach a direct external address or write
   outside its isolated root;
7. worker termination causes CQ supervisor recovery without duplicate
   listener or request authority;
8. stopping the service removes all CQ descendants and listeners;
9. temporary units, roots, namespaces, relays, sockets, and markers are
   removed.

After integration with packaging, the combined gate additionally proves
install, restart, status, refresh service/timer execution, uninstall, rollback,
and cleanup through the packaging-owned adapter.

## Completion criteria

Linux runtime compatibility is complete only when:

- Linux uses owned supervisor/worker runtime instead of monolithic fallback;
- status and validation expose exact Linux process, cgroup, executable, and
  listener evidence;
- installed HTTP, SSE, and WebSocket gates run on Linux with exact installed
  client and CQ identities;
- acceptance confinement denies direct egress and external writes at kernel
  boundaries;
- focused race tests, full race suite, vet, native builds, amd64/arm64 test
  compilation, and disposable systemd-user proof pass;
- integration with packaging proves all three units, refresh execution, and
  uninstall cleanup;
- no shared installer, package-manager, or installation-documentation files
  are changed by this branch.
