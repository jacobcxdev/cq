# CLI v2 migration rules — planned CQ 1.0.0

`migration.json` exhaustively accounts for all 92 source-inventory entries. Each row is either a canonical path, an exact spelling alias, a retired route, or a frozen machine ABI. The canonical command catalogue supplies execution semantics; migration rows supply only parsing and dispatch translation. No heuristic matching or prefix expansion is permitted.

1. Preserve the frozen machine interfaces first. Package-hook options select the old package grammar, never the public service command. A call mixing package flags with new public service options fails before mutation. The child/runtime ABIs retain fixed ordering and descriptor contracts. The separate installer never inherits `cq` globals.
2. Resolve exact legacy path. Collect canonical and renamed options under their canonical names. Duplicate non-repeatable options fail even when one spelling is old and another new. Unknown options and trailing arguments always fail before state access; formerly ignored arguments are not preserved.
3. Translate the explicitly listed flags and positionals, choose a conditional variant where defined, inject any fixed component selection, and validate against the destination command. A user-supplied component conflicting with injected component fails; it must never broaden scope. Identical explicit component is a duplicate and fails.
4. Apply canonical omission defaults, constraints and confirmations. Aliases are not a route around new validation, ownership checks, confirmation or deadlines. No aliases silently choose Codex. A required old argument remains required if canonical requires it; optional arguments use the exact canonical omission help recorded in the migration row. Old candidate timeout positionals can be omitted in favour of the canonical timeout default. Candidate stop/remove have fixed30s deadlines including15s cleanup: their legacy final duration accepts only30s and is consumed, never forwarded to a nonexistent --timeout option. Any other duration fails exit2 before access.
5. Execute the canonical command and report its canonical command name in JSON. Legacy public aliases preserve spellings only. CQ 1.0.0 deliberately changes old JSON schemas, human output, error codes and some semantics to the canonical v2 contract. Scripts consuming v0.x output must migrate; there is no `--legacy-json` or automatic v1 renderer.

For a successfully recognised deprecated command path, emit one warning with code `deprecated_alias` and exact message `Deprecated syntax; use cq {canonical_path}.` For a deprecated option on an otherwise canonical path, emit `deprecated_option` with exact message `Deprecated option {old}; use {new}.` Here `{old}` is the supplied deprecated spelling and `{new}` its canonical option spelling. If several deprecated options occur on an otherwise canonical path, emit one warning per distinct old option in argv order. A deprecated command path takes precedence and suppresses redundant deprecated-option warnings for that invocation. This preserves one path warning while giving precise guidance on unchanged paths.

Human stderr renders every warning as `cq: warning: MESSAGE\n`. Normal JSON results also include the matching `{code,message}` object in the v2 warnings array; stdout never contains a diagnostic line. The frozen no-global hook route receives stderr warning only. Alias warning is not a success signal and may accompany execution failure after successful syntax resolution. Retired routes emit their specified error without deprecation warning. Canonical identical paths receive no path warning. Help requests show canonical help and may receive the same stderr warning, but must not read state.

## Global spellings

All public routes accept canonical `--help/-h`, `--json/-j` and `--version/-v` according to the global grammar. They are not deprecated. False JSON selects human rendering. Boolean help/version false do not trigger inspection. Help and version take precedence before state access after syntactic validation. Legacy `help` command-path spellings translate using the explicit group table below or a leaf mapping in migration.json. Invalid or retired leaf paths report their specified error or unknown-command error without action. Broken v0.x leaf-help fallback is not preserved. Global help bypasses required leaf values according to the README; translation never executes the selected leaf.

Legacy `--refresh/-r` means `--fresh` only on `check`, including bare `cq`. Every other public command rejects `--refresh/-r` with exit 2 and exact message `--refresh is only supported by cq check; use --fresh.` This includes aliases where the flag was historically ignored. OAuth refresh is selected by `auth refresh`, never by quota cache options. `--json` always controls rendering, never state-inspection scope.

## Conditional routes and changed semantics

