package aggregate

import (
	"fmt"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

// BuildLabel creates the display label like "2 x max 20x = 40x".
func BuildLabel(results []quota.Result) string {
	type planGroup struct {
		plan  string
		multi int
		count int
	}
	var groups []planGroup
	for _, r := range results {
		if !r.IsUsable() {
			continue
		}
		m := quota.ExtractMultiplier(r.RateLimitTier)
		plan := r.Plan
		if plan == "" {
			plan = "unknown"
		}
		found := false
		for i, g := range groups {
			if g.plan == plan && g.multi == m {
				groups[i].count++
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, planGroup{plan: plan, multi: m, count: 1})
		}
	}

	if len(groups) == 0 {
		return ""
	}

	total := 0
	var parts []string
	for _, g := range groups {
		total += g.count * g.multi
		if g.multi > 1 {
			parts = append(parts, fmt.Sprintf("%d \u00d7 %s %dx", g.count, g.plan, g.multi))
		} else {
			parts = append(parts, fmt.Sprintf("%d \u00d7 %s", g.count, g.plan))
		}
	}

	label := strings.Join(parts, " + ")
	if total > 1 {
		label += fmt.Sprintf(" = %dx", total)
	}
	return label
}
