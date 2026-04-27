package claude

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

var usageJSON = []byte(`{
	"five_hour": {"utilization": 30.0, "resets_at": "2026-03-20T12:00:00Z"},
	"seven_day": {"utilization": 10.0, "resets_at": "2026-03-25T00:00:00Z"}
}`)

var profileJSON = []byte(`{
	"account": {"uuid": "abc-123", "email": "user@example.com"},
	"organization": {"rate_limit_tier": "default_claude_max_20x", "organization_type": "claude_max"}
}`)

func TestParseUsage(t *testing.T) {
	result := parseUsage(usageJSON, "max", "default_claude_max_20x", "user@example.com", "abc-123")

	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if result.Plan != "max" {
		t.Errorf("plan = %q, want %q", result.Plan, "max")
	}
	if result.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "user@example.com")
	}
	if result.AccountID != "abc-123" {
		t.Errorf("account_id = %q, want %q", result.AccountID, "abc-123")
	}

	fiveHour, ok := result.Windows[quota.Window5Hour]
	if !ok {
		t.Fatal("missing 5h window")
	}
	if fiveHour.RemainingPct != 70 {
		t.Errorf("5h remaining_pct = %d, want 70", fiveHour.RemainingPct)
	}
	if fiveHour.ResetAtUnix == 0 {
		t.Error("5h reset_at_unix should be non-zero")
	}

	sevenDay, ok := result.Windows[quota.Window7Day]
	if !ok {
		t.Fatal("missing 7d window")
	}
	if sevenDay.RemainingPct != 90 {
		t.Errorf("7d remaining_pct = %d, want 90", sevenDay.RemainingPct)
	}
	if sevenDay.ResetAtUnix == 0 {
		t.Error("7d reset_at_unix should be non-zero")
	}
}

func TestParseUsageExhausted(t *testing.T) {
	exhaustedJSON := []byte(`{
		"five_hour": {"utilization": 100.0, "resets_at": "2026-03-20T12:00:00Z"},
		"seven_day": {"utilization": 50.0, "resets_at": "2026-03-25T00:00:00Z"}
	}`)

	result := parseUsage(exhaustedJSON, "max", "default_claude_max_20x", "user@example.com", "abc-123")

	if result.Status != quota.StatusExhausted {
		t.Errorf("status = %q, want %q", result.Status, quota.StatusExhausted)
	}

	fiveHour := result.Windows[quota.Window5Hour]
	if fiveHour.RemainingPct != 0 {
		t.Errorf("5h remaining_pct = %d, want 0", fiveHour.RemainingPct)
	}
}

func TestParseUsageInvalidJSON(t *testing.T) {
	result := parseUsage([]byte(`not json`), "max", "", "", "")

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

func TestParseProfile(t *testing.T) {
	p := parseProfile(profileJSON)

	if p.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", p.Email, "user@example.com")
	}
	if p.AccountUUID != "abc-123" {
		t.Errorf("account_uuid = %q, want %q", p.AccountUUID, "abc-123")
	}
	if p.Plan != "max" {
		t.Errorf("plan = %q, want %q (expected claude_max normalised to max)", p.Plan, "max")
	}
	if p.RateLimitTier != "default_claude_max_20x" {
		t.Errorf("rate_limit_tier = %q, want %q", p.RateLimitTier, "default_claude_max_20x")
	}
}

func TestParseProfileInvalidJSON(t *testing.T) {
	p := parseProfile([]byte(`not json`))

	if p.Email != "" || p.AccountUUID != "" || p.Plan != "" {
		t.Errorf("expected zero profile, got %+v", p)
	}
}

func TestDedup(t *testing.T) {
	results := []quota.Result{
		{AccountID: "uuid-1", Email: "a@example.com", Status: quota.StatusOK},
		{AccountID: "uuid-1", Email: "a@example.com", Status: quota.StatusError},
		{AccountID: "uuid-2", Email: "b@example.com", Status: quota.StatusOK},
	}

	out := dedup(results)

	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	for _, r := range out {
		if r.Status != quota.StatusOK {
			t.Errorf("unexpected non-ok result: %+v", r)
		}
	}
}

func TestDedupAllErrors(t *testing.T) {
	results := []quota.Result{
		{AccountID: "uuid-1", Status: quota.StatusError},
		{AccountID: "uuid-2", Status: quota.StatusError},
	}

	out := dedup(results)

	// All errors — nothing filtered, both kept
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
}

func TestDedupByEmail(t *testing.T) {
	// No AccountID — dedup by email
	results := []quota.Result{
		{Email: "a@example.com", Status: quota.StatusOK},
		{Email: "a@example.com", Status: quota.StatusError},
	}

	out := dedup(results)

	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Status != quota.StatusOK {
		t.Errorf("status = %q, want ok", out[0].Status)
	}
}

