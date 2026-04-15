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
		RateLimit struct {
			PrimaryWindow *struct {
				UsedPercent float64 `json:"used_percent"`
				ResetAt     any     `json:"reset_at"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent float64 `json:"used_percent"`
				ResetAt     any     `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
	}

	windows := make(map[quota.WindowName]quota.Window)
	if usage.RateLimit.PrimaryWindow != nil {
		pct := int(math.Round(100 - usage.RateLimit.PrimaryWindow.UsedPercent))
		pct = max(0, min(100, pct))
		epoch := parseNumericResetAt(usage.RateLimit.PrimaryWindow.ResetAt)
		windows[quota.Window5Hour] = quota.Window{
			RemainingPct: pct,
			ResetAtUnix:  epoch,
		}
	}
	if usage.RateLimit.SecondaryWindow != nil {
		pct := int(math.Round(100 - usage.RateLimit.SecondaryWindow.UsedPercent))
		pct = max(0, min(100, pct))
		epoch := parseNumericResetAt(usage.RateLimit.SecondaryWindow.ResetAt)
		windows[quota.Window7Day] = quota.Window{
			RemainingPct: pct,
			ResetAtUnix:  epoch,
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
