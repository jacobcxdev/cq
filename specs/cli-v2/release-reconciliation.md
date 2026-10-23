# CLI release reconciliation — 23 October 2026

This handoff covers the complete CLI v2 and terminal presentation branch, reconciled with default-branch changes through `8938d058d7ddd2bbbbfa8a26a021eeba7728f410` (`v0.38.8`). The previous CLI branch head was `6fa7443caf000574142996dcd128b68a5e0aeed1`; its latest incorporated default-branch baseline was `2a6f29b`. The reconciliation includes all 34 intervening default-branch commits, including fixes not listed separately in release notes.

The next feature candidate is `0.39.0`. This is a build identity for future qualification, not a published release. No deployment, release, native service change or system installation replacement is part of this work.

## Released changes covered

GitHub's published-release inventory was checked on 23 October 2026. Releases newer than the September handoff are `v0.33.7`, `v0.33.8`, `v0.33.9`, `v0.33.10`, `v0.33.11`, `v0.34.2`, `v0.35.0`, `v0.35.1`, `v0.35.2`, `v0.35.3`, `v0.36.0`, `v0.37.0`, and `v0.38.0` through `v0.38.8`. Draft releases are not counted as published. Earlier releases were already incorporated in the September branch.

| Released capability or fix | Reconciliation |
|---|---|
| Multi-account Dots and subsequent native identity, streams, connections, launch and update-feed fixes | Retained the released Go controller and private helpers. Added all nine public commands beneath `codex dots`, with inherited `--root`, canonical JSON receipts, help, completion, semantic colour and authored success/failure examples. |
| Safe quota redistribution and expired lease recovery | Added `codex proxy lease redistribute` and the released `proxy leases redistribute` compatibility spelling. Existing continuity and safe-boundary rules remain authoritative. |
| Successful reset notification | Canonical reset execution schedules redistribution, preserves confirmed reset outcomes when notification fails, and respects cancellation during post-reset work. |
| Homebrew runtime handover, drain, recovery and ownership | Retained installer upgrade, snapshots, private runtime protocols and retained executable verification. Canonical service status distinguishes configured and live executables and reports upgrade versions/phase. |
| Missing proxy recovery | Canonical rescue entry can recover a refused default listener. Explicit-port requests cannot restart an unrelated service; uncertain responses are not replayed. |
| Owner-managed credential renewal and deadline fixes | Enabled released CodexBar renewal capability for the canonical standalone credential owner after ownership assertion. Inventory reads and other external credential sources retain their read-only behaviour. |
| Model discovery after app upgrades | Retained dynamic client-build discovery, refresh reconciliation, typed publication receipts and cache metadata. Local canonical refresh must use the current routing client build, not stale model-cache metadata. |
| HTTP/WebSocket routing, starvation and oversized retry fixes | Retained incoming runtime, persistence gate, quota replan and transport code. CLI adapters preserve their updated control receipts. |
| Gemini quota endpoint and missing-quota reporting | Retained released production Antigravity host, parser and provider changes. Release-time OAuth secret injection remains unchanged. |

`service upgrade` remains public, with help, completion and explicit parameter documentation. Its operational output retains the released raw receipt protocol through both canonical and frozen package invocations. `proxy runtime-check --json`, runtime handover arguments and `__dots` remain private interfaces documented in [the machine ABI contract](internal-abi.md). Public Dots commands use the canonical parser; they do not bypass it through the private helper entry.

## Public contract and review scope

The integrated catalogue contains **100 commands, 38 groups and 139 help pages**, with **568 authored output fixtures**. The new commands are `codex dots install`, `start`, `stop`, `status`, `sync`, `list`, `prime`, `broadcast`, `board`, `codex proxy lease redistribute`, and the retained public `service upgrade` operation.

Every command is registered with one handler family and one implementation owner. Catalogue, parser-clause, presentation, terminal-fixture and README-index checks verify these sets agree. Examples remain independently authored expectations; generated completion scripts are explicitly identified as protocol fixtures.

