# Codex Dots presentation

This chapter extends the terminal presentation contract to the released multi-account Dots controller. It defines command-owned help and human output; the [resource contract](../resources/dots.md) defines JSON, effects and exit semantics. Concrete hand-crafted examples live in [examples/dots.md](examples/dots.md). Examples are synthetic fixtures, never evidence of an installed app or delivered message.

`codex` identifies the provider. `dots` retains the released product name and belongs under that provider because its accounts, local executors and native app integration are Codex-specific. A Dot is a discovered assistant identified by a stable handle; a display name may repeat. The installation root is private adapter state, not the native app profile or the active Codex credential directory.

## Shared presentation rules

Use a bold `Codex Dots · {operation}` heading. Indent facts by two spaces and Dot account/setup details by four. Display only observations returned by the controller; never run a second query for decoration. Preserve false and zero separately from unavailable fields. An empty Dot list says `Dots: None discovered`; an empty delivery list says `Targets: None`. Omitted observations stay omitted. A successful board inspection or clear with no binding says `Board: Not bound`; a failed board observation says `Board: Unavailable`.

Use cyan for stable handles, account/display names, board links, launcher paths and broadcast IDs; blue for profile counts; green for confirmed `Yes`, `Ready`, `Running`, `accepted` and `included_in_setup`; amber for `Not ready`, pending/unknown delivery, restart/onboarding requirements and incomplete discovery; red for explicit `failed` or `rejected`. A name containing “failed” remains an identity, never a failure status. `System app modified: No`, `Catalogue changed`, absent binding and zero counts are neutral. Colour always accompanies readable status words. NO_COLOR retains the same hierarchy; redirected output is readable text without escapes.

Rows wrap within the terminal width without truncating copyable identifiers. The receipt does not justify a synthetic percentage or estimated completion time. Delivery timestamps, when available in JSON, retain their released Unix-seconds representation; no missing timestamp becomes “now”. A delivery acceptance proves acceptance only, not a completed assistant response. Do not echo the broadcast body or raw upstream errors.

Every help page uses the shared help structure: command title, summary, Usage, relevant context, Arguments, Options, Global options and Examples. Syntax receives the identity colour; headings are bold; explanation uses the body colour. The global options are `-h, --help` (show help), `-j, --json` (emit the schema-version-2 envelope and disable prompts), and `-v, --version` (show version). Help, bare groups and syntax errors require no state access. Group `--root` is inherited and may precede or follow the leaf, but duplicate occurrences are rejected.

All leaves preserve valid partial receipts on stdout. Diagnostics use the shared stderr error frame with the exact code, message and command. A nil controller error with pending restart/onboarding, incomplete discovery, `started: false` or a non-success delivery returns exit 0 plus `dots_incomplete`; it never prints an unconditional completed-success banner. Controller failure is exit 1 even when a receipt exists. Cancellation or timeout does not prove rollback, nor permit automatic message replay.

## `cq codex dots`

**Decision:** Which Dots operation matches the required action?

### Help

Bare invocation and `--help` show all nine children with their summaries, the inherited `--root ABS_PATH` option and the shared global options. Print no runtime output or installation status.

- `board`: Show or assign one local chat as the shared Dot message board.
- `broadcast`: Send a user message to every discovered Dot.
- `install`: Install multi-account Dots for the normal native app.
- `list`: List discovered Dots.
- `prime`: Prime one Dot or all discovered Dots.
- `start`: Start Dots in the configured native app.
- `status`: Show Dots status.
- `stop`: Stop the Dots adapter after quitting the app.
- `sync`: Refresh Dots account discovery.

`--root` defaults to `~/.local/share/cq/dots`, resolved only after syntax validation. It must be a clean absolute path when explicit and cannot overlap native app, home or credential state. It is not global outside this group.

Examples: `cq codex dots --help`; `cq codex dots --root /tmp/cq-dots status --help`. Unknown children use the shared pure parser error frame.

## `cq codex dots board`

**Decision:** Which existing local chat is bound as the shared message board?

**Effect:** With no argument, show the binding. With THREAD, bind that exact local chat and show each returned notice outcome. With --clear, remove the binding. A successful clear is not a claim that past board messages were deleted.

### Help and parameters

Summary: Show or assign one local chat as the shared Dot message board. The exact plain reference is [command help](../help/codex-dots-board.txt); its terminal form follows the shared structure above.

