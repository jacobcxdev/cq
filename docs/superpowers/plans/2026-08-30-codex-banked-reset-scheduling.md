# Codex Banked Reset Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add portfolio-wide Codex banked-reset recommendations plus explicit, retry-safe CLI consumption for every account visible through `cq codex accounts`.

**Architecture:** Keep upstream `/wham` transport and credential access inside `internal/provider/codex`; keep burn-history correction inside `internal/history`; keep percentage-only simulation and deterministic branch-and-bound inside `internal/aggregate`; orchestrate list/recommend/use in `internal/app`; expose only user-directed commands and public result types from `cmd/cq`. Recommendation fetches fresh usage and credits for one shared account snapshot, never calls consume, and becomes non-actionable when any portfolio input is missing. Consumption resolves one exact credential generation, persists deterministic idempotency state before POST, and never changes natural reset epochs.

**Tech Stack:** Go 1.21+, Kong CLI, `httputil.Doer`, `httputil.ReadBody`, credential coordinator exact-resolution interfaces, `fsutil`, file-backed cache/history, table-driven Go tests, `go test -race`.

**Spec:** `docs/superpowers/specs/2026-08-30-codex-banked-reset-scheduling-design.md`

## Global Constraints

- Treat upstream commit `b8c86376a258e55efc8e5ecfbabc21c16c07d814` as contract source. Keep `/wham` details isolated in one client.
- Keep `recommend` advisory-only. No recommendation path may call consume, credential activation, adoption, login persistence, removal, system projection, or shell execution.
- Make `recommend` portfolio-wide across every account projected by `cq codex accounts`; do not add account selector.
- Make `use` require account reference and default omitted `--credit` to oldest unresolved attempt, then earliest-expiring eligible credit.
- Preserve natural 5-hour and 7-day reset epochs when applying simulated or real banked resets. Only remaining percentages return to 100.
- Refresh only CQ-owned managed credential lineage, through existing coordinator broker, and only after authentication failure. Never refresh system, borrowed, exported, external, legacy, or uncertain credentials.
- Keep tokens, revisions, candidate IDs, internal account keys, and credential material out of public output, errors, logs, recommendation objects, and attempt filenames.
- Use red-green TDD. Every Go test command includes `-race -count=1`.
- Keep changes surgical. No proxy routing, account switching, aggregate gauge, or unrelated credential refactor.

## File Map

- `internal/provider/codex/reset_credits.go`: upstream `/wham` DTOs, validation, bounded list/consume HTTP.
- `internal/provider/codex/reset_accounts.go`: shared visible account snapshot, exact credential resolution, eligible managed refresh retry.
- `internal/provider/codex/reset_attempts.go`: deterministic idempotency key, immutable pending journal, optional-credit selection.
- `internal/provider/codex/inventory.go` and `accounts.go`: sanitised refresh-eligibility bit and shared account projection.
- `internal/history/store.go`: unchanged-epoch upward-correction censoring and rich EWMA metadata.
- `internal/aggregate/reset_schedule.go`: generic portfolio state, event simulation, objective comparison, branch-and-bound schedule.
- `internal/app/codex_resets.go`: best-effort list, completeness gate, recommendation mapping, two-phase manual use.
- `cmd/cq/codex_resets.go`: production dependency wiring, confirmation, TTY/JSON rendering.
- `cmd/cq/main.go`, `help.go`, and `README.md`: command tree, dispatch, manual help, public command roster.

---

### Task 1: Add bounded reset-credit transport and domain types

**Files:**

- Create: `internal/provider/codex/reset_credits.go`
- Create: `internal/provider/codex/reset_credits_test.go`

**Interfaces:**

- Consumes: `httputil.Doer`, `CredentialMaterial`, caller `context.Context`.
- Produces:

```go
type ResetType string
type ResetCreditStatus string
type ConsumeResetOutcome string

const (
	ResetTypeCodexRateLimits ResetType = "codex_rate_limits"
	ResetCreditAvailable    ResetCreditStatus = "available"
	ResetCreditRedeeming    ResetCreditStatus = "redeeming"
	ResetCreditRedeemed     ResetCreditStatus = "redeemed"
	ConsumeReset            ConsumeResetOutcome = "reset"
	ConsumeAlreadyRedeemed  ConsumeResetOutcome = "already_redeemed"
	ConsumeNothingToReset   ConsumeResetOutcome = "nothing_to_reset"
	ConsumeNoCredit         ConsumeResetOutcome = "no_credit"
)

type ResetCredit struct {
	ID          string
	ResetType   ResetType
	Status      ResetCreditStatus
	GrantedAt   time.Time
	ExpiresAt   *time.Time
	Title       string
	Description string
}

type ResetCreditInventory struct {
	Credits        []ResetCredit
	AvailableCount int
	EntryErrors    []ResetCreditEntryError
}

type ResetCreditEntryError struct {
	Index int
	Code  string
}

type ResetHTTPError struct { Status int }

type ConsumeResetResult struct {
	Outcome      ConsumeResetOutcome
	WindowsReset int64
}

type ResetCreditClient struct { HTTP httputil.Doer }

func (c ResetCreditClient) List(context.Context, CredentialMaterial) (ResetCreditInventory, error)
func (c ResetCreditClient) Consume(context.Context, CredentialMaterial, string, string) (ConsumeResetResult, error)
```

- `ResetCreditEntryError` carries index and stable code only; `ResetHTTPError` carries status only. Neither stores response body, token, account ID, or request headers.
- A partially valid list returns `ResetCreditInventory` plus typed validation error. `list` may render valid entries and error; `recommend` and default selection treat any validation error or `EntryErrors` as unusable inventory.

- [ ] **Step 1: Add failing GET contract tests**

Use injected `httputil.Doer` to capture request. Cover exact method/path, bearer and `ChatGPT-Account-Id` headers, 5-second child deadline, RFC 3339 timestamps, nil expiry, unknown status/reset type preservation, invalid IDs/timestamps, negative counts, contradictory available count, non-2xx typed status, cancellation, and body larger than 1 MiB.

