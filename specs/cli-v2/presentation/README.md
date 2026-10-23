# CQ terminal presentation design

Status: draft for review. This document changes human presentation, not command semantics. Operational designs below are not implemented. The help prototype and help-discovery fix are separate local changes; the installed binary remains unchanged.

## Intent and scope

A person should identify the result, compare relevant values, and find the next command without decoding machine fields. Every public command and group receives a concrete design. Success, empty, unavailable, partial, failure, timeout, interruption and confirmation are distinct where the underlying command supports them. Never manufacture a state merely to fill a design matrix.

Canonical grammar, option defaults, resource schemas, exit codes, authority boundaries and mutation receipts remain owned by `../commands.json`, `../resources/` and current adapters. Later reviewed corrections take precedence over stale prose: absent unconfigured providers do not fail an otherwise successful default quota check. Existing quota calculations and the weighted aggregate gauge are retained.

## Documents

- [Navigation and every group](groups.md): root and all 37 groups, immediate children, related commands and help states.
- [Accounts, quotas, models and utilities](accounts.md): individual leaf designs.
- [Codex routing and reserve](routing.md): individual leaf designs.
- [Runtime, services and lifecycle](runtime.md): individual leaf designs.
- [Operational outputs only](examples/operational-outputs.md): all 89 commands, with concrete success and failure scenarios; no help pages.
- [Hand-authored examples](examples/README.md): output, help and specification comparison, with verbatim text fixtures.
- [Coverage](coverage.json): exact ownership for all 127 help surfaces and 89 operational leaves.

Each command's `--help`, `-h` and `cq help PATH` share the same help presentation. Bare groups show help. Bare `cq` remains the quota dashboard, not root help. Compatibility aliases resolve to the canonical presentation with one existing deprecation warning; they do not create a parallel interface. Frozen machine invocations retain their exact existing byte protocols.

## Visual vocabulary

[Semantic colour contract](colour.md) defines precise role assignments, precedence and command-specific examples. Cyan identifies syntax and identity; bold blue marks quantities; magenta marks time. Outcome colours remain green for confirmed results, amber for attention and red for failure. These refined roles apply to every chapter below.

Use terminal palette colours, not hard-coded background assumptions: cyan (ANSI 6) identifies command syntax, selectable references and primary accents; bold identifies section headings and key results; default foreground is body text. Faint text is secondary explanation, never the only indication of failure or a critical deadline. Green (ANSI 2) marks confirmed success/health; yellow (ANSI 3) marks partial, stale, pending or indeterminate results; red (ANSI 1) marks failure. An exhausted quota is a valid observation and must not automatically imply command failure.

Every status has a word. Colour and glyphs are redundant cues. Default operational output does not depend on Nerd Fonts. Existing quota-renderer font fallback remains intact. Avoid panels, heavy borders, full-width backgrounds and ASCII logos. Use one blank line between sections and two-space content indentation. No decorative spinner for commands expected to finish immediately. Long operations may show a single transient phase line only on a terminal; final receipts replace it without losing errors.

Title: bold human resource name followed by a concise status phrase when meaningful. Next line: scope/account/provider, if needed to interpret the result. Then primary values or a comparison table. Secondary evidence follows. Warnings and recovery instructions come last except partial/unknown status, which also appears beside the title.

## Layout and terminals

Maximum body width is 100 display cells, constrained by actual stdout width. Calculate widths using visible graphemes, not byte length; ANSI escapes count as zero. Minimum supported test width is 20 columns. At widths where a table's labels and required values cannot fit, switch each row to a labelled record; do not horizontally clip identifiers or paths. Wrap prose at words; hard-wrap indivisible long identifiers only when unavoidable, with the intact value retained in plain/JSON output. Copyable recovery commands occupy their own lines and must not receive inserted prompt characters.

Do not silently drop fields at narrow widths. Remove optional bars, then use stacked labels, then wrap. Repeat column meaning as labels in stacked records. Preserve canonical ordering; presentation must not reorder command results whose schema defines an order. Where active-first quota order already exists, retain it.

Colour requires the destination stream to be a terminal, `TERM` not `dumb`, and nonempty `NO_COLOR` absent. Stdout and stderr are evaluated separately. `NO_COLOR` disables ANSI colour but retains layout and status words. Redirected output has no ANSI, cursor movement or transient progress. During initial rollout, existing plain human reference output remains byte-compatible; JSON and JSONL remain exact existing machine contracts. A future compact redirected format would need a separately reviewed contract change.

## Numbers, units and identifiers

