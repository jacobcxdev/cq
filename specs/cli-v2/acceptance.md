# Acceptance catalogue

These are implementation requirements, not claims that the current binary passes. Commands must also pass the global cases in README.md.

## `cq auth`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.

## `cq auth refresh`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.
- [ ] Provider selection never reads or writes unselected provider credentials.
- [ ] Unknown expiry is skipped.
- [ ] Exactly 30 minutes remaining is eligible.
- [ ] System/external Codex candidates are never refreshed.
- [ ] JSON never launches browser.
- [ ] EOF at reauthentication prompt counts outstanding login as unresolved, not success.
- [ ] Per-candidate broker failures are included in output instead of silently skipped.
- [ ] One successful credential write followed by a failed attempt reports credentials_changed true and changed_count 1 even when final account status is failed.

## `cq check`

- [ ] Bare cq and cq check have identical report scope.
- [ ] check codex --fresh works; check codex codex fails exit2 before credentials.
- [ ] Unknown --dry-run rejected before reads.
- [ ] One successful and one failed account emits report plus check_partial exit8.
- [ ] JSON contains no ANSI colour even on TTY.

## `cq claude`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq claude account`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq claude account activate`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Codex exact key resolves accounts with duplicate email.
- [ ] Alias/email collisions reject rather than silently prefer email.
- [ ] Already-active account reports changed=false.

## `cq claude account list`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Empty inventory returns accounts=[] and exit 0.
- [ ] Duplicate-email Codex identities have distinct exact account_reference values.
- [ ] No secret token, password or credential file contents appear in either output mode.

## `cq claude account login`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Default login leaves active credentials unchanged.
- [ ] --activate=false does not activate.
- [ ] OAuth callback invalid input does not consume the valid callback attempt.
- [ ] Partial activation error includes credentials_saved=true and exit 8.
- [ ] Default relogin preserves the native default and reports its observed active identity without activation.
- [ ] Post-save observation failure preserves credentials_saved=true with account=null; known explicit activation result remains available.
- [ ] An indeterminate dispatched activation reports activated=null, never retries the mutation, and retains timeout or interruption exit precedence.

## `cq claude account remove`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Default No cancels with removed=false and exit 0.
- [ ] JSON without --yes fails before mutation.
- [ ] External-only account fails account_read_only.
- [ ] Active credentials removal never activates another account.

## `cq claude proxy`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq claude proxy pin`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq claude proxy pin clear`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Canonical UUID input is rejected; a legacy UUID maps to exactly one known email or fails without saving.

## `cq claude proxy pin set`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Set and clear pass provider-aware parsing; regression for legacy pin arity bug.
- [ ] An ambiguous reference causes no write.
- [ ] Canonical UUID input is rejected; a legacy UUID maps to exactly one known email or fails without saving.

## `cq claude proxy pin show`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Canonical UUID input is rejected; a legacy UUID maps to exactly one known email or fails without saving.

## `cq codex`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex account`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex account activate`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Codex exact key resolves accounts with duplicate email.
- [ ] Alias/email collisions reject rather than silently prefer email.
- [ ] Already-active account reports changed=false.

## `cq codex account list`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Empty inventory returns accounts=[] and exit 0.
- [ ] Duplicate-email Codex identities have distinct exact account_reference values.
- [ ] No secret token, password or credential file contents appear in either output mode.

## `cq codex account login`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Default login leaves active credentials unchanged.
- [ ] --activate=false does not activate.
- [ ] OAuth callback invalid input does not consume the valid callback attempt.
- [ ] Partial activation error includes credentials_saved=true and exit 8.
- [ ] Default relogin preserves the native default and reports its observed active identity without activation.
- [ ] Post-save observation failure preserves credentials_saved=true with account=null; known explicit activation result remains available.
- [ ] An indeterminate dispatched activation reports activated=null, never retries the mutation, and retains timeout or interruption exit precedence.

## `cq codex account remove`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Default No cancels with removed=false and exit 0.
- [ ] JSON without --yes fails before mutation.
- [ ] External-only account fails account_read_only.
- [ ] Active credentials removal never activates another account.

## `cq codex proxy`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy canary`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy canary start`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Creates a protected local observation record for the current exact build/readiness tuple.

