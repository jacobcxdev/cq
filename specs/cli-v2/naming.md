# Canonical CQ naming glossary

Audited all six specification fragments: root, accounts, models, routing, candidate, validation. Covers all distinct option/positional names; injected globals listed separately. Command paths remain unchanged by this review. Each entry states concrete meaning and reason for its spelling. Command-specific help remains authoritative for values, defaults and prerequisites; this glossary does not duplicate mutable help text.

## Every option and positional name

### account

Selects an existing provider account rather than credentials or a routing session. Provider namespace determines accepted reference syntax; Codex stores resolved opaque AccountKey.

Used as option `--account`, positional `account`.


### activate

Makes newly logged-in account the native client default; distinguishes persistent account selection from proxy pinning and token refresh.

Used as option `--activate`.


### attempt-id

Selects one retained validation attempt, not latest result; exact identity prevents reading a different run's receipt.

Used as option `--attempt-id`.


### client-build

Binds evidence to the exact client version string, matched bytewise, rather than assuming every executable with the same name behaves identically.

Used as option `--client-build`.


### client-executable

Names executable bytes to exercise or attest; separate from client-build because a label alone does not identify a binary.

Used as option `--client-executable`.


### client-registry

Names the declared set of request senders and transports, not the model registry; needed to establish complete client participation.

Used as option `--client-registry`.


### clone-from

Names native source supplying missing model metadata; directional name distinguishes source ID from the new overlay ID.

Used as option `--clone-from`.


### command-path

Names an ordered sequence of command words for help lookup, not a filesystem path.

Used as positional `command-path`.


### component

Selects proxy service, token-refresh service, or both; service lifecycle remains independent from provider routing configuration.

Used as option `--component`.


### confirm-artifact-switch

Explicitly authorises replacing candidate runtime artifact; retained semantic confirmation prevents confusing inspection with runtime replacement.

Used as option `--confirm-artifact-switch`.


### confirm-candidate-healthy

Asserts replacement credential owner passed health verification before finalising legacy migration; assertion complements runtime checks.

Used as option `--confirm-candidate-healthy`.


### confirm-candidate-state-loss

Explicitly authorises permanent candidate directory/key/receipt deletion; names loss rather than using an undifferentiated yes flag.

Used as option `--confirm-candidate-state-loss`.


### confirm-client-stopped

Asserts all candidate clients stopped and requests drained before stopping candidate runtime.

Used as option `--confirm-client-stopped`.


### confirm-control-health

Asserts candidate passed control-plane health before release validation; does not claim end-to-end traffic proof.

Used as option `--confirm-control-health`.


### confirm-payload-capture

Explicit opt-in for recording request content during candidate workflow; preparation alone does not record content.

Used as option `--confirm-payload-capture`.


### confirm-read-only-credentials

Attests credentials are restricted to read-only use; does not grant refresh or import authority.

Used as option `--confirm-read-only-credentials`.


### confirm-stopped-and-drained

Asserts proxy and endpoint participants remain stopped with no work outstanding throughout credential endpoint transition.

Used as option `--confirm-stopped-and-drained`.


### content-encoding

Describes saved body encoding, not text character set or output format; auto recognises Zstandard bytes then identity.

Used as option `--content-encoding`.


### credential-manifest

Identifies credential attestation input; manifest is recorded evidence, not an import file.

Used as option `--credential-manifest`.


### credential-mode

Selects absent versus read-only credential attestation; modes constrain other required or forbidden flags.

Used as option `--credential-mode`.


### credit

Selects exact banked reset credit; distinguishes consumable credit identity from account identity and natural quota-window reset.

Used as option `--credit`.


### digest

Selects keyed privacy-safe session digest, not arbitrary file SHA-256; full value avoids prefix ambiguity.

Used as option `--digest`.


### file

Supplies complete routing-policy JSON document. Generic spelling is retained because command context fixes resource type; not a patch.

Used as option `--file`.


### follow

Keeps trace reader attached to new records and rotation after retained results; does not start capture or connect to proxy.

Used as option `--follow`.


### fresh

Requests fresh quota fetch instead of initial cache read; distinguishes quota data from OAuth refresh and model refresh.

