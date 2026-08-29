# Pool Identity and Aggregate Labels Design

- **Status:** Draft for review
- **Date:** 2026-08-29
- **Scope:** Routing-pool identity, policy migration, pool CLI, and TTY aggregate labels

## Outcome

Routing pools gain immutable internal UUIDs and mutable display names. Runtime
policy, session bindings, capability routing, and dispatch permits use UUIDs.
Users continue to create, select, inspect, and rename pools by name. UUIDs do
not appear in CLI arguments, normal CLI output, or public JSON.

TTY output renders a pool's name alone. It never prefixes the name with
`Proxy`. Provider, provider-aggregate, and pool-aggregate labels share one
right-aligned column sized from the longest label in the current report. CQ
does not truncate or impose a display-width cap.

For the current configuration, aggregate headers become:

```text
    Codex 3 × pro 20x = 60x
    Cyber 2 × pro 20x = 40x
```

Longer pool names move shorter labels right so every aggregate summary begins
in the same column.

## Non-goals

- Expose pool UUIDs as user-facing selectors or output fields.
- Render `Proxy`, `Pool`, or another type prefix beside a pool name.
- Truncate long pool names or add a configurable label-width limit.
- Allow ambiguous duplicate display names.
- Refactor unrelated routing, quota aggregation, or terminal layout.

## Public naming contract

Pool names are user-facing identifiers at every CLI boundary. Names:

- preserve user-supplied casing exactly;
- must be valid UTF-8, non-empty after trimming, and contain no control
  characters;
- have no separate character-count limit; the existing 1 MiB policy-body
  limit remains the aggregate input bound;
- are unique under case-insensitive comparison.

Case-insensitive uniqueness lets users operate entirely by name without
exposing internal IDs. A case-only rename of one pool is valid. A rename to
another pool's name is rejected.

Legacy schema-v1 names are lowercase ASCII slugs. Automatic migration
capitalises their first character for display, so `cyber` becomes `Cyber`.
Subsequent renames store exactly what the user enters, allowing names such as
`R&D` without renderer-side rewriting.

## CLI contract

Existing name-based commands remain ergonomic:

```text
cq proxy policy pool set Cyber --account ACCOUNT [--account ACCOUNT ...]
cq proxy policy session bind --pool Cyber SELECTOR
```

`pool set` resolves a case-insensitive name match. When found, it replaces
membership while preserving the stored display spelling and internal UUID.
When absent, it creates a pool with a new UUID and stores the supplied name.

One command is added:

```text
cq proxy policy pool rename OLD_NAME NEW_NAME [--port PORT]
```

`rename` resolves exactly one existing pool by case-insensitive name, validates
the new name, changes only its display name, increments normal policy
generations, and publishes through the existing authenticated control path.
Pool membership, session bindings, capability routing, and immutable identity
remain unchanged.

Shell quoting handles spaces in names. Examples:

```text
cq proxy policy pool rename Cyber "Security Research"
cq proxy policy session bind --pool "Security Research" --digest DIGEST
```

`policy status`, `session list`, `session show`, `cq check --json`, and TTY
output project UUID references back to current names. They never emit a pool
UUID. Error messages also identify pools by name.

## Internal identity model

Internal policy schema v2 introduces a distinct `PoolID` domain type. Its wire
form is a lowercase RFC 4122 UUID. New IDs use random version-4 UUIDs generated
from the already injected resilience-state random source.

Conceptually, schema v2 contains:

```text
AccountPoolV2 {
  ID      PoolID
  Name    string
  Members []AccountKey
}

SessionBindingV2 {
  SessionDigest string
  PoolID        PoolID
}

RoutingPolicyV2 {
  Pools           []AccountPoolV2
  SessionBindings []SessionBindingV2
  CapabilityPool  PoolID
  ...
}
```

Policy indexes are keyed by `PoolID`. A separate case-folded name index exists
only for boundary resolution and uniqueness validation. Runtime session-policy
decisions, capability-policy snapshots, request-plan inputs, and caller
dispatch-permit requests carry `PoolID`. Name changes therefore cannot alter
routing authority or detach a binding.

Dispatch permits are authenticated, one-use, and valid for five seconds. New
permits carry `PoolID`. Legacy permit files are not migrated: service restart
outlives their validity, and the new reader rejects their old schema. Durable
lease journals retain only the opaque permit digest, so they gain no pool-name
dependency.

## Public policy projection

Current CLI policy documents use names. That remains the public contract even
though persisted authority uses UUIDs.

The CLI/control boundary translates in both directions:

- status and session inspection join internal `PoolID` references to names;
- session binding resolves `--pool NAME` before publishing;
- `pool set` retains an existing UUID when its name resolves;
- new pools receive IDs internally;
- apply resolves every name reference within the submitted document before
  publishing an internal policy.

An apply document cannot express rename intent because it contains no UUID.
If a name disappears and another appears, CQ treats that as removal and
creation. Users must use `pool rename` when identity and bindings must survive.
CQ rejects apply input containing unresolved, missing, or duplicate names.

