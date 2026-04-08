package gemini

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

var quotaJSON = []byte(`{
	"buckets": [
		{"modelId": "gemini-2.0-pro", "remainingFraction": 0.75, "resetTime": "2026-03-21T00:00:00Z"},
		{"modelId": "gemini-2.0-flash", "remainingFraction": 0.90, "resetTime": "2026-03-21T00:00:00Z"}
	]
}`)

var tierJSON = []byte(`{
	"currentTier": {"id": "standard-tier"}
}`)

func TestParseQuotaPicksProModel(t *testing.T) {
	result := parseQuota(quotaJSON, "paid", "user@example.com")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if result.Tier != "paid" {
		t.Errorf("tier = %q, want %q", result.Tier, "paid")
	}
	if result.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "user@example.com")
	}

	w, ok := result.Windows[quota.WindowPro]
	if !ok {
		t.Fatal("missing quota window")
	}
	if w.RemainingPct != 75 {
		t.Errorf("quota remaining_pct = %d, want 75", w.RemainingPct)
	}

	wf, ok := result.Windows[quota.WindowFlash]
	if !ok {
		t.Fatal("missing flash window")
	}
	if wf.RemainingPct != 90 {
		t.Errorf("flash remaining_pct = %d, want 90", wf.RemainingPct)
	}
}

func TestParseQuotaNoProYieldsFlashWindow(t *testing.T) {
	noProJSON := []byte(`{
		"buckets": [
			{"modelId": "gemini-2.0-flash", "remainingFraction": 0.60, "resetTime": "2026-03-21T00:00:00Z"},
			{"modelId": "gemini-2.0-nano", "remainingFraction": 0.80, "resetTime": "2026-03-21T00:00:00Z"}
		]
	}`)

	result := parseQuota(noProJSON, "free", "user@example.com")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	// No pro — WindowPro absent; flash goes to WindowFlash.
	if _, ok := result.Windows[quota.WindowPro]; ok {
		t.Error("quota window should be absent when no pro model present")
	}
	wf := result.Windows[quota.WindowFlash]
	if wf.RemainingPct != 60 {
		t.Errorf("flash remaining_pct = %d, want 60", wf.RemainingPct)
	}
}

func TestParseQuotaExhausted(t *testing.T) {
	exhaustedJSON := []byte(`{
		"buckets": [
			{"modelId": "gemini-2.0-pro", "remainingFraction": 0.0, "resetTime": "2026-03-21T00:00:00Z"}
		]
	}`)

	result := parseQuota(exhaustedJSON, "paid", "user@example.com")

	if result.Status != quota.StatusExhausted {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusExhausted)
	}
	w := result.Windows[quota.WindowPro]
	if w.RemainingPct != 0 {
		t.Errorf("remaining_pct = %d, want 0", w.RemainingPct)
	}
}

func TestParseQuotaInvalidJSON(t *testing.T) {
	result := parseQuota([]byte(`not json`), "paid", "user@example.com")

	if result.Status != quota.StatusError {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusError)
	}
	if result.Error == nil {
		t.Fatal("expected non-nil error info")
	}
	if result.Error.Code != "parse_error" {
		t.Errorf("error code = %q, want parse_error", result.Error.Code)
	}
}

func TestParseTierStandardIsPaid(t *testing.T) {
	tier := parseTier(tierJSON)
	if tier != "paid" {
		t.Errorf("tier = %q, want %q", tier, "paid")
	}
}

func TestParseTierFree(t *testing.T) {
	freeJSON := []byte(`{"currentTier": {"id": "free-tier"}}`)
	tier := parseTier(freeJSON)
	if tier != "free" {
		t.Errorf("tier = %q, want %q", tier, "free")
	}
}

func TestParseTierPaidTierField(t *testing.T) {
	paidJSON := []byte(`{"paidTier": {"id": "some-paid-tier"}}`)
	tier := parseTier(paidJSON)
	if tier != "paid" {
		t.Errorf("tier = %q, want %q", tier, "paid")
	}
}

