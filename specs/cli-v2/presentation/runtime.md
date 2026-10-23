# Runtime and service presentation specification

Status: design draft, not implemented output. Shared [presentation rules](README.md) govern colours, date formats, numbers, stream handling and responsive layout; they take precedence over illustrative wireframe spacing below. Scope: every leaf listed below, including command help and all declared failure variants. Source authority: `commands.json`, `resources/root.md`, `resources/validation.md`, `resources/candidate.md`, and current `cmd/cq/cli_v2_{proxy,service,rescue,diagnostics,validation,canary,hook,candidate,endpoint}.go`. This document changes human presentation only; machine schemas, field values, exit status, ordering promises, side effects and frozen protocol bytes remain unchanged.

## Binding presentation rules

- Bold section headings; cyan command paths, flags and identifiers. Green only for verified healthy state or completed requested mutation. Amber means pending, partial, unavailable evidence or unknown; red means failure. Colour never replaces a word. A nonempty `NO_COLOR`, non-TTY output or `TERM=dumb` disables ANSI on the affected stream. No decorative frames or wide Unicode padding. These redesigned operational layouts apply to terminal output only during initial rollout; redirected plain human output retains existing reference bytes as README.md requires.
- Placeholder braces below mean the actual named resource field, not invented example evidence. Boolean fields render `yes`, `no`, `unknown` for true, false, null respectively; enum values remain literal. Zero is `0`; unknown numeric/date/path values are `unknown`; `—` is reserved for known not-applicable values. Each `|null-label` placeholder below means use this null-reason rule, never infer a zero, now, healthy or empty string. Operational state words must never be inferred from exit 0.
- Use comparison tables only when all labels and values fit under the shared width rules; otherwise use per-record label/value blocks, and then place values on following indented lines when needed. Long paths, opaque IDs and digests wrap visually without ellipsis, abbreviation, inserted characters or changes to copied bytes. Never truncate evidence IDs. Align quantity columns right; IDs, ports, PIDs, generations, schema IDs and exit codes use exact base-10 integers without grouping or units. Counts get labels and comma grouping; elapsed durations follow shared duration units (wireframe ms denotes source unit); sizes retain B.
- Dates follow README.md local-zone format, e.g. `23 Sep 2026, 16:30:05 BST (UTC+01:00)`, with relative age or deadline using one render-time reference for the whole invocation; preserve source fractional seconds when present. Relative time is secondary; exact absolute time is always present. Invalid source date is a validated error, never guessed. No timestamp is invented for a resource that has none.
- Successful human result goes to stdout. Failure heading and exact canonical error message go to stderr, including stable code and exit status. When an adapter returns useful partial data, retain the designed data block and mark it “Observed state · incomplete”; do not synthesise absent fields. Global failures use the shared parser/environment contract. Interrupt exits 130 with `interrupted` and “Operation interrupted; inspect state before retrying.” where supported by the adapter. Cancellation never implies rollback or unchanged state.
- Failure wireframe for every table row below is `Error · {exact Message}`, then secondary `cq COMMAND · CODE · exit N`; retain observed-state block only when returned. Timeout and interruption use their exact canonical messages in that same heading and retain their code/exit. Red failures may contain amber incomplete observations. No invented retry command after an indeterminate mutation.
- Help layout for every leaf follows README.md order: title, summary, usage, essential context, arguments, options, global options, related commands, examples. All support `-h, --help`, `-j, --json`, `-v, --version` with shared meanings. Bare help exits 0 without state access, regardless of `--json`. Repeatability/required metadata becomes concise `[required]`, `[repeatable]` or default text; exact validation is preserved in wrapped subordinate text, not repeated boolean boilerplate. No positional arguments unless enumerated per command.
- Stream exceptions: foreground proxy JSONL and default Codex hook JSON are byte/protocol-preserving, colourless, bannerless. Designed help remains available without starting streams or reading hook stdin.

## `cq codex proxy canary start`

```text
Usage: cq codex proxy canary start [OPTIONS]
```

**User decision:** Is routing observation active, and what evidence has it collected?

**Help lead:** Start a protected observation run of Codex routing.

### Help contract

**Essential context and preconditions:**

- A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.
- HTTP routing enforcement enabled, payload diagnostics disabled, current HTTP readiness marker available.

No positional parameters.

No command-specific options; global options only.

**Effect and scope copy:**

- Creates a protected local observation record for the current exact build/readiness tuple.
- Does not generate traffic or restart the service. The running service must attach the canary; restarting the proxy is a separate explicit action if required.
- Starting when an active run exists is a conflict; an inactive completed record may be replaced by a new run.

**Examples:**

```sh
cq codex proxy canary start
```

### Result design

```text
Codex canary · {active ? active : inactive}
  Run        {state.run_id}
  Finalised  {state.finalised}
  Started    {state.started_at: absolute (relative)}
  Last observed day  {state.last_observed_at: absolute (relative)|null-label}
  Ended      {state.ended_at: absolute (relative)|null-label}

Observations
  Admitted turns                  {state.admitted_turns}
  Keyed mismatches                 {state.keyed_mismatches}
  Protected-state changes         {state.automatic_protected_state_changes}
  Secret leaks                    {state.secret_leaks}
  Unexplained lifecycles           {state.unexplained_lifecycles}
  Live-session repairs            {state.live_session_repairs}
  Protected-state failures        {state.protected_state_failures}
  Consecutive calendar days        {state.consecutive_calendar_days}

Build binding
  CQ build               {state.tuple.cq_build}
  Client build           {state.tuple.client_build}
  Parser schema          {state.tuple.parser_schema}
  Lease schema           {state.tuple.lease_schema}
  Semantics revision     {state.tuple.semantics_revision}
  Retry budget           {state.tuple.retry_budget}
  Fixture SHA-256        {state.tuple.fixture_hash}
  Readiness fingerprint  {state.tuple.readiness_fingerprint}
Protected digests
  {each kind and complete digest}
Finalisation
  {null: not finalised; otherwise:}
  Stop request digest    {state.finalisation.stop_request_digest}
  Process binding digest {state.finalisation.process_binding_digest}
  Counters digest        {state.finalisation.counters_digest}
  Active sessions        {state.finalisation.active_sessions}
```

Last observed day is the UTC calendar-day bucket recorded by the canary, not its last heartbeat or exact event time. Created record does not prove service attached or traffic observed. No synthetic traffic is created. Zero admitted turns is 0, not success evidence.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| canary_missing | 3 | No Codex canary is recorded. | No run recorded |
| canary_precondition_failed | 6 | The Codex canary preconditions are not satisfied. | Active run already exists for start, readiness stale, or protected state changed |
| canary_state_unavailable | 1 | The Codex canary state is unavailable. | State read/write fails |

## `cq codex proxy canary status`

```text
Usage: cq codex proxy canary status [OPTIONS]
```

**User decision:** Is routing observation active, and what evidence has it collected?

**Help lead:** Show the retained Codex routing observation run.

### Help contract

**Essential context and preconditions:**

- A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.
- Existing owner-controlled canary store and readable protected sources.

No positional parameters.

No command-specific options; global options only.

**Effect and scope copy:**

- Reads retained canary state and verifies protected-state digests; never requests a transition.

**Examples:**

```sh
cq codex proxy canary status
```

### Result design

```text
Codex canary · {active ? active : inactive}
  Run        {state.run_id}
  Finalised  {state.finalised}
  Started    {state.started_at: absolute (relative)}
  Last observed day  {state.last_observed_at: absolute (relative)|null-label}
  Ended      {state.ended_at: absolute (relative)|null-label}

Observations
  Admitted turns                  {state.admitted_turns}
  Keyed mismatches                 {state.keyed_mismatches}
  Protected-state changes         {state.automatic_protected_state_changes}
  Secret leaks                    {state.secret_leaks}
  Unexplained lifecycles           {state.unexplained_lifecycles}
  Live-session repairs            {state.live_session_repairs}
  Protected-state failures        {state.protected_state_failures}
  Consecutive calendar days        {state.consecutive_calendar_days}

Build binding
  CQ build               {state.tuple.cq_build}
  Client build           {state.tuple.client_build}
  Parser schema          {state.tuple.parser_schema}
  Lease schema           {state.tuple.lease_schema}
  Semantics revision     {state.tuple.semantics_revision}
  Retry budget           {state.tuple.retry_budget}
  Fixture SHA-256        {state.tuple.fixture_hash}
  Readiness fingerprint  {state.tuple.readiness_fingerprint}
Protected digests
  {each kind and complete digest}
Finalisation
  {null: not finalised; otherwise:}
  Stop request digest    {state.finalisation.stop_request_digest}
  Process binding digest {state.finalisation.process_binding_digest}
  Counters digest        {state.finalisation.counters_digest}
  Active sessions        {state.finalisation.active_sessions}
```

Missing run is an error, never a blank healthy canary. Any nonzero failure counter is red; zero alone does not prove sufficient coverage. Finalisation is reported only from evidence.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| canary_missing | 3 | No Codex canary is recorded. | No run recorded |
| canary_precondition_failed | 6 | The Codex canary preconditions are not satisfied. | Active run already exists for start, readiness stale, or protected state changed |
| canary_state_unavailable | 1 | The Codex canary state is unavailable. | State read/write fails |

## `cq codex proxy canary stop`

```text
Usage: cq codex proxy canary stop [OPTIONS]
```

**User decision:** Is routing observation active, and what evidence has it collected?

**Help lead:** Request a graceful end to the Codex routing observation run.

### Help contract

**Essential context and preconditions:**

- A canary observes real routing and tracks continuity failures and protected credential-state changes. It does not create synthetic traffic or change routing policy.
- Existing owner-controlled canary store and readable protected sources.

No positional parameters.

No command-specific options; global options only.

**Effect and scope copy:**

- Records a stop request; the installed service drains active sessions and finalises the run asynchronously.
- Success means stop requested, not drain complete; inspect status until active=false and finalised=true.
- Repeated stop requests for the same run are idempotent; no forced termination.

**Examples:**

```sh
cq codex proxy canary stop
```

### Result design

```text
Codex canary · {active ? active : inactive}
  Run        {state.run_id}
  Finalised  {state.finalised}
  Started    {state.started_at: absolute (relative)}
  Last observed day  {state.last_observed_at: absolute (relative)|null-label}
  Ended      {state.ended_at: absolute (relative)|null-label}

Observations
  Admitted turns                  {state.admitted_turns}
  Keyed mismatches                 {state.keyed_mismatches}
  Protected-state changes         {state.automatic_protected_state_changes}
  Secret leaks                    {state.secret_leaks}
  Unexplained lifecycles           {state.unexplained_lifecycles}
  Live-session repairs            {state.live_session_repairs}
  Protected-state failures        {state.protected_state_failures}
  Consecutive calendar days        {state.consecutive_calendar_days}

Build binding
  CQ build               {state.tuple.cq_build}
  Client build           {state.tuple.client_build}
  Parser schema          {state.tuple.parser_schema}
  Lease schema           {state.tuple.lease_schema}
  Semantics revision     {state.tuple.semantics_revision}
  Retry budget           {state.tuple.retry_budget}
  Fixture SHA-256        {state.tuple.fixture_hash}
  Readiness fingerprint  {state.tuple.readiness_fingerprint}
Protected digests
  {each kind and complete digest}
Finalisation
  {null: not finalised; otherwise:}
  Stop request digest    {state.finalisation.stop_request_digest}
  Process binding digest {state.finalisation.process_binding_digest}
  Counters digest        {state.finalisation.counters_digest}
  Active sessions        {state.finalisation.active_sessions}
```