```go
func TestResetCreditClientListUsesWhamContract(t *testing.T) {
	client := ResetCreditClient{HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-1" {
			t.Fatalf("ChatGPT-Account-Id = %q", got)
		}
		return jsonResponse(http.StatusOK, `{"available_count":1,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-30T08:00:00Z","expires_at":"2026-08-31T08:00:00Z","title":"Reset","description":"One reset"}]}`), nil
	})}

	got, err := client.List(context.Background(), CredentialMaterial{AccessToken: "access-token", AccountID: "acct-1"})
	if err != nil || len(got.Credits) != 1 || got.Credits[0].ID != "credit-1" {
		t.Fatalf("List() = %+v, %v", got, err)
	}
}
```

- [ ] **Step 2: Add failing POST contract tests**

Cover exact method/path, JSON body fields, content type, 10-second child deadline, all four known backend `code` values mapped to CQ outcomes, `windows_reset`, unknown code rejection, malformed 2xx response, `401`, `5xx`, cancellation, and bounded body.

```go
func TestResetCreditClientConsumeUsesStableRequest(t *testing.T) {
	client := ResetCreditClient{HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			RedeemRequestID string `json:"redeem_request_id"`
			CreditID        string `json:"credit_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.RedeemRequestID != "cq-reset-v1-key" || body.CreditID != "credit-1" {
			t.Fatalf("body = %+v", body)
		}
		return jsonResponse(http.StatusOK, `{"code":"reset","windows_reset":2}`), nil
	})}

	got, err := client.Consume(context.Background(), CredentialMaterial{AccessToken: "access-token", AccountID: "acct-1"}, "credit-1", "cq-reset-v1-key")
	if err != nil || got.Outcome != ConsumeReset || got.WindowsReset != 2 {
		t.Fatalf("Consume() = %+v, %v", got, err)
	}
}
```

- [ ] **Step 3: Confirm red**

Run: `go test -race -count=1 ./internal/provider/codex -run 'TestResetCreditClient'`

Expected: compile failure because reset-credit types and client do not exist.

- [ ] **Step 4: Implement minimum transport**

Use fixed production URLs, `http.NewRequestWithContext`, `context.WithTimeout`, `httputil.ReadBody(response.Body)`, strict top-level JSON decoding, and explicit validation. Do not retry HTTP inside transport. Preserve unknown status/reset-type strings only for list visibility; reject unknown consume outcomes.

```go
const (
	resetCreditsURL     = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	consumeResetURL     = resetCreditsURL + "/consume"
	resetListTimeout    = 5 * time.Second
	resetConsumeTimeout = 10 * time.Second
)

func addResetHeaders(req *http.Request, material CredentialMaterial) {
	req.Header.Set("Authorization", "Bearer "+material.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", material.AccountID)
}
```

- [ ] **Step 5: Confirm green**

Run: `go test -race -count=1 ./internal/provider/codex -run 'TestResetCreditClient'`

- [ ] **Step 6: Commit**

```text
feat: added reset-credit transport

- added bounded list and consume requests
- validated reset-credit schemas and outcomes
```

### Task 2: Share visible account inventory and exact credential access

**Files:**

- Create: `internal/provider/codex/reset_accounts.go`
- Create: `internal/provider/codex/reset_accounts_test.go`
- Modify: `internal/provider/codex/inventory.go`
- Modify: `internal/provider/codex/inventory_test.go`
- Modify: `internal/provider/codex/accounts.go`
- Modify: `internal/provider/codex/accounts_test.go`
- Modify: `internal/app/accounts.go`
- Modify: `internal/app/accounts_test.go`

**Interfaces:**

- Consumes: `CredentialInventory.List`, `ResolveAccountReference`, `PlanCandidate`, `ResolvePlannedCandidate`, `ExactSecretResolver`, optional `CredentialRefreshBroker`.
- Produces:

```go
type ResetAccount struct {
	AccountKey AccountKey `json:"-"`
	AccountID  string
	Email      string
	PlanType   string
	Active     bool
	planned    PlannedCandidate
	refreshable bool
}

type ResetAccountSnapshot struct {
	Inventory Inventory
	Aliases   AccountAliasIndex
	Accounts  []ResetAccount
}

type ResetBackend struct {
	Inventory CredentialInventory
	Resolver  ExactSecretResolver
	Refresh   CredentialRefreshBroker
	Aliases   func() (AccountAliasIndex, error)
	Credits   ResetCreditClient
	Now       func() time.Time
}

type Accounts struct {
	FS        fsutil.FileSystem
	Admin     CredentialAdmin
	Inventory CredentialInventory
	Now       func() time.Time
}

func ProjectVisibleAccounts(Inventory, time.Time) []ResetAccount
func (b *ResetBackend) Snapshot(context.Context) (ResetAccountSnapshot, error)
func (s ResetAccountSnapshot) ResolveReference(string) (ResetAccount, error)
func (b *ResetBackend) ListCredits(context.Context, ResetAccount) (ResetCreditInventory, error)
func (b *ResetBackend) Consume(context.Context, ResetAccount, string, string) (ConsumeResetResult, error)
```

- Add `RefreshEligible bool` to `CredentialCandidate`. Derive it during managed-record discovery only when metadata is schema v1, provenance is `cq_oauth`, ownership is `cq_owned_never_exported`, operation is ready with no operation ID, refresh is not suspended, and refresh token exists. Sanitisation preserves this boolean while clearing credential material.
- `ResetBackend` resolves exact material immediately before each request. On `401` only, it may call `Refresh` when selected candidate is `SourceManaged`, CQ-authored, and broker-eligible; it then relists, replans same strong identity, resolves exact generation, and retries once.
- `Accounts.Discover` projects `provider.Account` from `ProjectVisibleAccounts`, so account listing and reset eligibility cannot drift.
- Visibility means each logical account with at least one non-dispatch-blocked candidate, including expired candidates and currently unroutable logical accounts. Projection chooses deterministic first candidate but does not hide account because token is expired. Exact resolution or upstream `401` later returns `auth_expired` when no allowed refresh succeeds.

- [ ] **Step 1: Add failing projection parity tests**

Create one sanitised inventory containing active system, CQ-managed, external, unroutable-but-visible, unstable, duplicate-email, and blocked candidates. Prove projected account order and metadata match `Accounts.Discover`; secrets remain absent; reference resolution accepts unique email, alias, and opaque key; unstable/ambiguous references return `account_reference_invalid` mapping.

```go
func TestProjectVisibleAccountsMatchesAccountListing(t *testing.T) {
	snapshot := resetInventoryFixture()
	resetAccounts := ProjectVisibleAccounts(snapshot, fixedNow)
	listed := projectProviderAccounts(resetAccounts)
	if !reflect.DeepEqual(wantVisibleProviderAccounts(), listed) {
		t.Fatalf("visible account projection = %+v, want %+v", listed, wantVisibleProviderAccounts())
	}
}
```

- [ ] **Step 2: Add failing exact-resolution and refresh-boundary tests**

Prove list and consume call `ResolvePlannedCandidate`; stale revisions replan once; identity mismatch fails; system/external/borrowed/exported/legacy/uncertain credentials never call refresh; only CQ-owned managed lineage retries once after `401`; non-401 errors never refresh; no method can reach `Activate`, `Adopt`, `SaveLogin`, `RemoveManaged`, or system projection.

```go
func TestResetBackendRefreshesOnlyCQOwnedManagedAfter401(t *testing.T) {
	refresh := &recordingRefreshBroker{}
	backend := resetBackendFixture(SourceManaged, true, refresh, []int{http.StatusUnauthorized, http.StatusOK})
	account := backendSnapshotAccount(t, backend)
	if _, err := backend.ListCredits(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}
```

- [ ] **Step 3: Confirm red**

Run: `go test -race -count=1 ./internal/provider/codex ./internal/app -run 'Test(ProjectVisibleAccounts|ResetBackend|RunAccounts.*Codex)'`

Expected: compile/test failure because shared reset account projection and backend do not exist.

- [ ] **Step 4: Implement shared projection and backend**

Select candidate with existing `ResolveCandidate(logical, "", now)` ordering. Store only secret-free `PlannedCandidate` metadata in snapshot. Map boundary failures to typed reset errors without including user-supplied reference or credential metadata. Keep refresh retry in one helper called by both list and consume.

```go
func (b *ResetBackend) resolve(ctx context.Context, account ResetAccount) (CredentialMaterial, ResetAccount, error) {
	material, planned, err := ResolvePlannedCandidate(ctx, b.Inventory, b.Resolver, account.planned)
	if err != nil {
		return CredentialMaterial{}, account, classifyResetCredentialError(err)
	}
	account.planned = planned
	return material, account, nil
}
```

Update `RunAccounts(provider.Codex, ...)` wiring to open existing `CredentialControl`, pass it as inventory, and close it. Do not mutate credentials or active projection.

- [ ] **Step 5: Confirm green**

Run: `go test -race -count=1 ./internal/provider/codex ./internal/app -run 'Test(ProjectVisibleAccounts|ResetBackend|RunAccounts.*Codex)'`

- [ ] **Step 6: Commit**

```text
refactor: shared Codex account projection

- aligned account listing with reset eligibility
- added exact read-only credential resolution
```

### Task 3: Preserve burn estimates across banked resets

**Files:**

- Modify: `internal/history/store.go`
- Modify: `internal/history/store_test.go`
- Modify: `internal/app/report.go`
- Modify: `internal/app/runner_test.go`

**Interfaces:**

- Preserve existing `BurnRates` and `UpdateAndGetBurnRates` callers.
- Add rich estimates for recommendation confidence:

```go
type RateEstimate struct {
	RatePctPerS float64
	Samples     int
	LastSeenUnix int64
}

type RateEstimates map[BurnRateKey]RateEstimate

func (r RateEstimates) Get(BurnRateKey) (RateEstimate, bool)
func (s *Store) UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (BurnRates, RateEstimates, error)
```

- Add `LastRemainingPctExact *float64` to `WindowState` with `omitempty`. Keep schema version and filename unchanged: existing state decodes this additive field as nil without losing EWMA or samples. Keep `UpdateAndGetBurnRates` as wrapper over `UpdateAndGetEstimates`.

- [ ] **Step 1: Add failing unchanged-epoch upward-correction test**

Seed at least two observations to create non-zero EWMA. Submit higher current remaining percentage with identical `ResetAtUnix`. Prove EWMA and sample count are unchanged while last-seen, remaining, and reset epoch advance.

```go
func TestStoreBankedResetPreservesEstimate(t *testing.T) {
	store := newHistoryFixture(t)
	seedEstimate(t, store, 80, 70, resetAt)
	before := readWindowState(t, store)

	_, estimates, err := store.UpdateAndGetEstimates(context.Background(), resultBatch(95, resetAt), observation3)
	if err != nil {
		t.Fatal(err)
	}
	after := readWindowState(t, store)
	if after.EWMARatePctPerS != before.EWMARatePctPerS || after.Samples != before.Samples {
		t.Fatalf("estimate changed across banked reset: before=%+v after=%+v", before, after)
	}
	if estimate, ok := estimates.Get(testBurnRateKey()); !ok || estimate.Samples != before.Samples {
		t.Fatalf("estimate = %+v, %t", estimate, ok)
	}
}
```

- [ ] **Step 2: Add regression tests**

Cover benign upward correction, exact-percent upward correction where rounded integer stays equal, natural reset with clean epoch advancement, backward/ambiguous epoch reseed, exhausted-zero censoring, first sample omission, stale-cache omission, persistence reopen, and existing Runner wrapper behaviour.

- [ ] **Step 3: Confirm red**

Run: `go test -race -count=1 ./internal/history ./internal/app -run 'Test(StoreBankedReset|StoreUpwardCorrection|StoreNaturalReset|Runner.*History)'`

Expected: compile failure for rich estimates and behavioural failure because current path blends a zero-rate sample.

- [ ] **Step 4: Implement correction before delta sampling**

Use `RemainingPctExact` when both observations provide it; otherwise use integer percentage. Detect upward movement only when reset epoch is unchanged. Update snapshot fields and `continue` before `computeDelta`, alpha, or `Samples++`.

```go
if w.ResetAtUnix == prev.LastResetAtUnix && remainingIncreased(prev, w) {
	prev.LastSeenUnix = nowEpoch
	prev.LastRemainingPct = w.RemainingPct
	prev.LastRemainingPctExact = cloneExact(w.RemainingPctExact)
	prev.LastResetAtUnix = w.ResetAtUnix
	continue
}
```

When both previous and current exact percentages exist, compare exact values. Otherwise compare integer percentages. Persist a cloned current exact pointer on seeding, ordinary samples, reseeds, natural resets, and upward corrections. Existing history with nil exact percentage continues through integer comparison; it never cold-starts.

- [ ] **Step 5: Return estimates without breaking gauge callers**

Build `RateEstimates` from every state with `Samples >= 2`; return EWMA, sample count, and last-seen time. Existing `BurnRates` remains same values and keys.

- [ ] **Step 6: Confirm green**

Run: `go test -race -count=1 ./internal/history ./internal/app -run 'Test(StoreBankedReset|StoreUpwardCorrection|StoreNaturalReset|Runner.*History)'`

- [ ] **Step 7: Commit**

```text
fix: preserved burn rates across resets

- censored unchanged-epoch upward corrections
- exposed sample metadata for scheduling confidence
```

### Task 4: Build percentage-only portfolio simulation

**Files:**

- Create: `internal/aggregate/reset_schedule.go`
- Create: `internal/aggregate/reset_schedule_test.go`

**Interfaces:**

- Consumes only generic, secret-free values; `internal/aggregate` must not import Codex provider package.
- Produces:

```go
type ResetScheduleConfidence string
type ResetScheduleStatus string
type ResetScheduleReason string
type ResetRateSource string

const (
	ResetConfidenceHigh ResetScheduleConfidence = "high"
	ResetConfidenceLow  ResetScheduleConfidence = "low"

	ResetDueNow      ResetScheduleStatus = "due_now"
	ResetScheduled   ResetScheduleStatus = "scheduled"
	ResetDeferred    ResetScheduleStatus = "deferred"
	ResetNotUseful   ResetScheduleStatus = "not_yet_useful"
	ResetUnsupported ResetScheduleStatus = "unsupported"

	ResetReasonGapAvoidance    ResetScheduleReason = "gap_avoidance"
	ResetReasonExpiryPressure  ResetScheduleReason = "expiry_pressure"
	ResetReasonNaturalReset    ResetScheduleReason = "natural_reset_interaction"
	ResetReasonWasteReduction  ResetScheduleReason = "waste_reduction"
	ResetReasonRateFallback    ResetScheduleReason = "low_confidence_fallback"

	ResetRateEWMA         ResetRateSource = "ewma"
	ResetRateCycleAverage ResetRateSource = "cycle_average"
)

type ResetScheduleInput struct {
	Now      time.Time
	Accounts []ResetScheduleAccountInput
}

type ResetScheduleAccountInput struct {
	Key        string
	Email      string
	AccountID  string
	Multiplier int
	Windows    map[quota.WindowName]ResetScheduleWindowInput
	Credits    []ResetScheduleCreditInput
}

type ResetScheduleWindowInput struct {
	RemainingPct float64
	ResetAt      time.Time
	Period       time.Duration
	BurnPctPerS  float64
	RateSource   ResetRateSource
	Samples      int
}

type ResetScheduleCreditInput struct {
	ID        string
	GrantedAt time.Time
	ExpiresAt *time.Time
	Supported bool
}

type ResetScheduleObjective struct {
	UnmetDemandPctSeconds float64
	GapDurationSeconds    int64
	UsefulExpiredUnused   int
	WeightedDiscardedPct  float64
	RestoredPct           float64
}

type ResetScheduleBlocker struct {
	Code         string
	AccountEmail string
	AccountID    string
}

type ResetSchedule struct {
	GeneratedAt time.Time
	Horizon     time.Time
	Exact       bool
	Complete    bool
	Confidence  ResetScheduleConfidence
	Items       []ResetScheduleItem
	Objective   ResetScheduleObjective
	Blockers    []ResetScheduleBlocker
}

type ResetScheduleItem struct {
	AccountEmail  string
	AccountID     string
	CreditID      string
	UseAt         time.Time
	UseBy         time.Time
	Status        ResetScheduleStatus
	Confidence    ResetScheduleConfidence
	RestoredPct   map[quota.WindowName]float64
	AvoidedGapSec int64
	ReasonCodes   []ResetScheduleReason
}
```

- [ ] **Step 1: Add failing transition tests**

Cover banked reset setting both shared windows to exactly 100 without changing `ResetAt`; natural reset advancing only matching epoch by `quota.PeriodFor`; joint 5-hour/7-day availability; weekly exhaustion gating 5-hour; scoped windows ignored as drivers; and unsupported credits reported without state mutation.

```go
func TestApplyBankedResetPreservesNaturalEpochs(t *testing.T) {
	state := simulationAccount{
		windows: map[quota.WindowName]simulationWindow{
			quota.Window5Hour: {remaining: 12, resetAt: fiveHourReset},
			quota.Window7Day:  {remaining: 44, resetAt: sevenDayReset},
		},
	}
	got, restored := applyBankedReset(state)
	if got.windows[quota.Window5Hour].resetAt != fiveHourReset || got.windows[quota.Window7Day].resetAt != sevenDayReset {
		t.Fatalf("banked reset changed natural epochs: %+v", got.windows)
	}
	if got.windows[quota.Window5Hour].remaining != 100 || got.windows[quota.Window7Day].remaining != 100 {
		t.Fatalf("remaining = %+v", got.windows)
	}
	if restored[quota.Window5Hour] != 88 || restored[quota.Window7Day] != 56 {
		t.Fatalf("restored = %+v", restored)
	}
}
```

- [ ] **Step 2: Add failing demand/event tests**

Cover multiplier-proportional demand, reallocation after either shared window exhausts one account, re-entry after natural/banked reset, exact exhaustion time, coverage-gap start/end, credit expiry event, and stable ordering when events share timestamp.

```go
func TestSimulationReallocatesDemandAfterWeeklyExhaustion(t *testing.T) {
	state := twoAccountSimulation(1, 3)
	next := advanceToNextEvent(state)
	if next.exhaustedAccount != "account-a" {
		t.Fatalf("exhausted = %q", next.exhaustedAccount)
	}
	after := advanceDemand(next.state, time.Hour)
	if got := after.accounts["account-b"].allocatedDemand; got != after.totalDemand {
		t.Fatalf("reallocated demand = %v, total = %v", got, after.totalDemand)
	}
}
```

- [ ] **Step 3: Confirm red**

Run: `go test -race -count=1 ./internal/aggregate -run 'Test(ApplyBankedReset|ApplyNaturalReset|Simulation|ResetScheduleHorizon)'`

Expected: compile failure because simulation input, state, and event helpers do not exist.

- [ ] **Step 4: Implement normalisation and horizon**

Validate unique non-empty account/credit keys, positive multipliers, finite percentages/rates, supported shared windows, and positive reset epochs. Use EWMA when available; otherwise derive cycle-average rate from elapsed window and used percentage, mark low confidence, and never make missing history a blocker.

For account `i`, define observed weighted demand as `burn_i * multiplier_i`; sum it once into `totalDemand`. When active multiplier sum is `M`, each active account depletes at `totalDemand / M` percentage points per second. When an account becomes unavailable, recompute `M`; do not recompute `totalDemand` from simulated depletion. Cycle fallback is `(100 - remaining) / elapsed` when elapsed is positive. When elapsed is zero but usage is non-zero, use bounded `(100 - remaining) / period`, matching existing aggregate fallback. Otherwise use zero. Every fallback is low confidence.

Horizon rules:

```go
func resetPlanningHorizon(now time.Time, credits []ResetScheduleCreditInput) time.Time {
	latest, finite := latestFiniteExpiry(credits)
	if !finite {
		return now.Add(quota.PeriodFor(quota.Window7Day))
	}
	return latest.Add(quota.PeriodFor(quota.Window7Day))
}
```

- [ ] **Step 5: Implement event engine**

Advance directly to earliest exhaustion, portfolio gap, natural reset, credit expiry, or candidate consumption event. Define stable tie order: natural reset, expiry, exhaustion/gap, candidate consumption, then account key and credit ID. Accumulate objective units continuously between events. Use `float64` percentages internally and clamp only at `[0,100]` boundaries.

- [ ] **Step 6: Confirm green**

Run: `go test -race -count=1 ./internal/aggregate -run 'Test(ApplyBankedReset|ApplyNaturalReset|Simulation|ResetScheduleHorizon)'`

- [ ] **Step 7: Commit**

```text
feat: added reset portfolio simulation

- modelled shared quota demand and reset events
- preserved natural window epochs during banked resets
```

### Task 5: Add deterministic branch-and-bound scheduling

**Files:**

- Modify: `internal/aggregate/reset_schedule.go`
- Modify: `internal/aggregate/reset_schedule_test.go`

**Interfaces:**

- Add one public pure entrypoint:

```go
func RecommendResetSchedule(ResetScheduleInput) ResetSchedule
```

- Internal search state contains current event time, per-account/window percentages and epochs, remaining credit set, chosen consumptions, objective prefix, confidence, and stable fingerprint.
- Comparator follows exact lexicographic order: unmet demand, gap duration, useful credits expired unused, weighted capacity discarded, negative restored percentage, then later consumption times.

- [ ] **Step 1: Add failing one-credit optimality tests**

Cover latest useful consumption before exhaustion, latest useful consumption before expiry, natural reset making earlier consumption wasteful, due-now five-minute lead, non-expiring deferred credit, credit that cannot restore one point, and no reset-epoch movement.

```go
func TestRecommendResetScheduleUsesFiveMinuteLead(t *testing.T) {
	input := oneCreditInput(now, expiry, exhaustion)
	got := RecommendResetSchedule(input)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v", got.Items)
	}
	wantBy := minTime(expiry, exhaustion)
	if got.Items[0].UseBy != wantBy || got.Items[0].UseAt != wantBy.Add(-5*time.Minute) {
		t.Fatalf("timing = %s/%s, want %s/%s", got.Items[0].UseAt, got.Items[0].UseBy, wantBy.Add(-5*time.Minute), wantBy)
	}
}
```

- [ ] **Step 2: Add failing portfolio and tie-break tests**

Cover three accounts with credits expiring same day but different optimal consumption times; multiple credits on one account; demand moving between multiplier tiers; identical objective stable ordering by account/credit IDs; useful-before-expiry hard constraint; and lexicographic objective counterexamples where a weighted scalar would choose wrong.

```go
func TestRecommendResetScheduleSeparatesSameDayExpiries(t *testing.T) {
	input := threeAccountSameDayExpiryFixture()
	got := RecommendResetSchedule(input)
	if len(got.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(got.Items))
	}
	if !(got.Items[0].UseBy.Before(got.Items[1].UseBy) && got.Items[1].UseBy.Before(got.Items[2].UseBy)) {
		t.Fatalf("deadlines were collapsed: %+v", got.Items)
	}
}
```

- [ ] **Step 3: Add failing pruning/approximation tests**

Prove dominance removes same-event/same-credit-set state only when capacity is no lower in every shared window and objective prefix is no worse. Generate more than 512 non-dominated states and prove `Exact=false`, deterministic survivor/result order, actionable schedule, and no hard-constraint loss for feasible useful credits.

- [ ] **Step 4: Confirm red**

Run: `go test -race -count=1 ./internal/aggregate -run 'TestRecommendResetSchedule|TestResetSchedule(Dominance|Beam|Objective)'`

Expected: compile failure because scheduling entrypoint and search do not exist.

- [ ] **Step 5: Implement search and dominance**

At each event branch to wait or consume each eligible credit. Prune infeasible branches when a useful supported credit cannot be scheduled before expiry. Group dominance by event timestamp plus remaining-credit fingerprint. Retain at most 512 states after stable objective/capacity/fingerprint sort. Do not expose beam width as configuration.

```go
const resetScheduleBeamWidth = 512

func betterResetObjective(a, b ResetScheduleObjective) bool {
	switch {
	case a.UnmetDemandPctSeconds != b.UnmetDemandPctSeconds:
		return a.UnmetDemandPctSeconds < b.UnmetDemandPctSeconds
	case a.GapDurationSeconds != b.GapDurationSeconds:
		return a.GapDurationSeconds < b.GapDurationSeconds
	case a.UsefulExpiredUnused != b.UsefulExpiredUnused:
		return a.UsefulExpiredUnused < b.UsefulExpiredUnused
	case a.WeightedDiscardedPct != b.WeightedDiscardedPct:
		return a.WeightedDiscardedPct < b.WeightedDiscardedPct
	default:
		return a.RestoredPct > b.RestoredPct
	}
}
```

Apply final timing as `UseAt = max(Now, UseBy-5m)`. Set `due_now` when `Now >= UseAt`; otherwise `scheduled`. Emit `deferred`, `not_yet_useful`, and `unsupported` rows for non-consumed credits. Sort actionable rows by `UseAt`, then account identity, then credit ID.

- [ ] **Step 6: Confirm green and repeat determinism**

Run: `go test -race -count=1 ./internal/aggregate -run 'TestRecommendResetSchedule|TestResetSchedule(Dominance|Beam|Objective)'`

Run: `go test -race -count=20 ./internal/aggregate -run 'TestRecommendResetScheduleSeparatesSameDayExpiries|TestResetScheduleBeamDeterministic'`

- [ ] **Step 7: Commit**

```text
feat: added reset schedule optimiser

- scheduled credits with deterministic branch-and-bound
- reported exactness, confidence, and timing reasons
```

### Task 6: Persist retry-safe consume attempts and select optional credits

**Files:**

- Create: `internal/provider/codex/reset_attempts.go`
- Create: `internal/provider/codex/reset_attempts_test.go`

**Interfaces:**

- Consumes: `fsutil.DurableFileSystem` plus `fsutil.ExclusiveFileCreator`, cache root, stable `AccountKey`, credit inventory, optional explicit ID, current time.
- Produces:

```go
type ResetAttempt struct {
	Version        int        `json:"version"`
	AccountKey     AccountKey `json:"account_key"`
	CreditID       string     `json:"credit_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	StartedAt      time.Time  `json:"started_at"`
}

type ResetAttemptStore struct {
	FS  fsutil.DurableFileSystem
	Dir string
}

type ResetCreditSelection struct {
	Credit  ResetCredit
	Attempt *ResetAttempt
	Resume  bool
}

func ResetIdempotencyKey(AccountKey, string) string
func NewResetAttemptStore(fsutil.DurableFileSystem, string) (*ResetAttemptStore, error)
func (s *ResetAttemptStore) Pending(AccountKey) ([]ResetAttempt, error)
func (s *ResetAttemptStore) Ensure(AccountKey, string, time.Time) (ResetAttempt, error)
func (s *ResetAttemptStore) Remove(AccountKey, string) error
func SelectResetCredit(time.Time, []ResetCredit, []ResetAttempt, string) (ResetCreditSelection, error)
```

- Attempt directory is `<cache-root>/reset-attempts-v1`, mode `0o700`. Filename is lowercase SHA-256 of `account-key + NUL + credit-id`, plus `.json`; never include raw IDs in path.

- [ ] **Step 1: Add failing deterministic key and selection tests**

Cover omitted credit with oldest pending; explicit matching pending; explicit differing pending rejection; explicit available credit; earliest finite expiry; nil expiry last; ties by grant time then ID; at-or-before-now expiry rejection; unsupported reset type; unknown status; and pending credit absent from latest list still resuming safely.

```go
func TestSelectResetCreditDefaultsToOldestPending(t *testing.T) {
	pending := []ResetAttempt{
		{CreditID: "later", StartedAt: now.Add(time.Minute)},
		{CreditID: "first", StartedAt: now},
	}
	got, err := SelectResetCredit(now, creditsFixture(), pending, "")
	if err != nil || !got.Resume || got.Credit.ID != "first" {
		t.Fatalf("selection = %+v, %v", got, err)
	}
}
```

- [ ] **Step 2: Add failing persistence and concurrency tests**

Prove deterministic `cq-reset-v1-<sha256>` idempotency; directory/file modes; exclusive first create; read-back verification before return; same account/credit concurrent `Ensure` returns same key; different credits do not overwrite; malformed/mismatched file fails closed; atomic replacement is never needed for immutable attempt; remove targets exact digest only; and no credential/account metadata appears beyond opaque account key required by schema.

```go
func TestResetAttemptEnsureReusesConcurrentAttempt(t *testing.T) {
	store := newAttemptStore(t)
	type result struct {
		attempt ResetAttempt
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			attempt, err := store.Ensure("account-key", "credit-1", now)
			results <- result{attempt: attempt, err: err}
		}()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("Ensure errors = %v, %v", a.err, b.err)
	}
	if a.attempt.IdempotencyKey != b.attempt.IdempotencyKey {
		t.Fatalf("keys differ: %q != %q", a.attempt.IdempotencyKey, b.attempt.IdempotencyKey)
	}
}
```

- [ ] **Step 3: Confirm red**

Run: `go test -race -count=1 ./internal/provider/codex -run 'Test(SelectResetCredit|ResetAttempt|ResetIdempotency)'`

Expected: compile failure because attempt store and selection policy do not exist.

- [ ] **Step 4: Implement immutable per-credit journal**

Use `CreateExclusive` with `0o600`, write JSON, sync file, close, sync directory, reopen/read, and validate full expected tuple. If create reports existence, read and validate existing file. Reject symlinks/unsafe ownership through existing secure filesystem helpers when running on OS filesystem. `Ensure` must return only after verification.

```go
func ResetIdempotencyKey(account AccountKey, creditID string) string {
	digest := sha256.Sum256([]byte(string(account) + "\x00" + creditID))
	return "cq-reset-v1-" + hex.EncodeToString(digest[:])
}
```

- [ ] **Step 5: Implement exact selection precedence**

Sort pending by `StartedAt`, then credit ID. A pending attempt always wins omitted selection. Any unresolved attempt blocks a different explicit credit. Without pending, filter to `codex_rate_limits`, `available`, non-expired credits and sort expiry, grant time, ID.

- [ ] **Step 6: Confirm green**

Run: `go test -race -count=1 ./internal/provider/codex -run 'Test(SelectResetCredit|ResetAttempt|ResetIdempotency)'`

- [ ] **Step 7: Commit**

```text
feat: added reset consume retry state