- `THREAD` (optional): Existing local thread ID or codex://threads/ID link. Omit to show the binding. Exact lower-case UUID or codex://threads/UUID link. Cannot combine with --clear.
- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.
- `--clear`: Remove the shared message board binding. Default: `false`. Cannot combine --clear=true with THREAD; --clear=false permits THREAD.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show the board link or `Not bound`. Show the `Board notices` block when returned; each row keeps its stable target and outcome. An empty notices list says `Targets: None`. A partial notification keeps the known binding and adds the warning; never retry unknown notices automatically.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots board` — Show the shared board binding.
- `cq codex dots board codex://threads/01234567-89ab-cdef-0123-456789abcdef` — Bind one existing local chat as the shared board.
- `cq codex dots board --clear` — Remove the shared board binding.

## `cq codex dots broadcast`

**Decision:** Which discovered Dots accepted this exact user message?

**Effect:** Send one CQ-labelled user message to every discovered Dot. This explicit command authorises sending; no hidden priming or uncertain-send retry is implied.

### Help and parameters

Summary: Send a user message to every discovered Dot. The exact plain reference is [command help](../help/codex-dots-broadcast.txt); its terminal form follows the shared structure above.

- `MESSAGE` (required): Message to send once to every discovered Dot (1 to 3000 characters). Valid UTF-8 containing 1 to 3000 Unicode characters; must not be whitespace-only. Quote a multi-word message as one argument; use -- before a message beginning with a dash.
- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show `Broadcast: {id}` followed by `Deliveries`, with one stable target and outcome per returned row. Include available display names and failure classifications. An empty recipient set says `Targets: None`; it is not an all-delivered success. Pending, rejected and unknown delivery retain individual states.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots broadcast 'Please summarise your current work.'` — Send one message to every discovered Dot.

## `cq codex dots install`

**Decision:** Was the private Dots adapter installed, and what remains before it can run?

**Effect:** Install the released Go adapter and its private state. Native mode uses the existing app profile and creates the CQ launcher; isolated mode selects the research profile. Preserve native app integrity and credential ownership.

### Help and parameters

Summary: Install multi-account Dots for the normal native app. The exact plain reference is [command help](../help/codex-dots-install.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.
- `--mode MODE`: Use the normal native profile or an isolated research profile. Default: native. Default: `native`. Exact enum native or isolated.
- `--app ABS_PATH`: Native app bundle. Default: /Applications/ChatGPT.app. Default: `/applications/chatgpt.app`. Clean absolute path.
- `--system-auth ABS_PATH`: Read-only native Codex authentication path. Default: ~/.codex/auth.json. Clean absolute path when supplied.
- `--cq-accounts ABS_PATH`: Read-only CQ Codex accounts directory. Default: ~/.codex/accounts. Clean absolute path when supplied.
- `--account-source ABS_PATH`: Optional read-only CodexBar account directory. Omission adds no source. Clean absolute path when supplied.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show returned installation/start/native-catalogue observations, onboarding/restart requirements, discovery state, watcher/relay state, app-modification observation, profile count and launcher path. Retain any priming or awareness delivery blocks. A restart or onboarding requirement remains visible even when installation succeeded.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots install` — Install Dots using native mode and default paths.
- `cq codex dots install --root /tmp/cq-dots --mode isolated --app /Applications/ChatGPT.app --system-auth /tmp/codex/auth.json --cq-accounts /tmp/codex/accounts --account-source /tmp/codexbar/accounts` — Install an isolated research profile with explicit read-only sources.

## `cq codex dots list`

**Decision:** Which Dots were discovered, and what are their stable IDs and setup states?

**Effect:** Read the released controller catalogue without selecting an account or sending a message.

### Help and parameters

