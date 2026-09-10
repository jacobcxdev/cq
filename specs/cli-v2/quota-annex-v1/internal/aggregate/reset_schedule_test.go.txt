package aggregate

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func resetTestWindow(remaining float64, resetAt time.Time, period time.Duration, rate float64) ResetScheduleWindowInput {
	return ResetScheduleWindowInput{
		RemainingPct: remaining,
		ResetAt:      resetAt,
		Period:       period,
		BurnPctPerS:  rate,
		RateSource:   ResetRateEWMA,
		Samples:      3,
	}
}

func TestApplyBankedResetPreservesNaturalEpochs(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour)
	sevenDayReset := now.Add(3 * 24 * time.Hour)
	state := simulationAccount{windows: map[quota.WindowName]simulationWindow{
		quota.Window5Hour: {remaining: 12, resetAt: fiveHourReset, period: 5 * time.Hour},
		quota.Window7Day:  {remaining: 44, resetAt: sevenDayReset, period: 7 * 24 * time.Hour},
	}}

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

func TestApplyNaturalResetAdvancesOnlyMatchingEpoch(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(time.Hour)
	sevenDayReset := now.Add(24 * time.Hour)
	state := simulationAccount{windows: map[quota.WindowName]simulationWindow{
		quota.Window5Hour: {remaining: 20, resetAt: fiveHourReset, period: 5 * time.Hour},
		quota.Window7Day:  {remaining: 40, resetAt: sevenDayReset, period: 7 * 24 * time.Hour},
	}}

	got, discarded := applyNaturalReset(state, quota.Window5Hour)
	if got.windows[quota.Window5Hour].remaining != 100 || got.windows[quota.Window5Hour].resetAt != fiveHourReset.Add(5*time.Hour) {
		t.Fatalf("5h window = %+v", got.windows[quota.Window5Hour])
	}
	if got.windows[quota.Window7Day].remaining != 40 || got.windows[quota.Window7Day].resetAt != sevenDayReset {
		t.Fatalf("7d window changed = %+v", got.windows[quota.Window7Day])
	}
	if discarded != 20 {
		t.Fatalf("discarded = %v, want 20", discarded)
	}
}

func TestSimulationReallocatesAfterWeeklyExhaustion(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		{
			Key: "account-a", Multiplier: 1,
			Windows: map[quota.WindowName]ResetScheduleWindowInput{
				quota.Window5Hour: resetTestWindow(100, now.Add(5*time.Hour), 5*time.Hour, 0.01),
				quota.Window7Day:  resetTestWindow(1, now.Add(7*24*time.Hour), 7*24*time.Hour, 0.01),
			},
		},
		{
			Key: "account-b", Multiplier: 3,
			Windows: map[quota.WindowName]ResetScheduleWindowInput{
				quota.Window5Hour: resetTestWindow(100, now.Add(5*time.Hour), 5*time.Hour, 0.01),
				quota.Window7Day:  resetTestWindow(100, now.Add(7*24*time.Hour), 7*24*time.Hour, 0.01),
			},
		},
	}}
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 {
		t.Fatalf("blockers = %+v", blockers)
	}
	event, ok := nextSimulationEvent(state)
	if !ok || event.kind != simulationEventExhaustion || event.accountKey != "account-a" || event.window != quota.Window7Day {
		t.Fatalf("event = %+v, %v", event, ok)
	}
	state = advanceSimulationTo(state, event.at)
	state = processSimulationEvent(state, event)
	if simulationActiveMultiplier(state) != 3 {
		t.Fatalf("active multiplier = %v, want 3", simulationActiveMultiplier(state))
	}
	before := state.accounts["account-b"].windows[quota.Window7Day].remaining
	state = advanceSimulationTo(state, state.now.Add(100*time.Second))
	got := before - state.accounts["account-b"].windows[quota.Window7Day].remaining
	want := state.totalDemand[quota.Window7Day] / 3 * 100
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("reallocated depletion = %v, want %v", got, want)
	}
}

