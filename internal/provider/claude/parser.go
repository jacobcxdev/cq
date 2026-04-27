package claude

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

// profile holds the normalised fields from the Claude profile API response.
type profile struct {
	Email         string
	AccountUUID   string
	RateLimitTier string
	Plan          string
}

// parseProfile decodes a Claude profile API JSON body and normalises the plan
// name (e.g. "claude_max" → "max").
func parseProfile(body []byte) profile {
	var raw struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			RateLimitTier    string `json:"rate_limit_tier"`
			OrganizationType string `json:"organization_type"`
		} `json:"organization"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return profile{}
	}

	plan := strings.TrimPrefix(raw.Organization.OrganizationType, "claude_")

	return profile{
		Email:         raw.Account.Email,
		AccountUUID:   raw.Account.UUID,
		RateLimitTier: raw.Organization.RateLimitTier,
		Plan:          plan,
	}
}

// parseUsage decodes a Claude usage API JSON body and returns a quota.Result.
func parseUsage(body []byte, plan, rateLimitTier, email, uuid string) quota.Result {
	var usage struct {
		FiveHour *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
		SevenDaySonnet *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day_sonnet"`
		SevenDayOpus *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day_opus"`
		SevenDayOmelette *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day_omelette"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
	}

	toWindow := func(utilization float64, resetsAt string) quota.Window {
		pct := int(math.Round(100 - utilization))
		pct = max(0, min(100, pct))
		return quota.Window{RemainingPct: pct, ResetAtUnix: quota.ParseResetTime(resetsAt)}
	}

	windows := make(map[quota.WindowName]quota.Window)
	if usage.FiveHour != nil {
		windows[quota.Window5Hour] = toWindow(usage.FiveHour.Utilization, usage.FiveHour.ResetsAt)
	}
	if usage.SevenDay != nil {
		windows[quota.Window7Day] = toWindow(usage.SevenDay.Utilization, usage.SevenDay.ResetsAt)
	}
	if usage.SevenDaySonnet != nil {
		windows[quota.WindowName("7d:sonnet")] = toWindow(usage.SevenDaySonnet.Utilization, usage.SevenDaySonnet.ResetsAt)
	}
	if usage.SevenDayOpus != nil {
		windows[quota.WindowName("7d:opus")] = toWindow(usage.SevenDayOpus.Utilization, usage.SevenDayOpus.ResetsAt)
	}
	if usage.SevenDayOmelette != nil {
		windows[quota.WindowName("7d:design")] = toWindow(usage.SevenDayOmelette.Utilization, usage.SevenDayOmelette.ResetsAt)
	}

	return quota.Result{
		Status:        quota.StatusFromWindows(windows),
		Plan:          plan,
		RateLimitTier: rateLimitTier,
		Email:         email,
		AccountID:     uuid,
		Windows:       windows,
	}
}

// dedup removes duplicate accounts and filters out errored results when usable
// results exist for the same account. If multiple results share the same
// account identity and some are usable while others are errors, the errors are
// dropped (likely stale tokens for the same account).
func dedup(results []quota.Result) []quota.Result {
	// First pass: collect by identity key. When a duplicate key is seen,
	// prefer usable results over error results so a fresh keychain token
	// is not discarded in favour of a stale credentials-file entry.
	seenIdx := make(map[string]int) // key -> index in out
	var out []quota.Result
	for i, r := range results {
		key := r.AccountID
		if key == "" {
			key = r.Email
		}
		if key == "" {
			key = fmt.Sprintf("idx-%d", i)
		}
		if idx, exists := seenIdx[key]; exists {
			// Replace an error result with a usable one for the same account.
			if !out[idx].IsUsable() && r.IsUsable() {
				out[idx] = r
			}
			continue
		}
		seenIdx[key] = len(out)
		out = append(out, r)
	}

	// Second pass: if an account has both usable and error results (e.g.
	// stale token for the same account), keep only the usable one.
	usableKeys := make(map[string]bool)
	for _, r := range out {
		if r.IsUsable() {
			key := r.AccountID
			if key == "" {
				key = r.Email
			}
			if key != "" {
				usableKeys[key] = true
			}
		}
	}
	hasUsable := len(usableKeys) > 0
	if hasUsable {
		var filtered []quota.Result
		for _, r := range out {
			if r.IsUsable() {
				filtered = append(filtered, r)
				continue
			}
			key := r.AccountID
			if key == "" {
				key = r.Email
			}
			// Drop unidentifiable error results (stale keychain cruft)
			// when usable results exist — they can't be associated with
			// any account and just add noise.
			if key == "" {
				continue
			}
			// Keep error results for identified accounts that have no usable result
			if !usableKeys[key] {
				filtered = append(filtered, r)
			}
		}
		out = filtered
	}
	return out
}
