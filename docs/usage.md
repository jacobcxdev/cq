# Using CQ

[Start here](../README.md) · [Configuration](configuration.md) · [Troubleshooting](troubleshooting.md)

This guide covers everyday operations and links to the operator procedures. Run `cq COMMAND --help` before using a new command; commands shown with uppercase placeholders need values from your own account/model/status output.

## Quota checks and output

### Providers

- **Claude** discovers stored accounts, fetches profile and usage data in parallel, refreshes eligible tokens, and reports each account.
- **Codex** discovers system, CQ-managed, and declared external accounts. Ordinary checks do not activate, remove, refresh, or rewrite system credentials. Eligible CQ-owned credentials refresh only through CQ's coordinator.
- **Gemini** reads Antigravity Keychain credential and cached project ID concurrently, calls Antigravity OAuth and quota HTTP APIs directly, and never invokes `agy`. Credential/project stores remain read-only; refreshed tokens live only in process memory.
- **Z.ai** reads saved browser sessions or Coding Plan API keys. Paid subscriptions expose 5-hour and weekly quota; Start Plan routes expose their separate token pools and entitlement windows. Monthly MCP usage appears separately when supplied by the API. Reset times remain unknown when Z.ai omits them.
- **Grok** reports included subscription quota for multiple CQ-owned or verified borrowed CodexBar accounts, preserving the actual allowance period. Its subscription model catalogue is account-specific; public xAI API keys and purchased balances are outside this integration. See [Grok setup and current limits](#grok-subscriptions).

### Cache and history

Successful quota rows are cached by provider. Default TTL is 30 seconds; `--refresh` bypasses it. If one account has a transient fetch failure and a matching usable cached row exists, cq shows stale quota with original error context instead of hiding that account. Auth errors are never written as fresh cache data.

cq also keeps per-account/window burn history. Exhaustion ETA uses the **shorter of the recent-rate and whole-window-average estimates**: fast burns shorten it, while idle periods cannot extend it beyond the whole-window baseline. Recent rates use a 30-minute EWMA half-life, at least five minutes between samples, and a 15-minute observation warmup. Exact percentages are used when available. Resets, upward adjustments, precision changes, and observation gaps over two hours restart warmup; stale or mismatched snapshots fall back to the whole-window average. Matching cached reads can reuse a forecast for up to 15 minutes without advancing history.

Account, aggregate, and proxy-pool ETAs select the same rate per account before weighting capacity and consumption. Budget pace and the main gauge retain their existing window-based calculations; the secondary imminent-block warning retains its separate EWMA. Cache and history failures degrade to uncached/cold-start behaviour.

### TTY report

For each provider/account, cq can show:

- plan/account identity and active credential marker;
- remaining percentage and reset time for each quota window;
- usage pace and smoothed burn rate;
- projected burndown;
- aggregate coverage across two or more usable accounts;
- demand/supply gauge: overburn on the left, on-pace centre and underburn on the right, with gap/waste timing metrics;
- Codex proxy-eligible subset when discovery and routing eligibility differ;
- cached/stale and error context.

Account rows within every provider use the same TTY ordering: the active/current account first, then higher known account values, then higher known usage multipliers and matching tier labels, then the earliest relevant reset. Codex account value is the maximum value across all routing-pool memberships; a known account outside every pool has value zero. Providers without account-value metadata skip that key. Explicit valid multipliers are used for other providers, while Codex retains its documented plan fallbacks. Plans without known multipliers, including current Z.ai and Grok plan labels, group alphabetically by their case-insensitive tier name rather than receiving an invented capacity weight. The relevant reset belongs to the shared window with the least remaining allowance, or a scoped window when no shared window exists. When several windows are exhausted, recovery uses their latest reset. Unknown values, multipliers and missing or expired limiting resets follow known ones; email and account ID break remaining ties. Account selection follows the available report metadata, with fresher identity metadata taking precedence when supplied. This ordering affects TTY display only, leaving provider order, JSON order and routing decisions unchanged. Gemini's ordinary single-account display is unaffected.

Gauge position uses the demand/supply ratio `rho`, with 0.95–1.05 on pace. Overburn severity uses the capacity deficit `1 - 1/rho`, with moderate/severe boundaries above 20%/40%; underburn uses surplus `1 - rho`, with boundaries above 30%/60%. Gap deadlines and projected waste are displayed metrics, not period-based severity buckets. An imminent-gap warning preserves the natural ratio-derived position.

TTY icons require a [Nerd Font](https://www.nerdfonts.com/). Recommended: [`jacobcxdev/tap/liga-sf-mono-nerd-font`](https://github.com/jacobcxdev/homebrew-tap).

### Codex subscription labels and capacity

TTY output uses readable plan/seat labels while JSON retains machine-facing plan and tier metadata. The included-usage weights below are relative to a Plus allowance, as represented by CQ's current catalogue; they are not speed-mode consumption multipliers, guaranteed request counts or spend balances.

| Provider plan identifier | TTY label | Known included-usage weight |
|---|---|---|
| `plus` | `plus` | 1× baseline |
| `prolite` | `pro 10x` | Personal Pro 10× |
| `pro` | `pro 20x` | Personal Pro 20× |
| `promax` | `pro 50x` | Personal Pro 50× |
| `team` | `business` | Standard Business 1× baseline |
| `self_serve_business_prolite` | `business premium 5x` | Business Premium 5× |
| `self_serve_business_usage_based` | `business` | No fixed included-usage weight inferred |

The baseline 1× suffix is omitted in TTY labels. An explicit valid positive upstream tier suffix such as `_10x` takes precedence over a plan fallback. An unfamiliar or malformed explicit tier does not silently borrow the plan's weight. Historical snapshots retain their recorded tier; current catalogue fallbacks do not rewrite old observations. Personal Pro 10×/20×/50× and Business Premium 5× are separate tiers; CQ does not treat every Business seat as personal Pro.

Other recognised plan names have readable labels, including enterprise, automation, education and research variants, without inventing a fixed allowance. Unknown plan identifiers remain visible. When a usable Codex account in an aggregate has no known fixed capacity, CQ omits that capacity-weighted aggregate/summary rather than assuming 1×; its individual quota row remains available. The same rule applies to the relevant proxy-pool subset. An omitted aggregate is not proof that the account is depleted.

Quota JSON preserves `plan` and `rate_limit_tier` metadata instead of replacing those fields with a prettified label. The tier can be reported upstream or derived from CQ's documented plan fallback when absent. Account-list JSON similarly retains its machine-facing label/tier values. Consumers should use these fields, not parse the TTY text.

The plan label identifies a subscription or seat type, not the organisation's name. When credential/usage data does not provide an authoritative business/workspace title, CQ shows the available email or account identity and leaves that title absent. It does not guess a business name from an email domain, a plan identifier or an opaque workspace ID.

### JSON report

Redirect or pipe JSON for machine consumption. A terminal report can be pretty-printed and coloured; redirected JSON is encoded without terminal colour. Trace `--json` is JSONL, with one event per line, rather than the quota report's single document.

```bash
cq --json
cq --json check codex
cq codex accounts --json
cq gemini accounts --json
```

JSON includes provider results, aggregates, cache age, error codes, proxy eligibility, and an `availability` object for automated consumers:

- `available`: normal work is safe.
- `limited`: quota is low; conserve it for small, necessary, or approved work.
- `exhausted`: new work should not route there without explicit override.

For example, with optional `jq` installed:

```sh
cq --json check codex | jq '.providers[] | {id, availability}'
cq --json check codex | jq '.providers[].results[] | {email, status, error}'
```

The report root contains `generated_at` and `providers`. Each provider has `id`, `name`, `availability`, `results` and optional `aggregate`, `proxy_eligibility` and `proxy_pools`. Results contain identity, windows, status, cache/error information and provider-specific metadata. Optional fields may be absent. Account listings, model listings, service snapshots and reset schedules have their own output shapes; do not parse one as another. See the current domain definitions in [`report.go`](../internal/app/report.go) and [`quota`](../internal/quota) when building a consumer.

Availability uses the best usable account: `limited` means a known remaining margin from 1% through 5%; `exhausted` also covers `reason: unavailable` when every result is an error. Inspect `reason` and result errors before treating that state as confirmed quota exhaustion. Unknown remaining percentage can be `-1`; an absent reset is not an immediate reset. Missing windows are not zero quota. The dashboard can complete successfully while individual providers report errors, so scripts must inspect the JSON as well as the process exit status. `proxy status --strict` deliberately makes unhealthy reconciled state a non-zero result; plain service/status inspection has different semantics.

Account-level `active` means credential default/current account. It is not a proxy routing decision; pins, bindings, eligibility, continuity, and failover can select another account.

## Accounts and authentication

### Claude

A plain `login` adds a stored account; `--activate` also writes its active credential projection. `switch` changes that projection. Removing a stored account and upstream account deletion are separate operations.

```bash
cq claude login --activate
cq claude accounts
cq claude switch EMAIL
cq claude remove EMAIL
```

Claude login uses browser OAuth. Stored account credentials support multi-account checks and proxy routing.

### Codex

A plain `login` stores the account for CQ. `login --activate` and `switch` explicitly change the system credential used by clients; automatic routing never does this. Switching can require reconnecting clients or MCP servers. Use `accounts` to inspect identity before removal.

```bash
cq codex login --activate
cq codex accounts
cq codex switch EMAIL
cq codex remove EMAIL
cq codex resets list
cq codex resets recommend
cq codex resets use EMAIL
cq codex resets use EMAIL --credit CREDIT_ID --yes
```

Account selectors vary: `switch`/`remove` take an email; routing/reset commands accept their documented email, CQ alias or opaque AccountKey. Multiple matching identities require an unambiguous reference.

CQ-owned Codex accounts live under `~/.codex/accounts/` with registry metadata. System `~/.codex/auth.json` remains distinct. Automatic quota/routing reads never switch the system account.

The credential coordinator also renews explicitly declared CodexBar managed accounts within 60 seconds of access expiry. It shares CodexBar's account lock and writes renewed tokens and their fingerprint back to the declared owner home; later CodexBar login takes priority. Renewal runs in the background, preserving healthy-account routing. If every credential is unavailable, reads wait up to one second for fast repair. Other external accounts and system auth remain read-only. An ambiguous refresh outcome is retained without replay and can require owner reauthentication.

`cq codex resets recommend` plans across every account shown by `cq codex accounts`. It fetches fresh usage and banked-reset inventories, reports a non-actionable incomplete schedule when any portfolio input is missing, and never consumes a credit. Fresh upstream usage supplies reset dates after consumption; an account may expose one or both shared windows.

`cq codex resets use EMAIL` previews selected credit, current shared usage, and current recommendation, then asks for confirmation with default No. Omit `--credit` to resume a pending attempt or select eligible credit with next expiry. Supply `--yes` only when explicit non-interactive consumption is intended.

An automated reset schedule is optional. Preview with `cq codex resets recommend`,
then accept with `cq codex resets recommend --schedule`. Confirmation defaults to
No. New schedules use **fixed** dates; `--mode adaptive` allows fresh portfolio
forecasts to move dates for the exact accepted credits. It never enrols newly
found credits. Use `--replace` to replace existing pending entries.

Set `RESET_AT` to a future RFC3339 timestamp with a timezone, before the selected credit expires. Read `list` and the recommendation first; dates are not inferred from these examples.

```sh
cq codex resets schedule                       # Entries and fresh executor status
cq codex resets schedule mode adaptive         # Confirm a mode change
cq codex resets schedule add EMAIL --at "$RESET_AT"
cq codex resets schedule set ENTRY --at "$RESET_AT"
cq codex resets schedule set ENTRY --follow-mode
cq codex resets schedule remove ENTRY
cq codex resets schedule enable
cq codex resets schedule disable
cq codex resets schedule cancel
```

Manual additions create a disabled fixed schedule. Manual dates must not be in
the past and pin entries in adaptive mode. Active edits and enabling require
confirmation; non-interactive acceptance requires `--yes`. Disable pauses
execution and replanning; cancel also cancels pending entries. A request already
sent upstream may finish, and unknown
outcomes remain visible with the same durable retry identity.

Execution requires the CQ proxy runtime. Saved schedules survive restarts; fixed
overdues catch up before expiry, while adaptive work first obtains a complete
fresh plan. Unavailable forecast inputs block adaptive entries; explicit pins
retain their fixed authority after fresh target validation. Status uses an
authenticated live heartbeat rather than assuming an installed service is running.

### Gemini

```bash
cq gemini accounts
cq gemini accounts --json
```

Sign in through Antigravity first and ensure its project selection is available. Gemini authentication and project selection remain owned by Antigravity. cq reads Keychain service `gemini`, account `antigravity`, plus `~/.gemini/antigravity-cli/cache/default_project_id.txt` when present. It does not provide Gemini login, switch, or removal commands.

### Z.ai Coding Plan

Run `cq zai login` to sign in through ZCode's browser OAuth flow. A completed login becomes the preferred account and uses its email as the default label. Repeat for another account; use `--no-activate` to keep the current preferred account or `--label` to choose a custom label. Existing `--zcode` and `--activate` flags remain accepted. CQ uses the inspected ZCode client identity and authentication protocol, exchanges the provider OAuth credential for its business identity, and obtains short-lived project access tokens for paid Coding Plan requests. OAuth sessions are encrypted in the private CQ configuration directory; `zai.json` contains account references rather than OAuth tokens. Each saved login retains its device identity. A fresh installation can register a stable local device UUID without installing ZCode.

For API-key authentication, run `cq zai login --api-key` and paste a Personal Coding Plan API key into the hidden prompt. Repeat for each subscription. Create the key under **API Keys → Personal Coding Plan**; Standard API Keys use separate billing. CQ validates quota access before saving the key in the CQ configuration directory's `zai.json` (default `~/.config/cq/zai.json` on macOS/Linux), using mode `0600` in a private directory. Quota access alone does not prove inference access; `cq zai prime now` checks whether an eligible dormant window can be started. Existing single-key files remain supported. Saved accounts also work for the background proxy service; new accounts and preferred-account changes apply on the next request without restarting CQ.

`ZAI_API_KEY` selects one account for that process. If its exact key has already been verified as belonging to a saved OAuth account, CQ uses that OAuth account; otherwise it uses only the supplied key. Remove the override to use the saved account pool. For scripts, `cq zai login --api-key-stdin` reads the key from standard input without putting it in command arguments.

CQ supports individual Coding Plan subscriptions through Z.ai's global endpoint. Group plans and regional endpoints are not configured separately.

```bash
cq zai login
cq zai login --no-activate
cq zai accounts
cq zai switch ACCOUNT_ID           # Prefer this subscription
cq zai remove ACCOUNT_ID          # Remove one saved account
cq zai remove --api-keys           # Remove stored API-key entries after browser login
cq --refresh check zai
cq models refresh
cq models list --provider zai
```

Browser login replaces only CQ-owned keys whose full value is verified against the OAuth account's personal projects. Unproved keys remain separate. For explicit bulk cleanup, `cq zai remove --api-keys` removes all stored API-key entries while retaining browser accounts and their relative preference order. It requires a saved browser login and makes no provider requests. An environment `ZAI_API_KEY` value remains under the caller's control; CQ never edits the environment or CodexBar's source.

Choose a saved account's billing route explicitly. New browser logins default to `zcode`, the paid Coding Plan gateway; `zcode-start` uses that OAuth account's separate Start Plan token pool. Start Plan access stays on the preferred account and never silently falls back to paid quota. Existing API-key accounts retain their saved route; `responses` is the original API-key route. Route changes apply on the next request.

```sh
cq zai route ACCOUNT_ID zcode
cq zai route ACCOUNT_ID zcode-start
cq zai route API_KEY_ACCOUNT_ID responses
cq zai zcode status ACCOUNT_ID --json
cq zai zcode resets issue ACCOUNT_ID
cq zai zcode resets use ACCOUNT_ID --window=5h
cq zai zcode claims list ACCOUNT_ID
cq zai zcode claims claim ACCOUNT_ID PLAN_ID
```

ZCode reset-card issuance is an explicit eligibility request. Using a card asks for confirmation; a weekly card restores both weekly and five-hour quotas. Durable request identities prevent a repeated accepted action from consuming another card. An unresolved retry uses the same request identity; `--new` confirms a separate action only after the previous one has resolved. Token claims require a currently offered plan, confirmation and the provider's browser verification. Status and offer listing never issue cards or claim plans. No ZCode issuance, card-use or claim schedule is enabled automatically. ZCode's advertised charging coefficient and promotional eligibility are applied by its server; CQ does not multiply displayed quota to simulate them.

The existing `cq zai resets list`, `use` and `schedule` commands also accept saved OAuth accounts. They use the account's business identity to read and select exact returned card IDs. Scheduling remains disabled until explicitly enabled; OAuth does not require an API key or a project access token for these reset operations.

Registry refresh fetches Z.ai's native Codex model catalogue from `https://api.z.ai/api/v1/models` and advertises its models through CQ's model-discovery routes. A compatible Codex client must fetch that catalogue; the client owns picker/cache refresh timing. The native Responses route uses the saved Z.ai key through CQ's proxy. ZCode routes translate Codex Responses into the gateway's Messages protocol, including streamed output and tool continuations. CQ bridges Codex WebSocket requests over HTTP and keeps continuation history for that connection. Codex account credentials remain scoped to Codex upstream requests. CQ handles native Codex v2 compaction triggers by asking GLM for a continuation summary and returning authenticated CQ-owned compact state. On later HTTP or WebSocket requests, CQ restores that summary before forwarding to either provider, including after model or account changes. Original chat history remains with Codex. The older `/responses/compact` endpoint remains unsupported for Z.ai (`501`).

When the preferred account uses `zcode-start`, discovery uses ZCode's public model configuration and that account's current model entitlements, without requesting a paid project token. Context, output limits and reasoning choices come from this source; CQ supplies its validated Codex harness defaults. A source advertising 1,000,000 context tokens retains that value. Future models appear when both the catalogue and current entitlement advertise them. Start requests include ZCode's public source identity and preserve Codex's caller instructions in one ordered block. OpenAI-hosted web search is unavailable through ZCode Messages; Codex's local function/custom tools and connected tools remain available. See [ZCode protocol and qualification](zcode-routing.md) for the observed billing result, native tool/compaction checks and provider capacity limits.

CQ-owned compact state uses the existing private proxy token as its key source. Retain the existing CQ proxy configuration across reinstalls or host migrations; changing that token makes previously generated CQ compact state unreadable. Provider account switching does not change this key.

CQ refreshes the provider catalogue every five minutes while running; `cq models refresh` requests an immediate refresh. New models appear when Z.ai adds them to that native catalogue for your subscription, without a CQ release or manual registry entry. Codex may retain its own catalogue cache until its next refresh. CQ caps GLM-5.3 automatic compaction at 70% of Codex's usable context window (for example, 665,000 tokens for a 1,000,000-token source window with a 95% input allowance), retaining any earlier provider limit. The cap is recalculated from downloaded metadata. GLM-5.3-Flash, GLM-5-Turbo and future models such as GLM-5.4 keep their provider defaults.

Routing tries the preferred subscription first, then the remaining saved subscriptions if Z.ai returns `401`, `403`, or `429` before a response starts. Each key is tried at most once per request. CQ does not replay a request after a transport error or a started response. WebSocket continuations retain full history across account fallback. HTTP requests containing an opaque `previous_response_id` use only the preferred account, because that upstream state cannot safely move between subscriptions.

Fresh quota data moves exhausted subscriptions behind available accounts. Route diagnostics include a redacted account hint and whether a rejected request fell back to another account. Stale or missing quota data keeps normal bounded fallback available.

Start dormant 5-hour windows with a minimal request, or enable automatic priming:

```sh
cq zai prime now
cq zai prime enable
cq zai prime status
cq zai prime disable
```

Automatic priming starts with the next CQ runtime launch. It checks each saved account independently, skips active or exhausted windows, and records dispatch before sending so a crash or uncertain result cannot repeat quota consumption within five hours. Weekly windows start with the subscription and do not need priming. Model selection uses Z.ai's current native catalogue and prefers a Flash model. Priming never consumes reset cards.

Inspect reset cards and use an exact card with default-No confirmation:

```sh
cq zai resets list
cq zai resets use ACCOUNT_ID --window 5h --credit CARD_ID
```

Weekly cards restore both weekly and 5-hour quota. A successful reset restarts the corresponding reset timer. CQ checks availability again after confirmation, saves intent before dispatch, and retains the same request ID if an operator explicitly retries an uncertain result. It never substitutes another card for an unresolved attempt.

Fixed-date schedules remain disabled until explicitly enabled:

```sh
cq zai resets schedule add ACCOUNT_ID --at RFC3339 --window 5h --credit CARD_ID
cq zai resets schedule status
cq zai resets schedule enable
cq zai resets schedule disable
cq zai resets schedule remove ENTRY_ID
cq zai resets schedule cancel
```

Replace `RFC3339` with a future timestamp including its timezone, before the selected card expires. Enabling prompts before accepting automatic card consumption; `--yes` supplies explicit non-interactive consent. Enabled schedules pick up changes without restarting CQ. Each entry binds one exact account and card. Interrupted or uncertain execution remains visible and is not automatically repeated. Schedule dates remain fixed; CQ does not infer adaptive reset dates for Z.ai.

See [Z.ai's Codex setup](https://docs.z.ai/devpack/tool/codex) for the supported Responses endpoint and model capabilities.

### CodexBar Z.ai discovery

CQ also reads personal/global Z.ai Coding Plan keys configured in CodexBar.
`ZAI_API_KEY` selects only its corresponding account, preferring a saved OAuth
account when exact ownership was previously verified. Borrowed keys are identity
verification candidates, not independent accounts. Unresolved sources stay out
of account lists, quota totals, priming and routing; a credential-free diagnostic
reports incomplete verification. Explicit CQ-owned API-key logins remain usable.
Borrowed credentials never enter CQ's write/removal paths.

Browser login verifies existing subscription keys against the OAuth account's
personal projects. A full key match replaces the CQ-owned key with OAuth and
records an opaque alias to suppress any borrowed duplicate. CodexBar's key stays
in CodexBar. Existing browser logins also gain aliases during quota refresh and
`cq zai accounts`, without signing in again. These refreshes append aliases only;
they preserve saved credentials, routes and logical account preference. Known
aliases need no further identity reads. Request-time account discovery stays
local and read-only. Removing the OAuth login leaves its borrowed keys unresolved
and excluded. A later login with an already verified key retains the OAuth account.
Paid Coding Plan and Start Plan are separate quota pools. A paid API key never
collapses into a `zcode-start` login or changes its selected billing route.

If ownership reads fail, CQ saves the browser login, preserves existing keys and
reports the limitation. Ambiguous CQ-owned keys and keys belonging to different
accounts remain separate; unresolved external keys are excluded. CQ does not
deduplicate or delete keys by matching a label,
email or key prefix. Explicit local key cleanup remains available through the
existing `cq zai remove --api-keys` command after a saved browser login; it is
not an automatic same-account upgrade and never modifies CodexBar.

### Grok

Use `cq grok login`, `accounts`, `switch ACCOUNT` and `remove ACCOUNT` for CQ-owned
subscription sessions. Login becomes preferred by default; `--no-activate`
preserves preference, and switching changes CQ's preference rather than a native
client login. `cq grok resets list` only inspects reward counts/expiry. See
[Grok subscriptions](#grok-subscriptions) for credentials, supported models,
client routing, continuation and qualification limits.

### Credential ownership

| Source | Automatic discovery/routing | Explicit changes |
|---|---|---|
| System Codex `~/.codex/auth.json` | Reads identity and available credential; never refreshes or rewrites it | `codex login --activate` / `switch` are user-directed activation |
| Eligible CQ-owned Codex accounts | Coordinator renews a ready, never-exported CQ-owned OAuth lineage | Login, activation and removal go through that coordinator |
| Declared CodexBar managed accounts | Reads only manifest-declared homes; the live coordinator owner may renew near expiry under CodexBar's lock and publish the new credential/fingerprint back | Newer owner login wins; uncertain refresh outcomes require reconciliation or owner reauthentication |
| Other external/borrowed/exported Codex credentials | Read-only; no speculative refresh | Reauthenticate with the owning tool |
| Gemini / Antigravity | Read-only credential/project stores; refresh result is process-local | Sign in/select a project in Antigravity |
| Claude CQ account store | Eligible tokens can refresh | Explicit activation also updates the client credential projection |
| Z.ai browser/API-key accounts | Browser sessions use CQ's encrypted private store; API keys are private stored credentials | Login/switch/remove/route commands; browser reauthentication when required |
| CQ-owned Grok subscriptions | Encrypted session store and adjacent private key; eligible sessions renew through CQ's owned authority | Login, CQ preference changes and removal; reauthenticate after uncertain refresh |
| Borrowed CodexBar Grok/Z.ai sources | Provider-scoped configured credentials are read/verified without adoption or renewal ownership | Repair credentials in CodexBar; CQ does not switch/remove their source records |

Codex's managed-account CodexBar integration reads its declared manifest beneath `~/Library/Application Support/CodexBar` on macOS. Grok/Z.ai use separate [provider-scoped CodexBar configuration](configuration.md#grok-credentials-and-codexbar-sources). Neither discovers arbitrary credential files by scanning the disk. The Dots `--account-source` override controls Dots discovery separately. Removing a CQ reference is not upstream account deletion or token revocation. Preserve originals and use the credential owner's sign-in flow when ownership is uncertain.

### Token refresh

```bash
cq refresh
cq agent install
cq agent uninstall
```

`cq refresh` refreshes eligible Claude, Codex and CQ-owned Grok OAuth credentials before expiry. Borrowed CodexBar Grok credentials remain read-only. Complete installers schedule this work using the platform user service manager. `cq agent install` remains a focused macOS maintenance command. Expired accounts or uncertain rotating-token outcomes can still require interactive login with their credential owner.

Ordinary quota and account commands do not change background service registration. Complete installers use `cq service install`; focused `cq agent` commands remain available for maintenance.

After an explicit account switch, already-running clients or MCP servers may need reconnection to reload credential state.

## Harness capability profiles

Harness profiles select optional capabilities without creating a new chat or rewriting history. Install the CQ curation skill, then ask Codex to inspect the installed inventory and prepare a profile for the task:

First save this synthetic definition as `review-definition.json` in your working directory. It disables optional capabilities by default and enables one selected skill. Replace `repo:review` with an ID returned by your own inventory before selecting a real skill.

```json
{
  "description": "A small review capability set",
  "changes": {
    "default": "disabled",
    "skills": {"repo:review": "enabled"}
  }
}
```

```sh
cq codex harness skill install
cq codex harness inventory --project /path/to/project --json
cq codex harness profile list
cq codex harness profile show minimal
cq codex harness profile save review --file review-definition.json
cq codex harness bind minimal review --chat current
cq codex harness explain --chat current --project /path/to/project --json
cq codex harness unbind --chat current
```

Definitions contain a description and `changes`, using `enabled`, `disabled` or `inherit` decisions keyed by stable IDs from `inventory`. `plugins`, `mcp` and `skills` each accept an ID-to-decision map; `"*"` supplies a per-kind default. `changes.default` applies to all kinds. `minimal` disables optional capabilities by default, while `full` restores native baseline choices. Multiple profile names apply in the given order. Saving or copying a definition does not bind it or enable policy. Existing names require explicit `--replace`; built-in `full` and `minimal` cannot be overwritten.

Bindings support every model. `--scope chat` is the default; `--scope project` writes `<project>/.cq/harness.json`, and `--scope global` writes CQ's private user policy. `--model` accepts an exact ID or a family with one trailing wildcard, such as `glm-*`; omit it for all models. Rules apply from global all-model and model-specific rules, through project ancestry, then chat all-model and model-specific rules. `inherit` preserves the previous decision. A first binding enables a fresh policy and requests application for a known chat after saving. Chat and project bindings preserve a disabled policy that already contains rules, saving as pending with `policy_disabled`; use `enable` or an explicit global binding to activate it. Application errors also retain the saved binding and report pending. `--chat current` uses only a validated `CODEX_THREAD_ID`; it does not search session files or chat history. `explain` reads the selected chat's model when `--model` is omitted, reports decision layers and current application status, and keeps the last application receipt separate as an audit record.

Profiles can be shared without installation paths:

```sh
cq codex harness export review > review-profile.json
cq codex harness import --file review-profile.json --name imported-review
cq codex harness profile copy review project-review --project /path/to/project
cq codex harness bind project-review --scope project --project /path/to/project --model 'gpt-*'
```

`export` writes JSON with `version: 1`, `name`, and a `definition` containing only its description and changes. `profile save --file` accepts either the raw definition or the exported envelope, using the explicit save name; `import --file` accepts the exported envelope and uses its name unless `--name` is supplied. Neither operation binds or activates the profile. Skill decisions use stable inventory IDs such as `repo:review`, not absolute paths. User definitions live in CQ's private `harness.json`; an explicit `--project` saves definitions and rules in that project's `.cq/harness.json`. Project writes promote legacy policy without enabling it or changing its existing choices.

The legacy GLM shortcuts remain available:

```sh
cq codex harness enable
cq codex harness project /path/to/project minimal
cq codex harness chat CHAT_ID full
cq codex harness chat CHAT_ID auto
cq codex harness status --chat CHAT_ID --model glm-5.3 --project /path/to/project
cq codex harness disable
```

With legacy policy enabled, GLM defaults to `minimal`. A chat override wins over the deepest matching project directory; `auto` removes an override. These shortcuts keep their GLM-only meaning after promotion to the newer format. Saving a legacy override does not enable policy.

On macOS, normal ChatGPT launches use per-chat plugin exclusions after CQ observes a native request. These apply to a subsequent eligible turn; first-turn application is not guaranteed. Standalone MCP servers and local skill catalogues remain available. Under the CQ launcher, the same policy also requests standalone MCP and fresh skill-catalogue changes before context assembly. Existing chats reload only at idle boundaries, retaining their ID and history. Another native subscriber can retain the previous configuration despite a successful reload. CQ verifies runtime controls and records retained or unverified changes as `partial`. Core tools and project instructions remain available; historical catalogue text already in the chat is preserved.

Newer native SDKs can remove skills from the current catalogue without publishing a removal message. A model repeating an earlier catalogue is therefore not proof that those skills remain enabled. Removing capabilities also preserves tokens already published in history; a later normal compaction builds a fresh catalogue from the current selection.

Native subagents inherit their parent's capability configuration. A child of a minimal parent starts minimal. Internal native spawns do not cross the launcher's chat-start hook, so CQ cannot independently apply a different model-based profile before that child's first turn. Start a separately managed chat when that distinction is required. A fresh stock native process restores its normal global capabilities when resuming a chat previously run through the launcher.

CQ saves the original plugin exclusions privately, retains them across broker restarts, and requests restoration when the profile returns to `full`, the model changes, or policy is disabled. Restoration can remain `partial` while another subscriber retains the configuration. If user changes diverge from CQ's saved exclusions, CQ preserves them. Desktop lacks an atomic plugin-list comparison, so CQ checks fresh settings before updating and guards the current model and reasoning effort. `status --chat` reports the last recorded application scope and state, separately from a policy preview. `changes.reset: true` first clears inherited capability choices before applying that definition's changes. Use it only for an intentional fresh baseline. Profiles are disabled by default and stored in CQ's private `harness.json`; per-chat receipts share ownership between the standard controller and launcher.

## Connecting a client

Compatible clients include desktop applications as well as terminal clients. ChatGPT Desktop's Codex chats use a Codex app-server backend; that backend can send supported model requests through CQ. Claude Code and compatible API clients can also connect. This does not make every desktop app or every ChatGPT conversation a CQ client: the application must support the required API, endpoint and authentication configuration.

Install CQ with a complete installer, sign in to the relevant provider, and check `cq service status --json` first. A portable binary can instead run `cq proxy start` in a foreground terminal. The default endpoint is `http://127.0.0.1:19280`; substitute your configured port. Keep it on loopback.

### Claude Code

With a recognised signed-in Claude credential, start a fresh terminal client using the local endpoint:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:19280 claude
```

For PowerShell, set `$env:ANTHROPIC_BASE_URL = 'http://127.0.0.1:19280'`, then start `claude`. The setting belongs to that shell/client process. Close the client and remove the variable to restore its ordinary endpoint. CQ accepts recognised Claude OAuth credentials or its generated local bearer; it does not forward that local bearer upstream. Anthropic model requests route to Claude; supported GPT/o-series Messages requests translate to Codex Responses.

### Codex CLI with a local bearer

CQ's supported Codex route is the Responses API. This custom-provider configuration pattern is exercised by CQ's installed-client fixtures. Install a compatible Codex client separately; its configuration syntax is owned by that client. First obtain a model ID from `cq models list --provider codex`, `--provider zai` or `--provider grok` that your account can use. Grok requires an entitled Responses-backend model; follow its [provider-specific limits](#grok-subscriptions).

The following shell example requires `jq`. Set `CQ_CONFIG` to your existing platform `proxy.json` path from [configuration](configuration.md#important-paths). It reads the local token into an environment variable without displaying it; keep shell tracing disabled and do not paste the token into a command argument, issue or transcript.

```sh
CQ_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/cq/proxy.json"  # macOS/Linux default
export CQ_LOCAL_TOKEN="$(jq -er '.local_token' "$CQ_CONFIG")"
codex -c 'model_provider="cq"' \
  -c 'model_providers.cq={name="CQ",base_url="http://127.0.0.1:19280",wire_api="responses",env_key="CQ_LOCAL_TOKEN",supports_websockets=true}'
unset CQ_LOCAL_TOKEN
```

On Windows PowerShell, use the CQ configuration known-folder path, for example `$cqConfig = Join-Path ([Environment]::GetFolderPath('ApplicationData')) 'cq\proxy.json'`, then `$env:CQ_LOCAL_TOKEN = (Get-Content -Raw $cqConfig | ConvertFrom-Json).local_token`. Run the same `codex -c` arguments on one line, and remove the variable afterwards with `Remove-Item Env:CQ_LOCAL_TOKEN`.

Choose a real model in the client. A prompt such as “Reply with PONG” sends an actual model request and uses that provider's quota. Inspect the client result and a matching CQ trace when diagnostics are enabled. Setting `supports_websockets=false` selects the HTTP variant for clients that support this option. API-key style clients use CQ's local bearer, while recognised native Codex credentials can use the OAuth-compatible routes. Never replace your provider credential file with CQ's local bearer.

Persistent client configuration can use the same `[model_providers.cq]` fields in the client's own config. Keep the token in a private environment/credential mechanism, preserve unrelated configuration, and consult the installed client's help. Removing the CQ provider selection restores the prior client endpoint. A model registry entry does not itself connect a client to CQ.

### ChatGPT Desktop integrations

For ordinary **Codex chat account routing**, configure the app's actual Codex
backend to use CQ's loopback Responses endpoint. A compatible app-server build
can use a persistent custom-provider fragment in its Codex `config.toml`,
normally beneath `~/.codex` unless its configuration home is overridden:

```toml
model_provider = "cq"

[model_providers.cq]
name = "CQ"
base_url = "http://127.0.0.1:19280"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true
```

Merge these fields into the client's existing configuration; do not overwrite
unrelated settings or duplicate an existing provider table. This variant uses
the client's existing recognised OpenAI sign-in rather than embedding CQ's local
bearer. API-key style clients can instead use the local-bearer pattern above,
but their credential environment must reach the app-server process. Exporting
a token or base URL in one terminal does not configure an app launched from
the Dock. Never replace native account credentials with the local bearer.

Check which provider/configuration the running app actually uses and coordinate
any reload or restart with the user. Verify a request from a real Desktop Codex
chat reaches CQ, then inspect configured/effective HTTP and WebSocket routing
modes before claiming account load balancing. The normal app icon can be used;
ordinary model-request routing does not require the Dots launcher, a US tunnel
or CQ Network. It does not route cloud work that never reaches this Mac's proxy.

Normal ChatGPT launches can use the optional [native network integration](configuration.md#native-chatgpt-network-routing). It requires the signed macOS companion, WireGuard and explicit activation. Usage response handling has an additional certificate-trust step. These are separate from account routing and from [multi-account Dots](dots.md), which requires launching through `ChatGPT via CQ` or `cq codex dots start`.

## Local proxy

`cq proxy` binds to loopback, routes Claude, Codex, Z.ai and supported Grok traffic, and publishes local model metadata. It supports Anthropic Messages clients plus native Codex Responses HTTP and WebSocket traffic, including compact, search, image, and realtime/live routes used by supported clients. Endpoint and capability support varies by provider: Grok models require Responses, not Anthropic Messages, and do not inherit every native Codex route. The retired `/app-server` compatibility endpoint returns `410 Gone` with guidance to run Codex app-server locally and route its outbound Responses traffic through cq.

### Service lifecycle and status

The complete lifecycle is `cq service install`, `upgrade`, `restart`, `status` and `uninstall`. Installation/removal normally belongs to the package manager. The explicit `upgrade` retained-runtime handoff is macOS Homebrew-specific; Linux/Windows still support ordinary package replacement and service reconciliation, but not that command's retained-runtime protocol. See [Homebrew upgrades](homebrew-upgrades.md). Platform jobs are macOS LaunchAgents, Windows tasks `\cq\Proxy` / `\cq\Refresh`, and Linux user units `cq-proxy.service`, `cq-refresh.service` / `cq-refresh.timer`.

macOS Homebrew upgrades retain the proxy listener, stop admitting new work, drain each current turn, and select verified runtime copies. Finished WebSocket chats reconnect while other turns finish. First adoption needs a maintenance window. See [Homebrew upgrades](homebrew-upgrades.md).

Complete installers own service installation and removal. Use `cq service` to
inspect or restart both package-owned components together:

```bash
cq service status --json
cq service restart
```

`cq service install` and `cq service uninstall` expose the same transaction for
development and repair work. Package hooks call them automatically; users do
not need them after a complete install.

Focused proxy commands remain available for foreground and compatibility work,
but do not form a complete installation:

```bash
cq proxy start
cq proxy start --port 19280
cq proxy start --migrate-legacy-managed
cq proxy install
cq proxy restart
cq proxy uninstall
```

`--migrate-legacy-managed` explicitly adds routing identity metadata to legacy CQ-managed Codex records. Ordinary startup does not perform this migration.

Bare `cq proxy status` performs a compatibility health probe. Inspection forms with flags reconcile desired configuration, service owner, listener, process, runtime health, and data-plane evidence:

```bash
cq proxy status                    # Compatibility /health JSON probe
cq proxy status --human            # Reconciled human summary
cq proxy status --json             # Reconciled stable JSON envelope
cq proxy status --strict --json    # Non-zero when reconciled state is unhealthy
cq proxy status --timeout 10s
cq proxy status --instance-state-root PATH
```

`GET /health` proves runtime reachability only; strict status checks broader ownership and runtime facts.

### Client routing

Point Claude Code-compatible traffic at `http://127.0.0.1:19280` with `ANTHROPIC_BASE_URL`. cq accepts its generated local bearer token and known Claude OAuth tokens, routes Anthropic models to Claude, and translates supported GPT/o-series Anthropic Messages requests to Codex Responses. Native Codex clients use Codex HTTP/WebSocket routes without Anthropic translation.

Core behaviour includes:

- model-registry-first provider selection with prefix fallback;
- Claude/Codex multi-account routing, plus separate Z.ai and Grok subscription selection contracts;
- quota/capacity-aware fair selection;
- account eligibility and explicit allowlists;
- hard continuity for bound Codex turns/sessions;
- HTTP per-turn routing and WebSocket connection-aware routing;
- bounded failover before response commitment;
- optional request headroom compression (`cache` or `token` mode);
- durable leases and retention for Codex continuity;
- automatic model-registry publication and drift repair.

### Pins, defaults, and priming

```bash
cq proxy pin                              # Show Claude and Codex pins
cq proxy pin claude EMAIL_OR_UUID
cq proxy pin claude --clear
cq proxy pin codex EMAIL_ALIAS_OR_KEY
cq proxy pin codex --clear

cq proxy default codex EMAIL_ALIAS_OR_KEY
cq proxy default codex --clear

cq proxy prime status
cq proxy prime enable
cq proxy prime disable
```

Claude pin changes hot-reload. Codex pin/default/priming changes require proxy restart. A Codex pin affects new and unbound work but does not break existing hard continuity.

### System account reserve

The running CQ service can protect a percentage of one Codex quota window for the current system account.
The reserve follows the system account when it changes. Other accounts remain available under their existing routing rules.

```bash
cq proxy reserve windows                     # List available window selectors
cq proxy reserve set --window 7d --percent 2   # Protect the final 2% of weekly quota
cq proxy reserve status
cq proxy reserve status --json
cq proxy reserve disable                     # Release the reserve until this window resets
cq proxy reserve enable                      # Restore protection immediately
cq proxy reserve clear                       # Remove the configuration
```

Use the selectors from `reserve windows` for scoped limits, such as `7d:gpt-reserve` or `5h:gpt-5.3-codex-spark`.
The commands require the running service. Changes apply immediately and survive service restarts.

At or below the threshold, the system account becomes unavailable to routing. Pools can continue through their other eligible accounts.
Pinned work receives HTTP 429 or the equivalent WebSocket usage-limit error when its account reaches the reserve.
The reserve applies to the account, not to a pool or its aggregate percentage.

`disable` requires fresh usage and reset evidence. A confirmed natural or forced reset restores protection automatically.
CQ never consumes a reset credit for this feature. A system account change also removes the previous account's temporary bypass.

The service consumes live response telemetry and refreshes usage in the background.
Polling follows Codex's 60/30/15/5-second cadence as usage approaches exhaustion; the reserve threshold can shorten that interval.
Concurrent refreshes share one request per account. Failed reads wait at least 30 seconds and honour longer `Retry-After` values.
If selected-window data exceeds its refresh interval plus 10 seconds, CQ temporarily excludes the protected account until fresh data arrives.
Already-running requests and usage outside CQ can cross the threshold before CQ receives an update.

### Capability policy and session pools

Initialise a private absolute state directory once, save its configuration, then restart the runtime at a suitable boundary so it opens that authority root. With the service running, prefer `pool set` and `session bind` over editing authenticated state files.

Advanced policy commands manage authenticated, capability-aware Codex account pools and privacy-safe session bindings:

```bash
cq proxy policy initialise --state-root DIR
cq proxy policy apply --file FILE
cq proxy policy status
cq proxy policy pool set NAME --account ACCOUNT --account ACCOUNT [--value VALUE]
cq proxy policy pool rename OLD_NAME NEW_NAME
cq proxy policy pool value NAME VALUE
cq proxy policy session bind --pool NAME --session-id ID
cq proxy policy session show --session-id ID
cq proxy policy session list
cq proxy policy session unbind --session-id ID
cq proxy policy session digest --session-id ID
```

For a synthetic two-account example, replace the addresses with unique entries from `cq codex accounts`, and use the actual native session ID/link where supported:

```sh
cq proxy policy pool set Research --account alice@example.test --account bob@example.test --value 10
cq proxy policy session bind --pool Research --session-id YOUR_SESSION_ID
cq proxy policy session show --session-id YOUR_SESSION_ID
cq proxy policy session unbind --session-id YOUR_SESSION_ID
```

`pool set` replaces that pool's member selection; omitted `--value` preserves an existing value. `pool rename` preserves pool identity and its session bindings. Values are unsigned 32-bit integers. Names are case-insensitive selectors with preserved display casing; quote names containing spaces. Inspect status before and after a change. `apply --file` is the full-document operator path; its schema and generation rules are in [configuration](configuration.md#routing-policy-documents).

Session selectors accept `--session-id`, `--session-id-stdin`, or a full keyed `--digest`. Live policy operations use authenticated loopback control; explicit `--state-root` supports offline state.

Pool names are case-insensitive selectors and retain their configured display casing. Higher values preserve a pool's account capacity by routing ordinary unbound work through lower-value viable accounts first.

The current Codex system account has an inherent preservation tier above every pool value because it may also serve clients on other hosts. Ordinary routing tries other eligible accounts first, even when soft affinity points to the system account. Within each tier, pool values retain their ordering. Hard continuation bindings and session-pool restrictions remain enforced, and the configured quota reserve still blocks protected system-account usage.

Bindings select one exact session at a time. To keep several related chats
within the same account group, bind each to the same pool. CQ does not provide
an automatic parent/child session-family or conversation-bundling command.
Account pools are separate from harness profiles, which select tools, plugins,
MCP servers and skills rather than provider credentials.

### Cyber-aware routing

Use this when some Codex accounts have Cyber access and ordinary accounts do
not. With routing-policy authority configured, CQ reads each account's own
model catalogue and reconciles a generated `Cyber` pool from explicit access
programme evidence. Background discovery runs every five minutes. Inspect
`cq proxy policy status` for the current policy instead of manually declaring
unknown accounts eligible.

When a supported client sends an active Cyber access-programme request, CQ
selects the generated pool for that request. The chat's saved ordinary pool
binding remains intact, so turning Cyber access off does not rewrite it.
Selection still checks the particular account, model and programme; membership
alone does not grant entitlement. Failed/absent discovery is unknown rather
than an explicit denial, while a confirmed upstream access denial temporarily
excludes that exact combination. Do not fabricate capability evidence or imply
that binding an account to a pool enrols it in an upstream programme.

### Lease redistribution and invalidation

```bash
cq proxy leases redistribute
cq proxy leases invalidate
```

Redistribution schedules all existing Codex chats for fresh account selection at
the next safe request boundary. Active responses finish unchanged; confirmed
included quota is preferred over unknown or exhausted accounts within existing
pools, pins, model compatibility, and reserve rules. WebSocket account-owned
deltas use the existing reconnect/resync path before moving. HTTP continuations
with `previous_response_id` stay on their account until the client sends a
portable full-history request. Pending redistribution survives a worker restart.

Successful `cq codex resets use` and fresh detected quota resets schedule this
automatically. Redistribution itself never consumes a banked reset. Only an explicitly enabled reset schedule authorises automatic consumption. A failed reset
notification emits a warning without undoing or repeating reset consumption.

Lease invalidation clears reusable Codex account affinity across all sessions.
Active requests and required continuity remain unchanged. Each next eligible
request selects again using current pool membership and account capacity.

### Rescue mode

```bash
cq proxy rescue enter
cq proxy rescue status
cq proxy rescue exit
```

Rescue mode is durable, local-token authenticated, and loopback-only. It lets operators move traffic through a bounded recovery path without sending control credentials upstream.

## Codex validation and operational controls

These commands exist for controlled validation, rollout, and recovery. Use each command's help before mutation.

### Installed routing validation

```bash
cq codex validate capture --input FILE --output FILE --content-encoding ENCODING --metadata FILE
cq codex validate http --client-build BUILD [--state-dir DIR]
cq codex validate websocket --client-build BUILD [--client-executable PATH] [--state-dir DIR]
cq proxy validate-http --port CANDIDATE_PORT
```

Validation checks current installed-listener readiness against cq/client builds, request/response evidence, and process attestation. `proxy validate-http` refuses live port `19280` and does not change shared proxy configuration.

### Routing canary

```bash
cq codex canary start
cq codex canary status
cq codex canary stop
```

Canary start requires enforced HTTP routing and disabled payload diagnostics. Stop requests a drain; it does not discard active continuity.

### Isolated candidate lifecycle

The full candidate command grammar and retained evidence requirements are in [release operations](releases.md). Ellipses below mark operator-supplied arguments, not runnable commands.

```bash
cq proxy candidate prepare ...
cq proxy candidate status --instance-state-root PATH
cq proxy candidate client-bearer-barrier refresh ...
cq proxy candidate start ...
cq proxy candidate artifact switch ...
cq proxy candidate validate-release ...
cq proxy candidate receipt show ...
cq proxy candidate stop ...
cq proxy candidate remove ...
```

Candidate validation uses an isolated state root, explicit port, pinned source/release/client digests, explicit credential mode, bounded timeouts, authenticated runtime control, client-bearer barrier, and retained receipts. It never uses installed listener or ambient credentials. Read-only credentials and payload capture require explicit confirmation; removal requires explicit state-loss confirmation.

### Durable operations

```bash
cq operation status
cq operation status --operation-id ID --json
cq operation recover --operation-id ID --json
```

Durable operation inspection returns active, retained terminal, or idle state. Recovery reconciles exact operation identity instead of starting unrelated work.

### Legacy credential endpoint maintenance

See [release operations](releases.md) and command help for snapshot, ticket and confirmation requirements. The transition examples are an operation sequence; replace ellipses with that operation's exact inputs.

```bash
cq proxy endpoint inspect-legacy
cq proxy endpoint transition-legacy prepare ...
cq proxy endpoint transition-legacy resume ...
cq proxy endpoint transition-legacy activate ...
cq proxy endpoint transition-legacy finalise ...
cq proxy endpoint transition-legacy rollback ...
```

Ordinary cq/proxy startup never performs legacy endpoint maintenance. Inspection is read-only. Transition steps require explicit snapshots/tickets, stopped-and-drained or healthy-candidate confirmations, and retain rollback state.

### Codex Stop hook and efficiency receipt

```bash
cq proxy hook codex-stop
```

Configured as a Codex `Stop` hook through the client's own hook mechanism, this command reads hook JSON from stdin, performs an authenticated loopback lookup, and returns a privacy-safe `systemMessage`. The required input fields are `hook_event_name: "Stop"`, nonempty `session_id` and nonempty `turn_id`; a missing receipt produces `{}`. Configure the command in the installed client's supported hook format; CQ does not install the hook automatically. Receipt summarises recorded route state, transport, pool/account hint, model/effort, route reason, and observational no-affinity comparison. Recorded state can be planned, attempted, completed, failed, rejected, or indeterminate. Receipt does not include prompts, transcripts, raw IDs, or credentials and does not change routing.

## Model registry

```bash
cq models refresh
cq models list
cq models list --json
cq models list --provider codex
cq models list --provider anthropic
cq models list --provider zai
cq models list --provider grok

cq models overlay add --provider codex --id NEW_MODEL_ID --clone-from NATIVE_MODEL_ID
cq models overlay remove --provider codex --id NEW_MODEL_ID
cq models overlay prune
```

The running proxy refreshes upstream models every five minutes. Each Codex fetch resolves the current installed client version, so Desktop upgrades take effect without restarting CQ. Failed fetches retain the last usable models. Use `cq models refresh` for an immediate refresh.

Registry refresh merges provider sources with local overlays, validates entries, and publishes:

- CQ-owned native Codex/Z.ai/Grok registry metadata at `model-registry.json` beneath the CQ cache root;
- Claude Code capability cache at `~/.claude/cache/model-capabilities.json` when Anthropic entries are present;
- managed Claude Code picker entries in `~/.claude.json`.

CQ serves compatible Codex clients through its model-discovery routes; it does not replace their account-scoped `models_cache.json`. The native cache is a read-only listing fallback before CQ's own registry cache exists. Once a CQ cache exists, including an intentionally empty one, it takes precedence for CQ listings. Client-side fetch/cache rules determine when a picker changes. `CLAUDE_CONFIG_DIR` selects the listing fallback and compatibility capability-cache path, but does not redirect the registry pipeline's publication at `~/.claude/cache`.

Replace `NATIVE_MODEL_ID` with an existing native ID from `models list`; `NEW_MODEL_ID` must be a real upstream-supported ID you intend to expose. An overlay does not create upstream model access. Overlays expose not-yet-native model IDs and can clone metadata from an existing model. `prune` removes overlays now supplied natively. Overlay store: `models.json` in the [platform CQ configuration directory](configuration.md#important-paths).

Proxy endpoints also expose model metadata and authenticated registry refresh/snapshot APIs for local clients.


## Multi-account Dots

[The Dots guide](dots.md) covers install/start/stop/sync/list/status, one-time setup, explicit `prime`, one-shot `broadcast` and shared `board` binding. Installation and initial discovery can send a setup message; `prime`, `broadcast` and a new board binding also send messages. Read the delivery and uncertain-outcome rules before running them. Dots and native network handling are optional macOS integrations, with separate readiness checks.

## Command and operation reference

The [complete command index](../README.md#complete-command-index) is checked against the public command tree. Use leaf help, for example `cq zai resets use --help`, for argument selectors and confirmation flags. The [release guide](releases.md) covers advanced installed validation, canary, candidate lifecycle, operation recovery and legacy endpoint transitions. Current [routing-policy types and validation](../internal/proxy/routing_policy_store.go) define the implemented policy format.

## Grok subscriptions

```bash
cq grok login
cq grok login --label Personal --no-activate
cq grok accounts
cq grok switch ACCOUNT
cq grok remove ACCOUNT
cq check grok
cq --json check grok
cq models list --provider grok
cq grok resets list
cq grok resets list ACCOUNT
```

Grok uses browser OAuth and the Grok Build subscription service. CQ stores its
own login in encrypted `grok-accounts.json` under the private CQ configuration
directory. Its adjacent private key file is required to read that store. Normal
refresh renews only CQ-owned sessions. Login, preference changes and removal do
not modify Grok Build or CodexBar state. An uncertain rotating-token exchange
requires reauthentication rather than replaying the consumed refresh token.

Browser login becomes CQ's preferred Grok account by default. `--no-activate`
preserves the existing preference; `switch` changes only CQ's preference and does
not sign Grok Build or another native client into a different account.

The provider reports included subscription usage and the actual reset period.
Purchased credit, top-ups, public xAI API keys and monetary reporting are outside
CQ's scope. Unknown plan capacity is excluded from weighted aggregates. New and
legacy plan names are retained rather than assigned guessed multipliers.
Reports use the account-scoped `subscription_tier_display` from Grok Build's
`/settings`, rather than internal `/user` identifiers such as `GrokPro`.
If display metadata is unavailable, CQ retains the saved plan label and still
reports usable quota. Current [X Subscription tiers](https://x.ai/pricing) are
Plus, Premium, Super and Ultra; existing SuperGrok subscriptions remain valid.

Percentages represent included allowance remaining, rounded upwards for display
while exact remaining percentage controls admission. A small positive allowance
is not treated as exhausted merely because of display rounding.

CodexBar's configured Grok bearer credentials are discovered dynamically and
verified against the subscription profile. A CQ-owned OAuth login takes priority
for the same verified user/principal type/principal/team, including when its saved
account ID predates the current identity scheme. Different users, seats and teams
remain separate even when their email or label matches. Borrowed credentials are never adopted,
renewed, switched or removed. Cookies, public API keys and unverified bearer
records are silently excluded from account lists and totals. A broken external
source does not hide healthy CQ-owned accounts.

Model routing currently requires an explicit native Responses backend and known
context in the authenticated catalogue. The adapter translates supported Codex
custom/namespaced tools to functions, restores their identities in results and
streams, and shares CQ's HTTP/SSE/WebSocket machinery. Function descriptions
retain custom grammar instructions; Grok does not thereby enforce that grammar.
Non-Responses models are not advertised as supported. CQ compacts through a
subscription Responses summary request and seals the result using the same CQ
state format as Z.ai. The subscription service does not expose the public API's
standalone compaction endpoint. No separately billed API fallback is used.

For unbound work, Grok walks account preference/source order, checks each
candidate's own authenticated model catalogue and fresh included quota, then
selects the first usable account. Unavailable or ineligible candidates can be
skipped before inference dispatch. After dispatch, CQ does not retry that
inference on another account. Native continuation and each WebSocket session
retain their producing account/model. Grok does not use Codex's fairness planner,
account pools, reserve or reset controls.

Native continuation response IDs and opaque state are bound to their producing
account and model in the current process. CQ retains the original native
reasoning container to undo client serialization changes; ciphertext stays
unchanged. This cache has an entry limit and a byte limit. After eviction or a
worker restart, unknown native opaque state
requires a fresh conversation with full readable input. CQ does not replay an
uncertain admitted inference request or fall back to separately billed API access.
CQ-owned summary state remains readable while its local proxy token is retained.
It reads included quota before dispatch and does not enable extra usage settings;
the provider still controls accounting between that read and the request.

`resets list` reads reward counts and expiry dates without exposing redemption
identifiers or consuming rewards. No reset redemption or scheduling is implemented.
Endpoint acceptance and entitlement are account-dependent; an unavailable read
is not reported as zero rewards.

Live qualification on 6 November 2026 verified browser login, rotating renewal,
included quota and the account-specific model catalogue on a GrokPro subscription.
With app-bundled Codex `0.167.0-alpha.3.2`, each model below completed local tools
before and after CQ compaction, two compactions in one conversation, and exact
recall of a marker and the changed file token on both transports.

| Model | HTTP | WebSocket |
| --- | --- | --- |
| `grok-4.7` | Passed | Passed |
| `grok-4.7-build-fast` | Passed | Passed |
| `grok-4.6` | Passed | Passed |
| `grok-4.5` | Passed | Passed |

Qualification covers these model IDs and client version on a personal GrokPro
subscription. Near-exhaustion and provider extra-usage behavior remain
unqualified.

The opt-in `TestGrokLiveSubscriptionLogin` check uses an isolated profile and performs
login/profile/quota/catalogue reads only. Set `CQ_GROK_LIVE_LOGIN=1` and an absolute
`CQ_GROK_LIVE_CONFIG`, then run it with `go test -race -count=1 -v ./cmd/cq -run
'^TestGrokLiveSubscriptionLogin$'`. It does not run inference or change billing.
`TestGrokLiveSavedSubscription` reuses a saved profile for read-only checks when
`CQ_GROK_LIVE_SAVED=1`. The separate
`TestGrokLiveInstalledToolsCompactionAndContinuation` proxy test requires
`CQ_GROK_LIVE_TEST=1` and spends subscription quota through an isolated listener.
`CQ_GROK_LIVE_MODEL` selects an entitled model; its default is the first eligible
catalogue entry. The installed client receives only a temporary loopback token.
