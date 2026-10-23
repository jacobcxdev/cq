# Runtime and service output examples

Hand-crafted comparison fixtures for the proposed terminal design. These are fictional scenarios, not captured production evidence. Reference clock: 23 Sep 2026, 16:30 BST (UTC+01:00). All paths belong to fictional Alex. Each scenario is independent; identifiers repeat only to make transitions comparable. No scenario authorises running a mutation.

Output fences contain actual proposed output, without shell prompts or stream labels. Empty streams are stated outside fences. Every help scenario has empty stderr and exit 0; `-h` and `cq help PATH` produce the same bytes. These are colour-free terminal layout fixtures; shared design assigns semantic colours without changing their text. Output lines fit 100 cells except raw protocol JSON, which must not be wrapped. Existing redirected output and JSON protocols retain their separate contracts.

## `cq codex proxy canary start`

### Help

Invocation:

```sh
cq codex proxy canary start --help
```

Stdout:

```text
cq codex proxy canary start
Start a protected observation run of Codex routing.

Usage
  cq codex proxy canary start [OPTIONS]

Observe real routing; no synthetic traffic or routing-policy changes.
Last observed day is a UTC calendar-day bucket, not the last heartbeat or exact event time.
No arguments or command-specific options.
Requires HTTP routing enforcement, payload diagnostics disabled and current HTTP readiness.
Creates an observation record; it does not prove service attachment or observed traffic.
An active run conflicts. A valid completed run may be replaced.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy canary status
  cq service restart

Examples
  Create a run before separately attaching the service:
  cq codex proxy canary start
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy canary start
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last observed day  unknown
  Ended              —

Observations
  Admitted turns             0
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  0

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy canary start
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary preconditions are not satisfied.
  Code: canary_precondition_failed
  Command: cq codex proxy canary start
```

Exit: 6.

### Failure after publication

A directory-sync failure may occur after replacement publication. Empty stdout does not prove rollback. Inspect the retained run before retrying.

Invocation:

```sh
cq codex proxy canary start
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary state is unavailable.
  Code: canary_state_unavailable
  Command: cq codex proxy canary start
```

Exit: 1.

## `cq codex proxy canary status`

### Help

Invocation:

```sh
cq codex proxy canary status --help
```

Stdout:

```text
cq codex proxy canary status
Show the retained Codex routing observation run.

Usage
  cq codex proxy canary status [OPTIONS]

Observe real routing; no synthetic traffic or routing-policy changes.
Last observed day is a UTC calendar-day bucket, not the last heartbeat or exact event time.
No arguments or command-specific options.
Reads retained state and verifies protected digests; requests no transition.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy canary stop

Examples
  Inspect retained observations:
  cq codex proxy canary status
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy canary status
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            16h ago · 23 Sep 2026, 00:30:00 BST (UTC+01:00)
  Last observed day  15h 30m ago · 23 Sep 2026, 01:00:00 BST (UTC+01:00)
  Ended              —

Observations
  Admitted turns             1,248
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  1

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy canary status
```

Stdout: empty.

Stderr:

```text
Error · No Codex canary is recorded.
  Code: canary_missing
  Command: cq codex proxy canary status
```

Exit: 3.

## `cq codex proxy canary stop`

### Help

Invocation:

```sh
cq codex proxy canary stop --help
```

Stdout:

```text
cq codex proxy canary stop
Request a graceful end to the Codex routing observation run.

Usage
  cq codex proxy canary stop [OPTIONS]

Observe real routing; no synthetic traffic or routing-policy changes.
Last observed day is a UTC calendar-day bucket, not the last heartbeat or exact event time.
No arguments or command-specific options.
Requires an existing readable run. Records a stop request; the service drains asynchronously.
Success means requested, not finalised. Repeating the same stop request is idempotent.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy canary status

Examples
  Request drain and inspect completion:
  cq codex proxy canary stop
  cq codex proxy canary status
```

Stderr: empty.

Exit: 0.

### Success · stop requested, still active

Invocation:

```sh
cq codex proxy canary stop
```

Stdout:

```text
Codex canary · active
  Run                AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  Finalised          no
  Started            16h ago · 23 Sep 2026, 00:30:00 BST (UTC+01:00)
  Last observed day  15h 30m ago · 23 Sep 2026, 01:00:00 BST (UTC+01:00)
  Ended              —

Observations
  Admitted turns             1,248
  Keyed mismatches            0
  Protected-state changes    0
  Secret leaks               0
  Unexplained lifecycles      0
  Live-session repairs       0
  Protected-state failures   0
  Consecutive calendar days  1

Build binding
  CQ build               1.0.0
  Client build           0.114.0
  Parser schema          1
  Lease schema           3
  Semantics revision     http-conservative-routing-v3
  Retry budget           1
  Fixture SHA-256        618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1
  Readiness fingerprint  1122334411223344112233441122334411223344112233441122334411223344

Protected digests
  account_registry  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  codexbar_auth     bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  codexbar_manifest cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  cq_managed_auth   dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  routing_default   eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  system_auth       ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

Finalisation
  Not finalised.

Stop requested; drain completion has not been observed.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy canary stop
```

Stdout: empty.

Stderr:

```text
Error · The Codex canary state is unavailable.
  Code: canary_state_unavailable
  Command: cq codex proxy canary stop
```

Exit: 1.

## `cq codex proxy credential-endpoint legacy activate`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy activate
Activate a reversible replacement credential endpoint.

