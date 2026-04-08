package gemini

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

// parseProjectID extracts the cloudaicompanionProject ID from the
// loadCodeAssist response body. The field may be a plain string or an object
// with an "id" or "projectId" sub-field. Returns empty string if not found.
func parseProjectID(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var outer struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if json.Unmarshal(data, &outer) != nil || len(outer.Project) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(outer.Project, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
	}
	if json.Unmarshal(outer.Project, &obj) == nil {
		if id := strings.TrimSpace(obj.ID); id != "" {
			return id
		}
		return strings.TrimSpace(obj.ProjectID)
	}
	return ""
}

// parseTier decodes the loadCodeAssist response and returns a human-readable
// tier string: "paid", "free", "legacy", or "unknown".
func parseTier(data []byte) string {
	if len(data) == 0 {
		return "unknown"
	}
	var tier struct {
		PaidTier    *struct{ ID string `json:"id"` } `json:"paidTier"`
		CurrentTier *struct{ ID string `json:"id"` } `json:"currentTier"`
	}
	if json.Unmarshal(data, &tier) != nil {
		return "unknown"
	}
	if tier.PaidTier != nil && tier.PaidTier.ID != "" {
		return "paid"
	}
	if tier.CurrentTier != nil {
		switch tier.CurrentTier.ID {
		case "standard-tier":
			return "paid"
		case "free-tier":
			return "free"
		case "legacy-tier":
			return "legacy"
		default:
			if tier.CurrentTier.ID != "" {
				return tier.CurrentTier.ID
			}
		}
	}
	return "unknown"
}

// parseQuota decodes the retrieveUserQuota response body and returns a
// quota.Result with per-tier windows: quota (Pro), flash (Flash), flash-lite
// (Flash Lite). Only windows with at least one matching bucket are populated.
// Models that match none of the three tiers are ignored.
func parseQuota(body []byte, tier, email string) quota.Result {
	var resp struct {
		Buckets []struct {
			ModelID           string  `json:"modelId"`
			RemainingFraction float64 `json:"remainingFraction"`
			ResetTime         string  `json:"resetTime"`
		} `json:"buckets"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return quota.ErrorResult("parse_error", "", 0)
	}

	type tierMin struct {
		pct       int
		resetTime string
		found     bool
	}
	var pro, flash, flashLite tierMin

	for _, b := range resp.Buckets {
		pct := int(math.Round(b.RemainingFraction * 100))
		pct = max(0, min(100, pct))
		lower := strings.ToLower(b.ModelID)
		switch {
		case strings.Contains(lower, "flash-lite") || strings.Contains(lower, "flash_lite"):
			if !flashLite.found || pct < flashLite.pct {
				flashLite = tierMin{pct, b.ResetTime, true}
			}
		case strings.Contains(lower, "flash"):
			if !flash.found || pct < flash.pct {
				flash = tierMin{pct, b.ResetTime, true}
			}
		case strings.Contains(lower, "pro"):
			if !pro.found || pct < pro.pct {
				pro = tierMin{pct, b.ResetTime, true}
			}
		}
	}

	windows := make(map[quota.WindowName]quota.Window)
	add := func(name quota.WindowName, tm tierMin) {
		if tm.found {
			windows[name] = quota.Window{
				RemainingPct: tm.pct,
				ResetAtUnix:  quota.ParseResetTime(tm.resetTime),
			}
		}
	}
	add(quota.WindowPro, pro)
	add(quota.WindowFlash, flash)
	add(quota.WindowFlashLite, flashLite)

	return quota.Result{
		Status:  quota.StatusFromWindows(windows),
		Tier:    tier,
		Email:   email,
		Windows: windows,
	}
}