## `cq codex proxy canary status`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Reads retained canary state and verifies protected-state digests; never requests a transition.

## `cq codex proxy canary stop`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Records a stop request; the installed service drains active sessions and finalises the run asynchronously.

## `cq codex proxy credential-endpoint`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy credential-endpoint legacy`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy credential-endpoint legacy activate`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Activate reversible replacement window while retaining exact quarantine for rollback.
- [ ] Verify postcondition: Does not launch shared proxy automatically; operator must start intended owner and verify it before finalise.

## `cq codex proxy credential-endpoint legacy finalise`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Acquire exact live owner verification lease, persist finalising state, then irreversibly finalise and retire quarantine.
- [ ] Verify postcondition: After finalisation rollback is unavailable; replay must use retained ticket identity and existing committed result.

## `cq codex proxy credential-endpoint legacy inspect`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Read refused socket identity or existing transition; no mutation and idempotent.

## `cq codex proxy credential-endpoint legacy prepare`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Preserve exact legacy socket in quarantine and persist transition ticket; do not start proxy or credential owner.
- [ ] Verify postcondition: Existing transition is a conflict; use resume with its ticket.

## `cq codex proxy credential-endpoint legacy resume`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Reopen and validate retained transition; keep its phase, then release maintenance handle.
- [ ] Verify postcondition: Does not activate, finalise or rollback; repeated resume leaves durable state unchanged.

## `cq codex proxy credential-endpoint legacy rollback`

- [ ] Reject any snapshot field duplication, missing field, unknown field or identity mismatch before mutation.
- [ ] Help runs with no filesystem reads.
- [ ] JSON implies noninteractive but never supplies the mandatory semantic confirmation.
- [ ] Timeout retains inspectable journal and does not falsely claim completion.
- [ ] Verify postcondition: Restore exact preserved legacy socket; refuse if replacement participants are live or finalisation has passed irreversibility.
- [ ] Verify postcondition: Record rolled_back; never create a substitute legacy identity.

## `cq codex proxy fallback`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy fallback clear`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy fallback set`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Set and clear pass provider-aware parsing; regression for legacy pin arity bug.
- [ ] An ambiguous reference causes no write.

## `cq codex proxy fallback show`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy fixture`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy fixture create`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Offline only; no credentials or service access.

## `cq codex proxy hook`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy hook stop`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Reads stdin up to 16 MiB; ignores additional event fields without logging or storing them.
- [ ] Unicode and space-containing pool names accepted according to routing PoolName; render pool as a JSON string literal inside systemMessage.
- [ ] Session and turn identifiers accept valid UTF-8 up to 4096 bytes; reject 4097 bytes and control characters consistently with CLI session selectors.

## `cq codex proxy lease`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy lease invalidate`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy pin`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy pin clear`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy pin set`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Set and clear pass provider-aware parsing; regression for legacy pin arity bug.
- [ ] An ambiguous reference causes no write.

## `cq codex proxy pin show`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy policy`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy policy apply`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject --port with --state-dir before access.
- [ ] Invalid policy does not partially update state.

## `cq codex proxy policy show`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject --port with --state-dir before access.
- [ ] Invalid policy does not partially update state.

## `cq codex proxy pool`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy pool rename`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject duplicate accounts after alias resolution.
- [ ] Show --port in leaf help.

## `cq codex proxy pool set`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject duplicate accounts after alias resolution.
- [ ] Show --port in leaf help.

## `cq codex proxy pool value`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject duplicate accounts after alias resolution.
- [ ] Show --port in leaf help.

## `cq codex proxy prime`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy prime disable`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy prime enable`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy prime status`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.

## `cq codex proxy readiness`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy readiness show`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Read-only local marker inspection; does not create configuration or renew evidence.

## `cq codex proxy reserve`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy reserve clear`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy reserve disable`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy reserve enable`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy reserve set`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy reserve status`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy reserve windows`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] 2% threshold blocks at remaining=2, not at 2% of a smaller balance.
- [ ] Non-system accounts are never protected by this reserve.
- [ ] Disable rejects stale evidence before changing state.

