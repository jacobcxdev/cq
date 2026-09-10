# CQ CLI v2: canonical implementation specification

Status: proposed implementation contract, 10 September 2026. This package specifies the whole public CLI and preserves the installed machine interfaces explicitly. It does not implement the redesign. Target release: **1.0.0**, because output, parsing, exit codes and some ambiguous legacy commands deliberately change. “CLI v2” names this contract and JSON envelope version; it is not the release number.

## Read this package

- [Implementation plan](plan.md): 29 sequenced tasks across six workstreams, with command/parameter ownership, file targets, regression cases and delivery gates.
- [Command reference](COMMANDS.md): every command, argument, option, constraint, effect, output, error, example and naming rationale.
- [Machine-readable catalogue](commands.json): authoritative public command definitions and global options.
- [Exact root help](help/cq.txt): entry point to generated help. Every command and group has its own `help/*.txt`.
- [Naming decisions](naming.md): reasons for every parameter name and the distinctions between overlapping terms.
- [Environment and paths](environment.md): exact configuration discovery and default resolution.
- [Resource contracts](resources/root.md): quota, runtime and service types; additional [accounts](resources/accounts.md), [models/auth](resources/models.md), [routing](resources/routing.md), [candidate](resources/candidate.md) and [validation](resources/validation.md) contracts.
- [Migration rules](migration.md) and [complete migration inventory](migration.json): every existing entry point and parameter is preserved, translated or explicitly retired.
- [Machine interfaces](internal-abi.md): frozen installer, child-process, descriptor and hook protocols, including the separate `cq-install` executable.
- [Verification record](verification.md): coverage counts, completed checks and their limits.
- [Acceptance cases](acceptance.md): per-command requirements, supplemented by global cases below.
- [Source inventory](source-inventory.json), [baseline](source-baseline.json), [source differences](source-differences.md): what was reviewed and how the installed release differs from the checkout.

`commands.json` owns command-specific grammar and exact prose. This README owns common behaviour. Resource documents own named data types and algorithms. `environment.md` owns omitted path/environment resolution; command-specific explicit overrides still apply. `migration.json` owns legacy translations, never canonical semantics. Frozen annexes own only the explicitly retained v1 mathematics and internal wire formats. `COMMANDS.md`, `acceptance.md` and `help/` are generated, never independently edited. An illustrative resource-document example cannot override the catalogue. Unknown capabilities are not inferred from a command name.

## Organisation

```text
cq
├── check [PROVIDER...]
├── claude
│   ├── account  login | list | activate | remove
│   └── proxy pin  show | set | clear
├── codex
│   ├── account  login | list | activate | remove
│   ├── reset  list | recommend | use
│   └── proxy
│       ├── pin  show | set | clear
│       ├── fallback  show | set | clear
│       ├── reserve  set | enable | disable | clear | status | windows
│       ├── prime  status | enable | disable
│       ├── pool  set | rename | value
│       ├── session  bind | show | list | unbind | digest
│       ├── policy  apply | show
│       ├── lease invalidate
│       ├── trace
│       ├── fixture create
│       ├── readiness show
│       ├── validate  http | websocket
│       ├── canary  start | status | stop
│       ├── hook stop
│       └── credential-endpoint legacy
│           inspect | prepare | resume | activate | finalise | rollback
├── gemini account show
├── proxy
│   ├── serve | status | health
│   ├── state initialise
│   ├── candidate
│   │   ├── prepare | status | start | stop | remove
│   │   ├── client-safety refresh
│   │   ├── release  activate | validate
│   │   └── receipt show
│   ├── rescue  enter | exit | status
│   └── operation status
├── service  install | start | stop | restart | status | uninstall
├── auth refresh [PROVIDER...]
├── models
│   ├── list | refresh
│   └── overlay  add | remove | prune
└── help | version | completion
```

Provider-first scope makes Codex-only capabilities explicit. Shared proxy processes still serve several providers; service ownership, health and isolated release candidates therefore stay under shared roots. Gemini remains inspectable without inventing unsupported multi-account management. No mutation silently chooses Codex because the user happens to use Codex most often.

A native-client account, a proxy pin, a routing fallback, a pool and a session binding are separate resources. `account activate` changes the native client's selected identity. `pin` constrains new/unbound proxy work. `fallback` names the configured terminal fallback account, not an unconditional preferred account and not permission to violate continuity. `reserve` protects only the current system account's quota. `reset use` consumes a provider reset credit. `prime` triggers eligible quota-window priming. None implies another.

Singular namespaces name resource kinds. `show` reads one resource; `list` enumerates resources; `status` reports operational state. Existing `models` stays plural because renaming it adds little clarity. `serve` means foreground process; `service start` means managed background process. Every intermediate group prints help on bare invocation, exits 0 and reads no state. The sole intentional exception is bare `cq`, which remains equivalent to `cq check` and checks all three providers.

