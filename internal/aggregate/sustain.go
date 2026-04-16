package aggregate

import (
	"math"
	"slices"

	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/quota"
)

type sustainAccount struct {
	remaining  float64
	rate       float64 // used / elapsed_in_window (cumulative, phase-invariant)
	ewmaRate   float64 // from burnRates, secondary signal for override only
	reset      float64 // seconds until this window resets
	elapsed    float64 // seconds elapsed in this window
	gateOffset float64 // seconds before which account is unavailable (weekly-gated)
	multiplier float64
}

type interval struct {
	start float64
	end   float64
}

// GaugeInfo holds the computed gauge position and supporting data for display.
type GaugeInfo struct {
	Pos           int     // -1=unknown, 0-6
	GapStart      float64 // seconds until dry spot, -1 if no gap
	GapDuration   float64 // duration of dry spot in seconds
	WastedPct     float64 // projected waste as fraction (0-1)
	WasteDeadline float64 // seconds until earliest wasting account resets, -1 if N/A
	// Override is set when the rate-ratio severity was overridden by an
	// imminent-block check. Values: "", "imminent_block".
	Override string
}

// computeSustainability computes the raw sustainability factor s: the maximum
// rate multiplier before dry spots appear. Retained for JSON output; the TTY
// gauge uses computeGaugeInfo instead.
//
// This path intentionally uses the per-account rate for buildIntervals
// (allocatedRate = 0) so the returned scalar stays bit-identical to the
// pre-phase-stability-fix behaviour and the JSON "sustainability" field
// keeps backward compatibility.
func computeSustainability(accounts []acctInfo, winName quota.WindowName, periodS int64, nowEpoch int64) float64 {
	const (
		maxF       = 100.0
		iterations = 50
		precision  = 0.01
	)

	if periodS <= 0 {
		return -1
	}

	var sustainers []sustainAccount
	allElapsedZero := true
	allDepleted := true

	for _, a := range accounts {
		w, ok := a.result.Windows[winName]
		if !ok {
			continue
		}

		if w.RemainingPct > 0 {
			allDepleted = false
		}

		used := float64(100 - w.RemainingPct)
		if used <= 0 {
			allElapsedZero = false
			sustainers = append(sustainers, sustainAccount{
				remaining:  100,
				rate:       math.SmallestNonzeroFloat64,
				reset:      float64(periodS),
				multiplier: float64(a.multiplier),
			})
			continue
		}

		elapsed := windowElapsed(w, periodS, nowEpoch)
		if elapsed <= 0 {
			elapsed = float64(periodS)
		}
		allElapsedZero = false

		sustainers = append(sustainers, sustainAccount{
			remaining:  float64(w.RemainingPct),
			rate:       used / elapsed,
			reset:      max(0, min(float64(w.ResetAtUnix-nowEpoch), float64(periodS))),
			multiplier: float64(a.multiplier),
		})
	}

	if allDepleted {
		return 0
	}
	if allElapsedZero {
		return -1
	}
	if len(sustainers) == 0 {
		return 0
	}

	covers := func(f float64) bool {
		if f == 0 {
			return false
		}
		// allocatedRate=0 → per-account rate path, identical to prior behaviour.
		ivs := buildIntervals(sustainers, f, float64(periodS), 0)
		return coversPeriod(ivs, float64(periodS))
	}

	lo, hi := 0.0, 1.0
	if covers(hi) {
		lo = hi
		for hi < maxF {
			next := math.Min(maxF, hi*2)
			if next == hi || !covers(next) {
				hi = next
				break
			}
			lo = next
			hi = next
		}
		if lo == maxF {
			return maxF
		}
	} else {
		hi = 1.0
	}

	for i := 0; i < iterations && hi-lo > precision; i++ {
		mid := (lo + hi) / 2
		if covers(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}

	return lo
}

// computeGaugeInfo returns gauge position and supporting data for display.
// 0-2 = overburn (severe/moderate/mild), 3 = on pace, 4-6 = underburn
// (mild/moderate/severe).
//
// Severity (the rho bucket) is driven by a phase-invariant rate ratio
// rho = totalDemand / totalSupply computed from the cumulative within-window
// rate used / elapsed_in_window. This answers the integral question "am I
// on track cumulatively within this window?" — the right question for the
// gauge — and is available without any persistent state.
//
// The EWMA burn store (internal/history) is a secondary signal that only
// feeds the imminent-block override. A "hot" EWMA relative to cumulative
// indicates a recent ramp-up that cumulative rho has not caught yet; if
// the ramp-up projects a gap within the window's imminent threshold we
// set Override so the renderer can escalate the warning glyph without
// rewriting the natural rho-derived Pos. Displayed GapStart/Duration stay
// on the cumulative pass so the user's mental model is coherent.
//
// Displayed GapStart/Duration are populated only from the cumulative pass
// so the gauge direction, the dry-spot deadline, and the pace-diff column
// all tell the same story.
func computeGaugeInfo(
	accounts []acctInfo,
	winName quota.WindowName,
	periodS int64,
	nowEpoch int64,
	burnRates history.BurnRates,
) GaugeInfo {
	unknown := GaugeInfo{Pos: -1, GapStart: -1, WasteDeadline: -1}
	period := float64(periodS)
	if period <= 0 {
		return unknown
	}

	sustainers, totalSupply, totalDemand, sumActiveMulti, haveData, hasWindowData :=
		buildSustainersAndRatios(accounts, winName, period, nowEpoch, burnRates)

	if !hasWindowData {
		return unknown
	}
	if len(sustainers) == 0 {
		// No sustainable intervals can be built: all accounts gated out of
		// scope. Treat as severe overburn so the user sees the warning, and
		// point the gap duration at the whole window.
		return GaugeInfo{Pos: 0, GapStart: 0, GapDuration: period, WasteDeadline: -1}
	}

	// "Everyone is unusable" short-circuit: if no account has any currently
	// accessible quota (all remaining=0 or all gated beyond the window),
	// the gauge is severe regardless of what rate data says. This catches
	// cold-start paths where the user has just depleted every account.
	allEmptyOrGated := true
	earliestGateOrReset := period
	for _, s := range sustainers {
		if s.remaining > 0 && s.gateOffset < period {
			allEmptyOrGated = false
			break
		}
		if s.gateOffset >= period && s.reset < earliestGateOrReset {
			earliestGateOrReset = s.reset
		}
		if s.remaining == 0 && s.reset < earliestGateOrReset {
			earliestGateOrReset = s.reset
		}
	}
	if allEmptyOrGated {
		return GaugeInfo{
			Pos:           0,
			GapStart:      earliestGateOrReset,
			GapDuration:   period - earliestGateOrReset,
			WasteDeadline: -1,
		}
	}

	// With no cumulative burn signal on any active account (every active
	// account is still fresh, used=0) we cannot compute rho; fall back to
	// Pos=-1 (dim dashes in the renderer). This is now a genuine
	// cold-start: zero activity rather than missing persistent state.
	if !haveData || totalSupply <= 0 {
		return unknown
	}

	rho := totalDemand / totalSupply
	gi := GaugeInfo{
		Pos:           posFromRho(rho),
		GapStart:      -1,
		WasteDeadline: -1,
	}

	// Pass 1: cumulative drives the displayed GapStart/Duration and the
	// severity position via rho above. allocatedRateCum is the uniform
	// drain rate each active account sees under the proportional-to-
	// multiplier load model: own-pct/s = D / Σm_active.
	//
	// TODO(jacob): If internal/proxy/router.go ever diverges from
	// proportional-to-multiplier load balancing (e.g. adopts
	// highest-remaining-first), this simulator must be updated in lockstep.
	var allocatedRateCum float64
	if sumActiveMulti > 0 {
		allocatedRateCum = totalDemand / sumActiveMulti
	}
	ivsCum := buildIntervals(sustainers, 1.0, period, allocatedRateCum)
	gapStartCum, gapEndCum, hasGapCum := firstGapSpan(ivsCum, period)
	if hasGapCum {
		gi.GapStart = gapStartCum
		gi.GapDuration = gapEndCum - gapStartCum
	}

	// Pass 2: max-rate pass drives the imminent-block override only.
	// Per-account max(cumulative, ewma) captures recent ramp-ups that the
	// integral view hasn't caught yet. If EWMA adds nothing (no entry, or
	// every EWMA ≤ cumulative), skip the second simulator call and reuse
	// the cumulative gap for the imminent check.
	ewmaBoost := false
	totalDemandMax := totalDemand
	for _, s := range sustainers {
		if s.remaining <= 0 || s.gateOffset >= period {
			continue
		}
		if s.ewmaRate > s.rate {
			totalDemandMax += s.multiplier * (s.ewmaRate - s.rate)
			ewmaBoost = true
		}
	}
	if ewmaBoost && sumActiveMulti > 0 {
		allocatedRateMax := totalDemandMax / sumActiveMulti
		ivsMax := buildIntervals(sustainers, 1.0, period, allocatedRateMax)
		if gapStartMax, _, hasGapMax := firstGapSpan(ivsMax, period); hasGapMax &&
			gapStartMax >= 0 && gapStartMax < imminentThresholdFor(winName) {
			gi.Override = "imminent_block"
		}
	} else if hasGapCum && gapStartCum >= 0 && gapStartCum < imminentThresholdFor(winName) {
		// Cumulative pass already sees an imminent gap — the override
		// still fires even without an EWMA boost so the renderer shows
		// the warning glyph while preserving the natural rho-derived Pos.
		gi.Override = "imminent_block"
	}

	// Populate projected-waste display fields (timing-weighted UX metric,
	// not used for severity bucketing). These can numerically disagree with
	// the rho-driven gauge — by design.
	waste, deadline := projectedWasteInfo(sustainers, period)
	gi.WastedPct = waste
	gi.WasteDeadline = deadline

	return gi
}

// buildSustainersAndRatios constructs sustainAccount records for the given
// accounts and window, while simultaneously computing:
//
//   - totalSupply: Σ(m_i × 100 / period) for currently-active accounts
//     (remaining > 0 and not gated). Exhausted and gated accounts contribute
//     zero supply while unavailable.
//   - totalDemand: Σ(m_i × rate_i) using the per-account cumulative rate
//     (used/elapsed), a phase-invariant "am I on track within this window?"
//     signal that does not depend on persistent EWMA data.
//   - sumActiveMulti: Σ(m_i) for active accounts with remaining quota and no
//     gate. Drives the uniform allocatedRate in the interval simulator.
//   - haveData: true if at least one active account has a positive cumulative
//     rate. Cold-start (every active account fresh, used=0) still returns
//     false so the gauge falls back to Pos=-1.
//   - hasWindowData: true if at least one account had this window at all.
//
// EWMA rates from burnRates are read into sustainAccount.ewmaRate as a
// secondary signal; they are only consulted by the max-rate override pass
// in computeGaugeInfo and never contribute to rho.
func buildSustainersAndRatios(
	accounts []acctInfo,
	winName quota.WindowName,
	period float64,
	nowEpoch int64,
	burnRates history.BurnRates,
) (
	sustainers []sustainAccount,
	totalSupply, totalDemand, sumActiveMulti float64,
	haveData, hasWindowData bool,
) {
	periodS := int64(period)
	allWeeklyExhausted := quota.BaseWindow(winName) == quota.Window5Hour && allWeeklyExhaustedForSession(accounts, winName)

	for _, a := range accounts {
		w, ok := a.result.Windows[winName]
		if !ok {
			continue
		}
		hasWindowData = true
		if w.ResetAtUnix <= 0 && periodS > 0 {
			w.ResetAtUnix = quota.DefaultResetEpoch(periodS, nowEpoch)
		}

		mult := float64(a.multiplier)
		remaining := float64(w.RemainingPct)
		elapsed := windowElapsed(w, periodS, nowEpoch)
		reset := max(0, min(float64(w.ResetAtUnix-nowEpoch), period))

		var gateOffset float64
		gated := false

		// Weekly gating: 5h window sits behind a 7d gate when the 7d is
		// exhausted. If every 5h account is gated, we treat them as gated
		// with a zero-length offset so the aggregate still reports data
		// instead of going silent.
		if quota.BaseWindow(winName) == quota.Window5Hour && weeklyExhausted(a.result, winName) {
			gateName := quota.Window7Day
			if bucket := quota.WindowBucket(winName); bucket != "" {
				bucketGate := quota.WindowName("7d:" + bucket)
				if _, ok := a.result.Windows[bucketGate]; ok {
					gateName = bucketGate
				}
			}
			w7d, have7d := a.result.Windows[gateName]
			if have7d && w7d.ResetAtUnix > 0 {
				offset := float64(w7d.ResetAtUnix - nowEpoch)
				if offset < 0 {
					offset = 0
				}
				if offset <= period {
					gateOffset = offset
					gated = true
				} else if allWeeklyExhausted {
					gateOffset = period
					gated = true
				} else {
					continue
				}
			} else if allWeeklyExhausted {
				gateOffset = period
				gated = true
			} else {
				continue
			}
		}

		// Cumulative rate: used / elapsed_in_window. This is phase-invariant
		// when burn is constant (used(t) = rate × elapsed → used/elapsed =
		// rate regardless of phase offset) and available on the very first
		// run without any persistent state. Fresh windows (used=0 or
		// elapsed=0) produce rate=0, which is "no signal yet" rather than
		// "definitely idle".
		used := 100.0 - remaining
		var rate float64
		if used > 0 && elapsed > 0 {
			rate = used / elapsed
		}

		// Secondary EWMA signal from the persistent burn store. Only used by
		// the max-rate override pass in computeGaugeInfo — never contributes
		// to rho. The store keys on account identity alone (provider not
		// captured), so we query with an empty provider field — see
		// internal/history.UpdateAndGetBurnRates.
		var ewmaRate float64
		accountKey := a.result.AccountID
		if accountKey == "" {
			accountKey = a.result.Email
		}
		if accountKey != "" {
			if r, have := burnRates.Get(history.BurnRateKey{
				ProviderID: "",
				AccountKey: accountKey,
				Window:     string(winName),
			}); have {
				ewmaRate = r
			}
		}

		// Active = usable and contributing to the simulator's drain model.
		// Exhausted accounts (remaining == 0) contribute neither demand nor
		// supply to rho — they sit in a dead zone until reset. Gated
		// accounts are dormant for the same reason. Both are still appended
		// to sustainers[] so the all-empty-or-gated shortcut can detect them.
		active := remaining > 0 && !gated
		if active {
			totalSupply += mult * 100.0 / period
			totalDemand += mult * rate
			sumActiveMulti += mult
			if rate > 0 {
				haveData = true
			}
		}

		sustainers = append(sustainers, sustainAccount{
			remaining:  remaining,
			rate:       rate,
			ewmaRate:   ewmaRate,
			reset:      reset,
			elapsed:    elapsed,
			gateOffset: gateOffset,
			multiplier: mult,
		})
	}
	return
}

// posFromRho buckets the demand/supply ratio into one of 7 gauge positions.
// Thresholds use the deficit (1 - 1/rho) on the overburn side and the surplus
// (1 - rho) on the underburn side, with a symmetric ±5% deadband at on-pace.
//
// Overburn thresholds:
//
//	deficit > 0.40 → severe (pos 0)   — rho > 1.667
//	deficit > 0.20 → moderate (pos 1) — rho > 1.250
//	otherwise      → mild (pos 2)     — 1.050 < rho ≤ 1.250
//
// Underburn thresholds:
//
//	surplus > 0.60 → severe (pos 6)   — rho < 0.400
//	surplus > 0.30 → moderate (pos 5) — rho < 0.700
//	otherwise      → mild (pos 4)     — 0.700 ≤ rho < 0.950
func posFromRho(rho float64) int {
	if rho >= 0.95 && rho <= 1.05 {
		return 3
	}
	if rho > 1.05 {
		deficit := 1.0 - 1.0/rho
		switch {
		case deficit > 0.40:
			return 0
		case deficit > 0.20:
			return 1
		default:
			return 2
		}
	}
	surplus := 1.0 - rho
	switch {
	case surplus > 0.60:
		return 6
	case surplus > 0.30:
		return 5
	default:
		return 4
	}
}

// imminentThresholdFor returns the warning threshold in seconds for the given
// window. When a coverage gap is predicted within this threshold,
// GaugeOverride is set to "imminent_block" while the natural rho-derived
// GaugePos is preserved. These values are ergonomic, not derived.
func imminentThresholdFor(win quota.WindowName) float64 {
	switch quota.BaseWindow(win) {
	case quota.Window5Hour:
		return 3600 // 1h
	case quota.Window7Day:
		return 21600 // 6h
	default:
		return 3600
	}
}

// buildIntervals constructs coverage intervals for the given rate multiplier f.
// Each account provides up to two intervals: one from current remaining, one
// after its window resets with full quota. Gate offsets clip coverage start.
//
// If allocatedRate > 0, every account drains at that uniform own-pct/s rate
// (the gauge path, proportional-to-multiplier load model). If allocatedRate
// is zero, the function falls back to the per-account fallback rate
// (f * a.rate), preserving the computeSustainability binary-search semantics.
func buildIntervals(sustainers []sustainAccount, f float64, period float64, allocatedRate float64) []interval {
	ivs := make([]interval, 0, len(sustainers)*2)
	for _, a := range sustainers {
		gate := a.gateOffset

		drainRate := allocatedRate
		if drainRate <= 0 {
			drainRate = f * a.rate
		}

		if drainRate <= math.SmallestNonzeroFloat64*2 {
			if gate < period {
				ivs = append(ivs, interval{start: gate, end: period})
			}
			continue
		}

		// Current remaining: from gate to min(reset, gate + remaining/drainRate).
		if a.remaining > 0 {
			dur := a.remaining / drainRate
			end := math.Min(a.reset, gate+dur)
			if end > gate {
				ivs = append(ivs, interval{start: gate, end: end})
			}
		}

		// After reset: from max(reset, gate) to min(period, start + 100/drainRate).
		if a.reset < period {
			start := math.Max(a.reset, gate)
			dur := 100.0 / drainRate
			end := math.Min(period, start+dur)
			if end > start {
				ivs = append(ivs, interval{start: start, end: end})
			}
		}
	}
	return ivs
}

func intervalCmp(a, b interval) int {
	if a.start != b.start {
		if a.start < b.start {
			return -1
		}
		return 1
	}
	if a.end < b.end {
		return -1
	}
	if a.end > b.end {
		return 1
	}
	return 0
}

// firstGap returns the time of the first coverage gap, or -1 if fully covered.
func firstGap(intervals []interval, period float64) float64 {
	start, _, found := firstGapSpan(intervals, period)
	if found {
		return start
	}
	return -1
}

// firstGapSpan returns the start and end of the first coverage gap.
// Returns found=false if the intervals fully cover [0, period].
func firstGapSpan(intervals []interval, period float64) (gapStart, gapEnd float64, found bool) {
	if period <= 0 {
		return 0, 0, false
	}
	if len(intervals) == 0 {
		return 0, period, true
	}

	slices.SortFunc(intervals, intervalCmp)

	covered := 0.0
	for _, iv := range intervals {
		if iv.end <= covered {
			continue
		}
		if iv.start > covered {
			return covered, iv.start, true
		}
		covered = iv.end
		if covered >= period {
			return 0, 0, false
		}
	}
	if covered < period {
		return covered, period, true
	}
	return 0, 0, false
}

// projectedWasteInfo computes the fraction of total capacity (0-1) that will
// go unused when windows reset, weighted by how far each account is into its
// window. Also returns the earliest reset time among wasting accounts.
//
// The elapsed/period timing weight is intentional UX: waste is more alarming
// when the window is nearly done. This is display-only — the gauge severity
// position is now driven by the rate ratio instead.
func projectedWasteInfo(sustainers []sustainAccount, period float64) (waste float64, deadline float64) {
	deadline = -1
	var totalUrgency, totalWeight float64
	for _, a := range sustainers {
		if a.rate <= math.SmallestNonzeroFloat64*2 {
			continue
		}
		timeToReset := a.reset - a.gateOffset
		if timeToReset <= 0 {
			continue
		}
		projected := a.remaining - a.rate*timeToReset
		if projected <= 0 {
			continue
		}
		wasteFrac := projected / 100.0
		timingWeight := a.elapsed / period
		if timingWeight > 1 {
			timingWeight = 1
		}
		totalUrgency += wasteFrac * timingWeight
		totalWeight += 1.0
		if deadline < 0 || a.reset < deadline {
			deadline = a.reset
		}
	}
	if totalWeight == 0 {
		return 0, -1
	}
	return totalUrgency / totalWeight, deadline
}

// coversPeriod checks whether the union of intervals covers [0, period].
func coversPeriod(intervals []interval, period float64) bool {
	if period <= 0 || len(intervals) == 0 {
		return false
	}
	return firstGap(intervals, period) < 0
}
