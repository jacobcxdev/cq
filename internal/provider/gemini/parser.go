package gemini

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

const (
	antigravityAccountID   = "antigravity-cli"
	geminiFiveHourBucketID = "gemini-5h"
	geminiWeeklyBucketID   = "gemini-weekly"
)

type usageEnvelope struct {
	Status   string        `json:"status"`
	NumTurns int           `json:"num_turns"`
	Usage    *usageTotals  `json:"usage"`
	Command  *usageCommand `json:"command"`
}

type usageTotals struct {
	TotalTokens int64 `json:"total_tokens"`
}

type usageCommand struct {
	Name string    `json:"name"`
	Data usageData `json:"data"`
}

type usageData struct {
	Groups []usageGroup `json:"groups"`
}

type usageGroup struct {
	Buckets []usageBucket `json:"buckets"`
}

type usageBucket struct {
	ID                string   `json:"id"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time"`
}

func parseUsage(data []byte) (quota.Result, error) {
	var envelope usageEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return quota.Result{}, errors.New("decode usage output")
	}
	if envelope.Status != "SUCCESS" || envelope.NumTurns != 0 {
		return quota.Result{}, errors.New("unsafe usage status")
	}
	if envelope.Usage == nil || envelope.Usage.TotalTokens != 0 {
		return quota.Result{}, errors.New("unsafe usage token count")
	}
	if envelope.Command == nil || envelope.Command.Name != "usage" {
		return quota.Result{}, errors.New("unexpected usage command")
	}

	windows := make(map[quota.WindowName]quota.Window, 2)
	counts := map[string]int{
		geminiFiveHourBucketID: 0,
		geminiWeeklyBucketID:   0,
	}
	for _, group := range envelope.Command.Data.Groups {
		for _, bucket := range group.Buckets {
			var name quota.WindowName
			switch bucket.ID {
			case geminiFiveHourBucketID:
				name = quota.Window5Hour
			case geminiWeeklyBucketID:
				name = quota.Window7Day
			default:
				continue
			}
			counts[bucket.ID]++
			if bucket.RemainingFraction == nil {
				return quota.Result{}, errors.New("missing quota fraction")
			}
			fraction := max(0, min(1, *bucket.RemainingFraction))
			windows[name] = quota.Window{
				RemainingPct: int(math.Round(fraction * 100)),
				ResetAtUnix:  quota.ParseResetTime(bucket.ResetTime),
			}
		}
	}
	if counts[geminiFiveHourBucketID] != 1 || counts[geminiWeeklyBucketID] != 1 {
		return quota.Result{}, errors.New("missing or duplicate Gemini quota bucket")
	}

	return quota.Result{
		AccountID: antigravityAccountID,
		Active:    true,
		Status:    quota.StatusFromWindows(windows),
		Windows:   windows,
	}, nil
}

// Legacy parser helpers remain until provider orchestration switches to
// Antigravity in the next TDD cycle.
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

func parseTier(data []byte) string {
	if len(data) == 0 {
		return "unknown"
	}
	var tier struct {
		PaidTier *struct {
			ID string `json:"id"`
		} `json:"paidTier"`
		CurrentTier *struct {
			ID string `json:"id"`
		} `json:"currentTier"`
		AllowedTiers []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"isDefault"`
		} `json:"allowedTiers"`
	}
	if json.Unmarshal(data, &tier) != nil {
		return "unknown"
	}
	if tier.PaidTier != nil && tier.PaidTier.ID != "" {
		return "paid"
	}
	tierID := ""
	if tier.CurrentTier != nil {
		tierID = tier.CurrentTier.ID
	} else {
		for _, allowed := range tier.AllowedTiers {
			if allowed.IsDefault {
				tierID = allowed.ID
				break
			}
		}
	}
	switch tierID {
	case "standard-tier":
		return "paid"
	case "free-tier":
		return "free"
	case "legacy-tier":
		return "legacy"
	case "":
		return "unknown"
	default:
		return tierID
	}
}

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
