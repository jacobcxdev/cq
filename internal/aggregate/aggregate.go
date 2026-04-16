package aggregate

import (
	"math"

	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/quota"
)

type acctInfo struct {
	result     quota.Result
	multiplier int
}

// Compute calculates aggregate pace for multiple Claude accounts across windows.
// Returns nil if fewer than 2 valid accounts exist.
//
// burnRates supplies per-account EWMA burn rates for the gauge math; a nil
// or empty map causes the gauge to cold-start (GaugePos = -1).
func Compute(results []quota.Result, nowEpoch int64, burnRates history.BurnRates) (map[quota.WindowName]quota.AggregateResult, *AccountSummary) {
	var valid []quota.Result
	for _, r := range results {
		if r.IsUsable() {
			valid = append(valid, r)
		}
	}
	if len(valid) < 2 {
		return nil, nil
	}

	accounts := make([]acctInfo, 0, len(valid))
	totalMulti := 0
	windowSet := make(map[quota.WindowName]struct{})
	for _, r := range valid {
		m := quota.ExtractMultiplier(r.RateLimitTier)
		accounts = append(accounts, acctInfo{result: r, multiplier: m})
		totalMulti += m
		for winName := range r.Windows {
			if quota.IsAggregable(winName) {
				windowSet[winName] = struct{}{}
			}
		}
	}

	summary := &AccountSummary{
		Count:      len(accounts),
		TotalMulti: totalMulti,
		Label:      BuildLabel(valid),
	}

	windowNames := make([]quota.WindowName, 0, len(windowSet))
	for winName := range windowSet {
		windowNames = append(windowNames, winName)
	}
	windowNames = quota.OrderedWindowNames(windowNames)

	agg := make(map[quota.WindowName]quota.AggregateResult)
	for _, winName := range windowNames {
		periodS := int64(quota.PeriodFor(winName).Seconds())
		result, ok := computeWindow(winName, periodS, accounts, nowEpoch, burnRates)
		if !ok {
			continue
		}
		agg[winName] = result
	}

	if len(agg) == 0 {
		return nil, nil
	}
	return agg, summary
}

func computeWindow(winName quota.WindowName, periodS int64, accounts []acctInfo, nowEpoch int64, burnRates history.BurnRates) (quota.AggregateResult, bool) {
	var sumRemaining, sumExpected, sumWeight float64
	// Burndown: Σ(pct_i * m_i) / Σ((100-pct_i) * m_i / elapsed_i)
	var burnNum, burnDen float64
	var sustainAccounts []acctInfo

	allWeeklyExhausted := quota.BaseWindow(winName) == quota.Window5Hour && allWeeklyExhaustedForSession(accounts, winName)

	for _, a := range accounts {
		w, ok := a.result.Windows[winName]
		if !ok {
			continue
		}
		if w.ResetAtUnix <= 0 && periodS > 0 {
			w.ResetAtUnix = quota.DefaultResetEpoch(periodS, nowEpoch)
		}

		weight := float64(a.multiplier)
		pct := float64(w.RemainingPct)
		weeklyGated := false

		if quota.BaseWindow(winName) == quota.Window5Hour {
			if weeklyExhausted(a.result, winName) {
				if !allWeeklyExhausted {
					continue
				}
				pct = 0
				weeklyGated = true
			}
		}

		elapsed := windowElapsed(w, periodS, nowEpoch)
		var expected float64
		if weeklyGated {
			expected = 0
		} else if w.ResetAtUnix > 0 && periodS > 0 {
			expected = 100.0 - (elapsed * 100.0 / float64(periodS))
		} else {
			expected = pct
		}

		sumRemaining += pct * weight
		sumExpected += expected * weight
		sumWeight += weight

		used := 100.0 - pct
		if pct > 0 && used > 0 && elapsed > 0 {
			burnNum += pct * weight
			burnDen += used * weight / elapsed
		}
		if !weeklyGated {
			sustainAccounts = append(sustainAccounts, a)
		}
	}

	if sumWeight == 0 {
		return quota.AggregateResult{}, false
	}

	aggRemaining := int(math.Round(sumRemaining / sumWeight))
	aggExpected := int(math.Round(sumExpected / sumWeight))
	result := quota.AggregateResult{
		RemainingPct: aggRemaining,
		ExpectedPct:  aggExpected,
		PaceDiff:     aggRemaining - aggExpected,
	}

	if burnDen > 0 {
		result.Burndown = int64(math.Round(burnNum / burnDen))
	}

	result.Sustainability = computeSustainability(sustainAccounts, winName, periodS, nowEpoch)
	if allWeeklyExhausted {
		result.Sustainability = 0
	}

	gi := computeGaugeInfo(accounts, winName, periodS, nowEpoch, burnRates)
	result.GaugePos = gi.Pos
	result.GaugeOverride = gi.Override
	if gi.GapStart >= 0 {
		result.GapStartS = int64(math.Round(gi.GapStart))
		result.GapDurationS = int64(math.Round(gi.GapDuration))
	}
	if gi.WastedPct > 0 {
		result.WastedPct = int(math.Round(gi.WastedPct * 100))
	}
	if gi.WasteDeadline >= 0 {
		result.WasteDeadlineS = int64(math.Round(gi.WasteDeadline))
	}

	return result, true
}

func allWeeklyExhaustedForSession(accounts []acctInfo, winName quota.WindowName) bool {
	hasSessionData := false
	for _, a := range accounts {
		if _, ok := a.result.Windows[winName]; !ok {
			continue
		}
		hasSessionData = true
		if !weeklyExhausted(a.result, winName) {
			return false
		}
	}
	return hasSessionData
}

// weeklyExhausted returns true when the matching 7-day window is at 0%.
// Bucket-scoped 5h windows are gated by their matching bucket-scoped 7d
// window when present, otherwise they fall back to the shared 7d window.
func weeklyExhausted(result quota.Result, winName quota.WindowName) bool {
	base := quota.BaseWindow(winName)
	if base != quota.Window5Hour {
		return false
	}

	gateName := quota.Window7Day
	if bucket := quota.WindowBucket(winName); bucket != "" {
		bucketGate := quota.WindowName("7d:" + bucket)
		if _, ok := result.Windows[bucketGate]; ok {
			gateName = bucketGate
		}
	}

	w, ok := result.Windows[gateName]
	if !ok {
		return false
	}
	return w.RemainingPct <= 0
}

func windowElapsed(w quota.Window, periodS int64, nowEpoch int64) float64 {
	if w.ResetAtUnix <= 0 || periodS <= 0 {
		return 0
	}

	elapsed := float64(periodS - (w.ResetAtUnix - nowEpoch))
	if elapsed < 0 {
		return 0
	}
	if elapsed > float64(periodS) {
		return float64(periodS)
	}
	return elapsed
}