Used as option `--fresh`.


### id

Names exact new/removed model identifier inside overlay namespace; retained for compatibility although model-id would be clearer in isolation.

Used as option `--id`.


### input

Selects saved request-body source for fixture creation; input role distinguishes it from generated fixture output.

Used as option `--input`.


### metadata-json

Supplies literal JSON metadata; suffix json and help explicitly distinguish inline data from a filename.

Used as option `--metadata-json`.


### migrate-legacy-managed

Opt-in to add routing identity metadata to old CQ-owned credentials before serve; limits mutation to legacy managed records.

Used as option `--migrate-legacy-managed`.


### name

Names pool selected for creation/update/value adjustment; pool context distinguishes it from account identity.

Used as positional `name`.


### new-name

Specifies replacement pool display name; paired with old-name makes rename direction explicit.

Used as positional `new-name`.


### non-interactive

Suppresses typed terminal confirmation after mandatory semantic confirmation; it does not grant the underlying operation consent.

Used as option `--non-interactive`.


### old-name

Selects existing pool being renamed; preserves distinction between lookup identity and replacement display name.

Used as positional `old-name`.


### operation-id

Selects durable coordinator operation spanning multiple steps, rather than one validation attempt or runtime session.

Used as positional `operation-id`.


### output

Selects new fixture destination file; existing files rejected, preventing silent replacement of evidence.

Used as option `--output`.


### payload

Chooses existing opt-in payload trace log instead of causal routing events; may reveal recorded user content but never enables recording.

Used as option `--payload`.


### percent

Specifies remaining full-window quota percentage-point threshold to protect; not percent of currently remaining capacity.

Used as option `--percent`.


### policy-snapshot

Supplies policy attestation bytes to candidate prepare; records hash without applying policy.

Used as option `--policy-snapshot`.


### pool

Selects named account group for session binding; not an individual account or provider.

Used as option `--pool`.


### port

Selects loopback network port; command distinguishes listener creation from control/health target and candidate-only restrictions.

Used as option `--port`.


### provider

Selects one model source; canonical public values use Claude/Codex product names.

Used as option `--provider`.


### providers

Selects repeated quota-check or authentication providers; plural consistently denotes one or more provider selections.

Used as positional `providers`.


### receipt-file

Selects new output path for release-promotion receipt; suffix file distinguishes filesystem destination from receipt identity.

Used as option `--receipt-file`.


### release-digest

Selects verified target bundle's declared digest; not file hash and not executable hash.

Used as option `--release-digest`.


### rollback-bundle

Supplies previously accepted ancestor release bundle that defines rollback floor; distinct from target bundle.

Used as option `--rollback-bundle`.


### rollback-receipt

Supplies original proof that rollback release was accepted; bundle presence alone does not prove prior acceptance.

Used as option `--rollback-receipt`.


### rollback-receipt-digest

Binds exact rollback receipt with domain-separated SHA-256; explicit noun distinguishes this digest algorithm from target release digest.

Used as option `--rollback-receipt-digest`.


### session

Trace filter accepting raw session ID or Codex thread URI; intentionally broader than exact session-id used by binding operations.

Used as option `--session`.


### session-id

Supplies exact raw routing session identifier; not provider account or retained trace ID.

Used as option `--session-id`.


### session-id-stdin

Reads exact session identifier bytes from stdin to avoid shell-history exposure; suffix specifies transport, not a different identity type.

Used as option `--session-id-stdin`.


### shell

Selects completion output grammar for bash, zsh, or fish; generated script is not installed or executed by this selection.

Used as positional `shell`.


### since

Sets relative lower time bound for trace records, anchored at command start; duration is not an absolute timestamp.

Used as option `--since`.


### snapshot-file

Supplies inspection snapshot for legacy credential transition preparation; snapshot describes observed state rather than authorising subsequent transition.

Used as option `--snapshot-file`.


### source-config

Supplies candidate source-configuration attestation; name identifies provenance input, though contents are not applied.

Used as option `--source-config`.


### state-dir

Selects owned state root rather than arbitrary report output; command context determines candidate isolation, readiness evidence, or offline policy.

Used as option `--state-dir`.


### strict

