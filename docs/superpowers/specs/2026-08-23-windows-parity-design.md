# Windows 11 Parity Design

**Status:** approved design

**Date:** 2026-08-23

**Authority baseline:** `af08695590ad0a86d1f257d3cf3980c31cb506ec`

**Targets:** Windows 11 on `windows/amd64` and `windows/arm64`

## Purpose

CQ must work as a native Windows 11 command-line application. Users must not
need WSL, Cygwin, MSYS2, Bash, or a PowerShell wrapper. Windows must support
quota checks, account management, model management, foreground and scheduled
proxy operation, credential coordination, candidate validation, installed HTTP
validation, JSON output, and terminal output.

Parity means preserving CQ's current user-visible behavior and security
boundaries. It does not mean translating Unix syscalls literally. Windows
adapters use Windows identities, handles, access control lists, named pipes,
job objects, Task Scheduler, AppContainer, and Windows Filtering Platform
(WFP) while keeping existing package ownership and platform-neutral policy.

## Current baseline

Current source has partial packaging support, not Windows parity:

- `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...` succeeds.
- GoReleaser emits a Windows x64 ZIP but excludes Windows ARM64.
- CI cross-builds Windows from Linux but runs no native Windows job.
- Windows test compilation fails in `internal/proxy/proxy_cu_manifest_test.go`
  because release manifest definitions are excluded by current build tags.
- Secure directory handles, durable directory handles, full file identity, and
  exclusive lifetime locks return unavailable on Windows.
- Compatibility-epoch initialization therefore prevents ordinary commands
  from starting on Windows.
- Codex credential control, proxy runtime roles, candidate confinement,
  installed process attestation, background refresh installation, proxy task
  installation, restart, and installed HTTP validation use platform stubs.
- Several config and state paths still use Unix-style home-directory paths.

Cross-compilation remains useful. It cannot prove Windows ACL, reparse-point,
console, Task Scheduler, named-pipe, job-object, AppContainer, WFP, or process
identity behavior.

## Decisions

### Product contract

- Supported operating system is Windows 11.
- Supported architectures are x64 and ARM64.
- Windows archives contain native executables. No emulation is part of the
  support contract.
- Existing Unix behavior, paths, schemas, build tags, and service integration
  remain unchanged unless a shared bug is exposed by platform-neutral tests.
- All current user-facing command leaves receive native Windows behavior. The
  only deliberate not-applicable operations are inspection and transition of a
  legacy Unix-domain credential socket, because that artifact cannot exist in
  the Windows named-pipe namespace. Their Windows result must name that exact
  reason; generic "macOS only" or "platform unsupported" errors are forbidden.
- WSL remains a separate Linux environment and is not used as Windows proof.

### Implementation boundary

Windows support adds small build-tagged adapters behind existing interfaces.
Platform-neutral policy remains in current packages:

- `internal/userdirs` resolves CQ config, state, cache, runtime, and log roots.
- `internal/fsutil` owns handle-safe filesystem operations, SID and ACL checks,
  full object identity, durable replacement, and locks.
- `internal/provider/codex` owns named-pipe credential coordination and exact
  peer authorization.
- `internal/proxy` owns job-backed runtime roles, executable and listener
  identity, AppContainer candidates, and Windows installed-process evidence.
- `cmd/cq` owns Task Scheduler commands, platform help, explicit legacy import,
  service discovery, and installed-validation orchestration.
- A small Windows-only WFP helper owns the elevated filter transaction. It does
  not parse CQ config, read credentials, contact providers, or run as a service.

Windows API calls use `golang.org/x/sys/windows` where it exposes required
contracts. Narrow Windows-only packages declare missing Windows SDK ABI types,
constants, and lazy-system calls for AppContainer, Network Isolation, and WFP.
Those declarations have native x64 and ARM64 size, alignment, field-offset,
and call-boundary tests. Named-pipe and job-object operation must not add
`go-winio`, COM automation, PowerShell, `cmd.exe`, WMI, or a general Windows
abstraction layer. Task registration uses `schtasks.exe` with strict XML
because Task Scheduler is an explicit product boundary.

## Paths and legacy import

### Native paths

One resolver supplies all default paths. On Windows its production adapter
reads exact current-user `User Shell Folders` registry values `AppData` and
`Local AppData`, accepts only `REG_SZ` or `REG_EXPAND_SZ`, and expands the latter
with token-bound `ExpandEnvironmentStringsForUserW`. It separately verifies the
real token profile through `GetUserProfileDirectoryW`, then requires clean
absolute local results while preserving legitimate AppData redirection.
`SHGetKnownFolderPath` is not authority because it expands through mutable
process environment in this context; default-path flags would instead discard
legitimate redirection. Injected `os.UserConfigDir`/`os.UserCacheDir`-shaped
functions remain test seams only; production authority never comes from mutable
`APPDATA`, `LOCALAPPDATA`, `USERPROFILE`, `HOME`, or current-directory
environment values:

| Data | Windows path |
|---|---|
| User configuration and model overlays | `%APPDATA%\cq` |
| Durable mutable state | `%LOCALAPPDATA%\cq\state` |
| Quota and model cache | `%LOCALAPPDATA%\cq\cache` |
| Runtime records and validation journals | `%LOCALAPPDATA%\cq\runtime` |
| Logs | `%LOCALAPPDATA%\cq\logs` |

The proxy config remains configuration even though it contains a local bearer.
It therefore resides under `%APPDATA%\cq` and receives secure-file treatment.
Credentials, owner records, compatibility epoch, operation journals, readiness
evidence, and lease state reside under `%LOCALAPPDATA%\cq\state`.

Windows does not fall back to `%TEMP%`, the current directory, or a Unix-style
home path when either application-data root is unavailable. Commands that need
the missing root fail before mutation. Read-only help and version output remain
available. Unix continues to honor current XDG and home-directory rules.

Tests inject path roots. Production code must not infer Windows behavior by
changing `HOME`, `APPDATA`, or `LOCALAPPDATA`, and it must not scatter known-
folder lookups across packages. Secure traversal retains and validates the
exact known-folder anchor handles before descending into CQ-owned roots.

### Explicit non-destructive import

CQ never scans or migrates old locations during startup. A Windows-only command
performs explicit import:

```text
cq windows import-legacy [--config PATH] [--local PATH]
```

