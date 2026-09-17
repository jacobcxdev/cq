# Parser acceptance fixtures

- `parser-cases.json` contains concrete argv, result fields and error expectations for canonical grammar and every leaf parameter. Size boundaries that would obscure the fixture file live in `values_test.go`.
- `parser-coverage.json` records every leaf validation/precondition sentence from `commands.json` verbatim. `parser` entries reference executable cases; `semantic` entries name the exact command owner in `specs/cli-v2/plan/coverage.json`; `mixed` entries require both. Family ownership is a deferred responsibility, not a claim that the family adapter is implemented.
- `compatibility.json` covers all 92 `migration.json` rows, their flag/positional mappings, conditional variants, retirements and public rejection of machine-only forms. Canonical/alias pairs compare the full invocation, apart from warnings.

The fixtures contain synthetic inputs only. `Parse` does not open these paths, resolve account references, read stdin/environment or call providers/services. Changing a catalogue clause or migration row requires reviewing its fixtures and ownership; the coverage checks fail on missing or stale entries.
