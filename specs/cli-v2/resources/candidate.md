# Candidate and Codex credential endpoint specification annex v1

This annex is normative for `../commands.json`. CLI v2 envelopes do not change the existing version-1 on-disk signed formats. The frozen source annex is `../candidate-annex-v1/`; `manifest.json` lists the SHA-256 of every copied file. Relative file references below resolve inside that annex, not a mutable checkout. Preserve the annex with the final specification. Source implementation gaps listed below are explicitly overridden by this specification's honest-success requirements.

## Naming and scope

`cq proxy candidate` remains shared: its registry includes `claude_bearer`, `codex_bearer`, and `cq_local_token`; it manages an isolated runtime, not a single provider's routing settings. `cq codex proxy credential-endpoint legacy` is provider-specific: the target is Codex's local credential coordinator at `<resolved CQ state directory>/credential.sock`. The word endpoint alone would confuse this with the HTTP listener. `legacy` distinguishes an old refused Unix socket and its quarantine journal from ordinary service controls. No command here selects a remote machine.

`client-safety refresh` replaces `client-bearer-barrier refresh`. A bearer credential is a token usable by its possessor; a barrier prevents credential-bearing application bytes reaching a foreign runtime while changing a release. The new name foregrounds the purpose. Its help deliberately describes the narrow credential-isolation proof; it is not a general security audit.

`release activate` replaces `artifact switch --role runtime-bundle`. The only supported role is the complete runtime bundle; remove the redundant role selector. The legacy alias requires the literal `runtime-bundle` and rejects every other role. `activate` means changing what runs inside the isolated candidate, not installing the shared service. `release validate` issues evidence and does not promote the release.

Use `--state-dir` consistently. A candidate is an isolated directory containing owned keys, input bindings and operation receipts. Never infer it from the current directory or shared configuration. `--release-digest` is the verified bundle's signed digest field; `--validation-run-id` is a random identifier; `--rollback-receipt-digest` is a domain-separated byte hash. These are deliberately different terms. An `attempt-id` addresses one retained receipt; an operation may own the corresponding attempt, but callers must use the returned ID rather than deriving it.

`--source-config`, `--credential-manifest`, and `--policy-snapshot` retain existing names for input compatibility, but their help explicitly says attestation bytes. They are not parsers or importers. No new manifest language is invented. If a future runtime needs a parsed credential format, that requires a separately versioned schema and explicit implementation decision; this spec does not silently give arbitrary bytes operational meaning.

## CandidateLegacyOptionsV1

Renamed old paths accept old names and translate before canonical validation; canonical names may also be used on old paths. For unchanged command paths, only the renamed flags are deprecated aliases: the canonical command itself is not deprecated. Supplying both spellings counts as a duplicate and fails with exit 2.

| Old option | Canonical option | Meaning |
|---|---|---|
| `--instance-state-root` | `--state-dir` | Exact owned candidate root. |
| `--target-release-set` or `--release-set` | `--release-digest` | Signed target bundle digest field. |
| `--local-token-client-registry` | `--client-registry` | Complete registered request-sender document. |
| `--validation-run` | `--validation-run-id` | Exact 64-hex random run identifier. |
| `--floor-release-bundle` | `--rollback-bundle` | Signed previously accepted release. |
| `--floor-acceptance-receipt-file` | `--rollback-receipt` | Exact accepted proof bytes. |
| `--floor-acceptance-receipt` | `--rollback-receipt-digest` | Domain-separated digest of those bytes. |
| `--receipt-out` | `--receipt-file` | New output receipt path. |
| final positional duration | `--timeout` | Total deadline, preserving old valid range. |
| `--role runtime-bundle` | removed | Legacy artifact alias requires this sole value; canonical release activate has no role flag. |

All unchanged flags retain their spelling. No generic `--yes` replaces semantic confirmation flags. `proxy endpoint transition-legacy commit` is already retired: reject with exit 4, code `endpoint_commit_retired`, exact message `commit is unavailable; use activate, verify the exact live owner, then finalise.` It is not an alias to finalise because that would bypass an essential stage.

## Input byte and filesystem contracts