At least one absolute source is required. `--config` identifies one old CQ
configuration directory. `--local` identifies one old CQ state/cache directory.
CQ opens only recognized relative entries beneath those roots. It does not
search the user profile or follow links or reparse points.

Import has these invariants:

1. Inspect source and destination through secure retained handles.
2. Reject unsafe ACLs, reparse points, non-regular files, unknown authority
   schemas, or source changes during the operation.
3. Copy byte-for-byte into a new temporary file, sync it, re-read and compare
   digest and size, then publish only when the destination is absent.
4. Never overwrite, merge, rename, move, truncate, or delete source data.
5. Never replace an existing destination, even when bytes match.
6. Record a privacy-safe journal and final receipt under the new state root.
   Receipts contain relative names, size, digest, and full object identities;
   they contain no token, account identifier, or raw source path.
7. Resume an interrupted copy from the journal. Re-running a completed import
   reports existing destinations without changing them.

Unix endpoint files and task/service metadata are not importable data. Users
must explicitly install Windows tasks after import.

## Secure Windows filesystem

Windows state mutation must preserve the intent of CQ's Unix secure-directory,
no-follow, identity, exclusive-lock, atomic-write, and durability contracts.
Plain `os.Stat`, `os.Open`, mode-bit checks, or path re-resolution are not
acceptable substitutes.

### Identity model

`SecureFileIdentity` gains a platform-neutral representation that can retain a
Windows volume serial number and the complete 128-bit `FILE_ID_INFO.FileId`.
Windows must not hash, fold, truncate, or place that identifier in the existing
64-bit inode field. Comparisons use the complete identity and link count.

Windows authority schemas that persist object identity encode volume serial as
an unsigned 64-bit value and file ID as exactly 32 lowercase hexadecimal
characters. Existing Unix `device`, `inode`, and link-count fields keep their
current meanings and bytes. Any durable shared schema that cannot represent the
Windows identity receives a versioned Windows-capable form; old readers fail
closed rather than accepting a partial identity.

Process identity is the current access-token user SID, retained as canonical
binary SID bytes. No UID placeholder or SID-to-integer conversion is allowed.
User-facing diagnostics may render the SID string but durable authority
comparisons use canonical bytes.

### Secure path contract

Secure paths are local NTFS paths. Remote, device, and unsupported filesystems
fail capability checks. Walk starts at retained volume-root handle. Every path
component is opened relative to retained parent with reparse processing
disabled. Volume root through known-folder anchor receives identity, type,
filesystem, and reparse checks. Authority ACL policy begins at selected
known-folder or provider anchor and applies to every descendant. Every ancestor
and final entry is rejected when it has `FILE_ATTRIBUTE_REPARSE_POINT`;
junctions, symbolic links, mount points, cloud placeholders, and unknown
reparse tags are all unsafe for authority state.

Windows has two ACL boundaries. Existing profile anchors such as `%APPDATA%`,
`%LOCALAPPDATA%`, provider-owned credential roots, and explicit legacy-import
sources remain externally owned. CQ never repairs them. It requires local NTFS,
stable retained-handle identity, no reparse component, a trusted owner, and no
write, delete-child, ownership, or DACL authority outside current user,
`LOCAL SYSTEM`, and `BUILTIN\Administrators`. Normal inherited read/traverse
entries on ancestors do not grant CQ-state authority and do not by themselves
invalidate an anchor.

CQ-owned roots and every descendant are checked from their handles for:

- expected file type;
- owner equal to current user SID;
- protected DACL with inheritance disabled;
- exactly three explicit allow ACEs: current user, `LOCAL SYSTEM`, and
  `BUILTIN\Administrators`, each with `FILE_ALL_ACCESS`; newly created
  descriptors use that order, while validation accepts normalized order;
- no inherited, deny, object, callback, conditional, unknown, or third-party
  ACE, including `Everyone`, `Authenticated Users`, `Users`,
  or `CREATOR OWNER`;
- unchanged volume serial, complete 128-bit file ID, and link count before and
  after each authority-changing step.

New directories and files are created with an explicit security descriptor,
not created broadly and repaired later. Existing broader objects are refused;
ordinary commands do not silently rewrite their ACLs. Any future repair command
requires a separate design and explicit user action.

AppContainer-accessible validation objects are one narrow exception, never a
relaxation of generic CQ state. A separate retained ephemeral-capability
directory/file type validates exactly the baseline user, `LOCAL SYSTEM`, and
`BUILTIN\Administrators` ACEs plus one expected package-SID ACE with exact
role-specific rights. The capability root and program directory receive only
the package traverse rights, and the program file receives only the package
read/execute rights; none of those three roles receives an explicit low label.
The candidate data directory receives only its package read/write/create-child
rights plus a protected inheritable low mandatory label. The AppContainer
controller pipe receives only its package read/write rights plus a protected
non-inherited low mandatory label. Each label contains one
`SYSTEM_MANDATORY_LABEL_ACE` whose only policy bit is
`SYSTEM_MANDATORY_LABEL_NO_WRITE_UP`; the data-directory label additionally has
`OBJECT_INHERIT_ACE|CONTAINER_INHERIT_ACE`, so created descendants inherit it.
The strict CQ-owned parent, generic CQ state, and ordinary named pipes receive
neither a package ACE nor an explicit low label. Both package-write surfaces
are created with their exact low label rather than created broadly and
repaired. Before adding a package ACE or making any later descriptor change,
the cleanup journal records retained object identity, complete prior owner/DACL
descriptor, and separate label descriptor.
Live validation reads owner/DACL and `LABEL_SECURITY_INFORMATION` separately
from the same retained handle, using `SE_KERNEL_OBJECT` for the pipe and
`SE_FILE_OBJECT` for data objects, and binds exact label bytes and policy into
pre-resume, health, recovery, and cleanup checks. Cleanup restores and
revalidates both descriptor classes or deletes the isolated object before
generic secure-path validation. Generic `SecureDirectory` must never accept a
package ACE or explicit low mandatory label.

### Atomic writes and locks

Windows secure directories implement current `SecureDirectory` and
`DurableDirectory` interfaces with retained handles. Temporary files use
cryptographically random names and exclusive creation in the retained parent.
CQ writes all bytes, calls `FlushFileBuffers`, validates descriptor identity,
closes when required by the replacement API, revalidates the named entry, and
performs same-directory atomic replace or no-replace. It then reopens the
installed entry, verifies complete identity and content, and flushes the parent
directory handle where Windows supports it.

