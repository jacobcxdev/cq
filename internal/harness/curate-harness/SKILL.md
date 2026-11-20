---
name: curate-harness
description: Reduce, expand, replace or restore a Codex chat's CQ harness profile when its role changes or new skills, MCP servers or plugins are needed. Also manage reusable profiles and requested workspace or model bindings.
metadata:
  short-description: Update Codex capabilities in place with CQ
---

<!-- cq-owned:curate-harness -->

## Discover

Use `cq codex harness --help` and relevant subcommand help. If inventory, profiles
or bind/explain are missing, report the version mismatch. Use a development CLI
only if its location was provided. Leave installation and launch settings alone.

Use the actual workspace root, including for multi-repo workspaces. Default to
`--chat current`, which validates `CODEX_THREAD_ID`. If missing, request the
exact chat link or ID. Change project/global scope only when requested.

- `cq codex harness inventory --project ROOT --json` lists configured plugins,
  standalone MCP servers and skills **including those excluded by curation**.
  Use `--kind` and `--search` for named additions. Refresh discovery when the
  inventory or chat role changes, without enabling everything or polling each turn.
- `cq codex harness explain --chat current --project ROOT --json` shows model,
  `effective.layers`, capability decisions and application status. Filter JSON
  output before reading it into context. Keep the layers, receipt status, issues
  and relevant capability rows. Supply the exact `--model ID` if lookup fails.

## Choose and bind

Use stable inventory IDs and include dependencies. A plugin is a bundle with its
declared `mcp_servers`. Standalone MCP rows are separate. Preserve CQ's curation
entrypoint and core tools such as web search, shell, file edits, user input and
task control. Built-in tools and Codex's Search connector sit outside the profile
inventory. CQ preserves Search when native app settings allow it, including with
a complete allowlist. The Browser plugin controls the in-app browser.

Save a definition with `cq codex harness profile save NAME --file FILE`. Example
delta, using a placeholder ID:

```json
{"description":"Add release review","changes":{"skills":{"repo:release-review":"enabled"}}}
```

Decisions in `plugins`, `mcp` and `skills` are `enabled`, `disabled` or
`inherit` (keep the prior decision). Omit `default` for a delta. Use
`"default":"disabled"` plus enabled IDs for a complete allowlist. Store only
portable decisions, without native paths, commands, URLs or secrets.

- **Expand or tune:** Save a delta and append it to the current chat layer's
  profile list. Newly discovered entries may need explicit enables under an
  allowlist. Keep the current binding if it already matches.
- **Change role:** Edit the JSON from `profile show NAME --json` and save under
  a new name, or create a fresh definition. Preserve shared definitions.
  Save chat profiles in the user registry. Pass `--project ROOT` to
  `profile save` for workspace definitions.
- **Restore:** Rebind the previous profile list to undo a chat update. Use
  `unbind` to remove that layer and inherit policy, or bind `full` to
  intentionally reset to native choices. Use export/import to share definitions.

Run `cq codex harness bind PROFILE... --scope chat --chat current --project ROOT`.
Binding replaces the list for that scope and selector. Names apply in order.
Preserve the existing model selector when updating a binding. Omit `--model`
for all models at that scope or use the intended ID/family (e.g. `'glm-*'`).
Binding requests application and keeps the same chat and history.

## Verify and recover

Re-read `explain`. Use the matching `application_status` to report state and
mode. `full` applies plugin, standalone MCP and skill changes at idle boundaries.
`plugins` covers plugins only. Report pending or partial changes, unsupported
controls and dependency issues. If no receipt matches, application is unconfirmed.
After application, check the changed capabilities and basic tools. If curation
removed a core tool, restore the previous binding and verify recovery.

For a matching failed full-mode receipt, address its failure stage/reason and use
`cq codex harness retry --chat current --project ROOT` once if supported. Use the
attempted model/project when the receipt records a failed context switch. Re-read
`explain`. Rebinding an unchanged profile does not replace this recovery step.

CQ may hold and resume automatic goal continuation during an idle reload.
`native_goal_api_unavailable` requires a valid native goal probe.
`native_goal_restore_ambiguous` requires an explicit user goal update. Leave
the recovery marker and goal state intact. Native child agents inherit the
parent's capabilities. Independent curation before their first turn is not
guaranteed.
