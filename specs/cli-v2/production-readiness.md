# CLI v2 production-readiness handoff

Updated 26 September 2026. Planned release: **0.34.0**. CLI v2 and JSON schema 2 describe the interface, not a 1.x product release.

## Current decision

The CLI implementation and terminal presentation are complete against the approved contracts. Local source verification is recorded below. This is a source-readiness handoff, not installed-product or release qualification.

The user explicitly prohibited deployment, release and replacement of the system installation. This work did not push, open a PR, tag, publish, dispatch CI, change a service or replace the installed executable. Keep `jacobcxdev/cli-v2` and its worktree for the next authorised step.

## Source and review

- Latest default branch fetched during final verification: `2a6f29b238971b281b8a68c8d8d63c1ad734dfa1`.
- Signed merge `e84be6c` incorporated all four newer default-branch commits, including reserved-turn rerouting and macOS graceful listener handover. Both v2 selected-service handling and the default-branch drain behaviour were retained.
- Independent whole-branch review covered default branch through `3b133c993e8aa962c7ca0a80e8360c5bb943b7bb`. It identified one important consumer defect and one minor wrapping defect. Both were reproduced by failing tests and fixed in signed `c2c71419a29269d39ea7ebc24003b7dee5d9abab`.
- The release validator now builds its candidate before taking read-only proxy snapshots. It uses that candidate's documented JSON interface; the installed release need not understand CLI v2. Hermetic tests forbid invocation of PATH-installed `cq` and verify cleanup after failed builds and invalid snapshots. The live validator itself was not run.
- Partial receipts for an existing proxy-state directory now wrap the whole heading together, including at 20 columns. Tests cover colour and no-colour at 20/40/80/120 columns.
- Signed `276e877` aligned an additional publication-boundary assertion with the shared 0.x candidate version. It changes tests only; executable source remains identical to `c2c7141`.

No unresolved consequential source defect remained from the review. Native service and live provider behaviour were outside this review's executed scope.

## Verified coverage

| Check | Result and limit |
|---|---|
| Canonical catalogue and generated help | PASS: 89 commands, 37 groups, root; 127 help pages; 154 usage examples |
| Plan ownership | PASS: 29 tasks; 92 legacy entries; 203 local options; 19 positionals; 3 global options. Ownership alone is not runtime proof. |
| Presentation catalogue | PASS: 522 authored output fixtures; exact renderer and handler comparisons in the race suite |
| Build and static analysis | `go build ./...` and `go vet ./...` PASS |
| Full race suite | `go test -race -count=1 ./...` PASS at `276e877`; all packages passed (proxy 122.422s, CLI entry point 64.670s) |
| Workflow validation | `actionlint` PASS for all three existing workflows |
| Six-platform build | PASS: Darwin/Linux/Windows, amd64/arm64; clean source provenance. Foreign targets compiled, not executed. |
| Native executable plain help | PASS: all 127 command paths; redirected `cq help --help` intentionally retains the root plain-help contract |
| Native executable syntax/JSON | PASS: invalid option on all 127 paths, schema 2, exit 2, no ANSI or stray stderr |
| Native executable terminal help | PASS: 1,143 cases, all 127 paths × 20/40/100 columns × colour/`NO_COLOR`/`TERM=dumb`; width and colour-free layout equality checked |
| Independent output streams | PASS: 12 terminal/pipe combinations for syntax diagnostics and JSON version output |
| Raw completion output | PASS: Bash/Zsh/Fish bytes unchanged across colour modes; 9 terminal comparisons |
| Native shell insertion | PASS: 24 Tab/Enter cases across stock Bash, PATH Bash and Zsh; Fish context and all three shell syntax checks also passed without skips |

Executable smoke tests used no HOME and deliberately invalid relative XDG roots, confirming pure commands do not require user configuration. No quota fetch, credential access or live service operation ran. The PTY harness disabled output newline processing before comparing bytes; an initial harness-only CR difference was resolved without a product change.

The first final race run exposed a stale `1.0.0` workflow assertion. It was corrected in `276e877` before the final rerun; no failed run is represented as passing evidence.

Earlier dark/light visual evidence remains at `.superpowers/sdd/2026-09-23-terminal-presentation/visual-review/report.md`: eight representative operational cases at 40/100 columns in both themes. Those are renderer captures from `a9a8d83`, not installed output. The later narrow partial-state fix is covered by the new width regression.

## Development artefacts

Local evidence: `.superpowers/sdd/2026-09-23-terminal-presentation/readiness-20260926/` contains build configuration, reproducible local build/smoke scripts, `build-manifest.json`, `executable-smoke.json` and six binaries. Version: `0.34.0-dev`. Source: `c2c71419a29269d39ea7ebc24003b7dee5d9abab`; every target records `vcs.modified=false`.

**These are development builds.** Gemini release OAuth link data was deliberately absent. Do not install or publish them as provider-complete candidates. Use the existing release build's legitimate secret injection for a future candidate; no secret extraction or runtime fallback was added.

| Target | Executable SHA-256 |
|---|---|
| darwin/amd64 | `b492d75ce5981e06f7de82cda0e6d1ca612da0fe7db1df7e706e8ac6c94bc90a` |
| darwin/arm64 | `764b87eb2732391464c15056782293b52959706c44a2555db6c27531ebc46af5` |
| linux/amd64 | `0717b01cce7589f7b54dea1b04019459bb6c52952ce8457fe4148824a24c9ead` |
| linux/arm64 | `2ec0e50f56ce943dba04b49eed1e3d15165d509d442505fe8672131443efa0fb` |
| windows/amd64 | `e692a40aa9c02d5a6c6222e73d52112605af498bae76f5141cdf7f5e857dda84` |
| windows/arm64 | `63cb892428ba6800261f755a7ed33323b5e4e3c209413ca1fcdc5d2d9f568d52` |

## Deliberate contract limits

Four candidate transitions remain explicitly unavailable, as specified: `proxy candidate start`, `proxy candidate client-safety refresh`, `proxy candidate release activate` and `proxy candidate release validate`. They return exit 4 with `candidate_evidence_unavailable` and perform no transition. The implementation does not invent qualification evidence or report unsupported transitions as successful.

Historical CU-0/CU-1 signed rosters remain historical. `scripts/verify-proxy-cu --cli-v2` is a distinct compatibility roster, not a substitute for historical CU acceptance. Its selected tests are included in the full race suite; this handoff claims no new historical qualification.

## Gates for a separately authorised release

1. Build the selected source with legitimate Gemini release-time configuration and bind all artefacts to the exact source/version. Do not reuse the incomplete development binaries above.
2. Run supported native package install/upgrade/uninstall/rollback checks on macOS, Linux and Windows. Cross-compilation does not satisfy these gates.
3. Run real provider and proxy qualification, including normal/rescue/handoff traffic, HTTP 200 and WebSocket 101, with no continuity mismatch or unexpected 503. Health endpoints and synthetic fixtures do not establish this.
4. Inspect the resulting artefact identities, CI results and live evidence before publication or replacement of an installed executable.

None of these unperformed gates is implied by the local PASS results. No disposable deployment was created. The current installation was left untouched.

[Earlier release evidence](release-evidence.md) is a historical archive. Its old version, installation state and authorisations do not describe or authorise this handoff.