## `cq codex proxy session`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy session bind`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject two selectors before reading stdin or contacting control.
- [ ] printf identifier and --session-id identifier yield the same digest; echo newline remains a different identifier.
- [ ] List rejects every selector.

## `cq codex proxy session digest`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject two selectors before reading stdin or contacting control.
- [ ] printf identifier and --session-id identifier yield the same digest; echo newline remains a different identifier.
- [ ] List rejects every selector.

## `cq codex proxy session list`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject two selectors before reading stdin or contacting control.
- [ ] printf identifier and --session-id identifier yield the same digest; echo newline remains a different identifier.
- [ ] List rejects every selector.

## `cq codex proxy session show`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject two selectors before reading stdin or contacting control.
- [ ] printf identifier and --session-id identifier yield the same digest; echo newline remains a different identifier.
- [ ] List rejects every selector.

## `cq codex proxy session unbind`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] Reject two selectors before reading stdin or contacting control.
- [ ] printf identifier and --session-id identifier yield the same digest; echo newline remains a different identifier.
- [ ] List rejects every selector.

## `cq codex proxy trace`

- [ ] All argument and help rules in the global contract apply.
- [ ] --help performs no state access and succeeds without configuration.
- [ ] --tail 0 returns all filtered records; duplicate --tail is rejected.
- [ ] Follow emits each record once through rotation.
- [ ] Output write failure is surfaced, not ignored.
- [ ] Timeout/interruption each emit exactly one terminal envelope with data.end and corresponding error; exit 7/130 without a second error envelope.

## `cq codex proxy validate`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex proxy validate http`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Durably records one-shot startup validation request and restarts the verified candidate service.
- [ ] Preparation, nested requests and cleanup share one monotonic deadline; none resets or extends the configured timeout.

## `cq codex proxy validate websocket`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Invalidates existing WebSocket readiness before exercise, after lexical/precondition validation.
- [ ] Preparation, nested requests and cleanup share one monotonic deadline; none resets or extends the configured timeout.
- [ ] Reserve 5s for cleanup inside the configured timeout; no additional grace period extends the deadline.

## `cq codex reset`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq codex reset list`

- [ ] Help is side-effect-free.
- [ ] Duplicate scalar flags and unknown arguments fail before writes.
- [ ] --json produces one v2 envelope.
- [ ] Empty visible inventory returns accounts=[] complete=true exit 0.
- [ ] One-account failure preserves successful rows and exits 8.
- [ ] Unknown upstream fields are tolerated; missing required reset_type/status remain explicit invalid rows.

## `cq codex reset recommend`

- [ ] Help is side-effect-free.
- [ ] Duplicate scalar flags and unknown arguments fail before writes.
- [ ] --json produces one v2 envelope.
- [ ] No upstream consume endpoint is called.
- [ ] Any missing account usage makes schedule non-actionable.
- [ ] exact=false does not imply complete=false.

## `cq codex reset use`

- [ ] Help is side-effect-free.
- [ ] Duplicate scalar flags and unknown arguments fail before writes.
- [ ] --json produces one v2 envelope.
- [ ] No consent means zero consume requests.
- [ ] Credit changed after preview does not select substitute.
- [ ] Uncertain result persists retry identity and idempotency key.
- [ ] reset/already_redeemed/nothing_to_reset/no_credit are explicit terminal outcomes; no_credit and nothing_to_reset are successful no-op exit 0.
- [ ] Known reset with failed usage refetch returns exit 8 and known outcome, never reports indeterminate.
- [ ] Natural reset timestamps remain unchanged.

## `cq completion`

- [ ] Deterministic script bytes for same schema and executable basename.
- [ ] Generation does not access user state.

## `cq gemini`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq gemini account`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq gemini account show`

- [ ] --help performs no credential, configuration, service or network access.
- [ ] Unknown options, duplicate scalar options and extra positionals fail with exit 2 before state writes.
- [ ] --json emits exactly one canonical v2 envelope; diagnostics stay on stderr.
- [ ] Missing Keychain entry yields configured=false, account=null, exit 0.
- [ ] Call Gemini DiscoverAccounts rather than unsupported AccountManager.

## `cq help`