func TestParseTierEmpty(t *testing.T) {
	tier := parseTier([]byte{})
	if tier != "unknown" {
		t.Errorf("tier = %q, want %q", tier, "unknown")
	}
}

func TestParseTierLegacy(t *testing.T) {
	legacyJSON := []byte(`{"currentTier": {"id": "legacy-tier"}}`)
	tier := parseTier(legacyJSON)
	if tier != "legacy" {
		t.Errorf("tier = %q, want legacy", tier)
	}
}

func TestParseTierUnknownID(t *testing.T) {
	unknownJSON := []byte(`{"currentTier": {"id": "some-future-tier"}}`)
	tier := parseTier(unknownJSON)
	if tier != "some-future-tier" {
		t.Errorf("tier = %q, want some-future-tier", tier)
	}
}

func TestParseTierInvalidJSON(t *testing.T) {
	tier := parseTier([]byte(`not json`))
	if tier != "unknown" {
		t.Errorf("tier = %q, want unknown", tier)
	}
}

func TestParseTierEmptyCurrentTierID(t *testing.T) {
	// currentTier present but ID is empty string → falls through to "unknown"
	emptyIDJSON := []byte(`{"currentTier": {"id": ""}}`)
	tier := parseTier(emptyIDJSON)
	if tier != "unknown" {
		t.Errorf("tier = %q, want unknown", tier)
	}
}

func TestParseQuotaEmptyBuckets(t *testing.T) {
	emptyJSON := []byte(`{"buckets": []}`)
	result := parseQuota(emptyJSON, "paid", "user@example.com")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want ok", result.Status)
	}
	if len(result.Windows) != 0 {
		t.Errorf("expected empty windows map, got %v", result.Windows)
	}
}

func TestParseQuotaMultipleProPicksMostConstrained(t *testing.T) {
	multiProJSON := []byte(`{
		"buckets": [
			{"modelId": "gemini-2.0-pro-exp", "remainingFraction": 0.80, "resetTime": "2026-03-21T00:00:00Z"},
			{"modelId": "gemini-1.5-pro", "remainingFraction": 0.30, "resetTime": "2026-03-21T00:00:00Z"},
			{"modelId": "gemini-2.0-flash", "remainingFraction": 0.95, "resetTime": "2026-03-21T00:00:00Z"}
		]
	}`)
	result := parseQuota(multiProJSON, "paid", "user@example.com")

	w := result.Windows[quota.WindowPro]
	// Two pro models: 80% and 30% — most constrained is 30%.
	if w.RemainingPct != 30 {
		t.Errorf("quota remaining_pct = %d, want 30", w.RemainingPct)
	}
	wf := result.Windows[quota.WindowFlash]
	if wf.RemainingPct != 95 {
		t.Errorf("flash remaining_pct = %d, want 95", wf.RemainingPct)
	}
}

func TestParseQuotaFlashLite(t *testing.T) {
	flashLiteJSON := []byte(`{
		"buckets": [
			{"modelId": "gemini-2.0-flash-lite", "remainingFraction": 0.50, "resetTime": "2026-03-21T00:00:00Z"},
			{"modelId": "gemini-2.0-flash", "remainingFraction": 0.70, "resetTime": "2026-03-21T00:00:00Z"},
			{"modelId": "gemini-2.0-pro", "remainingFraction": 0.40, "resetTime": "2026-03-21T00:00:00Z"}
		]
	}`)
	result := parseQuota(flashLiteJSON, "paid", "user@example.com")

	if result.Windows[quota.WindowPro].RemainingPct != 40 {
		t.Errorf("pro remaining_pct = %d, want 40", result.Windows[quota.WindowPro].RemainingPct)
	}
	if result.Windows[quota.WindowFlash].RemainingPct != 70 {
		t.Errorf("flash remaining_pct = %d, want 70", result.Windows[quota.WindowFlash].RemainingPct)
	}
	if result.Windows[quota.WindowFlashLite].RemainingPct != 50 {
		t.Errorf("flash-lite remaining_pct = %d, want 50", result.Windows[quota.WindowFlashLite].RemainingPct)
	}
}