func TestSimulationNaturalResetReentersJointCapacity(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 2,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour:                  resetTestWindow(80, now.Add(5*time.Hour), 5*time.Hour, 0.01),
			quota.Window7Day:                   resetTestWindow(0, now.Add(time.Hour), 7*24*time.Hour, 0.01),
			quota.WindowName("7d:model-scope"): resetTestWindow(0, now.Add(time.Hour), 7*24*time.Hour, 1),
		},
	}}}
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 || simulationActiveMultiplier(state) != 0 {
		t.Fatalf("initial state blockers=%+v multiplier=%v", blockers, simulationActiveMultiplier(state))
	}
	event, ok := nextSimulationEvent(state)
	if !ok || event.kind != simulationEventNaturalReset || event.window != quota.Window7Day {
		t.Fatalf("event = %+v, %v", event, ok)
	}
	state = advanceSimulationTo(state, event.at)
	state = processSimulationEvent(state, event)
	if simulationActiveMultiplier(state) != 2 {
		t.Fatalf("active multiplier after weekly reset = %v", simulationActiveMultiplier(state))
	}
	if _, retained := state.accounts["account-a"].windows[quota.WindowName("7d:model-scope")]; retained {
		t.Fatal("scoped window entered shared simulation")
	}
}

func TestSimulationTracksExactCoverageGap(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 1,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(1, now.Add(5*time.Hour), 5*time.Hour, 0.01),
			quota.Window7Day:  resetTestWindow(1, now.Add(2*time.Hour), 7*24*time.Hour, 0.01),
		},
	}}}
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	if !ok || !event.at.Equal(now.Add(100*time.Second)) {
		t.Fatalf("exhaustion event = %+v, %v", event, ok)
	}
	state = advanceSimulationTo(state, event.at)
	state = processSimulationEvent(state, event)
	state = advanceSimulationTo(state, now.Add(time.Hour))
	if state.objective.GapDurationSeconds != 3_500 {
		t.Fatalf("gap duration = %d, want 3500", state.objective.GapDurationSeconds)
	}
	if state.objective.UnmetDemandPctSeconds <= 0 {
		t.Fatalf("unmet demand = %v", state.objective.UnmetDemandPctSeconds)
	}
}

func TestSimulationUsesCycleAverageFallback(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 2,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: {RemainingPct: 80, ResetAt: now.Add(4 * time.Hour), Period: 5 * time.Hour},
			quota.Window7Day:  {RemainingPct: 90, ResetAt: now.Add(7 * 24 * time.Hour), Period: 7 * 24 * time.Hour},
		},
	}}}
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 || state.confidence != ResetConfidenceLow {
		t.Fatalf("state blockers=%+v confidence=%q", blockers, state.confidence)
	}
	want5h := (20.0 / float64(time.Hour/time.Second)) * 2
	if got := state.totalDemand[quota.Window5Hour]; math.Abs(got-want5h) > 1e-12 {
		t.Fatalf("5h total demand = %v, want %v", got, want5h)
	}
	want7d := (10.0 / float64((7*24*time.Hour)/time.Second)) * 2
	if got := state.totalDemand[quota.Window7Day]; math.Abs(got-want7d) > 1e-12 {
		t.Fatalf("7d bounded fallback = %v, want %v", got, want7d)
	}
}

func TestSimulationUnsupportedCreditDoesNotMutateState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 1,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(20, now.Add(5*time.Hour), 5*time.Hour, 0.01),
			quota.Window7Day:  resetTestWindow(40, now.Add(7*24*time.Hour), 7*24*time.Hour, 0.01),
		},
		Credits: []ResetScheduleCreditInput{{ID: "unsupported", Supported: false}},
	}}}
	state, _ := normaliseResetScheduleInput(input)
	before := state.accounts["account-a"]
	after, restored, applied := applySimulationCredit(state, "account-a", "unsupported")
	if applied || len(restored) != 0 {
		t.Fatalf("unsupported credit applied=%v restored=%v", applied, restored)
	}
	for name, window := range before.windows {
		if after.accounts["account-a"].windows[name] != window {
			t.Fatalf("window %s mutated: before=%+v after=%+v", name, window, after.accounts["account-a"].windows[name])
		}
	}
}

