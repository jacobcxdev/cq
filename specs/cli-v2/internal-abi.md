# Frozen machine interfaces and installer contract

These interfaces preserve the v0.32.5 baseline and the later released installer/runtime extensions explicitly recorded below. They are not subject to the new public parser, JSON envelope, exit-code table, reordered options, or public aliases. Process-child and package-hook entrypoints remain internal except for the released public `service upgrade` operation, which also preserves its package-hook form and native output protocol. All names beginning `__` remain internal. Human operators use the public command catalogue; package managers and CQ-owned launchers retain the frozen forms below. Existing executable paths remain valid through CLI migration. Public `--help` must not accidentally launch one of these interfaces.

## Package lifecycle hooks: `cq service`

Package-hook argument forms, subject to the dispatch rule below:

```
cq service install [--owner OWNER] [--service-executable=PATH] [--installer-lock-held]
cq service uninstall [--owner OWNER] [--service-executable=PATH] [--installer-lock-held]
cq service upgrade --owner homebrew --candidate-executable=PATH [--service-executable=PATH] [--installer-lock-held] [--json]
cq service snapshot --owner OWNER --snapshot-file=PATH --installer-lock-held
cq service restore --owner OWNER --snapshot-file=PATH --installer-lock-held
```

For install/uninstall, bare `cq service install` and `cq service uninstall` use the canonical v2 public parser, output and exit contracts. A machine-ABI invocation must contain at least one package-only marker: `--owner`, `--service-executable` or `--installer-lock-held`, in its exact permitted form, and the entire invocation must pass the package-hook grammar below. Marker presence alone does not grant machine dispatch or mutation authority. Invalid package options, empty explicit values and combinations with public `--component` or `--timeout` must be rejected without mutation; they must not fall through to a permissive legacy alias. Omitting `--owner` retains the legacy `manual` default only when another valid package-only marker establishes package context and all that marker's owner constraints are satisfied. This default does not make the bare forms machine entrypoints. Snapshot/restore remain hidden machine actions and require their explicit owner, snapshot path and inherited lock marker. Upgrade is a public command with ordinary help and completion. Its exact parsed Homebrew package invocation with a nonempty candidate path still receives frozen machine dispatch. Other valid public option spellings use the canonical parser and invoke the same released lifecycle/output protocol. Bare `cq service upgrade` fails required-option validation without mutation. A public help/global option must never execute a package transaction.

`install`, `upgrade`, `uninstall`, `snapshot`, and `restore` are literal action names. `upgrade` requests the Homebrew retained-runtime transaction: verify and stage the candidate, stop admission, drain current turns, hand over the retained listener and verify the selected runtime, preserving its receipt and recovery state. `snapshot` saves the operating-system service definitions and enabled/running state before a package transaction. `restore` reinstates and verifies that exact saved state after a failed transaction. These two actions are hidden, not new user backup commands.

| Option | Exact ABI |
|---|---|
| `--owner OWNER`, `--owner=OWNER` | Optional on install/uninstall; required on snapshot/restore because they require inherited package locking, and required as `homebrew` on upgrade. Explicit choices are `homebrew`, `winget`, `go`. Omission selects internal owner `manual`; explicit `manual` is rejected. Identifies the package authority allowed to claim or mutate the installation. No repeated owner. |
| `--service-executable=PATH` | Optional; omission defaults to empty. When supplied, the value must be nonempty and is valid on install/upgrade/uninstall/snapshot/restore only with owner `homebrew`. Equals form only. Clean absolute path, exactly equal to `filepath.Clean(PATH)`. The stable executable and currently running executable must resolve to the same file. Supplies Homebrew's stable link instead of its version-specific executable path. No repeated nonempty value. |
| `--candidate-executable=PATH` | Required on upgrade; forbidden on other actions. Equals form only. Nonempty clean absolute path to the candidate package executable. Candidate staging verifies its bytes, build provenance and runtime protocol before handover. No repeated nonempty value. |
| `--json` | Bare boolean, default false; only upgrade accepts it among package mutators. Emits the native upgrade receipt, not a v2 envelope. No repeated flag or value form. |
| `--snapshot-file=PATH` | Required snapshot/restore only. Equals form only. Clean absolute path. Secure snapshot file; maximum 3 MiB. No repeated nonempty value. |
| `--installer-lock-held` | Boolean marker, no value form, default false. Requires explicit non-manual package owner and package action. Required snapshot/restore. The caller must supply the already-held installer mutation lock through stdin; CQ validates the inherited descriptor, and rejects a marker without valid lock authority. This does not mean “skip locking”. |