Top line adds “Stop requested” rather than “Stopped”. The service drains asynchronously; active=true can remain valid after successful request. Repeat stop is idempotent. Finalised=true must never be inferred from accepted stop.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| canary_missing | 3 | No Codex canary is recorded. | No run recorded |
| canary_precondition_failed | 6 | The Codex canary preconditions are not satisfied. | Active run already exists for start, readiness stale, or protected state changed |
| canary_state_unavailable | 1 | The Codex canary state is unavailable. | State read/write fails |

## `cq codex proxy credential-endpoint legacy activate`

```text
Usage: cq codex proxy credential-endpoint legacy activate --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Activate a reversible replacement credential endpoint.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

No positional parameters.

- `--ticket-file TICKET_FILE` **required**: LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16384 bytes inclusive; no group or other write permission.
- `--confirm-stopped-and-drained` **required**: Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.
  Must be true.
- `--non-interactive`: Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Activate reversible replacement window while retaining exact quarantine for rollback.
- Does not launch shared proxy automatically; operator must start intended owner and verify it before finalise.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy activate --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

**Confirmation variant:** Before the existing typed prompt, show action `activate`, selected proof file, and “Proxy and every credential endpoint participant must remain stopped and drained.” Keep the prompt exactly `Type stopped-and-drained to confirm the proxy remains stopped and drained: `. No success styling before response. JSON or --non-interactive skips typing but never supplies the mandatory semantic confirmation. Waiting for confirmation pauses the operation budget; cancellation reports interruption, not a completed transition.

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Activated means replacement window opened; it does not mean the shared proxy was launched or its traffic verified. Preserve rollback proof. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy credential-endpoint legacy finalise`

```text
Usage: cq codex proxy credential-endpoint legacy finalise --ticket-file TICKET_FILE --confirm-candidate-healthy [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Finalise a verified credential endpoint replacement.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Finalisation removes the rollback path only after exact live-owner verification.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- State activated; exact live candidate owner verification succeeds.

No positional parameters.

- `--ticket-file TICKET_FILE` **required**: LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16384 bytes inclusive; no group or other write permission.
- `--confirm-candidate-healthy` **required**: Assert the exact replacement credential owner passed live health verification. Required; runtime verifier must also confirm.
  Must be true.
- `--non-interactive`: Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Acquire exact live owner verification lease, persist finalising state, then irreversibly finalise and retire quarantine.
- After finalisation rollback is unavailable; replay must use retained ticket identity and existing committed result.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy finalise --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-candidate-healthy --non-interactive
```