- Counts: base-10 integers, right-aligned in comparison columns. Human quantities use comma grouping at 1,000 and above. Do not group PIDs, ports, credit IDs, revisions, generation identifiers, account keys or exit codes.
- Percentages: include `%` in values unless a single column header unambiguously declares it. Use integer percentages when supplied as integers. For fractional inputs use at most one decimal, except show `<0.1%` for a positive smaller value and `>99.9%` for a non-full value that would round to 100%. Configured thresholds retain their exact decimal setting (for example `2.05%`); do not round a value the user may need to reproduce. Exact machine values remain unchanged. Threshold decisions always use domain values, never display rounding.
- Changes in percentages: `+4pp` / `−4pp`, not `+4%`. A signed change is not automatically good or bad; colour follows its domain meaning.
- Capacity: display the existing plan-weighted summary, e.g. `4 × pro 20x = 80x`. Do not infer capacity from account count alone.
- Durations: below one second use integer milliseconds; below one minute use seconds; longer intervals use two largest nonzero units (`2h 14m`, `2d 19h`). Round remaining durations up at the final displayed unit so an upcoming deadline is not shown as elapsed. Elapsed ages round down. Zero means `now` only for an actual deadline at or before the reference instant; unknown is never `now`.
- Unknown data: `unknown` where uncertainty matters. A dash means not applicable and is explained by its column/context. An empty collection is “No …”, not an unknown observation. Unlimited requires an explicit domain state; never infer it from a missing limit.
- Bars: supplementary depiction of a bounded percentage only; clamp drawing to bounds but never clamp the reported numeric value silently. Do not create bars for credits, capacity multipliers or unbounded counts. Preserve the existing aggregate gauge and its domain legend.

## Dates and freshness

Capture one display reference instant per command. Human absolute dates use `23 Sep 2026, 16:30 BST (UTC+01:00)` in the local timezone; retain year. Include seconds for observation, event and operation timestamps when useful for ordering; retain fractional precision for audit-sensitive events rather than silently conflating them. Deadline rows show relative and absolute time together. Narrow layouts put absolute time on a second line. Do not use ambiguous numeric dates.

Examples: `in 2h 15m · 23 Sep 2026, 18:45 BST (UTC+01:00)`; `4m ago · 23 Sep 2026, 16:26 BST (UTC+01:00)`. For local abbreviations that are unavailable or ambiguous, the numeric UTC offset remains authoritative. DST offsets are calculated for the displayed timestamp, not copied from current time. Sorting uses the original instant.

A cached result shows `Cached · observed …`; a stale fallback shows `Stale · refresh failed` in amber with observation time. Do not label cached data fresh because rendering happened recently. Relative times are display values, never new scheduling inputs. No added network requests for presentation.

## Help contract

Order: title, one-line summary, usage, essential context, immediate children (groups), arguments, options, global options, related commands, examples. Root children are grouped as Quotas / Providers / Operations / Utilities. Each child appears once and no group lists itself. Leaf help retains all meaningful validation, defaults, repeatability, conflicts, omission behaviour and mutation effects. Omit redundant schema phrases such as `Required: false`; show required arguments in usage and label required options explicitly. Global flags get concise descriptions, with boolean syntax explained once where needed, not repeated under each flag.

Examples include purpose, exact canonical command and required placeholders. They must not imply real reset consumption, destructive confirmation or unavailable candidate transitions are routine demonstrations. Related links name full commands; `cq proxy --help` must directly reveal `cq codex proxy reserve`, policy/routing controls, Claude pins and service lifecycle.

## Errors, partial receipts and confirmation

One stderr diagnostic block per command outcome, headed `Error · <brief result>` or `Warning · <brief result>`. Preserve each exact diagnostic code in secondary text for support, with the canonical command path. Message wording must retain the contract's meaning; command-specific drafts retain existing messages until a wording change is separately approved. Never display raw upstream errors, secrets or control characters.

Known successful data remains on stdout. Partial mutation output states what committed, what failed, and whether final state is unknown. A failed command is not evidence of rollback. `restored` requires its explicit receipt; `failed` rollback is red; `not_needed` is neutral and may appear in a secondary outcome field. Timeout and interruption show any known committed result before advice to inspect state. Never recommend repeating reset consumption with a new credit after an uncertain result.

Confirmation shows action, target, scope, effects and relevant current values before the existing prompt. Keep existing consent grammar and non-interactive `--yes` requirements. A declined operation is `Cancelled · no change requested`, not a green mutation success. Do not claim “nothing changed” when interruption or external effects make that unknown.

Recovery suggestions must be derived from known state and existing canonical commands. Do not invent an account reference, port, file or reset credit. Render missing inputs as explicit placeholders and label them. No automatic recovery actions are introduced by this design.

## Streaming and raw exceptions

Completion scripts remain raw, executable shell text. JSON, JSONL, hook payloads, installer protocols and other frozen ABIs receive no colour, banners or prose. Human trace follows its declared stream boundaries; no final success footer after broken output. Foreground server output distinguishes ready, stopping and stopped; interruption exit semantics remain unchanged. Help for these commands still uses the normal human help design.

## Verification and review

For each individual entry, prove help, success and every reachable declared error; include empty, no-op, partial, deadline and interrupted states where applicable. Validate at 20, 40, 80 and 120 columns, colour and NO_COLOR, stdout/stderr redirection, Unicode and hostile labels. Use one frozen clock and timezone fixtures including DST and year boundaries. Test null versus zero, fractional percentages near 0/100, long IDs, negative durations and partial receipts. Existing machine snapshots must remain byte-identical. Do not mutate credentials, consume resets or deploy merely to generate screenshots.

Design review is separate from implementation. Coverage proves every command has an entry, not that every design is semantically correct or implemented. No public grammar additions are proposed here.
