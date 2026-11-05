# Layered Harness Profiles Implementation Plan

> **For agentic workers:** Use the existing bounded policy, inventory, CLI and launcher owners. Root owns integration and the remaining-work checklist below. Do not create another tracker or delegate coordination.

**Goal:** Let agents curate reusable capability profiles, combine global/workspace/chat and model rules, and apply the resulting selection without losing chat history.

**Architecture:** Extend the existing private policy, standard Desktop controller and launcher bridge. A complete native metadata inventory remains separate from the reduced chat context. A small curation skill drives the CLI; CQ resolves and applies rules deterministically without an MCP server or an extra inference service.

**Tech Stack:** Go, existing fsutil persistence and native Codex stdio/Desktop IPC; Markdown skill.

**Spec:** Accepted design below, approved by the user's instruction to implement it on 5 November 2026.

## Accepted design

- Resolve global all-model rules, global matching model rules, ancestor workspace all/model rules, then chat all/model rules. Narrower scope wins; matching model families apply broad to narrow, then exact model. Version 2 applies to any model, with no implicit GLM-only gate.
- Named profiles contain capability deltas. Each plugin, MCP server and skill can be enabled, disabled or inherited. A kind wildcard and optional overall default support curated allowlists. An explicit reset returns to the native baseline. Built-in full/minimal remain available; no profile inheritance graph is needed.
- User profiles/global rules/private chat bindings live in CQ's harness.json. Portable workspace definitions/rules live in .cq/harness.json. Runtime receipts remain private and are not the source of policy.
- Preserve version 1 reads and semantics. Explicit promotion converts previous GLM defaults/overrides into model-specific version 2 rules without silently changing other models.
- Inventory uses native plugin/installed, skills/list and config/read in one bounded read-only child. Return names, descriptions, stable per-kind IDs, availability and dependencies, including disabled entries. Never emit raw configuration, credentials, command arguments or URLs. Do not start every MCP just to discover schemas.
- Profile definitions store stable IDs, not versioned cache paths. Runtime inventory maps skill IDs to native paths. Export/import moves definitions without private chat IDs or machine paths. Copy creates an independent definition; ordinary binding reuses named definitions with local deltas.
- CLI supports inventory, named profile operations, scoped bindings, explanation/provenance and portable export/import. Current chat resolves from validated CODEX_THREAD_ID, never a shared session/cache key. Missing identity fails explicitly.
- A first binding enables a fresh policy and requests application to the current/explicit chat. Narrow bindings preserve an already-disabled policy with existing rules; explicit enable/global binding activates it. The standard path can enforce plugin exclusions only and must report unsupported MCP/skill requests. The launcher enforces qualified plugin/server/skill controls at idle boundaries. Preserve baseline restoration, current chat settings, manual-divergence protection and cross-process leases.
- Curation defaults to the current chat unless the user asks for a broader scope. It reads the full inventory plus inherited policy, adds required dependencies conservatively, saves and binds a profile, and reports applied/pending/unsupported state. Keep its CLI escape path discoverable when context is reduced.
- Core tools, native permissions and history stay intact. Native children inherit parent capability settings; internal mixed-model spawns have no independent pre-first-turn bridge hook. A fresh stock native process resumes normal global capabilities.

## Global constraints

- Standard ChatGPT launch remains the default. CQ launcher is optional and supplies broader enforcement only when actually present.
- No new CQ MCP, daemon, app-bundle patch, provider credential or paid curation service.
- Every Go test uses -race. Run build, vet, full race tests, meaningful native-client acceptance and independent review before completion.
- No production profile activation, release, remote write or message to another chat is implied by implementation. Local skill installation may only create CQ-owned content and must not overwrite unrelated skills.
- Atomic bounded persistence; preserve existing configuration and fail on unresolved references or malformed native inventory.

## Review focus

- A globally reduced chat can still discover capabilities excluded by that profile and re-enable them through a narrower rule.
- An all-model task override beats a global model-specific rule; changing model recalculates the selection and restores original settings.
- Profile copy/export carries no versioned skill path or private chat binding; missing capabilities are reported rather than silently widened.
- Unsupported standalone controls in a standard launch produce a partial/pending result, not a false applied claim.
- Concurrent settings/profile edits, active turns, cancellation and process restart preserve user choices and release ownership correctly.

## Remaining work