Every candidate data input file is an absolute, lexically clean path to an owner-controlled regular file. Client executables use the separate trusted-executable rule below; candidate state remains strictly current-user-owned. Reject symlinks, changed identity between inspection/open, group/other writes, paths outside an owner-controlled parent, and sizes above the stated byte limit before state writes. All structured documents additionally reject unknown fields, duplicate keys, missing required fields, invalid UTF-8 and trailing JSON. Canonical documents contain exactly the canonical bytes, with no trailing newline. Receipt output is canonical JSON followed by one LF. Endpoint proof files need not use canonical whitespace.

`AttestationBytesV1`: sequence of 1..1048576 bytes, no parser, no interpretation, no network use. Record SHA-256 over exact bytes. This type is used by source-config, credential-manifest and policy-snapshot. Manifest omission is legal only with credential-mode none; mode read-only requires both manifest and true confirmation. Policy omission records null. Payload-capture confirmation defaults false; storing true authorises capture only during the selected candidate workflow, not globally. No capture occurs during prepare.

`RollbackAcceptanceBytesV1`: sequence of 1..65536 bytes from the rollback acceptance system. Expected digest is `hex_lower(SHA256(UTF8("cq/release-import-floor/v1") || 0x00 || bytes))`. The command must also authenticate the bytes through the acceptance-proof backend before release validation succeeds. Current source only hashes these bytes; until authentication is implemented, validation fails with `candidate_evidence_unavailable` rather than declaring them accepted. No undocumented JSON shape is inferred.

`ClientExecutableBytesV1`: executable regular file of 1..536870912 bytes, no symlink or group/other write bits, stable file identity through hashing. The executable and its trusted parent may belong to the current user or root; root-owned system executables are valid. This exception does not extend to candidate state, input attestations or receipt output parents. Identity is SHA-256 of exact bytes. The client-build value is the exact non-empty version/build string supplied by executable provenance and matched byte-for-byte to independently measured client evidence. Do not trim, fold case, infer versions or accept an arbitrary operator label as provenance. Preparation binds this value; binding alone does not authenticate it.

## Structured input schemas

All properties listed are required unless explicitly nullable. Additional properties are forbidden.

### OperationalReleaseBundleV1

Canonical JSON object with fields in `cmd/cq/proxy_candidate_release.go`, types `candidateOperationalReleaseV1` and `candidateOperationalReleaseRoleV1`, and cryptographic processing in `decodeOperationalCandidateRelease`. Canonical serialisation is exactly `internal/proxy/canonical_jcs.go:CanonicalJSONV1`; the frozen function and its helper implementation define number and string escaping, Unicode handling and key order.

| Field | Type and requirement |
|---|---|
| schema_version | integer, exactly 1 |
| kind | string, exactly `operational_release_bundle_v1` |
| purpose | string `target` for target input; `floor` for rollback input |
| authority_digest | 64 lowercase hex, identifies release authority |
| source_commit | 40 lowercase hex, Git commit identifying built source |
| source_tree_digest | 64 lowercase hex, built source tree content digest |
| roles | non-empty array of RoleV1, unique role names |
| built_at | non-zero UTC RFC3339 timestamp |
| signer_public_key | 64 lowercase hex encoding Ed25519 public key |
| signature | 128 lowercase hex encoding Ed25519 signature |
| digest | 64 lowercase hex; computed below |

RoleV1 fields: `role` is `launcher`, `supervisor`, or `worker`; `artifact_digest` is 64 lowercase hex of exact artifact bytes; `byte_count` is integer 1..536870912. Both supervisor and worker are required; launcher is optional. A declared role is not evidence that its artifact ran.

Signature signs canonical JSON of the entire object with `signature` and `digest` replaced by empty strings. Bundle digest is lowercase SHA-256 of `UTF8("cq/operational-release-bundle/v1") || 0x00 || canonical(object with digest="")`, retaining the signature. Verify signature and digest. Also verify the signer against the accepted release authority before treating the bundle as trusted; self-signature alone only establishes internal consistency. Current code validates fewer field constraints and lacks the full authority check; v2 must reject invalid/missing authority proof.

### ClientSenderRegistryV1

Canonical JSON object: `schema_version` integer exactly 1; `revision` integer 1..18446744073709551615; `senders` array of 1..17 ClientRequestSenderV1 entries. Sender IDs must be unique. Canonical validation and signing rules are frozen in `internal/proxy/client_bearer_barrier.go` (`validateClientBarrierInputs`, `SignClientBearerBarrier`, `ValidateClientBearerBarrier`).