Options follow the action; no positionals. Unknown arguments fail. Explicit empty package paths are rejected by machine dispatch. `--json` is rejected for install/uninstall/snapshot/restore. Without inherited marker, install/uninstall/upgrade acquire the mutation lock themselves. All hooks use current-user service scope. Successful install/uninstall/snapshot/restore write no stdout. Upgrade writes `Runtime upgrade: PHASE (PREVIOUS_VERSION -> CANDIDATE_VERSION)\n`, or one native RuntimeUpgradeReceiptV1 JSON document with `--json`; when upgrade returns a receipt alongside a failure, that receipt is still emitted before the error. An early failure before upgrade starts need not emit a receipt. Errors return process exit 1 and `cq: <error>\n` on stderr; successful completion returns 0. Install claims ownership and installs/starts both components. Uninstall stops/removes owned components. Snapshot checks ownership and platform preflight, then atomically writes the secure snapshot. Restore decodes one strict JSON document, rejects unknown fields or trailing JSON, checks schema/owner/executable identity, restores platform state, snapshots it again, and fails if the result differs.

Snapshot schema: `schema_version` integer exactly 1; `owner` one of the package owners above; `executable` absolute string; `platform` object with `manager` string, optional `folder_exists` boolean, optional `folder_security_descriptor` string, and `components` array. Each component contains `id` string and `exists` boolean, optional `definition` base64 string holding native definition bytes, `enabled` boolean, `unit_file_state` string, `running` boolean. Optional fields omitted when zero-valued by Go JSON. This is a restoration payload, not an editable user configuration file.

Legacy service parser also accepts public `restart` with no options and `status [--json]`. Neither accepts `--owner`; status and the later released package upgrade action accept JSON. Upgrade retains its raw native JSON protocol through both public and machine entrypoints; public status uses the canonical v2 envelope. The new public service grammar can replace these routes, but package-specific old forms above must remain dispatchable before public parsing. Snapshot/restore have no public help entry. Upgrade has ordinary public help describing every public parameter and its raw-output exception; the inherited-lock marker remains package-only. Public help must never execute a transaction.

RuntimeUpgradeReceiptV1 has required fields: `schema_version` exactly 1; `transaction_id` ASCII `[a-zA-Z0-9_-]{1,128}`; `generation` positive uint64; `phase` one of prepared|waiting|handoff|verifying|committed|deferred|rolling_back|rolled_back|failed; `previous` and `candidate` RuntimeArtifact objects; `listener_identity` nonempty string; `supervisor_pid` integer greater than 1; and `admission_pause_nanos` nonnegative integer. Optional nonzero fields are `guard_pid` (integer greater than 1), `recovered_supervisor_pid` (integer greater than 1), `recovery_attempts` (integer 1..3), and nonempty `error_code`. Recovery PID and attempt count must occur together. Each RuntimeArtifact has required `path` absolute string, `version` nonempty string, `sha256` 64 lowercase hex characters, and `protocol_version` exactly 1. Native failure receipts can be incomplete zero values if staging fails; only validated persisted receipts certify a transaction. A committed receipt records runtime selection; service liveness and health still require separate inspection.

Sources: baseline `cmd/cq/service_command.go` and `cmd/cq/service.go` in v0.32.5; released extension in current `cmd/cq/service_upgrade.go`, `cmd/cq/service_upgrade_darwin.go`, `internal/proxy/runtime_upgrade.go` and `internal/installer/runtime_artifact.go`.

## Separate installer executable: `cq-install`

```
cq-install [install|uninstall] [--silent] [--owner=OWNER]
cq-install --help
cq-install -h
```

No action defaults to `install`. Exactly one action may appear, anywhere among options. `install` downloads and verifies the tagged CQ release, installs it and both per-user services. `uninstall` removes the managed installation and services while preserving user data. `--silent` is a bare boolean, default false; suppresses success output only, not failures. `--owner=OWNER` is equals-only, default `go`; explicit `go` accepted. `winget` accepted only in a Windows release-linked installer build; every other owner rejected. Owner option and action cannot repeat. No other flags or positional arguments. Help must appear alone. The executable's own tagged unreplaced module or linked version determines the release; no runtime version parameter. Stable `MAJOR.MINOR.PATCH`, optionally prefixed `v`, is required.

Exact success stdout, unless silent:

* Install: `CQ <normalised-version> installed with proxy and refresh services.\n`
* Uninstall: `CQ uninstalled; user data preserved.\n`

Exit 0 on success/help. Every parse, download, verification, ownership or lifecycle failure exits 1 and writes `cq-install: <error>\n` to stderr. No JSON mode. No public global flags inherited from `cq`.

