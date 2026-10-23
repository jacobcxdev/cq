# Root and command-group designs

Status: draft. Shared formatting in [README](README.md) applies. Every section below specifies one help surface; group invocation performs no operational work. Links are navigation, never synthetic subcommands.

## `cq help`

**Help forms:** `cq help`, `cq --help`, `cq -h`.

**Purpose:** Provider quotas, accounts and proxy controls.

**Layout, in order:**

1. Bold cyan `cq` title; summary beneath.
2. `Usage` heading and `cq [COMMAND] [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `auth` | Refresh stored provider authentication credentials. |
| `check` | Report quota usage for selected providers. |
| `claude` | Manage Claude accounts and proxy routing pins. |
| `codex` | Manage Codex accounts, quota resets and provider-specific proxy controls. |
| `completion` | Print a shell completion script. |
| `gemini` | Inspect the externally managed Gemini account. |
| `help` | Show help for a command path. |
| `models` | Inspect model registries and publish local model-cache updates. |
| `proxy` | Run and inspect the shared proxy, state and lifecycle machinery. |
| `service` | Manage proxy and periodic token-refresh background services. |
| `version` | Show CQ build identity. |

**Examples:**

- `cq` — Show provider quotas.
- `cq check codex --fresh` — Fetch Codex quotas.
- `cq help service install` — Inspect installation options.

**Related navigation:** Provider commands and service lifecycle are grouped on root; use `cq help PATH` for deeper help.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq auth`

**Help forms:** `cq auth`, `cq auth --help`, `cq help auth`.

**Purpose:** Refresh stored provider authentication credentials.

**Layout, in order:**

1. Bold cyan `cq auth` title; summary beneath.
2. `Usage` heading and `cq auth <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `refresh` | Refresh eligible stored Claude and Codex OAuth credentials. |

**Examples:**

- `cq auth --help` — Show group help.

**Related navigation:**

- `cq claude account` — Add, inspect, select and remove Claude accounts.
- `cq codex account` — Add, inspect, select and remove Codex accounts.
- `cq gemini account` — Inspect externally managed Gemini account.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq claude`

**Help forms:** `cq claude`, `cq claude --help`, `cq help claude`.

**Purpose:** Manage Claude accounts and proxy routing pins.

**Layout, in order:**

1. Bold cyan `cq claude` title; summary beneath.
2. `Usage` heading and `cq claude <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `account` | Authenticate, inspect, activate and remove Claude accounts. |
| `proxy` | Control Claude-specific routing through the shared proxy. |

**Examples:**

- `cq claude --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq claude account`

**Help forms:** `cq claude account`, `cq claude account --help`, `cq help claude account`.

**Purpose:** Authenticate, inspect, activate and remove Claude accounts.

**Layout, in order:**

1. Bold cyan `cq claude account` title; summary beneath.
2. `Usage` heading and `cq claude account <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `activate` | Select the native client default Claude account. |
| `list` | List locally configured Claude accounts. |
| `login` | Authenticate and store a Claude account. |
| `remove` | Remove local Claude account credentials. |

**Examples:**

- `cq claude account --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help claude`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq claude proxy`

**Help forms:** `cq claude proxy`, `cq claude proxy --help`, `cq help claude proxy`.

**Purpose:** Control Claude-specific routing through the shared proxy.

**Layout, in order:**

1. Bold cyan `cq claude proxy` title; summary beneath.
2. `Usage` heading and `cq claude proxy <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `pin` | Inspect, set or clear the Claude proxy account override. |

**Examples:**

- `cq claude proxy --help` — Show all immediate subcommands and usage.

**Related navigation:**

- `cq proxy` — Shared proxy runtime, health, state and diagnostics.
- `cq service` — Manage background services.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq claude proxy pin`

**Help forms:** `cq claude proxy pin`, `cq claude proxy pin --help`, `cq help claude proxy pin`.

**Purpose:** Inspect, set or clear the Claude proxy account override.

**Layout, in order:**

1. Bold cyan `cq claude proxy pin` title; summary beneath.
2. `Usage` heading and `cq claude proxy pin <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `clear` | Clear the Claude proxy pin. |
| `set` | Set the Claude proxy pin. |
| `show` | Show the Claude proxy pin. |

**Examples:**

- `cq claude proxy pin --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help claude proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex`

