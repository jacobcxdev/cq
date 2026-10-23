# Hand-authored CQ output examples

These are proposed terminal outputs, not captures of the installed CLI. Each page pairs concrete examples with the [presentation specification](../README.md). Output bodies were written individually; the export script only copies those bodies into text files. Do not regenerate their content from the current renderer: they are independent design targets.

## Browse

[All operational outputs, without help pages](operational-outputs.md) — concrete outputs for every one of the 89 commands.

[Open the comparison catalogue](../preview.html) for separate Output, Help and Specification tabs. Each operational command opens on its first actual result, with a scenario selector for failures and other states. The initial page is the all-account `cq codex reset list` result. The colour toggle previews shared accent roles; text exports remain ANSI-free. Full completion scripts are expandable under `cq completion`.

- [Root and all command groups](groups.md)
- [Accounts, quotas, models and utilities](accounts.md)
- [Codex routing and reserves](routing.md)
- [Runtime and services](runtime.md)
- [Export index](index.json), with command, scenario, source location and SHA-256 for each text fixture

Each canonical command has its own help example and concrete operational examples. A representative failure demonstrates the command's diagnostic presentation; the specification still enumerates the full error contract. Additional examples cover meaningful empty, partial, confirmation, timeout and interruption states. Unavailable operations have no fabricated successful run. Equivalent help spellings and compatibility aliases share their canonical page rather than duplicating its body.

## Fixture conditions

- Fictional data only. `alex@example.test` and `sam@example.test` are illustrative identities, not discovered accounts. File paths under `/Users/alex` are illustrative local paths.
- Reference clock: **23 September 2026, 16:30 BST (UTC+01:00)**, equivalent to **2026-09-23T15:30:00Z**, unless a scenario explicitly states another clock. Relative dates are computed against that clock.
- Primary terminal width: **100 display cells**. Each explicitly narrow example declares its own width. A help metavariable such as `ACCOUNT` is legitimate syntax; operational output uses concrete values.
- Fixtures contain visible text without ANSI. Apply the shared palette to implementation output; a no-colour terminal must preserve these words and hierarchy. These are terminal-layout examples, not replacements for the frozen redirected/plain or JSON contracts.
- Invocation, stream labels, scenario explanation, user input and exit status sit outside output fences. They are test metadata and must not be printed by CQ. An explicitly empty stream emits zero bytes; absence of a fence must not produce the word `empty`.
- Every fenced output ends with one newline. Preserve intentional blank lines, indentation, punctuation and full identifiers. Comparison tooling may strip ANSI, but must not discard diagnostic codes, flatten columns, collapse whitespace or sort results to hide a difference.
- A scenario is independent unless labelled as a sequence. Matching fictional identities across scenarios do not imply that a previous mutation ran. No example authorises a real mutation, reset-credit consumption or service change.

## Use during implementation

1. Select the command and scenario. Construct its input/domain outcome using the stated fictional values; no live provider request is needed.
2. Freeze the scenario clock, timezone, terminal width and colour mode. Keep stdout and stderr separate.
3. Compare rendered visible text with the exported fixture. Test ANSI roles separately against the shared colour specification.
4. For other widths, implement the specified responsive layout and review its result; do not mistake browser wrapping or truncation for a correct terminal rendering.
5. Verify machine and redirected-output compatibility with their existing fixtures. The examples do not redefine those protocols.

Run `python3 specs/cli-v2/presentation/examples/export.py` to export the manually authored output blocks and refresh the index. Run the same command with `--check` to verify exports and command/help coverage without writing files. This proves fixture coverage and byte consistency, not runtime correctness.

Design changes should edit the authored Markdown first, then refresh exports. When an example exposes a specification conflict, correct both together and document the decision; never silently change semantics to make a screenshot prettier.