Exact legacy help:

```
Usage:
  cq-install [install|uninstall] [--silent]

Downloads and verifies tagged CQ release binaries, then manages proxy and
refresh services as one complete per-user installation. User data is preserved
on uninstall.
```

Package invocation example: `cq-install install --silent --owner=winget`, only from the eligible Windows release package. General installation example: `cq-install install`.

Source: `cmd/cq-install/main.go:117`.

## Retained runtime compatibility check

```text
cq proxy runtime-check --json
```

Exactly these three tokens, in this order; no public global options, aliases, help, defaults or extra arguments. Installer/artifact validation only. It reads an existing proxy configuration if present and rejects malformed configuration, but does not create configuration, open the credential coordinator, start a listener or send upstream requests. Missing configuration is accepted.

Success returns exit 0 and one JSON object with exactly `schema_version` (integer 1), `version` (the executable's build version string), `protocol_version` (integer 1), `goos` (the executable's Go OS identifier), and `goarch` (its Go architecture identifier). No v2 envelope. Failure returns exit 1 with `cq: <error>\n` on stderr and no successful result. The package validator separately verifies digest, provenance, native platform and retained-file authority; this self-report alone does not prove an authorised artifact.

Source: `cmd/cq/proxy_runtime_check.go`, `internal/installer/runtime_artifact.go`.

## macOS retained-runtime guard and resume children

```text
cq --runtime-upgrade-guard TRANSACTION_ID
cq --runtime-upgrade-resume LISTENER_FD LIFECYCLE_FD CONTROL_FD SECRET_FD
```

Only the CQ runtime handover launcher may invoke these entrypoints. They are Darwin-only, excluded from public grammar, help and completion, and accept no public flags or JSON result mode. The guard takes exactly one transaction identifier and checks it against the secure persisted receipt; when that receipt records a guard PID, it must equal the current process PID. It inherits listener FD3, placeholder FD4, authenticated control FD5 and secret FD6. The placeholder is closed on return. The guard retains the listener while the supervisor changes executable and supports bounded authenticated recovery; its lifetime is at most five minutes. The secret is never an argv value.

Resume takes exactly four distinct inherited descriptor numbers in the order shown. Each must parse as an integer in 100..1048576; unlike the runtime-role manifest, this released parser does not require canonical decimal spelling. CQ marks the descriptors close-on-exec, reads the secret file and authenticates an `upgrade_resume` control frame. The strict resume payload must match the persisted receipt and validate listener/lifecycle identity before resuming. Possession of numeric descriptor arguments without the descriptor contents and authority proofs grants no access. Control messages use the existing authenticated runtime transport; stdout carries no command-result document. Runtime errors return exit 1 with `cq: runtime upgrade: <error>\n` on stderr. Normal shutdown returns 0; runtime diagnostics may appear on stderr.

The exact historical `cq proxy start` launch form also preserves retained-runtime startup recovery before public alias handling. For a Homebrew owner record with a validated receipt, it verifies and selects the committed candidate, or the previous artifact for other phases; interrupted handoff/verifying/rolling_back phases use guarded listener recovery. Prepared/waiting receipts become deferred when the supervisor restarts before handoff. Unsafe unproven release fails closed. With no applicable retained selection it falls through to the normal proxy start compatibility route. Extra arguments, runtime-role children and `runtime-check` must never consume startup recovery state. These rules do not introduce a public handover command.

Sources: `cmd/cq/proxy_upgrade_darwin.go`, `cmd/cq/proxy_upgrade_exec_darwin.go`, `internal/proxy/runtime_upgrade_guard_darwin.go`, `internal/proxy/runtime_upgrade_resume.go`.

## Runtime role child: `cq proxy start --runtime-role`

Exactly these ordered forms, no optional values, no extra arguments:

```
cq proxy start --runtime-role supervisor --runtime-schema 1 --runtime-manifest-digest DIGEST --proxy-instance PROXY_ID --runtime-instance RUNTIME_ID --listener-fd 3 --lifecycle-fd 4 --control-fd 5 --lifecycle-holder-digest HOLDER_DIGEST --secret-fd 6
cq proxy start --runtime-role worker --runtime-schema 1 --runtime-manifest-digest DIGEST --proxy-instance PROXY_ID --runtime-instance RUNTIME_ID --work-fd 7 --lifecycle-fd 4 --control-fd 5 --lifecycle-holder-digest HOLDER_DIGEST --secret-fd 6
```

