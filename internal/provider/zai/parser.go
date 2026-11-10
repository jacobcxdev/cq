package zai

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jacobcxdev/cq/internal/quota"
)

type quotaLimit struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        *int     `json:"number"`
	Percentage    *float64 `json:"percentage"`
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	NextResetTime int64    `json:"nextResetTime"`
}

func parseQuota(body []byte) (quota.Result, string) {
	var response struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
		Data    struct {
			Limits []json.RawMessage `json:"limits"`
			Level  string            `json:"level"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return quota.Result{}, "parse_error"
	}
	code := strings.Trim(string(response.Code), `"`)
	if code != "" && code != "null" && code != "0" && code != "200" || response.Success != nil && !*response.Success {
		return quota.Result{}, "api_error"
	}
	var plan string
	var multiplier int
	// Current paid credit allowances are Lite 2,000/10,000, Pro
	// 12,000/60,000 and Max 28,000/140,000 per five hours/week.
	// https://docs.z.ai/devpack/overview#usage-instruction
	switch strings.ToLower(response.Data.Level) {
	case "lite":
		plan, multiplier = "Lite", 1
	case "pro":
		plan, multiplier = "Pro", 6
	case "max":
		plan, multiplier = "Max", 14
	}
	capacityKnown := multiplier > 0
	windows := make(map[quota.WindowName]quota.Window)
	for _, raw := range response.Data.Limits {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &kind) != nil || kind.Type != "TOKENS_LIMIT" && kind.Type != "CREDIT_LIMIT" && kind.Type != "TIME_LIMIT" {
			continue
		}
		var limit quotaLimit
		if err := json.Unmarshal(raw, &limit); err != nil {
			return quota.Result{}, "parse_error"
		}
		var name quota.WindowName
		switch {
		case (limit.Type == "TOKENS_LIMIT" || limit.Type == "CREDIT_LIMIT") && limit.Unit == 3 && (limit.Number == nil || *limit.Number == 5):
			name = quota.Window5Hour
		case (limit.Type == "TOKENS_LIMIT" || limit.Type == "CREDIT_LIMIT") && limit.Unit == 6 && (limit.Number == nil || *limit.Number == 1):
			name = quota.Window7Day
		case limit.Type == "TIME_LIMIT" && limit.Unit == 5 && (limit.Number == nil || *limit.Number == 1):
			name = "monthly:mcp"
		default:
			continue
		}
		if _, duplicate := windows[name]; duplicate {
			return quota.Result{}, "parse_error"
		}
		if name != "monthly:mcp" {
			baseAllowance := 2000.0
			if name == quota.Window7Day {
				baseAllowance = 10000
			}
			// Explicit legacy token caps and conflicting credit caps do
			// not establish a current paid allowance, even for a known tier.
			if limit.Type != "CREDIT_LIMIT" || limit.Usage != nil && *limit.Usage != baseAllowance*float64(multiplier) {
				capacityKnown = false
			}
		}
		percentage := limit.Percentage
		if percentage == nil && limit.Type == "TIME_LIMIT" && limit.Usage != nil && *limit.Usage > 0 && limit.CurrentValue != nil {
			used := *limit.CurrentValue / *limit.Usage * 100
			percentage = &used
		}
		if percentage == nil || math.IsNaN(*percentage) || math.IsInf(*percentage, 0) {
			return quota.Result{}, "parse_error"
		}
		remaining := max(0, min(100, 100-*percentage))
		windows[name] = quota.Window{
			RemainingPct:      int(math.Round(remaining)),
			RemainingPctExact: &remaining,
			ResetAtUnix:       max(0, limit.NextResetTime/1000),
		}
	}
	_, hasFiveHour := windows[quota.Window5Hour]
	_, hasWeekly := windows[quota.Window7Day]
	if !hasFiveHour && !hasWeekly {
		return quota.Result{}, "parse_error"
	}
	var tier string
	if capacityKnown {
		tier = fmt.Sprintf("zai_coding_%dx", multiplier)
	}
	return quota.Result{
		AccountID:     accountID,
		Active:        true,
		Status:        quota.StatusFromWindows(windows),
		Plan:          plan,
		RateLimitTier: tier,
		Windows:       windows,
	}, ""
}
