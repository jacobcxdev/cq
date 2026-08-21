# Codex Request-Shape Telemetry Plan

> **Required subskill:** Use `superpowers:executing-plans` to implement this plan task by task.

**Goal:** Record privacy-safe Codex request-shape facts needed to measure cache continuity across HTTP and long-lived WebSocket traffic.

**Architecture:** Extend existing `RouteEvent` diagnostics with closed request-shape fields. Derive fields only from protocol metadata already parsed at CQ's transport boundary. HTTP requests enrich their existing route event. Each accepted WebSocket request frame emits a distinct `codex_websocket_frame` route event so one long-lived socket cannot collapse many turns into one observation.

**Tech Stack:** Go 1.21+, existing JSONL diagnostics, Gorilla WebSocket, standard `encoding/json`, race-enabled Go tests.

**Spec:** `docs/superpowers/specs/2026-08-21-codex-efficiency-scratchpad.md`

**Global Constraints:** Preserve raw prompts, instructions, tools, schemas, outputs, response IDs, credentials, and account identities outside diagnostics. Persist only closed enums. Treat `previous_response_id` presence as factual lineage, not proof of a standalone/full/incremental request or server cache hit. Keep JSON changes additive. Preserve existing `RouteEvent.Model` semantics. Treat `route_kind` as authoritative transport dimension. Do not touch primary checkout or unrelated proxy-status work.

## Task 1: Parse and classify authoritative shape metadata

**Files:**

- Modify: `internal/proxy/codex_protocol.go`
- Modify: `internal/proxy/codex_protocol_test.go`
- Modify: `internal/proxy/codex_frozen_request.go`
- Modify: `internal/proxy/codex_frozen_request_test.go`
- Create: `internal/proxy/codex_request_shape.go`
- Create: `internal/proxy/codex_request_shape_test.go`

