# Collect diagnostics and report a CQ issue

[README](../README.md) · [Troubleshooting](troubleshooting.md) · [Configuration and paths](configuration.md#important-paths) · [Report a bug](https://github.com/jacobcxdev/cq/issues/new?template=bug_report.yml)

Give this guide to Codex or another coding agent when CQ does not work as expected.
Start with existing evidence; reproduce with additional tracing only when needed.
A report without logs is still useful: explain what could not be collected and why.
Do not make a fresh reproduction a requirement for reporting an intermittent bug.

## A prompt to give your agent

> Read https://github.com/jacobcxdev/cq/blob/main/docs/agent-issue-reporting.md.
> Help me investigate this CQ problem: [describe what happened and when].
> Preserve my accounts, configuration and running work. Collect the smallest
> relevant diagnostics, distinguish observed facts from guesses, and prepare a
> redacted bug report for jacobcxdev/cq using its bug-report template. Ask before
> restarting applications/services, enabling sensitive payload capture, or
> uploading anything. Show me the exact report and attachments first, then submit
> the approved report and give me its URL. Restore temporary tracing afterwards.

Agents should follow the installed command's `--help` and this workflow. Do not
upgrade, reinstall, clear caches/leases, switch accounts, consume resets, change
VPN routes or run release validators merely to collect diagnostics. Those actions
can alter the evidence, interrupt work or make real upstream requests. Propose a
specific repair separately once the evidence supports it.

## 1. Record the failure before changing anything

Capture the user's action or exact command, expected and actual behaviour, error
text, first/last occurrence with timezone, frequency and whether it ever worked.
Record the affected provider, model, client and transport if known. Say whether
this is ordinary client routing, CQ Network regional access, usage adaptation,
multi-account Dots, or a combination. These are separate integrations.

Record CQ's installed version, the actual executable resolved by the user's shell,
OS version/architecture, installation method and client version/build. `cq
--version` can differ from the active or pending service version. On macOS, a
running Dots adapter also has a separately staged executable; installing a package
does not prove that worker has adopted it. Do not publish full binary build/link
metadata: it can include release-linked OAuth material.

Collect applicable status commands into a new private local directory outside the
repository, with owner-only access (`umask 077` on Unix; user-only access controls
on Windows). Save stdout, stderr and exit status separately. A non-zero status is
useful evidence; do not discard it or stop collecting the other applicable files.
These commands inspect existing state without restarting or switching accounts:

```sh
cq --version
cq service status --json
cq proxy status --strict --json
```

Add only the relevant inspections:

```sh
cq codex network status
cq codex network usage status
cq codex dots status --json
```

Dots status can make native/provider reads; it is not an offline inspection.

Do not run macOS-only commands on another platform. For quota/account failures,
`cq --json check codex` (substitute the affected provider) can help, but reads
credentials and may contact the provider/coordinator. `--refresh` bypasses cache;
use it deliberately and label the result as a fresh read. A successful command
exit does not mean every provider result succeeded. Inspect per-account errors.

Keep a private collection manifest: command, start/end time, exit status and local
filename. Status output itself can identify users, account pools, paths and
network endpoints; it still needs review before publication.

## 2. Know what was already being recorded

| Evidence | Default and retention | Collection policy |
|---|---|---|
| Service errors/output | macOS service stderr goes to `proxy.log` / `refresh.log`, without CQ rotation/age expiry. Linux uses the systemd journal and its host retention policy. Windows tasks have no built-in stdout/stderr file redirection. This is not full request tracing. | Collect a short excerpt or task result around the incident, not whole lifetime logs. |
| Routing metadata, `diagnostics_log` | Unset by default. When configured, JSONL rotates at a 64 MiB target and retains four prior files plus the current file. Rotation is size-based, not a number of days; individual records can exceed the target. | Suitable for continuing bounded metadata collection when the user chooses it. It can still reveal activity, identifiers, pool names and errors. |
| Raw content, `payload_diagnostics_log` | Unset by default. Uses the same file rotation policy. Codex capture retains up to 64 KiB per HTTP body/WebSocket frame, so content can be incomplete. | Short, explicitly approved reproduction only. Do not leave enabled as a routine default. |
| Dots event/helper logs | Existing adapter workers append logs under their private `runtime` directory. CQ currently has no rotation/age expiry for them. | Collect only the relevant role and incident range; never archive the whole runtime. |
| Durable leases, quota history and recovery state | Product state with its own retention rules, not a public support bundle. For example, `codex_lease_retention_days` does not set trace-log retention. | Do not delete or upload it to diagnose a routing problem. |

Default log roots are listed in [configuration](configuration.md#important-paths);
respect `XDG_CONFIG_HOME`, redirected Windows known folders and custom Dots roots.
On macOS, inspect incident-time excerpts from `~/Library/Logs/cq/proxy.log` and
`refresh.log`. On Linux, use a bounded journal query for the affected user units,
for example:

```sh
journalctl --user -u cq-proxy.service -u cq-refresh.service \
  --since '15 minutes ago' --no-pager -n 300
```

Linux journal retention is controlled by the host. On Windows, inspect the current
user's `\cq\Proxy` / `\cq\Refresh` task results and any explicitly configured
diagnostic files. A Local AppData log root does not imply that those tasks created
service log files. Do not collect another user's tasks or whole event logs. For
Dots, prefer status first, then incident-time excerpts from the relevant
`events.jsonl`, `realtime-events.jsonl`, `cloud-stdio-events.jsonl` or
`release-ROLE.log` beneath `~/.local/share/cq/dots/runtime` (or the configured root).
Private runtime files can contain credentials or conversation data. Proxy traces
are not a complete record of every native-network or Dots operation.

### Read an existing Codex trace

`cq proxy trace` reads existing files; it does not enable tracing, contact the
proxy, replay the request or change routing. If no path is configured, report
that fact. It cannot reconstruct an earlier request that was never recorded.

Start with the affected session and a short time window. Replace placeholders
locally; do not publish the raw session URL merely to explain a hashed trace:

```sh
cq proxy trace --session codex://threads/THREAD_ID --since 15m --tail 0 --json
cq proxy trace --trace trace:ID --since 15m --tail 0 --json
```

These are alternatives for selecting a session or an exact trace. Default output
is the newest 200 matching records; `--tail 0` includes all matching records from
the retained files, so narrow by time/session first. `--since` is relative to when
the command runs. Preserve the collection time. For an unknown session, inspect a
short recent summary with `cq proxy trace --since 5m --tail 200`, then select the
relevant trace. Do not silently attach unrelated users' or tasks' traffic.

## 3. Capture one bounded reproduction when necessary

1. Agree on the smallest safe action, a short capture window and a stopping
   condition. Prefer a harmless prompt or test account. Do not replay a purchase,
   reset, message send or uncertain operation just to produce a log.
2. Inspect the existing `diagnostics_log` / `payload_diagnostics_log` fields in
   the real CQ `proxy.json` privately. Record whether each was absent, empty or
   set, and its prior value. There is no diagnostics-toggle CLI; do not invent one.
3. If metadata is absent and needed, create a private directory and add only
   `diagnostics_log` with an absolute path to a JSONL file. Keep payload capture
   unset. Preserve all other settings, unknown JSON fields and file permissions;
   validate JSON and write atomically. Do not replace the configuration with a
   documentation example or print its token. A private backup contains secrets.
4. A diagnostics-path change requires a proxy restart. Explain the interruption
   and obtain permission, then use the owning installation's restart procedure
   (`cq service restart` for a complete installation). Do not start a second
   listener, kill an unknown process or restart ChatGPT just to read logs.
   Check active runtime/service status after the restart.
   A writer-open failure can leave capture disabled despite the configured path:
   confirm a new matching event after the reproduction, not just the setting.
5. Note the start time, reproduce once, note the end time/error, and export the
   matching metadata trace promptly before rotation. `--follow` can watch a
   reproduction; stop it with Ctrl-C. Stopping the reader does **not** disable
   collection by the proxy.
6. If metadata cannot answer the question, explain precisely which raw content
   is needed and request separate payload-capture approval. Use a different
   absolute `payload_diagnostics_log` path in private storage, restart as above,
   and run only the agreed reproduction. `cq proxy trace --payload` reads that
   file; it does not turn capture on. Use the same session/trace/time filters.
7. On success, failure, cancellation or interruption, restore just the two
   diagnostics fields to their prior values/absence, preserving concurrent edits
   elsewhere. Restart through the owner if those values changed, then verify
   service health and restored settings. If restoration is blocked, prominently
   report what is still enabled and the exact remaining action. Keep pre-existing
   routine metadata collection if that was the user's setting.

If capture started but the agent was interrupted, perform the restoration step
first on resuming. Include any temporary raw files, `.1`–`.4` rotations and private
configuration backups in the local cleanup inventory; never upload those backups.

Raw payload capture can contain prompts, system prompts, tool arguments/results,
messages, compact summaries, provider responses and secrets inside those bodies.
Credential-bearing headers are excluded, but neither that filtering nor hashed
session/account hints make a file safe to upload. Capture can include concurrent
traffic, not only the intended reproduction. Keep raw files private and minimise
the window. Other providers can capture full buffered bodies rather than the
Codex 64 KiB prefix. Rotation does not erase a disabled log after a fixed number
of days. `--json` extraction is not a redaction step.

## 4. Prepare the smallest public-safe report

Work on copies in a separate `share` directory, keeping raw evidence private.
Review every line and screenshot. Remove tokens/API keys, passwords, cookies,
authorisation headers, OAuth URLs/callback queries, client secrets, WireGuard
keys/profiles, encrypted session keys, private prompts/code/tool output and other
people's data. Generalise personal paths, email/account identities, organisation
names, pool names and network endpoints when necessary. Keep stable replacements
such as `ACCOUNT_A` so events still correlate; preserve timestamps, trace IDs,
status/error codes and ordering where safe. Review nested JSON, headers, free-text
errors and encoded payloads too. A secret scanner alone is not a redaction review.

Never attach whole `proxy.json`, credential stores, a home/config directory,
private state database, shell environment, full binary link flags, or an untouched
raw payload log. Missing evidence is better than disclosing confidential data.
For a suspected security vulnerability or exposed credential, do not post sensitive
details in a public bug report; arrange private handling with the maintainer and
use the credential owner's revocation process where appropriate.

Prepare an `issue.md` using the [bug form's fields](../.github/ISSUE_TEMPLATE/bug_report.yml):

- **Problem:** concise summary, expected result and actual result/error.
- **Environment:** CQ/package/active/pending versions, OS/architecture, installer,
  client/build, provider/model and relevant integration/routing mode.
- **Reproduction and timing:** minimal numbered steps, timezone, frequency,
  incident and collection times; say if reproduction is unavailable.
- **Diagnostics:** sanitised status and smallest trace/log excerpt, trace ID,
  evidence source (installed client, isolated candidate or synthetic test), plus
  missing/truncated evidence and collection errors. Separate facts from hypotheses.
- **Changes and cleanup:** what was tried, outcome, temporary settings restored,
  and any tracing/restart still pending.

Use fenced excerpts or collapsible details for small text. If attachments are
needed, list each filename, what it contains, incident range and redactions.
Give the user the exact report and exact sanitised files for review before any
upload. Do not create a public Gist or temporary upload as a redaction preview.

## 5. Submit and confirm

Search [existing issues](https://github.com/jacobcxdev/cq/issues) for the symptom
and version. If it is clearly the same incident, propose adding the evidence there
rather than duplicating it. Do not claim an upstream cause merely because a
similar report exists.

After the user approves public submission of the exact text and files, open the
[bug-report form](https://github.com/jacobcxdev/cq/issues/new?template=bug_report.yml)
or use GitHub CLI with the prepared Markdown:

```sh
gh issue create --repo jacobcxdev/cq \
  --title 'Affected feature: concise symptom' --body-file /ABSOLUTE/PATH/issue.md
```

The CLI body must contain the same fields; web-form validation does not validate a
body supplied this way. If GitHub authentication is unavailable, hand over the
reviewed report and form URL rather than asking for a token in chat.

For reviewed text/log/JSON/archive attachments, use the GitHub issue/comment
editor's file upload, then include the resulting links. **Selecting an attachment
uploads it immediately, before you submit the issue/comment.** Public-repository
attachments are public; get approval before selecting files. A local filesystem
path in Markdown does not upload a file. Do not assume a CLI image/video attachment
flag accepts arbitrary diagnostic archives; consult installed `gh --help` first.
See [GitHub's attachment documentation](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/attaching-files)
and [CLI issue creation](https://cli.github.com/manual/gh_issue_create).

Confirm the returned issue URL, submitted body and attachment links. If a command
fails or its result is uncertain, inspect the repository before retrying so you
do not create duplicates. Finish by stating what was collected, what remains
unverified, whether temporary capture is off, and any user action still needed.
Retain local raw evidence privately only as long as needed with the user's
agreement; remove temporary copies deliberately, without deleting CQ's live state.
