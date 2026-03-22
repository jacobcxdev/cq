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

// GaugeInfo holds the computed gauge position and supporting data for display.
type GaugeInfo struct {
	Pos           int     // -1=unknown, 0-6
	GapStart      float64 // seconds until dry spot, -1 if no gap
	GapDuration   float64 // duration of dry spot in seconds
	WastedPct     float64 // projected waste as fraction (0-1)
	WasteDeadline float64 // seconds until earliest wasting account resets, -1 if N/A
}

// computeSustainability computes the raw sustainability factor s: the maximum
// rate multiplier before dry spots appear. Retained for JSON output; the TTY
// gauge uses computeGaugeInfo instead.
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
				remaining: 100,
				rate:      math.SmallestNonzeroFloat64,
				reset:     float64(periodS),
			})
			continue
		}

		elapsed := windowElapsed(w, periodS, nowEpoch)
		if elapsed <= 0 {
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
// 0-2 = overburn (severe/moderate/mild), 3 = on pace, 4-6 = underburn (mild/moderate/severe).
//
// Left side: time until first dry spot at current rate, mapped to severity via
// proportional thresholds (≤10% of window = severe, 10-25% = moderate, >25% = mild).
//
// Right side: projected wasted quota weighted by elapsed fraction, mapped to
// equivalent thresholds.
func computeGaugeInfo(accounts []acctInfo, winName quota.WindowName, periodS int64, nowEpoch int64) GaugeInfo {
	unknown := GaugeInfo{Pos: -1, GapStart: -1, WasteDeadline: -1}
	period := float64(periodS)
	if period <= 0 {
		return unknown
	}

	var sustainers []sustainAccount
	hasData := false

	for _, a := range accounts {
		w, ok := a.result.Windows[winName]
		if !ok {
			continue
		}
		hasData = true
		if w.ResetAtUnix <= 0 && periodS > 0 {
			w.ResetAtUnix = quota.DefaultResetEpoch(periodS, nowEpoch)
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
		return unknown
	}
	if len(sustainers) == 0 {
		return GaugeInfo{Pos: 0, GapStart: 0, GapDuration: period, WasteDeadline: -1}
	}

	// Check effective coverage at t=0 instead of raw remaining.
	ivs := buildIntervals(sustainers, 1.0, period)
	gapStart, gapEnd, hasGap := firstGapSpan(ivs, period)

	if hasGap {
		dur := gapEnd - gapStart
		frac := gapStart / period
		pos := 2 // mild
		if frac <= 0.10 {
			pos = 0 // severe: dry spot within 10% of window
		} else if frac <= 0.25 {
			pos = 1 // moderate: within 25%
		}
		return GaugeInfo{
			Pos:           pos,
			GapStart:      gapStart,
			GapDuration:   dur,
			WasteDeadline: -1,
		}
	}

	// No overburn. Check underburn: projected wasted quota.
	waste, deadline := projectedWasteInfo(sustainers, period)
	if waste <= 0.02 {
		return GaugeInfo{Pos: 3, GapStart: -1, WasteDeadline: -1}
	}
	pos := 4 // mild
	if waste > 0.25 {
		pos = 6 // severe
	} else if waste > 0.10 {
		pos = 5 // moderate
	}
	return GaugeInfo{
		Pos:           pos,
		GapStart:      -1,
		WastedPct:     waste,
		WasteDeadline: deadline,
	}
}

// buildIntervals constructs coverage intervals for the given rate multiplier f.
// Each account provides up to two intervals: one from current remaining, one
// after its window resets with full quota. Gate offsets clip coverage start.
func buildIntervals(sustainers []sustainAccount, f float64, period float64) []interval {
	ivs := make([]interval, 0, len(sustainers)*2)
	for _, a := range sustainers {
		gate := a.gateOffset

		if a.rate <= math.SmallestNonzeroFloat64*2 {
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

// projectedWasteInfo computes the fraction of total capacity (0-1) that will go
// unused when windows reset, weighted by how far each account is into its
// window. Also returns the earliest reset time among wasting accounts.
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