## Exact parser contract

1. Input is the operating system's argv sequence. CQ does not invoke a shell, expand globs, split values on spaces or expand `$VARIABLE` inside an already supplied value. Examples use POSIX shell quoting; shells perform their own expansion before CQ starts. Windows users must pass equivalent argv using their shell's quoting.
2. Command tokens and option names are exact, lowercase and case-sensitive. No prefix abbreviations, guessed providers, fuzzy account matches, merged short options or single-dash long options. Provider IDs are `claude`, `codex`, `gemini`; only explicitly listed commands accept a given ID. `anthropic` is a deprecated model-provider input spelling, not a fourth provider.
3. Global `--help/-h`, `--json/-j`, `--version/-v` may occur anywhere before `--`, including between command tokens. Local options may occur after the complete command path, before/between/after positionals. A local option before the complete command path is an unknown option at that group: exit 2. This keeps group navigation deterministic. Argument names in the catalogue are schema labels; users type values, not those names.
4. Value options accept `--name VALUE` and `--name=VALUE`. Short options accept `-j` and `-j=true`; no short value-taking options currently exist. Boolean flags accept a bare flag for true or `=true`/`=false`; a separate `false` is a positional token, not a boolean value. Empty values fail unless that parameter explicitly permits an empty string. A value beginning with `-` must use `--name=VALUE`.
5. `--` terminates option parsing. Subsequent tokens are positional values, including strings beginning with `-`. It does not terminate or abbreviate a command path. A command with no remaining positional slots rejects those tokens. Root `--` with nothing after it still invokes default check.
6. Nonrepeatable options may occur once. Long and short aliases count as the same option. Old and new spellings count as the same option. Repetition is an error even when values agree. Repeatable options preserve occurrence order; a command's set/uniqueness rule applies after parsing. Commas are ordinary characters unless an option expressly defines comma syntax; no current repeatable account/provider parameter uses comma splitting.
7. Unexpected positional values, unknown options, missing option values, bad types, out-of-range numbers and mutually exclusive selectors fail with exit 2 before credentials, network, locks, services or writes. Required consent and state identity checks have their separately specified exit 6. There is no generic `--dry-run`, `--force`, `--quiet` or `--verbose`.
8. Help bypasses missing required command parameters and their value validation, but command/option names and supplied option arity must be recognised. Thus `cq codex proxy reserve set --help` succeeds; `cq nonsense --help` and `cq proxy health --port --help` fail usage. No help path reads environment, discovers credentials, contacts services or creates files. `cq help PATH...` resolves the same exact path as `cq PATH... --help`. Unknown help paths fail exit 2.
9. A true help flag and a true version flag together fail exit 2. Otherwise version bypasses required parameter/value validation like help, prints version output and exits 0. False control flags do not alter execution. Help is always plain text, including `--json`. Version with `--json` uses the version command envelope.
10. Duplicate-key JSON input is invalid, including nested objects. Structured user input rejects unknown keys unless its resource contract expressly preserves a provider-owned schema. JSON must contain exactly one value followed only by whitespace, encoded as UTF-8 without BOM. Named size limits include all input bytes. No YAML, comments, trailing commas or implicit coercion.

### Primitive values

- `integer`: base-10 digits, no exponent, decimal point, plus sign, whitespace or separators. A minus sign is allowed only for a parameter whose range includes negatives. Leading zeroes are accepted and parsed as decimal; output uses canonical decimal.
- `number`: finite decimal number, optional decimal fraction; no exponent, NaN, infinity or locale commas. Bounds are inclusive unless help says otherwise. Percentage points refer to the full quota window, not a fraction of current remaining quota.
- `duration`: Go duration syntax with `ns`, `us`, `µs`, `ms`, `s`, `m`, `h`, including combinations and fractions; no days unit and no bare numeric seconds. Positive unless explicitly stated; numeric bounds are applied to total nanoseconds. Output duration formats are declared per field.
- `string`: valid UTF-8, no NUL. A narrower control-character, length or whitespace constraint comes from that parameter. CQ applies only the normalisation explicitly declared by that selector contract. Account email/alias trimming follows the account resolver; opaque keys, model IDs and digests are not guessed. Explicit pool-name normalisation is separate.
- `Timestamp`: UTC RFC3339 string ending `Z`, with optional fractional seconds of up to nine digits; emit the shortest exact nanosecond representation. Input timestamp exceptions in frozen vendor/receipt formats are explicit in their resource contracts.
- `digest`: the algorithm comes from that particular field. A session digest is keyed; a release digest is a verified bundle field; a rollback-receipt digest is domain-separated. Equal-looking 64-character encodings do not make them interchangeable.
- `path`: shell-expanded literal path. Ordinary user file inputs may be relative and resolve against process cwd. Owned candidate/state paths require the absolute, clean, non-symlink and ownership checks declared by that command. Output paths do not gain automatic `~` expansion. A parent may be created only where effects explicitly permit it.

