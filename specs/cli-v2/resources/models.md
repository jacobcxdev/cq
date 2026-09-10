# Models and auth specification notes

Draft only. `../commands.json` defines nine command/group objects. Both implementations support list, refresh, overlay add/remove/prune and token refresh; proposed structure preserves those operations. Root owns service replacement and shared globals/envelope. JSON fields below describe `data`, not outer envelope. Every canonical unchanged leaf accepts its previous valid invocation; aliases arrays record changed paths/values only.

## Resource schemas

All object fields below required unless explicitly nullable. All arrays may be empty. IDs retain case. No provider-native raw JSON is emitted: opaque vendor metadata is an internal publication concern, avoiding an unbounded unstable CLI schema. Unknown numeric metadata uses null, not zero. Schema-v2 output intentionally replaces legacy bare model entry array; root supplies version/migration policy.

- `HelpCommand`: `{path: string, summary: string}`. Path excludes `cq`.
- `ModelIdentity`: `{provider: "claude"|"codex", id: string}`.
- `ModelEntry`: `{provider: "claude"|"codex", id: string, aliases: string[], display_name: string|null, description: string|null, context_window: integer|null, max_context_window: integer|null, max_output_tokens: integer|null, visibility: string|null, priority: integer|null, source: "native"|"overlay", clone_from: string|null, inferred_from: string|null}`. Context/token counts positive when present. Visibility preserves provider-native descriptive string; it is not a CLI enum or permission. Priority preserves integer rank when present. `clone_from` is explicit requested source; `inferred_from` records actual metadata source including an explicit source. `aliases` lists actual model aliases, not CLI command aliases. `raw` deliberately excluded.
- `ModelCloneSelectionResult`: `{mode: "explicit"|"inferred"|"none", source_id: string|null}`. Human `source_id_or_none` renders null as `none`; no other null-to-string conversion.
- `ModelSourceResult`: `{provider: "claude"|"codex", status: "refreshed"|"failed", native_count: integer>=0, malformed_count: integer>=0, error_code: string|null, message: string|null}`. Error code is a stable machine code; message is redacted operational diagnostic. Native count refers to source's accepted entries, malformed count to rejected entries. Source failure cannot be presented as successful freshness.
- `ModelPublicationTarget`: `{target: "codex_cache"|"claude_capabilities"|"claude_picker", path: string, status: "written"|"skipped"|"failed", reason: "published"|"optional_client_absent"|"write_failed", error_code: string|null, message: string|null}`. Path is resolved local absolute path. No token/password data. A skipped absent optional client target is success, not partial failure.
- `ModelPublication`: `{status: "complete"|"partial"|"failed", via: "proxy"|"local", active_count: integer>=0, sources: ModelSourceResult[], targets: ModelPublicationTarget[]}`. Sources ordered claude,codex; targets ordered codex_cache,claude_capabilities,claude_picker. Human refresh template repeats target line once per target; `{publication.target}` and `{publication.status}` on repeated line denote target fields. Elsewhere `publication.status` is overall status. Source failure with usable other source or publication success yields partial. No usable refresh/publication success yields failed. Publication pipeline must expose real write outcomes, not infer them from no returned error.
- `AuthRefreshAccountResult`: `{account_label: string, account_key: string|null, credentials_changed: boolean, status: "refreshed"|"unchanged"|"reauth_required"|"failed", reason: "refreshed"|"not_expiring"|"unknown_expiry"|"no_refresh_token"|"read_only_source"|"reauthenticated"|"reauthentication_skipped"|"refresh_failed"|"store_failed", error_code: string|null, message: string|null}`. Codex account_key is opaque AccountKey; Claude null. Account label is display label/email or `unknown`, never token, token fragment, credential filename content, or derived secret. Aggregate multiple candidate attempts for one logical account: refreshed if any eligible candidate succeeds and none fail; failed if any eligible attempt fails after others succeed (with partial command result). Read-only candidates may appear unchanged; no write.
- `AuthRefreshProviderResult`: `{provider: "claude"|"codex", credentials_changed: boolean, changed_count: integer>=0, refreshed: integer>=0, unchanged: integer>=0, reauth_required: integer>=0, failed: integer>=0, accounts: AuthRefreshAccountResult[]}`. Status counts match final account statuses. credentials_changed is true if any selected logical account had a committed credential write, including reconciliation; changed_count counts distinct changed logical accounts, independent of final status. A write followed by an error still counts. Top-level credentials_changed is the OR across providers and changed_count is the sum of provider changed_count. No discovered accounts yields zero counts and successful result. Accounts ordered by display label then account_key, null before string. Human output prints one row per provider. Complete run with zero failed/reauth-required exits 0; partial success exits 8; all auth failures or unresolved reauth exits 5. Unavailable/degraded authority and store failure retain assigned code unless other-provider work succeeded, in which case overall exit 8 and constituent errors remain in errors array. Existing unchanged work alone does not count as successful attempted refresh when choosing between 5 and 8.

## ModelCloneSelection: deterministic metadata inference

Preserve existing inference algorithm from `internal/modelregistry/infer.go:10-97`, make its behaviour visible. Explicit `--clone-from` searches only native entries belonging to selected provider and exact ID. Draft changes absent explicit source from silent incomplete metadata to not-found error before write. This is validation, not a new feature. `models refresh` may be required before `add`; add validates against a loaded native snapshot before committing and must not require live network merely to interpret a flag.

