# Semantic colour contract

Colour is a scanning aid with stable meanings across every command. This contract refines the shared visual vocabulary and applies to operational outputs as well as help. The viewer annotates the hand-authored fixtures; this is a design proposal, not a change to the installed CLI.

## Roles

| Role | Terminal treatment | Apply to | Do not imply |
|---|---|---|---|
| Structure | Bold default foreground | Resource titles, section headings, column headings | Success or urgency |
| Identity and syntax | Cyan, ANSI 36 | Account email/reference, credit/model/session IDs, paths, commands, options, diagnostic codes, selected configuration values | Authentication, eligibility or health |
| Quantity | Bold blue, ANSI 1;34 | Counts, remaining percentages, thresholds, credit balances, durations expressed as measured quantities in comparison tables | Good/bad status based on size alone |
| Time | Magenta, ANSI 35 | Dates, reset/expiry deadlines, relative ages, elapsed time and periods in labelled temporal fields | Imminent failure just because a date is near |
| Confirmed outcome | Green, ANSI 32 | Confirmed writes, completed operations, verified health, complete observations, explicitly available credits | An active process is healthy, or a saved setting is applied live |
| Attention | Yellow, ANSI 33 | Partial, stale, pending, indeterminate or unknown state; explicit domain warnings | Every unavailable or empty collection is a failed command |
| Failure | Red, ANSI 31 | Failed/rejected/conflicted outcomes and the error marker | Zero values, exhausted quota, disabled configuration or normal shutdown are errors |
| Supporting text | Normal or faint default foreground | Field labels, units when separate, separators, known absence, explanatory detail | Important evidence can be hidden through low contrast |

The ANSI palette is user-controlled. HTML swatches demonstrate one dark palette; implementations must not copy those RGB values into terminal output. No background fills, rainbow rows or provider-specific palettes. Emphasise the value or status token rather than colouring its entire paragraph.

## Precedence and exact assignment

Assign roles from typed fields and domain outcomes, never by searching user-supplied text for words such as “failed” or “healthy”. An account called `failed@example.test` remains cyan identity text. Codes are cyan identifiers even when they describe a red failure.

A domain warning or failure overrides a value's normal quantity/time colour only when the domain explicitly establishes that condition. The accompanying status word or warning must state it. Otherwise, percentages remain blue regardless of their numeric value, and deadlines remain magenta regardless of proximity. Do not invent generic low-quota or expiring-soon cutoffs.

`unknown` is amber. `none`, a known not-applicable dash and an absent optional setting are supporting text. Enabled/disabled, yes/no and active/inactive are neutral cyan configuration/state values, not green/red health signals. Counts retain their blue role at zero. An available credit may have a green status word beside a blue count; an expired credit may carry an amber `expired` status beside its magenta expiry date without making a successful inventory read red.

In headlines, keep the resource name bold default and colour each outcome phrase independently. For example, `Codex reset · Completed · Verification incomplete` uses green for Completed and amber for Verification incomplete. Never paint the entire headline green because one part succeeded.

Tables use the same roles as labelled records. Colour all cells in a semantic column consistently; do not colour alternate rows. IDs that happen to contain digits remain cyan, not blue. PID, port, generation and schema values are identifiers: exact digits, no quantity grouping. Model version numbers are identity, not quantities. Ordinary prose remains default foreground.

## Concrete application

For `cq codex reset list`:

- `Codex reset credits` is bold; `Complete` is green only when every requested inventory is complete.
- Account emails, references, account IDs and credit IDs are cyan.
- `1 credit` and `0 credits` are blue; zero does not become red.
- `available` is green because the returned credit state establishes it; `Supported: Yes` is neutral cyan.
- Granted and expiry values, including relative and absolute forms, are magenta.
- An unreadable inventory shows amber `unknown` and retains its diagnostic code and explanation. The aggregate Incomplete marker is amber.
- Field labels are supporting text. The full result stays understandable with colour removed.

For reserve, policy and account commands, colour configured identifiers cyan and numerical settings blue. Keep configured-versus-live acknowledgement text visible. Reserve exclusion is a routing decision, not a failed `status` command. For quota dashboards, retain the established domain gauge colours and meanings; generic quantity colouring must not replace the gauge's evaluated risk.

For services and validation, distinguish neutral active/running state from explicitly verified healthy/passed state. In partial mutations, independently colour committed results green and unresolved work amber. Error markers are red; the human explanation remains default foreground for readability.

Help uses bold structure, cyan command paths/flags/metavariables, blue numeric defaults and magenta temporal values where semantically identified. Help does not use outcome colours to decorate ordinary examples. Descriptions stay readable default text.

## Plain output and verification

NO_COLOR, dumb terminals and redirected streams preserve existing output contracts. Raw completion scripts, JSON/JSONL, hook payloads and frozen human trace records receive no semantic colour at all. The same plain fixture remains the text target: styling must not add, remove or reorder characters.

Verify role assignment separately from text snapshots. Include zero counts, unknown values, disabled settings, IDs containing numbers, hostile labels containing status words, mixed completed/incomplete outcomes and domain warnings. Test colour removal preserves the complete visible text. Browser preview styling is illustrative; implementation must use the typed role assignments above, not the preview's label matching.