Failures preserve current outcomes:

- before namespace publication: `not_committed`;
- after publication when installed identity or durability cannot be proved:
  `indeterminate`;
- proved final identity and durability: committed.

Windows locks use `LockFileEx` with exclusive, nonblocking semantics. The handle
remains open for lock lifetime. Existing unlocked entries are not accepted by
new-lock APIs. Probe operations never retain ownership. Process exit releases
the lock, but durable recovery still requires current journals and identities.

### Credential bytes

Windows uses the same existing JSON credential and authority schemas. CQ does
not add DPAPI envelopes, alternate token formats, registry storage, or a second
Windows-only credential database. Secure-file ACL and handle rules protect JSON
state. Existing Claude `go-keyring` entries may continue to use Windows
Credential Manager; that backend does not change CQ's JSON formats or Codex's
read-only treatment of system credentials.

No error, log, import receipt, task XML, named-pipe sidecar, or WFP request may
contain access tokens, refresh tokens, authorization codes, local bearer values,
or full credential JSON.

## Credential and runtime IPC

### Named-pipe credential owner

Windows replaces the Unix-domain credential socket with one local named pipe
per credential-store owner. Its name is the SHA-256 digest of the exact domain
bytes `cq-credential-control-v1\x00` followed by canonical binary current
`TokenUser` SID, not
an email, account, logon SID, protocol version, or caller-controlled value.
Protocol version remains inside the handshake and durable sidecar. Old and new
binaries and simultaneous logon sessions therefore contend on the same first-
instance endpoint instead of creating split owners over one user store.
Every server instance uses `PIPE_ACCESS_DUPLEX|FILE_FLAG_OVERLAPPED`; initial
instance alone also uses `FILE_FLAG_FIRST_PIPE_INSTANCE`. Client `CreateFileW`
uses `GENERIC_READ|GENERIC_WRITE`, share zero, `OPEN_EXISTING`, and
`FILE_FLAG_OVERLAPPED`, so deadlines and `CancelIoEx` never operate on a
synchronous handle. Every non-null `SECURITY_ATTRIBUTES` has `Length` equal to
its native 24-byte struct size on x64/ARM64, descriptor pointer, and
`InheritHandle=0`, with both objects live through the API call. Creation uses
byte/message limits, bounded waits, and a protected descriptor with exactly
current user SID and `LOCAL SYSTEM` full-access ACEs. Network access is denied
explicitly.

Default named-pipe ACLs are forbidden because Windows defaults can grant read
access to `Everyone` and anonymous users. On every accepted connection, the
server impersonates the client long enough to compare user SID, logon SID,
session ID, and client process identity, then immediately reverts. A mismatched
or unverifiable peer receives no protocol bytes.

Impersonation is OS-thread scoped. Server locks its goroutine to the current OS
thread before `ImpersonateNamedPipeClient`, remains locked through thread-token
read/copy/close and checked `RevertToSelf`, and unlocks only after successful
revert. Revert failure poisons and terminates the credential owner rather than
returning an impersonated thread to Go scheduling.

Existing `net/rpc` gob framing, request validation, exact-generation resolution,
owner/delegate selection, refresh eligibility, panic recovery, and automatic
read-only system credential rules remain unchanged. This gate does not claim an
application-frame or gob-allocation bound. Pipe transport adds I/O deadlines,
64 KiB kernel buffers, and a bounded concurrent-instance count; those transport
bounds are not a codec bound. Named pipes leave no stale socket entry. Crash
recovery proves lock, owner generation, pipe peer, and durable sidecar state
before replacing an owner.

Legacy Unix endpoint inspection and transition return the precise
not-applicable result described above and create no Windows pipe or state.

### Runtime roles and process lifetime

Windows proxy supervisor, worker, candidate, and helper channels use direct
named-pipe handles with the same per-channel authentication and bounded framing
as Unix runtime control. Handle inheritance uses an explicit allowlist in
`STARTUPINFOEX`; unrelated handles are never inherited.

