# Validation, canary, hooks and shared recovery: normative supplement

## Naming decisions

`fixture create` is offline conversion, not capture or validation. `readiness show` checks retained HTTP evidence, not service health. `validate http` requests an asynchronous candidate restart; `validate websocket` runs a synchronous isolated exercise. Their transport-specific help must state these different completion contracts explicitly. No `--installed-result` flag exists: users cannot assert that validation passed. HTTP validation targets only a verified candidate service; port selection alone never grants authority to restart an arbitrary listener.

`canary` means a real-traffic observation run, not a synthetic test or rollout percentage. `hook stop` receives a Codex Stop event; it does not stop the proxy. `proxy rescue` stays shared because its admission and worker transitions affect the shared listener. `proxy operation status` inspects shared durable coordination records; it does not recover them.

Retire `operation recover`: the implementation has no recovery control. The compatibility parser requires its original `--operation-id ID`, rejects malformed input, and returns exit 4 / `operation_recovery_unavailable` with `Active operation recovery is unavailable; use cq proxy operation status OPERATION_ID to inspect retained state.` This fixed diagnostic is owned by the legacy translation in `migration.json`; help bypasses the required ID while retaining the exit-4 retirement result. Never translate this request silently into successful inspection. No new recovery engine is specified.

## Primitive rendering

All JSON resource fields described below are required unless explicitly nullable or optional. No extra fields. Counts are nonnegative JSON integers within uint64. Timestamps are RFC3339 UTC strings. JSON booleans render `true` or `false` in human output. Null human values render `—`, except the operation ID renders `idle` when null. JSON arrays are present even when empty. Standard CQ envelope applies except default `hook stop` output, which is the Codex integration protocol.

## FixtureMetadata input

One JSON object, maximum 65536 UTF-8 bytes, no duplicate or unknown fields. Fields: `session_id`, `thread_id`, `turn_id` (strings, default empty); `window_id` (optional string, default empty); `request_kind` (required enum `turn|prewarm|compaction|memory`); `compaction` (optional enum `standalone_turn|pre_turn|mid_turn`, absent by default). Each identifier is at most 4096 UTF-8 bytes. `turn` requires nonempty session/thread/turn. `prewarm` requires nonempty session/thread and empty turn. `compaction` requires nonempty session/thread/turn and a compaction value. `memory` permits empty identifiers. Non-compaction requests must omit `compaction`. Canonical explicit metadata accepts the string form only; legacy capture may continue accepting `{"phase":"..."}` by normalising it first.

If `--metadata-json` is absent, extract metadata using the supported Codex protocol's flat/nested `x-codex-turn-metadata` representations. Conflicting valid representations must fail, not silently select one. Canonical explicit metadata takes the same validation rules; it cannot hide malformed or contradictory body metadata. The metadata source reflects the selected original representation; direct explicit metadata uses `header`, preserving the current wire meaning.

## Fixture output resource

- `schema_version`: literal `1`, frozen by `CurrentCodexParserSchema` in the annex `codex_readiness.go`.
- `captured_at`: timestamp of fixture conversion, not the request's original network time.
- `body_bytes`: encoded input byte count, 0..2097152.
- `body_hash`: lowercase SHA256 of the encoded original body.
- `metadata_source`: enum `nested|header|flat`; `header` means explicit metadata input.
- `request_kind`: enum `turn|prewarm|compaction|memory`.
- `session_hint`, `thread_hint`, `turn_hint`: strings, respectively `session:`, `thread:`, or `turn:` plus first 12 lowercase SHA256 hex digits of the original identifier. Empty identifiers hash as empty strings, matching the existing fixture engine.
- `has_previous_response_id`: boolean, whether request contains previous response lineage.
- `has_encrypted_state`: boolean, whether encrypted continuation data exists; content is never retained.

Fixture content is deterministic apart from `captured_at`. JSON output uses all fields, including false booleans. Output-file creation must not clobber an existing path; this intentionally tightens the existing overwrite behaviour. The current outer 10 MiB capture limit is misleading because the decoder already rejects encoded input above 2 MiB; canonical help reports the actual 2 MiB limit.

Runnable local fixture preparation:

```sh
printf '%s\n' '{"model":"gpt-5.6-sol","input":[]}' > request.json
cq codex proxy fixture create --input request.json --output fixture.json --metadata-json '{"session_id":"s1","thread_id":"t1","turn_id":"u1","request_kind":"turn"}'
```

The second invocation fails because the output exists. Choose another filename. These examples contain only synthetic identifiers.

## ReadinessMarker resource

- `version`: literal `3`, frozen by `CodexReadinessMarkerVersion` in the annex `codex_readiness.go`.
- `transport`: `http|websocket`.
- `cq_build`, `client_build`: nonempty exact build identifiers.
- `parser_schema`, `lease_schema`: literals `1` and `3`, respectively, for this specification revision.
- `semantics_revision`: nonempty compiled routing-semantics revision string; opaque to callers.
- `retry_budget`: literal `1` for both transports in this specification revision.
- `fixture_hash`: SHA256 of the compiled acceptance fixture set.
- `cq_executable_sha256`, `client_executable_sha256`, `service_identity_sha256`: SHA256 for HTTP; null for isolated WebSocket evidence when no such installed-service binding exists.
- `service_kind`: `launchd|homebrew|systemd-user` for HTTP; null for isolated WebSocket evidence.
- `installed_result`: literal `passed` for valid evidence.
- `completed_gates`: array of unique nonempty gate IDs, sorted lexically. Each ID names an engine acceptance gate, not a CLI option. The exact set must equal the frozen transport-specific gate set listed below; callers must not invent gates or treat a subset as equivalent.
- `validated_at`: validation completion timestamp.

These are evidence fields, not user-settable assertions. Unrecognised schema, absent required fields, duplicate gates, invalid digests and mismatch against the binary's requirements fail inspection. `current` means compatible evidence, not fresh live traffic. No age-based expiry is invented.

HTTP request output adds `outcome: "accepted"`; `validation_complete` remains false. Exhausting the elapsed CLI operation budget can leave an accepted asynchronous service validation running: inspect evidence before retrying. HTTP status inspection must never reuse an unrelated prior marker to claim this request completed. As the current engine does not expose a public request ID, no per-request polling guarantee is invented: help explicitly says inspection is retained build evidence, not request correlation.

## Canary resource

- `run_id`: nonempty opaque run identifier emitted by the existing canary engine; never infer its encoding or manufacture one.
- `active`: boolean, observation still open.
- `finalised`: boolean, true exactly when finalisation evidence exists.
- `started_at`: timestamp; `ended_at`, `last_observed_at`: timestamps or null when never recorded.
- `tuple`: CanaryTuple object.
- `admitted_turns`, `keyed_mismatches`, `automatic_protected_state_changes`, `secret_leaks`, `unexplained_lifecycles`, `live_session_repairs`, `protected_state_failures`: uint64 counters with their literal meanings. None is inferred from service health.
- `consecutive_calendar_days`: nonnegative integer, qualifying consecutive calendar days accumulated by the engine.
- `protected_digests`: array of `{kind,digest}`. `kind` is `system_auth|account_registry|cq_managed_auth|codexbar_manifest|codexbar_auth|routing_default`; `digest` is SHA256. Exactly one per kind, sorted by kind; never expose original protected contents.
- `finalisation`: null or `{stop_request_digest,process_binding_digest,counters_digest,active_sessions}`; three SHA256 strings and nonnegative integer respectively. Represents retained drain completion proof, not a CLI assertion.

CanaryTuple fields: `cq_build` and `client_build` nonempty exact identifiers; `parser_schema`, `lease_schema` literals `1` and `3`; `semantics_revision` opaque nonempty string; `retry_budget` literal `1`; `fixture_hash`, `readiness_fingerprint` SHA256 strings. All bind the run to one compiled/runtime readiness tuple. Start cannot change any tuple field through a flag.

Starting records intent to observe; it does not prove a running service attached. A separately authorised service restart may be needed. Stopping returns once the stop request is recorded; no `stopped`/`passed` claim is allowed while drain remains outstanding. Repeated stop for a matching existing request becomes success without creating another request, normalising the existing already-requested error. The resource projects active and finalised independently; do not infer finalisation from active=false. Retained records still must satisfy the frozen engine finalisation validator: an inactive record without valid finalisation evidence is unavailable, rather than accepted as a completed run.