Usage
  cq codex proxy credential-endpoint legacy activate --ticket-file TICKET_FILE
    --confirm-stopped-and-drained [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Open a reversible replacement window; preserve quarantine for rollback.
Does not start the proxy. Start and verify the exact intended owner separately.

Options
  --ticket-file TICKET_FILE
      Required. Exact proof from the preceding JSON result; preserve unchanged.
      Absolute clean non-root path; no symlink; current-user-owned regular file and
      owner-controlled parent. Size 1–16,384 B; no group/other write permission.
  --confirm-stopped-and-drained
      Required; must be true. Assert every proxy/credential participant stays stopped and drained.
      This includes all participants, not only the current request.
  --non-interactive
      Skip typed confirmation only. Default: false; --json implies true.
      Required when stdin is not a TTY. Does not supply semantic confirmation.
      Explicit false equals omission.
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Use the retained, independently obtained proof:
  cq codex proxy credential-endpoint legacy activate --ticket-file \
    /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · activated
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy activate
```

Exit: 6.

### Timeout · inspect before continuing

No result was returned. The journal may contain an effect; timeout is not rollback proof.

Invocation:

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint operation timed out; inspect migration state before continuing.
  Code: endpoint_timeout
  Command: cq codex proxy credential-endpoint legacy activate
```

Exit: 7.

## `cq codex proxy credential-endpoint legacy finalise`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy finalise
Finalise a verified credential endpoint replacement.

Usage
  cq codex proxy credential-endpoint legacy finalise --ticket-file TICKET_FILE
    --confirm-candidate-healthy [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Irreversibly retire rollback state only after exact live-owner verification.
Requires activated state. A successful finalisation cannot be rolled back.

Options
  --ticket-file TICKET_FILE
      Required. Exact proof from the preceding JSON result; preserve unchanged.
      Absolute clean non-root path; no symlink; current-user-owned regular file and
      owner-controlled parent. Size 1–16,384 B; no group/other write permission.
  --confirm-candidate-healthy
      Required; must be true. Assert the exact candidate passed live health verification.
      CQ also verifies the owner itself.
  --non-interactive
      Skip typed confirmation only. Default: false; --json implies true.
      Required when stdin is not a TTY. Does not supply semantic confirmation.
      Explicit false equals omission.
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Use the retained, independently obtained proof:
  cq codex proxy credential-endpoint legacy finalise --ticket-file \
    /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy --non-interactive
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy --non-interactive
```

Stdout:

```text
Credential endpoint · committed
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy finalise
```

Exit: 6.

### Confirmation · pending

Interactive prompt before mutation; input has not yet been supplied. The actual consent phrase remains unchanged.

Invocation:

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-candidate-healthy
```

Stdout: empty.

Stderr:

```text
Finalise credential endpoint replacement
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Ticket  0123456789abcdef0123456789abcdef
  Effect  Permanently retire the rollback path after live-owner verification.

Type candidate-healthy to confirm the exact live candidate passed health verification: 
```

Exit: pending.

## `cq codex proxy credential-endpoint legacy inspect`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy inspect --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy inspect
Inspect the legacy Codex credential socket and migration state.

Usage
  cq codex proxy credential-endpoint legacy inspect [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Observe the refused legacy socket and return an identity snapshot. No transition.

Options
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Inspect the exact local endpoint:
  cq codex proxy credential-endpoint legacy inspect
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy inspect
```

Stdout:

```text
Credential endpoint · legacy_refused
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    snapshot
  Ticket  none

Identity observed.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy inspect
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint or transition does not exist.
  Code: endpoint_not_found
  Command: cq codex proxy credential-endpoint legacy inspect
```

Exit: 3.

## `cq codex proxy credential-endpoint legacy prepare`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy prepare --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy prepare
Quarantine a refused legacy credential socket for replacement.

Usage
  cq codex proxy credential-endpoint legacy prepare --snapshot-file SNAPSHOT_FILE
    --confirm-stopped-and-drained [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Quarantine the exact refused socket and retain a transition ticket.
Preparation neither activates a replacement nor starts the proxy.

Options
  --snapshot-file SNAPSHOT_FILE
      Required. Exact proof from the preceding JSON result; preserve unchanged.
      Absolute clean non-root path; no symlink; current-user-owned regular file and
      owner-controlled parent. Size 1–16,384 B; no group/other write permission.
  --confirm-stopped-and-drained
      Required; must be true. Assert every proxy/credential participant stays stopped and drained.
      This includes all participants, not only the current request.
  --non-interactive
      Skip typed confirmation only. Default: false; --json implies true.
      Required when stdin is not a TTY. Does not supply semantic confirmation.
      Explicit false equals omission.
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Use the retained, independently obtained proof:
  cq codex proxy credential-endpoint legacy prepare --snapshot-file \
    /Users/alex/cq-inputs/endpoint-snapshot.json --confirm-stopped-and-drained --non-interactive
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file /Users/alex/cq-inputs/endpoint-snapshot.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · prepared
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file /Users/alex/cq-inputs/endpoint-snapshot.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy prepare
```

Exit: 6.

## `cq codex proxy credential-endpoint legacy resume`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy resume --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy resume
Reopen a retained credential endpoint migration.

Usage
  cq codex proxy credential-endpoint legacy resume --ticket-file TICKET_FILE
    --confirm-stopped-and-drained [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Reopen the exact journal and report its actual phase. Does not imply completion.

Options
  --ticket-file TICKET_FILE
      Required. Exact proof from the preceding JSON result; preserve unchanged.
      Absolute clean non-root path; no symlink; current-user-owned regular file and
      owner-controlled parent. Size 1–16,384 B; no group/other write permission.
  --confirm-stopped-and-drained
      Required; must be true. Assert every proxy/credential participant stays stopped and drained.
      This includes all participants, not only the current request.
  --non-interactive
      Skip typed confirmation only. Default: false; --json implies true.
      Required when stdin is not a TTY. Does not supply semantic confirmation.
      Explicit false equals omission.
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Use the retained, independently obtained proof:
  cq codex proxy credential-endpoint legacy resume --ticket-file \
    /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · activated
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy resume
```

Exit: 6.

## `cq codex proxy credential-endpoint legacy rollback`

### Help

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --help
```

Stdout:

```text
cq codex proxy credential-endpoint legacy rollback
Restore the quarantined legacy credential socket.

Usage
  cq codex proxy credential-endpoint legacy rollback --ticket-file TICKET_FILE
    --confirm-stopped-and-drained [OPTIONS]

Maintains the current user's Codex credential.sock, not the HTTP listener.
No arbitrary socket path. Requires a supported local Unix backend.
Input identity and owner must still match the retained proof.
Restore and verify the exact quarantined socket. Committed transitions cannot roll back.

Options
  --ticket-file TICKET_FILE
      Required. Exact proof from the preceding JSON result; preserve unchanged.
      Absolute clean non-root path; no symlink; current-user-owned regular file and
      owner-controlled parent. Size 1–16,384 B; no group/other write permission.
  --confirm-stopped-and-drained
      Required; must be true. Assert every proxy/credential participant stays stopped and drained.
      This includes all participants, not only the current request.
  --non-interactive
      Skip typed confirmation only. Default: false; --json implies true.
      Required when stdin is not a TTY. Does not supply semantic confirmation.
      Explicit false equals omission.
  --timeout TIMEOUT
      Total deadline including cleanup. Default: 30s; Go duration from 1s to 5m inclusive.
      Cleanup reserve: 0s. Expiry does not prove rollback or an unchanged journal.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy credential-endpoint legacy inspect

Examples
  Use the retained, independently obtained proof:
  cq codex proxy credential-endpoint legacy rollback --ticket-file \
    /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout:

```text
Credential endpoint · rolled_back
  Path    /Users/alex/Library/Application Support/cq/codex/credential.sock
  Kind    transition
  Ticket  0123456789abcdef0123456789abcdef

Ticket retained.
Use --json to retain the exact structured proof.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint migration precondition failed: authority, identity or migration state
  does not match.
  Code: endpoint_conflict
  Command: cq codex proxy credential-endpoint legacy rollback
```

Exit: 6.

### Timeout · inspect before continuing

No result was returned. The journal may contain an effect; timeout is not rollback proof.

Invocation:

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file /Users/alex/cq-inputs/endpoint-ticket.json --confirm-stopped-and-drained --non-interactive
```

Stdout: empty.

Stderr:

```text
Error · Credential endpoint operation timed out; inspect migration state before continuing.
  Code: endpoint_timeout
  Command: cq codex proxy credential-endpoint legacy rollback
```

Exit: 7.

## `cq codex proxy fixture create`

### Help

Invocation:

```sh
cq codex proxy fixture create --help
```

Stdout:

```text
cq codex proxy fixture create
Create a sanitised fixture from a saved Codex request.

Usage
  cq codex proxy fixture create --input INPUT --output OUTPUT [OPTIONS]

No raw request content or credentials appear in the output fixture.
No positional arguments.

Options
  --input INPUT
      Required readable regular file; not stdin. Maximum encoded size: 2,097,152 B.
  --output OUTPUT
      Required new destination; must not exist or alias input. Creates missing parents as 0700.
  --content-encoding ENCODING
      auto, identity or zstd. Default: auto; detects Zstandard magic, otherwise identity.
      Decoded size at most 8,388,608 B; zstd expansion at most 128 times encoded size.
  --metadata-json JSON
      Literal FixtureMetadata object, not a filename. Default: metadata from request body.
      Reject duplicate keys and trailing JSON; metadata at most 65,536 UTF-8 bytes.
      request_kind: turn, prewarm, compaction or memory. turn requires session_id,
      thread_id and turn_id. prewarm requires session_id/thread_id and empty turn_id.
      compaction requires all three IDs and standalone_turn, pre_turn or mid_turn compaction.
      memory permits empty IDs. Non-compaction omits compaction; IDs at most 4,096 bytes.
      Optional window_id defaults empty. session_id/thread_id/turn_id default empty when
      their request kind permits it. Only documented FixtureMetadata fields are accepted.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy validate websocket

Examples
  Sanitise a saved request into a new file:
  cq codex proxy fixture create --input /Users/alex/cq-inputs/request.json \
    --output /Users/alex/cq-inputs/fixture.json
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy fixture create --input /Users/alex/cq-inputs/request.json --output /Users/alex/cq-inputs/fixture.json
```

Stdout:

```text
Fixture · created
  File               /Users/alex/cq-inputs/fixture.json
  Request kind       turn
  Input size         2,048 B
  Converted          0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Metadata source    nested
  Session hint       session:111122223333
  Thread hint        thread:444455556666
  Turn hint          turn:777788889999
  Previous response  no
  Encrypted state    no
  Body SHA-256       0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  Schema             1
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy fixture create --input /Users/alex/cq-inputs/request.json --output /Users/alex/cq-inputs/fixture.json
```

Stdout: empty.

Stderr:

```text
Error · The output file already exists.
  Code: fixture_output_exists
  Command: cq codex proxy fixture create
```

Exit: 6.

## `cq codex proxy hook stop`

### Help

Invocation:

```sh
cq codex proxy hook stop --help
```

Stdout:

```text
cq codex proxy hook stop
Return a private-data-safe routing receipt for a Codex Stop hook.

Usage
  cq codex proxy hook stop [OPTIONS]

Reads one UTF-8 JSON object from stdin, at most 16,777,216 bytes; only whitespace may follow.
Requires hook_event_name "Stop", nonempty session_id and turn_id; IDs at most 4,096 bytes,
without C0/DEL controls. Duplicate required keys fail. Extra event fields are ignored.
No positional arguments or command-specific options. Total lookup deadline: 5s.
Default stdout is one raw JSON object plus newline: systemMessage receipt, or {} if absent.
No colour, banners or wrapping. --json is a manual diagnostic envelope, not hook integration.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy trace

Examples
  Read a saved synthetic Stop event:
  cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stderr: empty.

Exit: 0.

### Success · no receipt

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout:

```text
{}
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
printf '{}\n' | cq codex proxy hook stop
```

Stdout: empty.

Stderr:

```text
Error · Invalid Codex Stop hook input.
  Code: hook_input_invalid
  Command: cq codex proxy hook stop
```

Exit: 2.

### Failure · lookup deadline

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout: empty.

Stderr:

```text
Error · The Codex turn receipt lookup timed out.
  Code: hook_timeout
  Command: cq codex proxy hook stop
```

Exit: 7.


### Success · retained receipt protocol

The raw JSON string below is intentionally one line, even beyond 100 cells. Never wrap or colour it.

Invocation:

```sh
cq codex proxy hook stop < /Users/alex/cq-inputs/stop-event.json
```

Stdout:

```text
{"systemMessage":"CQ route: completed via WebSocket; account codex:0123456789ab (actual); Sol/High; warm affinity. Shadow: no-affinity comparison agreed."}
```

Stderr: empty.

Exit: 0.

## `cq codex proxy validate http`

### Help

Invocation:

```sh
cq codex proxy validate http --help
```

Stdout:

```text
cq codex proxy validate http
Request startup validation of an installed candidate HTTP listener.

Usage
  cq codex proxy validate http --port PORT [OPTIONS]

Requires the verified installed candidate's service label, process and listener binding.
An accepted request is not completed validation. Readiness inspection is retained build evidence,
not correlation with this request: the engine exposes no public request ID.

Options
  --port PORT
      Required integer from 1 to 65535, excluding live port 19280.
      Must match the verified installed candidate binding.
  --timeout DURATION
      Total monotonic budget for preparation, requests and cleanup. Default: 30s.
      Go duration from 1s to 5m inclusive; interactive confirmation time excluded.
      Nested requests cannot extend the deadline. Accepted work may outlive a timeout.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy readiness show
  cq proxy candidate status

Examples
  Request validation of the already installed candidate:
  cq codex proxy validate http --port 29280
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy validate http --port 29280
```

Stdout:

```text
HTTP validation · requested
  Candidate port  29280
  Completion      not yet known

Request accepted; validation has not completed.
Inspect retained readiness evidence before retrying.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy validate http --port 29280
```

Stdout: empty.

Stderr:

```text
Error · The installed candidate service is unavailable.
  Code: validation_candidate_unavailable
  Command: cq codex proxy validate http
```

Exit: 4.

### Timeout · completion unknown

Invocation:

```sh
cq codex proxy validate http --port 29280 --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The HTTP validation operation timed out; inspect readiness before retrying.
  Code: validation_timeout
  Command: cq codex proxy validate http
```

Exit: 7.

## `cq codex proxy validate websocket`

### Help

Invocation:

```sh
cq codex proxy validate websocket --help
```

Stdout:

```text
cq codex proxy validate websocket
Validate WebSocket routing with an isolated installed Codex client.

Usage
  cq codex proxy validate websocket --client-build CLIENT_BUILD [OPTIONS]

Exercises an isolated installed client and records readiness only after all gates and cleanup pass.
Failure invalidates selected WebSocket readiness; no stale success fallback.

Options
  --client-build CLIENT_BUILD
      Required exact executable version, such as 0.114.0. Must match evidence byte for byte.
      Format: major.minor.patch, optionally followed by - or + and ASCII letters, digits, dots
      or hyphens. No whitespace, trimming, case folding, prefix matching or arbitrary labels.
  --client-executable PATH
      Default: /Applications/ChatGPT.app/Contents/Resources/codex, then
      /Applications/Codex.app/Contents/Resources/codex, then codex on PATH.
      Explicit value resolves to an absolute regular executable; its version must match.
  --state-dir PATH
      Default: resolved CQ proxy state directory, not runtime resilience state.
      If supplied, a nonempty canonical absolute directory without symlink traversal.
  --timeout DURATION
      Total monotonic budget including preparation, requests and cleanup. Default: 5m.
      Go duration from 30s to 30m inclusive. Final 5s reserved inside this budget for cleanup.
      Interactive confirmation time excluded; nested requests cannot extend the deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq codex proxy readiness show
  cq codex proxy fixture create

Examples
  Validate the installed client version:
  cq codex proxy validate websocket --client-build 0.114.0
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.114.0
```

Stdout:

```text
WebSocket validation · passed
  Client build  0.114.0
  Validated     0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  CQ build      1.0.0
  Gates passed  9

Evidence
  Version             3
  Transport           websocket
  Parser schema       1
  Lease schema        3
  Semantics revision  websocket-terminating-routing-v1
  Retry budget        1
  Fixture SHA-256      858d90f3e827194cdfc0dbf5e11d35dfb25e7afc395cf547f17a30936889f9f9
  CQ executable       —
  Client executable   —
  Service identity    —
  Service kind        —
  Installed result    passed
  Completed gates
    admitted-no-migration
    canonical-terminal-error
    compression-subprotocol
    installed-isolated-client
    persistent-account-upstream
    portable-pre-admission-hard429-rotation
    same-account-candidate-auth-recovery
    strong-frame-authority
    upstream-generation-fence
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.113.0
```

Stdout: empty.

Stderr:

```text
Error · The Codex executable does not match --client-build.
  Code: validation_build_mismatch
  Command: cq codex proxy validate websocket
```

Exit: 6.

### Timeout · no readiness recorded

Invocation:

```sh
cq codex proxy validate websocket --client-build 0.114.0 --timeout 30s
```

Stdout: empty.

Stderr:

```text
Error · WebSocket validation timed out; readiness was not recorded.
  Code: validation_timeout
  Command: cq codex proxy validate websocket
```

Exit: 7.

## `cq proxy candidate prepare`

### Help

Invocation:

```sh
cq proxy candidate prepare --help
```

Stdout:

```text
cq proxy candidate prepare
Prepare an isolated release candidate without starting it.

Usage
  cq proxy candidate prepare --state-dir STATE_DIR --port PORT --source-config PATH
    --target-release-bundle PATH --release-digest DIGEST --client-build CLIENT_BUILD
    --client-executable PATH --client-registry PATH [OPTIONS]

Input files require absolute clean non-root paths, no symlinks, current-user ownership,
owner-controlled parents and no group/other writes. Structured JSON rejects unknown/missing fields,
duplicate keys, invalid UTF-8 and trailing JSON; canonical documents have no trailing newline.
Preparation binds supplied attestations; it does not apply their contents, import/refresh
  credentials,
start a listener, or modify the shared installed service. Inputs come from the release system.

Required options
  --state-dir STATE_DIR
      New candidate directory under an owner-controlled parent. Absolute clean non-root path;
      no symlink or existing candidate state. Never defaults to shared state.
  --port PORT
      Integer 1–65535 except 19280; must not match any active shared/candidate listener.
  --source-config PATH
      Source attestation bytes, 1–1,048,576 B. Hash recorded; contents not applied.
  --target-release-bundle PATH
      Signed OperationalReleaseBundleV1, purpose target; 1–16,777,216 B.
  --release-digest DIGEST
      Exactly 64 lowercase hex characters; equals verified target bundle's digest field.
      Not SHA-256 of the bundle file.
  --client-build CLIENT_BUILD
      Nonempty UTF-8 without NUL/controls. Exact executable provenance and independently
      measured build, byte for byte; no trimming, aliases, case folding or arbitrary labels.
  --client-executable PATH
      Absolute clean regular executable, 1–536,870,912 B; no symlink/group/other writes.
      Stable identity through opening. File and trusted parent may belong to current user or root.
  --client-registry PATH
      Canonical ClientSenderRegistryV1 describing every sender/transport; 1–65,536 B.

Options
  --credential-mode MODE
      none or read-only. Default: none. read-only requires --credential-manifest and true
      --confirm-read-only-credentials; none forbids both.
  --credential-manifest PATH
      Credential attestation bytes, 1–1,048,576 B; only with read-only mode.
      No importing or refreshing credentials.
  --confirm-read-only-credentials
      Assert attested credentials permit read-only access. Does not authorise refresh.
      Default: false; explicit false equals omission.
  --policy-snapshot PATH
      Optional attestation bytes, 1–1,048,576 B. Omission records no digest; contents not applied.
  --confirm-payload-capture
      Authorise capture only in this candidate workflow. Default: false; omission prohibits it.
      Preparation itself captures nothing. Explicit false equals omission.
  --timeout DURATION
      Total budget including cleanup. Default: 150s; Go duration 150s–5m inclusive.
      Reserve final 30s within that budget for cleanup; no unrecorded mutation on cancellation.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status
  cq proxy candidate remove

Examples
  Bind supplied release-system inputs without starting a listener:
  cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json
```

Stdout:

```text
Candidate · prepared
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      1
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate prepare
```

Exit: 6.

### Timeout · inspect retained candidate

No receipt was returned. A retained directory or pending effect is possible; do not assume no change.

Invocation:

```sh
cq proxy candidate prepare \
  --state-dir /Users/alex/cq-candidate-29280 --port 29280 \
  --source-config /Users/alex/cq-inputs/source-config.bin \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --client-registry /Users/alex/cq-inputs/client-registry.json --timeout 150s
```

Stdout: empty.

Stderr:

```text
Error · Candidate operation timed out; inspect candidate status before retrying.
  Code: candidate_timeout
  Command: cq proxy candidate prepare
```

Exit: 7.

## `cq proxy candidate receipt show`

### Help

Invocation:

```sh
cq proxy candidate receipt show --help
```

Stdout:

```text
cq proxy candidate receipt show
Show one authenticated retained candidate receipt.

Usage
  cq proxy candidate receipt show --state-dir STATE_DIR --attempt-id ATTEMPT_ID [OPTIONS]

This reads a previously published receipt; it does not request new validation.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --attempt-id ATTEMPT_ID
      Required exact returned attempt ID: 32 lowercase hexadecimal characters.
      No latest-attempt fallback; never derive it from an operation ID.
  --timeout DURATION
      Total deadline including cleanup. Default: 10s; Go duration 1s–30s inclusive.
      Cleanup reserve: 0s; expiry does not establish an unchanged state.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Inspect a retained attempt:
  cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id \
    0123456789abcdef0123456789abcdef
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Candidate receipt · published
  Attempt           0123456789abcdef0123456789abcdef
  Receipt digest    5555555555555555555555555555555555555555555555555555555555555555
  Promotion digest  6666666666666666666666666666666666666666666666666666666666666666
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout: empty.

Stderr:

```text
Error · Candidate state or receipt does not exist.
  Code: candidate_not_found
  Command: cq proxy candidate receipt show
```

Exit: 3.

### Failure · retained conflicted receipt

Receipt came from an earlier qualified runtime. This build cannot create new qualification evidence.

Invocation:

```sh
cq proxy candidate receipt show --state-dir /Users/alex/cq-candidate-29280 --attempt-id 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Candidate receipt · conflicted
  Attempt           0123456789abcdef0123456789abcdef
  Receipt digest    5555555555555555555555555555555555555555555555555555555555555555
  Promotion digest  6666666666666666666666666666666666666666666666666666666666666666
```

Stderr:

```text
Error · Candidate receipt is conflicted.
  Code: candidate_receipt_conflicted
  Command: cq proxy candidate receipt show
```

Exit: 6.

## `cq proxy candidate client-safety refresh`

### Help

Invocation:

```sh
cq proxy candidate client-safety refresh --help
```

Stdout:

```text
cq proxy candidate client-safety refresh
Measure and renew client credential-isolation evidence.

Usage
  cq proxy candidate client-safety refresh --state-dir STATE_DIR --validation-run-id
    VALIDATION_RUN_ID [OPTIONS]

Unavailable in this build.
Syntax is validated, then exit 4 before state access. No transition, receipt or evidence is created.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --validation-run-id VALIDATION_RUN_ID
      Required exact stored run identifier: 64 lowercase hexadecimal characters.
      Obtain from prepare or status; this is an identifier, not a file hash.
  --timeout DURATION
      Total budget including cleanup. Default: 150s; Go duration 150s–5m inclusive.
      Reserve final 30s within this budget for cleanup.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Demonstrate the unavailable result with syntactically valid inputs:
  cq proxy candidate client-safety refresh --state-dir /Users/alex/cq-candidate-29280 \
    --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate client-safety refresh --state-dir /Users/alex/cq-candidate-29280 --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate client-safety refresh
```

Exit: 4.

## `cq proxy candidate start`

### Help

Invocation:

```sh
cq proxy candidate start --help
```

Stdout:

```text
cq proxy candidate start
Start the exact release bound to an isolated candidate.

Usage
  cq proxy candidate start --state-dir STATE_DIR [OPTIONS]

Unavailable in this build.
Syntax is validated, then exit 4 before state access. No transition, receipt or evidence is created.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --timeout DURATION
      Total budget including cleanup. Default: 30s; Go duration 30s–90s inclusive.
      Reserve final 15s within this budget for cleanup.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Demonstrate the unavailable result with syntactically valid inputs:
  cq proxy candidate start --state-dir /Users/alex/cq-candidate-29280
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate start --state-dir /Users/alex/cq-candidate-29280
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate start
```

Exit: 4.

## `cq proxy candidate release activate`

### Help

Invocation:

```sh
cq proxy candidate release activate --help
```

Stdout:

```text
cq proxy candidate release activate
Activate the target release inside an isolated candidate.

Usage
  cq proxy candidate release activate --state-dir STATE_DIR --release-digest DIGEST
    --validation-run-id RUN_ID --confirm-artifact-switch [OPTIONS]

Unavailable in this build.
Syntax is validated, then exit 4 before state access. No transition, receipt or evidence is created.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --release-digest DIGEST
      Required verified target bundle digest from prepare; exactly 64 lowercase hex characters.
      Must match the prepared target, not the bundle file hash.
  --validation-run-id VALIDATION_RUN_ID
      Required exact stored run identifier: 64 lowercase hexadecimal characters.
      Obtain from prepare or status; this is an identifier, not a file hash.
  --confirm-artifact-switch
      Required; must be true. Authorise replacement of this candidate runtime only.
  --timeout DURATION
      Total budget including cleanup. Default: 90s; Go duration 90s–2m inclusive.
      Reserve final 30s within this budget for cleanup.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Demonstrate the unavailable result with syntactically valid inputs:
  cq proxy candidate release activate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 \
    --confirm-artifact-switch
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate release activate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --release-digest abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 --confirm-artifact-switch
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate release activate
```

Exit: 4.

## `cq proxy candidate release validate`

### Help

Invocation:

```sh
cq proxy candidate release validate --help
```

Stdout:

```text
cq proxy candidate release validate
Validate one isolated candidate release against accepted rollback evidence.

Usage
  cq proxy candidate release validate --state-dir STATE_DIR --target-release-bundle PATH
    --rollback-bundle PATH --rollback-receipt PATH --rollback-receipt-digest DIGEST --client-build
    CLIENT_BUILD --client-executable PATH --validation-run-id RUN_ID --receipt-file PATH
    --confirm-control-health [OPTIONS]

Unavailable in this build.
Syntax is validated, then exit 4 before state access. No transition, receipt or evidence is created.
Validation does not install or promote the shared service.
Input files require absolute clean non-root paths, no symlinks, current-user ownership,
owner-controlled parents and no group/other writes. Structured JSON rejects unknown/missing fields,
duplicate keys, invalid UTF-8 and trailing JSON; canonical documents have no trailing newline.

Required options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --target-release-bundle PATH
      Exact target OperationalReleaseBundleV1 used by prepare; 1–16,777,216 B.
  --rollback-bundle PATH
      Accepted OperationalReleaseBundleV1, purpose floor; 1–16,777,216 B.
      Source commit must be a strict ancestor of target.
  --rollback-receipt PATH
      Original authenticated acceptance-system bytes, 1–65,536 B; never fabricated proof.
  --rollback-receipt-digest DIGEST
      Exactly 64 lowercase hex characters. SHA-256 over UTF-8 cq/release-import-floor/v1,
      then NUL, then exact receipt bytes. Not an ordinary file SHA-256.
  --client-build CLIENT_BUILD
      Nonempty UTF-8 without NUL/controls; exact measured executable provenance and value
      bound by prepare. No trimming, case folding, aliases or arbitrary operator labels.
  --client-executable PATH
      Absolute clean regular executable, 1–536,870,912 B; stable identity; no symlink or
      group/other writes. File and trusted parent may belong to current user or root.
  --validation-run-id VALIDATION_RUN_ID
      Required exact stored run identifier: 64 lowercase hexadecimal characters.
      Obtain from prepare or status; this is an identifier, not a file hash.
  --receipt-file PATH
      New absolute clean non-root path; no existing leaf, including symlink.
      Existing current-user-owned, owner-controlled parent; hold identity through creation.
      Write a new regular file as 0600 atomically; never overwrite or follow a leaf.
  --confirm-control-health
      Required; must be true. Assert exact candidate passed control health.
      CQ must also verify live identity and health itself.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status
  cq proxy candidate receipt show

Examples
  Show the unavailable result; values below are fictional, not valid proof:
  cq proxy candidate release validate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --rollback-bundle /Users/alex/cq-inputs/floor.json \
  --rollback-receipt /Users/alex/cq-inputs/floor-receipt.bin \
  --rollback-receipt-digest 7777777777777777777777777777777777777777777777777777777777777777 \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 \
  --receipt-file /Users/alex/cq-inputs/promotion.json --confirm-control-health
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate release validate \
  --state-dir /Users/alex/cq-candidate-29280 \
  --target-release-bundle /Users/alex/cq-inputs/target.json \
  --rollback-bundle /Users/alex/cq-inputs/floor.json \
  --rollback-receipt /Users/alex/cq-inputs/floor-receipt.bin \
  --rollback-receipt-digest 7777777777777777777777777777777777777777777777777777777777777777 \
  --client-build 0.114.0 --client-executable /Users/alex/bin/codex \
  --validation-run-id 1234567812345678123456781234567812345678123456781234567812345678 \
  --receipt-file /Users/alex/cq-inputs/promotion.json --confirm-control-health
```

Stdout: empty.

Stderr:

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  Code: candidate_evidence_unavailable
  Command: cq proxy candidate release validate
```

Exit: 4.

## `cq proxy candidate status`

### Help

Invocation:

```sh
cq proxy candidate status --help
```

Stdout:

```text
cq proxy candidate status
Show retained state for one isolated candidate.

Usage
  cq proxy candidate status --state-dir STATE_DIR [OPTIONS]

Inspection does not repair, qualify or infer liveness from retained phase.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --timeout DURATION
      Total deadline including cleanup. Default: 10s; Go duration 1s–30s inclusive.
      Cleanup reserve: 0s.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate stop

Examples
  Inspect retained state:
  cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout:

```text
Candidate · prepared
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      1
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout: empty.

Stderr:

```text
Error · Candidate state or receipt does not exist.
  Code: candidate_not_found
  Command: cq proxy candidate status
```

Exit: 3.

### Success · pending external effect

Inspection completed, but the recorded stop effect is unresolved. Exit 0 does not mean healthy.

Invocation:

```sh
cq proxy candidate status --state-dir /Users/alex/cq-candidate-29280
```

Stdout:

```text
Candidate · running
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      6
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  stop
  Effect started  yes

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         —
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         —
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.

## `cq proxy candidate stop`

### Help

Invocation:

```sh
cq proxy candidate stop --help
```

Stdout:

```text
cq proxy candidate stop
Stop an isolated candidate after its client has stopped.

Usage
  cq proxy candidate stop --state-dir STATE_DIR --confirm-client-stopped [OPTIONS]

Requires running or validated phase and proof of process/listener absence before success.
Already stopped is a phase conflict; pending effects are never automatically replayed.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --confirm-client-stopped
      Required; must be true. Assert every client using this candidate has stopped
      and no request remains active.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Use exact retained candidate identity after the stated preconditions:
  cq proxy candidate stop --state-dir /Users/alex/cq-candidate-29280 --confirm-client-stopped
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy candidate stop --state-dir /Users/alex/cq-candidate-29280 --confirm-client-stopped
```

Stdout:

```text
Candidate · stopped
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      7
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         unknown
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         9999999999999999999999999999999999999999999999999999999999999999
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate stop --state-dir /Users/alex/cq-candidate-29280 --confirm-client-stopped
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate stop
```

Exit: 6.

## `cq proxy candidate remove`

### Help

Invocation:

```sh
cq proxy candidate remove --help
```

Stdout:

```text
cq proxy candidate remove
Delete stopped candidate state and retained receipts.

Usage
  cq proxy candidate remove --state-dir STATE_DIR --confirm-candidate-state-loss [OPTIONS]

Permanently deletes only the exact candidate directory, keys and retained receipts.
Requires independent process/listener absence proof; never removes the shared installation.

Options
  --state-dir STATE_DIR
      Required existing candidate directory from prepare. Absolute, clean, non-root,
      current-user-owned; no symlink; parent owner-controlled. Never defaults to shared state.
  --confirm-candidate-state-loss
      Required; must be true. Authorise permanent loss of this candidate directory,
      keys and retained receipts.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy candidate status

Examples
  Use exact retained candidate identity after the stated preconditions:
  cq proxy candidate remove --state-dir /Users/alex/cq-candidate-29280 \
    --confirm-candidate-state-loss
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy candidate remove --state-dir /Users/alex/cq-candidate-29280 --confirm-candidate-state-loss
```

Stdout:

```text
Candidate · removed
  Instance        11112222333344445555666677778888
  State           /Users/alex/cq-candidate-29280
  Port            29280
  Operation       88887777666655554444333322221111
  Validation run  1234567812345678123456781234567812345678123456781234567812345678
  Generation      10
  Updated         0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Pending action  —
  Effect started  no

Identity and authority
  Client build     0.114.0
  Credential mode  none
  Payload capture  no
  Attempt          —

Digests
  Source config          1111111111111111111111111111111111111111111111111111111111111111
  Target bundle file     2222222222222222222222222222222222222222222222222222222222222222
  Target release         abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd
  Active release         unknown
  Client executable      3333333333333333333333333333333333333333333333333333333333333333
  Client registry file   4444444444444444444444444444444444444444444444444444444444444444
  Credential manifest    —
  Policy snapshot        —
  Effect receipt         9999999999999999999999999999999999999999999999999999999999999999
  Client safety receipt  —
  Validation receipt     —
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy candidate remove --state-dir /Users/alex/cq-candidate-29280 --confirm-candidate-state-loss
```

Stdout: empty.

Stderr:

```text
Error · Candidate state conflicts with this operation.
  Code: candidate_conflict
  Command: cq proxy candidate remove
```

Exit: 6.

## `cq proxy health`

### Help

Invocation:

```sh
cq proxy health --help
```

Stdout:

```text
cq proxy health
Probe proxy HTTP liveness.

Usage
  cq proxy health [OPTIONS]

Probe the health endpoint only; success does not verify model traffic or routing acceptance.

Options
  --port PORT
      Loopback integer 1–65535. Default: configured port, or 19280 if absent.
      Reading the default does not create configuration.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 5s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy status
  cq service status

Examples
  Check the configured listener:
  cq proxy health
  Probe a specific loopback listener:
  cq proxy health --port 29280
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · healthy
  Address    127.0.0.1:19280
  HTTP       200
  Reachable  yes
  Elapsed    12ms

Health endpoint only; model traffic not verified.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · failed
  Address    127.0.0.1:19280
  HTTP       unknown
  Reachable  no
  Elapsed    5ms

Health endpoint only; model traffic not verified.
```


Stderr:

```text
Error · Proxy is not reachable at 127.0.0.1:19280.
  Code: proxy_unreachable
  Command: cq proxy health
```

Exit: 4.

### Failure · reachable but unhealthy

Invocation:

```sh
cq proxy health
```

Stdout:

```text
Proxy health · failed
  Address    127.0.0.1:19280
  HTTP       503
  Reachable  yes
  Elapsed    14ms

Health endpoint only; model traffic not verified.
```

Stderr:

```text
Error · Proxy HTTP health check failed.
  Code: proxy_health_failed
  Command: cq proxy health
```

Exit: 1.

## `cq proxy operation status`

### Help

Invocation:

```sh
cq proxy operation status --help
```

Stdout:

```text
cq proxy operation status
Show a retained shared proxy operation record.

Usage
  cq proxy operation status [OPERATION_ID] [OPTIONS]

Inspect the selected coordinator record without requesting recovery or another transition.

Arguments
  OPERATION_ID
      Optional exact operation ID: 32 lowercase hexadecimal characters.
      Default: currently selected coordinator record. Explicit missing IDs fail.

No command-specific options.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy status
  cq proxy rescue status

Examples
  Inspect the selected record:
  cq proxy operation status
  Inspect an exact returned ID:
  cq proxy operation status 0123456789abcdef0123456789abcdef
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy operation status 0123456789abcdef0123456789abcdef
```

Stdout:

```text
Proxy operation · result_available
  Operation         0123456789abcdef0123456789abcdef
  Phase             terminal
  Result available  yes
  Result digest     aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  Recovery support  unavailable
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy operation status fedcba9876543210fedcba9876543210
```

Stdout: empty.

Stderr:

```text
Error · The requested operation does not exist.
  Code: operation_not_found
  Command: cq proxy operation status
```

Exit: 3.

### Success · idle coordinator

Invocation:

```sh
cq proxy operation status
```

Stdout:

```text
Proxy operation · idle
  Operation         idle
  Phase             —
  Result available  no
  Result digest     —
  Recovery support  unavailable
```

Stderr: empty.

Exit: 0.

## `cq proxy rescue enter`

### Help

Invocation:

```sh
cq proxy rescue enter --help
```

Stdout:

```text
cq proxy rescue enter
Request shared proxy rescue traffic mode.

Usage
  cq proxy rescue enter [OPTIONS]

Entry may return while normal-worker sessions drain. Repeating the same intent is idempotent.

Options
  --port PORT
      Loopback control port, integer 1–65535. Default: configured rescue bootstrap port,
      or 19280 when its value is zero. Omission reads existing config without creating it.
  --timeout DURATION
      Total monotonic budget for preparation, requests and cleanup. Default: 30s.
      Go duration 1s–5m inclusive. Interactive confirmation time excluded.
      Nested requests cannot extend this deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy rescue status
  cq proxy operation status

Examples
  Request the transition, then inspect its progress:
  cq proxy rescue enter
```

Stderr: empty.

Exit: 0.

### Success · transition acknowledged, drain pending

Invocation:

```sh
cq proxy rescue enter
```

Stdout:

```text
Proxy rescue · rescue_draining
  Generation         12
  Active requests    0
  Draining sessions  2

Draining sessions
  session:123456789abc
  session:abcdef123456
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy rescue enter
```

Stdout: empty.

Stderr:

```text
Error · The proxy cannot perform this rescue transition.
  Code: rescue_transition_conflict
  Command: cq proxy rescue enter
```

Exit: 6.

### Timeout · inspect state before retrying

Invocation:

```sh
cq proxy rescue enter --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The proxy rescue operation timed out; inspect status before retrying.
  Code: rescue_timeout
  Command: cq proxy rescue enter
```

Exit: 7.

## `cq proxy rescue exit`

### Help

Invocation:

```sh
cq proxy rescue exit --help
```

Stdout:

```text
cq proxy rescue exit
Request return to normal proxy traffic.

Usage
  cq proxy rescue exit [OPTIONS]

New normal traffic can resume while old rescue work drains. An acknowledgement is not completion.

Options
  --port PORT
      Loopback control port, integer 1–65535. Default: configured rescue bootstrap port,
      or 19280 when its value is zero. Omission reads existing config without creating it.
  --timeout DURATION
      Total monotonic budget for preparation, requests and cleanup. Default: 30s.
      Go duration 1s–5m inclusive. Interactive confirmation time excluded.
      Nested requests cannot extend this deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy rescue status
  cq proxy operation status

Examples
  Request the transition, then inspect its progress:
  cq proxy rescue exit
```

Stderr: empty.

Exit: 0.

### Success · transition acknowledged, drain pending

Invocation:

```sh
cq proxy rescue exit
```

Stdout:

```text
Proxy rescue · rescue_exit_draining
  Generation         13
  Active requests    1
  Draining sessions  1

Draining sessions
  session:abcdef123456
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy rescue exit
```

Stdout: empty.

Stderr:

```text
Error · The proxy cannot perform this rescue transition.
  Code: rescue_transition_conflict
  Command: cq proxy rescue exit
```

Exit: 6.

### Timeout · inspect state before retrying

Invocation:

```sh
cq proxy rescue exit --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · The proxy rescue operation timed out; inspect status before retrying.
  Code: rescue_timeout
  Command: cq proxy rescue exit
```

Exit: 7.

## `cq proxy rescue status`

### Help

Invocation:

```sh
cq proxy rescue status --help
```

Stdout:

```text
cq proxy rescue status
Show shared proxy rescue and drain state.

Usage
  cq proxy rescue status [OPTIONS]

Read-only inspection; does not request a transition. Session labels are privacy-safe hints.

Options
  --port PORT
      Loopback control port, integer 1–65535. Default: configured rescue bootstrap port,
      or 19280 when its value is zero. Omission reads existing config without creating it.
  --timeout DURATION
      Total monotonic budget for preparation, requests and cleanup. Default: 30s.
      Go duration 1s–5m inclusive. Interactive confirmation time excluded.
      Nested requests cannot extend this deadline.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy rescue enter
  cq proxy rescue exit

Examples
  Observe traffic mode and drain progress:
  cq proxy rescue status
```

Stderr: empty.

Exit: 0.

### Success · normal

Invocation:

```sh
cq proxy rescue status
```

Stdout:

```text
Proxy rescue · normal
  Generation         14
  Active requests    0
  Draining sessions  0

No draining sessions.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy rescue status
```

Stdout: empty.

Stderr:

```text
Error · Proxy rescue control is unavailable.
  Code: rescue_unavailable
  Command: cq proxy rescue status
```

Exit: 4.

## `cq proxy serve`

### Help

Invocation:

```sh
cq proxy serve --help
```

Stdout:

```text
cq proxy serve
Run the shared API proxy in the foreground.

Usage
  cq proxy serve [OPTIONS]

Owns a foreground listener for Claude and Codex. Does not install a background service.
Ready output appears only after binding and runtime initialisation; it is not model acceptance
  proof.
Ctrl-C requests shutdown and exits 130. SIGTERM uses the successful termination contract.
--json emits ready/stopped JSONL events, not a single JSON envelope; never colour or wrap it.

Options
  --port PORT
      Loopback integer 1–65535. Default: proxy.json port, or 19280 if absent.
      Port 0 is forbidden. A listener owned by another process is a conflict.
  --migrate-legacy-managed
      Add missing routing identity metadata to legacy CQ-managed credentials before serving.
      Default: false. Never changes external or system-owned credentials.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service install
  cq proxy health

Examples
  Run in the foreground on the configured port:
  cq proxy serve
  Run on an explicit port:
  cq proxy serve --port 29280
```

Stderr: empty.

Exit: 0.

### Success · foreground run ended by SIGTERM

Invocation:

```sh
cq proxy serve
```

Stdout:

```text
Proxy · listening
  Address    127.0.0.1:19280
  PID        4217
  Providers  Claude, Codex

Ctrl-C stops this foreground process.

Proxy · stopped
  Reason     interrupted
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy serve
```

Stdout: empty.

Stderr:

```text
Error · Port 19280 is already in use.
  Code: proxy_port_in_use
  Command: cq proxy serve
```

Exit: 6.

### Interruption · completed shutdown after Ctrl-C

SIGINT arrived after readiness; the stopped block requires observed owned-listener closure. A second interrupt before closure must omit it.

Invocation:

```sh
cq proxy serve
```

Stdout:

```text
Proxy · listening
  Address    127.0.0.1:19280
  PID        4217
  Providers  Claude, Codex

Ctrl-C stops this foreground process.

Proxy · stopped
  Reason     interrupted
```

Stderr:

```text
Error · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq proxy serve
```

Exit: 130.

## `cq proxy state initialise`

### Help

Invocation:

```sh
cq proxy state initialise --help
```

Stdout:

```text
cq proxy state initialise
Initialise shared authenticated proxy state.

Usage
  cq proxy state initialise --state-dir STATE_DIR [OPTIONS]

Creates or safely reuses shared runtime-control authority. Does not restart the service.
Known creation/binding receipts survive later failures; failure never proves rollback.

Options
  --state-dir STATE_DIR
      Required clean absolute non-root directory. Reject symlink path components and
      existing foreign or incompatible state.
  --timeout DURATION
      Maximum elapsed time including locks, requests and verification. Default: 30s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy status
  cq service restart

Examples
  Initialise an explicitly chosen owned directory:
  cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout: empty.

Stderr:

```text
Error · Proxy state ownership conflicts with the requested directory.
  Code: proxy_state_conflict
  Command: cq proxy state initialise
```

Exit: 6.

### Success · safely reused state

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · existing
  Directory         /Users/alex/cq-runtime
  Restart required  no
```

Stderr: empty.

Exit: 0.

### Partial · creation committed, later binding failed

Durable authority creation and restart requirement are known; later configuration persistence failed. No rollback claim.

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created · incomplete
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr:

```text
Error · Proxy state initialisation partially completed; inspect state before retrying.
  Code: proxy_state_initialise_partial
  Command: cq proxy state initialise
```

Exit: 8.

### Interruption · preserve known creation

Invocation:

```sh
cq proxy state initialise --state-dir /Users/alex/cq-runtime
```

Stdout:

```text
Proxy state · created · incomplete
  Directory         /Users/alex/cq-runtime
  Restart required  yes
```

Stderr:

```text
Error · Operation interrupted; inspect state before retrying.
  Code: interrupted
  Command: cq proxy state initialise
```

Exit: 130.

## `cq proxy status`

### Help

Invocation:

```sh
cq proxy status --help
```

Stdout:

```text
cq proxy status
Inspect shared proxy readiness evidence.

Usage
  cq proxy status [OPTIONS]

Collect inspector, desired configuration, service, listener, process, runtime and data-plane facts.
Inspection does not create missing state, repair services or send model acceptance traffic.
Default exit 0 means inspection completed, not that every fact is healthy.

Options
  --state-dir STATE_DIR
      Optional existing clean absolute non-root candidate directory, without symlink ancestors.
      Omit for configured live proxy state. Never creates the supplied directory.
  --strict
      Return non-zero for unhealthy, absent or indeterminate results. Default: false.
      Changes exit policy only; collection and displayed evidence are unchanged.
  --timeout DURATION
      Maximum elapsed time including locks, requests and verification. Default: 10s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq proxy health
  cq service status

Examples
  Inspect live readiness:
  cq proxy status
  Require a ready result:
  cq proxy status --strict
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq proxy status
```

Stdout:

```text
Proxy · ready    live
  Observed  0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Elapsed   24ms
  State     /Users/alex/cq-runtime

Evidence
  Inspector · known
    Executable  /opt/homebrew/bin/cq
    Version     1.0.0
    Commit      1234567890abcdef1234567890abcdef12345678
    Digest      1111111111111111111111111111111111111111111111111111111111111111
  Desired · known
    Manager     homebrew
    Configured  yes
    Listener    127.0.0.1:19280
  Service · known
    Manager     homebrew
    State       running
    Executable  /opt/homebrew/bin/cq
    PID         4217
  Listener · known
    State       listening
    Listener    127.0.0.1:19280
    Executable  /opt/homebrew/bin/cq
    PID         4217
  Process · known
    PID         4217
    Executable  /opt/homebrew/bin/cq
  Runtime · known
    Reachable   yes
    PID         4217
    Executable  /opt/homebrew/bin/cq
    Health      healthy
  Data plane · known
    Proven      no
    Code        unproven

Readiness inspection does not establish model-traffic acceptance.
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq proxy status --strict
```

Stdout: empty.

Stderr:

```text
Error · Proxy readiness could not be determined.
  Code: proxy_indeterminate
  Command: cq proxy status
```

Exit: 4.

### Timeout · no complete snapshot

Invocation:

```sh
cq proxy status --timeout 1ms
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: proxy_status_timeout
  Command: cq proxy status
```

Exit: 7.

## `cq service install`

### Help

Invocation:

```sh
cq service install --help
```

Stdout:

```text
cq service install
Install and start selected CQ services.

Usage
  cq service install [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
Creates selected registrations, enables automatic starts and verifies health. Equivalent installed
registrations are idempotent. Proxy starts before token-refresh; refresh must complete one run.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 60s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service status
  cq service uninstall

Examples
  Install both components:
  cq service install
  Install only the shared proxy:
  cq service install --component proxy
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service install
```

Stdout:

```text
CQ services · install    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  idle     yes         yes     —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service install
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service install
```

Exit: 6.

### Partial · proxy started, refresh failed

Both rows are observed final state after incomplete rollback. Proxy remains running; do not summarise this as unchanged.

Invocation:

```sh
cq service install
```

Stdout:

```text
CQ services · install · incomplete    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  failed   yes         no      —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   1
  Error       service_unhealthy
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · failed
```

Stderr:

```text
Error · Service operation was only partially completed; inspect component results.
  Code: service_partial
  Command: cq service install
```

Exit: 8.

## `cq service start`

### Help

Invocation:

```sh
cq service start --help
```

Stdout:

```text
cq service start
Start selected installed CQ services.

Usage
  cq service start [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
Requires existing registrations; never installs missing components. Enables automatic starts,
starts proxy before token-refresh and verifies health. Already healthy/enabled is idempotent.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 60s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service install
  cq service status

Examples
  Start the installed proxy and enable automatic starts:
  cq service start --component proxy
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service start --component proxy
```

Stdout:

```text
CQ services · start    proxy

Component  State    Auto-start  Health  PID
proxy      running  yes         yes     4217

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service start --component proxy
```

Stdout: empty.

Stderr:

```text
Error · Service component proxy is not installed.
  Code: service_not_installed
  Command: cq service start
```

Exit: 3.

### Timeout · selected verification incomplete

This adapter returned no component observations. The manager may have accepted the start; no rollback or unchanged-state claim.

Invocation:

```sh
cq service start --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_start_timeout
  Command: cq service start
```

Exit: 7.

## `cq service restart`

### Help

Invocation:

```sh
cq service restart --help
```

Stdout:

```text
cq service restart
Restart selected installed CQ services.

Usage
  cq service restart [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
Requires existing registrations. May interrupt active Claude/Codex traffic when proxy selected.
Preserves automatic-start policy. A disabled refresh job can complete one requested run without
becoming enabled or scheduled-healthy. Verification must identify the newly completed run.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 60s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service status
  cq service start

Examples
  Run the installed token-refresh job again without changing its enablement:
  cq service restart --component token-refresh
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service restart --component token-refresh
```

Stdout:

```text
CQ services · restart    token-refresh

Component      State  Auto-start  Health  PID
token-refresh  idle   no          no      —

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Requested run completed successfully; automatic starts remain disabled.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service restart --component token-refresh
```

Stdout: empty.

Stderr:

```text
Error · Service component token-refresh is unhealthy.
  Code: service_unhealthy
  Command: cq service restart
```

Exit: 1.

### Timeout · restart completion unknown

Invocation:

```sh
cq service restart --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_restart_timeout
  Command: cq service restart
```

Exit: 7.

## `cq service status`

### Help

Invocation:

```sh
cq service status --help
```

Stdout:

```text
cq service status
Inspect selected CQ service components.

Usage
  cq service status [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
Read-only inspection: no starts, repairs or acceptance model calls. Default exit 0 means inspection
completed. Idle token-refresh is healthy when enabled and its latest completed run succeeded within
35 minutes; it need not remain running. Missing run evidence stays unknown.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --strict
      Default: false. Return non-zero unless every selected component is installed and healthy.
      Proxy must run; enabled token-refresh must have a successful completed run within 35m.
  --timeout DURATION
      Maximum elapsed time including locks, requests and verification. Default: 10s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service start
  cq proxy status

Examples
  Inspect both components:
  cq service status
  Require a healthy proxy:
  cq service status --component proxy --strict
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service status
```

Stdout:

```text
CQ services · status    all

Component      State    Auto-start  Health  PID
proxy          running  yes         yes     4217
token-refresh  idle     yes         yes     —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   unknown
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service status
```

Stdout: empty.

Stderr:

```text
Error · The current-user service manager is unavailable.
  Code: service_unavailable
  Command: cq service status
```

Exit: 4.

### Success · component absent

Inspection succeeded. Missing registration is not an empty table, and no shell-derived roots replace unknown authority.

Invocation:

```sh
cq service status --component token-refresh
```

Stdout:

```text
CQ services · status    token-refresh

Component      State   Auto-start  Health  PID
token-refresh  absent  no          no      —

token-refresh
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure · strict absence

Invocation:

```sh
cq service status --component token-refresh --strict
```

Stdout:

```text
CQ services · status    token-refresh

Component      State   Auto-start  Health  PID
token-refresh  absent  no          no      —

token-refresh
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Rollback · not_needed
```

Stderr:

```text
Error · Service component token-refresh is not installed.
  Code: service_not_installed
  Command: cq service status
```

Exit: 3.

## `cq service stop`

### Help

Invocation:

```sh
cq service stop --help
```

Stdout:

```text
cq service stop
Stop and disable selected CQ services.

Usage
  cq service stop [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
May interrupt active Claude/Codex traffic when proxy selected. Disables automatic starts, then stops
and verifies token-refresh before proxy. Keeps definitions, credentials, configuration, caches and
logs. Already stopped is idempotent. Later quota/account commands do not re-enable services.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 60s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service start
  cq service status

Examples
  Stop and disable both components:
  cq service stop
  Stop only the shared proxy:
  cq service stop --component proxy
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service stop
```

Stdout:

```text
CQ services · stop    all

Component      State    Auto-start  Health  PID
proxy          stopped  no          no      —
token-refresh  stopped  no          no      —

proxy
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    0s ago · 23 Sep 2026, 16:30:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

token-refresh
  Installed   yes
  Manager     launchd
  Owner       cq
  Executable  /opt/homebrew/bin/cq
  Last run    2m ago · 23 Sep 2026, 16:28:00 BST (UTC+01:00)
  Last exit   0
  Error       —
  Config      /Users/alex/Library/Application Support/cq
  State       /Users/alex/Library/Application Support/cq
  Cache       /Users/alex/Library/Caches/cq
  Runtime     /Users/alex/Library/Application Support/cq/runtime
  Logs        /Users/alex/Library/Logs/cq

Selected services stopped or absent; automatic starts disabled where registered.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service stop
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service stop
```

Exit: 6.

### Timeout · stop completion unknown

Invocation:

```sh
cq service stop --component proxy --timeout 1s
```

Stdout: empty.

Stderr:

```text
Error · Operation timed out; inspect state before retrying.
  Code: service_stop_timeout
  Command: cq service stop
```

Exit: 7.

## `cq service uninstall`

### Help

Invocation:

```sh
cq service uninstall --help
```

Stdout:

```text
cq service uninstall
Stop and remove selected CQ service registrations.

Usage
  cq service uninstall [OPTIONS]

CQ services: shared API proxy and token-refresh job. The job runs auth refresh at load and
then every 30 minutes; it does not fetch quota reports. Managers: launchd on macOS,
per-user systemd on Linux, current-user Task Scheduler on Windows. No automatic elevation.
Writes verify ownership. Package-owned services permit start/stop/restart; manual install/uninstall
cannot replace package or foreign ownership. Selected components never widen after validation.
May interrupt active Claude/Codex traffic when proxy selected. Stops/removes token-refresh before
proxy and persists disabled state. Keeps credentials, config, caches, logs, receipts and binary.
Absence is idempotent. Never removes foreign registrations or package-owned definitions manually.

Options
  --component COMPONENT
      all, proxy or token-refresh. Default: all; exact enum.
  --timeout DURATION
      Maximum elapsed time including locks, requests and local verification. Default: 60s.
      Positive Go duration, at most 10m; no bare number. Expiry does not imply rollback.

Global options
  -h, --help     Show this help without reading state.
  -j, --json     Emit JSON; disable terminal prompts. Help remains human text.
  -v, --version  Show CQ version without reading state.

  Boolean flags accept a bare flag, =true or =false; default false.
  Options are not repeatable unless marked. Long/short duplicates are rejected.
  --version --json emits the version envelope; raw stream exceptions are described above.

Related commands
  cq service status
  cq service install

Examples
  Remove only the manually installed proxy registration:
  cq service uninstall --component proxy
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service uninstall --component proxy
```

Stdout:

```text
CQ services · uninstall    proxy

Component  State   Auto-start  Health  PID
proxy      absent  no          no      —

proxy
  Installed   no
  Manager     launchd
  Owner       none
  Executable  unknown
  Last run    unknown
  Last exit   unknown
  Error       —
  Config      unknown
  State       unknown
  Cache       unknown
  Runtime     unknown
  Logs        unknown

Selected registrations absent; credentials and user data retained.
Rollback · not_needed
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service uninstall --component proxy
```

Stdout: empty.

Stderr:

```text
Error · Service ownership conflicts with this operation.
  Code: service_owner_conflict
  Command: cq service uninstall
```

Exit: 6.

## `cq service upgrade`

This public Homebrew operation preserves the released native receipt protocol. It has no component selector or public timeout. Candidate means the executable to verify and retain; service executable means Homebrew’s stable installation link. These examples use synthetic paths, digests, versions and identifiers.

### Help

Invocation:

```sh
cq service upgrade --help
```

Stdout:

```text
cq service upgrade
Upgrade a Homebrew proxy runtime while retaining its listener.

Usage
  cq service upgrade --owner homebrew --candidate-executable ABS_PATH [OPTIONS]

Verify and retain the candidate, stop admitting new work, drain active turns,
and hand over the existing listener. Then update token-refresh registration
and ownership metadata. Requires an existing macOS Homebrew installation.

First adoption of a predecessor without retained-runtime support requires a
maintenance window. Failure or deferral is not proof that no state changed;
inspect cq service status before retrying.

Options
  --owner OWNER
      Required: homebrew. The package authority must already own the installation.
  --candidate-executable ABS_PATH
      Required. Existing candidate CQ executable; use a clean absolute path.
      Its bytes, native build identity and runtime protocol are verified.
  --service-executable ABS_PATH
      Optional stable Homebrew CQ link. Defaults to the running executable path.
      Must be clean and absolute, and resolve to the running executable.

Global options
  -h, --help       Show help without reading state.
  -j, --json       Emit the raw released upgrade receipt, without a v2 envelope.
  -v, --version    Show CQ version without executing the upgrade.

Output compatibility
  Upgrade results keep their released protocol. Human output is one plain line.
  JSON is a native RuntimeUpgradeReceiptV1 document; operational errors use
  stderr and exit 1 even with --json. Syntax errors retain canonical CLI errors.
  --component, --timeout and --installer-lock-held are not public options.

Examples
  cq service upgrade --owner homebrew \
    --candidate-executable /opt/homebrew/bin/cq \
    --service-executable /opt/homebrew/bin/cq
      Verify the installed package and select its retained runtime.
  cq service upgrade --owner homebrew \
    --candidate-executable /opt/homebrew/bin/cq \
    --service-executable /opt/homebrew/bin/cq --json
      Emit the native receipt for automation.
  cq service status
      Inspect selected runtime, actual executable and component health afterwards.
```

Stderr: empty.

Exit: 0.

### Success

Invocation:

```sh
cq service upgrade --owner homebrew --candidate-executable /opt/homebrew/bin/cq --service-executable /opt/homebrew/bin/cq
```

Stdout:

```text
Runtime upgrade: committed (0.34.0 -> 0.34.1)
```

Stderr: empty.

Exit: 0.

### Success · native JSON receipt

Invocation:

```sh
cq service upgrade --owner homebrew --candidate-executable /opt/homebrew/bin/cq --service-executable /opt/homebrew/bin/cq --json
```

Stdout:

```json
{"schema_version":1,"transaction_id":"example-upgrade","generation":1,"phase":"committed","previous":{"path":"/Users/river/.local/state/cq/runtime-artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/cq","version":"0.34.0","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","protocol_version":1},"candidate":{"path":"/Users/river/.local/state/cq/runtime-artifacts/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/cq","version":"0.34.1","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol_version":1},"listener_identity":"tcp|127.0.0.1:19280","supervisor_pid":2142,"admission_pause_nanos":18000000}
```

Stderr: empty.

Exit: 0.

### Failure

Invocation:

```sh
cq service upgrade --owner homebrew --candidate-executable /opt/homebrew/bin/cq --service-executable /opt/homebrew/bin/cq
```

Stdout: empty.

Stderr:

```text
cq: service platform adapter is unavailable
```

Exit: 1.

### Deferred · active turns

Invocation:

```sh
cq service upgrade --owner homebrew --candidate-executable /opt/homebrew/bin/cq --service-executable /opt/homebrew/bin/cq
```

Stdout:

```text
Runtime upgrade: deferred (0.34.0 -> 0.34.1)
```

Stderr:

```text
cq: runtime upgrade deferred; previous runtime remains active: active_turns
```

Exit: 1.

### Deferred · native JSON receipt

Invocation:

```sh
cq service upgrade --owner homebrew --candidate-executable /opt/homebrew/bin/cq --service-executable /opt/homebrew/bin/cq --json
```

Stdout:

```json
{"schema_version":1,"transaction_id":"example-upgrade","generation":1,"phase":"deferred","previous":{"path":"/Users/river/.local/state/cq/runtime-artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/cq","version":"0.34.0","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","protocol_version":1},"candidate":{"path":"/Users/river/.local/state/cq/runtime-artifacts/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/cq","version":"0.34.1","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol_version":1},"listener_identity":"tcp|127.0.0.1:19280","supervisor_pid":2142,"admission_pause_nanos":18000000,"error_code":"active_turns"}
```

Stderr:

```text
cq: runtime upgrade deferred; previous runtime remains active: active_turns
```

Exit: 1.

Native operational output remains plain and unwrapped at every terminal width, including when colour is enabled. Operational failures retain stderr and exit 1 with `--json`; they have no CLI-v2 envelope. A preparation failure has no receipt. A failure after lifecycle entry can emit the released zero-value receipt or empty-phase human line; never fabricate a transaction result. Public syntax errors retain canonical exit 2 diagnostics (a v2 envelope with `--json`), because no native transaction began. Inspect `cq service status` before retrying any failed or deferred transaction.