Each process tree receives a job object created with an explicit current-user
security descriptor and `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Breakaway is not
allowed. Child is created suspended, assigned to job, and verified as member
before its first instruction; it resumes only after required confinement is
proved. Starter retains the only kill-on-close job handle through resume. Two
sealed lifetime modes exist. A trusted CQ controller that must outlive starter
uses one authenticated, replay-protected, deadline-bounded ownership sequence.
After authenticated ready, but before any job-handle duplicate, the starter
transfers one Foundation-owned opaque strict-state-directory capability with
fixed least rights. The controller adopts it exactly once, derives two
domain-separated store keys from the runtime control token, authenticates the
fixed applied-journal and handoff stores, reopens the journal-bound root, and
returns the sealed state-capability digest. Raw paths, token bytes, derived keys,
transfer grants, and target handle values never enter argv or durable records.
Only after constant-time verification of that digest may the starter duplicate
the job handle non-inheritable into the controller. The controller queries exact
job limits/accounting, proves its own retained process is assigned, and returns
the typed job proof. The starter durably commits and reads back the authenticated
handoff record, sends sealed committed, and requires the controller to load the
same record through its adopted store and acknowledge it. Only that
acknowledgement permits the typed ownership commit and starter job-handle release.
Every pre-commit outcome is `(zero ownership result, non-nil primary error)` and
must abort/terminate/wait; every accepted outcome is
`(OwnershipCommitted=true, nil primary error)`, after which abort is forbidden
and any starter-copy close debt is reported separately. EOF, timeout, bad frame,
or crash before acknowledgement makes the controller close provisional handles
and exit; the starter retains kill authority until then. Workers, AppContainer
clients, and external installed Codex use parent-retained authority: controller
keeps the sole job handle for complete child lifetime and never grants it to the
child. Controller crash then closes the last handle and kills the job.
Windows jobs have no durable intrinsic identity. CQ records controller nonce and
policy digest, retains authoritative job handle, and records root PID, process
creation time, executable path, executable digest, and full executable file
identity. Trusted CQ AppContainer controller child receives a
query/synchronise-only duplicate of parent process handle over its inherited
one-way bootstrap, because PID-only `OpenProcess` is not assumed to pass
AppContainer dual-principal access checks. External Codex receives no CQ
bootstrap handle.
PID alone is never authority. Closing validation or worker job terminates its
complete child tree; completion waits until job reports zero active processes.

Foreground proxy start retains current HTTP and WebSocket behavior, loopback
binding, graceful shutdown, and error classification. Windows console control
events trigger the same bounded shutdown. Runtime code uses `NUL` handles when
discarded standard streams are required; it never uses `/dev/null` or `/tmp`.

## Per-user Task Scheduler integration

### Task model

Windows uses two installed per-user Task Scheduler tasks. Their exact root
names derive from the canonical full caller user SID so simultaneous Windows
users do not collide:

- `\cq-quota-refresh-<caller-user-SID>` for periodic `cq.exe refresh`;
- `\cq-proxy-<caller-user-SID>` for long-running
  `cq.exe proxy start --cq-internal-windows-task-role=persistent-proxy`.

Installed validation may create one temporary candidate name by appending
`-candidate-<32-lowercase-hex-session-id>` to the exact proxy name. Its action
is exact `cq.exe proxy start --port <port> --cq-internal-windows-task-role=candidate --cq-internal-windows-validation-session=<session-id>`. It follows the same
receipt rules and is absent after every completed validation run.

Only explicit `cq agent install` and `cq proxy install` create persistent tasks.
Caller must hold a primary, non-service interactive token with nonzero session
and unique logon SID, `TokenElevation=false`, elevation type `Default` or
`Limited`, integrity exactly medium, `IsTokenRestricted=false`, and
`TokenIsAppContainer=false`. A normal filtered administrator with a linked
elevated token remains valid when its current primary token meets those exact
conditions; low/untrusted/high/system integrity, restricted or AppContainer,
current elevated/full, `SYSTEM`, service, impersonation, session-zero, or
ambiguous callers are refused before task query or durable intent. Installation binds caller token user SID, logon SID, and
session. Ordinary checks, account reads, help, version, status, or startup never
register tasks. Both tasks run as that exact user with `InteractiveToken` logon
and `LeastPrivilege` run level. No password is stored. Executable path is
absolute, arguments use Windows command-line quoting, and working directory is
explicit.

Task XML is generated from fixed structs, serialized with XML escaping, parsed
back through a strict schema projection, and compared before registration.
Unknown actions, triggers, principals, settings, namespaces, or extra XML are
rejected. Registration writes XML to one secure temporary file and invokes
`schtasks.exe` directly with argument arrays. CQ never invokes a shell.

The proxy task uses a logon trigger and these fixed settings:

- `MultipleInstancesPolicy=IgnoreNew`;
- `StartWhenAvailable=true`;
- no battery-triggered stop;
- no execution time limit;
- restart interval `PT1M`;
- restart count exactly `255`.

Hidden role/session arguments route Windows startup but are not authority.
Persistent and candidate paths first rederive their caller-SID-qualified task
name and revalidate exact live task receipt, role, port, and durable
attempt/journal tuple. Missing or mismatched state refuses before config,
listener, or network access. Ordinary foreground `proxy start [--port]` has no
internal role and never consumes scheduled attempt or candidate state. A
same-user process reproducing the exact valid arguments and live durable tuple
cannot be distinguished from Task Scheduler and remains inside the documented
same-user threat/race boundary; arguments alone never prove readiness.

Each validated scheduled persistent proxy action atomically records its
task-receipt-bound attempt generation and count before config, listener, or
network startup. Count never exceeds 255 and resets only after complete live
readiness proof or an explicit install/restart creates a new generation. When
count is 255 with no live attested process, human and JSON status report
`recovery_exhausted`; another automatic action refuses startup until explicit
restart mints a generation.

The refresh task uses the existing 30-minute interval and a bounded execution
time. It never starts the proxy. Before create or replacement, install writes a
durable mutation intent binding prior live observation and complete desired
authority. After mutation it queries live XML/descriptor and writes a secure
receipt binding exact task name, normalised complete XML digest, caller
user/logon/session identity, principal SID, and task security descriptor. Crash
recovery accepts only exact complete live readback matching intent. Install is
idempotent only when queried XML and descriptor match receipt and expected
normalised form. Drift causes clear refusal. Explicit `--replace-drifted` is
destructive consent: CQ displays full definition diff and replaces only exact
task name and caller principal after a second equal observation. Marker alone
never proves ownership. Normal and JSON output describe every mismatch.

### Query, restart, and removal

CQ uses these direct operations:

- create when absent: `schtasks.exe /Create /TN ... /XML ...` with closed input;
- explicitly consented replacement: `schtasks.exe /Create /TN ... /XML ... /F`;
- obtain authority: `schtasks.exe /Query /TN ... /XML /HRESULT`;
- request start: `schtasks.exe /Run /TN ...`;
- request stop: `schtasks.exe /End /TN ...`;
- remove: `schtasks.exe /Delete /TN ... /F`.

Only HRESULT-form exit status and strict XML are authority. CQ does not parse
localized human-readable columns or generic exit code `1`. Exact-name absence
is accepted only for a CQ-generated root task name, empty XML stdout, and
native-verified `HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND)` or
`HRESULT_FROM_WIN32(ERROR_PATH_NOT_FOUND)` (`0x80070002` or `0x80070003`). A
live Windows 11 ARM64 build 26100 probe returned `0x80070003`. Access denial,
service unavailability, `SCHED_E_CANNOT_OPEN_TASK`, malformed-task errors, a
missing non-root task folder, and every other status fail closed for explicit
compatibility review rather than widening this allowlist. An absent-query/create race cannot overwrite a newly
created task because absent create omits `/F`. Task Scheduler has no compare-
and-swap between final query and consented `/F`; CQ serialises its own writers,
records intent, and documents the remaining uncooperative same-user race rather
than claiming impossible atomic replacement. Uninstall deletes only exact task
after complete live XML, security descriptor, name, and principal match secure
install receipt. Foreign, unreceipted, or drifted tasks are refused, not
deleted.

`/End` nonzero status is never classified from localized text. A caller may
treat stop as already achieved only after the task still matches its exact
receipt and independent retained process-generation plus TCP-listener evidence
proves the bound action absent. Otherwise stop fails closed.

`IRunningTask::EnginePID` identifies the Task Scheduler engine process, not the
launched `cq.exe` action. It is never used as CQ process identity. Scheduled CQ
writes a secure runtime record containing PID, process creation time, executable
digest and full file identity, task XML digest, start nonce, port, and readiness
generation. Status correlates that record with a live process and listener.

Restart ends the exact task, waits for the recorded process and listener to
disappear, requests a new run, and waits for a new readiness generation. Timeout
or ambiguous state returns failure; CQ never kills a process selected only by
name or PID.

## Candidate confinement and installed validation

### AppContainer candidate

Each candidate run creates a unique AppContainer profile and isolated root.
The profile receives no internet, private-network, enterprise-authentication,
user-data, or broad filesystem capability. Its SID receives read/execute access
only to verified candidate binaries and read/write access only to the isolated
data directory. Package DACL access is not sufficient for the low-integrity
candidate: the controller pipe carries the exact non-inherited protected low
mandatory label, while the data directory and its descendants carry the exact
protected inheritable low mandatory label defined above. Label-only retained
handle readback is mandatory before resume and throughout health, recovery,
and cleanup. APPDATA, LOCALAPPDATA, USERPROFILE, temp, config, cache, and state
variables all point inside that data directory. No production CQ credential or
state directory is mounted or copied.

Candidate process and descendants run in a kill-on-close job object. Launch
uses explicit handles, environment, executable digest, and architecture. The
controller retains all signing and validation authority outside the candidate
job. Candidate output remains bounded and untrusted.

Before resume and on every retained-handle recheck, live token inspection
requires `TokenIsAppContainer == 1`, a well-formed `TokenCapabilities` group
count of zero, `TokenAppContainerSid` equal to the exact run package SID, and
the expected low AppContainer integrity. These values participate in process
identity digest/equality. A zero-capability `SECURITY_CAPABILITIES` launch
request is intent, not live attestation.

"Zero-network candidate" means candidate AppContainer and every process in its
job can reach no provider, internet, LAN, DNS, arbitrary loopback address, or
other local port. During installed validation its only permitted network flow
is TCP to the exact loopback address family and candidate port named by the
approved validation request. Negative tests prove all other IPv4 and IPv6
connects and listens fail.

Windows may require an AppContainer loopback exemption for that one connection.
The exemption is only a connectivity prerequisite; it is not a security
boundary. Network Isolation API replaces the whole exemption
`SID_AND_ATTRIBUTES` list; it offers no atomic add/remove. Every elevated CQ
helper across all users and sessions calls `CreateMutexExW` for exact versioned
mutex `Global\cq-validation-loopback-v1` with `dwFlags=0`, never initial owner,
requesting only
`SYNCHRONIZE|MUTEX_MODIFY_STATE|READ_CONTROL`. Protected DACL grants exactly
those three rights to `LOCAL SYSTEM` and `BUILTIN\Administrators` only; this lets
either trusted helper wait/release and inspect live owner/DACL without granting
`MUTEX_ALL_ACCESS`. Its complete `SECURITY_ATTRIBUTES` has native size,
descriptor, and non-inherit fields. Pinned x/sys may return a valid nonzero
handle together with `ERROR_ALREADY_EXISTS`; that exact pair is successful
existing-object acquisition and the handle must still be inspected and closed.
Creation supplies trusted owner and exact DACL, then helper calls
`GetSecurityInfo` with `SE_KERNEL_OBJECT` and only owner/DACL information and
reads back owner, protected DACL, ACE masks, and inheritance before waiting. An
existing object is accepted only with owner `LOCAL SYSTEM` or
`BUILTIN\Administrators` and exactly those two non-inherited allow ACEs with
that exact three-right mask; a pre-created user-owned, broad, inherited, or
otherwise changed object is left untouched and validation refuses. Under that
proved mutex, bounded wait accepts `WAIT_OBJECT_0`; it also accepts
`WAIT_ABANDONED_0` as acquired crash evidence but then trusts no prior list
snapshot and starts from a fresh whole-list read. Timeout or wait failure causes
zero list mutation. Helper calls checked `ReleaseMutex` exactly once if and only
if either acquired result occurred, then closes the handle. Under that lock it
reads newest complete tuples, appends only the
unique run SID with zero attributes, writes, rereads, and verifies. Cleanup
reads the newest list and removes only that run tuple; it never restores a stale
snapshot. CQ preserves all foreign tuples present in its immediately preceding
read and detects only observable reread drift. A writer that ignores CQ's
machine-wide lock
can race between Get and Set and be invisibly overwritten, so transactional
preservation is not claimed. This remains an explicit platform limitation.
Helper installs exemption only after
receipt-bound persistent exact-port filters are active and removes it after
candidate job drains but before filters. Validation fails if order or cleanup
cannot be proved. Durable cleanup journal lets next explicit validation command
remove exact leftovers before another run. Ordinary startup grants nothing.

### Narrow elevated WFP helper

WFP policy changes require elevation. Windows archives therefore include a
small `cq-wfp-helper.exe` built from a separate command package. `cq.exe` starts
it through Windows `runas` elevation verb only for installed validation. Both
split-token filtered-administrator elevation and standard-user
over-the-shoulder alternate-administrator elevation are supported; controller
always remains ordinary interactive and non-elevated.
User cancellation returns a specific validation refusal and makes no change.
The helper never persists, installs a service, registers a task, opens CQ state,
or receives credentials.

Before elevation, controller durably journals complete immutable intent for the
run, random persistent WFP provider/sublayer/filter keys, and every expected
object property, condition, display datum, flag, and policy digest. It then creates random
current-session named pipe and one-use request containing protocol version,
validation-run ID, nonce, AppContainer SID, current user SID, exact candidate
application identity, exact loopback address family, TCP protocol, port, WFP
keys, and deadline. Helper connects only to that pipe, authenticates exact
unelevated controller process plus anchored helper process returned by
`ShellExecuteExW`, and rejects replay, broad addresses, port zero, live port
`19280`, expired requests, or unknown fields. Split-token helper must prove its
linked limited token equals controller user/logon/session. Over-the-shoulder
helper may have different administrator identity, but independently derives
controller user/logon/session from live process and never substitutes helper
identity into request authority or `ALE_USER_ID`.

Helper opens WFP session and installs persistent, run-unique provider, sublayer,
and seven filters in one transaction. Every filter binds exact
`ALE_PACKAGE_ID`, `ALE_USER_ID`, and `ALE_APP_ID`. Only the higher-weight
`ALE_AUTH_CONNECT_V4` permit additionally binds TCP, `127.0.0.1/32`, and exact
remote port. Lower-weight package-wide `ALE_AUTH_CONNECT_V4/V6` filters block
all other connects; package-wide `ALE_AUTH_LISTEN_V4/V6` filters block listens;
resource-assignment V4/V6 filters block non-TCP assignment. They do not claim a
nonexistent blanket TCP bind condition. Job membership, digest, file ID, PID,
creation time, logon SID, and session are controller attestation facts, not WFP
conditions. Filter precedence and effective behavior are acceptance facts, not
inferred from successful API calls.

Helper reports installed keys, returned IDs, and exact conditions. Controller
persists receipt before candidate resumes. If helper crashes after transaction
commit but before report, recovery constructs a receipt only when exact live
readback proves every intended object and property present; partial, missing,
disabled, or mismatched state remains journalled and blocks another run.
Persistent filters remain if helper crashes, so unique package SID stays
confined while controller terminates job. Cleanup
first terminates and drains candidate job, then elevated helper removes only
run SID exemption and receipt-bound WFP objects. Filter receipt alone does not
prove confinement. Controller tests forbidden destinations before candidate
exercise and again before promotion.

### Installed service and process evidence

Installed HTTP validation first binds these independent facts:

- exact registered task and normalized XML digest;
- current secure runtime record and readiness generation;
- live CQ action PID plus process creation time;
- live process token user SID, unique logon SID, session, non-elevated state,
  and medium integrity;
- absolute executable path, SHA-256, architecture, and complete file identity;
- loopback TCP listener owner from Windows TCP tables;
- configured candidate port and health response generation;
- WFP/AppContainer validation-run identity;
- exact client executable identity and zero-network test results.

Task Scheduler engine PID is not part of that chain. A `200 /health` response
alone proves only a listener. Any mismatch, substitution, exit, restart, task
drift, executable replacement, listener owner change, port reuse, or helper
loss invalidates the run. Candidate work never targets live port `19280`.

Cleanup order is candidate job termination and zero-process proof, run SID
loopback exemption removal, receipt-bound persistent WFP object deletion,
isolated root, AppContainer profile, then secure journal completion. CQ retains
a privacy-safe failed-run receipt when cleanup is incomplete and blocks another
validation until explicit cleanup succeeds.

## Error handling and user experience

- Security capability failures are hard errors before protected reads or writes.
- Cache creation and cache writes retain current graceful degradation because
  cache is not credential authority.
- Task Scheduler, named-pipe, job, AppContainer, and WFP errors retain Windows
  error codes internally and expose concise operation-specific messages.
- JSON output uses stable error codes and contains no ANSI escapes. Redirected
  terminal output disables color. Windows console detection uses handle-aware
  terminal support rather than Unix mode-bit assumptions.
- OAuth keeps loopback callback and browser behavior. Native tests cover browser
  launch quoting, cancellation, timeout, invalid callbacks, and one valid
  callback without live provider credentials.
- Administrator elevation is requested only for installed-validation WFP setup.
  Quota checks, account operations, proxy operation, and task installation stay
  per-user and unelevated.

## Five gated increments

Later increments do not begin until prior gate passes on current main ancestry.
Each increment remains independently reviewable and keeps Unix CI green.

### Gate 1: Windows foundation and secure state

Deliver:

- centralized AppData/LocalAppData paths;
- explicit non-destructive legacy import;
- SID, protected-DACL, reparse-safe retained handles;
- complete 128-bit Windows object identity;
- secure read, directory, atomic write/create/replace, sync, and `LockFileEx`;
- compatibility epoch, config, cache, model overlay, history, account manifests,
  and journals using correct roots;
- native console/JSON behavior and existing Windows browser path tests;
- Windows-safe test build tags for shared release-manifest definitions.

Gate:

- native Windows x64 and ARM64 filesystem suites pass on NTFS;
- ACL broadening, ancestor/final reparse substitution, pathname replacement,
  hard-link count, temporary replacement, lock contention, interrupted write,
  and indeterminate-sync cases fail closed;
- x64 and ARM64 builds and non-race tests pass;
- Unix paths and current Unix race suite remain byte-for-byte compatible where
  goldens exist.

### Gate 2: Credential owner and foreground runtime

Deliver:

- direct `x/sys/windows` named-pipe credential owner/delegate transport;
- same-user, same-logon-session, and process peer checks;
- direct named-pipe runtime channels and explicit handle inheritance;
- job-backed supervisor, worker, and child lifecycle;
- Windows executable, process, and listener identity;
- foreground proxy, all provider checks, account commands, refresh, models,
  proxy status/configuration/policy/rescue, Codex validate, and canary behavior.
- zero-network AppContainer candidate primitives used by foreground validation;
  Gate 4 extends those exact types with installed-client package ACLs,
  loopback exemption, and exact-port WFP rather than creating a second launcher.

Gate:

- fake-server flows for Claude, Codex, and Gemini pass natively on both
  architectures without live credentials;
- multi-process owner election, delegate operation, crash, replay, wrong-user,
  wrong-session, frame overflow, stale-generation, and job cleanup tests pass;
- automatic Codex operation never changes system credentials;
- foreground proxy serves HTTP and WebSocket synthetic traffic and shuts down
  with no child process or pipe left behind.

### Gate 3: Per-user scheduled operation

Deliver:

- strict Task Scheduler XML model and direct `schtasks.exe` runner;
- quota-refresh and proxy install, query, run, stop, restart, and uninstall;
- exactly 255 proxy restart attempts at one-minute intervals;
- durable attempt generations and `recovery_exhausted` human/JSON status;
- runtime record correlation instead of Task Scheduler engine PID;
- task-aware executable update and platform-specific help.

Gate:

- native x64 and ARM64 isolated tests use unique task names and temporary CQ
  roots;
- XML round-trip, quoting, Unicode/space paths, locale-independent query,
  principal drift, action substitution, 254/255/256 restart counts, idempotent
  install, restart generation, and exact-only removal tests pass;
- no test stores a password, uses an administrator token, or touches production
  task names or port `19280`.

### Gate 4: Confined candidate and installed validation

Deliver:

- unique AppContainer profiles and isolated roots;
- job-backed candidate process trees;
- separate elevated WFP helper with authenticated one-use IPC;
- default-empty CQ helper-digest seam that refuses installed validation before
  UAC in ordinary source builds;
- guarded helper-first, linker-anchored CQ/helper source pair used only for
  native Gate 4 qualification;
- exact-port IPv4 and IPv6 confinement plus bounded loopback prerequisite;
- Windows installed task/process/executable/listener/client attestation;
- ordered crash-safe cleanup and recovery journal.

Gate:

- native x64 and ARM64 tests prove candidate reaches exact candidate port and
  cannot reach DNS, internet, LAN, another loopback address/port, or listen;
- substitution tests cover task, XML, runtime record, PID reuse, creation time,
  executable path/digest/file ID, listener PID, port, AppContainer SID, helper,
  WFP conditions, and client binary;
- UAC refusal, helper crash, controller crash, filter transaction failure,
  exemption failure, and every cleanup boundary leave no untracked authority;
- exact source pair proves marker parsing and retained sibling hashing through
  production installed HTTP validation; zero marker and helper substitution
  fail before elevation or policy mutation;
- validation never exposes credentials or contacts live providers.

### Gate 5: Native qualification, release, and documentation

Deliver:

- required Windows 11 x64 and ARM64 CI jobs;
- command acceptance manifest covering every dispatch/help leaf;
- Windows ARM64 GoReleaser archive and packaged WFP helper;
- helper-first per-architecture digest anchoring inside `cq.exe`, with official
  Windows ZIPs as the full-parity installation source;
- build-once artifact manifest and native archive smoke tests;
- Windows install, update, task-safe replacement, uninstall, state retention,
  import, elevation, and troubleshooting documentation.

Gate:

- Windows x64 runs `go vet ./...`, `go build ./...`, and
  `go test -race -count=1 ./...` with a compatible MinGW runtime;
- Windows ARM64 runs `go vet ./...`, `go build ./...`, and
  `go test -count=1 ./...`; Go race detector does not support `windows/arm64`,
  so x64 race proof plus native ARM64 concurrency integration tests are
  required and this limitation is recorded;
- Linux keeps current native race suite plus Windows x64 and ARM64 cross-build
  and cross-test compilation;
- each final ZIP is tested on matching native architecture from a Unicode path
  containing spaces: `--version`, parseable JSON, provider fake flows,
  foreground proxy health/shutdown, scheduled proxy lifecycle, and installed
  validation;
- publication consumes exactly tested archives and checksums without rebuilding;
- post-publication downloads match pre-publication SHA-256 values;
- no command leaf returns a generic platform stub.
- both final ZIP byte sets pass fake-provider, foreground proxy, scheduled task,
  and protected installed-validation acceptance on matching native architecture.

## Test isolation and evidence

Native tests use synthetic credentials, fake upstreams, unique SIDs/profiles,
unique named pipes, unique task names, disposable Windows users/profiles, and
candidate ports selected outside `19280`. Known-folder anchors must resolve
inside the disposable profile; changing `APPDATA` or `LOCALAPPDATA` is not
isolation. Tests record OS build, architecture, filesystem, Go version, runner
image or owned-image digest, executable hashes, and artifact manifest digest.

Windows x64 and ARM64 are separate release authorities. Cross-build success on
one architecture cannot substitute for native proof on the other. Hosted runner
labels may move, so evidence records actual Windows 11 build and runner image.
If a native ARM64 hosted runner is unavailable, an owned Windows 11 ARM64 runner
must supply the release gate; emulation is not accepted.

Security integration tests may request UAC only in an isolated runner reserved
for Gate 4. Ordinary pull-request jobs exercise helper request validation and
negative paths without elevation; protected native qualification supplies the
real WFP proof before release.

### Owned-runner qualification chain

All five gates require an externally provisioned, time-bounded Windows
qualification lease. Repository code verifies the lease; it does not provision
or silently trust the runner. Capability `gate1-native` binds native source,
toolchain, test harness, disposable profile, remote-filesystem fixture, and
receipt-owned scratch only; controller, UAC broker, secure-desktop adapter, and
Codex fixture fields are canonically absent and are neither provisioned nor
validated. Gates 2 and 3 add capability `native-interactive`, which binds one
ordinary medium-integrity, non-elevated, nonzero-session interactive controller,
its exact real profile, and a launcher-owned authenticated live channel. It
adds no alternate administrator, UAC broker, Codex fixture, WFP authority, or
controller-scoped egress policy. Capabilities `gate4-source-protected` and
`gate5-release-protected` independently extend the base with privileged watchdog,
disposable controller/admin accounts, profiles and sessions, per-session
secure-desktop UAC broker, reviewed Codex fixture storage,
controller-scoped default-deny egress policy, signing key, and snapshot
rollback. A normal service-mode Actions coordinator may orchestrate but is not
an interactive medium-token controller and cannot answer secure-desktop UAC.

Admission, qualification, and cleanup receipts are bounded canonical JSON,
signed with a repository-pinned Ed25519 provisioner key, and chained by digest.
They bind exact repository, workflow, run ID, attempt, target job, commit,
native architecture, opaque lease generation, validity interval, Windows
image/build, requested capability set, previous reconciled cleanup digest,
canonical file/tree materialisations, baseline, and capability-conditional
controller, broker, fixture, policy, and artifact identities. Admission must be
live before mutation and qualification must be issued within that lease.
Cleanup has its own bounded watchdog deadline; later admissions validate prior
cleanup's historical issue/order/signature/baseline rather than requiring an
old receipt to remain currently unexpired. Receipt fields are evidence, never
process, token, filesystem, network, or artifact authority.

Schema `cq-windows-runner-receipt-v1` and package `internal/runnerproof` are the
canonical implementation contract. `DecodeAdmission`, `DecodeQualification`,
and `DecodeCleanup` enforce the bounded canonical envelopes;
`VerifyAdmission`, `VerifyQualification`, `VerifyCleanup`, and
`VerifyGateChain` enforce the signed chain and return only receipt digests or a
redacted `ChainSummary`. The exact field, capability, tree, broker, artifact,
and cleanup rules are frozen in the Windows runner infrastructure plan. No
single preflight receipt, workflow boolean, raw locator, or target-job cleanup
claim substitutes for any member of this three-receipt chain.

Gate 1 consumes `gate1-native` before setup-go, scratch creation, fixture
mutation, or tests, using the externally pinned verifier and exact retained
tree/file bindings. Gate 2 and Gate 3 each consume sorted capabilities
`gate1-native,native-interactive`, run only through that one live controller,
qualify their exact native results, request cleanup, and require the distinct
post-exit observer before the next gate begins. Gate 4 consumes the exact sorted
capability set `gate1-native,gate4-source-protected`; Gate 5 consumes
`gate5-release-protected`. The protected capabilities require broker-launched limited-administrator
and standard-user controllers in real nonzero interactive sessions, a reviewed
architecture-matching Codex fixture, exact production `ShellExecuteExW runas`
through the pinned UAC broker, and egress denial from each test controller while
the Actions management channel remains outside that policy. Plaintext
credentials may exist only in locked broker memory and direct Windows logon or
credential APIs for the lease; they never cross into Actions secrets,
environment, argv, stdin, files, registry, event logs, console output, or
artifacts. Windows account verifiers in SAM/LSA are not misdescribed as
plaintext credential persistence.

Workflow `always()` cleanup is not sufficient after hard cancellation or runner
loss, and a target job cannot attest its own destruction. Target sends one
idempotent private cleanup request and exits. A distinct observer, keyed by
repository/workflow/run/attempt/target-job/architecture/lease/generation,
terminates retained generations, unloads and deletes leased
profiles/accounts/sessions, removes test-only tasks, pipes, listeners,
AppContainers, WFP/egress objects, journals, materialisations, and temporary
roots, then restores or snapshot-reverts baseline and signs cleanup. External
lease store retains that private signed receipt after target raw roots are
retired; a separate attestation job verifies its chain and publishes only a
redacted summary outside the retired root. Next admission accepts a prior
`qualified` or `aborted` cleanup only when signature, digest, and every required
absence/baseline check are complete; gate success accepts only `qualified`.
Every gate passes only when matching admission, qualification, and independently
observed cleanup digests verify. Artifacts contain only canonical redacted
summaries and digests, never raw usernames, SIDs, paths, secrets, tokens,
environment, pipe names, or private lease-store locators.

## Acceptance requirements

Windows parity is complete only when all statements below are current:

1. Both Windows 11 architectures build and run native tests.
2. Windows x64 race suite and Windows ARM64 native concurrency suite pass.
3. All current command leaves have native behavior or the one exact Unix-socket
   not-applicable result.
4. All three provider fake flows pass without live credentials.
5. Secure state survives replacement and crash tests without accepting partial
   identity, broad ACL, or reparse traversal.
6. Codex owner/delegate coordination preserves exact-generation and read-only
   system credential rules.
7. Foreground and scheduled proxy HTTP/WebSocket behavior matches Unix-visible
   semantics.
8. Task operations are per-user, least-privilege, drift-aware, and exact-only.
9. Candidate installed validation proves exact-port confinement and complete
   task/process/executable/listener/client binding.
10. Final x64 and ARM64 archives are byte-identical to tested and published
    artifacts.
11. Windows documentation matches shipped paths, executables, commands, task
    names, elevation behavior, and cleanup semantics.
12. Existing Unix build, race, integration, and release checks remain green.

## Non-goals

- Windows 10, Windows Server, or `windows/386` support.
- MSI, MSIX, Microsoft Store, winget, Scoop, or Chocolatey packaging.
- A Windows service, `SYSTEM` proxy, stored Task Scheduler password, or machine-
  wide task.
- WSL as qualification evidence.
- PowerShell, `cmd.exe`, WMI, or COM automation in core operation.
- DPAPI encryption or a Windows-only credential schema.
- Automatic discovery, deletion, or relocation of legacy data.
- Silent ACL repair or ownership takeover.
- Provider policy, routing policy, quota calculation, or rendering redesign.
- Treating `IRunningTask::EnginePID`, task state, clean WFP API return, or
  `200 /health` as standalone delivery proof.

## Primary references

- [Go `os.UserConfigDir`](https://pkg.go.dev/os#UserConfigDir)
- [Go `os.UserCacheDir`](https://pkg.go.dev/os#UserCacheDir)
- [`ExpandEnvironmentStringsForUserW`](https://learn.microsoft.com/en-us/windows/win32/api/userenv/nf-userenv-expandenvironmentstringsforuserw)
- [`GetUserProfileDirectoryW`](https://learn.microsoft.com/en-us/windows/win32/api/userenv/nf-userenv-getuserprofiledirectoryw)
- [Recognized environment variables](https://learn.microsoft.com/en-us/windows/deployment/usmt/usmt-recognized-environment-variables)
- [`golang.org/x/sys/windows`](https://pkg.go.dev/golang.org/x/sys/windows)
- [Windows security identifiers](https://learn.microsoft.com/en-us/windows/win32/secauthz/security-identifiers)
- [Windows security descriptors](https://learn.microsoft.com/en-us/windows/win32/secauthz/security-descriptors)
- [`SetNamedSecurityInfoW`](https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-setnamedsecurityinfow)
- [`CreateFileW` and `FILE_FLAG_OPEN_REPARSE_POINT`](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew)
- [Reparse points and file operations](https://learn.microsoft.com/en-us/windows/win32/fileio/reparse-points-and-file-operations)
- [`FILE_ID_INFO`](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_id_info)
- [`GetFileInformationByHandleEx`](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-getfileinformationbyhandleex)
- [`LockFileEx`](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)
- [`FlushFileBuffers`](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers)
- [`FILE_RENAME_INFO`](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_rename_info)
- [Named-pipe security and access rights](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights)
- [Windows job objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
- [Task Scheduler XML schema](https://learn.microsoft.com/en-us/windows/win32/taskschd/task-scheduler-schema)
- [`schtasks.exe`](https://learn.microsoft.com/en-us/windows/win32/taskschd/schtasks)
- [Task restart interval](https://learn.microsoft.com/en-us/windows/win32/taskschd/tasksettings-restartinterval)
- [`IRunningTask::get_EnginePID`](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-irunningtask-get_enginepid)
- [Implementing an AppContainer](https://learn.microsoft.com/en-us/windows/win32/secauthz/implementing-an-appcontainer)
- [`NetworkIsolationSetAppContainerConfig`](https://learn.microsoft.com/en-us/windows/win32/api/networkisolation/nf-networkisolation-networkisolationsetappcontainerconfig)
- [WFP Application Layer Enforcement](https://learn.microsoft.com/en-us/windows/win32/fwp/application-layer-enforcement--ale-)
- [WFP ALE layers](https://learn.microsoft.com/en-us/windows/win32/fwp/ale-layers)
- [Go race detector requirements](https://go.dev/doc/articles/race_detector#Requirements)
- [GitHub-hosted Windows runners](https://docs.github.com/en/actions/how-tos/write-workflows/choose-where-workflows-run/choose-the-runner-for-a-job)