**Help forms:** `cq codex`, `cq codex --help`, `cq help codex`.

**Purpose:** Manage Codex accounts, quota resets and provider-specific proxy controls.

**Layout, in order:**

1. Bold cyan `cq codex` title; summary beneath.
2. `Usage` heading and `cq codex <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `account` | Authenticate, inspect, activate and remove Codex accounts. |
| `proxy` | Control Codex routing, quota protection and transport evidence. |
| `reset` | Inspect, schedule and use banked Codex quota-reset credits. |

**Examples:**

- `cq codex --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex account`

**Help forms:** `cq codex account`, `cq codex account --help`, `cq help codex account`.

**Purpose:** Authenticate, inspect, activate and remove Codex accounts.

**Layout, in order:**

1. Bold cyan `cq codex account` title; summary beneath.
2. `Usage` heading and `cq codex account <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `activate` | Select the native client default Codex account. |
| `list` | List locally configured Codex accounts. |
| `login` | Authenticate and store a Codex account. |
| `remove` | Remove local Codex account credentials. |

**Examples:**

- `cq codex account --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy`

**Help forms:** `cq codex proxy`, `cq codex proxy --help`, `cq help codex proxy`.

**Purpose:** Control Codex routing, quota protection and transport evidence.

**Layout, in order:**

1. Bold cyan `cq codex proxy` title; summary beneath.
2. `Usage` heading and `cq codex proxy <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `canary` | Manage a bounded Codex routing-observation run. |
| `credential-endpoint` | Maintain the Codex local credential-owner endpoint. |
| `fallback` | Inspect, set or clear the terminal fallback account for Codex routing. |
| `fixture` | Create sanitised test fixtures from saved Codex request bodies. |
| `hook` | Produce receipt annotations for native client lifecycle hooks. |
| `lease` | Invalidate reusable Codex routing affinities. |
| `pin` | Inspect, set or clear the Codex proxy account override. |
| `policy` | Export or replace the complete Codex routing-policy document. |
| `pool` | Define account pools and their relative capacity-preservation values. |
| `prime` | Inspect or toggle automatic Codex quota-window priming. |
| `readiness` | Inspect retained transport-readiness evidence for an exact client build. |
| `reserve` | Protect remaining quota on the current Codex system account. |
| `session` | Inspect or change the routing pool bound to a client session. |
| `trace` | Read or follow retained Codex request traces. |
| `validate` | Request HTTP checks or run WebSocket transport validation. |

**Examples:**

- `cq codex proxy --help` — Show all immediate subcommands and usage.

**Related navigation:**

- `cq proxy` — Shared proxy runtime, health, state and diagnostics.
- `cq service` — Manage background services.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy canary`

**Help forms:** `cq codex proxy canary`, `cq codex proxy canary --help`, `cq help codex proxy canary`.

**Purpose:** Manage a bounded Codex routing-observation run.

**Layout, in order:**

1. Bold cyan `cq codex proxy canary` title; summary beneath.
2. `Usage` heading and `cq codex proxy canary <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `start` | Start a protected observation run of Codex routing. |
| `status` | Show the retained Codex routing observation run. |
| `stop` | Request a graceful end to the Codex routing observation run. |

**Examples:**

- `cq codex proxy canary --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy credential-endpoint`

**Help forms:** `cq codex proxy credential-endpoint`, `cq codex proxy credential-endpoint --help`, `cq help codex proxy credential-endpoint`.

**Purpose:** Maintain the Codex local credential-owner endpoint.

**Layout, in order:**

1. Bold cyan `cq codex proxy credential-endpoint` title; summary beneath.
2. `Usage` heading and `cq codex proxy credential-endpoint <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `legacy` | Inspect and migrate the legacy credential-owner endpoint protocol. |

**Examples:**

- `cq codex proxy credential-endpoint --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy credential-endpoint legacy`

**Help forms:** `cq codex proxy credential-endpoint legacy`, `cq codex proxy credential-endpoint legacy --help`, `cq help codex proxy credential-endpoint legacy`.

**Purpose:** Inspect and migrate the legacy credential-owner endpoint protocol.

**Layout, in order:**

