# CLI v2 implementation baseline

## Identity

| Item | Recorded value |
| --- | --- |
| Implementation worktree | `/Users/jacob/Developer/src/github/jacobcxdev/cq/.worktrees/cli-v2` |
| Branch | `jacobcxdev/cli-v2` |
| Release tag object | `debcb2cb19b61fd0cb2983554299d0edaaa052c5` |
| Dereferenced release commit | `0a83339aafe6f0a23ba2a616cbc5805a523c2035` |
| Release tag | `v0.32.5` |
| Source checkout recorded by the inventory | `dc976e6591d2b729185a80b974f55300dea255c2` on `jacobcxdev/fix/codex-reset-credits` |
| Go toolchain | `go1.27.1 darwin/arm64` |
| Review date | `2026-09-10` |

`debcb2cb19b61fd0cb2983554299d0edaaa052c5` is an annotated tag object whose object is `0a83339aafe6f0a23ba2a616cbc5805a523c2035`. The implementation worktree starts from that commit. It was clean apart from the copied, previously untracked `specs/cli-v2/` package before this baseline document was added.

The original checkout at `/Users/jacob/Developer/src/github/jacobcxdev/cq` was read only. No source or test diff from it was copied into this worktree.

## Release checks

The following checks were repeated against the implementation worktree:

```text
git rev-parse HEAD
0a83339aafe6f0a23ba2a616cbc5805a523c2035

git cat-file -e debcb2cb19b61fd0cb2983554299d0edaaa052c5^{commit}
PASS

python3 specs/cli-v2/generate.py --check
Validated 126 command/group entries, 154 examples and 127 exact help pages.
```

The controller also ran the following at the same release commit before this documentation-only task: `go build ./...`, `go vet ./...`, and `go test -race -count=1 ./...`. All passed; the race run included `internal/proxy` in 127.015 seconds and `internal/tools/proxycu` in 163.857 seconds. No tracked Go files changed in this task, so those full gates are retained controller evidence rather than needlessly repeated.

## Preserved original inputs

The following SHA-256 checks were repeated against the original checkout. Every input matched `source-baseline.json`.

| Original dirty path | SHA-256 |
| --- | --- |
| `cmd/cq/codex_registry_credentials_test.go` | `61b6bce1479995bbb350afef4ad7f3c93c3ee5c8e3b8a7553f42a51ba455b236` |
| `cmd/cq/models.go` | `ba59c64511a6cadc7ef687b53b706100ccd529ef672ddf2d4060b16e0d5bf04d` |
| `cmd/cq/models_test.go` | `f8dda724bedd61980bb898b05a1e5876d0b78c1c01ef42966734da97a93a0987` |
| `internal/provider/codex/accounts.go` | `cf12aab9c33a289c849a21722975bb1fb8c5a160eeeca47568d97b4c0cd87e42` |
| `internal/provider/codex/accounts_test.go` | `b65dd2e6bdab5193bff23b351f52df0405123f50d845cc19fec30082869ccfc2` |
| `internal/provider/codex/reset_accounts.go` | `d54c092eb267a5a84111e9fc3cc462a82784ab388b632e8e404fe5f77934b3d0` |
| `internal/provider/codex/reset_accounts_test.go` | `505dba3de370366a9b8b9b2a414d5ac5ce121e86443bbd8eea9228406ec07758` |
| `internal/provider/codex/reset_credits.go` | `640639898eb3486bf1a7e44fca1ae54461aabc997925a0bbe3fcff9ec59c898b` |
| `internal/provider/codex/reset_credits_test.go` | `80d8297ca1cb8c0d49db9daa4b26df493c550da5a742ddabb279d9a2a4f73558` |
| `internal/provider/codex/system_activator.go` | `8afa694739d5819ab056bbe8c6d4b42f1dc14fd7827ed6e635ac0e840e46c736` |

## Semantic comparison and retained regressions

The comparison used the ten preserved dirty diffs and the released implementations. It records behaviour only; it does not import an original patch. T07–T12 must express the applicable cases through the CLI v2 adapters and their owned tests.

