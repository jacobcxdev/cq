# Environment and path resolution

Normative target CLI specification, grounded in released v0.32.5 source at `release v0.32.5 source` and the current checkout. No environment values, credential files or token values were read for this review. Where existing implementations disagree, the canonical decision and required change are explicit below.

## General resolution rules

- Resolve only inputs required by the selected command. Help, version, group help and parser errors require no HOME, credentials, writable directories or valid environment configuration.
- Unset and empty environment variables have the same meaning unless explicitly stated otherwise. Do not trim path values: spaces are literal path characters. Never expand `~`, `$VARIABLE`, command substitution or shell syntax inside an environment value or CLI path; expansion done by the user's shell has already happened before CQ starts.
- Ordinary user input paths may be relative; resolve once against the process working directory at command start and clean lexical `.`/`..` components. An error obtaining that directory is `environment_path_invalid`, exit 2. Embedded NUL is invalid. Owned-state roots follow stricter command rules: clean absolute non-root path, rejecting relative paths and paths whose cleaned spelling differs. CQ does not resolve symlinks merely to redefine its root identity; owned-state validation subsequently checks ownership, symlinks and descriptor identity.
- Explicit CLI option wins only for the resource that option names. It does not change unrelated roots or persist a setting unless that command explicitly promises persistence. There is no generic `CQ_HOME`, `CQ_CONFIG`, `CQ_PORT`, `CQ_STATE_DIR`, `CQ_ACCOUNT`, `CQ_TOKEN` or environment credential override.
- Explicit invalid public configuration fails before mutation. Canonical `environment_path_invalid` exit 2: `Environment variable {NAME} must name an absolute path.` Other home/OS-root resolution failures use `environment_root_unavailable`, exit 4: `Cannot resolve the user storage root.` Never include secret-bearing values in error messages.

## Public environment variables

| Variable | Scope | Exact precedence, empty and invalid behaviour |
|---|---|---|
| `CQ_TTL` | Ordinary provider quota cache reuse | No CLI TTL option exists. Unset/empty means 30 seconds. Parse a signed base-10 integer using Go `strconv.Atoi`; leading `+`/`-` are accepted, whitespace, suffixes, fractions and integer overflow are invalid. To preserve released behaviour, invalid strings use 30 seconds, negative integers clamp to 0 and values above 3600 clamp to 3600. Zero prevents reuse. Explicit fresh/check bypass wins over TTL. Reset-credit list/recommend/use always fetch their required fresh inputs and do not inherit TTL as an authority to reuse stale data. |
| `XDG_CONFIG_HOME` | Unix CQ config/state and Linux user-service definition base | Unset/empty => HOME/.config. Non-empty must be absolute; canonical target rejects relative values instead of silently falling back. Config root is BASE/cq, not BASE itself. Windows ignores this variable and uses subject-user OS anchors. |
| `XDG_CACHE_HOME` | Unix quota/history/reset-attempt cache base | Non-empty absolute => BASE/cq. Canonical target rejects relative non-empty values. Unset/empty => platform cache base: HOME/Library/Caches on macOS, HOME/.cache on Linux; append cq. Windows ignores this variable and uses subject-user LocalAppData/cq/cache. |
| `HOME` | Unix native credential homes, default CQ roots, Claude global config and macOS service files | Non-empty absolute HOME is the home root, matching `os.UserHomeDir`. Empty/unset fails when home is required; no passwd-database or working-directory fallback. Canonical target rejects relative HOME. An explicit XDG root may avoid needing HOME for an isolated config/cache lookup, but native credential commands and macOS log/service resolution still require home. |
| `CODEX_HOME` | Codex model cache, client-version cache and supported native client-discovery inputs | Non-empty => that directory; relative value resolves against command-start working directory. Unset/empty => HOME/.codex. Model cache is resolved directory/models_cache.json. This variable DOES NOT relocate CQ managed/system Codex credentials, registry or durable credential-owner state. No error merely because cache is absent; malformed cache is a data error for model list, while client-version discovery proceeds to its next documented fallback. |
| `CLAUDE_CONFIG_DIR` | Claude model-capability cache only | Non-empty => that directory; relative value resolves against command-start working directory. Unset/empty => HOME/.claude. Canonical reader and publisher both use resolved directory/cache/model-capabilities.json. It DOES NOT relocate HOME/.claude/.credentials.json, HOME/.claude.json or native Keychain entries. |
| `PATH` | Executable discovery and Go installation destination verification | Normal OS executable search for codex and required platform tools. An explicit --client-executable path, when supported, takes precedence for that validation only. Missing executable is an explicit unsupported/unavailable result or documented version-discovery fallback, never implicit installation. No shell is used to evaluate PATH entries. |
| `USER` | Legacy macOS Claude Keychain account label | Released UpdateKeychainEntry uses non-empty USER, otherwise literal `unknown`; this labels the Keychain item only and does not establish security identity, directory ownership or authorisation. Retain for compatibility, but do not advertise as account-selection flag. |