func TestSimulationInvalidAccountDoesNotContributeDemand(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "invalid", Multiplier: 10,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(90, now.Add(5*time.Hour), 5*time.Hour, 1),
		},
	}}}
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) == 0 {
		t.Fatal("invalid account produced no blocker")
	}
	if len(state.accounts) != 0 || state.totalDemand[quota.Window5Hour] != 0 {
		t.Fatalf("invalid account leaked into state: accounts=%+v demand=%+v", state.accounts, state.totalDemand)
	}
}

func TestSimulationZeroDemandDoesNotCreateCoverageGap(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 1,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(0, now.Add(5*time.Hour), 5*time.Hour, 0),
			quota.Window7Day:  resetTestWindow(0, now.Add(7*24*time.Hour), 7*24*time.Hour, 0),
		},
	}}}
	state, _ := normaliseResetScheduleInput(input)
	state = advanceSimulationTo(state, now.Add(time.Hour))
	if state.objective.GapDurationSeconds != 0 || state.objective.UnmetDemandPctSeconds != 0 {
		t.Fatalf("zero demand objective = %+v", state.objective)
	}
}

func TestSimulationCreditExpiryAndTieOrdering(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{
		Key: "account-a", Multiplier: 1,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(100, now.Add(5*time.Hour), 5*time.Hour, 0),
			quota.Window7Day:  resetTestWindow(100, now.Add(7*24*time.Hour), 7*24*time.Hour, 0),
		},
		Credits: []ResetScheduleCreditInput{{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}},
	}}}
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	if !ok || event.kind != simulationEventExpiry || event.creditID != "credit-a" || !event.at.Equal(expiresAt) {
		t.Fatalf("expiry event = %+v, %v", event, ok)
	}

	tied := []simulationEvent{
		{at: expiresAt, kind: simulationEventConsumption, accountKey: "b", creditID: "b"},
		{at: expiresAt, kind: simulationEventExhaustion, accountKey: "a", window: quota.Window5Hour},
		{at: expiresAt, kind: simulationEventExpiry, accountKey: "a", creditID: "a"},
		{at: expiresAt, kind: simulationEventNaturalReset, accountKey: "a", window: quota.Window7Day},
	}
	sort.Slice(tied, func(i, j int) bool { return simulationEventLess(tied[i], tied[j]) })
	want := []simulationEventKind{simulationEventNaturalReset, simulationEventExpiry, simulationEventExhaustion, simulationEventConsumption}
	for i := range want {
		if tied[i].kind != want[i] {
			t.Fatalf("tie order = %+v", tied)
		}
	}
}

func TestResetScheduleHorizon(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiryA := now.Add(24 * time.Hour)
	expiryB := now.Add(72 * time.Hour)
	credits := []ResetScheduleCreditInput{{ID: "a", ExpiresAt: &expiryA}, {ID: "b", ExpiresAt: &expiryB}}
	if got, want := resetPlanningHorizon(now, credits), expiryB.Add(7*24*time.Hour); !got.Equal(want) {
		t.Fatalf("finite horizon = %s, want %s", got, want)
	}
	if got, want := resetPlanningHorizon(now, []ResetScheduleCreditInput{{ID: "forever"}}), now.Add(7*24*time.Hour); !got.Equal(want) {
		t.Fatalf("rolling horizon = %s, want %s", got, want)
	}
}

func scheduleTestAccount(now time.Time, key string, multiplier int, remaining, rate float64, credit ResetScheduleCreditInput) ResetScheduleAccountInput {
	return ResetScheduleAccountInput{
		Key: key, Email: key + "@example.com", AccountID: "id-" + key, Multiplier: multiplier,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window5Hour: resetTestWindow(remaining, now.Add(5*time.Hour), 5*time.Hour, rate),
			quota.Window7Day:  resetTestWindow(remaining, now.Add(7*24*time.Hour), 7*24*time.Hour, rate),
		},
		Credits: []ResetScheduleCreditInput{credit},
	}
}