ClientRequestSenderV1: `sender_id` non-empty string, identifier for one sender; `adapter_id` non-empty string, implementation adapter identifier; `stateful` boolean, whether sender retains client state; `credential_domains` array of unique strings from `claude_bearer`, `codex_bearer`, `cq_local_token`, allowed empty; `transports` non-empty array of unique strings from `compact`, `http`, `retained`, `websocket`; `hook_supported` boolean exactly true. Arrays are sorted before evidence generation, senders sorted by sender_id. At least one entry has adapter_id `cq_config_read_per_call_v1`, stateful false, and credential_domains exactly `["cq_local_token"]`. `http` and `websocket` name wire transports; `compact` names the compaction request path; `retained` names a retained connection/request path. These are capability identifiers, not CLI provider spellings.

Each Cartesian combination of sender_id, credential_domain and transport requires one observation with both `foreign_bind_application_bytes` and `release_window_application_bytes` equal to measured zero. No missing or duplicate observations. The signed receipt's exact fields and signing domains remain ClientBearerBarrierReceiptV1 in that frozen source; do not rename signed fields when renaming the CLI.

### LegacyEndpointIdentityV1

Object: `device` uint64 filesystem device; `inode` uint64 >0 inode; `uid` uint64 equal to current effective owner; `links` uint64 >0; `type` string; `mode` uint32 decimal numeric permission bits. Directory identity requires type `directory`, mode 448 (0700). Socket requires type `socket`, mode 384 (0600), links exactly 1. Lock requires type `regular`, mode 384, links exactly 1. Compare full recorded identity to live owned filesystem objects; names alone are not identity proof.

### LegacyEndpointSnapshotV1

Object: `version` integer exactly 1; `path` absolute clean path equal to resolved Codex credential.sock; `state` string exactly `legacy_refused`; `directory` LegacyEndpointIdentityV1 for parent; `socket` LegacyEndpointIdentityV1 for refused socket. Size 1..16384 bytes. Exact parser is `internal/provider/codex/credential_endpoint_maintenance.go:ParseLegacyCredentialEndpointSnapshot`, including all validation helpers. Do not hand-author filesystem identities; obtain snapshot through inspect.

### LegacyEndpointTicketV1

Object: `version` integer exactly 1; `id` exactly 32 lowercase hex; `path` same endpoint; `directory`, `socket`, `lock` identities as above; `quarantine_name` exactly `.` + basename(path) + `.legacy-` + id + `.quarantine`, no directory components. Size 1..16384 bytes. Exact parser and identity contract are `internal/provider/codex/credential_endpoint_maintenance_journal.go:ParseLegacyCredentialEndpointTransitionTicket` and `validate`. Preserve emitted ticket unchanged. It contains operation authority; do not expose it in diagnostics or copy it into logs. JSON result intentionally returns the ticket to its caller for secure persistence.

## Result schemas

The following objects are nested in the root v2 envelope's data object. Unless specified nullable, every field is required. Digests are 64 lowercase hex; times are UTC RFC3339; integers are JSON integers, never float strings. No result includes credentials, control token, key, MAC or signature private material.

### CandidateStatusV2

| Field | Type and meaning |
|---|---|
| state_dir | absolute path to isolated owned directory |
| operation_id | 32 lowercase hex, owning lifecycle operation |
| instance_id | 32 lowercase hex, isolated runtime identity |
| validation_run_id | 64 lowercase hex random run identity, not a digest |
| port | integer 1..65535 except 19280 |
| source_config_digest | SHA-256 of exact source attestation bytes |
| target_bundle_file_digest | SHA-256 of exact target bundle file bytes |
| target_release_digest | bundle's verified digest field |
| active_release_digest | digest or null; set only after exact runtime artifact observation |
| client_build | exact supplied build label |
| client_executable_digest | SHA-256 of client executable bytes |
| client_registry_digest | SHA-256 of exact registry file bytes; distinct from signed domain-separated registry digest |
| credential_mode | `none` or `read-only` |
| credential_manifest_digest | digest or null when absent |
| policy_snapshot_digest | digest or null when absent |
| payload_capture | boolean; explicit capture authority, not proof capture happened |
| phase | `prepared`, `running`, `stopped`, `validated`, or `removed` |
| generation | uint64 >=1; changes after durable transitions |
| pending_action | null or `start`, `stop`, `refresh_client_bearer_barrier`, `artifact_switch`, `validate_release`, `remove`; existing storage identifiers retained |
| effect_started | boolean; whether pending external effect began |
| effect_receipt_digest | digest or null; bound external-effect receipt |
| client_safety_receipt_digest | digest or null; verified measured isolation receipt |
| validation_receipt_digest | digest or null; successful release validation receipt |
| attempt_id | 32 lowercase hex or null; returned after receipt publication for receipt show |
| updated_at | last durable state update timestamp |