- [ ] All canonical groups and leaves resolve.
- [ ] Unknown child exit2 with available sibling names.

## `cq models`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.

## `cq models list`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.
- [ ] -j and --json produce identical envelopes.
- [ ] --provider=codex equals --provider codex.
- [ ] Omitted filter includes both providers.
- [ ] No caches returns models: [] and exit 0.

## `cq models overlay`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.

## `cq models overlay add`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.
- [ ] Replacing an overlay changes only matching provider/ID.
- [ ] Missing explicit source fails without saving.
- [ ] Automatic inference ties resolve deterministically.
- [ ] Publication failure reports saved overlay instead of claiming rollback.

## `cq models overlay prune`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.
- [ ] Reject --provider codex rather than silently pruning both providers.
- [ ] Missing source data never deletes an unproven overlay.
- [ ] Zero prunable entries succeeds with count 0.

## `cq models overlay remove`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.

## `cq models refresh`

- [ ] Help succeeds without configuration or network access.
- [ ] Unknown flags and extra positional arguments fail before writes.

## `cq proxy`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy candidate`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy candidate client-safety`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy candidate client-safety refresh`

- [ ] Help succeeds without state access.
- [ ] Recognised well-formed invocation returns exit 4 and data=null with no writes, locks, file reads or child processes.
- [ ] Unknown, duplicate or malformed arguments fail exit 2 before the unavailable result.
- [ ] No supplied digest, attestation or confirmation can bypass the unavailable capability boundary.

## `cq proxy candidate prepare`

- [ ] Help and bare parent group succeed without reading environment or state.
- [ ] Reject unknown flags, duplicate non-repeatable flags, unexpected positional arguments and invalid paths before writes.
- [ ] Accept global --json before or after every command token and emit the common v2 envelope.
- [ ] Exercise every documented precondition failure and prove no mutation occurred.
- [ ] Verify postcondition: Create a new 0700 candidate directory and 0600 state, key and registry files atomically.
- [ ] Verify postcondition: Generate operation_id (32 lowercase hex), instance_id (32 lowercase hex), and validation_run_id (64 lowercase hex); record input SHA-256 digests and phase prepared.
- [ ] Verify postcondition: Do not start a listener, modify the installed service, import credentials, or apply attested configuration.
- [ ] Verify postcondition: Existing candidate state is a conflict; prepare never overwrites or silently reuses it.

## `cq proxy candidate receipt`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy candidate receipt show`

- [ ] Help and bare parent group succeed without reading environment or state.
- [ ] Reject unknown flags, duplicate non-repeatable flags, unexpected positional arguments and invalid paths before writes.
- [ ] Accept global --json before or after every command token and emit the common v2 envelope.
- [ ] Exercise every documented precondition failure and prove no mutation occurred.
- [ ] Verify postcondition: Read and authenticate retained receipt; repeated lookup is idempotent.

## `cq proxy candidate release`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy candidate release activate`

- [ ] Help succeeds without state access.
- [ ] Recognised well-formed invocation returns exit 4 and data=null with no writes, locks, file reads or child processes.
- [ ] Unknown, duplicate or malformed arguments fail exit 2 before the unavailable result.
- [ ] No supplied digest, attestation or confirmation can bypass the unavailable capability boundary.

## `cq proxy candidate release validate`

- [ ] Help succeeds without state access.
- [ ] Recognised well-formed invocation returns exit 4 and data=null with no writes, locks, file reads or child processes.
- [ ] Unknown, duplicate or malformed arguments fail exit 2 before the unavailable result.
- [ ] No supplied digest, attestation or confirmation can bypass the unavailable capability boundary.

## `cq proxy candidate remove`

- [ ] Help and bare parent group succeed without reading environment or state.
- [ ] Reject unknown flags, duplicate non-repeatable flags, unexpected positional arguments and invalid paths before writes.
- [ ] Accept global --json before or after every command token and emit the common v2 envelope.
- [ ] Exercise every documented precondition failure and prove no mutation occurred.
- [ ] Verify postcondition: Prove associated process/listener absent, then delete only identity-matched owned files and directory.
- [ ] Verify postcondition: Return final CandidateStatusV2 with phase removed before discarding state.
- [ ] Verify postcondition: Missing directory returns not found; never treats an arbitrary directory as removable candidate state.

