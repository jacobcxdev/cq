package provider

import (
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

// CodexUsageMultiplier uses Plus as the baseline and resolves explicit tiers
// for other plans before falling back to their documented included allowance.
// https://help.openai.com/en/articles/9793128-about-chatgpt-pro-tiers
// https://help.openai.com/en/articles/8542115-chatgpt-business-general-faq
func CodexUsageMultiplier(plan, tier string) (int, bool) {
	plan = strings.ToLower(plan)
	if plan == "plus" {
		return 1, true
	}
	if tier != "" {
		return quota.ParseMultiplier(tier)
	}
	switch plan {
	case "team":
		return 1, true
	case "prolite":
		return 10, true
	case "pro":
		return 20, true
	case "promax":
		return 50, true
	case "self_serve_business_prolite":
		return 5, true
	default:
		return 0, false
	}
}

// CodexPlanLabel follows Codex status labels and groups Pro variants for the
// appended usage multiplier. Unknown values retain the upstream name.
// https://github.com/openai/codex/blob/main/codex-rs/tui/src/subscription.rs
func CodexPlanLabel(plan string) string {
	switch strings.ToLower(plan) {
	case "", "unknown":
		return "Unknown"
	case "free":
		return "Free"
	case "go":
		return "Go"
	case "plus":
		return "Plus"
	case "pro", "prolite", "promax":
		return "Pro"
	case "team", "self_serve_business_usage_based":
		return "Business"
	case "self_serve_business_prolite":
		return "Business Premium"
	case "business", "ent26", "enterprise", "hc", "enterprise_cbp_usage_based":
		return "Enterprise"
	case "enterprise_cbp_automation":
		return "Enterprise (Automation)"
	case "education", "edu":
		return "Edu"
	case "edu_plus":
		return "Edu Plus"
	case "edu_pro":
		return "Edu Pro"
	case "sci":
		return "Academic Researchers"
	case "sci_plus":
		return "Academic Researchers Plus"
	default:
		return plan
	}
}