1. Bold cyan `cq codex proxy credential-endpoint legacy` title; summary beneath.
2. `Usage` heading and `cq codex proxy credential-endpoint legacy <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `activate` | Activate a reversible replacement credential endpoint. |
| `finalise` | Finalise a verified credential endpoint replacement. |
| `inspect` | Inspect the legacy Codex credential socket and migration state. |
| `prepare` | Quarantine a refused legacy credential socket for replacement. |
| `resume` | Reopen a retained credential endpoint migration. |
| `rollback` | Restore the quarantined legacy credential socket. |

**Examples:**

- `cq codex proxy credential-endpoint legacy --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy credential-endpoint`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy fallback`

**Help forms:** `cq codex proxy fallback`, `cq codex proxy fallback --help`, `cq help codex proxy fallback`.

**Purpose:** Inspect, set or clear the terminal fallback account for Codex routing.

**Layout, in order:**

1. Bold cyan `cq codex proxy fallback` title; summary beneath.
2. `Usage` heading and `cq codex proxy fallback <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `clear` | Clear the Codex proxy fallback. |
| `set` | Set the Codex proxy fallback. |
| `show` | Show the Codex proxy fallback. |

**Examples:**

- `cq codex proxy fallback --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy fixture`

**Help forms:** `cq codex proxy fixture`, `cq codex proxy fixture --help`, `cq help codex proxy fixture`.

**Purpose:** Create sanitised test fixtures from saved Codex request bodies.

**Layout, in order:**

1. Bold cyan `cq codex proxy fixture` title; summary beneath.
2. `Usage` heading and `cq codex proxy fixture <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `create` | Create a sanitised fixture from a saved Codex request. |

**Examples:**

- `cq codex proxy fixture --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy hook`

**Help forms:** `cq codex proxy hook`, `cq codex proxy hook --help`, `cq help codex proxy hook`.

**Purpose:** Produce receipt annotations for native client lifecycle hooks.

**Layout, in order:**

1. Bold cyan `cq codex proxy hook` title; summary beneath.
2. `Usage` heading and `cq codex proxy hook <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `stop` | Return a private-data-safe routing receipt for a Codex Stop hook. |

**Examples:**

- `cq codex proxy hook --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy lease`

**Help forms:** `cq codex proxy lease`, `cq codex proxy lease --help`, `cq help codex proxy lease`.

**Purpose:** Invalidate reusable Codex routing affinities.

**Layout, in order:**

1. Bold cyan `cq codex proxy lease` title; summary beneath.
2. `Usage` heading and `cq codex proxy lease <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `invalidate` | Invalidate reusable Codex task-account affinity. |

**Examples:**

- `cq codex proxy lease --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy pin`

**Help forms:** `cq codex proxy pin`, `cq codex proxy pin --help`, `cq help codex proxy pin`.

**Purpose:** Inspect, set or clear the Codex proxy account override.

**Layout, in order:**

1. Bold cyan `cq codex proxy pin` title; summary beneath.
2. `Usage` heading and `cq codex proxy pin <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `clear` | Clear the Codex proxy pin. |
| `set` | Set the Codex proxy pin. |
| `show` | Show the Codex proxy pin. |

**Examples:**

- `cq codex proxy pin --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy policy`

**Help forms:** `cq codex proxy policy`, `cq codex proxy policy --help`, `cq help codex proxy policy`.

**Purpose:** Export or replace the complete Codex routing-policy document.

**Layout, in order:**

1. Bold cyan `cq codex proxy policy` title; summary beneath.
2. `Usage` heading and `cq codex proxy policy <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `apply` | Apply a complete Codex routing-policy document. |
| `show` | Show the complete Codex routing-policy document. |

**Examples:**

- `cq codex proxy policy --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy pool`

**Help forms:** `cq codex proxy pool`, `cq codex proxy pool --help`, `cq help codex proxy pool`.

**Purpose:** Define account pools and their relative capacity-preservation values.

**Layout, in order:**

1. Bold cyan `cq codex proxy pool` title; summary beneath.
2. `Usage` heading and `cq codex proxy pool <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `rename` | Rename a pool without changing its identity. |
| `set` | Create a pool or replace its account membership. |
| `value` | Set a pool’s relative capacity-preservation value. |

**Examples:**

- `cq codex proxy pool --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy prime`

**Help forms:** `cq codex proxy prime`, `cq codex proxy prime --help`, `cq help codex proxy prime`.

**Purpose:** Inspect or toggle automatic Codex quota-window priming.

**Layout, in order:**

1. Bold cyan `cq codex proxy prime` title; summary beneath.
2. `Usage` heading and `cq codex proxy prime <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `disable` | Disable automatic Codex quota-window priming. |
| `enable` | Enable automatic Codex quota-window priming. |
| `status` | Show configured Codex quota-window priming. |

