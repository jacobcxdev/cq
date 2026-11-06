---
name: curate-harness
description: Curate and apply CQ capability profiles when asked to reduce, restore, reuse or configure skills, MCP servers or plugins for a chat, workspace or model.
metadata:
  short-description: Curate scoped Codex capabilities through CQ
---

<!-- cq-owned:curate-harness -->

Use CQ's CLI; make task-specific choices without another MCP or model service.

1. Check `cq codex harness --help` and relevant subcommand help. If inventory,
   named profiles or bind/explain are unavailable, report the version mismatch.
   Do not claim success, install an upgrade or change unrelated launch settings.
   Use a development CLI only when the session provides its location.
2. Default to current chat. Change workspace/global policy only when requested.
   An explicit GLM restriction uses `--model 'glm-*'`; omitting model applies to
   all models at that scope. Identify the workspace root from task context; it
   can contain several repositories. Do not substitute a nested Git root.
3. Use `--chat current`: CQ validates `CODEX_THREAD_ID`. Never substitute a shared
   session ID, cache key, latest chat or guessed ID. Ask for the target if missing.
4. Read `cq codex harness inventory --project ROOT --json`, optionally filtering
   by kind/search. This is the full configured metadata inventory, including
   capabilities excluded by current profiles. Do not curate only from tools
   visible in this chat or temporarily enable everything to discover choices.
   Read `explain --chat current --project ROOT --json` for inheritance and model;
   supply `--model ID` when native model lookup is unavailable.
5. Select relevant capabilities and available dependencies. A plugin-owned skill
   may require its plugin and expose coupled tools. Use exact stable inventory
   IDs. Plugins are bundles: selecting one includes its declared `mcp_servers`;
   standalone MCP rows are controlled separately. Read details only for plausible
   candidates. Never store native paths,
   versioned cache locations, commands, URLs, environment values or credentials.
6. Save a Definition JSON containing description and changes. Decisions are
   enabled/disabled/inherit in plugins, mcp and skills maps. Use default:disabled
   plus enabled IDs for a complete allowlist; omit default for a narrower delta.
   Do not reset inherited policy unless asked to restore the native baseline.
   Use `profile save NAME --file FILE`; task profiles belong in user registry,
   workspace profiles use `--project ROOT`. Do not replace a shared definition to
   change one chat. Use profile copy for an independent variant or save a delta.
7. Bind with `cq codex harness bind NAME --scope chat --chat current --project ROOT`.
   Use --scope project or global only when requested. Add --model only for the
   intended restriction. Bindings request application automatically. For reuse
   outside a workspace, export/import definitions; copy stable IDs, not private
   chat bindings or receipts. Follow actual CLI help for flags.
8. Re-read explain before reporting. Distinguish saved/requested/pending/partial
   from confirmed application. State retained/removed capabilities and scope
   briefly. Read `application_status.mode` from the current receipt: `full`
   covers plugins, standalone MCP servers and local skills; `plugins` covers
   plugins only. Do not infer the active mode from launch method or generic help.
   If no current receipt establishes the mode, report it as unconfirmed.
   Report unsupported controls, missing capabilities and
   dependency conflicts. Existing history/catalogue text stays intact.

Keep CQ's small curation entrypoint and native core tools available. Native child
agents inherit parent capabilities; independent model-based changes before an
internally spawned child's first turn are not guaranteed.