func TestRecommendResetScheduleUsesFiveMinuteLead(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(2 * time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 10, 0.01, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}),
	}}
	got := RecommendResetSchedule(input)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v", got.Items)
	}
	wantBy := now.Add(1_000 * time.Second)
	if got.Items[0].UseBy != wantBy || got.Items[0].UseAt != wantBy.Add(-5*time.Minute) {
		t.Fatalf("timing = %s/%s, want %s/%s", got.Items[0].UseAt, got.Items[0].UseBy, wantBy.Add(-5*time.Minute), wantBy)
	}
	if got.Items[0].Status != ResetScheduled || got.Items[0].RestoredPct[quota.Window5Hour] != 100 || got.Items[0].AvoidedGapSec <= 0 {
		t.Fatalf("item = %+v", got.Items[0])
	}
}

func TestResetSearchBranchesAtExhaustion(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(2 * time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 10, 0.01, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}),
	}}
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	if !ok || event.kind != simulationEventExhaustion {
		t.Fatalf("first event = %+v, %v", event, ok)
	}
	branches := expandResetSearchEvent(resetSearchNode{state: state, useful: map[string]bool{}}, event)
	if len(branches) != 1 {
		t.Fatalf("branches = %+v", branches)
	}
	if len(branches[0].consumptions) != 1 || branches[0].consumptions[0].useBy != event.at {
		t.Fatalf("consume branch = %+v", branches[0])
	}
}

func TestRecommendResetScheduleUsesLatestUsefulExpiry(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 90, 0.001, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}),
	}}
	got := RecommendResetSchedule(input)
	if len(got.Items) != 1 || got.Items[0].UseBy != expiresAt || got.Items[0].Status != ResetScheduled {
		t.Fatalf("schedule = %+v", got)
	}
	if got.Items[0].RestoredPct[quota.Window5Hour] < 3.59 {
		t.Fatalf("restored = %+v", got.Items[0].RestoredPct)
	}
}

func TestRecommendResetScheduleDueNowForCurrentGap(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 0, 0.01, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}),
	}}
	got := RecommendResetSchedule(input)
	if len(got.Items) != 1 || got.Items[0].Status != ResetDueNow || got.Items[0].UseAt != now || got.Items[0].UseBy != now {
		t.Fatalf("schedule = %+v", got)
	}
}

func TestRecommendResetScheduleDefersNonExpiringUnusedCredit(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 100, 0, ResetScheduleCreditInput{ID: "credit-a", Supported: true}),
	}}
	got := RecommendResetSchedule(input)
	if len(got.Items) != 1 || got.Items[0].Status != ResetDeferred || !got.Items[0].UseAt.IsZero() {
		t.Fatalf("schedule = %+v", got)
	}
}

func TestRecommendResetScheduleReportsUnsupportedAndNotUseful(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	account := scheduleTestAccount(now, "account-a", 1, 100, 0.0001, ResetScheduleCreditInput{ID: "not-useful", ExpiresAt: &expiresAt, Supported: true})
	account.Credits = append(account.Credits, ResetScheduleCreditInput{ID: "unsupported", ExpiresAt: &expiresAt})
	got := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account}})
	statuses := map[string]ResetScheduleStatus{}
	for _, item := range got.Items {
		statuses[item.CreditID] = item.Status
	}
	if statuses["not-useful"] != ResetNotUseful || statuses["unsupported"] != ResetUnsupported {
		t.Fatalf("statuses = %+v", statuses)
	}
}

func TestRecommendResetScheduleRequiresOnePointInOneWindow(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(50 * time.Second)
	account := scheduleTestAccount(now, "account-a", 1, 100, 0.011, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true})
	got := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account}})
	if len(got.Items) != 1 || got.Items[0].Status != ResetNotUseful {
		t.Fatalf("schedule = %+v", got)
	}
}