`--runtime-role` selects owned supervisor or worker process. `--runtime-schema` is exactly canonical decimal `1`. `--runtime-manifest-digest` and `--lifecycle-holder-digest` are nonzero SHA-256 values encoded as 64 lowercase hex characters; the former identifies the authorised manifest, the latter the lifecycle holder identity. `--proxy-instance` and `--runtime-instance` are nonempty opaque strings, not human account identifiers; no additional lexical restriction is introduced. Descriptor options identify inherited open descriptors: listener 3 for supervisor; work 7 for worker; lifecycle 4; authenticated control 5; secret 6. Numeric spellings must be canonical decimal with no `+` or leading zeros. Worker does not accept listener-fd; supervisor does not accept work-fd. No defaults.

Only the CQ runtime launcher may invoke this interface with the matching open descriptor set and authority proofs. Secret FD contains exactly 32 bytes followed by EOF and is closed after reading. The worker closes reserved descriptor 3 before role validation. Control communication occurs on the inherited descriptor, not stdout, and uses length-prefixed authenticated frames (4-byte big-endian size; payload 1..65536 bytes). Never place secret contents on the command line.

Success runs the role until its lifecycle terminates and returns 0. Invalid manifest returns exit 1 with `cq: invalid runtime role manifest\n`; unavailable role/platform returns exit 1 with `cq: runtime role unavailable\n`. Other runtime errors use `cq: <error>\n`. Runtime diagnostics may appear on stderr; stdout has no command-result schema. Base implementation rejects supervisor execution unless the platform adapter supplies it; preserving parser acceptance does not claim platform support.

Sources: `internal/proxy/runtime_control.go:77`, `cmd/cq/proxy.go:146`, `cmd/cq/proxy.go:590`.

## Linux installed-validation child

```
cq proxy start --port PORT --linux-validation-candidate-fd 3
```

Internal Linux launcher only. `--port` selects the candidate loopback listener, integer 1..65535; higher-level validation rejects live port 19280. `--linux-validation-candidate-fd` supplies inherited controller descriptor, exactly integer 3; required for this child mode, cannot repeat. These two flag/value pairs may exchange order. No user confirmation flag replaces controller authority. Absent candidate flag, this is ordinary proxy start, not this ABI. No JSON or public global options. Unsupported platform/controller fails with exit 1 and stderr `cq: proxy validation candidate is unavailable\n` or platform-specific underlying error. Success runs the candidate service; no stdout command-result schema. The runtime may log diagnostics to stderr. Launch, validation, and cleanup remain controlled by the parent service adapter.

Source: `cmd/cq/proxy.go:617`, `cmd/cq/proxy_http_validation_service_linux.go`.

## Candidate health child

```
cq proxy candidate __runtime --instance INSTANCE --validation-run DIGEST --generation GENERATION --port PORT --token-fd 3
```

Fixed order; exactly five flag/value pairs. `--instance`: exactly 32 lowercase hex characters, candidate identity. `--validation-run`: exactly 64 lowercase hex characters, validation run identity. `--generation`: unsigned 64-bit decimal greater than zero. `--port`: integer 1..65535 excluding 19280. `--token-fd`: inherited descriptor exactly 3. No defaults, duplicate flags, extra arguments, help, JSON or public aliases. Parent candidate lifecycle launcher only.

Read exactly 32 token bytes then EOF from FD3; close it, zero buffered secret on return. Bind `127.0.0.1:PORT`. Requests require `X-CQ-Candidate-Control` containing the matching token in hex. `GET /health` and `POST /__cq_candidate_control/stop` return JSON `{schema_version:1,kind:"candidate_runtime_health_v1",proxy_instance_id:INSTANCE,validation_run_id:DIGEST,generation:GENERATION}`; stop requests orderly shutdown. Other authenticated routes return 404; missing/wrong token returns 401. HTTP header-read and shutdown deadlines both 2 seconds. No stdout result. Normal stop exits 0; any child error exits 1 with exactly `cq: candidate runtime failed\n`, without detailed secret-bearing failures.

Source: `cmd/cq/proxy_candidate_runtime.go:196`, `cmd/cq/main.go:239`.

## Linux namespace helper

```
cq proxy __linux-acceptance-helper
```

Linux only; exactly two command tokens, no flags or positionals. CQ acceptance launcher only. Inherits control FD4 and executable FD5; closes reserved FD3. Parent sends namespace configuration through control transport: version, executable display path, argv array, environment array, working directory, allowed write root and permitted relay definitions. These are descriptor protocol data, not CLI parameters. The child installs filesystem and network confinement, launches the passed executable descriptor with stdin `/dev/null`, and relays child stdout/stderr unchanged. Parent-death signal kills the child. Missing authority, failed confinement or child failure must fail closed. No direct user-call example because valid invocation requires the parent descriptor/namespace protocol. Success exits 0; any helper failure exits 1 and writes exactly `cq: Linux acceptance helper failed\n` after any relayed child stderr. No JSON command envelope. Non-Linux binaries do not recognise this entrypoint.

