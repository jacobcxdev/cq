package codex

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

var (
	nowFunc                 = time.Now
	codexProLitePromoEndsAt = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	codexUsageIncreasedAt   = time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)
)

type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            any      `json:"reset_at"`
}

type usageRateLimit struct {
	Allowed         *bool        `json:"allowed"`
	LimitReached    *bool        `json:"limit_reached"`
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
		RateLimitTier        string          `json:"rate_limit_tier"`
		RateLimit            *usageRateLimit `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName string          `json:"limit_name"`
			RateLimit *usageRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		Credits *struct {
			HasCredits          *bool `json:"has_credits"`
			Unlimited           *bool `json:"unlimited"`
			OverageLimitReached bool  `json:"overage_limit_reached"`
		} `json:"credits"`
		SpendControl *struct {
			Reached bool `json:"reached"`
		} `json:"spend_control"`
		ModelUsage map[string]ModelUsage `json:"model_usage"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return UsageObservation{Result: quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)}
	}

	remainingPct := func(usedPercent float64) int {
		pct := int(math.Round(100 - usedPercent))
		return max(0, min(100, pct))
	}
	exactRemainingPct := func(usedPercent float64) float64 {
		return math.Max(0, math.Min(100, 100-usedPercent))
	}

	windows := make(map[quota.WindowName]quota.Window)
	var descriptors []WindowDescriptor
	includedUsageValid := true
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
			usedPercent := 0.0
			if entry.window.UsedPercent == nil {
				includedUsageValid = false
			} else {
				usedPercent = *entry.window.UsedPercent
				if usedPercent < 0 || usedPercent > 100 {
					includedUsageValid = false
				}
			}
			remaining := remainingPct(usedPercent)
			exactRemaining := exactRemainingPct(usedPercent)
			windows[name] = quota.Window{RemainingPct: remaining, RemainingPctExact: &exactRemaining, ResetAtUnix: resetAtUnix}
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
					ResetAt: time.Unix(resetAtUnix, 0), RemainingPct: exactRemaining,
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
		if usage.ModelUsage == nil {
			usage.ModelUsage = make(map[string]ModelUsage)
		}
		model := usage.ModelUsage[extra.LimitName]
		model.IncludedUsageAllowed = extra.RateLimit.Allowed
		model.LimitReached = extra.RateLimit.LimitReached
		usage.ModelUsage[extra.LimitName] = model
	}

	plan := usage.PlanType
	if plan == "" {
		plan = "unknown"
	}

	rlt := usage.RateLimitTier
	if rlt == "" {
		rlt = rateLimitTierForPlan(plan, nowFunc())
	}
	credits := &quota.ExtraCredits{Enabled: true}
	if usage.Credits != nil {
		if usage.Credits.HasCredits != nil || usage.Credits.Unlimited != nil {
			available := (usage.Credits.HasCredits != nil && *usage.Credits.HasCredits) || (usage.Credits.Unlimited != nil && *usage.Credits.Unlimited)
			credits.Available = &available
		}
		if usage.Credits.OverageLimitReached {
			available := false
			credits.Available = &available
		}
	}
	if usage.SpendControl != nil && usage.SpendControl.Reached {
		available := false
		credits.Available = &available
	}
	var allowed, reached *bool
	if usage.RateLimit != nil {
		allowed, reached = usage.RateLimit.Allowed, usage.RateLimit.LimitReached
	}

	return UsageObservation{
		IncludedUsageAllowed: allowed,
		IncludedUsageValid:   includedUsageValid && len(windows) > 0,
		LimitReached:         reached,
		ModelUsage:           usage.ModelUsage,
		Result: quota.Result{
			ExtraCredits:  credits,
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
	// These are published plan allowances relative to Plus, not speed-mode
	// consumption multipliers. Pro 100 and Pro 500 doubled on 29 October.
	// https://help.openai.com/en/articles/9793128-about-chatgpt-pro-tiers
	// https://openai.com/index/devday-2026-recap/ (Pro 500 launched at 25x)
	multiplier, known := provider.CodexUsageMultiplier(plan, "")
	if !known || multiplier == 1 {
		return ""
	}
	plan = strings.ToLower(plan)
	if plan == "prolite" && !now.Before(codexProLitePromoEndsAt) && now.Before(codexUsageIncreasedAt) {
		multiplier = 5
	}
	if plan == "promax" && now.Before(codexUsageIncreasedAt) {
		multiplier = 25
	}
	return fmt.Sprintf("codex_%s_%dx", plan, multiplier)
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
