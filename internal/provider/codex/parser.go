package codex

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/jacobcxdev/cq/internal/quota"
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

	// Codex Pro has ~6.7x the rate limits of Plus (rounded to 7x).
	// Encode as a synthetic rateLimitTier so ExtractMultiplier can parse it.
	var rlt string
	if plan == "pro" {
		rlt = "codex_pro_7x"
	}

	return quota.Result{
		Status:        quota.StatusFromWindows(windows),
		Plan:          plan,
		RateLimitTier: rlt,
		Email:         email,
		AccountID:     accountID,
		Windows:       windows,
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
