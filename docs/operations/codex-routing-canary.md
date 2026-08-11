# Codex turn-routing canary

HTTP enforcement remains explicit opt-in. WebSocket enforcement remains unavailable until a supported client sends a model-bearing handshake before downstream upgrade.

## Start

1. Run the serving-process HTTP validation mode for the exact installed CQ and client builds. Restart normally and confirm its current readiness marker still matches the configured parser, lease, retry, semantics, and fixture tuple.
2. Disable payload diagnostics. Set `codex_turn_routing` to `enforce`; keep `codex_ws_turn_routing` at `observe`.
3. Run `cq codex canary start` to capture protected-state baselines for the enforced HTTP configuration.
4. Restart CQ once, after active streams drain, then confirm `/health` reports HTTP `effective: enforce`, WebSocket `effective: observe`, and `canary_errors: 0`.

Starting canary after restart does not attach it to the running process. Do not replace an active canary; stop and archive completed evidence first.

Canary state stores counters, timestamps, the exact readiness tuple, and named integrity-protected digests only. The six protected kinds are system auth, account registry, CQ-managed auth, the validated Codex Bar manifest, only its declared auth files, and the parsed routing-default projection. Optional absence is explicit evidence. Canary state never stores account, session, thread, turn, response, path, prompt, or credential values.

## Promotion gate

Keep the installed-service canary active for at least seven elapsed 24-hour periods, with observations on seven consecutive UTC calendar days and at least 100 admitted turns. Exercise:

- one long turn across quota depletion;
- parallel short turns;
- next-turn affinity reuse, necessary reselection, and same-lane supersession;
- terminal routing-default selection when every ordinary route is unavailable;
- a late same-turn hard 429 without alternate dispatch;
- restart while quiescent;
- Codex Bar observation without switching;
- an explicit account switch only after its exact coordinator activation receipt can be bound to the protected-state delta.

Only the running service records observation days. `cq codex canary status` is read-only and cannot create day credit. Missing a service-observed UTC day resets the consecutive-day counter.

Promotion requires the current readiness marker and exact tuple, the installed-listener/build proof, named scenario evidence, and an installed-service rollback receipt proving a drained transition with retained authority, an unseen legacy route, and no shadow promotion. Every failure counter must remain zero, including keyed mismatches, unreceipted protected-state changes, secret leaks, unexplained lifecycles, live-session repairs, protected-source failures, canary write errors, and unproven mutations.

There is no manual baseline-reset command. Until an explicit switch carries an exact coordinator-bound receipt, it invalidates the run. Never infer or acknowledge a protected-state writer from a digest change alone.

## Rollback

1. Capture privacy-safe ownership evidence for the exact listener and installed service.
2. Set the persisted HTTP mode to `observe` or `off`; the current process remains in its already-open enforce epoch.
3. Run `cq codex canary stop`. The current serving process claims the signed intent, closes native admission, drains every admitted request, seals the zero-session final envelope, then exits.
4. Let the installed service restart from the new `observe` or `off` configuration.
5. Invalidate or remove the matching readiness marker only after the replacement listener is ready.
6. Confirm `/health` no longer reports effective enforcement.
7. Keep the authoritative journal until retention expires. Never promote a shadow epoch.

Stopping does not create observation credit. Rollback does not delete the lease journal or rewrite system authentication. Code-only validation does not satisfy the seven-day, 100-turn, installed-listener, named-scenario, activation-receipt, or post-restart rollback gates.