Lifecycle phase is not process liveness. `status` does not convert it into health. `validate` can leave a process alive; consequently `stop` accepts running or validated, and `remove` requires independent proof of process/listener absence even in validated. `start` accepts prepared, stopped or validated only if no runtime remains. A failed proof must not create a success phase or a receipt. A pending external effect is a conflict requiring inspection; do not automatically replay an indeterminate mutation.

### CandidateReceiptV2

Object: `attempt_id` exact 32 lowercase hex; `outcome` enum `published` or `conflicted`; `receipt_digest` authenticated compact terminal receipt digest; `promotion_digest` authenticated release promotion receipt digest. A conflicted receipt returns exit 6 and `candidate_receipt_conflicted`, message `Candidate receipt is conflicted.` Its data remains available with ok false. Receipt output file is exactly CandidateReleasePromotionReceiptV1 in frozen `internal/proxy/release_promotion.go`: its embedded CandidateReleasePromotionInputV1 fields and MAC/digest algorithms are normative. These signed file fields retain their v1 names; v2 envelope naming does not rewrite signed objects.

### LegacyEndpointResultV2

Discriminated object, exactly `kind`, `path`, `state`, `snapshot`, `ticket`, `ticket_id_or_none`. For kind `snapshot`: state `legacy_refused`; snapshot is LegacyEndpointSnapshotV1; ticket null; ticket_id_or_none string `none`. For kind `transition`: snapshot null; ticket LegacyEndpointTicketV1; ticket_id_or_none equals ticket.id; state one of `prepared`, `quarantined`, `activating`, `activated`, `finalising`, `committing`, `rolling_back`, `rolled_back`, `committed`. path always equals snapshot.path or ticket.path. `committing` is a readable historical state only; no new commit action exists.

## Workflows and examples

Candidate examples use shell variables with these exact meanings. They are not fabricated valid proof fixtures. Before running them, the release preparation system must have supplied a private input directory containing source-config.bin (AttestationBytesV1), target.json (signed trusted target bundle), floor.json (signed trusted accepted floor bundle), floor-receipt.bin (acceptance-system proof) and client-registry.json (canonical complete registry), plus the exact client executable and its build label. The CLI does not currently expose a producer for these release-system artifacts. Do not synthesize them with arbitrary JSON, zeros or fabricated signatures to make an example run. Failure to obtain verified files blocks the workflow honestly.

```sh
# Set these to the real, operator-reviewed release inputs before use.
INPUTS="$HOME/cq-release-inputs"
CANDIDATE="$HOME/cq-candidate-29280"
CLIENT="/absolute/path/to/the/reviewed/client-executable"
CLIENT_BUILD="the-exact-reviewed-client-build-label"
RELEASE_DIGEST=$(jq -er '.digest' "$INPUTS/target.json")
ROLLBACK_DIGEST=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(b"cq/release-import-floor/v1\0"+open(sys.argv[1],"rb").read()).hexdigest())' "$INPUTS/floor-receipt.bin")
```

The two operator-specific values above must be replaced with the build provenance supplied by the release system; no default is implied. All command examples use those fully defined variables. After successful prepare, obtain run identity without guessing:

```sh
VALIDATION_RUN=$(cq proxy candidate status --state-dir "$CANDIDATE" --json | jq -er '.data.candidate.validation_run_id')
```

Run client-safety refresh while prepared; start exact candidate; activate selected release only if needed; validate; save its JSON output if an attempt lookup is needed. Obtain `ATTEMPT_ID` from `.data.candidate.attempt_id` in that successful validation result. Stop the validated runtime with `--confirm-client-stopped` before remove. No step changes the shared installed service. A current implementation unable to produce exact proof exits 4 at the affected step; that is expected until the implementation gaps are fixed.