| Original path | Semantic result | Classification and owner |
| --- | --- | --- |
| `cmd/cq/codex_registry_credentials_test.go` | Keeps the stale managed credential valid long enough to exercise a `401` fallback to a fresh external credential, instead of letting token-expiry selection bypass that route. | Already present in v0.32.5 production routing; the changed test is independent original-checkout verification. Retain it there. |
| `cmd/cq/models.go` | Is byte-identical to v0.32.5; the aligned model, route and discovered-from table is already released. | Released T11 presentation to preserve. No dirty patch or outstanding regression. |
| `cmd/cq/models_test.go` | Is byte-identical to v0.32.5; its aligned-column and provenance cases already protect the released presentation. | Released T11 tests to preserve. No dirty patch or outstanding regression. |
| `internal/provider/codex/accounts.go` | Diverges from v0.32.5. Released `accountAccessExpiresAt` uses an access-token JWT expiry whenever it is available, falls back to `cq_expires_at` only for an opaque or expiry-less access token, and otherwise returns unknown. The dirty helper takes the later value instead. | Reject the dirty metadata-extension rule. T07 must preserve the released inspection rule, and T08 must use the same rule for activation candidates: an expired access-token JWT remains unavailable even if CQ metadata is later. |
| `internal/provider/codex/accounts_test.go` | Removes the released opaque-token fallback and access-token-precedence coverage, then adds narrower access-token-only fixtures. In particular, v0.32.5 `TestAccountParsingPrefersAccessTokenExpiryOverCQExpiry` rejects a later CQ metadata value as an override. | Released T07 contract tests to retain or reproduce in the v2 adapter. Do not carry the dirty test simplification forward. |
| `internal/provider/codex/reset_accounts.go` | Preserves all resolved credential candidates, tries each on authentication failure, and only refreshes eligible managed candidates after the existing candidates fail. | Required reset inventory/advisory regression for T09 and exact-consumption regression for T10. Preserve candidate affinity and request identity. |
| `internal/provider/codex/reset_accounts_test.go` | Covers list and consume fallback, stable redemption request IDs, and resolver failure before an HTTP call. | Required regression tests for T09 and T10. |
| `internal/provider/codex/reset_credits.go` | v0.32.5 already accepts additive upstream list and consume metadata while retaining top-level, trailing-value, malformed-entry and available-count checks. The dirty change adds only presence validation for `reset_type` and `status`, distinguishing absent/null fields from empty or unknown values. | Required T09 input-validation regression. T10 preserves the released additive consume decoding and does not treat it as new work. |
| `internal/provider/codex/reset_credits_test.go` | v0.32.5 already has additive list and consume metadata tests. The dirty-only cases cover absent and null required credit fields. | Retain the released additive-response tests; add the dirty-only missing-required-field regression under T09. |
| `internal/provider/codex/system_activator.go` | Replaces the released `accountAccessExpiresAt` parsing rule with the dirty later-metadata helper for activation candidates. | Reject the dirty metadata-extension rule. T08 must preserve the released access-token precedence and opaque-token-only metadata fallback. |

The released source already contains candidate `__runtime`, runtime-role descriptors, legacy-managed migration, Codex Stop hook, account/reset/model/policy/candidate/endpoint/operation families, and non-live `validate-http --port`. It also differs from the older reviewed checkout by removing ordinary-command implicit `ensureAgent` calls after completion. Therefore the older uninstall-resurrection observation is not an unfixed v0.32.5 requirement and must not be carried into CLI v2 work as one.

## Handoff boundary

T07 owns read-only account inspection and opaque account references. T08 owns login, activation and removal transactions. T09 owns reset list/recommend data and advisory outcomes. T10 owns consented, idempotent reset use. T11 owns model-list and overlay publication outcomes. T12 owns explicit provider refresh. The preserved diffs are evidence for those tasks only; their source changes must be reimplemented against the canonical command, resource and acceptance contracts rather than applied wholesale.