Sources: `cmd/cq/linux_acceptance_helper_linux.go:11`, `internal/proxy/linux_namespace_helper.go:106`.

## Codex Stop hook compatibility route

`cq proxy hook codex-stop` with no global options retains this frozen contract for installed Codex hook configurations. It takes no local options or positionals. If recognised global --help/-h, --version/-v, or --json/-j is supplied, dispatch to canonical codex proxy hook stop instead: canonical globals, output and exits apply, including the v2 envelope for --json=true. False globals use canonical execution and its human/native hook rendering. Unknown flags fail exit2 before consuming stdin. This explicitly versioned extension does not alter the no-global ABI. The legacy spelling emits deprecated_alias warning on stderr, never into the native stdout document. This route is machine-advertised, unlike the private child routes above.

Stdin is exactly one JSON document, at most 16 MiB. Required fields: `hook_event_name` exactly `Stop`; `session_id` and `turn_id` nonempty UTF-8 strings, each at most 4096 bytes, without bytes below 0x20 or byte 0x7f. Unknown input fields are ignored for forward compatibility; trailing JSON is rejected. Resolve existing local proxy configuration, use configured port or 19280 if zero, require local bearer credential, and perform bounded local receipt lookup with 5-second HTTP timeout; redirects rejected. This reads the retained routing receipt and does not send a model request.

Success stdout is one JSON document: `{}` when no receipt exists, otherwise `{"systemMessage":"<rendered receipt>"}`. Keep existing receipt formatter. Message begins `CQ route: <state> via HTTP` or `WebSocket`, optional `; pool <name>`, optional `; account <hint> (actual)` or `(planned)`, then model/effort and route explanation, ending with shadow-comparison explanation. In the no-global frozen branch, do not wrap in the new public JSON envelope. Success exits 0; invalid input, config, HTTP or response exits 1 with `cq: <error>\n` on stderr. Input errors use `invalid Codex Stop hook input`. No help or global flag may be interpreted as hook input.

Source: `cmd/cq/proxy_codex_hook.go:32`.

## Dots runtime child

The default-branch integration also retains the released private Dots entry point:

```
cq __dots ROLE ABSOLUTE_INSTALLATION_ROOT [ROLE_ARGUMENTS...]
```

Only CQ-owned Dots launchers and workers use this interface. It is classified before public parsing. It does not inherit public globals, help, aliases, semantic output formatting or the v2 JSON envelope. Role arguments, including stock native-executor and cloud-stdio arguments, retain their order and are passed to the released role handler. Public `cq codex dots ...` commands always use the canonical public parser.

The installation root must be clean and absolute. The helper loads owned `settings.json`, requires its runtime root to equal `INSTALLATION_ROOT/runtime`, then retains the released role-specific authority and executable checks. Missing role/root or an invalid root fails before those reads. Roles are `native-broker`, `probe`, `realtime`, `relay`, `executors`, `local-executor`, `native-executor`, `cloud-stdio`, `cloud-stdio-bridge`, `account-watch`, `desktop-launch` and `desktop-recover`. This role list does not grant direct user launch authority or add public discovery.

Stdin, stdout and stderr belong to the selected role's existing protocol. No successful helper completion document is added. Errors use `cq: <error>\n` on stderr. Normal completion returns 0; a preserved `CloudStdioExitError` returns its original positive code through 255; other failures return 1. Existing tests cover preservation of direct and wrapped child exit status and rejection of malformed helper invocations. Public help and completion do not advertise `__dots`.

Sources: `cmd/cq/dots.go`, `internal/dots/control_helper.go`, `cmd/cq/main.go` in the integrated default-branch source. Public resource details are in [`resources/dots.md`](resources/dots.md).

## Boundary and migration coverage

The machine ABI exceptions above are deliberate compatibility contracts, not examples for public command design. Existing implicit `cq` quota-check, `help` aliases, parent-only legacy help and invalid legacy help routes belong to the public migration table: all new groups show help without state access; old public help requests resolve to the canonical destination. Do not preserve broken missing help as public behaviour. Private unsupported help remains side-effect-free rejection.

Repository-only build/test tools `internal/tools/proxyrelease`, `releasestatus`, `proxycu` and `wingetmanifest`, plus shell scripts under `scripts/`, are outside the installed `cq`/`cq-install` product CLI. They retain their developer interfaces unchanged; no public command is inferred from their existence.
