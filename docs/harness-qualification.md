# Harness qualification contract

CQ curation is qualified against real native Codex app-server instances before
release. Each instance uses an empty private home, synthetic skills and plugins,
a loopback model provider and a macOS sandbox denying external network access
and writes outside the fixture. Qualification does not change the installed app,
live chats, account credentials or provider quota.

## Required outcomes

A saved binding requests application. A matching receipt confirms what was
actually applied. `pending`, `partial`, `failed` and an absent receipt are
distinct from `applied`. Unknown runtime metadata, unsupported controls, missing
dependencies or a lost acknowledgement must never become proof of application.

An unchanged inventory refresh preserves the existing application state during
an active turn. A changed compiled plan holds automatic continuation until the
current turn finishes and the configuration is applied, even when the saved
policy fingerprint has not changed.

Profiles preserve chat identity, conversation history, native core tools, the
available curation entrypoint, unselected chats and explicit user choices.
Restoring Full or disabling policy restores the recorded native baseline rather
than enabling capabilities that the user had disabled globally.

The conformance families below are required together. An ordinary race test run
does not execute the opt-in native fixtures.

| Family | Required happy path | Required failure and edge paths | Existing test authority |
| --- | --- | --- | --- |
| CLI and policy | Save a portable profile, bind the intended scope, explain selection and observed application | Invalid IDs, missing current chat, unknown fields, failed writes, disabled policy, inheritance and unrelated scopes stay intact | `cmd/cq/harness*_test.go`; selective installed fixture runs the real CQ CLI |
| Capability selection | Full, Minimal and selective plugins, standalone MCP, local skills and dependencies | Missing capabilities, globally disabled controls, denied dependencies and uncertain native assemblies remain explicit | `cloud_harness_profiles_test.go`, `cloud_harness_runtime*_test.go`, native capability-isolation fixture |
| Core web search | A third-party model under Minimal calls Codex Search and receives the result in its continuation | Native app and connector denials remain in force; unrelated apps stay excluded | `cloud_harness_apps_test.go`, `TestHarnessInstalledCuratedWebSearch` |
| Turn boundaries | Apply configuration after the current turn finishes | In-flight starts/settings, late completion, stale reads and another active turn cannot trigger an early reload | `cloud_harness_activity_test.go`, `cloud_harness_test.go` |
| Automatic goals | Hold continuation, finish current turn, apply configuration, resume the same goal | Budgeted and unbudgeted goals, accrued usage notifications and paused-turn accounting | `cloud_harness_goal*_test.go`, native goal-continuation fixture |
| User goal authority | Preserve objective, identity and budget limit | Explicit pause, clear, completion and objective edits supersede CQ restoration; rejected edits retain CQ recovery state | Goal hook tests and native goal-user-mutation fixture |
| Native RPC failures | Verified pause and restoration acknowledgements | Rejection, timeout, cancellation, malformed replies, wrong chat, unknown status and failed readback cannot count as success | Goal tests, `cloud_harness_failure_test.go`, runtime protocol boundary tests |
| Persistence and recovery | Durable ownership before pause; clear receipt only after restoration or a confirmed ownership change | Failed intent writes, lost pause results, crash recovery, failed restoration writes and repeated recovery retain sufficient state | Goal tests, receipt tests, controller recovery tests |
| Subscription ownership | Reload only the selected chat and restore its actual owners | Multiple subscribers, disconnected writers, replay, response chunking and failed subscription restoration | Source/gateway tests and both native multiple-subscriber/gateway code modes |
| Native lifecycle | Same-chat restoration after process restart | Empty chats wait for durable history; ephemeral chats and retained native assemblies report their actual limits | Capability-isolation and runtime-state tests |
| Cache and privacy | Unchanged reconciliation preserves inference prefix and current selection | Retried failures do not mutate active configuration; diagnostics omit goal text, credentials and native error payloads | `cloud_harness_cache_test.go`, failure and receipt tests |
| Upgrade compatibility | Required native families pass on the pinned desktop-version baseline and current public Codex | Missing, skipped, cancelled or failing cases block qualification; executable identity must remain unchanged | Harness compatibility workflow and installed validator |

## Native goal recovery

CQ uses native pause accounting for all goals. The objective and token budget
limit remain intact. Native Codex excludes the remainder of the paused turn from
token and time counters. Curation must not reset those counters or use a terminal
budget status as a temporary hold.

A verified held goal is reactivated only while its identity, paused status and
native mutation timestamp still match CQ's receipt. Explicit user mutations
take precedence. The current native goal API has no compare-and-swap mutation or
public goal generation ID. A crash or lost reply between requesting pause and
recording its verified result can therefore leave ownership ambiguous.

For that case, CQ retains the recovery marker and reports
`native_goal_restore_ambiguous` with a partial application receipt. It keeps the
goal paused instead of guessing whether a later pause belonged to the user.
An explicit native goal update resolves that ownership: resuming, editing,
clearing or completing the goal remains the user's choice. Rejected updates or
failed receipt writes must retain the recoverable state. A partial receipt is an
unresolved qualification outcome and must not be reported as complete automatic
restoration.

## Running qualification

Use the Go toolchain declared in `go.mod`. Before native qualification, check
available shared disk space for the test build, isolated homes and native client
archive. The test fixtures own and remove their temporary processes and homes.

```sh
bash .github/scripts/validate-harness-installed.sh /absolute/path/to/native/codex
```

The validator records source commit, native version and executable SHA-256,
discovers the installed test cases, requires the critical families, runs them
with race detection and rejects skipped cases. The latest public CLI and pinned
desktop-version baseline run in the reusable Harness compatibility workflow on
pull requests, main CI, releases, manual dispatch and a weekly schedule.

The public binary at a desktop version can differ from the executable bundled
with the desktop app. Local deployment also qualifies the actual bundled
executable. A previous version's passing result does not qualify a new native
binary. Update a baseline only after reviewing the changed native contract,
running the complete matrix and retaining regressions that distinguish the old
failure from the corrected behaviour.
