package quota

import "math"

// RemainingPercent uses upstream precision when available and valid.
func (w Window) RemainingPercent() float64 {
	if w.RemainingPctExact != nil {
		pct := *w.RemainingPctExact
		if !math.IsNaN(pct) && pct >= 0 && pct <= 100 {
			return pct
		}
	}
	return float64(w.RemainingPct)
}

// BurnRate chooses the faster of recent and whole-window consumption. This
// gives the shorter ETA and prevents idle periods extending the baseline.
func (w Window) BurnRate(periodS, nowEpoch int64) float64 {
	var rate float64
	elapsed := min(periodS, periodS-(w.ResetAtUnix-nowEpoch))
	used := 100 - w.RemainingPercent()
	if periodS > 0 && w.ResetAtUnix > 0 && elapsed > 0 && used > 0 && used <= 100 {
		rate = used / float64(elapsed)
	}
	if w.RecentBurnRate != nil {
		recent := *w.RecentBurnRate
		if !math.IsNaN(recent) && !math.IsInf(recent, 0) {
			rate = max(rate, recent)
		}
	}
	return rate
}

// Burndown returns seconds until exhaustion at the selected rate. False means
// no meaningful forecast, including no observed consumption.
func (w Window) Burndown(periodS, nowEpoch int64) (int64, bool) {
	remaining := w.RemainingPercent()
	if remaining <= 0 {
		return 0, true
	}
	rate := w.BurnRate(periodS, nowEpoch)
	if rate <= 0 || remaining > 100 {
		return 0, false
	}
	seconds := math.Round(remaining / rate)
	if math.IsInf(seconds, 0) || seconds >= float64(math.MaxInt64) {
		return 0, false
	}
	return int64(seconds), true
}
