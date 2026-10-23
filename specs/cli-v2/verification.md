# Specification verification

Verified 10 September 2026 against the source baseline recorded in [source-baseline.json](source-baseline.json). These checks validate this specification package; they do not claim that the existing CLI implements the proposed contract.

## Coverage

| Surface | Count |
| --- | ---: |
| Leaf command contracts | 89 |
| Intermediate groups | 37 |
| Exact help pages, including root | 127 |
| Local option declarations | 203 |
| Positional declarations | 19 |
| Distinct local parameter names | 65 |
| Shared global options | 3 |
| Command examples | 154 |
| Legacy source-inventory entries mapped | 92 / 92 |
| Frozen source snapshots with verified SHA-256 | 66 |

Four leaf contracts explicitly return unavailable until a separate qualification backend is specified: candidate start, client-safety refresh, release activate and release validate. Their reserved success notes must not be implemented as successful stubs.

Legacy coverage comprises 79 public entry points, one advertised hook, four public service paths with hidden machine options, six hidden machine paths and two separate installer paths. Migration assigns 20 unchanged canonical paths, 62 aliases, two explicit retirements and eight frozen machine interfaces. Extra historical spellings and group-help translations are documented in the migration notes.

## Checks performed

- `python3 specs/cli-v2/generate.py --check` passed: required catalogue fields, unique command/parameter names, parameter types/defaults, enums, per-token rationale, examples and acceptance cases.
- All 154 examples parsed as shell text without execution. CQ command/option names, required options, positional counts and literal enum choices checked. Prepared shell variables and file/account prerequisites remain explicit in resource workflows; no provider or account operation was executed.
- Every generated command reference, acceptance catalogue and help file matched the generator byte-for-byte.
- All 92 legacy inventory paths matched migration rows without duplication. Destination paths, source flag/positional coverage and conditional translations received a separate review.
- Every local Markdown link resolved. All 66 annex snapshot hashes matched manifests. Snapshots use `.go.txt` so documentation cannot become a Go package.
- Independent semantic reviews covered provider scope, account selectors, policy/pool/session vocabulary, aliases, JSON scope, terminal stream records, deadlines, periodic service health, candidate proof boundaries and environment/path resolution. Identified contradictions were resolved in the final contract.
- `git diff --check` passed for existing tracked changes. `go list ./...` confirmed the new specification introduces no Go packages.
- All ten pre-existing modified source/test files still match their recorded SHA-256 values. No CLI implementation, installed executable, service configuration, credential store or provider state changed.

No Go test suite was run for the specification-only addition. The generated acceptance catalogue states the required implementation tests, including the repository's race-enabled test policy. Mechanical checks establish coverage and consistency; they do not substitute for later implementation review or user experience testing.