Converts unhealthy/absent/indeterminate status into nonzero exit; does not request deeper probes or stricter state mutation.

Used as option `--strict`.


### tail

Limits newest matching retained trace rows before follow; zero means all retained matches, not zero output.

Used as option `--tail`.


### target-release-bundle

Supplies intended signed operational release bundle; target distinguishes it from accepted rollback floor.

Used as option `--target-release-bundle`.


### ticket-file

Supplies prepared transition authority/progress ticket for exact legacy operation; distinct from read-only inspection snapshot.

Used as option `--ticket-file`.


### timeout

Bounds time spent on operation work; duration spelling avoids bare-unit ambiguity. Total operation time includes cleanup and excludes terminal consent; fixed-deadline commands omit the option.

Used as option `--timeout`.


### trace

Selects exact causal trace ID in logs; separate from session because one session can contain many traces.

Used as option `--trace`.


### validation-run-id

Binds all candidate workflow stages to same prepared validation run; 64 hex characters are identifier encoding, not file hash.

Used as option `--validation-run-id`.


### value

Sets relative capacity-preservation rank of pool; higher ranks preserve expensive/scarce pools. It is neither money nor quota percentage.

Used as option `--value`, positional `value`.


### window

Selects exact provider quota window returned by reserve windows; no guessed provider or time-window default.

Used as option `--window`.


### yes

Confirms account removal or reset-credit consumption without prompt; only skips interactive consent where command explicitly allows it.

Used as option `--yes`.

## Injected global names

- `--help`, `-h`: show argument grammar, behaviour, prerequisites and examples for selected command; never access operational state.
- `--json`, `-j`: choose one structured result/error envelope (except documented trace streaming); also prevent interactive prompts. It does not mean quiet, successful, or confirmed.
- `--version`, `-v`: print CQ version, not provider/client version. Short `-v` is deliberately not verbosity.

## Architectural naming decisions and overlapping terms

- **Provider-first** places Codex-only routing, resets, account work and validation under `codex`; Claude account/pin work under `claude`. Shared service/process management stays at root. Users should not infer Codex from an omitted provider on a shared command.
- **Account** is an upstream account identity. **System account** is the account supplying the native client/system credential source, not the operating-system administrator or a CQ service user. **Managed** means CQ owns credential writes; **external** means another declared source owns credentials. One logical account may have several candidate credential sources. Activating an account changes native-client default; pinning affects proxy routing.
- **Reset** consumes one banked reset credit to restore quota percentages. It does not reset credentials, recreate files, restart the proxy or shift natural quota-window dates. **Reserve** protects remaining system-account quota below a threshold. **Prime** starts eligible quota windows through priming work; it neither adds quota nor consumes a reset credit. These three names must remain separate.
- **Pin** constrains proxy routing to selected account. **Fallback** sets the configured Codex default account used by the routing engine's applicable fallback rule; it is distinct from account activation and forced pinning. **Pool** groups accounts with capacity-preservation value. **Policy** is the complete rules document; apply means replacement, not merge patch.
- **Session** is routing identity for related client work. **Thread** is client conversation identity that may appear in trace filtering. **Lease** is proxy-owned routing/admission state retaining continuity or ownership; invalidating leases is not deleting a user conversation or revoking upstream credentials. **Trace** is one causal execution record chain. **Operation** is durable coordinator workflow. **Attempt** is one execution attempt within validation. None is interchangeable with an account ID.
- **Candidate** is isolated runtime under explicit owned state root for testing proposed release. **Release** is verified signed operational bundle and source identity; **artifact** is runtime binary/package activated from release. **Receipt** is retained verification evidence, not a prediction that future runs succeed. **Readiness** reports evidence availability/status; **validate** performs proof-producing activity. **Health** probes current liveness; **status** inspects broader configured and runtime state. Healthy never implies accepted release.
- **Fresh** requests new quota data. **Auth refresh** rotates eligible stored OAuth credentials. **Models refresh** rebuilds model data/publications. **Client-safety refresh** updates candidate safety barrier evidence. All use a resource-specific context so refreshing one resource does not imply refreshing all resources.
- **Serve** runs proxy in foreground. **Service** manages supervisor registration/running state. **Start/stop/restart** act on that selected service or isolated candidate. **Install/uninstall** change registration. **Enable/disable** retain reserve/priming settings while toggling policy. **Clear** removes selection/configuration. **Remove** deletes named local resource. These pairs deliberately distinguish reversible settings from deletion.
- **Show** reads one resource; **list** enumerates resources; **status** reports operational condition. Bare groups show help. Singular namespace nouns (`account`, `reset`, `pool`, `session`, `lease`, `overlay`) describe resource kind; existing root `models` is retained to avoid unnecessary migration. `windows` enumerates selectable quota windows, rather than managing one window resource.
- **Auth** groups authentication work across providers. Gemini's automatic in-process token refresh remains part of quota fetching; `auth refresh gemini` is rejected because no stored Gemini refresh operation exists.
- **Fixture** is saved, normalised test input created from request bytes. **Capture** would imply collecting live content; replacing the old capture spelling with fixture create prevents that implication. **Content encoding** is compression format, not JSON schema or character encoding.
- **Credential endpoint legacy** scopes migration machinery to old credential-owner protocol. **Prepare** records/validates transition state; **resume** continues it; **activate** switches replacement owner; **finalise** confirms healthy transition and commits completion; **rollback** restores recorded predecessor. **Ticket** carries prepared transition state; **snapshot** is observed inspection input.