Review includes source changes required to preserve released behaviour through the canonical adapters, not only textual conflict resolution. Regression checks cover owner renewal, reset notification and cancellation, rescue recovery, retained runtime inspection, current model-version selection, private ABI classification and inherited Dots arguments.

## Verification record

Production source was verified at `c8c84e413d66d7334ee4e96310731ea642a1fe15`. The final follow-up only strengthens dispatch and fixture tests; its focused race tests also passed. The build identity is `0.39.0-dev` and the development builds omit release-time Gemini OAuth link data.

| Check | Result |
|---|---|
| `go build ./...` and `go vet ./...` | Passed. |
| `go test -race -count=1 ./...` | All 27 test packages passed, including provider, runtime, installer, Dots, terminal and CLI contract checks. |
| Final service-upgrade dispatch and fixture regressions | Passed with `-race`: public/package parity across 30 phase/argument combinations, preparation/output failures, cancelled contexts and five terminal widths. |
| Catalogue generation, plan coverage, presentation coverage and verbatim export checks | Passed: 100 commands, 38 groups, 94 legacy entries, 224 local options, 22 positionals, 139 help pages and 568 output fixtures. |
| Cross-compilation | Passed for Darwin, Linux and Windows, each on amd64 and arm64, with `CGO_ENABLED=0`. Foreign builds were compiled, not executed. |
| Native executable plain help | All 139 pages matched exact canonical text. |
| Native executable invalid JSON syntax | All 139 paths returned schema 2 diagnostics, exit 2, no ANSI and empty stderr. |
| Native executable terminal help | 556 cases passed: every page at 20 and 100 columns, with colour and `NO_COLOR`; stripped colour output matched plain terminal content. Renderer tests separately enforce width behaviour. |
| Workflow validation | PR workflow passed full `actionlint`; all three workflows passed `actionlint -shellcheck= -pyflakes=`. |
| Branch reconciliation | `origin/main` remained `8938d058d7ddd2bbbbfa8a26a021eeba7728f410` at the final fetch; it is an ancestor of the verified source. |

Executable smoke checks removed `HOME` and deliberately supplied invalid relative XDG/Codex state locations. They exercised only pure help and invalid syntax, without provider credentials or live state. Synthetic lifecycle tests used fake service managers and temporary stores.

Development binary digests:

| Target | SHA-256 |
|---|---|
| darwin/amd64 | `ff17955a8a31031eb9758955048032b7efde50c450bc0ea5884ee2741b89f7f1` |
| darwin/arm64 | `e80f65216d7d611421cfe7b731de858fa4e01c28252aa79a8011835b44ead2d4` |
| linux/amd64 | `0fd340b04522559d26ba33023337c527255224daa38ed0ea820370cf29378e34` |
| linux/arm64 | `757c9631902bfa5c183ba2a299f4d4ac4a96ca2f034b7899801b1dcd464e6d37` |
| windows/amd64 | `7d2faa9255c863760e45371e2d4023e79a71cd3e2482ce4b3364c149fe3d527a` |
| windows/arm64 | `16df1fa5a33d306ea2c4683226de14d61ed69324438f0d693eeaf3b7801c0baa` |

These binaries are local verification artefacts, not signed or qualified release packages. Native package lifecycle, real provider traffic and installed-client acceptance remain release gates and were not run. No release workflow, tag, system installation replacement or live service operation was performed.

The [September readiness report](production-readiness.md) remains historical evidence. Its source hashes, binary hashes, platform results and counts do not prove this reconciled revision. No native package lifecycle or live provider acceptance is claimed by local fake-service and synthetic HTTP tests.

The four previously reserved candidate transitions remain explicitly unavailable with exit 4: candidate start, client-safety refresh, release activation and release validation. This reconciliation does not manufacture release qualification evidence or expand those capabilities.