Internal UUIDs may cross the authenticated loopback control channel between CQ
components, but command output and documented JSON do not expose them.

## Automatic migration

Routing-policy open authenticates and validates the existing schema-v1 object
before migration. While holding the existing routing-policy ownership lock, CQ:

1. generates one UUID for each legacy pool;
2. changes the first character of each legacy display name to uppercase;
3. rewrites session bindings and capability-pool references through the
   validated old-name-to-ID map;
4. preserves account membership, capability evidence, predicates,
   delegations, and authority/routing/effective generations;
5. validates and seals one schema-v2 object;
6. publishes the immutable object and replaces the authenticated anchor using
   exact-prior CAS.

The migration changes representation, not routing meaning, so generations do
not advance. Future user mutations advance generations normally.

Failure before anchor replacement leaves schema v1 authoritative. A published
but unselected schema-v2 object is harmless. Failure after successful CAS means
schema v2 is authoritative and complete. Reopen verifies the selected object's
digest, MAC, schema, IDs, references, and name uniqueness before routing starts.

Migration failure stops policy-backed routing startup rather than serving a
partially translated policy. Error messages contain no UUID, account key,
session digest, MAC, or raw policy body.

Schema-v2 state is not readable by older CQ releases. Release qualification
must therefore snapshot the selected schema-v1 policy and anchor before first
upgrade start, prove automatic migration against a copied state root, and
prove downgrade by restoring that snapshot before an older binary opens it.
Normal migration never deletes old immutable objects.

## TTY layout

`BuildTTYModel` computes label width before building headers. Width is maximum
visible width across:

- every rendered provider name used by account headers;
- every rendered provider-aggregate name;
- every rendered pool display name.

Existing seven-column padding remains only as minimum compatibility width when
all labels are shorter. Every header builder receives the shared width and
right-aligns its label. Summary text, plan text, and email text then begin one
space after the common trailing edge.

Pool aggregate headers use the pool name only. Generic proxy-eligibility
aggregates, when a proper subset exists without a named pool, retain `Proxy`
because no pool name exists to describe that aggregate. Named pool blocks never
render `Proxy`.

Separator measurement continues after headers are built, so longer names widen
separators through the existing visible-width calculation. ANSI styling does
not affect alignment.

## Quota/report projection

Policy-to-report projection keys bound pools by `PoolID`, resolves their
current names, and emits one aggregate for each bound named subset. Rename
therefore changes only the next display projection. Account eligibility,
membership, aggregate values, and session routing remain identical.

All-accounts proxy eligibility remains hidden when it duplicates the provider
aggregate. Named subset pools remain visible.

## Validation and errors

Policy validation rejects:

- malformed, nil, or duplicate pool UUIDs;
- empty or control-bearing names;
- case-insensitive duplicate names;
- empty or duplicate members;
- bindings or capability references to missing UUIDs;
- v2 policy objects containing legacy name references;
- migration input that fails existing v1 authentication or validation.

CLI rename rejects missing old names, duplicate target names, invalid target
names, unknown options, and failed policy publication. No local file fallback
bypasses the live control path when live mode is selected.

## Verification

Development follows red-green TDD.

### Policy and migration

- Open the current name-keyed policy fixture and prove UUIDs are assigned once.
- Prove `cyber` becomes `Cyber` and every binding/capability reference points to
  the same migrated pool.
- Reopen migrated state and prove IDs remain stable.
- Inject failures before object publication, after object publication, and
  before/after anchor CAS; prove one complete policy is always authoritative.
- Reject malformed IDs, duplicate IDs, duplicate names, and dangling refs.
- Prove MAC validation covers IDs and UUID references.
- Prove copied-state migration leaves production state untouched.

### CLI

- Create and update pools by name while retaining hidden identity.
- Rename a pool and prove memberships and bindings survive.
- Support case-only rename and quoted names.
- Reject ambiguous/missing/colliding names.
- Prove status, session inspection, errors, and JSON contain names but no UUIDs.
- Prove name-based apply compiles references and treats remove-plus-add as new
  identity.

### Runtime

- Route HTTP and WebSocket sessions through a pool before and after rename.
- Prove capability routing, authenticated continuation, dispatch permits, and
  routing-generation fences use pool IDs.
- Prove old five-second permit records cannot be consumed by the new schema.

### Renderer

- Reproduce current `Codex` versus `Proxy cyber` misalignment as a failing test.
- Prove named pool output is `Cyber`, with no `Proxy` prefix.
- Prove provider/account/aggregate labels align against a pool name longer than
  seven columns.
- Prove no truncation occurs and separators widen accordingly.
- Preserve generic unnamed `Proxy` subset behavior.

### Release gate

- Run focused package tests with `-race`.
- Run `go build ./...`, `go vet ./...`, and `go test -race -count=1 ./...`.
- Run isolated normal, repeated, compaction, rescue, and handoff validation
  against copied migrated state.
- Upgrade installed CQ, verify migration and name-only output, then complete a
  real Codex request through the installed listener.
- Restore the pre-migration snapshot with the prior binary in isolation and
  prove downgrade recovery before production cutover.