## `cq proxy candidate start`

- [ ] Help succeeds without state access.
- [ ] Recognised well-formed invocation returns exit 4 and data=null with no writes, locks, file reads or child processes.
- [ ] Unknown, duplicate or malformed arguments fail exit 2 before the unavailable result.
- [ ] No supplied digest, attestation or confirmation can bypass the unavailable capability boundary.

## `cq proxy candidate status`

- [ ] Help and bare parent group succeed without reading environment or state.
- [ ] Reject unknown flags, duplicate non-repeatable flags, unexpected positional arguments and invalid paths before writes.
- [ ] Accept global --json before or after every command token and emit the common v2 envelope.
- [ ] Exercise every documented precondition failure and prove no mutation occurred.
- [ ] Verify postcondition: Read state without starting, repairing or validating a runtime.
- [ ] Verify postcondition: Repeated invocation is idempotent.

## `cq proxy candidate stop`

- [ ] Help and bare parent group succeed without reading environment or state.
- [ ] Reject unknown flags, duplicate non-repeatable flags, unexpected positional arguments and invalid paths before writes.
- [ ] Accept global --json before or after every command token and emit the common v2 envelope.
- [ ] Exercise every documented precondition failure and prove no mutation occurred.
- [ ] Verify postcondition: Stop only the identity-matched runtime; prove process/listener absence before phase stopped.
- [ ] Verify postcondition: Already stopped is a phase conflict; no implicit retries or second stop.

## `cq proxy health`

- [ ] Non2xx maps exit1, connection refused exit4, deadline exit7.
- [ ] No raw response body emitted.

## `cq proxy operation`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy operation status`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Read-only existing resilience state; does not create directories, restart processes or resume operations.
- [ ] No operation selected returns idle and exit 0.
- [ ] An existing pending record returns pending and exit 0.
- [ ] A retained action failure still returns result_available, never action-success.
- [ ] Retired operation recover returns exit 4 even when a retained receipt exists.

## `cq proxy rescue`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy rescue enter`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- [ ] Preparation, nested requests and cleanup share one monotonic deadline; none resets or extends the configured timeout.

## `cq proxy rescue exit`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Authenticated loopback transition; persists mode intent and changes traffic admission without changing provider account policy.
- [ ] Preparation, nested requests and cleanup share one monotonic deadline; none resets or extends the configured timeout.

## `cq proxy rescue status`

- [ ] Help and bare parent help are side-effect-free.
- [ ] Reject unknown options, extra arguments, duplicate options and malformed values before state access.
- [ ] The global --json option returns the canonical envelope without changing scope.
- [ ] Authenticated loopback read only; no configuration or mode mutation.
- [ ] Preparation, nested requests and cleanup share one monotonic deadline; none resets or extends the configured timeout.

## `cq proxy serve`

- [ ] No service registration after serve exit.
- [ ] port0 and65536 rejected; existing listener never signalled.
- [ ] JSON emits ready and stopped envelopes as JSONL; logs remain stderr.

## `cq proxy state`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq proxy state initialise`

- [ ] Idempotent same directory does not rotate authority secrets.
- [ ] Conflicting directory fails before config write.

## `cq proxy status`

- [ ] JSON and human modes collect identical facts.
- [ ] No configuration: no files created.
- [ ] --port rejected; legacy proxy status --port maps explicitly to proxy health, never full status.

## `cq service`

- [ ] Bare group, -h and --help produce identical text and exit 0 without state access.

## `cq service install`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.

## `cq service restart`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.
- [ ] Restart disabled token-refresh, verify one newly completed successful run, preserve enabled=false and healthy=false, and exit0.

## `cq service start`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.

## `cq service status`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.

## `cq service stop`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.

## `cq service uninstall`

- [ ] Unknown --dry-run rejected before manager access.
- [ ] Component selection never changes unselected component registration/enabled state.
- [ ] Stop then check leaves disabled state unchanged.
- [ ] Owner conflicts never replace registrations.
- [ ] JSON supported for every action.

## `cq version`

- [ ] version succeeds without home directory or credentials.