## Final naming decisions

1. **Timeout** means total operation time including cleanup and excluding terminal consent. Cleanup receives no extension beyond that budget. Candidate stop/remove use fixed 30-second deadlines and candidate release validate uses fixed 16-minute deadline; these commands expose no timeout option. Models/auth have no whole-command deadline and retain explicitly documented source/request bounds.
2. **Providers** is the repeatable positional name for both check and auth refresh. Models uses singular --provider because it selects one source.
3. **Value** stays scoped under pool and means capacity-preservation rank: higher values preserve the pool by preferring lower-value viable accounts. It is neither money nor quota percentage.
4. **ID** stays scoped under models overlay and always means exact model ID. Retaining --id avoids unnecessary migration; help and metavar identify model explicitly.
5. **Source-config and policy-snapshot** retain existing names as attestation inputs. Their contents are hashed and recorded, never applied as candidate configuration. Help and examples state this explicitly.
6. **Confirm-artifact-switch** remains under release activate. Release identifies verified bundle; artifact is the candidate runtime being replaced. Flag explicitly authorises that replacement.
7. **Confirm-candidate-healthy** remains scoped to legacy endpoint transition and means replacement credential owner passed live verification. It does not refer to a separate isolated proxy candidate workflow.
8. **Digest algorithms are resource-specific.** Session digest is keyed; release digest is verified bundle field; rollback receipt digest is domain-separated SHA-256. Exact computation and validation are stated per resource; generic file hashing is never implied.
9. **Session versus session-id** intentionally distinguishes flexible trace filtering from exact routing selector. Trace accepts raw session ID or supported thread URI; binding accepts exact raw ID bytes or full keyed digest.
10. **State-dir** consistently selects operational state root, with exact ownership/default rules per command. Candidate isolation, readiness evidence and offline policy roots are not interchangeable output directories.
11. **Account timeout help** describes operation duration only. Reset-retry cautions appear only where a reset credit can be consumed; unrelated repeated cautions are removed.
12. **Client-build** means exact evidence version string, matched bytewise in candidate and validation paths. No arbitrary label or undocumented normalisation can satisfy executable version evidence.
13. **Tail zero** deliberately requests all matching retained historical log records. It does not request zero output and cannot recover records already rotated away.
14. **Non-interactive grants no consent.** It suppresses typed interaction only after required semantic confirmations; JSON also suppresses interaction. --yes confirms only account removal/reset consumption where explicitly supported.
15. **Fallback** means engine's configured default-account selection at its defined routing stage. It never overrides a bound session, pinned account or continuity constraint and does not promise arbitrary failover.

Coverage: all distinct option/positional names from six fragments are listed, with shared globals separately. Repeated help snapshots are intentionally omitted so this glossary cannot contradict later edits to command-specific constraints. No command paths changed during this naming review.