Canonical start and stop publishers use the owner-controlled, nonblocking `.codex-canary-stop-request.lock` in the canary state directory. The 0600 lock file remains after release; status never creates it. Matching signed pending or inflight intent is reused without rewriting it. Pending requests retain the frozen five-minute validity period; claimed inflight intent retains its expiry exemption. Invalid, expired, mismatched or unreadable intent and lock contention return `canary_state_unavailable`. Older callers using the legacy publication entry point do not share this canonical publisher lock; cross-version publisher serialisation is not guaranteed. Service claiming and finalisation retain their existing protocol.

When canonical start replaces a valid completed run, it holds both the canonical publisher lock and existing serving-owner lock. Before publishing the new run, it retires only exact signed stop artifacts matching that completed run's finalisation digest, after validating every present artifact and retaining its filesystem identity through removal and directory sync. Orphan, malformed, mismatched or replaced artifacts prevent a new run and are never removed as general cleanup. Retirement failure or a definite failure before replacement publication leaves the old signed completed record available; already retired intent is not recreated. Once replacement rename occurs, a later validation, directory-sync or fence failure is indeterminate: start returns an error, but a new active run may already have replaced the old finalisation. No rollback or retention of the old proof is guaranteed after publication. Inspect retained state before retrying: a new active run causes an active-run conflict, while retry may proceed when the old completed record remains valid and its matching artifacts have already been retired. Lock-release failure is also reported even if publication has occurred.

## HookStopEvent and protocol output

Input is one UTF-8 JSON object, maximum 16777216 bytes, followed only by whitespace/EOF. Required fields: `hook_event_name` exactly `Stop`, `session_id` and `turn_id` each nonempty valid UTF-8 of at most 4096 bytes, with no U+0000..U+001F or U+007F. Extra event fields are ignored and must never appear in diagnostics; duplicate required keys are rejected.

Default stdout is exactly one JSON object and newline: `{}` when no receipt exists, otherwise `{"systemMessage":"MESSAGE"}`. Errors use stderr and a nonzero exit code; default stdout remains empty. Global `--json` is for manual diagnostics and uses the standard CQ envelope with this object as `data`. Help still follows normal help rules and never reads stdin. Integration configuration uses `cq codex proxy hook stop`, without `--json`.

MESSAGE grammar:

`CQ route: STATE via TRANSPORT[; pool POOL][; account HINT (KIND)]; MODEL/EFFORT; REASON. Shadow: no-affinity comparison SHADOW.`

STATE is `planned|attempted|completed|failed|rejected|indeterminate`; TRANSPORT is `HTTP|WebSocket`. Optional pool is included only when nonempty. Pool names follow the canonical routing PoolName rules, including Unicode and spaces; do not apply the legacy ASCII slug regex. Render POOL as a JSON string literal, including its surrounding quotes, with Unicode preserved and quotes, backslashes and control characters escaped. Then JSON-encode the complete systemMessage normally. This prevents pool text from being mistaken for receipt delimiters. HINT is `codex:` plus 12 lowercase hex characters. Prefer actual account hint and KIND `actual`; otherwise planned hint and KIND `planned`; omit segment if neither exists. MODEL: `Sol|Terra|Luna|Other|Unknown` for underlying `gpt_5_6_sol|gpt_5_6_terra|gpt_5_6_luna|other|unknown`. EFFORT: `None|Minimal|Low|Medium|High|XHigh|Max|Ultra|Unspecified|Unknown`, mapping directly from lowercase wire enum. REASON: `fixed account|warm affinity|quota/fairness|fallback|selected route`, respectively for bound, affinity reuse, fairness selection, terminal default or unknown. SHADOW: `agreed|favoured account HINT|not applicable|unavailable`; dot terminates the entire message. Values outside supported wire enums fail response validation rather than being interpolated.

## Shared rescue state

`normal`: normal worker admits traffic. `drain`: normal admission is draining during a runtime transition. `rescue_draining`: rescue entry accepted, normal worker still draining. `rescue`: fallback handler serves traffic. `rescue_exit_draining`: normal worker resumes new work while previously admitted rescue work drains. `generation` identifies the durable transition revision. `draining_sessions` contains opaque hashed session hints, never raw session IDs. The human template's `draining_sessions_count` is the array length.