- persisted deterministic idempotency attempts
- selected pending or earliest-expiring credits safely
```

### Task 7: Orchestrate list, recommendation, and manual consumption

**Files:**

- Create: `internal/app/codex_resets.go`
- Create: `internal/app/codex_resets_test.go`
- Modify: `internal/app/report.go`

**Interfaces:**

- Define narrow fakes at app boundary:

```go
type CodexResetBackend interface {
	Snapshot(context.Context) (codex.ResetAccountSnapshot, error)
	ListCredits(context.Context, codex.ResetAccount) (codex.ResetCreditInventory, error)
	Consume(context.Context, codex.ResetAccount, string, string) (codex.ConsumeResetResult, error)
}

type CodexResetUsage interface {
	Fetch(context.Context, time.Time) ([]quota.Result, error)
}

type CodexResetHistory interface {
	UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error)
}

type CodexResetAttempts interface {
	Pending(codex.AccountKey) ([]codex.ResetAttempt, error)
	Ensure(codex.AccountKey, string, time.Time) (codex.ResetAttempt, error)
	Remove(codex.AccountKey, string) error
}

type CodexResetApp struct {
	Backend  CodexResetBackend
	Usage    CodexResetUsage
	History  CodexResetHistory
	Attempts CodexResetAttempts
	Cache    Cache
	Clock    Clock
}