**Examples:**

- `cq codex proxy prime --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy readiness`

**Help forms:** `cq codex proxy readiness`, `cq codex proxy readiness --help`, `cq help codex proxy readiness`.

**Purpose:** Inspect retained transport-readiness evidence for an exact client build.

**Layout, in order:**

1. Bold cyan `cq codex proxy readiness` title; summary beneath.
2. `Usage` heading and `cq codex proxy readiness <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `show` | Check retained HTTP readiness evidence for an exact Codex build. |

**Examples:**

- `cq codex proxy readiness --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy reserve`

**Help forms:** `cq codex proxy reserve`, `cq codex proxy reserve --help`, `cq help codex proxy reserve`.

**Purpose:** Protect remaining quota on the current Codex system account.

**Layout, in order:**

1. Bold cyan `cq codex proxy reserve` title; summary beneath.
2. `Usage` heading and `cq codex proxy reserve <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `clear` | Remove the system-account reserve configuration. |
| `disable` | Disable the reserve until a verified reset. |
| `enable` | Enable the configured system-account reserve. |
| `set` | Set the system-account quota reserve. |
| `status` | Show system-account reserve state and evidence. |
| `windows` | List observed system-account quota window selectors. |

**Examples:**

- `cq codex proxy reserve --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy session`

**Help forms:** `cq codex proxy session`, `cq codex proxy session --help`, `cq help codex proxy session`.

**Purpose:** Inspect or change the routing pool bound to a client session.

**Layout, in order:**

1. Bold cyan `cq codex proxy session` title; summary beneath.
2. `Usage` heading and `cq codex proxy session <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `bind` | Bind one exact session to a Codex pool. |
| `digest` | Return the keyed digest for a session selector. |
| `list` | List all session-to-pool bindings. |
| `show` | Show one session-to-pool binding. |
| `unbind` | Remove one exact session-to-pool binding. |

**Examples:**

- `cq codex proxy session --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex proxy validate`

**Help forms:** `cq codex proxy validate`, `cq codex proxy validate --help`, `cq help codex proxy validate`.

**Purpose:** Request HTTP checks or run WebSocket transport validation.

**Layout, in order:**

1. Bold cyan `cq codex proxy validate` title; summary beneath.
2. `Usage` heading and `cq codex proxy validate <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `http` | Request startup validation of an installed candidate HTTP listener. |
| `websocket` | Validate WebSocket routing with an isolated installed Codex client. |

**Examples:**

- `cq codex proxy validate --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq codex reset`

**Help forms:** `cq codex reset`, `cq codex reset --help`, `cq help codex reset`.

**Purpose:** Inspect, schedule and use banked Codex quota-reset credits.

**Layout, in order:**

1. Bold cyan `cq codex reset` title; summary beneath.
2. `Usage` heading and `cq codex reset <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `list` | List Codex banked reset credits. |
| `recommend` | Recommend when to use Codex reset credits. |
| `use` | Consume one Codex banked reset credit. |

**Examples:**

- `cq codex reset --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help codex`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq gemini`

**Help forms:** `cq gemini`, `cq gemini --help`, `cq help gemini`.

**Purpose:** Inspect the externally managed Gemini account.

**Layout, in order:**

1. Bold cyan `cq gemini` title; summary beneath.
2. `Usage` heading and `cq gemini <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `account` | Show the single externally managed Gemini account. |

**Examples:**

- `cq gemini --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq gemini account`

**Help forms:** `cq gemini account`, `cq gemini account --help`, `cq help gemini account`.

**Purpose:** Show the single externally managed Gemini account.

**Layout, in order:**

1. Bold cyan `cq gemini account` title; summary beneath.
2. `Usage` heading and `cq gemini account <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `show` | Show the externally managed Gemini account. |

**Examples:**