CLI v2 explicitly changes the frozen rescue-control response projection: enter, exit and status report the supervisor's actual traffic mode, including `rescue_draining` while entry is pending. The health response retains its existing effective-mode projection. Transition, drain, authority and persistence algorithms and all response field types remain unchanged. Legacy raw rescue callers also observe `rescue_draining` during pending entry; no new endpoint or negotiation is introduced.

Entry from rescue is a no-op; repeated entry during entry drain retains the same intent. Entry while exit is draining requests rescue again with a new durable generation. Exit requires an available admitted worker. Neither entry nor exit guarantees all old traffic has drained when the control response returns. Exhausting the elapsed operation budget never triggers an automatic retry.

## Operation inspection

`result_available` only indicates a receipt/terminal retained record. It is not action success. Phase enumerates `intent|anchor|receipt|terminal`. For idle, ID/phase/digest are null and result_available=false. Pending inspection exits zero. Explicit missing ID exits 3. The old top-level `operation status --operation-id ID` maps to positional selection; no ID maps to current selected record.

## Required implementation changes and tests

This is a specification, not a claim of existing behaviour. Required changes include strict shared parsing; structured outputs; canonical paths and aliases; fixture non-clobber atomic creation; explicit timeout bounds; metadata duplicate/conflict rejection; canary idempotent stop; truthful operation result labels and recover retirement. Output resources deliberately normalise omitted zero values to explicit values/null.

Per-family behavioural checks: fixture conversion leaves no raw request text or identifiers and refuses existing outputs; encoded/decoded/expansion boundaries tested exactly; stale/malformed HTTP marker rejected without writes; candidate port 19280 rejected before service access; changed candidate process binding prevents restart; async HTTP reports accepted only; failed validation cleans temporary processes and invalidates readiness; client build mismatch fails before exercising; canary start rejects active run and disabled enforcement; stop repeated safely and never claims final drain; hook unknown turn returns {}, Unicode/space pool names render safely without rejection, session and turn identifiers enforce the same UTF-8 4096-byte limit as their CLI selectors, prompt-bearing extra fields never leak; rescue timeouts do not retry; operation pending is successful inspection and retained failed-action receipt is never relabelled action success; recover always gives unsupported diagnostic.

## Deadline and exact-build contract

Every exposed `--timeout` is one monotonic elapsed operation budget beginning before preparation and including all local preparation, requests, validation work and cleanup. Only time spent awaiting interactive confirmation is excluded. Nested calls inherit the remaining deadline; they never reset or extend it. WebSocket validation reserves the final 5 seconds inside its configured timeout for cleanup; its main exercise deadline is the overall deadline minus 5 seconds. There is no additional cleanup allowance. This requires replacing the current independently budgeted cleanup behaviour. Tests must exhaust time in preparation and requests and verify that cleanup still honours the original overall deadline.

`--client-build` is the exact version in recorded evidence or produced by the selected executable. It must satisfy the documented version grammar and match byte for byte. Do not trim whitespace, fold case, accept a matching prefix, substitute the CQ version, or allow arbitrary operator-chosen labels. Executable identity and version must remain bound throughout an active validation exercise.

## Frozen validation annex v1

The self-contained source annex is [../validation-annex-v1](../validation-annex-v1), with [SHA256SUMS](../validation-annex-v1/SHA256SUMS) and [provenance.json](../validation-annex-v1/provenance.json). These copies freeze referenced wire types and algorithms; implementations must not silently substitute later checkout contents. Canonical changes explicitly specified above override legacy behaviour in these copies, including strict metadata duplicate/conflict rejection, pool-name support and timeout accounting. A future engine schema change requires an explicit specification revision, not an unannounced reinterpretation of “current”.

Exact frozen values: fixture/parser schema `1`; readiness marker schema `3`; lease schema `3`; canary stored schema `2`; retry budget `1` for both transports. HTTP semantics revision is `http-conservative-routing-v3` and fixture hash is `618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1`. WebSocket semantics revision is `websocket-terminating-routing-v1` and fixture hash is `858d90f3e827194cdfc0dbf5e11d35dfb25e7afc395cf547f17a30936889f9f9`.