// TestDedupMultipleEmptyIdentity verifies that results with no AccountID and no
// Email are not collapsed — each gets a unique synthetic key.
func TestDedupMultipleEmptyIdentity(t *testing.T) {
	results := []quota.Result{
		{Status: quota.StatusOK, Plan: "max"},
		{Status: quota.StatusOK, Plan: "pro"},
	}
	out := dedup(results)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (empty-identity results must not be collapsed)", len(out))
	}
}

// TestParseUsageEmptyAccountID verifies that parseUsage works correctly when
// AccountID is empty but Email is populated.
func TestParseUsageEmptyAccountID(t *testing.T) {
	result := parseUsage(usageJSON, "max", "default_claude_max_20x", "user@example.com", "")

	if result.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "user@example.com")
	}
	if result.AccountID != "" {
		t.Errorf("account_id = %q, want empty", result.AccountID)
	}
	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want ok", result.Status)
	}
}

// TestParseUsageEmptyEmail verifies that parseUsage works correctly when Email
// is empty but AccountID is populated.
func TestParseUsageEmptyEmail(t *testing.T) {
	result := parseUsage(usageJSON, "max", "default_claude_max_20x", "", "uuid-only")

	if result.Email != "" {
		t.Errorf("email = %q, want empty", result.Email)
	}
	if result.AccountID != "uuid-only" {
		t.Errorf("account_id = %q, want %q", result.AccountID, "uuid-only")
	}
	if result.Status != quota.StatusOK {
		t.Errorf("status = %q, want ok", result.Status)
	}
}

// TestDedupUnidentifiableErrorDropped verifies that error results with no email
// and no account_id are dropped when usable results exist — they are stale
// keychain cruft that can't be associated with any account.
func TestDedupUnidentifiableErrorDropped(t *testing.T) {
	results := []quota.Result{
		{AccountID: "uuid-1", Email: "a@example.com", Status: quota.StatusOK},
		{Status: quota.StatusError, Error: &quota.ErrorInfo{Code: "auth_expired"}}, // no identity
	}

	out := dedup(results)

	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (unidentifiable error should be dropped); got %+v", len(out), out)
	}
	if out[0].Email != "a@example.com" {
		t.Errorf("expected a@example.com, got %+v", out[0])
	}
}

// TestDedupUnidentifiableErrorKeptWhenNoUsable verifies that unidentifiable
// error results are kept when there are no usable results at all.
func TestDedupUnidentifiableErrorKeptWhenNoUsable(t *testing.T) {
	results := []quota.Result{
		{Status: quota.StatusError, Error: &quota.ErrorInfo{Code: "not_configured"}},
	}

	out := dedup(results)

	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (sole error kept when no usable results)", len(out))
	}
}

// TestDedupExhaustedKept verifies that an exhausted result and an error result
// for different accounts are both retained — the error is only dropped when it
// shares an identity key with a usable result.
func TestDedupExhaustedKept(t *testing.T) {
	results := []quota.Result{
		{AccountID: "uuid-1", Status: quota.StatusExhausted},
		{AccountID: "uuid-2", Status: quota.StatusError},
	}

	out := dedup(results)

	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (error for different account must be kept)", len(out))
	}
}

func TestParseUsageModelSpecificSevenDayWindows(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 30.0, "resets_at": "2026-03-20T12:00:00Z"},
		"seven_day": {"utilization": 10.0, "resets_at": "2026-03-25T00:00:00Z"},
		"seven_day_sonnet": {"utilization": 25.0, "resets_at": "2026-03-24T00:00:00Z"},
		"seven_day_opus": {"utilization": 40.0, "resets_at": "2026-03-23T00:00:00Z"},
		"seven_day_omelette": {"utilization": 55.0, "resets_at": "2026-03-22T00:00:00Z"}
	}`)

	result := parseUsage(body, "max", "default_claude_max_20x", "user@example.com", "abc-123")
	tests := []struct {
		name string
		want int
	}{
		{name: "7d:sonnet", want: 75},
		{name: "7d:opus", want: 60},
		{name: "7d:design", want: 45},
	}

	for _, tc := range tests {
		window, ok := result.Windows[quota.WindowName(tc.name)]
		if !ok {
			t.Fatalf("missing %s window", tc.name)
		}
		if window.RemainingPct != tc.want {
			t.Errorf("%s remaining_pct = %d, want %d", tc.name, window.RemainingPct, tc.want)
		}
		if window.ResetAtUnix == 0 {
			t.Errorf("%s reset_at_unix should be non-zero", tc.name)
		}
	}
}

func TestParseUsageDoesNotEmitDesignWindowWhenOmeletteAbsent(t *testing.T) {
	result := parseUsage(usageJSON, "max", "default_claude_max_20x", "user@example.com", "abc-123")

	if _, ok := result.Windows[quota.WindowName("7d:design")]; ok {
		t.Fatal("unexpected 7d:design window")
	}
}