Endpoint migration requires stopping and draining every participant before prepare, resume, activate or rollback. These examples do not stop processes implicitly:

```sh
umask 077
INPUTS="$HOME/cq-endpoint-maintenance"
mkdir -m 700 -p "$INPUTS"
cq codex proxy credential-endpoint legacy inspect --json > "$INPUTS/inspection.json"
jq -e '.data.endpoint.kind == "snapshot"' "$INPUTS/inspection.json" >/dev/null
jq -e '.data.endpoint.snapshot' "$INPUTS/inspection.json" > "$INPUTS/endpoint-snapshot.json"
cq codex proxy credential-endpoint legacy prepare --snapshot-file "$INPUTS/endpoint-snapshot.json" --confirm-stopped-and-drained --non-interactive --json > "$INPUTS/prepared.json"
jq -e '.data.endpoint.ticket' "$INPUTS/prepared.json" > "$INPUTS/endpoint-ticket.json"
```

Use that ticket for activate, then launch and verify the exact intended replacement owner through the service workflow. Only then finalise with `--confirm-candidate-healthy`. If verification fails, stop and drain again before rollback. Preserve ticket and output files mode 0600. In human interactive mode the mandatory flag is still required, followed by exact prompt `Type stopped-and-drained to confirm the proxy remains stopped and drained: ` and response `stopped-and-drained`; finalise instead prompts `Type candidate-healthy to confirm the exact live candidate passed health verification: ` and accepts `candidate-healthy`. Other responses fail exit 6. JSON implies noninteractive and still requires the semantic flag. All prompts go to stderr; timeout includes prompt waiting.

## Required implementation corrections, not optional naming polish

1. Validate all structured inputs before prepare writes state; current prepare hashes target/registry without fully parsing them.
2. Do not pretend source config, manifest or policy attestation files are operational imports. Their contracts remain exact opaque bytes.
3. Start the actual target artifact set and inspect its identity. Current `candidateRuntimeExecutable` starts the current executable's `__runtime` control server; that is not release activation.
4. Observe all client evidence; current refresh creates all-zero evidence entries from registry shape, which cannot substantiate safety.
5. Verify actual source ancestry, trust authority, rollback acceptance, stopped-work, broker and confinement proof. Current validate derives some hashes from identifiers and assumes a two-commit ancestry list. Return evidence-unavailable until real proof exists; never issue a promotion receipt on those substitutes.
6. Allow stop after successful validation, and require runtime absence before remove/start. Current phase transitions admit validated remove while stop only accepts running.
7. Standardise global JSON and bounded explicit timeouts; preserve all existing minimum cleanup reserves. Read-only deadline defaults added here do not imply extra retries.
8. Keep credential endpoint exact identity, drain, quarantine and finalisation verifier gates. Renaming must not weaken them.
9. `proxy candidate __runtime --instance --validation-run --generation --port --token-fd` is an internal inherited-descriptor process protocol, not a user command. Hide from public help. Direct operator invocation fails exit 4 with `candidate_internal_command`, `Candidate runtime entrypoint is internal; use candidate start.` Internal launching must authenticate the inherited pipe and exact parent-owned state, never accept a command-line bearer token. Internal protocol compatibility is separate from public aliases.

## Clarified unavailable capability boundary

The actual target-artifact activation, complete measured client evidence, release trust authority, source-ancestry proof and rollback-acceptance verifier do not have complete independent backend contracts in this specification. The descriptions state the eventual success condition, not an implemented or fully specified qualification subsystem. `candidate start`, `candidate client-safety refresh`, `candidate release activate` and `candidate release validate` must return exit 4 `candidate_evidence_unavailable` until the corresponding separate contract and implementation exist. They must not generate successful transitions or qualification receipts from current control-server stubs, declared zeros or derived identifiers. A later qualification specification must define authority provisioning, producer and consumer wire formats, proof collection and verification before enabling success.

`candidate release validate` has a fixed 16-minute deadline with a 30-second cleanup reserve. It has no `--timeout` option. Other explicitly variable timeout ranges remain as listed per command.

`--receipt-file` names a new output leaf, not an input file: validate an existing current-user-owned trusted parent directory and hold its identity; require the leaf to be absent, including symlinks; atomically create mode 0600. Do not require an existing receipt file and never overwrite one.