func TestRecommendResetScheduleSeparatesSameDayExpiries(t *testing.T) {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(20 * time.Hour)
	input := ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		scheduleTestAccount(now, "account-a", 1, 5, 0.01, ResetScheduleCreditInput{ID: "credit-a", ExpiresAt: &expiresAt, Supported: true}),
		scheduleTestAccount(now, "account-b", 1, 10, 0.01, ResetScheduleCreditInput{ID: "credit-b", ExpiresAt: &expiresAt, Supported: true}),
		scheduleTestAccount(now, "account-c", 1, 15, 0.01, ResetScheduleCreditInput{ID: "credit-c", ExpiresAt: &expiresAt, Supported: true}),
	}}
	got := RecommendResetSchedule(input)
	actionable := make([]ResetScheduleItem, 0, 3)
	for _, item := range got.Items {
		if item.Status == ResetScheduled || item.Status == ResetDueNow {
			actionable = append(actionable, item)
		}
	}
	if len(actionable) != 3 {
		t.Fatalf("actionable = %+v", actionable)
	}
	if !(actionable[0].UseBy.Before(actionable[1].UseBy) && actionable[1].UseBy.Before(actionable[2].UseBy)) {
		t.Fatalf("deadlines were collapsed: %+v", actionable)
	}
}

func TestResetScheduleObjectiveIsLexicographic(t *testing.T) {
	lessGapButMoreUnmet := ResetScheduleObjective{UnmetDemandPctSeconds: 2, GapDurationSeconds: 0}
	moreGapButLessUnmet := ResetScheduleObjective{UnmetDemandPctSeconds: 1, GapDurationSeconds: 10_000}
	if betterResetObjective(lessGapButMoreUnmet, moreGapButLessUnmet) {
		t.Fatal("gap duration overrode unmet demand")
	}
	moreRestored := ResetScheduleObjective{RestoredPct: 10}
	lessRestored := ResetScheduleObjective{RestoredPct: 5}
	if !betterResetObjective(moreRestored, lessRestored) {
		t.Fatal("restored percentage was not maximised after earlier ties")
	}
}

func TestResetScheduleDominanceRequiresNoLowerCapacity(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	state := simulationState{
		now: now,
		accounts: map[string]simulationAccount{"account-a": {
			key: "account-a", multiplier: 1,
			windows: map[quota.WindowName]simulationWindow{
				quota.Window5Hour: {remaining: 80},
				quota.Window7Day:  {remaining: 70},
			},
		}},
		credits: map[string]simulationCredit{"credit-a": {id: "credit-a"}},
	}
	better := resetSearchNode{state: state, useful: map[string]bool{}}
	worse := cloneResetSearchNode(better)
	worse.state.accounts["account-a"] = simulationAccount{
		key: "account-a", multiplier: 1,
		windows: map[quota.WindowName]simulationWindow{
			quota.Window5Hour: {remaining: 79},
			quota.Window7Day:  {remaining: 70},
		},
	}
	if !dominatesResetSearchNode(better, worse) {
		t.Fatal("higher-capacity state did not dominate")
	}
	notDominant := cloneResetSearchNode(better)
	notDominant.state.accounts["account-a"] = simulationAccount{
		key: "account-a", multiplier: 1,
		windows: map[quota.WindowName]simulationWindow{
			quota.Window5Hour: {remaining: 100},
			quota.Window7Day:  {remaining: 69},
		},
	}
	if dominatesResetSearchNode(notDominant, better) {
		t.Fatal("state with lower weekly capacity dominated")
	}
}

func TestResetScheduleBeamDeterministic(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	nodes := make([]resetSearchNode, 600)
	for index := range nodes {
		id := fmt.Sprintf("credit-%03d", 599-index)
		nodes[index] = resetSearchNode{
			state:  simulationState{now: now, accounts: map[string]simulationAccount{}, credits: map[string]simulationCredit{id: {id: id}}},
			useful: map[string]bool{},
		}
	}
	first, firstTruncated := pruneResetSearchNodes(append([]resetSearchNode(nil), nodes...), resetScheduleBeamWidth)
	second, secondTruncated := pruneResetSearchNodes(append([]resetSearchNode(nil), nodes...), resetScheduleBeamWidth)
	if !firstTruncated || !secondTruncated || len(first) != resetScheduleBeamWidth || len(second) != resetScheduleBeamWidth {
		t.Fatalf("beam sizes = %d/%d truncated=%v/%v", len(first), len(second), firstTruncated, secondTruncated)
	}
	for index := range first {
		if resetSearchFingerprint(first[index]) != resetSearchFingerprint(second[index]) {
			t.Fatalf("survivor %d differs", index)
		}
	}
}
