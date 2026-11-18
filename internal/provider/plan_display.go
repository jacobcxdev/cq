package provider

import "strings"

// PlanDisplayLabel returns the provider's display name for a subscription plan.
func PlanDisplayLabel(id ID, plan string) string {
	switch id {
	case Codex:
		return CodexPlanLabel(plan)
	case Claude:
		switch strings.ToLower(plan) {
		case "free":
			return "Free"
		case "pro":
			return "Pro"
		case "max":
			return "Max"
		case "team":
			return "Team"
		case "enterprise":
			return "Enterprise"
		case "unknown":
			return "Unknown"
		}
	}
	return plan
}