`candidate stop` and `candidate remove` each have a fixed 30-second deadline including a 15-second cleanup reserve; neither exposes `--timeout`. Deprecated final positional `30s` on unchanged legacy spellings may be consumed solely as a compatibility assertion of that fixed deadline; no other duration is valid. The canonical syntax has no positional duration.

Source snapshots use the `.go.txt` suffix to keep documentation outside Go package discovery. Prose source paths such as `internal/proxy/example.go` name the corresponding `.go.txt` snapshot listed in the annex manifest.

## Local removal inspection checkpoint

Canonical removal publishes one bounded local checkpoint before deleting lifecycle metadata. This is inspection/recovery state, not release qualification. In the retained current-user-owned 0700 or 0755 parent of the exact clean `--state-dir`, its normal name is `.cq-candidate-remove-` + lowercase SHA-256 of the UTF-8 absolute state-dir string + `.checkpoint`. Its only alternate name is that name plus `.final`, the deterministic final-unlink quarantine. Both leaves must be private current-user-owned regular single-link files, opened without following links; two present leaves, an unsafe leaf, or an existing collision is a conflict. No arbitrary sibling search or overwrite is permitted.

The checkpoint is canonical JSON of at most 262144 bytes. Required fields are `version` (1), `kind` (`candidate_removal_checkpoint_v1`), `root` (the exact clean original root), `device` and `inode` (native uint64 identity), `file_id` (the native Windows 16-byte ID, represented as a 16-integer JSON array), `state`, `key`, `registry` (base64 encodings of the exact existing candidate.json, candidate.key and client-sender-registry.json bytes), and `mac` (64 lowercase hex). Unix uses zero file_id bytes; Windows retains its native FileID. The exact existing state MAC and registry SHA-256 binding are verified without changing their frozen formats. The outer MAC is HMAC-SHA256 keyed by the existing 32-byte candidate key over UTF-8 `cq/candidate-removal-checkpoint/v1`, one zero byte, and canonical JSON of the complete checkpoint with mac replaced by the empty string. The stored state must be authenticated phase removed with no pending action. It records retirement, not proof that every file has already been deleted.

The checkpoint is exclusively published mode 0600 and synced before tree retirement. Canonical status checks the two exact names without mutation, authenticates the checkpoint and projects CandidateStatusV2 using the original state_dir. If the original root or its exact operation-specific tombstone remains, its native identity, ownership and security must match the checkpoint. A missing root permits checkpoint inspection; an invalid or replaced root is a conflict, never a fallback excuse. Simultaneous original/tombstone roots are a conflict. Without either checkpoint, normal status applies and missing state remains exit 3. Canonical receipt show uses a surviving identity-matched original root/tombstone; an already-deleted or absent receipt remains exit 3. Prepare, stop and remove reject any retained checkpoint; there is no automatic mutation replay or recovery command.

Before final checkpoint deletion, removal proves both tree names absent, revalidates the retained parent, syncs the parent, and checks the remaining cleanup budget. Unix moves the checkpoint without replacement to the known `.final` name, validates the exact file again and checks the deadline immediately before unlink. Failures retain the checkpoint at a known name and require no restoration writes. Successful final unlink is the deletion commit point; it has no subsequent sync or deadline check that could falsely report pending cleanup. Windows uses an exclusive native handle, POSIX deletion disposition and successful deletion-handle close as its commit boundary; merely marking conventional delete-pending is insufficient. Unsupported disposition fails closed. An exceptional native-close error permits only bounded read-only observation: authoritative absence proves commit, while unknown/denied observation is indeterminate and must not claim retained inspectability. Native Windows qualification remains a separate gate. [Windows POSIX deletion semantics](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntddk/ns-ntddk-_file_disposition_information_ex).

Cancellation before commit leaves the checkpoint inspectable at the original state-dir through canonical status. After committed deletion, the removed DTO is retained even if shared output handling returns interruption 130 or an output error; this is not failed cleanup and does not invent candidate_timeout/pending state. A crash can retain a stale checkpoint because no post-commit durability promise is fabricated. Interrupted removal can therefore retain one additional secret-bearing bounded local file; it remains inspection-only until an explicit future recovery contract exists.
