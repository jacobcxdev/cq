# Codex turn-routing canary

HTTP enforcement remains explicit opt-in. WebSocket enforcement remains unavailable until a supported client sends a model-bearing handshake before downstream upgrade.

## Start

1. Run HTTP validation against installed build and local test upstream.
2. Run `cq codex canary start` to capture protected-file baselines before enforcement.
3. Set `codex_turn_routing` to `enforce`; keep `codex_ws_turn_routing` at `observe`.
4. Restart CQ once, after active streams drain, then confirm `/health` reports HTTP `effective: enforce`, WebSocket `effective: observe`, and `canary_errors: 0`.

Starting canary after restart does not attach it to running process. Do not replace active canary; stop and archive completed evidence first.

Canary state stores counters, timestamps, build/schema/fixture tuple, and SHA-256 digests only. It never stores account, session, thread, turn, response, path, prompt, or credential values.

## Promotion gate

Keep canary active for seven consecutive calendar days and at least 100 admitted turns. Exercise:

- one long turn across quota depletion;
- parallel short turns;
- next-turn reselection and same-lane supersession;
- restart while quiescent;
- explicit account switch followed by `cq codex canary acknowledge-explicit-switch`;

Run `cq codex canary status` at least once each UTC calendar day. Admitted turns also count as observations. Missing a calendar day resets consecutive-day counter.
- Codex Bar observation without switching.

`cq codex canary status` must finish with zero keyed mismatches, automatic auth/registry hash changes, secret leaks, and unexplained lifecycles. Explicit switch is sole permitted system-auth/registry change and must be acknowledged immediately.

## Rollback

1. Set HTTP mode to `observe` or `off`.
2. Restart CQ; drain existing requests first.
3. Invalidate/remove matching readiness marker only after restart is ready.
4. Confirm `/health` no longer reports effective enforcement.
5. Keep authoritative journal until retention expires. Never promote shadow epoch.

Run `cq codex canary stop` after collecting privacy-safe evidence. Rollback does not delete lease journal or rewrite system authentication.
