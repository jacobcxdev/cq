package aggregate

import (
	"math"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func resetSimulationDemand(state simulationState, name quota.WindowName) float64 {
	demand := state.totalDemand[name]
	if state.now.Before(state.paceUntil) {
		demand *= float64(state.pacePercent) / 100
	}
	return demand
}

// Settle all windows due to run out at this instant. Rounded event times
// can leave a fractional remainder, hiding a gap until another event at
// the same instant, when there is no time left to model a slowdown.
func finishAdaptiveExhaustions(previous, state simulationState) simulationState {
	active := simulationActiveMultiplier(previous)
	cloned := false
	for key, account := range previous.accounts {
		if !simulationAccountAvailable(account) {
			continue
		}
		for name, before := range account.windows {
			if at, ok := simulationExhaustionAt(previous, name, before, active); ok && at.Equal(state.now) {
				if !cloned {
					state = cloneSimulationState(state)
					cloned = true
				}
				current := state.accounts[key].windows[name]
				current.remaining = simulationReserveFloor(current)
				state.accounts[key].windows[name] = current
			}
		}
	}
	return state
}

func (node *resetSearchNode) recordStateAndPace(state simulationState) {
	node.state = state
	if node.nextPace == nil && state.now.Before(state.paceUntil) {
		node.nextPace = &quota.ResetPacePhase{From: state.now, Until: state.paceUntil, Percent: state.pacePercent}
	}
}

// Retry with the slowdown spread from the first redemption, or from now if
// a slowdown is already in progress. Keep the new plan only if it protects
// coverage and use of expiring credits.
func smoothAdaptiveResetPace(initial simulationState, baseline resetSearchNode, baselineExact, continuing bool) (resetSearchNode, bool) {
	phase := baseline.nextPace
	if phase == nil {
		return baseline, baselineExact
	}
	start := initial.now
	if !continuing {
		if len(baseline.consumptions) == 0 {
			return baseline, baselineExact
		}
		start = baseline.consumptions[0].useBy
	}
	if !start.Before(phase.From) {
		return baseline, baselineExact
	}
	span := phase.Until.Sub(start).Seconds()
	deferred := float64(100-phase.Percent) * phase.Until.Sub(phase.From).Seconds()
	percent := min(99, int(math.Floor(100-deferred/span+1e-9)))
	if continuing {
		initial.paceUntil = phase.Until
		initial.pacePercent = percent
	} else {
		initial.paceAfterFirst = &quota.ResetPacePhase{From: start, Until: phase.Until, Percent: percent}
	}
	candidate, exact, feasible := searchResetSchedule(initial)
	if !feasible || candidate.nextPace == nil || !candidate.nextPace.From.Equal(start) ||
		candidate.state.objective.GapDurationSeconds > baseline.state.objective.GapDurationSeconds ||
		expiringResetUses(initial, candidate) < expiringResetUses(initial, baseline) {
		return baseline, baselineExact
	}
	if !continuing && (len(candidate.consumptions) == 0 ||
		candidate.consumptions[0].creditID != baseline.consumptions[0].creditID) {
		return baseline, baselineExact
	}

	// Both quantities are weighted quota percentages. Allow at most one
	// percentage point of pace rounding over the changed phase.
	var rounding float64
	for _, name := range sharedResetWindows() {
		rounding += initial.totalDemand[name] * span / 100
	}
	if candidate.state.objective.UnmetDemandPctSeconds > baseline.state.objective.UnmetDemandPctSeconds+rounding ||
		candidate.state.objective.WeightedDiscardedPct > baseline.state.objective.WeightedDiscardedPct+rounding {
		return baseline, baselineExact
	}
	return candidate, exact
}

func expiringResetUses(initial simulationState, node resetSearchNode) int {
	count := 0
	for _, use := range node.consumptions {
		if initial.credits[use.creditID].expiresAt != nil {
			count++
		}
	}
	return count
}

// bridgeAdaptiveReset stretches the last spendable tranche when exhaustion
// would leave the pool dry and no credit can be redeemed without waste.
func bridgeAdaptiveReset(node resetSearchNode, depleted simulationState) (resetSearchNode, bool) {
	start := node.state.now
	if !depleted.now.After(start) || start.Before(node.state.paceUntil) {
		return node, false
	}
	until := nextAdaptiveRefill(depleted)
	if !until.After(depleted.now) {
		return node, false
	}
	// The depletion event was the next event from start. Slowing its
	// proportional burn preserves that tranche until the refill. Any
	// intervening recovery can only add capacity to this estimate.
	percent := min(99, int(math.Floor(100*float64(depleted.now.Sub(start))/float64(until.Sub(start))+1e-9)))
	node.state.paceUntil = until
	node.state.pacePercent = percent
	if node.nextPace == nil {
		node.nextPace = &quota.ResetPacePhase{From: start, Until: until, Percent: percent}
	}
	return node, true
}

func nextAdaptiveRefill(depleted simulationState) time.Time {
	depleted.totalDemand = nil
	depleted.paceUntil = time.Time{}
	depleted.paceAfterFirst = nil
	through := depleted.now.Add(quota.PeriodFor(quota.Window7Day) + quota.PeriodFor(quota.Window5Hour))
	if through.After(depleted.horizon) {
		depleted.horizon = through
	}
	for {
		event, ok := nextSimulationEvent(depleted)
		if !ok {
			return time.Time{}
		}
		depleted.now = event.at
		if event.kind == simulationEventConsumption {
			var applied bool
			depleted, _, applied = applySimulationCredit(depleted, event.accountKey, event.creditID)
			if !applied {
				return time.Time{}
			}
		} else {
			depleted = processSimulationEvent(depleted, event)
		}
		if simulationActiveMultiplier(depleted) > 0 {
			return depleted.now
		}
	}
}
