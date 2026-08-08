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

type usageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            any     `json:"reset_at"`
}

type usageRateLimit struct {
	PrimaryWindow   *usageWindow `json:"primary_window"`
	SecondaryWindow *usageWindow `json:"secondary_window"`
}

// parseUsage decodes a Codex usage API JSON body and returns a quota.Result.
func parseUsage(body []byte, email, accountID string) quota.Result {
	return ParseUsageObservation(body, email, accountID).Result
}

// ParseUsageObservation returns existing quota output plus scheduler-safe
// descriptors that preserve backend window scope and exact reset epochs.
func ParseUsageObservation(body []byte, email, accountID string) UsageObservation {
	var usage struct {
		PlanType             string          `json:"plan_type"`
		RateLimit            *usageRateLimit `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName string          `json:"limit_name"`
			RateLimit *usageRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return UsageObservation{Result: quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)}
	}

	remainingPct := func(usedPercent float64) int {
		pct := int(math.Round(100 - usedPercent))
		return max(0, min(100, pct))
	}

	windows := make(map[quota.WindowName]quota.Window)
	var descriptors []WindowDescriptor
	addWindows := func(rateLimit *usageRateLimit, bucket string) error {
		if rateLimit == nil {
			return nil
		}
		for _, entry := range []struct {
			slot   string
			window *usageWindow
		}{
			{slot: "primary_window", window: rateLimit.PrimaryWindow},
			{slot: "secondary_window", window: rateLimit.SecondaryWindow},
		} {
			if entry.window == nil {
				continue
			}
			name, ok := quota.WindowNameForPeriod(entry.window.LimitWindowSeconds, bucket)
			if !ok {
				return fmt.Errorf("invalid %s limit_window_seconds", entry.slot)
			}
			if _, exists := windows[name]; exists {
				return fmt.Errorf("conflicting rate limit window %q", name)
			}
			resetAtUnix := parseNumericResetAt(entry.window.ResetAt)
			remaining := remainingPct(entry.window.UsedPercent)
			windows[name] = quota.Window{RemainingPct: remaining, ResetAtUnix: resetAtUnix}
			if resetAtUnix > 0 {
				rawLimitName := entry.slot
				scopeKind := WindowScopeShared
				if bucket != "" {
					rawLimitName = bucket
					scopeKind = WindowScopeModelFamily
				}
				descriptors = append(descriptors, WindowDescriptor{
					RawLimitName: rawLimitName, WindowName: name,
					Period:    time.Duration(entry.window.LimitWindowSeconds) * time.Second,
					ScopeKind: scopeKind, Scope: bucket,
					ResetAt: time.Unix(resetAtUnix, 0), RemainingPct: float64(remaining),
				})
			}
		}
		return nil
	}

	if err := addWindows(usage.RateLimit, ""); err != nil {
		return UsageObservation{Result: quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)}
	}
	for _, extra := range usage.AdditionalRateLimits {
		if extra.LimitName == "" || extra.RateLimit == nil {
			continue
		}
		if err := addWindows(extra.RateLimit, extra.LimitName); err != nil {
			return UsageObservation{Result: quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)}
		}
	}

	plan := usage.PlanType
	if plan == "" {
		plan = "unknown"
	}

	rlt := rateLimitTierForPlan(plan, nowFunc())

	return UsageObservation{
		Result: quota.Result{
			Status:        quota.StatusFromWindows(windows),
			Plan:          plan,
			RateLimitTier: rlt,
			Email:         email,
			AccountID:     accountID,
			Windows:       windows,
		},
		Windows: descriptors,
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
