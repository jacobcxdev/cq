package aggregate

import (
	"math"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func reserveResetInput() ResetScheduleInput {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(24 * time.Hour)
	window := resetTestWindow(5, now.Add(48*time.Hour), 7*24*time.Hour, .001)
	window.ReservePct = 5
	return ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{
		{Key: "system", AccountID: "system-id", Multiplier: 1,
			Windows: map[quota.WindowName]ResetScheduleWindowInput{quota.Window7Day: window},
			Credits: []ResetScheduleCreditInput{{ID: "system-credit", GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiry, Supported: true}}},
		{Key: "other", AccountID: "other-id", Multiplier: 1,
			Windows: map[quota.WindowName]ResetScheduleWindowInput{quota.Window7Day: resetTestWindow(0, now.Add(48*time.Hour), 7*24*time.Hour, .001)},
			Credits: []ResetScheduleCreditInput{{ID: "other-credit", GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiry, Supported: true}}},
	}, Constraints: &ResetScheduleConstraints{AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "system", CreditID: "system-credit"}, {AccountKey: "other", CreditID: "other-credit"}}, NextAccountKey: "other"}}
}

func TestReserveProtectedQuotaDoesNotPostponeOtherAccountReset(t *testing.T) {
	input := reserveResetInput()
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 || simulationActiveMultiplier(state) != 0 {
		t.Fatalf("protected quota counted as available: multiplier=%g blockers=%+v", simulationActiveMultiplier(state), blockers)
	}
	got := RecommendResetSchedule(input)
	if !got.Complete {
		t.Fatalf("schedule incomplete: %+v", got)
	}
	for _, item := range got.Items {
		if item.CreditID == "other-credit" && item.Status == ResetDueNow && item.UseAt.Equal(input.Now) {
			return
		}
	}
	t.Fatalf("empty account reset postponed behind reserve: %+v", got.Items)
}

func TestReserveSystemFirstConsumesActualQuotaBeforeReset(t *testing.T) {
	input := reserveResetInput()
	input.Constraints.NextAccountKey = "system"
	window := input.Accounts[0].Windows[quota.Window7Day]
	window.ReserveReleased = true
	input.Accounts[0].Windows[quota.Window7Day] = window
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 || simulationActiveMultiplier(state) != 1 {
		t.Fatalf("released system unavailable: multiplier=%g blockers=%+v", simulationActiveMultiplier(state), blockers)
	}
	if _, _, ok := applySimulationCredit(state, "other", "other-credit"); ok {
		t.Fatal("another account consumed before committed system reset")
	}
	event, ok := nextSimulationEvent(state)
	if !ok || event.kind != simulationEventExhaustion || event.accountKey != "system" || !event.at.Equal(input.Now.Add(2500*time.Second)) {
		t.Fatalf("reserve drain event=%+v ok=%t", event, ok)
	}
	state = processSimulationEvent(advanceSimulationTo(state, event.at), event)
	state, restored, ok := applySimulationCredit(state, "system", "system-credit")
	if !ok || restored[quota.Window7Day] != 100 {
		t.Fatalf("reset restoration=%+v ok=%t", restored, ok)
	}
	if state.accounts["system"].windows[quota.Window7Day].remaining != 100 {
		t.Fatal("banked reset lost physical quota")
	}
	state, _, ok = applySimulationCredit(state, "other", "other-credit")
	if !ok {
		t.Fatal("first-account constraint survived system consumption")
	}
}

func TestReserveRefillsRearmAndRetainPhysicalPercentages(t *testing.T) {
	input := reserveResetInput()
	input.Constraints.NextAccountKey = "system"
	window := input.Accounts[0].Windows[quota.Window7Day]
	window.ReserveReleased = true
	input.Accounts[0].Windows[quota.Window7Day] = window
	state, _ := normaliseResetScheduleInput(input)
	banked, restored, ok := applySimulationCredit(state, "system", "system-credit")
	if !ok || restored[quota.Window7Day] != 95 || banked.objective.WeightedDiscardedPct != 5 {
		t.Fatalf("physical banked accounting=%+v restored=%+v ok=%t", banked.objective, restored, ok)
	}
	banked = advanceSimulationTo(banked, input.Now.Add(47500*time.Second))
	if remaining := banked.accounts["system"].windows[quota.Window7Day].remaining; remaining != 5 || simulationAccountAvailable(banked.accounts["system"]) {
		t.Fatalf("banked reserve did not rearm: remaining=%g available=%t", remaining, simulationAccountAvailable(banked.accounts["system"]))
	}
	natural := processSimulationEvent(state, simulationEvent{kind: simulationEventNaturalReset, accountKey: "system", window: quota.Window7Day})
	natural = advanceSimulationTo(natural, input.Now.Add(47500*time.Second))
	if remaining := natural.accounts["system"].windows[quota.Window7Day].remaining; remaining != 5 || simulationAccountAvailable(natural.accounts["system"]) {
		t.Fatalf("natural reserve did not rearm: remaining=%g", remaining)
	}
}