**Confirmation variant:** Before the existing typed prompt, show action `finalise`, selected proof file, and “Finalisation retires rollback state after exact live-owner verification.” Keep the prompt exactly `Type candidate-healthy to confirm the exact live candidate passed health verification: `. No success styling before response. JSON or --non-interactive skips typing but never supplies the mandatory semantic confirmation. Waiting for confirmation pauses the operation budget; cancellation reports interruption, not a completed transition.

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Committed is irreversible retirement of rollback state after live-owner verification. Confirmation text must retain that consequence plainly; green committed only after durable success. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy credential-endpoint legacy inspect`

```text
Usage: cq codex proxy credential-endpoint legacy inspect [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Inspect the legacy Codex credential socket and migration state.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.

No positional parameters.

- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Read refused socket identity or existing transition; no mutation and idempotent.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy inspect
```

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Inspection observes legacy_refused snapshot; it is not general socket health. `none` is the documented ticket sentinel, not a missing migration failure. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy credential-endpoint legacy prepare`

```text
Usage: cq codex proxy credential-endpoint legacy prepare --snapshot-file SNAPSHOT_FILE --confirm-stopped-and-drained [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Quarantine a refused legacy credential socket for replacement.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

No positional parameters.

- `--snapshot-file SNAPSHOT_FILE` **required**: LegacyEndpointSnapshotV1 extracted from inspect JSON; required, at most 16384 bytes.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16384 bytes inclusive; no group or other write permission.
- `--confirm-stopped-and-drained` **required**: Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.
  Must be true.
- `--non-interactive`: Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Preserve exact legacy socket in quarantine and persist transition ticket; do not start proxy or credential owner.
- Existing transition is a conflict; use resume with its ticket.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy prepare --snapshot-file "$INPUTS/endpoint-snapshot.json" --confirm-stopped-and-drained --non-interactive
```

**Confirmation variant:** Before the existing typed prompt, show action `prepare`, selected proof file, and “Proxy and every credential endpoint participant must remain stopped and drained.” Keep the prompt exactly `Type stopped-and-drained to confirm the proxy remains stopped and drained: `. No success styling before response. JSON or --non-interactive skips typing but never supplies the mandatory semantic confirmation. Waiting for confirmation pauses the operation budget; cancellation reports interruption, not a completed transition.

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Show prepared/quarantined exactly as returned. Keep ticket identity prominent: subsequent actions require the exact proof. Preparation is not activation. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy credential-endpoint legacy resume`

```text
Usage: cq codex proxy credential-endpoint legacy resume --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Reopen a retained credential endpoint migration.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

No positional parameters.

- `--ticket-file TICKET_FILE` **required**: LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16384 bytes inclusive; no group or other write permission.
- `--confirm-stopped-and-drained` **required**: Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.
  Must be true.
- `--non-interactive`: Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Reopen and validate retained transition; keep its phase, then release maintenance handle.
- Does not activate, finalise or rollback; repeated resume leaves durable state unchanged.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy resume --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

**Confirmation variant:** Before the existing typed prompt, show action `resume`, selected proof file, and “Proxy and every credential endpoint participant must remain stopped and drained.” Keep the prompt exactly `Type stopped-and-drained to confirm the proxy remains stopped and drained: `. No success styling before response. JSON or --non-interactive skips typing but never supplies the mandatory semantic confirmation. Waiting for confirmation pauses the operation budget; cancellation reports interruption, not a completed transition.

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Resumption reports actual journal phase, including historical committing. Do not turn a retained intermediate phase into committed. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy credential-endpoint legacy rollback`

```text
Usage: cq codex proxy credential-endpoint legacy rollback --ticket-file TICKET_FILE --confirm-stopped-and-drained [OPTIONS]
```

**User decision:** What exact credential-endpoint migration state was observed or committed?

**Help lead:** Restore the quarantined legacy credential socket.

### Help contract

**Essential context and preconditions:**

- Maintain the local Codex credential-owner Unix socket, not the HTTP listener. Legacy means a refused socket whose exact filesystem identity must be preserved during migration.
- Supported local Unix credential endpoint implementation; target is the current user CQ state directory plus credential.sock. No arbitrary socket-path option.
- Input proof authenticates and its path, owner and inode identities still match.
- Mandatory confirmation true; noninteractive or exact typed phrase supplied.
- All proxy and credential endpoint participants remain stopped and drained; this is wider than one Codex request.

No positional parameters.

- `--ticket-file TICKET_FILE` **required**: LegacyEndpointTicketV1 extracted from prepare JSON; required, at most 16384 bytes. Preserve unchanged for resume or rollback.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16384 bytes inclusive; no group or other write permission.
- `--confirm-stopped-and-drained` **required**: Assert the proxy and every credential endpoint participant remain stopped and all work drained. Required throughout this operation.
  Must be true.
- `--non-interactive`: Skip the typed confirmation after the mandatory semantic confirmation flag. Default: false. JSON mode implies true; stdin without a TTY requires true.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 1s..5m inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Restore exact preserved legacy socket; refuse if replacement participants are live or finalisation has passed irreversibility.
- Record rolled_back; never create a substitute legacy identity.

**Examples:**

```sh
cq codex proxy credential-endpoint legacy rollback --ticket-file "$INPUTS/endpoint-ticket.json" --confirm-stopped-and-drained --non-interactive
```

**Confirmation variant:** Before the existing typed prompt, show action `rollback`, selected proof file, and “Proxy and every credential endpoint participant must remain stopped and drained.” Keep the prompt exactly `Type stopped-and-drained to confirm the proxy remains stopped and drained: `. No success styling before response. JSON or --non-interactive skips typing but never supplies the mandatory semantic confirmation. Waiting for confirmation pauses the operation budget; cancellation reports interruption, not a completed transition.

### Result design

```text
Credential endpoint · {endpoint.state}
  Path    {endpoint.path}
  Kind    {endpoint.kind}
  Ticket  {endpoint.ticket_id_or_none}

{snapshot: Identity observed; transition: ticket retained}
Use --json to retain the exact structured proof.
```

Show rolled_back only after exact restoration verification. An I/O error or timeout is not evidence restoration happened; retained journal controls recovery. Do not dump signed snapshot/ticket bodies, private material or inode proof as decorative text. Their exact unchanged machine form remains available via --json.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| endpoint_invalid_proof | 2 | Credential endpoint proof is invalid: {safe_reason}. | Proof JSON fails exact schema, size or required fields. |
| endpoint_not_found | 3 | Credential endpoint or transition does not exist. | Required socket or transition does not exist. |
| endpoint_unsupported | 4 | Credential endpoint maintenance is unavailable on this platform. | Platform has no supported maintenance backend. |
| endpoint_conflict | 6 | Credential endpoint migration precondition failed: {safe_reason}. | Snapshot/ticket identity changed, drain condition fails, confirmation absent, live verifier fails or transition phase disallows action. |
| endpoint_timeout | 7 | Credential endpoint operation timed out; inspect migration state before continuing. | Deadline expires; preserved journal determines next safe action. |
| endpoint_io_failed | 1 | Credential endpoint operation failed: {safe_reason}. | Filesystem or owner RPC fails after validation. |

## `cq codex proxy fixture create`

```text
Usage: cq codex proxy fixture create --input INPUT --output OUTPUT [OPTIONS]
```

**User decision:** Was a sanitised fixture created without retaining request content?

**Help lead:** Create a sanitised fixture from a saved Codex request.

### Help contract

**Essential context and preconditions:**

- Reads a previously captured request body from disk. This command does not capture network traffic or validate an installed proxy.
- Stores hashes, metadata classifications and presence flags; never stores prompt text, tokens, raw session identifiers or encrypted state.

No positional parameters.

- `--input INPUT` **required**: Saved request-body file; required. Maximum encoded size: 2 MiB.
  Readable regular file; not stdin; nonempty path; at most 2097152 bytes.
- `--output OUTPUT` **required**: Destination fixture JSON file; required. Existing files are rejected.
  Nonempty path; must not exist; must not alias the input; parent created with mode 0700 when missing.
- `--content-encoding CONTENT_ENCODING`: Body encoding. Default: auto, which recognises Zstandard magic bytes and otherwise uses identity.
  Exact enum; decoded size <= 8388608 bytes; zstd expansion <=128 times encoded size.
- `--metadata-json METADATA_JSON`: Literal JSON turn metadata; not a filename. Default: read metadata from the request body.
  When supplied, parse one FixtureMetadata object; reject trailing JSON and duplicate keys.

**Effect and scope copy:**

- Offline only; no credentials or service access.
- Validate complete input before writing; atomically create output with mode 0600, without replacing an existing file.
- Repeated invocation with the same output fails with conflict; use a new output path.

**Examples:**

```sh
cq codex proxy fixture create --input request.json --output fixture.json --metadata-json '{"session_id":"s1","thread_id":"t1","turn_id":"u1","request_kind":"turn"}'
```

### Result design

```text
Fixture · created
  File               {output_path}
  Request kind       {fixture.request_kind}
  Input size         {fixture.body_bytes} B
  Converted          {fixture.captured_at: absolute (relative)}
  Metadata source    {fixture.metadata_source}
  Session hint       {fixture.session_hint}
  Thread hint        {fixture.thread_hint}
  Turn hint          {fixture.turn_hint}
  Previous response  {fixture.has_previous_response_id}
  Encrypted state    {fixture.has_encrypted_state}
  Body SHA-256       {fixture.body_hash}
  Schema             {fixture.schema_version}
```

Empty/malformed input fails; it is never a zero-byte successful fixture. Existing output is a conflict. Display only hashes/hints and metadata; never raw body, credentials or original identifiers. Conversion time is not original request time.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| fixture_input_invalid | 2 | The request cannot be converted to a sanitised fixture. | Malformed metadata, unsupported encoding or size limit exceeded |
| fixture_output_exists | 6 | The output file already exists. | Output exists |
| fixture_io_failed | 1 | The fixture file could not be read or written. | Read or atomic write fails |

## `cq codex proxy hook stop`

```text
Usage: cq codex proxy hook stop [OPTIONS]
```

**User decision:** Supply the Codex Stop integration with its exact receipt message.

**Help lead:** Return a private-data-safe routing receipt for a Codex Stop hook.

### Help contract

**Essential context and preconditions:**

- Machine integration: read exactly one Codex Stop event JSON object from standard input and look up its turn receipt through authenticated loopback control.
- Default output follows the Codex hook protocol: {} when absent, or {"systemMessage":"..."} when found. With --json, wrap that same object in the canonical CQ envelope for manual inspection; do not use --json in Codex hook configuration.
- stdin is one HookStopEvent object.
- Existing proxy configuration includes local control authentication.

No positional parameters.

No command-specific options; global options only.

**Effect and scope copy:**

- Reads stdin up to 16 MiB; ignores additional event fields without logging or storing them.
- Sends only session_id and turn_id to the existing configured loopback endpoint; 5s fixed timeout, redirects forbidden.
- Does not alter routing, allocate an account, write credentials or create configuration.

**Examples:**

```sh
printf '%s\n' '{"hook_event_name":"Stop","session_id":"s1","turn_id":"u1"}' | cq codex proxy hook stop
```

### Result design

```text
{"systemMessage":"{receipt_message}"}

OR, when no receipt exists:
{}
```

Protocol exception: default stdout is exactly one JSON object plus LF, never both examples, no ANSI, title, spacing changes or banners. No receipt is {} exit 0. Errors leave stdout empty and use stderr. --json is the existing manual diagnostic envelope. Help is ordinary designed help and never reads stdin.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| hook_input_invalid | 2 | Invalid Codex Stop hook input. | Invalid JSON, oversized input, wrong event or invalid identifiers |
| hook_unavailable | 4 | The Codex turn receipt lookup is unavailable. | Configuration or receipt endpoint unavailable |
| hook_auth_failed | 5 | The Codex turn receipt lookup was not authorised. | Control returns HTTP401/403 |
| hook_timeout | 7 | The Codex turn receipt lookup timed out. | 5s elapsed |

## `cq codex proxy validate http`

```text
Usage: cq codex proxy validate http --port PORT [OPTIONS]
```

**User decision:** Was a candidate-only asynchronous validation request accepted?

**Help lead:** Request startup validation of an installed candidate HTTP listener.

### Help contract

**Essential context and preconditions:**

- Requires an installed candidate service on an explicitly selected non-live port. Restarts only that verified candidate service in one-shot validation mode.
- Success means the request was durably recorded and the restart was accepted; it does not mean validation passed. Use cq codex proxy readiness show --client-build BUILD to inspect recorded evidence after startup.
- Candidate service installed on the selected port.
- Installed candidate executable and listener ownership proof available on this platform.

No positional parameters.

- `--port PORT` **required**: Installed candidate loopback port; required. The live port 19280 is forbidden.
  1..65535 except 19280. Must match verified loaded candidate service label, process and listener binding.
- `--timeout TIMEOUT`: Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.
  Go duration, 1s <= value <= 5m. Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

**Effect and scope copy:**

- Durably records one-shot startup validation request and restarts the verified candidate service.
- On restart failure, cancels the request and invalidates affected readiness evidence; reports cleanup failure as operational error.
- Does not wait for validation completion; no implicit retry. Each invocation requests a new validation.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

**Examples:**

```sh
cq codex proxy validate http --port 19281
```

### Result design

```text
HTTP validation · requested
  Candidate port  {port}
  Completion      not yet known

Request accepted; validation has not completed.
Inspect retained readiness evidence before retrying.
```

Amber requested badge; never green passed. No public request ID exists: do not invent polling correlation. An older retained marker cannot prove this request completed. Timeout may leave accepted work running. Unsupported candidate backend exits 4 before request persistence.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| validation_candidate_unavailable | 4 | The installed candidate service is unavailable. | No supported candidate service or attestation |
| validation_candidate_changed | 6 | The candidate service binding changed; no validation was requested. | Candidate binding changed after inspection |
| validation_request_failed | 1 | The HTTP validation request failed. | Persistence, restart or cleanup fails |
| validation_timeout | 7 | The HTTP validation operation timed out; inspect readiness before retrying. | Elapsed operation budget exceeded |

## `cq codex proxy validate websocket`

```text
Usage: cq codex proxy validate websocket --client-build CLIENT_BUILD [OPTIONS]
```

**User decision:** Did every isolated WebSocket validation gate pass for this exact client?

**Help lead:** Validate WebSocket routing with an isolated installed Codex client.

### Help contract

**Essential context and preconditions:**

- Runs the exact installed Codex client against temporary loopback listeners and a synthetic upstream. Checks the executable build before recording readiness evidence.
- Does not restart the configured proxy or send requests to a provider. Success means the isolated exercise passed; it is not live production traffic evidence.
- Matching supported Codex executable available.
- Selected state directory is owner controlled.

No positional parameters.

- `--client-build CLIENT_BUILD` **required**: Expected Codex client version; required. Must match the recorded or exercised client version byte for byte; arbitrary build labels are not accepted.
  Match ^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace. Require byte-for-byte equality with the evidence or executed client version; no whitespace trimming, case folding, prefix matching or arbitrary label substitution.
- `--client-executable CLIENT_EXECUTABLE`: Codex executable to exercise. Default: search ChatGPT.app, Codex.app, then PATH, in that order.
  If supplied, resolve to absolute regular executable file. Default order: /Applications/ChatGPT.app/Contents/Resources/codex, /Applications/Codex.app/Contents/Resources/codex, then executable codex found through PATH. Observed version must equal --client-build.
- `--state-dir STATE_DIR`: Read or write readiness evidence in this absolute directory. Default: the resolved CQ proxy state directory.
  If present, nonempty canonical absolute directory; reject symlink traversal. If omitted, use ResolveDefaultPaths().StateDir; this is not the runtime resilience-state directory.
- `--timeout TIMEOUT`: Maximum elapsed operation time, including preparation, requests and cleanup. Default: 5m. Range: 30s..30m. Reserve the final 5s within this budget for cleanup; interactive confirmation time is excluded.
  Go duration; 30s <= value <=30m. Reserve 5s within the configured timeout for cleanup; the main exercise must stop no later than timeout minus 5s. Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

**Effect and scope copy:**

- Invalidates existing WebSocket readiness before exercise, after lexical/precondition validation.
- Starts temporary local listeners and child Codex processes; closes and drains them on success, failure, interruption and timeout.
- Writes a new readiness marker only after all gates pass; failure leaves readiness invalidated.
- Repeated execution reruns validation and replaces only the selected WebSocket marker.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

**Examples:**

```sh
cq codex proxy validate websocket --client-build 0.146.0 --client-executable /opt/homebrew/bin/codex
```

### Result design

```text
WebSocket validation · passed
  Client build   {client_build}
  Validated      {validated_at: absolute (relative)}
  CQ build       {marker.cq_build}
  Gates passed   {len(marker.completed_gates)}

Evidence
  Version            {marker.version}
  Transport          {marker.transport}
  Parser schema      {marker.parser_schema}
  Lease schema       {marker.lease_schema}
  Semantics revision {marker.semantics_revision}
  Retry budget       {marker.retry_budget}
  Fixture SHA-256    {marker.fixture_hash}
  CQ executable      {marker.cq_executable_sha256|null-label}
  Client executable  {marker.client_executable_sha256|null-label}
  Service identity   {marker.service_identity_sha256|null-label}
  Service kind       {marker.service_kind|null-label}
  Installed result   {marker.installed_result}
  Completed gates
    {one complete marker.completed_gates value per line}
```

Green passed appears only after all gates pass and cleanup completes. Failure invalidates selected WebSocket readiness; never fall back to stale success. Entire marker resource remains visible in Evidence, including null HTTP-only executable/service digests as —. Cleanup receives final 5 seconds inside total budget, not extra time.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| validation_client_unavailable | 4 | The Codex client cannot be validated on this installation. | Executable absent or attestation unsupported |
| validation_build_mismatch | 6 | The Codex executable does not match --client-build. | Version differs |
| validation_failed | 1 | WebSocket validation failed; readiness was not recorded. | Exercise or cleanup fails |
| validation_timeout | 7 | WebSocket validation timed out; readiness was not recorded. | Duration exceeded |

## `cq proxy candidate client-safety refresh`

```text
Usage: cq proxy candidate client-safety refresh --state-dir STATE_DIR --validation-run-id VALIDATION_RUN_ID [OPTIONS]
```

**User decision:** Understand that this reserved capability cannot execute.

**Help lead:** Measure and renew client credential-isolation evidence.

**Availability banner:** Unavailable in this build. No transition or evidence creation is supported.

### Help contract

**Essential context and preconditions:**

- Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
- Measure every registered sender, credential domain and transport. Issue a signed receipt only when no credential-bearing application bytes escaped the authorised runtime boundary.
- The old term client-bearer-barrier describes an internal credential boundary; client-safety names the operator purpose. A declared zero or a hash derived from identifiers is not measured evidence.
- The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.
- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--validation-run-id VALIDATION_RUN_ID` **required**: Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.
  Exactly 64 lowercase hexadecimal characters.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 150s. Allowed: 150s..5m inclusive; 30s reserved for cleanup.
  Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

**Examples:**

```sh
cq proxy candidate client-safety refresh --state-dir "$CANDIDATE" --validation-run-id "$VALIDATION_RUN"
```

### Result design

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  cq proxy candidate client-safety refresh · candidate_evidence_unavailable · exit 4
```

No success, progress, empty-data or partial-mutation view exists. Valid invocation returns exit 4 before state access; no I/O, process transition, receipt or evidence creation. Help must lead with “Unavailable in this build” before syntax. Syntax validation still precedes unsupported result.

**Empty, partial and interrupted variants:** Unavailable is the only operational result; malformed syntax uses global validation errors. No result data is invented.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_evidence_unavailable | 4 | Candidate qualification evidence is unavailable; no transition was performed. | This reserved capability has no complete implemented proof-backend contract. |

## `cq proxy candidate prepare`

```text
Usage: cq proxy candidate prepare --state-dir STATE_DIR --port PORT --source-config SOURCE_CONFIG --target-release-bundle TARGET_RELEASE_BUNDLE --release-digest RELEASE_DIGEST --client-build CLIENT_BUILD --client-executable CLIENT_EXECUTABLE --client-registry CLIENT_REGISTRY [OPTIONS]
```

**User decision:** What local candidate lifecycle state is proven?

**Help lead:** Prepare an isolated release candidate without starting it.

### Help contract

**Essential context and preconditions:**

- Bind the input bytes, client identity, release digest and isolated port to a new candidate state directory. Preparation is local bookkeeping, not release validation.
- Credential and policy attestation files record operator-supplied bytes; they do not import credentials, configure a running proxy or prove read-only access.
- State directory does not contain a prepared candidate; parent is owner-controlled.
- All structured inputs validate before any state is created.
- Manifest/mode/confirmation combinations obey option rules.

No positional parameters.

- `--state-dir STATE_DIR` **required**: New isolated candidate state directory. Required; absolute clean path below a current-user-owned parent. Existing candidate state is a conflict; never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; parent owner-controlled; no existing candidate state.
- `--port PORT` **required**: Loopback listener port reserved for this candidate. Required; 1..65535, excluding shared default port 19280.
  Integer 1..65535; must not equal 19280; must not equal any active shared or candidate listener.
- `--source-config SOURCE_CONFIG` **required**: Source configuration attestation file; required, at most 1048576 bytes. Its byte hash is recorded; contents are not applied.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..1048576 bytes inclusive; no group or other write permission.
- `--target-release-bundle TARGET_RELEASE_BUNDLE` **required**: Target signed OperationalReleaseBundleV1 file; required, purpose target, at most 16777216 bytes.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16777216 bytes inclusive; no group or other write permission.
- `--release-digest RELEASE_DIGEST` **required**: Target bundle digest field, not SHA-256 of the bundle file. Required; must equal the verified target bundle digest.
  Exactly 64 lowercase hexadecimal characters.
- `--client-build CLIENT_BUILD` **required**: Exact client version/build string supplied by executable provenance. Required; must match the independently measured client evidence byte-for-byte. An arbitrary operator label cannot satisfy validation.
  Non-empty UTF-8 string; no NUL or control characters. Must match the client executable provenance and independently measured evidence byte-for-byte; no case folding, trimming, aliasing or inferred version. During release validation must also match the string bound during prepare.
- `--client-executable CLIENT_EXECUTABLE` **required**: Exact client executable whose bytes will be bound to validation; required, absolute regular file, at most 536870912 bytes.
  Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions. File and trusted parent may be owned by current user or root; system-owned executables are allowed. Size 1..536870912 bytes inclusive.
- `--client-registry CLIENT_REGISTRY` **required**: ClientSenderRegistryV1 canonical JSON file; required, at most 65536 bytes. Enumerates all request senders and transports participating in validation.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..65536 bytes inclusive; no group or other write permission.
- `--credential-mode CREDENTIAL_MODE`: Credential attestation mode. Default: none. read-only requires --credential-manifest and --confirm-read-only-credentials; none forbids both.
- `--credential-manifest CREDENTIAL_MANIFEST`: Credential manifest attestation bytes; at most 1048576 bytes. Required only with --credential-mode read-only; never imports or refreshes credentials.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..1048576 bytes inclusive; no group or other write permission.
- `--confirm-read-only-credentials`: Assert that the attested credentials are restricted to read-only access. Required with read-only mode; forbidden with none. This assertion does not authorise credential refresh.
  Explicit false equals omission.
- `--policy-snapshot POLICY_SNAPSHOT`: Optional policy attestation bytes, at most 1048576 bytes. Omission records no policy digest; contents are not applied.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..1048576 bytes inclusive; no group or other write permission.
- `--confirm-payload-capture`: Explicitly authorise capture of request payloads during this candidate workflow. Default: false; omission prohibits capture. Preparation itself captures nothing.
  Explicit false equals omission.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 150s. Allowed: 150s..5m inclusive; 30s reserved for cleanup.
  Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Create a new 0700 candidate directory and 0600 state, key and registry files atomically.
- Generate operation_id (32 lowercase hex), instance_id (32 lowercase hex), and validation_run_id (64 lowercase hex); record input SHA-256 digests and phase prepared.
- Do not start a listener, modify the installed service, import credentials, or apply attested configuration.
- Existing candidate state is a conflict; prepare never overwrites or silently reuses it.

**Examples:**

```sh
cq proxy candidate prepare --state-dir "$CANDIDATE" --port 29280 --source-config "$INPUTS/source-config.bin" --target-release-bundle "$INPUTS/target.json" --release-digest "$RELEASE_DIGEST" --client-build "$CLIENT_BUILD" --client-executable "$CLIENT" --client-registry "$INPUTS/client-registry.json"
```

### Result design

```text
Candidate · {candidate.phase}
  Instance        {candidate.instance_id}
  State           {candidate.state_dir}
  Port            {candidate.port}
  Operation       {candidate.operation_id}
  Validation run  {candidate.validation_run_id}
  Generation      {candidate.generation}
  Updated         {candidate.updated_at: absolute (relative)}
  Pending action  {candidate.pending_action|null-label}
  Effect started  {candidate.effect_started}

Identity and authority
  Client build    {candidate.client_build}
  Credential mode {candidate.credential_mode}
  Payload capture {candidate.payload_capture}
  Attempt         {candidate.attempt_id|null-label}

Digests
  Source config            {candidate.source_config_digest}
  Target bundle file       {candidate.target_bundle_file_digest}
  Target release           {candidate.target_release_digest}
  Active release           {candidate.active_release_digest|null-label}
  Client executable        {candidate.client_executable_digest}
  Client registry file     {candidate.client_registry_digest}
  Credential manifest      {candidate.credential_manifest_digest|null-label}
  Policy snapshot          {candidate.policy_snapshot_digest|null-label}
  Effect receipt           {candidate.effect_receipt_digest|null-label}
  Client safety receipt    {candidate.client_safety_receipt_digest|null-label}
  Validation receipt       {candidate.validation_receipt_digest|null-label}
```

Prepared means local authority files created; no listener started, no credentials imported and no installed service changed.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_not_found | 3 | Candidate state or receipt does not exist. | Candidate state directory or requested receipt does not exist. |
| candidate_conflict | 6 | Candidate state conflicts with this operation. | State identity, generation, phase, validation run or file ownership differs from the required state. |
| candidate_io_failed | 1 | Candidate operation failed: {safe_reason}. | A validated local read, write, process operation or receipt publication fails. |
| candidate_timeout | 7 | Candidate operation timed out; inspect candidate status before retrying. | Total deadline expires; recorded pending state remains inspectable. |

## `cq proxy candidate receipt show`

```text
Usage: cq proxy candidate receipt show --state-dir STATE_DIR --attempt-id ATTEMPT_ID [OPTIONS]
```

**User decision:** Was the retained candidate receipt published or conflicted?

**Help lead:** Show the authenticated receipt for one candidate validation attempt.

### Help contract

**Essential context and preconditions:**

- Look up an exact retained attempt identifier. A receipt reports its recorded result; lookup performs no live validation.
- Requested receipt and its key exist under the candidate state directory.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--attempt-id ATTEMPT_ID` **required**: Exact attempt identifier returned by candidate validation; required, 32 lowercase hexadecimal characters. No latest-attempt fallback.
  Exactly 32 lowercase hexadecimal characters.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 10s. Allowed: 1s..30s inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Read and authenticate retained receipt; repeated lookup is idempotent.

**Examples:**

```sh
cq proxy candidate receipt show --state-dir "$CANDIDATE" --attempt-id "$ATTEMPT_ID"
```

### Result design

```text
Candidate receipt · {receipt.outcome}
  Attempt          {receipt.attempt_id}
  Receipt digest   {receipt.receipt_digest}
  Promotion digest {receipt.promotion_digest}
```

Published describes authenticated retained outcome, not a new validation. Conflicted retains all four fields beside error candidate_receipt_conflicted and exit 6. Missing receipt is an error; no empty receipt card.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_not_found | 3 | Candidate state or receipt does not exist. | Candidate state directory or requested receipt does not exist. |
| candidate_conflict | 6 | Candidate state conflicts with this operation. | State identity, generation, phase, validation run or file ownership differs from the required state. |
| candidate_io_failed | 1 | Candidate operation failed: {safe_reason}. | A validated local read, write, process operation or receipt publication fails. |
| candidate_timeout | 7 | Candidate operation timed out; inspect candidate status before retrying. | Total deadline expires; recorded pending state remains inspectable. |
| candidate_receipt_conflicted | 6 | Candidate receipt is conflicted. | The authenticated retained receipt records outcome conflicted. |

## `cq proxy candidate release activate`

```text
Usage: cq proxy candidate release activate --state-dir STATE_DIR --release-digest RELEASE_DIGEST --validation-run-id VALIDATION_RUN_ID --confirm-artifact-switch [OPTIONS]
```

**User decision:** Understand that this reserved capability cannot execute.

**Help lead:** Activate the prepared runtime release inside a candidate.

**Availability banner:** Unavailable in this build. No transition or evidence creation is supported.

### Help contract

**Essential context and preconditions:**

- Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
- Stop and replace the candidate runtime with the exact signed target artifact set. This does not promote or install the release into the shared service.
- The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.
- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--release-digest RELEASE_DIGEST` **required**: Verified target release digest returned by prepare; required and must match the prepared target.
  Exactly 64 lowercase hexadecimal characters.
- `--validation-run-id VALIDATION_RUN_ID` **required**: Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.
  Exactly 64 lowercase hexadecimal characters.
- `--confirm-artifact-switch` **required**: Authorise replacement of this candidate runtime with the selected release. Required; must be true.
  Must be true.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 90s. Allowed: 90s..2m inclusive; 30s reserved for cleanup.
  Go duration syntax; 90s <= value <= 2m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

**Examples:**

```sh
cq proxy candidate release activate --state-dir "$CANDIDATE" --release-digest "$RELEASE_DIGEST" --validation-run-id "$VALIDATION_RUN" --confirm-artifact-switch
```

### Result design

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  cq proxy candidate release activate · candidate_evidence_unavailable · exit 4
```

No success, progress, empty-data or partial-mutation view exists. Valid invocation returns exit 4 before state access; no I/O, process transition, receipt or evidence creation. Help must lead with “Unavailable in this build” before syntax. Syntax validation still precedes unsupported result.

**Empty, partial and interrupted variants:** Unavailable is the only operational result; malformed syntax uses global validation errors. No result data is invented.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_evidence_unavailable | 4 | Candidate qualification evidence is unavailable; no transition was performed. | This reserved capability has no complete implemented proof-backend contract. |

## `cq proxy candidate release validate`

```text
Usage: cq proxy candidate release validate --state-dir STATE_DIR --target-release-bundle TARGET_RELEASE_BUNDLE --rollback-bundle ROLLBACK_BUNDLE --rollback-receipt ROLLBACK_RECEIPT --rollback-receipt-digest ROLLBACK_RECEIPT_DIGEST --client-build CLIENT_BUILD --client-executable CLIENT_EXECUTABLE --validation-run-id VALIDATION_RUN_ID --receipt-file RECEIPT_FILE --confirm-control-health [OPTIONS]
```

**User decision:** Understand that this reserved capability cannot execute.

**Help lead:** Verify candidate release evidence and write a validation receipt.

**Availability banner:** Unavailable in this build. No transition or evidence creation is supported.

### Help contract

**Essential context and preconditions:**

- Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
- Check exact target artifacts, rollback acceptance, source ancestry, client identity, client isolation, stopped-work proof, control health and runtime confinement.
- Success permits the receipt to be reviewed for promotion; it does not install or deploy the release. File hashes and self-declared ancestry cannot substitute for the corresponding observations.
- The deadline is fixed at 16 minutes, including a 30-second cleanup reserve; there is no timeout option.
- The required live-proof and trust-authority backends are not specified by this CLI contract. Until their independent contracts and implementations exist, this command returns candidate_evidence_unavailable (exit 4) and emits no successful qualification or promotion receipt.
- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--target-release-bundle TARGET_RELEASE_BUNDLE` **required**: Exact target OperationalReleaseBundleV1 used by prepare, at most 16777216 bytes.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16777216 bytes inclusive; no group or other write permission.
- `--rollback-bundle ROLLBACK_BUNDLE` **required**: Accepted rollback OperationalReleaseBundleV1, purpose floor, at most 16777216 bytes. Its source commit must be a strict ancestor of target.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..16777216 bytes inclusive; no group or other write permission.
- `--rollback-receipt ROLLBACK_RECEIPT` **required**: Original rollback acceptance receipt bytes, at most 65536 bytes; verified by the acceptance-proof backend, never fabricated.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled. Regular file; size 1..65536 bytes inclusive; no group or other write permission.
- `--rollback-receipt-digest ROLLBACK_RECEIPT_DIGEST` **required**: SHA-256 of UTF-8 bytes cq/release-import-floor/v1 followed by NUL followed by exact rollback receipt bytes. Required; not an ordinary file SHA-256.
  Exactly 64 lowercase hexadecimal characters.
- `--client-build CLIENT_BUILD` **required**: Exact client version/build string supplied by executable provenance. Required; must match the independently measured client evidence byte-for-byte. An arbitrary operator label cannot satisfy validation.
  Non-empty UTF-8 string; no NUL or control characters. Must match the client executable provenance and independently measured evidence byte-for-byte; no case folding, trimming, aliasing or inferred version. During release validation must also match the string bound during prepare.
- `--client-executable CLIENT_EXECUTABLE` **required**: Exact client executable whose bytes will be bound to validation; required, absolute regular file, at most 536870912 bytes.
  Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions. File and trusted parent may be owned by current user or root; system-owned executables are allowed. Size 1..536870912 bytes inclusive.
- `--validation-run-id VALIDATION_RUN_ID` **required**: Exact validation run identifier returned by candidate prepare or status. Required; 64 lowercase hexadecimal characters. An identifier, not a file hash; must match the stored candidate.
  Exactly 64 lowercase hexadecimal characters.
- `--receipt-file RECEIPT_FILE` **required**: New absolute output file for CandidateReleasePromotionReceiptV1. Must not exist; parent must be owner-controlled.
  Absolute, lexically clean output path; not /; leaf must not exist, including as a symbolic link. Parent must already exist as a current-user-owned, owner-controlled directory; validate and hold its identity through atomic creation. Create a new regular file with mode 0600; never overwrite or follow an existing leaf.
- `--confirm-control-health` **required**: Assert the exact candidate passed control-health verification. Required; CQ also verifies live identity and health itself.
  Must be true.

**Effect and scope copy:**

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

**Examples:**

```sh
cq proxy candidate release validate --state-dir "$CANDIDATE" --target-release-bundle "$INPUTS/target.json" --rollback-bundle "$INPUTS/floor.json" --rollback-receipt "$INPUTS/floor-receipt.bin" --rollback-receipt-digest "$ROLLBACK_DIGEST" --client-build "$CLIENT_BUILD" --client-executable "$CLIENT" --validation-run-id "$VALIDATION_RUN" --receipt-file "$INPUTS/promotion.json" --confirm-control-health
```

### Result design

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  cq proxy candidate release validate · candidate_evidence_unavailable · exit 4
```

No success, progress, empty-data or partial-mutation view exists. Valid invocation returns exit 4 before state access; no I/O, process transition, receipt or evidence creation. Help must lead with “Unavailable in this build” before syntax. Syntax validation still precedes unsupported result.

**Empty, partial and interrupted variants:** Unavailable is the only operational result; malformed syntax uses global validation errors. No result data is invented.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_evidence_unavailable | 4 | Candidate qualification evidence is unavailable; no transition was performed. | This reserved capability has no complete implemented proof-backend contract. |

## `cq proxy candidate remove`

```text
Usage: cq proxy candidate remove --state-dir STATE_DIR --confirm-candidate-state-loss [OPTIONS]
```

**User decision:** What local candidate lifecycle state is proven?

**Help lead:** Delete stopped candidate state and retained receipts.

### Help contract

**Essential context and preconditions:**

- Permanently remove only this owned candidate state directory. Export any needed receipts first. Client and runtime absence must be verified independently of lifecycle phase.
- The deadline is fixed at 30 seconds, including a 15-second cleanup reserve; there is no timeout option.
- Phase prepared, stopped or validated; no process or listener remains.
- Candidate-state-loss confirmation true; directory identity still matches.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--confirm-candidate-state-loss` **required**: Authorise permanent deletion of this candidate directory, keys and retained receipts. Required; must be true.
  Must be true.

**Effect and scope copy:**

- Prove associated process/listener absent, then delete only identity-matched owned files and directory.
- Return final CandidateStatusV2 with phase removed before discarding state.
- Missing directory returns not found; never treats an arbitrary directory as removable candidate state.

**Examples:**

```sh
cq proxy candidate remove --state-dir "$CANDIDATE" --confirm-candidate-state-loss
```

### Result design

```text
Candidate · {candidate.phase}
  Instance        {candidate.instance_id}
  State           {candidate.state_dir}
  Port            {candidate.port}
  Operation       {candidate.operation_id}
  Validation run  {candidate.validation_run_id}
  Generation      {candidate.generation}
  Updated         {candidate.updated_at: absolute (relative)}
  Pending action  {candidate.pending_action|null-label}
  Effect started  {candidate.effect_started}

Identity and authority
  Client build    {candidate.client_build}
  Credential mode {candidate.credential_mode}
  Payload capture {candidate.payload_capture}
  Attempt         {candidate.attempt_id|null-label}

Digests
  Source config            {candidate.source_config_digest}
  Target bundle file       {candidate.target_bundle_file_digest}
  Target release           {candidate.target_release_digest}
  Active release           {candidate.active_release_digest|null-label}
  Client executable        {candidate.client_executable_digest}
  Client registry file     {candidate.client_registry_digest}
  Credential manifest      {candidate.credential_manifest_digest|null-label}
  Policy snapshot          {candidate.policy_snapshot_digest|null-label}
  Effect receipt           {candidate.effect_receipt_digest|null-label}
  Client safety receipt    {candidate.client_safety_receipt_digest|null-label}
  Validation receipt       {candidate.validation_receipt_digest|null-label}
```

Removed is final result captured before directory discard; it does not imply installation removal. Require exact identity and absence proof; arbitrary/missing directory is not successful removal.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_not_found | 3 | Candidate state or receipt does not exist. | Candidate state directory or requested receipt does not exist. |
| candidate_conflict | 6 | Candidate state conflicts with this operation. | State identity, generation, phase, validation run or file ownership differs from the required state. |
| candidate_io_failed | 1 | Candidate operation failed: {safe_reason}. | A validated local read, write, process operation or receipt publication fails. |
| candidate_timeout | 7 | Candidate operation timed out; inspect candidate status before retrying. | Total deadline expires; recorded pending state remains inspectable. |

## `cq proxy candidate start`

```text
Usage: cq proxy candidate start --state-dir STATE_DIR [OPTIONS]
```

**User decision:** Understand that this reserved capability cannot execute.

**Help lead:** Start the exact release bound to an isolated candidate.

**Availability banner:** Unavailable in this build. No transition or evidence creation is supported.

### Help contract

**Essential context and preconditions:**

- Unavailable in this specification: real qualification evidence backend is not defined. Valid syntax returns exit 4 without state access or transitions.
- Launch only the prepared release and verify its identity before returning success. A substitute health-only process is not a valid candidate runtime.
- The required target-artifact or measured-evidence backend lacks a complete independent contract. Until that contract and implementation exist, return candidate_evidence_unavailable (exit 4); the existing control-server substitute cannot qualify as success.
- Parse and validate syntax only. Do not open state, acquire locks or inspect supplied files before returning unavailable.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 30s. Allowed: 30s..90s inclusive; 15s reserved for cleanup.
  Go duration syntax; 30s <= value <= 90s; cleanup reserve 15s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Return candidate_evidence_unavailable, exit 4. Do not start, stop, switch or qualify any process; do not create evidence or receipts.

**Examples:**

```sh
cq proxy candidate start --state-dir "$CANDIDATE"
```

### Result design

```text
Error · Candidate qualification evidence is unavailable; no transition was performed.
  cq proxy candidate start · candidate_evidence_unavailable · exit 4
```

No success, progress, empty-data or partial-mutation view exists. Valid invocation returns exit 4 before state access; no I/O, process transition, receipt or evidence creation. Help must lead with “Unavailable in this build” before syntax. Syntax validation still precedes unsupported result.

**Empty, partial and interrupted variants:** Unavailable is the only operational result; malformed syntax uses global validation errors. No result data is invented.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_evidence_unavailable | 4 | Candidate qualification evidence is unavailable; no transition was performed. | This reserved capability has no complete implemented proof-backend contract. |

## `cq proxy candidate status`

```text
Usage: cq proxy candidate status --state-dir STATE_DIR [OPTIONS]
```

**User decision:** What local candidate lifecycle state is proven?

**Help lead:** Show retained state for one isolated candidate.

### Help contract

**Essential context and preconditions:**

- Read lifecycle phase, immutable input bindings and pending action. A running phase is retained state, not a fresh traffic or health test.
- Candidate state exists and authenticates under its owned key.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--timeout TIMEOUT`: Total deadline, including cleanup. Default: 10s. Allowed: 1s..30s inclusive; 0s reserved for cleanup.
  Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.

**Effect and scope copy:**

- Read state without starting, repairing or validating a runtime.
- Repeated invocation is idempotent.

**Examples:**

```sh
cq proxy candidate status --state-dir "$CANDIDATE"
```

### Result design

```text
Candidate · {candidate.phase}
  Instance        {candidate.instance_id}
  State           {candidate.state_dir}
  Port            {candidate.port}
  Operation       {candidate.operation_id}
  Validation run  {candidate.validation_run_id}
  Generation      {candidate.generation}
  Updated         {candidate.updated_at: absolute (relative)}
  Pending action  {candidate.pending_action|null-label}
  Effect started  {candidate.effect_started}

Identity and authority
  Client build    {candidate.client_build}
  Credential mode {candidate.credential_mode}
  Payload capture {candidate.payload_capture}
  Attempt         {candidate.attempt_id|null-label}

Digests
  Source config            {candidate.source_config_digest}
  Target bundle file       {candidate.target_bundle_file_digest}
  Target release           {candidate.target_release_digest}
  Active release           {candidate.active_release_digest|null-label}
  Client executable        {candidate.client_executable_digest}
  Client registry file     {candidate.client_registry_digest}
  Credential manifest      {candidate.credential_manifest_digest|null-label}
  Policy snapshot          {candidate.policy_snapshot_digest|null-label}
  Effect receipt           {candidate.effect_receipt_digest|null-label}
  Client safety receipt    {candidate.client_safety_receipt_digest|null-label}
  Validation receipt       {candidate.validation_receipt_digest|null-label}
```

Phase is retained lifecycle state, not process liveness or health. Keep pending action and effect started visible above digests. Inspection never repairs or qualifies.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_not_found | 3 | Candidate state or receipt does not exist. | Candidate state directory or requested receipt does not exist. |
| candidate_conflict | 6 | Candidate state conflicts with this operation. | State identity, generation, phase, validation run or file ownership differs from the required state. |
| candidate_io_failed | 1 | Candidate operation failed: {safe_reason}. | A validated local read, write, process operation or receipt publication fails. |
| candidate_timeout | 7 | Candidate operation timed out; inspect candidate status before retrying. | Total deadline expires; recorded pending state remains inspectable. |

## `cq proxy candidate stop`

```text
Usage: cq proxy candidate stop --state-dir STATE_DIR --confirm-client-stopped [OPTIONS]
```

**User decision:** What local candidate lifecycle state is proven?

**Help lead:** Stop an isolated candidate after its client has stopped.

### Help contract

**Essential context and preconditions:**

- Confirm every client using this candidate has stopped before stopping its runtime. Confirmation is mandatory in both human and JSON modes.
- A validated candidate can still be running; stop accepts that phase and verifies process absence.
- The deadline is fixed at 30 seconds, including a 15-second cleanup reserve; there is no timeout option.
- Phase running or validated; an exact candidate runtime remains.
- Client stopped confirmation true; runtime identity matches stored candidate.

No positional parameters.

- `--state-dir STATE_DIR` **required**: Candidate state directory created by prepare. Required; absolute, clean, current-user-owned path. Never defaults to shared proxy state.
  Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.
- `--confirm-client-stopped` **required**: Assert that every client using this candidate has stopped and no request remains active. Required; must be true.
  Must be true.

**Effect and scope copy:**

- Stop only the identity-matched runtime; prove process/listener absence before phase stopped.
- Already stopped is a phase conflict; no implicit retries or second stop.

**Examples:**

```sh
cq proxy candidate stop --state-dir "$CANDIDATE" --confirm-client-stopped
```

### Result design

```text
Candidate · {candidate.phase}
  Instance        {candidate.instance_id}
  State           {candidate.state_dir}
  Port            {candidate.port}
  Operation       {candidate.operation_id}
  Validation run  {candidate.validation_run_id}
  Generation      {candidate.generation}
  Updated         {candidate.updated_at: absolute (relative)}
  Pending action  {candidate.pending_action|null-label}
  Effect started  {candidate.effect_started}

Identity and authority
  Client build    {candidate.client_build}
  Credential mode {candidate.credential_mode}
  Payload capture {candidate.payload_capture}
  Attempt         {candidate.attempt_id|null-label}

Digests
  Source config            {candidate.source_config_digest}
  Target bundle file       {candidate.target_bundle_file_digest}
  Target release           {candidate.target_release_digest}
  Active release           {candidate.active_release_digest|null-label}
  Client executable        {candidate.client_executable_digest}
  Client registry file     {candidate.client_registry_digest}
  Credential manifest      {candidate.credential_manifest_digest|null-label}
  Policy snapshot          {candidate.policy_snapshot_digest|null-label}
  Effect receipt           {candidate.effect_receipt_digest|null-label}
  Client safety receipt    {candidate.client_safety_receipt_digest|null-label}
  Validation receipt       {candidate.validation_receipt_digest|null-label}
```

Stopped requires process/listener absence proof. Already stopped is phase conflict, not fabricated idempotence. No automatic retry of pending effects.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| candidate_not_found | 3 | Candidate state or receipt does not exist. | Candidate state directory or requested receipt does not exist. |
| candidate_conflict | 6 | Candidate state conflicts with this operation. | State identity, generation, phase, validation run or file ownership differs from the required state. |
| candidate_io_failed | 1 | Candidate operation failed: {safe_reason}. | A validated local read, write, process operation or receipt publication fails. |
| candidate_timeout | 7 | Candidate operation timed out; inspect candidate status before retrying. | Total deadline expires; recorded pending state remains inspectable. |

## `cq proxy health`

```text
Usage: cq proxy health [OPTIONS]
```

**User decision:** Is this listener responding to its health endpoint?

**Help lead:** Probe proxy HTTP liveness.

### Help contract

**Essential context and preconditions:**

- Request /health from one loopback listener. A successful probe establishes HTTP liveness only; it does not prove routing, account continuity or installed-client validation.
- Read configuration only when --port omitted.

No positional parameters.

- `--port PORT`: Loopback port, 1–65535. Omit to use configured port or 19280 without creating config.
  Integer 1–65535.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 5s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- GET /health, no redirects, maximum response 1 MiB; never expose response tokens or raw unexpected content.

**Examples:**

```sh
cq proxy health --port 29280
```

### Result design

```text
Proxy health · {healthy: healthy|failed}
  Address    {address}
  HTTP       {http_status|null-label}
  Reachable  {reachable}
  Elapsed    {duration_ms} ms

Health endpoint only; model traffic not verified.
```

Unreachable: keep address and elapsed time, HTTP unknown (no response). Never show HTTP 0. A 2xx response with invalid/non-ok body is failed, not healthy.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| proxy_health_failed | 1 | Proxy HTTP health check failed. | Response received but status not healthy. |
| proxy_unreachable | 4 | Proxy is not reachable at {address}. | Listener not reachable. |
| proxy_health_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq proxy operation status`

```text
Usage: cq proxy operation status [OPERATION_ID] [OPTIONS]
```

**User decision:** Is an operation pending, or is a retained result available?

**Help lead:** Show a retained shared proxy operation record.

### Help contract

**Essential context and preconditions:**

- An operation is a durable coordinator record for a shared runtime transition. Without an ID, inspect the currently selected coordinator record; no record returns state=idle.
- This command never performs recovery. Result availability means a retained result exists, not that its underlying action succeeded.

- Positional `operation-id`: Operation ID to inspect. Default: the currently selected coordinator record. Required: False. If supplied, exactly 32 lowercase hexadecimal characters.
No command-specific options; global options only.

**Effect and scope copy:**

- Read-only existing resilience state; does not create directories, restart processes or resume operations.
- Pending operations are valid status results and exit 0; absent explicitly requested IDs exit 3.

**Examples:**

```sh
cq proxy operation status
cq proxy operation status 0123456789abcdef0123456789abcdef
```

### Result design

```text
Proxy operation · {state}
  Operation         {operation_id|idle}
  Phase             {phase|null-label}
  Result available  {result_available}
  Result digest     {result_digest|null-label}
  Recovery support  unavailable
```

Idle is a valid empty state: ID idle, phase/digest —, result unavailable. Explicit missing ID fails. Pending is amber but inspection may exit 0. Result available is neutral: a retained receipt is not proof of successful action.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| operation_not_found | 3 | The requested operation does not exist. | Explicit ID missing |
| operation_state_unavailable | 4 | The operation state is unavailable. | State corrupt or unreadable |

## `cq proxy rescue enter`

```text
Usage: cq proxy rescue enter [OPTIONS]
```

**User decision:** Which traffic mode is active, and is draining still in progress?

**Help lead:** Request shared proxy rescue traffic mode.

### Help contract

**Essential context and preconditions:**

- Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
- Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.
- Existing rescue bootstrap control credentials and reachable supervisor.
- For exit, admitted normal worker available; for enter, rescue handler and durable mode state available.

No positional parameters.

- `--port PORT`: Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.
  Integer 1..65535; omission resolves existing bootstrap configuration without creating it.
- `--timeout TIMEOUT`: Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.
  Go duration, 1s <= value <= 5m. Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

**Effect and scope copy:**

- Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- Repeated enter is idempotent when the same transition is already requested.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

**Examples:**

```sh
cq proxy rescue enter
```

### Result design

```text
Proxy rescue · {mode}
  Generation       {generation}
  Active requests  {active_rescue_requests}
  Draining sessions {len(draining_sessions)}

{each privacy-safe draining session hint on its own line}
```

Entry acknowledgement may mean rescue_draining, not rescue. Show pending drain amber. Repeated same intent is idempotent. Counts must come from returned state. Timeout does not trigger retry or rollback.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| rescue_unavailable | 4 | Proxy rescue control is unavailable. | Supervisor or configured control unavailable |
| rescue_auth_failed | 5 | Proxy rescue control was not authorised. | HTTP401/403 |
| rescue_transition_conflict | 6 | The proxy cannot perform this rescue transition. | Supervisor refuses transition |
| rescue_timeout | 7 | The proxy rescue operation timed out; inspect status before retrying. | Elapsed operation budget exceeded |
| rescue_response_invalid | 1 | Proxy rescue control returned an invalid response. | Malformed or >64KiB response |

## `cq proxy rescue exit`

```text
Usage: cq proxy rescue exit [OPTIONS]
```

**User decision:** Which traffic mode is active, and is draining still in progress?

**Help lead:** Request return to normal proxy traffic.

### Help contract

**Essential context and preconditions:**

- Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
- Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.
- Existing rescue bootstrap control credentials and reachable supervisor.
- For exit, admitted normal worker available; for enter, rescue handler and durable mode state available.

No positional parameters.

- `--port PORT`: Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.
  Integer 1..65535; omission resolves existing bootstrap configuration without creating it.
- `--timeout TIMEOUT`: Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.
  Go duration, 1s <= value <= 5m. Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

**Effect and scope copy:**

- Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- Repeated exit is idempotent when the same transition is already requested.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

**Examples:**

```sh
cq proxy rescue exit
```

### Result design

```text
Proxy rescue · {mode}
  Generation       {generation}
  Active requests  {active_rescue_requests}
  Draining sessions {len(draining_sessions)}

{each privacy-safe draining session hint on its own line}
```

Exit acknowledgement may mean rescue_exit_draining; old rescue work can remain. Do not announce drain complete until observed. New normal traffic does not prove old requests finished. Counts must come from returned state. Timeout does not trigger retry or rollback.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| rescue_unavailable | 4 | Proxy rescue control is unavailable. | Supervisor or configured control unavailable |
| rescue_auth_failed | 5 | Proxy rescue control was not authorised. | HTTP401/403 |
| rescue_transition_conflict | 6 | The proxy cannot perform this rescue transition. | Supervisor refuses transition |
| rescue_timeout | 7 | The proxy rescue operation timed out; inspect status before retrying. | Elapsed operation budget exceeded |
| rescue_response_invalid | 1 | Proxy rescue control returned an invalid response. | Malformed or >64KiB response |

## `cq proxy rescue status`

```text
Usage: cq proxy rescue status [OPTIONS]
```

**User decision:** Which traffic mode is active, and is draining still in progress?

**Help lead:** Show shared proxy rescue and drain state.

### Help contract

**Essential context and preconditions:**

- Rescue is a supervisor-wide fallback mode; its control affects the shared listener, not only Codex.
- Transitions may still be draining when this command returns. Inspect status to distinguish normal, rescue_draining, rescue and rescue_exit_draining.
- Existing rescue bootstrap control credentials and reachable supervisor.

No positional parameters.

- `--port PORT`: Loopback control port. Default: the configured rescue bootstrap port, or 19280 when its value is zero.
  Integer 1..65535; omission resolves existing bootstrap configuration without creating it.
- `--timeout TIMEOUT`: Maximum elapsed operation time, including preparation, requests and cleanup. Default: 30s. Range: 1s..5m. Interactive confirmation time is excluded.
  Go duration, 1s <= value <= 5m. Use one monotonic deadline from operation preparation through cleanup; nested requests cannot extend it.

**Effect and scope copy:**

- Authenticated loopback read only; no configuration or mode mutation.
- Repeated inspection does not change state.
- The timeout covers preparation, all requests and cleanup as one elapsed operation budget; it excludes only interactive confirmation time.

**Examples:**

```sh
cq proxy rescue status
```

### Result design

```text
Proxy rescue · {mode}
  Generation       {generation}
  Active requests  {active_rescue_requests}
  Draining sessions {len(draining_sessions)}

{each privacy-safe draining session hint on its own line}
```

Read-only observation. Mode normal is not a missing result; zero counts are shown as 0, and an empty session list is “No draining sessions”. Counts must come from returned state. Timeout does not trigger retry or rollback.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| rescue_unavailable | 4 | Proxy rescue control is unavailable. | Supervisor or configured control unavailable |
| rescue_auth_failed | 5 | Proxy rescue control was not authorised. | HTTP401/403 |
| rescue_transition_conflict | 6 | The proxy cannot perform this rescue transition. | Supervisor refuses transition |
| rescue_timeout | 7 | The proxy rescue operation timed out; inspect status before retrying. | Elapsed operation budget exceeded |
| rescue_response_invalid | 1 | Proxy rescue control returned an invalid response. | Malformed or >64KiB response |

## `cq proxy serve`

```text
Usage: cq proxy serve [OPTIONS]
```

**User decision:** Has the foreground listener started, and when has it stopped?

**Help lead:** Run the shared API proxy in the foreground.

### Help contract

**Essential context and preconditions:**

- Serve Claude and Codex requests on the configured loopback port until interrupted. This starts no operating-system service.
- A live port override applies to this process only. Existing clients are unaffected until they connect to this listener.
- Validate config, credential ownership and port before binding. Configuration absence permits explicit creation because serve is a mutation command.
- Do not replace, signal or steal another listener.

No positional parameters.

- `--port PORT`: Loopback listen port, 1–65535. Omit to use proxy.json port, or 19280 if absent.
  No ephemeral port 0; reject a listener owned by another process.
- `--migrate-legacy-managed`: Add missing routing identity metadata to legacy CQ-managed credentials before serving. Default: false.
  Never changes external or system-owned credentials.

**Effect and scope copy:**

- Create missing CQ proxy config with a new local token using owner-only atomic writes; never print token.
- Bind loopback only, run until SIGINT/SIGTERM, drain owned requests and close owned listener.
- No service registration or automatic daemonisation. Migration is explicit, idempotent and restricted to CQ-managed records.
- Streaming command: writes ready event only after bind and runtime initialisation; ready means accepting requests, not installed-client qualification.

**Examples:**

```sh
cq proxy serve
cq proxy serve --port 29280
```

### Result design

```text
Proxy · listening
  Address    {listen_address}
  PID        {pid}
  Providers  {providers joined by comma}

Ctrl-C stops this foreground process.

Proxy · stopped
  Reason     {reason|null-label}
```

Ready block appears only after bind and runtime initialisation; stopped block only after observed shutdown. No moving spinner or fabricated percentage. SIGINT exits 130; SIGTERM follows existing successful termination contract. A ready event does not establish installed-client qualification. JSON mode remains the frozen ready/stopped JSONL stream, not a styled envelope.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| proxy_port_in_use | 6 | Port {port} is already in use. | Requested address already has a listener. |
| proxy_config_invalid | 6 | Proxy configuration is invalid: {reason}. | Existing configuration cannot safely start. |

## `cq proxy state initialise`

```text
Usage: cq proxy state initialise --state-dir DIR [OPTIONS]
```

**User decision:** Was state created or reused, and must I restart the proxy?

**Help lead:** Initialise shared authenticated proxy state.

### Help contract

**Essential context and preconditions:**

- Create the shared runtime-control state directory and record its location in proxy configuration. This is shared infrastructure, not a Codex account pool.
- This command does not create pools or bind sessions. Use codex proxy policy, pool and session commands for Codex routing.
- Existing service/config ownership must belong to current user.
- Same already-configured authenticated directory succeeds idempotently. Different configured nonempty root is conflict; this command does not migrate it.

No positional parameters.

- `--state-dir DIR` **required**: Absolute directory to own as shared proxy runtime-control state.
  Required clean absolute non-root path; reject symlink path components and existing foreign/incompatible state.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 30s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Create state owner-only, publish authenticated initial records atomically, set proxy_resilience_state_dir in config.
- Do not restart service automatically. Output restart_required=true iff running service has not adopted state.

**Examples:**

```sh
cq proxy state initialise --state-dir "$HOME/.config/cq/runtime"
```

### Result design

```text
Proxy state · {created ? created : existing}
  Directory         {state_dir}
  Restart required  {restart_required}
```

Existing state is not an error when safely reused. Created must reflect durable creation. Partial creation/binding reports known directory and created value alongside failure; unknown adoption must not be guessed. Restart required is amber; never restart automatically.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| proxy_state_conflict | 6 | Proxy state ownership conflicts with the requested directory. | Directory owned by another instance or config refers to another initialised root. |
| proxy_state_initialise_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |
| proxy_state_initialise_partial | 8 | Proxy state initialisation partially completed; inspect state before retrying. | A positive authority-creation or configuration-binding mutation receipt is followed by a later failure, without deadline expiry or interruption. Retain the known outcome. |

## `cq proxy status`

```text
Usage: cq proxy status [OPTIONS]
```

**User decision:** Do service, process, listener and runtime observations agree?

**Help lead:** Inspect shared proxy readiness evidence.

### Help contract

**Essential context and preconditions:**

- Inspect configured service, process, listener, runtime and data-plane evidence without mutating state. This does not send model requests or create new validation evidence. Inspect provider routing policy and retained transport readiness through their dedicated commands.
- Human and JSON output describe exactly the same inspection. Use proxy health for the older, narrower HTTP liveness probe.
- No configuration or state creation.

No positional parameters.

- `--state-dir DIR`: Inspect an isolated candidate state directory. Omit to inspect configured live proxy state.
  When supplied: existing clean absolute non-root directory; no symlink ancestors; never create it.
- `--strict`: Return non-zero when inspected proxy is unhealthy, absent or indeterminate. Default: false.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 10s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Collect inspection facts from explicit candidate root or default live configuration, service manager and loopback runtime.
- Default exit0 means inspection completed; inspect data.state for readiness. --strict changes exit policy only, never inspection scope.
- No --port accepted: explicit root/config identifies coherent process evidence. Use proxy health --port for arbitrary liveness.

**Examples:**

```sh
cq proxy status
cq proxy status --strict --json
```

### Result design

```text
Proxy · {state}    {scope}
  Observed  {collected_at: absolute (relative)}
  Elapsed   {duration_ms} ms
  State     {state_dir|null-label}

Evidence
  Inspector   {facts[inspector].state}   {typed inspector values}
  Desired     {facts[desired].state}     {typed desired values}
  Service     {facts[service].state}     {typed service values}
  Listener    {facts[listener].state}    {typed listener values}
  Process     {facts[process].state}     {typed process values}
  Runtime     {facts[runtime].state}     {typed runtime values}
  Data plane  {facts[data_plane].state}  {proven; code}
```

Absent is an observed state, not a fabricated empty success. Default exit 0 means inspection completed; strict mode changes exit policy only. Indeterminate is amber, degraded red/amber per constituent failures. Never infer ready from health alone. Every typed fact value from root.md is shown as labelled subordinate fields; do not print compact JSON as prose.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| proxy_unhealthy | 1 | Proxy readiness is degraded. | --strict and state degraded. |
| proxy_absent | 3 | No ready proxy is running. | --strict and state absent or stopped. |
| proxy_indeterminate | 4 | Proxy readiness could not be determined. | --strict and state indeterminate. |
| proxy_status_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## Shared service runtime evidence

This conditional block applies to all six public service commands below, including retained observations after a failure. Fields are defined in [ServiceOperationV2 and ServiceComponentV2](../resources/root.md#serviceoperationv2). No extra field appears when the corresponding optional resource value is absent; existing no-metadata layouts remain unchanged.

After the component table and before the individual component details, render the available recorded runtime metadata in this exact order:

```text
Active runtime  {active_runtime_version}
Pending runtime {pending_runtime_version}
Upgrade         {upgrade_phase}
```

The active version is cyan identity text. The pending version is amber. Upgrade phase is green only for committed, red for failed, and amber for every other phase. A recorded active version or committed phase is not proof of a running or healthy service; the component table retains its observed state and health. Omit the pending line for committed, rolled_back and failed. Deferred retains the pending candidate without claiming current execution.

In each component detail block, after Logs, append `Live binary {live_executable}` only when a live path is available and differs from Executable (or the configured path is unknown). Append `Listener {listener}` whenever a live listener is observed. Live binary paths use ordinary body text; listener addresses are cyan identity text. Executable always means the native definition's configured path. Never relabel it as the live process. JSON includes a known live executable even when it equals the configured path. Stopped or uninstalled components omit both live fields; unknown live evidence must not invent a path or imply health. Long paths wrap without truncation at the usual widths.

Hand-crafted committed example, showing only the conditional additions to an otherwise complete proxy status result:

```text
Active runtime  0.35.1
Upgrade         committed

proxy
  Executable  /Users/river/.local/state/cq/runtime-artifacts/1111111111111111111111111111111111111111111111111111111111111111/cq
  Live binary /Users/river/.local/state/cq/runtime-artifacts/2222222222222222222222222222222222222222222222222222222222222222/cq
  Listener    127.0.0.1:19280
```

Hand-crafted deferred example, before handover; the component still reports the observed predecessor and its normal health:

```text
Active runtime  0.35.0
Pending runtime 0.35.1
Upgrade         deferred
```

Hand-crafted failed example; the table must still show the actual state, and a failure diagnostic keeps its normal stderr/exit contract:

```text
Active runtime  0.35.0
Upgrade         failed
```

Service help explains that package installers manage retained-runtime upgrades and `cq service status` reports recorded upgrade state separately from observed live process identity. Package-only `service upgrade`, `proxy runtime-check` and private runtime children are excluded from public command lists, examples and completion; their exact contracts are in [the machine ABI](../internal-abi.md).

## `cq service install`

```text
Usage: cq service install [OPTIONS]
```

**User decision:** What did install do to each selected service, and was any rollback verified?

**Help lead:** Install and start selected CQ services.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 60s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Preflight ownership and executable, acquire installer lock, snapshot selected registrations, install proxy before token-refresh when both selected, start and verify each.
- On failure restore previously selected registrations and report rollback outcome; never claim success for partial installation.
- Register current executable identity; package-managed ownership remains protected. No automatic install by check/account commands.

**Examples:**

```sh
cq service install --component proxy
cq service install --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Confirmed installed and verified health can be green. Already equivalent registration is idempotent. Package/foreign ownership conflict is not permission to replace registration. Only selected components appear. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Health verification fails, or strict status detects unhealthy selected component. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_install_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq service restart`

```text
Usage: cq service restart [OPTIONS]
```

**User decision:** What did restart do to each selected service, and was any rollback verified?

**Help lead:** Restart selected installed CQ services.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Stopping or restarting the proxy can interrupt active Claude and Codex traffic.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 60s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Require selected registrations; restart proxy before token-refresh when both selected. Verify proxy runtime health. For token-refresh, wait for the newly requested run to complete with exit 0; this verifies the restart even when durable enablement remains false and reported healthy remains false.
- Preserve selected components existing enabled/disabled-at-login policy. A disabled component is started for this session only; future automatic starts remain disabled.
- May interrupt provider traffic if proxy selected; does not run model acceptance validation.

**Examples:**

```sh
cq service restart --component proxy
cq service restart --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Success proves the requested new start/run, not an earlier run. Preserve enabled state. A disabled refresh job can complete this one run successfully with enabled=false and healthy=false; label that result neutrally rather than claiming future scheduled health. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Proxy runtime verification fails, or the newly requested token-refresh run completes with nonzero exit. Disabled automatic-start policy alone is not restart failure. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_restart_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq service start`

```text
Usage: cq service start [OPTIONS]
```

**User decision:** What did start do to each selected service, and was any rollback verified?

**Help lead:** Start selected installed CQ services.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 60s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Require selected registrations already installed; enable their automatic starts, start proxy before token-refresh and wait for health.
- Already running and enabled healthy selection is idempotent. Never installs missing components.

**Examples:**

```sh
cq service start --component proxy
cq service start --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Start is not install: a missing registration fails. Green success requires selected health verification. Already running, enabled and healthy is idempotent. Show enabled separately from state. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Health verification fails, or strict status detects unhealthy selected component. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_start_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq service status`

```text
Usage: cq service status [OPTIONS]
```

**User decision:** Which services are installed and healthy, which runtime is selected, and which executable is actually running?

**Help lead:** Inspect selected CQ service components.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--strict`: Return non-zero if any selected component is missing or unhealthy. A healthy periodic token-refresh job may be idle. Default: false.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 10s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Inspect selected definitions, desired enablement, process and health without writes. Show recorded runtime versions and upgrade phase when available; distinguish configured Executable from an observed differing Live binary and Listener.
- Default exit0 means inspection completed. --strict requires every selected component installed and healthy: proxy running; token-refresh enabled with a successful latest scheduled run within 35 minutes (idle between runs is normal); no actual model acceptance calls.

**Examples:**

```sh
cq service status --component proxy
cq service status --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Exit 0 means inspection completed, not that every component is healthy. Idle token-refresh is healthy when enabled and its latest successful completed run is within 35 minutes. Show its completion time; no invented continuously-running requirement. --strict changes exit policy. Absent component remains visible. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Health verification fails, or strict status detects unhealthy selected component. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_status_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq service stop`

```text
Usage: cq service stop [OPTIONS]
```

**User decision:** What did stop do to each selected service, and was any rollback verified?

**Help lead:** Stop and disable selected CQ services.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Stopping or restarting the proxy can interrupt active Claude and Codex traffic.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 60s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Disable future automatic starts, stop token-refresh before proxy, and verify selected components stopped.
- Persist disabled state: later quota/account commands do not restart components; only explicit start/install changes desired enabled state.
- Already stopped selection is idempotent. Do not delete definitions, credentials, caches, logs or configuration.

**Examples:**

```sh
cq service stop --component proxy
cq service stop --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Confirmed stopped and disabled is green operation success even though healthy=false. Do not colour the intended false health value as a failed stop. Definitions and user data remain where registrations exist; an absent selected component must not be described as retained. Already stopped is idempotent. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Health verification fails, or strict status detects unhealthy selected component. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_stop_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |

## `cq service uninstall`

```text
Usage: cq service uninstall [OPTIONS]
```

**User decision:** What did uninstall do to each selected service, and was any rollback verified?

**Help lead:** Stop and remove selected CQ service registrations.

### Help contract

**Essential context and preconditions:**

- CQ services comprise the shared API proxy and a token-refresh job. The token-refresh job runs auth refresh at load and every 30 minutes; it does not fetch quota reports.
- Supported service managers: launchd on macOS, per-user systemd on Linux, and current-user Task Scheduler on Windows. Unsupported environments fail explicitly before mutation.
- Stopping or restarting the proxy can interrupt active Claude and Codex traffic.
- Check current-user ownership and package-owner record before service writes.
- Package-owned registrations may be started/stopped/restarted by their user; installing/removing package ownership is reserved for package hooks. Manual install/uninstall encountering package ownership fails with service_owner_conflict.
- Do not request elevation automatically. Linux requires reachable user service manager; Windows uses current-user SID; macOS uses current GUI/user domain.

No positional parameters.

- `--component COMPONENT`: Service component: all, proxy, or token-refresh. Default: all.
  Exact enum; selection never widens after validation.
- `--timeout DURATION`: Maximum elapsed time, including locks, requests and local verification. Default: 60s.
  Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.

**Effect and scope copy:**

- Stop and remove selected CQ-owned service registrations, token-refresh before proxy.
- Persist desired disabled state so later check/account commands cannot reinstall them. Keep credentials/config/cache/logs/receipts/installation binary.
- Absence is idempotent; foreign ownership is conflict, never remove foreign registration.

**Examples:**

```sh
cq service uninstall --component proxy
cq service uninstall --component token-refresh
```

### Result design

```text
CQ services · {action}    {component}

Component       State        Auto-start  Health    PID
proxy           {state}      {enabled}   {healthy} {pid|null-label}
token-refresh   {state}      {enabled}   {healthy} {pid|null-label}

{Optional recorded runtime block, as specified above}

{For each selected component, in proxy/token-refresh order:}
{id}
  Installed   {installed}
  Manager     {manager|null-label}
  Owner       {owner}
  Executable  {executable|null-label}
  Last run    {last_run_at: absolute (relative)|null-label}
  Last exit   {last_exit_code|null-label}
  Error       {error_code|null-label}
  Config      {config_dir|null-label}
  State       {state_dir|null-label}
  Cache       {cache_dir|null-label}
  Runtime     {runtime_dir|null-label}
  Logs        {log_dir|null-label}
  {Conditional Live binary and Listener fields, as specified above}

Rollback · {rollback}
```

Confirmed absence is green operation success, not service health. Absence is idempotent; the final observation alone does not prove a registration was removed. Credentials/config/cache/logs and binary remain; no removal claim beyond selected CQ-owned registrations. Rollback not_needed is neutral, restored is amber (operation still failed), failed is red. Partial components retain their observed states; never replace whole table with a generic error.

**Empty, partial and interrupted variants:** Render nullable values according to their domain meaning: unknown for unobserved/uncertain, — only for known not applicable. An empty collection is “No <resource plural>” using its section name; retain documented explicit sentinel words where required. Missing mandatory resources use the declared failure below. Preserve any returned observations after deadline/interruption; do not show a success title, assume rollback, or claim unchanged state. No progress percentage is defined.

**Narrow and numeric rendering:** Apply the binding widths above to this exact field order. Keep identity and outcome first; subordinate evidence/digest fields follow. Quantities retain labels and units; timestamps retain full local date, zone and numeric offset plus secondary relative time. No field disappears in narrow mode.

### Declared failures

| Code | Exit | Exact message | Condition / presentation |
|---|---:|---|---|
| service_owner_conflict | 6 | Service ownership conflicts with this operation. | Requested write conflicts with package or foreign ownership. |
| service_unavailable | 4 | The current-user service manager is unavailable. | OS manager unavailable or unsupported. |
| service_not_installed | 3 | Service component {component} is not installed. | start/restart targets an unregistered component; strict status observes missing. |
| service_unhealthy | 1 | Service component {component} is unhealthy. | Health verification fails, or strict status detects unhealthy selected component. |
| service_partial | 8 | Service operation was only partially completed; inspect component results. | At least one component changed but final state/rollback incomplete. |
| service_uninstall_timeout | 7 | Operation timed out; inspect state before retrying. | The declared total operational deadline expires. Retain completed observations or known mutation outcomes; do not infer rollback. |
