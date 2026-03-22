package aggregate

import (
	"math"
	"slices"

	"github.com/jacobcxdev/cq/internal/quota"
)

type sustainAccount struct {
	remaining  float64
	rate       float64
	reset      float64 // seconds until this window resets
	elapsed    float64 // seconds elapsed in this window
	gateOffset float64 // seconds before which account is unavailable (weekly-gated)
}

type interval struct {
	start float64
	end   float64
}

// computeSustainability computes the raw sustainability factor s: the maximum
// rate multiplier before dry spots appear. Retained for JSON output; the TTY
// gauge uses computeGaugePos instead.
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
			// Unused account — provides indefinite coverage at current rate.
			// Use near-zero rate so binary search treats it as infinite.
			allElapsedZero = false
			sustainers = append(sustainers, sustainAccount{
				remaining: 100,
				rate:      math.SmallestNonzeroFloat64,
				reset:     float64(periodS),
			})
			continue
		}

		elapsed := windowElapsed(w, periodS, nowEpoch)
		if elapsed <= 0 {
			// Rate unidentifiable — use period-average as bounded fallback.
			elapsed = float64(periodS)
		}
		allElapsedZero = false

		sustainers = append(sustainers, sustainAccount{
			remaining: float64(w.RemainingPct),
			rate:      used / elapsed,
			reset:     max(0, min(float64(w.ResetAtUnix-nowEpoch), float64(periodS))),
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
		ivs := buildIntervals(sustainers, f, float64(periodS))
		return firstGap(ivs, float64(periodS)) < 0
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

// computeGaugePos returns a gauge position (0-6) or -1 for unknown.
// 0-2 = overburn (severe/moderate/mild), 3 = on pace, 4-6 = underburn (mild/moderate/severe).
//
// Left side: time until first dry spot at current rate, mapped to severity via
// proportional thresholds (≤10% of window = severe, 10-25% = moderate, >25% = mild).
//
// Right side: projected wasted quota weighted by elapsed fraction, mapped to
// equivalent thresholds.
func computeGaugePos(accounts []acctInfo, winName quota.WindowName, periodS int64, nowEpoch int64) int {
	period := float64(periodS)
	if period <= 0 {
		return -1
	}

	var sustainers []sustainAccount
	hasData := false
	allDepleted := true

	for _, a := range accounts {
		w, ok := a.result.Windows[winName]
		if !ok {
			continue
		}
		hasData = true
		// Normalize reset time.
		if w.ResetAtUnix <= 0 && periodS > 0 {
			w.ResetAtUnix = quota.DefaultResetEpoch(periodS, nowEpoch)
		}

		if w.RemainingPct > 0 {
			allDepleted = false
		}

		// Handle weekly-gated 5h accounts (Issue 1).
		if winName == quota.Window5Hour && weeklyExhausted(a.result) {
			w7d, ok := a.result.Windows[quota.Window7Day]
			if !ok || w7d.ResetAtUnix <= 0 {
				continue
			}
			offset := float64(w7d.ResetAtUnix - nowEpoch)
			if offset > period {
				continue // 7d reset beyond 5h horizon
			}
			if offset < 0 {
				offset = 0
			}
			// After the weekly gate lifts, assume full 5h quota.
			sustainers = append(sustainers, sustainAccount{
				remaining:  100,
				rate:       math.SmallestNonzeroFloat64,
				reset:      period,
				elapsed:    0,
				gateOffset: offset,
			})
			continue
		}

		used := float64(100 - w.RemainingPct)
		elapsed := windowElapsed(w, periodS, nowEpoch)

		if used <= 0 {
			sustainers = append(sustainers, sustainAccount{
				remaining: 100,
				rate:      math.SmallestNonzeroFloat64,
				reset:     period,
				elapsed:   elapsed,
			})
			continue
		}

		if elapsed <= 0 {
			elapsed = period // Issue 3: bounded fallback rate
		}

		sustainers = append(sustainers, sustainAccount{
			remaining: float64(w.RemainingPct),
			rate:      used / elapsed,
			reset:     max(0, min(float64(w.ResetAtUnix-nowEpoch), period)),
			elapsed:   elapsed,
		})
	}

	if !hasData {
		return -1
	}
	if len(sustainers) == 0 {
		return 0 // all accounts present but none can provide coverage
	}
	if allDepleted {
		return 0
	}

	// Overburn: at current rate (f=1), find first coverage gap.
	ivs := buildIntervals(sustainers, 1.0, period)
	gap := firstGap(ivs, period)

	if gap >= 0 {
		frac := gap / period
		if frac <= 0.10 {
			return 0 // severe: dry spot within 10% of window
		}
		if frac <= 0.25 {
			return 1 // moderate: within 25%
		}
		return 2 // mild: beyond 25%
	}

	// Underburn: projected wasted quota weighted by elapsed.
	waste := projectedWaste(sustainers, period)
	if waste <= 0.02 {
		return 3 // on pace
	}
	if waste <= 0.10 {
		return 4 // mild underburn
	}
	if waste <= 0.25 {
		return 5 // moderate underburn
	}
	return 6 // severe underburn
}

// buildIntervals constructs coverage intervals for the given rate multiplier f.
// Each account provides up to two intervals: one from current remaining, one
// after its window resets with full quota. Gate offsets clip coverage start.
func buildIntervals(sustainers []sustainAccount, f float64, period float64) []interval {
	ivs := make([]interval, 0, len(sustainers)*2)
	for _, a := range sustainers {
		gate := a.gateOffset

		if a.rate <= math.SmallestNonzeroFloat64*2 {
			// Near-zero rate: covers from gate to period.
			if gate < period {
				ivs = append(ivs, interval{start: gate, end: period})
			}
			continue
		}

		// Current remaining: from gate to min(reset, gate + remaining/(f*rate)).
		if a.remaining > 0 {
			dur := a.remaining / (f * a.rate)
			end := math.Min(a.reset, gate+dur)
			if end > gate {
				ivs = append(ivs, interval{start: gate, end: end})
			}
		}

		// After reset: from max(reset, gate) to min(period, start + 100/(f*rate)).
		if a.reset < period {
			start := math.Max(a.reset, gate)
			dur := 100.0 / (f * a.rate)
			end := math.Min(period, start+dur)
			if end > start {
				ivs = append(ivs, interval{start: start, end: end})
			}
		}
	}
	return ivs
}

// firstGap returns the time of the first coverage gap, or -1 if fully covered.
func firstGap(intervals []interval, period float64) float64 {
	if period <= 0 {
		return -1
	}
	if len(intervals) == 0 {
		return 0
	}

	slices.SortFunc(intervals, func(a, b interval) int {
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
	})

	covered := 0.0
	for _, iv := range intervals {
		if iv.end <= covered {
			continue
		}
		if iv.start > covered {
			return covered
		}
		covered = iv.end
		if covered >= period {
			return -1
		}
	}
	if covered < period {
		return covered
	}
	return -1
}

// projectedWaste computes the fraction of total capacity (0-1) that will go
// unused when windows reset, weighted by how far each account is into its
// window. Early-window accounts contribute less urgency since their rate
// projection is less reliable.
func projectedWaste(sustainers []sustainAccount, period float64) float64 {
	var totalUrgency, totalWeight float64
	for _, a := range sustainers {
		// Skip accounts with unmeasurable rate.
		if a.rate <= math.SmallestNonzeroFloat64*2 {
			continue
		}
		timeToReset := a.reset - a.gateOffset
		if timeToReset <= 0 {
			continue
		}
		projected := a.remaining - a.rate*timeToReset
		if projected <= 0 {
			continue // will use all remaining before reset
		}
		wasteFrac := projected / 100.0
		timingWeight := a.elapsed / period
		if timingWeight > 1 {
			timingWeight = 1
		}
		totalUrgency += wasteFrac * timingWeight
		totalWeight += 1.0
	}
	if totalWeight == 0 {
		return 0
	}
	return totalUrgency / totalWeight
}

// coversPeriod checks whether the union of intervals covers [0, period].
func coversPeriod(intervals []interval, period float64) bool {
	if period <= 0 || len(intervals) == 0 {
		return false
	}
	return firstGap(intervals, period) < 0
}