Summary: List discovered Dots. The exact plain reference is [command help](../help/codex-dots-list.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show each Dot name and stable ID, then its account association and setup state. Preserve domain order. An observed empty array says `Dots: None discovered`; absent inventory does not become an empty list. Incomplete discovery remains an explicit warning alongside available rows.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots list` — List discovered Dots.

## `cq codex dots prime`

**Decision:** Did the selected Dot or all discovered Dots accept an explicit setup reminder?

**Effect:** Send a fresh setup reminder to exactly one stable handle/unambiguous display name, or to every discovered Dot with --all. This is an explicit repeat request; it does not change the once-per-installation automatic setup record.

### Help and parameters

Summary: Prime one Dot or all discovered Dots. The exact plain reference is [command help](../help/codex-dots-prime.txt); its terminal form follows the shared structure above.

- `DOT` (optional): Stable Dot handle or exact unambiguous display name. Choose DOT or --all. Non-empty and not whitespace-only when supplied. Exactly one of DOT or --all must be selected.
- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.
- `--all`: Prime all discovered Dots instead of selecting DOT. Default: `false`. Cannot combine --all=true with DOT; --all=false permits DOT.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show `Priming` and every returned target/outcome. Display names are secondary to stable IDs. Keep `accepted`, `included_in_setup`, skipped and unknown outcomes distinct in the receipt. Empty targets say `Targets: None`. An accepted reminder does not prove its response completed.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots prime dot-handle` — Prime one Dot by stable handle.
- `cq codex dots prime --all` — Prime every discovered Dot.

## `cq codex dots start`

**Decision:** Did the configured Dots integration start, or does the app need user action?

**Effect:** Start the configured native or isolated integration using its persisted settings. Native startup uses the configured CQ proxy and preserves the normal app profile.

### Help and parameters

Summary: Start Dots in the configured native app. The exact plain reference is [command help](../help/codex-dots-start.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show `Started`, `Native catalogue`, restart/onboarding requirements and returned watcher/relay/profile observations. Show discovery or delivery details when present. `Started: No` plus a restart requirement must remain an incomplete outcome, never a green “started” title.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots start` — Start Dots in the configured native app.

## `cq codex dots status`

**Decision:** What does the installed adapter currently report about its running components and catalogue?

**Effect:** Inspect the existing installation and controller receipt. Do not turn a stored installation marker into runtime liveness.

### Help and parameters

Summary: Show Dots status. The exact plain reference is [command help](../help/codex-dots-status.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show only observed installation/start/catalogue/discovery/component facts and any profile count. Keep missing facts absent. A status receipt with pending restart or incomplete discovery retains those warnings; it does not imply a repair was attempted.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots status` — Show Dots status.

## `cq codex dots stop`

**Decision:** Did the adapter stop after the native app was quit?

**Effect:** Stop the adapter through the released lifecycle. The user must quit the native app first; the CLI does not add an implicit app quit or remove installation data.

### Help and parameters

Summary: Stop the Dots adapter after quitting the app. The exact plain reference is [command help](../help/codex-dots-stop.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show the returned `Stopped` observation and any remaining component or detail fields. If shutdown fails, preserve the partial receipt with an error. An absent stopped field cannot become `Stopped: Yes`.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots stop` — Stop the Dots adapter after quitting the app.

## `cq codex dots sync`

**Decision:** Was account discovery reconciled, and does the native app need a restart?

**Effect:** Request immediate account reconciliation. Account watching remains automatic. A changed native catalogue may require restarting through CQ; sync does not interrupt existing chats.

### Help and parameters

Summary: Refresh Dots account discovery. The exact plain reference is [command help](../help/codex-dots-sync.txt); its terminal form follows the shared structure above.

- `--root ABS_PATH`: Private Dots installation directory (default ~/.local/share/cq/dots). Accept before or after the leaf command. This is not a global CQ option. Must be a clean absolute path when supplied. Must not contain the home directory or overlap native app, credential or application-support paths.

All scalar options are non-repeatable. Global help/json/version behaviour applies. Boolean `=false` disables that option: it does not satisfy an exclusive true selector or forbid a positional on its own.

### Runtime output

Show catalogue/discovery/profile observations and any account outcome rows. Show `Restart required: Yes` when returned, alongside watcher/relay and setup delivery outcomes. Discovery incomplete or retryable is visible; a successful controller return does not claim every account was discovered.

### Failure, interruption and partial states

| Code | Exit | Exact diagnostic |
| --- | --- | --- |
| `dots_invalid_argument` | 2 | Invalid Dots argument: {detail}. |
| `dots_operation_failed` | 1 | The Dots operation failed; inspect cq codex dots status before retrying. |
| `dots_result_invalid` | 1 | The Dots operation returned an invalid receipt; inspect state before retrying. |
| `dots_result_missing` | 1 | The Dots operation returned no receipt; inspect state before retrying. |
| `dots_timeout` | 7 | The Dots operation timed out; inspect state before retrying. |
| `interrupted` | 130 | Operation interrupted; inspect state before retrying. |

`dots_incomplete` is a warning with exit 0 when the controller returned normally; retain the receipt and state what remains. Parser-invalid options or arity fail before execution. The error frame uses safe fixed diagnostics and never includes the message body, credentials or raw upstream replies.

### Usage examples

- `cq codex dots sync` — Refresh Dots account discovery.