Without explicit source:

1. Tokenise lowercased model IDs on `-`, `_`, and `.`; discard empty tokens. Tokens entirely digits are numeric/version signals. Others are family tokens.
2. Score each same-provider native by Jaccard similarity of nonnumeric token sets (intersection / union). Keep positive-score candidates only.
3. Sort descending by similarity, then descending number of consecutive matching leading family tokens, then descending lexicographic numeric-token values; if numeric sequences share prefix, longer sequence first; final tie ascending native ID byte order.
4. If overlay has nonnumeric tokens, require similarity >= 0.5. If no candidate qualifies, return mode none and retain missing metadata. This is allowed; model ID availability is not asserted.
5. Copy only unset fields: display name, description, context window, maximum context window, maximum output tokens, visibility and priority. Never replace provider, ID, source or explicit clone-from. Record selected ID as inferred_from.

Codex publication with an explicit clone copies native vendor metadata internally before replacing model identity fields. Inferred-only selection currently copies normalised metadata and synthesises publication fields; it does not imply byte-for-byte vendor metadata cloning. Help uses “missing metadata”, not “complete copy”. Existing `Merge` inference short-circuit must not bypass missing output/token fields merely because four other fields are populated.

Native entries with same provider/exact ID supersede overlays in authoritative registry refresh; prune removes these redundant stored overlays. Current local listing removes shadowing native entries before merge (`cmd/cq/models.go:314`), so list and refreshed registry can disagree. Canonical list must match authoritative native-wins rule and report native source for collision. Cross-provider case-insensitive duplicate IDs remain rejected by `internal/modelregistry/validate.go:10` because routing otherwise ambiguous.

## Examples and storage

Examples using `$SOURCE_MODEL` and `$NEW_MODEL` require this preparation. Select SOURCE_MODEL from the native rows actually displayed; no current upstream model IDs are assumed.

```sh
cq models refresh
cq models list --provider codex
printf 'Exact native source model ID shown above: '
read -r SOURCE_MODEL
printf 'Desired new upstream model ID: '
read -r NEW_MODEL
cq models overlay add --provider codex --id "$NEW_MODEL" --clone-from "$SOURCE_MODEL"
```

The explicit-source validation verifies the selected source is native and same-provider before writing. NEW_MODEL is user-selected; adding an overlay does not assert upstream support.

Model overlay store remains `$XDG_CONFIG_HOME/cq/models.json` with existing resolver fallback; internal on-disk provider enum may remain anthropic to avoid unrelated migration. Public provider spelling is claude. Codex publication target `$CODEX_HOME/models_cache.json`; Claude capabilities `$CLAUDE_CONFIG_DIR/cache/model-capabilities.json`; picker path follows existing discovered installation rules. Exact resolved paths reported in publication outcomes. Service installation never occurs as a side effect.

## Compatibility and required implementation changes

- Legacy `cq refresh` translates to `cq auth refresh`; its old no-argument grammar stays strict. Existing LaunchAgent task updates belong to service implementation, not this family specification.
- Legacy models `--provider anthropic` maps to claude, with warning. Existing paths and `--id`/`--clone-from` retained; no needless parameter rename. Models provider choices only claude/codex because Gemini has no registry implementation.
- Reject unknown trailing flags on refresh/prune; never implement generic dry-run. Standard global JSON/flag grammar comes from root.
- Auth provider selection is new CLI filtering of existing operations. Extract independent provider execution while preserving credential authority and owned-write boundaries.
- Current token refresh prints diagnostics rather than structured outcome; Codex per-candidate failure currently swallowed (`cmd/cq/refresh.go:204-206`), Claude store failure also only printed. New report aggregation and truthful exit status required.
- Interactive Claude reauthentication preserved, JSON/non-TTY disabled. EOF leaves unresolved work; do not report success.
- Current publication APIs may log failure without returning structured evidence. Refactor reporting enough to fulfil schema; do not claim legacy engine already produces every field.
- Explicit clone-source validation and native-wins local list are deliberate fixes; no new model management subcommands, no overlay-only filters, no per-provider prune option, no arbitrary timeout option introduced.

## Fixed request deadlines; no command-wide deadline

No models/auth command introduces an overall command deadline or a timeout flag. Existing local I/O and terminal prompt waits remain unbounded except interruption. Specific existing bounds:

- Models proxy registry refresh: 5-second context deadline (`cmd/cq/models.go:162`); deadline expiry is reported, not bypassed with local fallback.
- Models local registry source refresh: 30-second phase context (`cmd/cq/models.go:178`); registry HTTP client 30 seconds per request (`cmd/cq/local_registry.go:52`). Publication and prune occur outside that source-refresh deadline.
- Legacy proxy health selection probe, when used: 2-second HTTP timeout (`cmd/cq/models.go:58`).
- Auth refresh HTTP client: 10 seconds per request (`cmd/cq/refresh.go:55`). Credential-authority transport retains its internal safety bounds rather than inventing a user-facing whole-command deadline.
- Claude interactive OAuth: 5-minute browser-callback wait (`internal/auth/oauth.go:232`); callback server header/read/write timeouts are 10/15/15 seconds and shutdown timeout 3 seconds (`internal/auth/oauth.go:199-212`). Terminal confirmation before starting OAuth has no deadline.
