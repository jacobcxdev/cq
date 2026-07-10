package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type usageLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	Scope    *scope  `json:"scope"`
}

type scope struct {
	Model   *scopeValue `json:"model"`
	Surface *scopeValue `json:"surface"`
}

type scopeValue struct {
	ID          *string `json:"id"`
	DisplayName string  `json:"display_name"`
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
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(body, &usage); err != nil {
		return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
	}

	toWindow := func(utilization float64, resetsAt string) quota.Window {
		pct := int(math.Round(100 - utilization))
		pct = max(0, min(100, pct))
		return quota.Window{RemainingPct: pct, ResetAtUnix: quota.ParseResetTime(resetsAt)}
	}
	toWindowFromPercent := func(percent float64, resetsAt string) quota.Window {
		pct := int(math.Round(100 - percent))
		pct = max(0, min(100, pct))
		return quota.Window{RemainingPct: pct, ResetAtUnix: quota.ParseResetTime(resetsAt)}
	}

	windows := make(map[quota.WindowName]quota.Window)
	keys := make([]string, 0, len(usage))
	for key := range usage {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		winName, ok := claudeUsageWindowName(key)
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(usage[key]), []byte("null")) {
			continue
		}
		var win usageWindow
		if err := json.Unmarshal(usage[key], &win); err != nil {
			return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
		}
		if _, exists := windows[winName]; exists {
			continue
		}
		windows[winName] = toWindow(win.Utilization, win.ResetsAt)
	}
	if raw, ok := usage["limits"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var limits []usageLimit
		if err := json.Unmarshal(raw, &limits); err != nil {
			return quota.ErrorResult("parse_error", fmt.Sprintf("parse: %v", err), 0)
		}
		for _, limit := range limits {
			winName, ok := claudeLimitWindowName(limit)
			if !ok {
				continue
			}
			windows[winName] = toWindowFromPercent(limit.Percent, limit.ResetsAt)
		}
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

func claudeLimitWindowName(limit usageLimit) (quota.WindowName, bool) {
	base, ok := claudeLimitBaseWindow(limit.Group, limit.Kind)
	if !ok {
		return "", false
	}
	bucket := claudeScopeBucket(limit.Scope)
	if bucket == "" {
		return base, true
	}
	return quota.WindowName(string(base) + ":" + bucket), true
}

func claudeLimitBaseWindow(group, kind string) (quota.WindowName, bool) {
	switch group {
	case "session":
		return quota.Window5Hour, true
	case "weekly":
		return quota.Window7Day, true
	}
	switch kind {
	case "session":
		return quota.Window5Hour, true
	case "weekly_all", "weekly_scoped":
		return quota.Window7Day, true
	default:
		return "", false
	}
}

func claudeScopeBucket(scope *scope) string {
	if scope == nil {
		return ""
	}
	for _, value := range []*scopeValue{scope.Model, scope.Surface} {
		if value == nil {
			continue
		}
		if value.ID != nil && strings.TrimSpace(*value.ID) != "" {
			return normaliseClaudeUsageBucket(*value.ID)
		}
		if strings.TrimSpace(value.DisplayName) != "" {
			return normaliseClaudeUsageBucket(value.DisplayName)
		}
	}
	return ""
}

func claudeUsageWindowName(key string) (quota.WindowName, bool) {
	switch key {
	case "five_hour":
		return quota.Window5Hour, true
	case "seven_day":
		return quota.Window7Day, true
	}

	for _, scoped := range []struct {
		prefix string
		base   quota.WindowName
	}{
		{prefix: "five_hour_", base: quota.Window5Hour},
		{prefix: "seven_day_", base: quota.Window7Day},
	} {
		bucket, ok := strings.CutPrefix(key, scoped.prefix)
		if !ok || bucket == "" {
			continue
		}
		bucket = claudeUsageBucket(bucket)
		if bucket == "" {
			continue
		}
		return quota.WindowName(string(scoped.base) + ":" + bucket), true
	}

	return "", false
}

func claudeUsageBucket(bucket string) string {
	bucket = normaliseClaudeUsageBucket(bucket)
	switch bucket {
	case "omelette":
		return "design"
	default:
		return bucket
	}
}

func normaliseClaudeUsageBucket(bucket string) string {
	bucket = strings.TrimSpace(strings.ToLower(bucket))
	bucket = strings.ReplaceAll(bucket, " ", "-")
	bucket = strings.ReplaceAll(bucket, "_", "-")
	return bucket
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
