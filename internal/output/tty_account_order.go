package output

import (
	"sort"
	"strings"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

// accountDisplayOrder leaves the report and its routing-independent JSON order intact.
func accountDisplayOrder(pr app.ProviderReport, nowEpoch int64) []quota.Result {
	if pr.ID != provider.Codex || len(pr.Results) < 2 {
		return pr.Results
	}
	type row struct {
		result          quota.Result
		active          bool
		value           uint32
		valueKnown      bool
		multiplier      int
		multiplierKnown bool
		tier            string
		reset           int64
	}
	rows := make([]row, len(pr.Results))
	for i, result := range pr.Results {
		value, known := pr.DisplayAccountValues[i]
		active, activeKnown := pr.DisplayActiveAccounts[i]
		if !activeKnown {
			active = result.Active
		}
		multiplier, multiplierKnown := provider.CodexUsageMultiplier(result.Plan, result.RateLimitTier)
		plan := result.Plan
		if plan == "" {
			plan = result.Tier
		}
		rows[i] = row{result, active, value, known, multiplier, multiplierKnown,
			strings.ToLower(provider.CodexPlanLabel(plan)), accountDisplayReset(result, nowEpoch)}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.active != b.active {
			return a.active
		}
		if a.valueKnown != b.valueKnown {
			return a.valueKnown
		}
		if a.value != b.value {
			return a.value > b.value
		}
		if a.multiplierKnown != b.multiplierKnown {
			return a.multiplierKnown
		}
		if a.multiplier != b.multiplier {
			return a.multiplier > b.multiplier
		}
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if (a.reset > 0) != (b.reset > 0) {
			return a.reset > 0
		}
		if a.reset != b.reset {
			return a.reset < b.reset
		}
		if ae, be := strings.ToLower(a.result.Email), strings.ToLower(b.result.Email); ae != be {
			return ae < be
		}
		return a.result.AccountID < b.result.AccountID
	})
	results := make([]quota.Result, len(rows))
	for i := range rows {
		results[i] = rows[i].result
	}
	return results
}

// accountDisplayReset uses the limiting shared window, or scoped windows when
// no shared allowance exists. Exhausted windows must all reset before recovery.
// Zero means the limiting reset is missing, expired or otherwise unknown.
func accountDisplayReset(result quota.Result, nowEpoch int64) int64 {
	shared := false
	for name := range result.Windows {
		if quota.WindowBucket(name) == "" {
			shared = true
			break
		}
	}
	minimum := 101.0
	var reset int64
	unknown := false
	for name, window := range result.Windows {
		if shared && quota.WindowBucket(name) != "" {
			continue
		}
		remaining := window.RemainingPercent()
		if remaining < 0 || remaining > 100 || remaining > minimum {
			continue
		}
		if remaining < minimum {
			minimum, reset, unknown = remaining, 0, false
		}
		if window.ResetAtUnix <= nowEpoch {
			unknown = true
			continue
		}
		if reset == 0 || (remaining == 0 && window.ResetAtUnix > reset) || (remaining > 0 && window.ResetAtUnix < reset) {
			reset = window.ResetAtUnix
		}
	}
	if unknown {
		return 0
	}
	return reset
}