- [x] Policy/resolver/storage: implement v2 types, precedence, named definitions, provenance, v1 promotion and portable workspace reads/writes; test invalid inputs and inheritance.
- [x] Full inventory: native metadata snapshot, stable IDs, disabled visibility, dependency metadata, redaction, bounded lifecycle and installed read-only proof.
- [x] CLI: named profile lifecycle, scoped bind/unbind, current chat/model, inventory filtering, explain, export/import and command documentation.
- [x] Standard integration: contextual resolution, selective plugin lists, explicit unsupported/unavailable receipts, live policy changes and direct current-chat application.
- [x] Launcher integration: selective plugin/MCP/skill controls, effective fingerprints, context policy, restoration and native per-skill qualification.
- [x] Curation skill: repo-owned embedded skill and explicit install path, scope rules, full-inventory workflow, fallback/version checks and isolated forward test.
- [x] Qualification: independent review, installed selective bridge/history/isolation/restart checks, build/vet/full race suite, cross-platform builds and signed local commit.

## Interfaces and ownership

`internal/harness` owns `Kind`, `Decision`, `Changes`, `Definition`, `Binding`, `Scope`, `Effective`, `WorkspacePolicy`, strict stores and `LoadForContext`. `Selection` stays comparable through a fingerprint. `Effective.Choose(kind,id,baseline)` returns the final state; Reset means native baseline, not unconditional enable.

`NativeInventory.Snapshot(ctx,cwd)` returns `Inventory{Capabilities,Issues}`. Capability metadata contains per-kind stable ID, native ID, skill path, optional owning plugin/dependencies and baseline availability. Paths are runtime details and never enter profile definitions.

CLI injects snapshot, apply, readModel, getenv and getwd functions. Root wires native dependencies and direct application. Launcher owns only its existing bridge files and optional context/inventory callbacks. Existing receipts gain fingerprint and explicit unsupported/issues metadata without storing history.

## Verification sequence

1. Add failing resolver, storage, inventory and CLI boundary tests before their implementation; run focused race checks as each integrated slice lands.
2. Prove selective standalone/plugin skill and MCP behaviour against isolated installed native client with synthetic loopback responses, then prove the actual bridge path.
3. Exercise curation skill in an isolated workspace through real CLI with temporary configuration; inspect saved scope, stable IDs and explanation.
4. Review final diff independently. Resolve findings with regressions. Run go build ./..., go vet ./..., go test -race -count=1 ./... and Linux/Windows builds.
5. Sign a local commit only after checks pass. Keep merge/release/install distinct from source completion.

## Qualification evidence

- Independent policy, inventory, standard-controller, launcher and CLI review findings resolved with regression tests. Disabled-policy promotion no longer adds implicit global restrictions; narrow bindings do not reactivate existing disabled rules. Unmatched models perform no launcher capability work. Workspace changes recapture the correct native baseline.
- Installed native inventory read returned 49 plugins, 24 MCP servers and 122 skills, including disabled entries. Duplicate skill names use stable source-qualified IDs. Raw configuration is not emitted.
- Final installed-client acceptance on 5 November passed all three `TestHarnessInstalledChatCapabilityIsolation` subtests against the installed ChatGPT native executable. Synthetic loopback traffic proved selective plugins/MCP/skills, model changes, workspace edits, restoration, same-chat history/identity and scope isolation. Six target reloads, zero task-sibling reloads, one matching workspace-sibling reload and zero outside-workspace reloads. No external cloud requests.
- A separate curation agent used the real development CLI in an isolated fixture to discover the full inventory, stack a workspace GLM delta over a global restriction, export/import it into another workspace and preserve non-GLM behaviour. Effective fingerprints matched across copied workspaces; no private chat identity or native path entered exported definitions. With no Desktop IPC in that fixture, bind/explain correctly reported pending rather than confirmed application.
- Skill frontmatter validation and installer race tests passed, including owned upgrades, idempotence, unrelated-content refusal, symlink/path/lock boundaries and CODEX_HOME defaults.
- macOS build and vet passed. Linux ARM64 and Windows AMD64 CLI cross-builds passed. Final `go test -race -count=1 ./...` passed 9,480 tests across 34 packages. The local implementation commit is signed after these gates; no remote integration or production executable upgrade is included.
- Installed the CQ-owned curation skill at `~/.codex/skills/curate-harness/SKILL.md`. Validation passed and a second install reported unchanged. No live profile or existing chat configuration was activated.

Native limitations remain explicit: stock Desktop controls plugins only and cannot guarantee first-turn application; launcher-owned MCP coupling cannot always be separated from retained plugins; internal child creation has no independent model-specific first-turn hook. Existing history is retained, including catalogue text already in history.