HTTP required gate set, all required exactly once: `frozen-single-transform-envelope`, `warm-affinity`, `deterministic-fallback`, `terminal-default-once`, `exact-pre-admission-hard429-replay`, `admitted-no-migration`, `v2-journal-runtime`, `installed-listener`. WebSocket required gate set, all required exactly once: `strong-frame-authority`, `portable-pre-admission-hard429-rotation`, `same-account-candidate-auth-recovery`, `admitted-no-migration`, `persistent-account-upstream`, `upstream-generation-fence`, `canonical-terminal-error`, `compression-subprotocol`, `installed-isolated-client`. Output arrays sort these values lexically; order does not alter set equality.

Frozen source references:

- [codex_readiness.go](../validation-annex-v1/codex_readiness.go.txt): `CodexReadinessMarker`, `DefaultCodexRoutingRequirements`, `ValidateCodexReadinessMarker`, exact gate lists and constants; readiness fingerprint and marker validation algorithms.
- [codex_fixture.go](../validation-annex-v1/codex_fixture.go.txt): `SanitisedCodexFixture` and `BuildSanitisedCodexFixture`; canonical output normalises false fields rather than omitting them.
- [codex_turn_metadata.go](../validation-annex-v1/codex_turn_metadata.go.txt): metadata wire forms, `decodeCodexCompactionPhase`, parser/extraction structure and `validateCodexTurnMetadata`. Canonical stricter grammar above overrides permissive legacy unknown/duplicate fields and representation priority.
- [codex_protocol.go](../validation-annex-v1/codex_protocol.go.txt): `ParseCodexProtocolRequest`, request lineage/encrypted-state classification and the 8 MiB decoded boundary.
- [codex_zstd.go](../validation-annex-v1/codex_zstd.go.txt): `DecodeCodexRequest`, magic detection and bounded Zstandard expansion.
- [diag.go](../validation-annex-v1/diag.go.txt): `hashPrefix`, the precise fixture hint algorithm.
- [codex_canary.go](../validation-annex-v1/codex_canary.go.txt), [codex_canary_stop.go](../validation-annex-v1/codex_canary_stop.go.txt), [codex_canary_promotion.go](../validation-annex-v1/codex_canary_promotion.go.txt): canary wire types, tuple fingerprint, protected digest, run identity and finalisation validators. Canonical output adds derived `finalised` and explicit nulls.
- [codex_installed_ws_validation.go](../validation-annex-v1/codex_installed_ws_validation.go.txt), [codex_installed_http_validation.go](../validation-annex-v1/codex_installed_http_validation.go.txt), [codex_installed_acceptance_harness.go](../validation-annex-v1/codex_installed_acceptance_harness.go.txt): exercise contracts and service-kind literals.
- [codex_installed_process_attestation.go](../validation-annex-v1/codex_installed_process_attestation.go.txt), [codex_installed_http_route_audit.go](../validation-annex-v1/codex_installed_http_route_audit.go.txt): executable resolution and build-version grammar.
- [operation_inspection.go](../validation-annex-v1/operation_inspection.go.txt): retained coordinator inspection structure.
- [runtime_control.go](../validation-annex-v1/runtime_control.go.txt), [runtime_supervisor.go](../validation-annex-v1/runtime_supervisor.go.txt): rescue mode wire values and transition/drain semantics.

Source snapshots use the `.go.txt` suffix to keep documentation outside Go package discovery. Prose source paths such as `internal/proxy/example.go` name the corresponding `.go.txt` snapshot listed in the annex manifest.

## Installed HTTP request platform support

| Platform | Canonical `codex proxy validate http` capability |
| --- | --- |
| macOS | Requests an already loaded, attested candidate launchd service; never starts an absent candidate or restarts the production listener. |
| Linux | Returns `validation_candidate_unavailable` (exit 4) before request persistence. The existing private owned-candidate exercise creates its own controller and waits synchronously; it is retained but does not provide this asynchronous installed-service contract. |
| Windows and other platforms | Returns `validation_candidate_unavailable` (exit 4); no installed candidate attestation backend is available. |

This limitation applies only to the canonical HTTP validation request. Fixture conversion, retained readiness inspection and isolated WebSocket validation keep their existing platform capabilities. Installed platform acceptance remains separately authorised work.

The public readiness resource also accepts `service_kind: "systemd-user"` for retained Linux HTTP evidence. This explicitly preserves the existing engine's Linux marker validation; it does not change stored marker bytes, the frozen transport requirements, or imply support for the canonical asynchronous Linux request above.