- Bare `cq` is canonical `check` with all three providers (claude, codex, gemini), not deprecated.
- Bare `cq proxy pin` is retired, exit 2, code `provider_required`, exact message `Specify a provider: use cq claude proxy pin show or cq codex proxy pin show.` No configuration read and no automatic provider default.
- `proxy pin PROVIDER` / `proxy default codex` select `show` when selector absent and clear false, `set ACCOUNT` when one selector exists and clear false, and `clear` when clear true without selector. Selector plus clear true fails before access. Account resolution follows the canonical provider account-reference contract; selector contents never determine provider.
- Bare `proxy status` intentionally becomes the canonical full readiness inspection. Only explicit legacy `proxy status --port PORT` translates to `proxy health --port PORT`. Full-status options cannot accompany the health variant. Use the new `proxy health` spelling for old liveness behaviour.
- `operation recover` is retired with exit 4, code `operation_recovery_unavailable`, exact message `Active operation recovery is unavailable; use cq proxy operation status OPERATION_ID to inspect retained state.` No implied recovery. `--operation-id` remains syntactically required on this retired route and must be 32 lowercase hex characters; malformed syntax produces exit 2 before retirement error. `--json` gives the v2 error envelope, no retained-state read.
- `proxy candidate artifact switch --role runtime-bundle` consumes the validated fixed role token when translating to `proxy candidate release activate`; no other role is supported.
- `codex validate http` translates to `codex proxy readiness show`, which reads evidence. `proxy validate-http` translates to `codex proxy validate http`, which requests active validation. These are deliberately distinct; no alias conflates them.
- `models ... --provider anthropic` maps to canonical `--provider claude`; provider `codex` is unchanged. Required provider flags never gain a default.
- `service install/restart/status/uninstall` retain their paths and default component `all`; they are canonical and receive no deprecation. Legacy `proxy install/restart/uninstall` inject component `proxy`; `agent install/uninstall` inject `token-refresh`.
- `gemini accounts` becomes supported `gemini account show` instead of preserving its old dispatch bug.

## Coverage acceptance

The migration inventory must have exactly one row per source-inventory path, no extra or missing row. Every public destination and conditional destination must exist in the canonical catalogue. Every old flag and positional must have a mapping or explicit rejection rule. Every frozen row must name its ABI exception. Public CLI tests must run canonical and alias forms against the same destination handler and assert identical v2 results, apart from warnings. Tests must prove old ignored `--dry-run` tails reject before mutation, mixed old/new duplicate options reject, old help has no state access, component aliases cannot widen their targets, and public account aliases cannot bypass confirmation.

## Exact legacy group-help map

This table applies to `cq help OLD_PATH`, `cq OLD_PATH --help`, and legacy `cq PREFIX help SUFFIX` (join PREFIX and SUFFIX to form OLD_PATH). It applies only to help requests, not bare retired command execution. All destination group help is exact generated canonical help, exit0, with no state access. The alias warning uses the destination path. A canonical group path not listed here maps to itself without warning. No unknown prefix is inferred.

| Old group path | Canonical help path |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `gemini` | `gemini` |
| `agent` | `service` |
| `codex resets` | `codex reset` |
| `codex validate` | `codex proxy` |
| `codex canary` | `codex proxy canary` |
| `proxy reserve` | `codex proxy reserve` |
| `proxy default` | `codex proxy fallback` |
| `proxy default codex` | `codex proxy fallback` |
| `proxy prime` | `codex proxy prime` |
| `proxy policy` | `codex proxy` |
| `proxy policy pool` | `codex proxy pool` |
| `proxy policy session` | `codex proxy session` |
| `proxy leases` | `codex proxy lease` |
| `proxy hook` | `codex proxy hook` |
| `proxy endpoint` | `codex proxy credential-endpoint legacy` |
| `proxy endpoint transition-legacy` | `codex proxy credential-endpoint legacy` |
| `proxy candidate client-bearer-barrier` | `proxy candidate client-safety` |
| `proxy candidate artifact` | `proxy candidate release` |
| `operation` | `proxy operation` |
| `proxy pin claude` | `claude proxy pin` |
| `proxy pin codex` | `codex proxy pin` |

`proxy policy` help uses the broader Codex proxy group because pool/session/policy moved to sibling groups. In addition to the normal alias warning, emit stderr warning code `legacy_group_split`, message `Shared state initialisation moved to cq proxy state initialise.` Do not append noncanonical prose to generated help stdout.

`proxy pin --help`, `cq help proxy pin`, and `cq proxy help pin` have no single provider destination. They produce exactly this plain help text on stdout and exit0, with no warning or state access:

```
Usage: cq <provider> proxy pin <command>

Choose a provider explicitly:
  cq claude proxy pin --help
  cq codex proxy pin --help
```

The final line ends with LF. This is a compatibility help exception, not a new executable group. Bare `cq proxy pin` without true help still returns its specified provider-required error.

Leaf help follows the migration target, except the conditional pin/fallback paths listed above deliberately show their group help rather than selecting `show`. A help request for an unchanged canonical leaf stays on that leaf. `operation recover --help` remains the explicit retired exit4 error after help bypasses its required ID; ordinary execution still requires a valid ID first.

## Hook option boundary

Legacy `cq proxy hook codex-stop` with no global options retains the raw frozen stdin/stdout and exit-code ABI, with only stderr alias warning added. If any recognised global option is present, route through canonical `cq codex proxy hook stop`: true help/version inspect without consuming stdin; `--json=true` wraps the canonical result in the v2 envelope; false globals execute with canonical human/native hook rendering. No local options exist. Unknown options fail exit2 before consuming stdin. Canonical hook input validation applies in the globals-present branch; no command-line token becomes event data.