- `cq gemini account --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help gemini`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq models`

**Help forms:** `cq models`, `cq models --help`, `cq help models`.

**Purpose:** Inspect model registries and publish local model-cache updates.

**Layout, in order:**

1. Bold cyan `cq models` title; summary beneath.
2. `Usage` heading and `cq models <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `list` | List active models from local registry caches. |
| `overlay` | Add, remove or prune local model-registry additions. |
| `refresh` | Refresh model sources and publish client model caches. |

**Examples:**

- `cq models --help` — Show group help.

**Related navigation:** Parent: `cq help`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq models overlay`

**Help forms:** `cq models overlay`, `cq models overlay --help`, `cq help models overlay`.

**Purpose:** Add, remove or prune local model-registry additions.

**Layout, in order:**

1. Bold cyan `cq models overlay` title; summary beneath.
2. `Usage` heading and `cq models overlay <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `add` | Add or replace one local model overlay and publish caches. |
| `prune` | Remove overlays now supplied by native model sources. |
| `remove` | Remove one local model overlay and publish caches. |

**Examples:**

- `cq models overlay --help` — Show group help.

**Related navigation:** Parent: `cq help models`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy`

**Help forms:** `cq proxy`, `cq proxy --help`, `cq help proxy`.

**Purpose:** Run and inspect the shared proxy, state and lifecycle machinery.

**Layout, in order:**

1. Bold cyan `cq proxy` title; summary beneath.
2. `Usage` heading and `cq proxy <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `candidate` | Manage isolated candidate state and inspect release evidence. |
| `health` | Probe proxy HTTP liveness. |
| `operation` | Inspect retained shared proxy lifecycle operation state. |
| `rescue` | Inspect or transition the shared runtime rescue-serving mode. |
| `serve` | Run the shared API proxy in the foreground. |
| `state` | Initialise an explicitly selected shared proxy authority directory. |
| `status` | Inspect shared proxy readiness evidence. |

**Examples:**

- `cq proxy --help` — Show all immediate subcommands and usage.

**Related navigation:**

- `cq codex proxy reserve` — Codex quota reserves; previously cq proxy reserve.
- `cq codex proxy policy` — Codex routing policy; previously cq proxy policy. Pools and sessions now live under cq codex proxy.
- `cq codex proxy` — Codex pins, fallback, priming, leases, trace, validation, credential endpoints and hooks.
- `cq claude proxy pin` — Claude routing pins; previously cq proxy pin claude.
- `cq service` — Install, start, stop, restart and uninstall background services.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy candidate`

**Help forms:** `cq proxy candidate`, `cq proxy candidate --help`, `cq help proxy candidate`.

**Purpose:** Manage isolated candidate state and inspect release evidence.

**Layout, in order:**

1. Bold cyan `cq proxy candidate` title; summary beneath.
2. `Usage` heading and `cq proxy candidate <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `client-safety` | Reserve measured client-safety operations; currently unavailable. |
| `prepare` | Prepare an isolated release candidate without starting it. |
| `receipt` | Inspect a retained candidate attempt receipt. |
| `release` | Reserve release activation and qualification; currently unavailable. |
| `remove` | Delete stopped candidate state and retained receipts. |
| `start` | Start the exact release bound to an isolated candidate. |
| `status` | Show retained state for one isolated candidate. |
| `stop` | Stop an isolated candidate after its client has stopped. |

**Examples:**

- `cq proxy candidate --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy candidate client-safety`

**Help forms:** `cq proxy candidate client-safety`, `cq proxy candidate client-safety --help`, `cq help proxy candidate client-safety`.

**Purpose:** Reserve measured client-safety operations; currently unavailable.

**Layout, in order:**

1. Bold cyan `cq proxy candidate client-safety` title; summary beneath.
2. `Usage` heading and `cq proxy candidate client-safety <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `refresh` | Measure and renew client credential-isolation evidence. |

**Examples:**

- `cq proxy candidate client-safety --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy candidate`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy candidate receipt`

**Help forms:** `cq proxy candidate receipt`, `cq proxy candidate receipt --help`, `cq help proxy candidate receipt`.

**Purpose:** Inspect a retained candidate attempt receipt.

**Layout, in order:**

1. Bold cyan `cq proxy candidate receipt` title; summary beneath.
2. `Usage` heading and `cq proxy candidate receipt <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `show` | Show the authenticated receipt for one candidate validation attempt. |

**Examples:**

- `cq proxy candidate receipt --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy candidate`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy candidate release`