- [x] Add failing parser tests for top-level and WebSocket `params.reasoning.effort`, plus absent effort.
- [x] Add failing frozen-request tests proving `InspectCodexNativeRequest(...).Protocol()` preserves requested effort for both envelope shapes and rejects malformed/duplicate authority consistently.
- [x] Add failing table tests for request lineage: explicit non-empty, empty, and null `previous_response_id` all classify `previous_response_id_present`; absent key classifies `previous_response_id_absent`; parsing/authority failure classifies `unknown`.
- [x] Add failing tests proving closed requested-effort values (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra`) survive, absent effort becomes `unspecified`, and unsupported/caller-controlled values become `unknown`.
- [x] Cover strict-authority duplicates, escaped keys, invalid reasoning/effort types, and unsupported values without leaking caller bytes.
- [x] Run `go test -race -count=1 ./internal/proxy -run 'TestParseCodexProtocolRequest|TestCodexRequestShape'` and confirm failure is missing behaviour, not broken fixtures.
- [x] Extend protocol parsing with reasoning effort for both envelope shapes.
- [x] Extend the frozen-authority scanner with the same requested-effort field without reparsing or retaining raw input.
- [x] Add one internal shape classifier with closed lineage and requested-effort values. Do not add session/body correlation.
- [x] Re-run focused tests to green.

## Task 2: Extend diagnostics without weakening privacy

**Files:**

- Modify: `internal/proxy/diag.go`
- Modify: `internal/proxy/diag_test.go`
- Modify: `internal/proxy/codex_request_shape.go`
- Modify: `internal/proxy/codex_request_shape_test.go`
- Modify: `internal/proxy/server_codex_diagnostics_test.go`

- [x] Add failing tests for additive JSON fields `request_lineage`, `requested_reasoning_effort`, `requested_model_class`, and `compaction_phase`.
- [x] Add exact legacy JSON-byte coverage plus tolerant old/new decoder fixtures proving `omitempty` compatibility.
- [x] Replace manual unsafe-field coverage with automatic coverage tied to every `RouteEvent` string field. Arbitrary values in every new field must fail closed, write zero bytes, and increment the canary.
- [x] Add failing projection tests for fixed `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna` requested-model classes. Preserve existing coarse `model` projection unchanged; use `other`/`unknown` in separate field.
- [x] Add shape tests for model class, validated request kind, and compaction phase: supported phase, `not_applicable` for validated non-compaction, `unknown` for absent or weak metadata.
- [x] Add failing writer test proving shape events contain no raw model, session, response, or effort bytes.
- [x] Run focused diagnostics tests and confirm RED.
- [x] Add fields to `RouteEvent` and request-scoped diagnostics with `omitempty` compatibility.
- [x] Add closed validators and model projection constants.
- [x] Do not derive or persist session/response correlation from request bodies.
- [x] Re-run focused diagnostics tests to green.

## Task 3: Wire one observation per real request

**Files:**

- Modify: `internal/proxy/codex_request_shape.go`
- Modify: `internal/proxy/codex_legacy_native_http.go`
- Modify: `internal/proxy/codex_compact.go`
- Modify: `internal/proxy/codex_http_request_plan.go`
- Modify: `internal/proxy/codex_ws_request.go`
- Modify: `internal/proxy/codex_ws_broker.go`
- Modify: `internal/proxy/codex_stage11_lifecycle_corpus_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/codex_http_request_plan_test.go`
- Modify: `internal/proxy/codex_ws_broker_test.go`
- Modify: `internal/proxy/server_test.go`

- [x] Add failing ordinary legacy HTTP tests proving off, observe, and enforce modes record one shape when optional components are nil/present, plus native and compact handler persistence.
- [x] Add failing native-plan test proving authoritative HTTP preparation enriches request-local diagnostics only for HTTP transport; existing request retries overwrite fields but never emit.
- [x] Add failing terminating-broker test proving each accepted request frame invokes the shape sink exactly once, including prewarm followed by a request carrying `previous_response_id`.
- [x] Add failing legacy WebSocket tests proving off-mode traffic emits one fresh shape event per accepted frame with no observer, and enabling an observer does not double-emit.
- [x] Add failing server diagnostics test proving two request frames on one socket yield two `codex_websocket_frame` events rather than one last-frame snapshot.
- [x] Add retry/rejection tests proving planner retries and upstream rotations do not duplicate telemetry; malformed, control, response, or unsupported legacy frames emit none.
- [x] Run focused tests and confirm RED.
- [x] Add one small shape-to-observation helper; every producer uses it rather than duplicating field assignment.
- [x] Parse once and enrich request-local diagnostics in ordinary legacy and compact fallback HTTP handlers before optional enforcement/observation. In native plan factory, enrich immediately after strict protocol inspection only when transport is HTTP. Keep existing terminal server handlers as sole HTTP writers.
- [x] Give each terminating WebSocket frame fresh diagnostics. Invoke one acceptance sink after successful build/adoption or dispatch/reservation, before dial/relay loops; retries and rotations must not emit.
- [x] Install a WebSocket frame sink in server context. Emit fixed-field `RouteEvent` values through existing projection and safety boundary. In legacy relay, strictly accept only text `type=response.create` or JSON-RPC `method=response/create` client frames before parse/apply/emit; never inspect upstream frames.
- [x] Keep existing connection-level WebSocket event unchanged; frame events remain distinguishable by authoritative `route_kind` in both legacy and terminating paths.
- [x] Re-run focused tests to green under `-race`.

## Task 4: Record evidence and verify whole repository

**Files:**

- Modify: `docs/superpowers/specs/2026-08-21-codex-efficiency-scratchpad.md`

- [x] Update Phase 0 ledger with exact delivered fields, factual lineage semantics, WebSocket event cardinality, and deferred unsupported fields.
- [x] Run `gofmt` on changed Go files.
- [x] Run `go vet ./...`.
- [x] Focused proxy race tests passed during implementation tasks, including the Stage 11 fallout fix.
- [x] Run `go test -race -count=1 ./...`.
- [x] Run `go build ./...`.
- [x] Inspect `git diff --check`, focused diff, and worktree status.
- [x] Commit docs locally with a conventional, past-tense message. Do not push or open a PR without user authority.