### Deadlines, interruption and consent

Where `--timeout` exists it bounds elapsed operational work: local preparation, locks, requests, verification and cleanup. Time spent waiting for an interactive confirmation is excluded. Do not reset the timer between phases or add an undocumented cleanup extension. Candidate cleanup reserves and WebSocket's five-second cleanup reserve are **inside** the declared total. Models/auth and some local inspection commands expose no overall deadline; their exact existing phase/request bounds are specified in the resource contracts. A fixed deadline is documented rather than exposed as a flag accepting only one value.

A timeout or interrupt does not prove that a remote request was not applied. Persist and report known outcomes; preserve uncertain state for inspection. Never automatically repeat reset-credit consumption after an uncertain response. SIGINT cancels, runs bounded cleanup, emits the command's terminal result and exits 130. Successful cleanup does not turn the interrupted command into exit 0. Killing the process externally without letting it run cannot guarantee a terminal output record.

`--json` and non-terminal stdin disable terminal prompts. They never grant consent. Account removal/reset use require `--yes` under those conditions. Other commands retain their named semantic confirmation flags. Legacy endpoint `--non-interactive` suppresses a typed phrase only when the required semantic confirmation is also supplied. No global yes/force override exists. Prompt text and previews go to stderr; structured stdout remains parseable. Consent is evaluated before any mutating phase.

## Output and errors

Every normal `--json` result is exactly one UTF-8 JSON object plus LF on stdout:

```json
{"schema_version":2,"command":"codex proxy reserve status","ok":true,"data":{},"errors":[],"warnings":[]}
```

`command` is the resolved canonical path without `cq`, including for aliases. For an unresolved root usage error it is the empty string; for an error below a known group it is the longest recognised group path. For a single-result command or terminal stream record, `ok` is true exactly for exit 0. An interim stream event uses `ok` for that event alone; a ready/trace event cannot predict the later process exit. `data` is the command's declared object, or null if no valid result exists. Partial failure retains valid result data and errors. `errors` and `warnings` are arrays of `{code:string,message:string}`; no omitted envelope fields. Codes use lowercase snake_case. A successful no-op is success only where the command defines it. JSON object key order is not semantic; arrays follow the declared ordering. No ANSI escapes or progress text appear on structured stdout. Dynamic error substitutions are escaped/sanitised strings, never raw provider bodies, tokens or credentials.

Exceptions: help/group output is plain text; shell completion without `--json` is source text; proxy serve and trace use their declared JSONL stream envelopes; hook stop without `--json` uses the frozen hook JSON protocol. Machine-only ABIs follow their frozen protocol. None is permitted to change operation scope merely because `--json` was supplied. Trace explicitly exposes captured payload content only when requested; redaction covers known authentication fields and headers, not arbitrary secrets typed into user content.

Human templates in command/resource contracts are normative plain output. Interpolated booleans are lowercase `true`/`false`. Nullable values render `—` unless a template specifies a different fallback. Arrays render the documented loop zero times when empty; no undocumented “none” row. Strings escape all Unicode Cc control characters (including C0, C1 and DEL) as literal `\u` plus four lowercase hex digits, preserving ordinary Unicode. Numeric output uses a decimal point independent of locale. Human errors are `cq: MESSAGE\n` on stderr. Warning messages are `cq: warning: MESSAGE\n` on stderr. JSON carries corresponding warning objects; it may also emit the warning line on stderr, never mixed into stdout.

| Exit | Meaning |
| ---: | --- |
| 0 | Complete success or explicitly defined successful no-op |
| 1 | Operational failure: I/O, transport, provider or internal operation |
| 2 | Invalid command, arguments, options or structured input |
| 3 | Requested account, resource or file not found |
| 4 | Capability/platform unavailable or explicitly retired |
| 5 | Authentication failed or credentials require renewal |
| 6 | Conflict, missing confirmation, ownership or state precondition |
| 7 | Deadline expired |
| 8 | Partial result or partially applied operation |
| 130 | Interrupted |

Common parser failures use the following exact messages. Specific command error contracts override only their declared conditions, not the parser's before-effects requirement.

