# Troubleshooting CQ

[Getting started](../README.md#quick-start) · [Usage](usage.md) · [Configuration and paths](configuration.md) · [Release operations](releases.md)

Start with the affected provider, client and exact command. Preserve the error, timestamps, CQ/client versions and whether the request went through CQ. A healthy process or `/health` response establishes reachability; a completed routed request establishes that request's path. Avoid changing several routing, VPN and credential settings at once.

For a workflow you can give Codex or another agent, use [Collect diagnostics and report an issue](agent-issue-reporting.md). It covers existing logs, temporary capture, restoration, redaction and the public [bug-report form](https://github.com/jacobcxdev/cq/issues/new?template=bug_report.yml).

## First inspection

These commands inspect existing state. They do not log in, switch an account, send a Dot message, consume a reset or restart a service:

```sh
cq --version
cq service status --json
cq proxy status --strict --json
cq codex network status
cq codex network usage status
cq codex dots status --json
```

Run only commands applicable to your platform/integration. Some other apparent inspections, such as bare pin/default or offline policy commands, can initialise CQ configuration/state when absent; use help first when preserving an untouched profile matters. Account and quota commands read provider credentials and can contact the provider or eligible managed credential coordinator; they are not pure offline inspection. `cq --refresh check PROVIDER` bypasses quota cache and makes a fresh read. A `check` report can contain individual provider errors even when the CLI exits successfully. Read `availability.reason` and each result's error.

## Installation and background services

| Symptom | Inspect / interpretation | Next action |
|---|---|---|
| `cq` is missing or unexpected version | Inspect your shell's executable resolution (`command -v cq` on Unix; `Get-Command cq` in PowerShell), package version and service executable in `service status`. | Repair using the installer that owns it. A second portable binary earlier in `PATH` can differ from the service runtime. |
| Proxy/refresh not registered after `go install` or archive extraction | These paths provide an executable only. | Use a complete installer, or intentionally use `service install` for development/repair after selecting its owner/executable. Do not run multiple package owners concurrently. |
| Homebrew reports a deferred upgrade | `service status --json` shows active/pending runtime versions and upgrade phase. Normal package drainage is bounded to 15 seconds. | Let active work finish; the retained candidate refresh service automatically retries the handoff. Inspect active/pending versions and phase rather than repeating the package upgrade. Read [Homebrew upgrades](homebrew-upgrades.md); pending candidate bytes are not the active version. |
| Legacy Homebrew Cask sandbox hook failure | Verify Caskroom symlink and `homebrew` service owner before changing anything. | Use the exact legacy procedure in [Homebrew upgrades](homebrew-upgrades.md#legacy-sandboxed-cask-hooks), retaining a rollback executable. |
| Linux service manager unavailable | `systemctl --user status cq-proxy.service cq-refresh.timer` and `journalctl --user -u cq-proxy.service` inspect the user manager/log. Units belong to the installing user. | Repair the user-manager/login-session prerequisite with the host administrator. Lingering may be needed to run after logout; CQ does not promise persistence without it. |
| Windows scheduled tasks absent/unhealthy | Inspect `\cq\Proxy` and `\cq\Refresh` in Task Scheduler and CQ service status as the same Windows user. | Run the owning installer repair; do not register an unrelated administrator's task. Configuration is Roaming AppData; runtime/logs are Local AppData. |
| Port already in use | Compare configured port, listener and service identity. A foreground process and installed service can compete. | Stop the process through its owner only when intended, or use a separate foreground port. Repoint clients deliberately. Do not kill an unidentified listener. |
| Service healthy but client fails | Confirm client base URL/provider and correlate a real request. | Follow [client setup](usage.md#connecting-a-client); strict status does not prove model entitlement, upstream quota or client compatibility. |

`service restart` changes both components and can interrupt work. Use it after a restart-required configuration change or a diagnosed lifecycle problem. Ordinary account/quota commands do not repair service registration. Package uninstall retains credentials, configuration, caches, history and logs; reinstalling is not a credential reset.

## Login and account discovery

| Symptom | Meaning / recovery |
|---|---|
| `not_configured` | No supported credential was found for that provider. Select only configured providers or use its login flow. For Gemini, sign in through Antigravity. |
| `no_token` / `auth_expired` | Credential missing, expired or rejected. CQ-owned eligible credentials renew through the coordinator; system/external accounts require their owner's sign-in. Expired Claude and Z.ai browser logins can need interactive reauthentication. |
| Browser does not open | Use the authentication URL printed by that login attempt on the same machine. Keep the command running and return to its loopback callback. Do not share the URL or callback query; it contains temporary authentication state. |
| Codex login cannot listen on port 1455 | Another local login/listener occupies the required callback port. Identify and finish that operation before retrying. CQ does not choose an arbitrary replacement port for Codex. |
| Login times out / callback rejected | Codex waits up to five minutes. Start a new login if its state expired, and complete that attempt rather than replaying another attempt's callback. Supported long opaque Codex authorisation codes are accepted up to the defensive 16 KiB bound; do not shorten or edit the callback value. |
| Signed in, but client still uses another account | A plain Claude/Codex login saves an account. `--activate` or `switch` explicitly changes the current system projection; running clients may need reconnection. Proxy routing remains independently selected. |
| Duplicate email/ambiguous account reference | Use the command's documented unique alias or opaque AccountKey where supported. `switch`/`remove` retain email semantics. Inspect `accounts` first. |
| CodexBar managed refresh outcome uncertain | CQ preserves the journal instead of replaying a rotating exchange. A newer owner login wins. Reauthenticate with the owner when required; do not delete journals to force a retry. |
| Gemini project/credential error | Credential service/account is `gemini` / `antigravity`; project cache is `~/.gemini/antigravity-cli/cache/default_project_id.txt`. Re-establish credentials/project through Antigravity. CQ does not invoke `agy` or write those stores. |
| Gemini works until token expiry only in a development build | Official releases include refresh link material; plain source builds do not. Install an official binary rather than inventing a runtime client-secret environment variable. |
| Saved Z.ai accounts seem ignored | `ZAI_API_KEY` selects one account, using saved OAuth only when that exact key's ownership was previously verified. Remove it from the relevant client/service environment to use the saved pool; do not print it while diagnosing. |

## Quota, routing and continuation

| Symptom | Meaning / recovery |
|---|---|
| Cached/stale quota or forecast warming up | Default TTL is 30 seconds. A transient account failure can retain a usable stale row with its error. Forecast history needs observations and restarts on resets/gaps. Use a fresh check, then inspect original errors. |
| `Codex usage runtime egress unavailable; no direct fallback` | An egress proxy, `direct`, or WireGuard profile is configured, so Codex checks borrow the runtime broker. Verify that runtime, port and egress configuration. Other providers keep their own transport. |
| WireGuard/profile change blocks requests | Invalid configuration, private-file permissions, uncovered DNS/destination routes or interface loss fail closed. Review the [profile constraints](configuration.md#proxy-configuration); CQ does not silently fall back to the host route. |
| 401 / 403 | Credential or entitlement rejected. Check the affected account, model and route before retrying; another account's catalogue does not grant this account access. |
| 429 / usage-limit error | Actual quota, configured reserve or provider capacity can reject work. Inspect the route reason, account and fresh quota. Do not assume all 429s mean an entire portfolio is exhausted. |
| Reserve blocks the system account | Other eligible accounts can still route. `reserve disable` is an explicit temporary bypass requiring fresh evidence; it does not consume a reset. Protection returns on reset. |
| `usage_stale` on a protected account | Fresh selected-window data is required for reserve safety. Diagnose the usage fetch/egress failure; stale data does not authorise reserve bypass. |
| `affinity_unresolved` or continuity mismatch | Account-owned continuation, prior-response state, session pool, pin and soft affinity have different authority. Capture the matching trace; do not erase leases or switch accounts blindly. Portable full-history requests can permit reselection where opaque prior state cannot. |
| Chat stays on its prior account after pin/default/pool changes | Hard continuity can preserve an existing route. Codex pin/default changes also need a restart. Lease redistribution requests reselection at safe boundaries; it cannot move an opaque HTTP `previous_response_id`. |
| Routing configured `enforce` but effective mode differs | Read the inhibition reason in status. Installed-client readiness receipts and canary requirements are separate from configuration. See [release operations](releases.md). |
| ZCode Start returns busy codes 3008/3009/3010 | These provider capacity errors were observed while tokens remained. CQ does not silently switch Start work to paid quota or replay an uncertain inference. Keep the provider error and retry deliberately when appropriate. |
| Z.ai quota works but inference fails | Quota access does not establish inference entitlement. Confirm the saved account and `responses` / paid `zcode` / `zcode-start` route. Standard API keys have different billing from Personal Coding Plan keys. |
| Old Z.ai `/responses/compact` returns 501 | Supported integration uses native Codex v2 compaction; the older endpoint is unsupported. Retain the proxy token so previously generated CQ compact state stays readable. |

## Models and harness profiles

Use `cq models list --provider codex` or `--provider zai` to distinguish registry discovery from client picker caching. `cq models refresh` publishes immediately; the running service also refreshes every five minutes. Failed sources retain the last usable catalogue. A model must be offered by the relevant source and entitled for the caller; a manually added overlay does not grant entitlement. Inspect CQ's own cache-root `model-registry.json` separately from the native client's account-scoped `models_cache.json`; CQ does not overwrite that native entitlement cache. The client must fetch CQ's model-discovery route before its picker can reflect it. `CODEX_HOME` selects native client/version discovery and the read-only native fallback; `CLAUDE_CONFIG_DIR` selects listing/compatibility capability paths, while registry pipeline publication remains at `~/.claude/cache`. Check the actual client endpoint and cache owner before changing paths. `models overlay prune` removes overlays now available natively; it is a write.

For profiles, run `cq codex harness inventory --project /path/to/project --json` and `cq codex harness explain --chat CHAT_ID --project /path/to/project --json`. `--chat current` requires a validated `CODEX_THREAD_ID`; it deliberately does not search transcripts. A saved definition is not a binding. `policy_disabled` means an existing disabled policy retained rules; choose explicit `harness enable` if you intend to activate them. Pending application is distinct from a policy preview and last recorded receipt. Normal-launch native enforcement is narrower than the launcher and can apply only after an observed request. First-turn and internal-subagent independence limits are described in [usage](usage.md#harness-capability-profiles).

## Reset schedules and uncertain operations

Codex recommendations are advisory unless a schedule is explicitly accepted. Manual additions start disabled with fixed timing. Adaptive mode moves only accepted credits; it does not enrol new ones. The runtime must run, and schedule status reports a fresh authenticated executor heartbeat. A date must be future RFC3339 with a timezone and fall before credit expiry.

An enabled overdue fixed entry can execute immediately. Disable pauses future execution; cancellation cannot undo an upstream request already sent. Unknown reset/claim outcomes remain durable and are not an invitation to consume another card. Retry through the documented same-identity path after inspection. ZCode window-selected card use and exact-card Z.ai schedules have different contracts. Never substitute a new request identity solely to clear an uncertain result. See [reset usage](usage.md#codex) and the [ZCode protocol ledger](zcode-routing.md).

## Native network and Dots

`cq codex network status` checks the signed extension and authenticated broker. Enabling requires the companion, running CQ and a valid WireGuard profile, plus any macOS approval. Existing connections retain their original route until they reconnect. Native network routing and usage response adaptation are separate switches. Usage handling also requires a prepared certificate and separately approved SSL trust for both printed hosts. Use `usage status` to inspect; `prepare` creates state, and trust commands change Keychain. Do not reinstall/trust certificates to repair an unrelated provider or routing error.

Dots requires a compatible installed renderer, an onboarded system Dot and free loopback port 8000. `dots status --json` reports compatibility, discovery, runtime and `restart_required` independently. A normal launch can have native network routing while still lacking the Dots adapter. Quit ChatGPT yourself before `dots stop` or reinstall, then reopen through the CQ launcher when intended. Missing/ambiguous renderer contracts stop startup; a version bump alone is not evidence of incompatibility. See [Dots lifecycle and limits](dots.md#account-changes-and-lifecycle).

Broadcasts, board notices and setup reminders are ordinary messages. If delivery is uncertain, inspect the chat before a new send; CQ does not automatically replay uncertain messages. Cached access denials for active configured Dots are rechecked through their owner, but deleted profiles remain deleted. Successful catalogue discovery does not establish cloud video, executor or all secondary-account capabilities.

## Diagnostics and privacy

### Routing diagnostics

`diagnostics_log` is unset by default. Set an absolute private log path and arrange an approved restart to enable it; clearing/restoring the field also requires restart. Every Codex HTTP request and WebSocket `response.create` frame receives a trace ID. Ordered JSONL events record ingress, request identity, candidate eligibility and exclusion, selected account hint, pool, durable lease state before and after transitions, credential refresh, upstream dispatch/status, retries, failover, relay result, close/error classification, and terminal outcome. WebSocket events also carry a stable connection ID. Route summaries share the same trace ID. Metadata and payload JSONL logs rotate at a 64 MiB target and retain four prior files plus the current file. Retention is size-based, not age-based; oversized records can exceed the target. This policy does not apply to macOS service stderr or Dots logs. Enabling diagnostics does not change routing policy.

Query one user-facing task, one trace, or recent traffic without manually searching JSONL:

```bash
cq proxy trace --session codex://threads/THREAD_ID --since 15m
cq proxy trace --trace trace:ID --json
cq proxy trace --follow
```

The command reads existing logs; it does not enable capture. Default output is the newest 200 matching records; use a narrow session/time filter with `--tail 0` when a complete retained trace is needed. `--json` emits original JSONL, not sanitised output. Ctrl-C stops `--follow`, not the proxy's logging. Follow the [capture and cleanup workflow](agent-issue-reporting.md#3-capture-one-bounded-reproduction-when-necessary) before changing log settings.

### Payload diagnostics

`payload_diagnostics_log` is disabled by default and requires restart both to enable and disable it. Codex trace capture records up to 64 KiB of each HTTP request and response body and each downstream and upstream Codex WebSocket frame. Other provider payload paths can record full buffered bodies; do not treat 64 KiB as a universal cap. Codex entries contain causal trace ID, direction, original byte count, encoding, account hint where applicable, and whether capture reached a complete body or was truncated. Common credential-bearing headers are excluded, but bodies and custom headers can still contain secrets. Query Codex records with `cq proxy trace --payload` and the same session/trace/time filters.

> **Warning:** payload diagnostics can contain prompts, system prompts, tool inputs, compact summaries, messages, provider responses, and other sensitive content. Do not share without review. Request and response bodies can themselves contain secrets.

Session and thread keys in diagnostics are short deterministic hashes, not raw identifiers. `session_source` records which header/body/WebSocket signal supplied session correlation.


## Sharing useful evidence

Use the [agent issue-reporting workflow](agent-issue-reporting.md#4-prepare-the-smallest-public-safe-report) and [bug-report form](https://github.com/jacobcxdev/cq/issues/new?template=bug_report.yml) for collection, public-sharing review and submission.

Retain exact command, versions/builds, timestamp and timezone, platform, configured/effective routing mode, service owner/executable and the smallest matching trace. Redact identities when needed. Do not share `proxy.json`, WireGuard profiles, credential stores, OAuth callback URLs, encrypted session keys or raw payload logs without a separate review. Include whether a result came from a synthetic test, candidate listener or installed live client. A sent message, healthy status or successful build is not proof of installed affected traffic.

## Grok subscriptions

Run `cq grok accounts` and `cq check grok`. Reauthenticate with `cq grok login`
when a CQ-owned session expires or renewal has an uncertain outcome. A CodexBar
source remains owned by CodexBar; repair or replace its login there. Unverified
cookies and public API keys are visible as unavailable sources, not inference
credentials. Healthy CQ-owned accounts remain available when an external source
is malformed.

If no Grok model appears, inspect `cq models list --provider grok`: the current
adapter publishes only native Responses models with known context. A missing or
Chat Completions backend is not silently converted. Unknown opaque continuation
state after restart requires a fresh readable conversation. Reset inventory
failure does not imply no rewards; redemption is not implemented. See
[current qualification limits](usage.md#grok-subscriptions).
