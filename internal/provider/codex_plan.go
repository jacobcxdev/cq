package provider

import (
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

// CodexUsageMultiplier resolves an explicit tier before the current documented
// included allowance. Usage-based and unfamiliar plans have no fixed weight.
// https://help.openai.com/en/articles/9793128-about-chatgpt-pro-tiers
// https://help.openai.com/en/articles/8542115-chatgpt-business-general-faq
func CodexUsageMultiplier(plan, tier string) (int, bool) {
	if tier != "" {
		return quota.ParseMultiplier(tier)
	}
	switch strings.ToLower(plan) {
	case "plus", "team":
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

// CodexPlanLabel follows Codex's status labels, using CQ's lowercase style.
// Keep this exact catalogue: business seat types are not aliases for every
// personal Pro tier. Unknown values retain the upstream name.
// https://github.com/openai/codex/blob/ddd1812a51f4aded35c3ec5446672474d5cb546b/codex-rs/tui/src/subscription.rs
func CodexPlanLabel(plan string) string {
	switch strings.ToLower(plan) {
	case "":
		return "unknown"
	case "free", "go", "plus", "unknown":
		return strings.ToLower(plan)
	case "pro", "prolite", "promax":
		return "pro"
	case "team", "self_serve_business_usage_based":
		return "business"
	case "self_serve_business_prolite":
		return "business premium"
	case "business", "ent26", "enterprise", "hc", "enterprise_cbp_usage_based":
		return "enterprise"
	case "enterprise_cbp_automation":
		return "enterprise (automation)"
	case "education", "edu":
		return "edu"
	case "edu_plus":
		return "edu plus"
	case "edu_pro":
		return "edu pro"
	case "sci":
		return "academic researchers"
	case "sci_plus":
		return "academic researchers plus"
	default:
		return plan
	}
}