`CQ_TTL` compatibility parsing is intentionally unusual but precisely defined. It must not be confused with command `--timeout`, which is a Go duration and has per-command bounds. The future schema may deprecate permissive TTL parsing only through an explicit versioned change; this specification does not add a second TTL setting.

`ANTHROPIC_BASE_URL` configures a compatible external client to contact CQ. CQ does not consume it as its upstream override. Provider upstream endpoints come from `proxy.json` fields. In particular, setting ANTHROPIC_BASE_URL must not recursively redirect CQ's Anthropic upstream back to itself.

## Derived root table

Define H = resolved home, C = configuration root, S = state root, K = cache root, R = runtime root, L = logs root.

| Platform | C | S | K | R | L |
|---|---|---|---|---|---|
| macOS | (absolute XDG_CONFIG_HOME or H/.config)/cq | C/state | (absolute XDG_CACHE_HOME or H/Library/Caches)/cq | S | H/Library/Logs/cq |
| Linux | (absolute XDG_CONFIG_HOME or H/.config)/cq | C/state | (absolute XDG_CACHE_HOME or H/.cache)/cq | S | S/logs |
| Windows | subject RoamingAppData/cq | subject LocalAppData/cq/state | subject LocalAppData/cq/cache | subject LocalAppData/cq/runtime | subject LocalAppData/cq/logs |

Windows anchors are obtained from the current subject user's authenticated OS known-folder/registry resolution in `internal/userdirs/dirs_windows.go`, not from untrusted `APPDATA`, `LOCALAPPDATA`, `HOME`, `XDG_*` or another user's shell strings. They must be clean absolute local drive paths (`C:\...` style), not UNC paths, drive-relative paths, rootless paths, colon-bearing alternate data stream paths or NUL-containing strings. Canonical native credential home on Windows is the same subject-user profile anchor. Released `fsutil.OSFileSystem.UserHomeDir` instead wraps Go `os.UserHomeDir` (USERPROFILE on Windows); unify that boundary to avoid splitting CQ roots and credentials between user identities. This is a required implementation delta, not a claim of released equivalence.

No `XDG_STATE_HOME` or `XDG_RUNTIME_DIR` override is implemented. Setting either does not change S or R. No `TMPDIR` persistent-cache fallback is permitted by the canonical target: if home/platform roots cannot be established, report unavailable rather than persist reset-attempt authority in a shared temporary namespace. Current Unix resolver has a final TempDir/cq-cache fallback; remove it for canonical persistent roots (see contradictions).

## File and directory layout

| Resource | Canonical path |
|---|---|
| Proxy configuration, including local token | C/proxy.json |
| User model overlays | C/models.json |
| Compatibility epoch | S/compatibility_epoch |
| Proxy rescue bootstrap | S/proxy-rescue.json |
| Default credential-control endpoint/journal base | S; platform-specific endpoint and journal filenames remain internal, not selectable with an unrelated model-home variable |
| Default Codex runtime readiness/canary base | S |
| Default Codex continuity/lease base | S unless proxy.json codex_continuity_state_dir is non-empty |
| Policy/runtime-resilience authority | proxy.json proxy_resilience_state_dir; empty means disabled/uninitialised and does not invent a directory |
| Candidate instance root | Exact command --state-dir; required where declared, clean absolute non-root; never falls back to installed S |
| Standalone explicit validation state directory | Exact --state-dir where declared; omitted means S, not C |
| Provider quota cache | K/claude.json, K/codex.json, K/gemini.json; provider-specific additional cache rows are implementation details |
| Burn-rate history | K/burn_state_v2.json |
| Durable reset attempts | K/reset-attempts-v1/; internal hashed filenames must not be manually derived |
| Claude credentials file | H/.claude/.credentials.json |
| Claude global settings/model picker | H/.claude.json |
| Claude capability cache | (CLAUDE_CONFIG_DIR resolved or H/.claude)/cache/model-capabilities.json |
| Codex system credentials | H/.codex/auth.json |
| CQ-managed Codex credentials | H/.codex/accounts/; owned candidate files and registry.json |
| Codex external credentials | Existing declared read-only source paths from account inventory; neither CODEX_HOME nor CLI account selection grants ownership |
| Codex model cache | (CODEX_HOME resolved or H/.codex)/models_cache.json |
| Antigravity project cache | H/.gemini/antigravity-cli/cache/default_project_id.txt |
| Gemini native credential entry | OS credential service `gemini`, account `antigravity`; not an environment variable or user-supplied token file |
| macOS proxy LaunchAgent | H/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist |
| macOS token-refresh LaunchAgent | H/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist |
| macOS service logs | L/proxy.log and L/refresh.log |
| Linux user units | (absolute XDG_CONFIG_HOME or H/.config)/systemd/user/cq-proxy.service, cq-refresh.service and cq-refresh.timer |

