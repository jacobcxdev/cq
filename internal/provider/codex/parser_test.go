package codex

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

var codexUsageJSON = []byte(`{
	"plan_type": "plus",
	"rate_limit": {
		"primary_window": {"used_percent": 25.0, "reset_at": 1774051200},
		"secondary_window": {"used_percent": 10.0, "reset_at": 1774569600}
	}
}`)

func TestParseUsageNormal(t *testing.T) {
	result := parseUsage(codexUsageJSON, "user@example.com", "")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if result.Plan != "plus" {
		t.Errorf("plan = %q, want %q", result.Plan, "plus")
	}
	if result.RateLimitTier != "" {
		t.Errorf("RateLimitTier = %q, want empty for plus", result.RateLimitTier)
	}
	if result.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "user@example.com")
	}

	fiveHour, ok := result.Windows[quota.Window5Hour]
	if !ok {
		t.Fatal("missing 5h window")
	}
	if fiveHour.RemainingPct != 75 {
		t.Errorf("5h remaining_pct = %d, want 75", fiveHour.RemainingPct)
	}
	if fiveHour.ResetAtUnix != 1774051200 {
		t.Errorf("5h reset_at_unix = %d, want 1774051200", fiveHour.ResetAtUnix)
	}

	sevenDay, ok := result.Windows[quota.Window7Day]
	if !ok {
		t.Fatal("missing 7d window")
	}
	if sevenDay.RemainingPct != 90 {
		t.Errorf("7d remaining_pct = %d, want 90", sevenDay.RemainingPct)
	}
	if sevenDay.ResetAtUnix != 1774569600 {
		t.Errorf("7d reset_at_unix = %d, want 1774569600", sevenDay.ResetAtUnix)
	}
}

func TestParseUsageExhausted(t *testing.T) {
	exhaustedJSON := []byte(`{
		"plan_type": "plus",
		"rate_limit": {
			"primary_window": {"used_percent": 100.0, "reset_at": 1774051200},
			"secondary_window": {"used_percent": 50.0, "reset_at": 1774569600}
		}
	}`)

	result := parseUsage(exhaustedJSON, "user@example.com", "")

	if result.Status != quota.StatusExhausted {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusExhausted)
	}

	fiveHour := result.Windows[quota.Window5Hour]
	if fiveHour.RemainingPct != 0 {
		t.Errorf("5h remaining_pct = %d, want 0", fiveHour.RemainingPct)
	}
}

func TestParseUsageInvalidJSON(t *testing.T) {
	result := parseUsage([]byte(`not json`), "user@example.com", "")

	if result.Status != quota.StatusError {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusError)
	}
	if result.Error == nil {
		t.Fatal("expected non-nil error info")
	}
	if result.Error.Code != "parse_error" {
		t.Errorf("error code = %q, want %q", result.Error.Code, "parse_error")
	}
}

func TestParseUsageMissingWindows(t *testing.T) {
	noWindowsJSON := []byte(`{
		"plan_type": "plus",
		"rate_limit": {}
	}`)

	result := parseUsage(noWindowsJSON, "user@example.com", "")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if len(result.Windows) != 0 {
		t.Errorf("windows = %v, want empty", result.Windows)
	}
}

func TestParseUsageOnlyPrimaryWindow(t *testing.T) {
	onlyPrimaryJSON := []byte(`{
		"plan_type": "free",
		"rate_limit": {
			"primary_window": {"used_percent": 30.0, "reset_at": 1774051200}
		}
	}`)

	result := parseUsage(onlyPrimaryJSON, "user@example.com", "")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if result.Plan != "free" {
		t.Errorf("plan = %q, want free", result.Plan)
	}
	if _, ok := result.Windows[quota.Window5Hour]; !ok {
		t.Error("expected 5h window")
	}
	if _, ok := result.Windows[quota.Window7Day]; ok {
		t.Error("unexpected 7d window when secondary absent")
	}
	fiveHour := result.Windows[quota.Window5Hour]
	if fiveHour.RemainingPct != 70 {
		t.Errorf("5h remaining_pct = %d, want 70", fiveHour.RemainingPct)
	}
}

func TestParseUsageUnknownPlanType(t *testing.T) {
	noPlanJSON := []byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 10.0, "reset_at": 1774051200}
		}
	}`)

	result := parseUsage(noPlanJSON, "user@example.com", "")

	if result.Plan != "unknown" {
		t.Errorf("plan = %q, want unknown", result.Plan)
	}
}

func TestParseUsageProMultiplier(t *testing.T) {
	proJSON := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {"used_percent": 10.0, "reset_at": 1774051200}
		}
	}`)

	result := parseUsage(proJSON, "user@example.com", "")

	if result.Plan != "pro" {
		t.Errorf("plan = %q, want pro", result.Plan)
	}
	if result.RateLimitTier != "codex_pro_7x" {
		t.Errorf("RateLimitTier = %q, want codex_pro_7x", result.RateLimitTier)
	}
	if m := quota.ExtractMultiplier(result.RateLimitTier); m != 7 {
		t.Errorf("ExtractMultiplier = %d, want 7", m)
	}
}

func TestParseNumericResetAt(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want int64
	}{
		{"float64", float64(1774051200), 1774051200},
		{"string RFC3339", "2026-03-19T12:00:00Z", 1773921600},
		{"nil", nil, 0},
		{"bool (unknown type)", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNumericResetAt(tt.v)
			if got != tt.want {
				t.Errorf("parseNumericResetAt(%v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}
