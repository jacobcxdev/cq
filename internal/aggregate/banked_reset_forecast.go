package aggregate

import (
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// applyBankedResetForecast follows the accepted schedule with the same refill
// and weighted demand model used by reset recommendations. It never searches
// for alternate credit timings or changes observed quota.
func applyBankedResetForecast(result *quota.AggregateResult, accounts []acctInfo, name quota.WindowName, period, now int64) {
	if period <= 0 || (name != quota.Window5Hour && name != quota.Window7Day) {
		return
	}
	if !slices.ContainsFunc(accounts, func(a acctInfo) bool { return a.result.BankedResets != nil }) {
		return
	}
	state := simulationState{
		now: time.Unix(now, 0), horizon: time.Unix(now+period, 0),
		accounts: make(map[string]simulationAccount), credits: make(map[string]simulationCredit),
		totalDemand: make(map[quota.WindowName]float64),
	}
	var firstReset int64
	var supply float64
	haveBankedHistory := false
	for i, a := range accounts {
		w, ok := a.result.Windows[name]
		if !ok {
			continue
		}
		current, ok := reportSimulationWindow(name, w, now)
		if !ok {
			return
		}
		key := strconv.Itoa(i)
		multiplier := float64(a.multiplier)
		windows := map[quota.WindowName]simulationWindow{name: current}
		if name == quota.Window5Hour {
			if weekly, exists := a.result.Windows[quota.Window7Day]; exists {
				gate, valid := reportSimulationWindow(quota.Window7Day, weekly, now)
				if !valid {
					return
				}
				windows[quota.Window7Day] = gate
			}
		}
		state.accounts[key] = simulationAccount{key: key, multiplier: multiplier, windows: windows}
		start, _ := a.result.BankedResets.WindowBounds(name, w, now)
		haveBankedHistory = haveBankedHistory || start > w.ResetAtUnix-period
		state.totalDemand[name] += multiplier * w.BurnRate(w.ResetAtUnix-start, now)
		supply += multiplier * current.remaining
		if a.result.BankedResets == nil {
			continue
		}
		for _, at := range a.result.BankedResets.ScheduledAtUnix {
			if at <= now || at >= now+period {
				continue
			}
			when := time.Unix(at, 0)
			id := key + ":" + strconv.FormatInt(at, 10)
			state.credits[id] = simulationCredit{id: id, accountKey: key, supported: true, fixedAt: &when}
			if firstReset == 0 || at < firstReset {
				firstReset = at
			}
		}
	}
	resetCount := len(state.credits)
	if resetCount == 0 && !haveBankedHistory {
		return
	}

	projected := simulateBankedResetForecast(state, name, firstReset)
	result.ScheduledResetCount, result.NextScheduledResetAtUnix = resetCount, firstReset
	result.GapStartS, result.GapDurationS, result.Burndown = 0, 0, 0
	result.WastedPct, result.WasteDeadlineS = 0, 0
	result.GaugePos, result.GaugeOverride = -1, ""
	if demand, forecastSupply := state.totalDemand[name], supply+projected.refillSupply; demand > 0 && forecastSupply > 0 {
		result.GaugePos = posFromRho(demand * float64(period) / forecastSupply)
	}
	if !projected.gapStart.IsZero() {
		result.GapStartS = int64(math.Round(projected.gapStart.Sub(state.now).Seconds()))
		result.GapDurationS = max(1, int64(math.Round(projected.gapEnd.Sub(projected.gapStart).Seconds())))
		result.Burndown = result.GapStartS
		result.GaugePos = max(0, min(2, result.GaugePos))
		if float64(result.GapStartS) < imminentThresholdFor(name) {
			result.GaugeOverride = "imminent_block"
		}
	} else if result.GaugePos >= 0 {
		result.GaugePos = max(3, result.GaugePos)
	}
	if projected.replacedWeight > 0 && !projected.wasteAt.IsZero() {
		result.WastedPct = int(math.Round(projected.discarded / projected.replacedWeight))
		result.WasteDeadlineS = projected.wasteAt.Unix() - now
	}
	if projected.nextResetWeight > 0 {
		remaining := int(math.Round(projected.nextResetRemaining / projected.nextResetWeight))
		result.RemainingAtNextResetPct = &remaining
		result.PaceAdvice, result.PaceTargetPct = bankedResetPaceAdvice(state, name, firstReset, supply, projected)
	}
}

type bankedResetProjection struct {
	gapStart, gapEnd, wasteAt           time.Time
	discarded, replacedWeight           float64
	refillSupply                        float64
	nextResetRemaining, nextResetWeight float64
	discardedThroughNext                float64
	naturalSupplyBeforeNext             float64
}

func simulateBankedResetForecast(state simulationState, name quota.WindowName, firstReset int64) bankedResetProjection {
	var p bankedResetProjection
	for {
		event, hasEvent := nextSimulationEvent(state)
		until := state.horizon
		if hasEvent {
			until = event.at
		}
		if until.After(state.now) && p.gapEnd.IsZero() {
			if simulationActiveMultiplier(state) == 0 {
				if p.gapStart.IsZero() {
					p.gapStart = state.now
				}
			} else if !p.gapStart.IsZero() {
				p.gapEnd = state.now
			}
		}
		state = advanceSimulationTo(state, until)
		if !hasEvent {
			break
		}
		if event.kind == simulationEventConsumption || (event.kind == simulationEventNaturalReset && event.window == name) {
			account := state.accounts[event.accountKey]
			left := account.windows[name].remaining
			if until.Before(state.horizon) {
				p.refillSupply += account.multiplier * (100 - left)
			}
			weighted := left * account.multiplier
			p.discarded += weighted
			p.replacedWeight += account.multiplier
			if until.Unix() <= firstReset {
				p.discardedThroughNext += weighted
			}
			if event.kind == simulationEventNaturalReset && until.Unix() < firstReset {
				p.naturalSupplyBeforeNext += 100 * account.multiplier
			}
			if event.kind == simulationEventConsumption && until.Unix() == firstReset {
				p.nextResetRemaining += weighted
				p.nextResetWeight += account.multiplier
			}
			if left > 0.01 && p.wasteAt.IsZero() {
				p.wasteAt = until
			}
		}
		if event.kind == simulationEventConsumption {
			state, _, _ = applySimulationCredit(state, event.accountKey, event.creditID)
		} else {
			state = processSimulationEvent(state, event)
		}
	}
	if !p.gapStart.IsZero() && p.gapEnd.IsZero() {
		p.gapEnd = state.horizon
	}
	return p
}

func bankedResetPaceAdvice(state simulationState, name quota.WindowName, nextReset int64, observedSupply float64, current bankedResetProjection) (quota.PaceAdvice, *int) {
	cutoff := time.Unix(nextReset, 0)
	phase := state
	phase.horizon = cutoff
	projectAt := func(pct int) bankedResetProjection {
		phase.totalDemand = map[quota.WindowName]float64{name: state.totalDemand[name] * float64(pct) / 100}
		return simulateBankedResetForecast(phase, name, nextReset)
	}
	// Search in whole percentages so the displayed pace itself clears the
	// gap test. Only the interval before the next refill constrains this pace.
	maxSafe := func(safe, unsafe int) int {
		for unsafe-safe > 1 {
			mid := (safe + unsafe) / 2
			if projectAt(mid).gapStart.IsZero() {
				safe = mid
			} else {
				unsafe = mid
			}
		}
		return safe
	}
	if !current.gapStart.IsZero() && current.gapStart.Before(cutoff) {
		if current.gapStart.Equal(state.now) {
			return quota.PaceNeedsRefill, nil
		}
		safe := maxSafe(0, 100)
		return quota.PaceSlowDown, &safe
	}
	demand := state.totalDemand[name]
	if demand <= 0 {
		return "", nil
	}
	if current.nextResetRemaining >= current.nextResetWeight {
		// Current quota and natural refills bound all work possible before
		// this credit. An upcoming banked reset contributes only at cutoff.
		gross := observedSupply + current.naturalSupplyBeforeNext
		bound := 100 * gross / (demand * cutoff.Sub(state.now).Seconds())
		if bound >= 101 && bound < float64(math.MaxInt)/2 {
			faster := maxSafe(100, int(bound)+1)
			probe := projectAt(faster)
			if faster > 100 && probe.gapStart.IsZero() && probe.nextResetRemaining < current.nextResetRemaining &&
				probe.discardedThroughNext < current.discardedThroughNext {
				return quota.PaceBurnHarder, &faster
			}
		}
	}
	return quota.PaceKeep, nil
}

func reportSimulationWindow(name quota.WindowName, w quota.Window, now int64) (simulationWindow, bool) {
	remaining := w.RemainingPercent()
	if w.ResetAtUnix <= now || remaining < 0 || remaining > 100 {
		return simulationWindow{}, false
	}
	return simulationWindow{remaining: remaining, resetAt: time.Unix(w.ResetAtUnix, 0), period: quota.PeriodFor(name)}, true
}