All paths above are symbolic, never literal shell expansion instructions. The specification does not direct users to edit credential files, reset attempts, receipts or compatibility epoch state. Local secret-bearing files retain mode 0600 and owned secret directories mode 0700 on Unix. Reading an account inventory must not create directories merely to calculate one of these paths.

## Proxy listen port and authority-root precedence

1. For commands explicitly declaring --port, a supplied valid integer 1..65535 selects only that command's listener/probe/control target. Explicit zero is invalid; it is not an omission sentinel. Candidate-validation commands retain their additional forbidden live-port 19280 rule.
2. Otherwise read proxy.json port. Missing configuration, omitted port or stored port=0 resolves to 19280 for read-only probe discovery; malformed config or negative/>65535 port fails rather than probing a different port.
3. Foreground serve uses --port before stored port before 19280. Its override is ephemeral and never writes shared proxy.json merely to retarget the run. Missing config creation is allowed only by the explicit mutating command's contract, never by help/status/health.
4. Loopback HTTP control/probe addresses are `http://127.0.0.1:PORT/...`. There is no public bind-address environment variable or implicit all-interface bind.
5. Non-empty codex_continuity_state_dir and proxy_resilience_state_dir must be clean absolute non-root paths; relative, whitespace-only, root `/`, lexical `..`/`.` variants or NUL-containing input fail. Whitespace-only is not equivalent to unset.
6. Continuity default is S. Resilience root has no default. An explicit offline --state-dir, where a command provides it, selects that operation's offline authority root and must not silently alter proxy.json. The explicit state-initialise command may persist its selected root according to its own contract.
7. Endpoint/listener and candidate state identity remain bound by the lifecycle/receipt rules. A --port override does not authorise moving existing authority roots or treating a candidate as the installed instance.

Default upstreams: `https://api.anthropic.com` and `https://chatgpt.com/backend-api/codex`. Empty stored upstream field uses its respective default. These are config fields, not guessed from ANTHROPIC_BASE_URL/OPENAI_API_KEY. Configuration validation must reject unusable schemes/hosts before serving, even though released Config.validate only parses URLs permissively.

## Service process environments

Interactive shell environment belongs to the invoking process; already-running launchd/systemd/Windows services do not automatically inherit later shell exports. Installing or restarting a service is not permission to copy arbitrary shell variables or credentials into service definitions. Canonical installation records the resolved non-secret roots and executable identity needed by the selected component through its existing managed launch definition/authority mechanism; status reports the installed component's actual roots, not assumptions from today's shell.

No new public environment-forwarding flag is introduced. When an installed process has different root configuration, operations target its declared authority or return conflict; they must not silently create a second live account/authority namespace. Account cache-home overrides do not retarget existing listener identity. Exact environment binding/persistence belongs to the shared service schema and requires validation against its platform definitions.

## Go installation and platform environment

For Go-managed executable destination resolution only: non-empty GOBIN takes precedence and must be a clean absolute existing writable directory. Otherwise use non-empty GOPATH, or run `go env GOPATH` if empty/unset; select the first clean absolute entry in the OS path list, then append `bin`. The directory must already exist, be writable, and appear in PATH. Result is directory/cq on Unix or directory/cq.exe on Windows. Invalid explicit GOBIN fails; no fallback to GOPATH. GOPATH with no valid absolute entry fails. No command edits shell PATH/profile automatically. These Go installation environment variables do not relocate CQ data or model caches.

