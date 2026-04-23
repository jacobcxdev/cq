package codex

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

var (
	nowFunc          = time.Now
	codexPromoEndsAt = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
)

// parseUsage decodes a Codex usage API JSON body and returns a quota.Result.
func parseUsage(body []byte, email, accountID string) quota.Result {
	var usage struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow *struct {
				UsedPercent float64 `json:"used_percent"`
				ResetAt     any     `json:"reset_at"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent float64 `json:"used_percent"`
				ResetAt     any     `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName string `json:"limit_name"`
			RateLimit *struct {
				PrimaryWindow *struct {
					UsedPercent float64 `json:"used_percent"`
					ResetAt     any     `json:"reset_at"`
				} `json:"primary_window"`
				SecondaryWindow *struct {
					UsedPercent float64 `json:"used_percent"`
					ResetAt     any     `json:"reset_at"`
				} `json:"secondary_window"`
			} `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
	}

	toWindow := func(usedPercent float64, resetAt any) quota.Window {
		pct := int(math.Round(100 - usedPercent))
		pct = max(0, min(100, pct))
		return quota.Window{RemainingPct: pct, ResetAtUnix: parseNumericResetAt(resetAt)}
	}

	// Free accounts have only a weekly limit; their primary_window is a 7d window.
	primaryWindowName := quota.Window5Hour
	if usage.PlanType == "free" {
		primaryWindowName = quota.Window7Day
	}

	windows := make(map[quota.WindowName]quota.Window)
	if usage.RateLimit != nil {
		if usage.RateLimit.PrimaryWindow != nil {
			windows[primaryWindowName] = toWindow(usage.RateLimit.PrimaryWindow.UsedPercent, usage.RateLimit.PrimaryWindow.ResetAt)
		}
		if usage.RateLimit.SecondaryWindow != nil {
			windows[quota.Window7Day] = toWindow(usage.RateLimit.SecondaryWindow.UsedPercent, usage.RateLimit.SecondaryWindow.ResetAt)
		}
	}
	for _, extra := range usage.AdditionalRateLimits {
		if extra.LimitName == "" || extra.RateLimit == nil {
			continue
		}
		if extra.RateLimit.PrimaryWindow != nil {
			windows[quota.WindowName("5h:"+extra.LimitName)] = toWindow(extra.RateLimit.PrimaryWindow.UsedPercent, extra.RateLimit.PrimaryWindow.ResetAt)
		}
		if extra.RateLimit.SecondaryWindow != nil {
			windows[quota.WindowName("7d:"+extra.LimitName)] = toWindow(extra.RateLimit.SecondaryWindow.UsedPercent, extra.RateLimit.SecondaryWindow.ResetAt)
		}
	}

	plan := usage.PlanType
	if plan == "" {
		plan = "unknown"
	}

	rlt := rateLimitTierForPlan(plan, nowFunc())

	return quota.Result{
		Status:        quota.StatusFromWindows(windows),
		Plan:          plan,
		RateLimitTier: rlt,
		Email:         email,
		AccountID:     accountID,
		Windows:       windows,
	}
}

func rateLimitTierForPlan(plan string, now time.Time) string {
	switch plan {
	case "pro":
		if now.Before(codexPromoEndsAt) {
			return "codex_pro_20x"
		}
		return "codex_pro_10x"
	case "prolite":
		if now.Before(codexPromoEndsAt) {
			return "codex_prolite_10x"
		}
		return "codex_prolite_5x"
	default:
		return ""
	}
}

// parseNumericResetAt handles reset_at as either a number (epoch seconds) or string.
// Standard json.Unmarshal always produces float64 for JSON numbers, so only
// float64 and string cases are reachable.
func parseNumericResetAt(v any) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case string:
		return quota.ParseResetTime(val)
	default:
		return 0
	}
}