type CodexResetPublicError struct {
	Code string `json:"code"`
}

type CodexResetAccountCredits struct {
	AccountID string                 `json:"account_id,omitempty"`
	Email     string                 `json:"email,omitempty"`
	Credits   []codex.ResetCredit    `json:"credits"`
	Error     *CodexResetPublicError `json:"error,omitempty"`
}

type CodexResetListResult struct {
	Accounts []CodexResetAccountCredits `json:"accounts"`
}

type CodexResetUsePlan struct {
	AccountID      string                           `json:"account_id,omitempty"`
	Email          string                           `json:"email,omitempty"`
	Credit         codex.ResetCredit                `json:"credit"`
	CurrentWindows map[quota.WindowName]quota.Window `json:"current_windows"`
	Recommendation *aggregate.ResetSchedule         `json:"recommendation,omitempty"`
	account        codex.ResetAccount
	selection      codex.ResetCreditSelection
}

type CodexResetWindowChange struct {
	Before quota.Window `json:"before"`
	After  quota.Window `json:"after"`
}

type CodexResetWarning struct {
	Code string `json:"code"`
}

type CodexResetUseResult struct {
	AccountID     string                                      `json:"account_id,omitempty"`
	Email         string                                      `json:"email,omitempty"`
	CreditID      string                                      `json:"credit_id"`
	Outcome       codex.ConsumeResetOutcome                   `json:"outcome"`
	WindowsReset  int64                                       `json:"windows_reset"`
	ChangedWindows map[quota.WindowName]CodexResetWindowChange `json:"changed_windows,omitempty"`
	Warnings      []CodexResetWarning                         `json:"warnings,omitempty"`
}

