# Codex Dots resource contract

The nine public `codex dots` operations use the standard CLI schema-version-2 envelope. The private `__dots` helper retains its released machine protocol and exit status; it is not a public command or a v2 JSON resource.

`data` is `DotsData`, with `operation` equal to the leaf name and `result` equal to a typed `DotsResult` or null when no receipt was returned. Invalid arguments and failures before execution may have null `data`. No credential, raw provider response or broadcast message body is included.

## DotsResult

Fields are optional observations, preserving the released domain receipt's distinction between absence and an explicit false or zero. Unknown domain-map fields are not copied into the resource.

- Optional booleans: `installed`, `started`, `stopped`, `native_catalog_ready`, `onboarding_required`, `restart_required`, `system_app_modified`, `account_watcher_started`, `account_watcher_existing`, `relay_started`, `relay_existing`, `secondary_executors_existing`, `catalogue_changed`, `discovery_complete`, `discovery_retryable`.
- Optional integer `profiles` records the observed catalogue size, including zero.
- Optional string array `research_components` lists verified helper roles.
- Optional strings: `controller_runtime`, `runtime`, `dock_launcher`, `reason`.
- Optional `accounts` array contains the released account discovery outcomes: required boolean `system` and string `status`; optional strings `owner_alias`, `owner_account_id`, `source`, `failure_type`, `handle`, and optional integer `http_status`.
- Optional `dots` array contains objects with required strings `id`, `name`, `account`, `setup`. Stable `id` identifies routing; display names do not.
- Optional `priming`, `awareness` and `notices` arrays contain `DotDelivery` objects.
- Optional `broadcast` contains required string `id` and a `deliveries` array of `DotDelivery` objects.
- Optional `board` contains optional strings `id`, `thread_id` and optional boolean `bound`. A successful observation of no binding is normalised to `{ "bound": false }`, as is a successful clear. No binding is inferred from a failed observation.

`DotDelivery` has required strings `target` and `outcome`; optional strings `display_name`, `message_id`, `failure_type`, `error`; optional booleans `skipped`, `manual`; and optional number `at` containing the released Unix timestamp in seconds. `accepted` confirms message acceptance, not a completed response. `rejected`, `pending` and `unknown` remain distinct; an existing skipped attempt retains its original outcome. `included_in_setup` records inclusion in setup instructions.

Successful empty list and delivery arrays remain arrays. Fields absent in the released receipt are omitted. Ordering follows the released domain operation, which sorts Dot handles; no adapter reordering or synthetic timestamps are introduced.

## Completion and failures

A nil controller error preserves the released exit 0. This indicates that the operation returned a receipt, not that every possible follow-on action completed. Restart required, onboarding required, explicit `started: false`, incomplete discovery, and any delivery outcome other than `accepted` or `included_in_setup` add a `dots_incomplete` warning. Human and terminal output show each observation separately; they never turn pending work into a completed start or delivery.

A controller error retains any valid partial receipt and reports exit 1 with `dots_operation_failed`. Error text is a fixed safe diagnostic; raw upstream error bodies are not emitted. Cancellation is exit 130 with `interrupted`; a context deadline is exit 7 with `dots_timeout`. An unencodable or wrongly typed receipt is exit 1 with `dots_result_invalid`; a nil receipt with no controller error is exit 1 with `dots_result_missing`. These errors do not claim rollback or retry delivery.

## Validation and presentation

Help, bare groups and parser errors complete before operational lookup. Adapter validation checks explicit clean absolute paths, install mode, exactly one prime selector, the broadcast Unicode length and nonblank requirement, and the board thread/clear combination before resolving the home directory or reading state. The released runner then protects native-app and credential locations and resolves native launch requirements.

`--root` belongs to the Dots group and is accepted before or after the leaf; duplicate occurrences are rejected. It is not a global option. `board --clear=false THREAD` binds the thread; `prime --all=false DOT` selects the Dot. The positional delimiter preserves message text that begins with a hyphen.

Terminal roles come from typed fields: identifiers remain identity-coloured even when their text contains words such as failed; accepted and ready observations are confirmed; unknown, pending and required user actions attract attention; rejected is failure. False app modification, absent setup and zero counts are neutral observations. Redirected output is readable text without terminal escapes. JSON emits one envelope and never mixes legacy output into stdout.
