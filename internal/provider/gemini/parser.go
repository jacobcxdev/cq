package gemini

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

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
// quota.Result. It picks the most constrained pro model; if no pro model is
// found it falls back to the overall minimum across all buckets.
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

	minPct := 100
	var resetTime string
	foundPro := false

	for _, b := range resp.Buckets {
		pct := int(math.Round(b.RemainingFraction * 100))
		pct = max(0, min(100, pct))
		isPro := strings.Contains(strings.ToLower(b.ModelID), "pro")
		if isPro {
			if !foundPro || pct < minPct {
				minPct = pct
				resetTime = b.ResetTime
				foundPro = true
			}
		} else if !foundPro && pct < minPct {
			minPct = pct
			resetTime = b.ResetTime
		}
	}

	epoch := quota.ParseResetTime(resetTime)

	windows := map[quota.WindowName]quota.Window{
		quota.WindowQuota: {
			RemainingPct: minPct,
			ResetAtUnix:  epoch,
		},
	}

	return quota.Result{
		Status:  quota.StatusFromWindows(windows),
		Tier:    tier,
		Email:   email,
		Windows: windows,
	}
}