**Help forms:** `cq proxy candidate release`, `cq proxy candidate release --help`, `cq help proxy candidate release`.

**Purpose:** Reserve release activation and qualification; currently unavailable.

**Layout, in order:**

1. Bold cyan `cq proxy candidate release` title; summary beneath.
2. `Usage` heading and `cq proxy candidate release <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `activate` | Activate the prepared runtime release inside a candidate. |
| `validate` | Verify candidate release evidence and write a validation receipt. |

**Examples:**

- `cq proxy candidate release --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy candidate`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy operation`

**Help forms:** `cq proxy operation`, `cq proxy operation --help`, `cq help proxy operation`.

**Purpose:** Inspect retained shared proxy lifecycle operation state.

**Layout, in order:**

1. Bold cyan `cq proxy operation` title; summary beneath.
2. `Usage` heading and `cq proxy operation <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `status` | Show a retained shared proxy operation record. |

**Examples:**

- `cq proxy operation --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy rescue`

**Help forms:** `cq proxy rescue`, `cq proxy rescue --help`, `cq help proxy rescue`.

**Purpose:** Inspect or transition the shared runtime rescue-serving mode.

**Layout, in order:**

1. Bold cyan `cq proxy rescue` title; summary beneath.
2. `Usage` heading and `cq proxy rescue <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `enter` | Request shared proxy rescue traffic mode. |
| `exit` | Request return to normal proxy traffic. |
| `status` | Show shared proxy rescue and drain state. |

**Examples:**

- `cq proxy rescue --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq proxy state`

**Help forms:** `cq proxy state`, `cq proxy state --help`, `cq help proxy state`.

**Purpose:** Initialise an explicitly selected shared proxy authority directory.

**Layout, in order:**

1. Bold cyan `cq proxy state` title; summary beneath.
2. `Usage` heading and `cq proxy state <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `initialise` | Initialise shared authenticated proxy state. |

**Examples:**

- `cq proxy state --help` — Show all immediate subcommands and usage.

**Related navigation:** Parent: `cq help proxy`.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.

## `cq service`

**Help forms:** `cq service`, `cq service --help`, `cq help service`.

**Purpose:** Manage proxy and periodic token-refresh background services.

**Layout, in order:**

1. Bold cyan `cq service` title; summary beneath.
2. `Usage` heading and `cq service <COMMAND> [OPTIONS]`.
3. Immediate commands below; command names cyan, descriptions normal; never include this group itself.
4. Concise global options: `-h, --help`, `-j, --json`, `-v, --version`. Help remains text with `--json`.
5. Related commands, then purpose-labelled examples.

| Child | Description |
|---|---|
| `install` | Install and start selected CQ services. |
| `restart` | Restart selected installed CQ services. |
| `start` | Start selected installed CQ services. |
| `status` | Inspect selected CQ service components. |
| `stop` | Stop and disable selected CQ services. |
| `uninstall` | Stop and remove selected CQ service registrations. |
| `upgrade` | Upgrade a Homebrew proxy runtime while retaining its listener. |

**Upgrade boundary:** `service upgrade` is a public Homebrew operation with ordinary help and completion. It retains the released raw receipt and error protocol; help explains this exception. Use `cq service status` to inspect recorded upgrade state and the observed live executable. `proxy runtime-check`, inherited installer locks and private runtime children remain machine interfaces in [the frozen machine ABI](../internal-abi.md).

**Examples:**

- `cq service --help` — Show all immediate subcommands and usage.

**Related navigation:**

- `cq proxy` — Inspect proxy readiness or run proxy in foreground.

**Successful help / bare group:** exit 0; same complete content for all help spellings, no state/network access, no success badge. Root is exceptional: only explicit help renders this page; bare `cq` renders quota data.

**Failure:** unknown child or option uses existing exit-2 diagnostic, entered path and canonical help suggestion. No account/service state may be inspected to render it. If writing fails, use existing safe output-failure diagnostic and exit semantics; do not append a second document.

**Empty / partial / pending / interrupted operational result:** not applicable; this is a pure navigation surface. Do not invent empty-data cards or service-health indicators.

**Narrow / no colour:** child name followed by indented description when columns do not fit; full commands and examples wrap without truncation. Labels and headings retain meaning without ANSI.
