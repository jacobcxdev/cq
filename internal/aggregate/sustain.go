package aggregate

import (
	"math"
	"slices"

	"github.com/jacobcxdev/cq/internal/quota"
)

type sustainAccount struct {
	remaining float64
	rate      float64
	reset     float64
}

type interval struct {
	start float64
	end   float64
}

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
			continue
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

	intervalBuf := make([]interval, 0, len(sustainers)*2)
	covers := func(f float64) bool {
		if f == 0 {
			return false
		}
		intervals := intervalBuf[:0]
		period := float64(periodS)

		for _, a := range sustainers {
			if a.rate <= 0 {
				return true
			}

			dNow := a.remaining / (f * a.rate)
			endNow := math.Min(a.reset, dNow)
			if endNow > 0 {
				intervals = append(intervals, interval{start: 0, end: endNow})
			}

			dFull := 100.0 / (f * a.rate)
			if a.reset < period {
				endAfter := math.Min(period, a.reset+dFull)
				if endAfter > a.reset {
					intervals = append(intervals, interval{start: a.reset, end: endAfter})
				}
			}
		}

		return coversPeriod(intervals, float64(periodS))
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

func coversPeriod(intervals []interval, period float64) bool {
	if period <= 0 {
		return false
	}
	if len(intervals) == 0 {
		return false
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
			return false
		}
		covered = iv.end
		if covered >= period {
			return true
		}
	}
	return covered >= period
}