type CodexResetError struct {
	Code string
	Err  error
}

func (a *CodexResetApp) List(context.Context, string) (CodexResetListResult, error)
func (a *CodexResetApp) Recommend(context.Context) (aggregate.ResetSchedule, error)
func (a *CodexResetApp) PrepareUse(context.Context, string, string) (CodexResetUsePlan, error)
func (a *CodexResetApp) ExecuteUse(context.Context, CodexResetUsePlan) (CodexResetUseResult, error)
```

- Use stable error codes: `account_reference_invalid`, `credential_unavailable`, `auth_expired`, `credits_unavailable`, `credit_not_found`, `credit_expired`, `unsupported_reset_type`, `recommendation_incomplete`, `consume_indeterminate`, `consume_failed`.

- [ ] **Step 1: Add failing best-effort list tests**

Cover all accounts, one account reference, mixed success/failure, panic recovery per account goroutine, deterministic original account order, unknown values visible, and no internal account key/candidate/revision in JSON.

```go
func TestCodexResetListKeepsOtherAccountsAfterFailure(t *testing.T) {
	app := resetAppFixture(creditResult("a"), creditFailure("b", errors.New("offline")), creditResult("c"))
	got, err := app.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 3 || got.Accounts[1].Error.Code != "credits_unavailable" {
		t.Fatalf("list = %+v", got)
	}
}
```

- [ ] **Step 2: Add failing complete/incomplete recommendation tests**

Prove fresh usage bypasses cache by calling `Usage.Fetch` directly; every visible account requires one matched fresh usable result and successful credit inventory, including accounts with zero credits; missing/duplicate usage or failed credits sets `Complete=false`, adds blockers, clears actionable `UseAt`/`UseBy`, and returns `recommendation_incomplete`; missing EWMA uses cycle fallback with low confidence but remains complete; approximate search remains actionable and returns nil error; and backend consume call count stays zero in every recommendation case.

```go
func TestCodexResetRecommendNeverConsumes(t *testing.T) {
	backend := &recordingResetBackend{snapshot: completeSnapshotFixture()}
	app := completeResetAppFixture(backend)
	got, err := app.Recommend(context.Background())
	if err != nil || !got.Complete {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
	if backend.consumeCalls != 0 {
		t.Fatalf("consume calls = %d, want 0", backend.consumeCalls)
	}
}
```

- [ ] **Step 3: Add failing prepare/execute tests**

Prepare must return target account, selected credit, expiry, current shared percentages, and best-effort current portfolio recommendation without POST or attempt creation. Execute must rerun pending selection, reject changed selection, `Ensure` attempt before POST, and classify outcomes:

- `reset`/`already_redeemed`: terminal success, remove pending, invalidate Codex cache, refetch usage, update history, report changed windows;
- `nothing_to_reset`/`no_credit`: definitive no-op, remove pending, no success refetch;
- timeout/transport/`5xx`/malformed success: retain pending, `consume_indeterminate`, include exact retry arguments;
- other `4xx`: retain pending because no known terminal outcome was decoded; return `consume_failed`;
- pending removal failure after terminal outcome: warning only;
- refetch/history failure after success: warning only, mutation remains successful.

```go
func TestCodexResetExecutePersistsBeforeConsume(t *testing.T) {
	sequence := []string{}
	app := resetUseFixture(
		withAttemptHook(func() { sequence = append(sequence, "persist") }),
		withConsumeHook(func() { sequence = append(sequence, "post") }),
	)
	plan := prepareResetUse(t, app, "user@example.com", "")
	if _, err := app.ExecuteUse(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string{"persist", "post"}, sequence) {
		t.Fatalf("order = %v, want [persist post]", sequence)
	}
}
```

- [ ] **Step 4: Confirm red**

Run: `go test -race -count=1 ./internal/app -run 'TestCodexReset'`

Expected: compile failure because reset application service does not exist.

- [ ] **Step 5: Implement concurrent reads with panic recovery**

List credit inventories concurrently. Recommendation may fetch credits concurrently with fresh usage, but every external-code goroutine must recover panic into typed blocker/error. Join usage to snapshot by strong account ID first, then unique case-insensitive email only when account ID is unavailable. Reject duplicate matches.

- [ ] **Step 6: Implement recommendation mapping**

Map only shared `5h` and `7d` windows into aggregate input. Use `quota.ExtractMultiplier`. Map EWMA estimates by provider/account/window; let aggregate derive cycle fallback when missing. Keep scoped windows available for list/preflight reporting only.

For incomplete input, still return generated time, horizon, blockers, confidence, and non-actionable credit rows; zero all actionable timing fields before returning typed `recommendation_incomplete`.

- [ ] **Step 7: Implement two-phase use**

`PrepareUse` performs no durable write and no POST. Store opaque account key and selected credit ID in unexported fields of plan. `ExecuteUse` refreshes snapshot/list, re-applies selection rules, compares exact account/credit, persists attempt, then calls backend consume. Never let recommendation result block explicit valid manual consumption.

- [ ] **Step 8: Confirm green**

Run: `go test -race -count=1 ./internal/app -run 'TestCodexReset'`

- [ ] **Step 9: Commit**

```text
feat: added Codex reset workflows

- orchestrated portfolio recommendations and listings
- added confirmed retry-safe manual consumption
```

### Task 8: Expose CLI commands, confirmation, TTY, JSON, and help

**Files:**

- Create: `cmd/cq/codex_resets.go`
- Create: `cmd/cq/codex_resets_test.go`
- Modify: `cmd/cq/main.go`
- Modify: `cmd/cq/main_test.go`
- Modify: `cmd/cq/help.go`
- Modify: `cmd/cq/help_test.go`
- Modify: `cmd/cq/readme_test.go`
- Modify: `README.md`

**Interfaces:**

- Kong command model:

```go
type CodexCmd struct {
	Login    LoginCmd
	Accounts AccountsCmd
	Switch   SwitchCmd
	Remove   RemoveCmd
	Resets   CodexResetsCmd `cmd:"" help:"Inspect and use banked reset credits"`
}

type CodexResetsCmd struct {
	List      CodexResetsListCmd      `cmd:"" help:"List reset credits"`
	Recommend CodexResetsRecommendCmd `cmd:"" help:"Recommend a portfolio schedule"`
	Use       CodexResetsUseCmd       `cmd:"" help:"Use one reset credit"`
}

type CodexResetsListCmd struct {
	Reference string `arg:"" optional:"" name:"account-reference"`
}

type CodexResetsRecommendCmd struct{}

type CodexResetsUseCmd struct {
	Reference string `arg:"" name:"account-reference"`
	Credit    *string `help:"Credit ID; defaults to pending or next expiry" placeholder:"CREDIT_ID"`
	Yes       bool   `help:"Consume without interactive confirmation"`
}
```

- Runtime dependencies are injectable for tests:

```go
type codexResetsDependencies struct {
	App    *app.CodexResetApp
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

func runCodexResetsList(context.Context, CodexResetsListCmd, bool, codexResetsDependencies) error
func runCodexResetsRecommend(context.Context, bool, codexResetsDependencies) error
func runCodexResetsUse(context.Context, CodexResetsUseCmd, bool, codexResetsDependencies) error
```

- [ ] **Step 1: Add failing parser and dispatch tests**

Cover exact invocations:

```text
cq codex resets list
cq codex resets list user@example.com
cq codex resets recommend
cq codex resets use user@example.com
cq codex resets use user@example.com --credit credit-1
cq codex resets use user@example.com --credit credit-1 --yes
cq --json codex resets recommend
```

Reject recommendation account args, missing use account, empty credit flag, unknown flags, and extra args. Prove global `--json` reaches each handler.

- [ ] **Step 2: Add failing confirmation tests**

Before prompt, print account, selected credit, expiry, shared percentages, and current recommendation when available. Prompt `Use this banked reset? [y/N]` on `ErrOut`. Accept only case-insensitive `y`/`yes`; empty, EOF, other text, and non-terminal input decline without creating attempt or POST. `--yes` skips input exactly once. Decline returns success with explicit `cancelled` output.

```go
func TestRunCodexResetsUseDefaultsConfirmationToNo(t *testing.T) {
	deps, backend := useCLIFixture("\n")
	err := runCodexResetsUse(context.Background(), CodexResetsUseCmd{Reference: "user@example.com"}, false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if backend.consumeCalls != 0 {
		t.Fatalf("consume calls = %d, want 0", backend.consumeCalls)
	}
}
```

- [ ] **Step 3: Add failing render tests**

TTY list groups account rows and shows credit ID, reset type, status, grant, expiry, title, description, plus per-account errors. TTY recommendation sorts chronologically and shows email, title-or-ID, `UseAt`, `UseBy`, restored percentages, primary reason, `exact`, `complete`, and confidence. JSON uses stable structs/codes and contains no token, account key, revision, candidate ID, or secret fixture. Incomplete recommendation prints body then returns non-zero. Approximate complete recommendation prints `exact=false` and returns zero.

- [ ] **Step 4: Add failing help/README parity tests**

Add manual help paths `codex resets`, `codex resets list`, `codex resets recommend`, and `codex resets use`. State portfolio-wide scope, advisory-only recommendation, optional credit default, confirmation default No, and `--yes`. Add all public command paths to README command roster so `readme_test.go` remains authoritative.

- [ ] **Step 5: Confirm red**

Run: `go test -race -count=1 ./cmd/cq -run 'Test(CLIParsesCodexResets|DispatchCodexResets|RunCodexResets|CodexResetsHelp|READMEListsEveryPublicCommandPath)'`

Expected: compile/test failure because CLI model, handlers, rendering, and help paths do not exist.

- [ ] **Step 6: Wire production dependencies**

Open one default credential refresh control using `fsutil.OSFileSystem{}` and existing HTTP client; construct `ResetBackend`, reset attempt store under `cache.DefaultDir()`, Codex usage provider, history store, and cache. Close control on every path. Do not start agent, switch account, or mutate system auth.

Dispatch command strings produced by Kong, including optional list argument variants. On successful `reset`/`already_redeemed`, invalidate provider cache through `Cache.Delete`; keep existing best-effort cache diagnostic style.

- [ ] **Step 7: Implement renderers and confirmation**

Use dedicated render functions in `codex_resets.go`; do not expand generic quota report types. JSON encoder receives public DTOs only. Prompt after `PrepareUse` and before `ExecuteUse`. `--yes` is passed only from parsed user command; recommendation output never prints or invokes a follow-up command with `--yes`.

- [ ] **Step 8: Confirm green**

Run: `go test -race -count=1 ./cmd/cq -run 'Test(CLIParsesCodexResets|DispatchCodexResets|RunCodexResets|CodexResetsHelp|READMEListsEveryPublicCommandPath)'`

- [ ] **Step 9: Commit**

```text
feat: added Codex reset CLI

- added list, recommend, and confirmed use commands
- documented portfolio scheduling and credit defaults
```

### Task 9: Add cross-layer contract tests and run release gates

**Files:**

- Modify: `internal/provider/codex/reset_accounts_test.go`
- Modify: `internal/app/codex_resets_test.go`
- Modify: `cmd/cq/codex_resets_test.go`
- Modify: `cmd/cq/readme_test.go`

**Interfaces:**

- No new production API. Tests bind transport, credential snapshot, optimiser, retry store, app workflow, and CLI output.

- [ ] **Step 1: Add recommendation non-mutation contract test**

Use fakes that panic on consume and all credential mutators. Run portfolio recommendation through CLI handler. Assert complete output, zero attempt files, zero cache deletion, no prompt, and no panic.

```go
func TestCodexResetsRecommendIsAdvisoryAcrossLayers(t *testing.T) {
	deps := advisoryOnlyCLIFixture(t)
	if err := runCodexResetsRecommend(context.Background(), true, deps); err != nil {
		t.Fatal(err)
	}
	assertNoAttemptFiles(t, deps)
	assertNoMutations(t, deps)
}
```

- [ ] **Step 2: Add lost-response replay contract test**

First `use --yes` persists attempt and receives transport timeout after backend records consumption. Second invocation omits `--credit`, latest credit list omits redeemed credit, pending attempt wins, same `redeem_request_id` is sent, backend returns `already_redeemed`, exact pending file is removed, cache is invalidated, and usage refetch/history correction preserves EWMA.

- [ ] **Step 3: Add three-account same-day acceptance test**

Build three visible accounts, fresh 5-hour/7-day usage, different multipliers/rates/reset epochs, and one same-day-expiring credit each. Run `recommend` twice. Assert three independently timed items, deterministic JSON byte-for-byte, no natural reset epoch changes in trace fixture, and no consume calls.

- [ ] **Step 4: Add incomplete portfolio acceptance test**

Make one visible account's credit GET fail. Assert all accounts remain reported, `complete=false`, blocker code `credits_unavailable`, no actionable timestamps for any item, non-zero handler result, and no consume/attempt/cache mutation.

- [ ] **Step 5: Run focused package suites**

Run:

```bash
go test -race -count=1 ./internal/provider/codex
go test -race -count=1 ./internal/history
go test -race -count=1 ./internal/aggregate
go test -race -count=1 ./internal/app
go test -race -count=1 ./cmd/cq
```

Expected: every command passes with race detector enabled.

- [ ] **Step 6: Run full repository gates**

Run:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Expected: build, vet, and complete race suite pass.

- [ ] **Step 7: Inspect security and scope invariants**

Run:

```bash
rg -n 'AccessToken|RefreshToken|CandidateID|Revision|AccountKey' internal/aggregate/reset_schedule.go internal/app/codex_resets.go cmd/cq/codex_resets.go
rg -n 'Consume\(' internal/app/codex_resets.go cmd/cq/codex_resets.go
git diff --check
git status --short
```

Expected: no secret-bearing public fields/output; consume call exists only in confirmed execute path; no whitespace errors; only planned files changed.

- [ ] **Step 8: Commit acceptance coverage**

```text
test: added banked reset acceptance coverage

- proved advisory scheduling across all accounts
- proved idempotent replay and incomplete-input safety
```

## Completion Criteria

- `cq codex accounts` and reset commands use same sanitised visible inventory.
- `cq codex resets list [account-reference]` is best effort and exposes typed per-account errors.
- `cq codex resets recommend` always plans whole visible portfolio, consumes nothing, separates same-day expiries when optimal, and disables actionable times when any fresh input is missing.
- Banked reset simulation restores percentages only; natural 5-hour/7-day reset dates remain unchanged until their own reset events.
- `cq codex resets use <account-reference>` defaults to pending attempt or earliest expiry, prompts with default No, and consumes only after explicit confirmation or user-supplied `--yes`.
- Lost responses reuse deterministic idempotency key across process restarts; indeterminate attempts remain pending.
- Successful terminal consume invalidates cache, refetches usage, updates history, and treats refetch failure as warning.
- Upward percentage correction with unchanged reset epoch preserves EWMA and sample count.
- No tokens, credential revisions, candidate IDs, or internal account keys appear in public output or error text.
- `go build ./...`, `go vet ./...`, and `go test -race -count=1 ./...` pass.
