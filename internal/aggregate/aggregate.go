package aggregate

import (
	"math"

	"github.com/jacobcxdev/cq/internal/quota"
)

type acctInfo struct {
	result     quota.Result
	multiplier int
}

// Compute calculates aggregate pace for multiple Claude accounts across windows.
// Returns nil if fewer than 2 valid accounts exist.
func Compute(results []quota.Result, nowEpoch int64) (map[quota.WindowName]quota.AggregateResult, *AccountSummary) {
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
	for _, r := range valid {
		m := quota.ExtractMultiplier(r.RateLimitTier)
		accounts = append(accounts, acctInfo{result: r, multiplier: m})
		totalMulti += m
	}

	summary := &AccountSummary{
		Count:      len(accounts),
		TotalMulti: totalMulti,
		Label:      BuildLabel(valid),
	}

	// WindowQuota is excluded because it uses different reset semantics
	// (daily rolling) that don't compose across accounts the same way.
	windows := []quota.WindowName{quota.Window5Hour, quota.Window7Day}
	periods := map[quota.WindowName]int64{
		quota.Window5Hour: int64(quota.PeriodFor(quota.Window5Hour).Seconds()),
		quota.Window7Day:  int64(quota.PeriodFor(quota.Window7Day).Seconds()),
	}
	agg := make(map[quota.WindowName]quota.AggregateResult)

	for _, winName := range windows {
		result, ok := computeWindow(winName, periods[winName], accounts, nowEpoch)
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

func computeWindow(winName quota.WindowName, periodS int64, accounts []acctInfo, nowEpoch int64) (quota.AggregateResult, bool) {
	var sumRemaining, sumExpected, sumWeight float64
	// Burndown: Σ(pct_i * m_i) / Σ((100-pct_i) * m_i / elapsed_i)
	var burnNum, burnDen float64
	var sustainAccounts []acctInfo

	allWeeklyExhausted := winName == quota.Window5Hour && allWeeklyExhaustedForSession(accounts)

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

		if winName == quota.Window5Hour {
			if weeklyExhausted(a.result) {
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

	result.GaugePos = computeGaugePos(accounts, winName, periodS, nowEpoch)

	return result, true
}

func allWeeklyExhaustedForSession(accounts []acctInfo) bool {
	hasSessionData := false
	for _, a := range accounts {
		if _, ok := a.result.Windows[quota.Window5Hour]; !ok {
			continue
		}
		hasSessionData = true
		if !weeklyExhausted(a.result) {
			return false
		}
	}
	return hasSessionData
}

// weeklyExhausted returns true when the 7-day window is at 0%. When this
// happens the 5-hour window is effectively gated by the API regardless of its
// own percentage — the account cannot consume quota until the weekly window
// resets.
func weeklyExhausted(result quota.Result) bool {
	w, ok := result.Windows[quota.Window7Day]
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