func TestReserveNaturalReleaseUsesPooledDemandAndMargin(t *testing.T) {
	input := reserveResetInput()
	window := input.Accounts[0].Windows[quota.Window7Day]
	window.ResetAt = input.Now.Add(time.Hour)
	input.Accounts[0].Windows[quota.Window7Day] = window
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	// 5 points / .002 points per second = 2500 seconds, plus 300 seconds.
	if !ok || !event.at.Equal(input.Now.Add(800*time.Second)) {
		t.Fatalf("natural release event=%+v ok=%t", event, ok)
	}
	before := advanceSimulationTo(state, input.Now.Add(799*time.Second))
	if simulationActiveMultiplier(before) != 0 || before.accounts["system"].windows[quota.Window7Day].remaining != 5 {
		t.Fatal("reserve consumed before natural release")
	}
	state = processSimulationEvent(advanceSimulationTo(state, event.at), event)
	if simulationActiveMultiplier(state) != 1 {
		t.Fatal("natural release did not restore usable quota")
	}
	state = advanceSimulationTo(state, state.now.Add(100*time.Second))
	if remaining := state.accounts["system"].windows[quota.Window7Day].remaining; math.Abs(remaining-4.8) > 1e-9 {
		t.Fatalf("released reserve remaining=%g, want 4.8", remaining)
	}
}

func TestReserveRejectsInvalidFloorsAndFirstAccount(t *testing.T) {
	for _, floor := range []float64{-1, 100, math.NaN(), math.Inf(1)} {
		input := reserveResetInput()
		window := input.Accounts[0].Windows[quota.Window7Day]
		window.ReservePct = floor
		input.Accounts[0].Windows[quota.Window7Day] = window
		if got := RecommendResetSchedule(input); got.Complete || len(got.Blockers) == 0 {
			t.Fatalf("accepted invalid floor %g", floor)
		}
	}
	input := reserveResetInput()
	input.Constraints.NextAccountKey = "missing"
	if got := RecommendResetSchedule(input); got.Complete || len(got.Blockers) == 0 {
		t.Fatal("accepted first account without an accepted credit")
	}
	input = reserveResetInput()
	window := input.Accounts[0].Windows[quota.Window7Day]
	window.ReserveReleased = true
	input.Accounts[0].Windows[quota.Window7Day] = window
	if got := RecommendResetSchedule(input); got.Complete || len(got.Blockers) == 0 {
		t.Fatal("released reserve while next reset targets another account")
	}
}

func TestReserveFirstAccountRejectsIncompatiblePinnedConsumption(t *testing.T) {
	input := reserveResetInput()
	input.Constraints.NextAccountKey = "system"
	input.Constraints.FixedUses = []ResetScheduleFixedUse{{AccountKey: "other", CreditID: "other-credit", At: input.Now}}
	state, blockers := normaliseResetScheduleInput(input)
	if len(blockers) != 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	branches := expandResetSearchEvent(resetSearchNode{state: state, useful: make(map[string]bool)}, simulationEvent{kind: simulationEventConsumption, accountKey: "other", creditID: "other-credit", at: input.Now})
	if len(branches) != 0 {
		t.Fatal("incompatible pinned consumption retained a repeating branch")
	}
}

func TestReserveNaturalReleaseWithNoDemandWaitsForMargin(t *testing.T) {
	input := reserveResetInput()
	for i := range input.Accounts {
		window := input.Accounts[i].Windows[quota.Window7Day]
		window.BurnPctPerS = 0
		window.ResetAt = input.Now.Add(time.Hour)
		input.Accounts[i].Windows[quota.Window7Day] = window
	}
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	if !ok || !event.at.Equal(input.Now.Add(55*time.Minute)) {
		t.Fatalf("zero-demand release=%+v ok=%t", event, ok)
	}
}

func TestReserveNaturalReleaseDoesNotBypassAnotherExhaustedWindow(t *testing.T) {
	input := reserveResetInput()
	window := input.Accounts[0].Windows[quota.Window7Day]
	window.ResetAt = input.Now.Add(time.Minute)
	input.Accounts[0].Windows[quota.Window7Day] = window
	input.Accounts[0].Windows[quota.Window5Hour] = resetTestWindow(0, input.Now.Add(5*time.Hour), 5*time.Hour, .001)
	state, _ := normaliseResetScheduleInput(input)
	event, ok := nextSimulationEvent(state)
	if !ok || !event.at.Equal(input.Now) {
		t.Fatalf("natural release=%+v ok=%t", event, ok)
	}
	state = processSimulationEvent(state, event)
	if simulationAccountAvailable(state.accounts["system"]) {
		t.Fatal("reserve release bypassed exhausted short window")
	}
}

func TestReserveFirstAccountSurvivesExecutionLeadProjection(t *testing.T) {
	input := reserveResetInput()
	input.Constraints.NextAccountKey = "system"
	expiry := input.Now.Add(5 * time.Second)
	for i := range input.Accounts {
		input.Accounts[i].Credits[0].ExpiresAt = &expiry
		window := input.Accounts[i].Windows[quota.Window7Day]
		window.BurnPctPerS = 1
		if i == 0 {
			window.ReserveReleased = true
		} else {
			window.ResetAt = input.Now.Add(3 * time.Second)
		}
		input.Accounts[i].Windows[quota.Window7Day] = window
	}
	got := RecommendResetSchedule(input)
	if !got.Complete {
		t.Fatalf("schedule incomplete: %+v", got)
	}
	var systemAt, otherAt time.Time
	for _, item := range got.Items {
		switch item.CreditID {
		case "system-credit":
			systemAt = item.UseAt
		case "other-credit":
			otherAt = item.UseAt
		}
	}
	if systemAt.IsZero() || otherAt.IsZero() || !systemAt.Before(otherAt) {
		t.Fatalf("execution lead reordered commitment: system=%s other=%s items=%+v", systemAt, otherAt, got.Items)
	}
}