| Code | Exit | Exact message |
| --- | ---: | --- |
| unknown_command | 2 | `Unknown command: {token}. Run cq help.` |
| unknown_option | 2 | `Unknown option: {option}. Run cq {path} --help.` |
| missing_option_value | 2 | `Option {option} requires {metavar}.` |
| duplicate_option | 2 | `Option {option} may be specified only once.` |
| missing_argument | 2 | `Missing required argument: {metavar}.` |
| missing_option | 2 | `Missing required option: {option}.` |
| unexpected_argument | 2 | `Unexpected argument: {value}.` |
| invalid_argument | 2 | `Invalid {name}: {constraint}.` |
| conflicting_options | 2 | `Options {options} cannot be used together.` |
| invalid_json | 2 | `Invalid JSON input: {safe_reason}.` |
| interrupted | 130 | `Operation interrupted; inspect state before retrying.` |
| internal_error | 1 | `Operation failed because of an internal error.` |

`{option}` is the canonical long spelling; `{name}` the canonical parameter name; `{constraint}` the failed catalogue constraint; `{options}` the conflicting long spellings sorted lexicographically and joined with `, `. `{path}` is the resolved path, omitted together with its following space at root. `{safe_reason}` identifies class/location without echoing sensitive input. Print one primary parser error, selected by first offending argv position; missing required values follow catalogue order. Once execution begins, command-specific partial-result and precedence rules apply. If no narrower rule exists: interrupt, timeout, partial mutation/result, authentication, conflict, not-found, unavailable, operational failure, success, in that order. Output serialization failure exits 1; a failed stdout stream cannot promise a valid JSON document.

## Compatibility and capability boundaries

Version 1.0.0 preserves documented old command spellings for one major release as deprecated aliases, except explicit retirements. Remove aliases no earlier than 2.0.0. No aliases appear in primary group help or completion; migration documentation lists them. Alias help resolves canonical help. A deprecated command path suppresses redundant option warnings for that invocation. On an unchanged path, each distinct deprecated option emits one warning in argv order. A deprecated spelling emits one `deprecated_alias` warning with `Deprecated syntax; use cq {canonical_path}.` An unchanged path receives no path warning; an old option on that path still receives `deprecated_option` with `Deprecated option {old}; use {new}.` Canonical bare `cq` is never deprecated. Frozen internal ABIs are not subject to this expiry.

Aliases preserve operation intent and parameter values, **not** old JSON shapes, ignored options, output-scope changes or old exit-code bugs. Unknown tails reject before effects. Provider ambiguity never selects Codex. Bare old `proxy pin` is retired with a usage error directing users to the two provider-specific show commands. `operation recover` is unavailable because its old implementation only inspected state; it must not claim recovery. `proxy status --port` translates to health, while canonical proxy status always reports the same broader scope in human and JSON formats. See every exact translation in migration.json.

Candidate `start`, `client-safety refresh`, `release activate` and `release validate` have a precise **unavailable** contract in this version: validate syntax, then exit 4 with the specified evidence-unavailable error before pretending to perform a qualified transition. Their documented success prerequisites describe reserved lifecycle semantics, not permission to implement a stub that claims proof. Enabling them requires a separate complete proof-backend contract. The current control-server stub and derived hashes cannot satisfy it. Candidate preparation remains bookkeeping: source-config, credential-manifest and policy-snapshot are attested bytes and are not applied configuration.

## Implementation acceptance

- Generate parser, help and completion metadata from one catalogue. No independent hand-written help trees. Every declared leaf and intermediate group must be reachable.
- For every option, test both value spellings, bounds, omission, duplicate forms and rejection before effects. Test `--` and quoting by passing argv arrays, not shell strings.
- For every group and leaf, compare help byte-for-byte with this package, with no home directory, credentials, network or daemon available. Verify help/version side-effect freedom.
- Test globals before/between/after command words; test local options after the full path and reject them before it. Test missing, unknown, duplicate and incompatible arguments before acquiring mutating dependencies.
- For every JSON command, compare logical operation scope with human mode, validate envelope/resource fields, verify exit codes and prove stdout contains no diagnostics or credentials. Test empty, partial, error and interrupted results.
- Test noninteractive and JSON consent rejection before mutation; prove `--yes` cannot substitute for semantic confirmations. Test uncertainty without replaying remote consumption.
- Run all canonical/legacy mappings against the same handlers; cover every old flag/positional and each explicit retirement. Preserve every frozen machine-ABI byte/order/descriptor contract.
- Verify platform service ownership and periodic refresh idle health separately from continuously running proxy health. No ordinary account, quota or model command installs a service implicitly.
- Verify current-system-account-only reserve, shared versus isolated state directories and exact account identity resolution. Provider-wide scopes must never silently narrow to Codex.
- Retain frozen quota mathematics and internal formats. Run relevant Go tests with `-race`, plus build/vet, during implementation. This specification itself is checked without running CQ, provider requests or services.

Regenerate and verify this specification package:

```sh
python3 specs/cli-v2/generate.py
python3 specs/cli-v2/generate.py --check
```