Ordinary network clients using Go's default transport inherit standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY (and lowercase equivalents); loopback destinations and isolated acceptance transports follow their explicitly local/no-proxy transport policy. These are transport settings, not CQ credential or model-provider selectors. Do not assume that a generic shell proxy setting changes the installed service environment. This specification does not invent separate CQ-specific transport environment variables.

## Internal knobs are not public configuration

`CQ_LINUX_VALIDATION_BACKEND` and `CQ_LINUX_VALIDATION_CREDENTIAL` belong to the controlled Linux validation harness (`cmd/cq/proxy_http_validation_runtime_linux.go:35-36,120`). They are not documented alternatives to account discovery, production backend selection or the credential-owner contract. Parent code constructs them for isolated child validation; ordinary callers must not gain production authority by setting them. Test fixtures, linker-provided `version`/Gemini OAuth client secret, inherited runtime descriptors and release harness variables are not public CLI parameters. No runtime environment override for the release-linked Gemini OAuth client secret is added.

## Observed contradictions and required changes

1. README says XDG bases must be absolute (`README.md:529-530`), but `internal/userdirs/dirs_unix.go:30,86` silently ignores relative values. Linux service install also ignores relative XDG_CONFIG_HOME (`cmd/cq/service_linux.go:142`), while installed attestation rejects it (`internal/proxy/codex_installed_systemd_linux.go:44-55`). Canonical behaviour is one upfront error for explicit relative XDG input on Unix; use the same resolver across installation and attestation.
2. README says continuity defaults to `~/.config/cq/` (`README.md:543`). Released/current serving code passes roots.State (`cmd/cq/proxy.go:861`), and readiness uses paths.StateDir (`internal/proxy/codex_readiness.go:303-308`). Canonical default is C/state. Do not migrate existing explicitly configured continuity roots implicitly.
3. Model-cache readers honour CLAUDE_CONFIG_DIR (`cmd/cq/models.go:525`), but registry publication hardcodes H/.claude/cache/model-capabilities.json (`cmd/cq/registry_pipeline.go:253`). Standalone helper honours the variable (`internal/proxy/models.go:107`). Canonical read and write must share the resolved model-cache path; global Claude picker stays H/.claude.json.
4. CODEX_HOME is non-empty-path tolerant in models (`cmd/cq/models.go:514`) but client-version discovery first demands UserHomeDir even when CODEX_HOME is supplied (`cmd/cq/codex_version.go:78`). Canonical resolver needs HOME only when no explicit CODEX_HOME is supplied. Resolve relative overrides once to an absolute path so subprocess working-directory changes cannot retarget them.
5. Unix cache code can fall back to TempDir/cq-cache (`internal/userdirs/dirs_unix.go:86-103`), but unified root resolution often fails first if config/log home is unavailable. Canonical persistent roots fail explicitly rather than creating an undocumented temporary reset-attempt namespace.
6. Windows CQ roots use authenticated subject anchors, while native credential fsutil.UserHomeDir wraps os.UserHomeDir/USERPROFILE (`internal/fsutil/fs.go:258`). Canonical roots and native credentials must share the subject-user profile anchor. XDG variables must not become Windows root overrides.
7. README's blanket H/.config paths are Unix defaults, not Windows paths and not absolute when XDG overrides apply. Use C/S/K/L terminology and concrete platform table throughout generated help/docs.
8. Current dirty source changes inspected for accounts/model rendering do not fix these path contradictions. Preserve those changes; apply any resolver changes in a later authorised implementation.

## Acceptance cases

Help succeeds with HOME unset and invalid XDG variables. An account command with required home unset fails without writes. Empty XDG uses defaults; relative explicit XDG fails consistently in config/cache/service/attestation paths. Absolute XDG relocates only intended CQ roots. Relative CODEX_HOME and CLAUDE_CONFIG_DIR resolve once against startup CWD, and read/publish choose identical paths. Neither variable moves native credential files. Model version lookup with absolute CODEX_HOME works even without HOME when no other selected operation needs home. Cache TTL tests cover unset, empty, 0, -1, +30, 3601, whitespace, overflow and invalid unit suffix. Explicit --port 0 fails, missing stored port resolves 19280, malformed stored port/config fails, and read-only discovery never generates a local token/config file. Candidate instance root never defaults to installed state. Windows poisoned shell APPDATA/USERPROFILE cannot redirect the canonical subject-user roots. No test or help output prints credential-bearing environment values.
