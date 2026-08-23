package gemini

import (
	"fmt"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

const validGeminiBuckets = `
	{"bucketId":"gemini-weekly","remainingFraction":0.734,"resetTime":"2026-08-27T06:56:51Z"},
	{"bucketId":"gemini-5h","remainingFraction":0.426,"resetTime":"2026-08-20T11:56:51Z"}`

func quotaFixture(buckets string) []byte {
	return []byte(fmt.Sprintf(`{
		"groups":[
			{"buckets":[%s]},
			{"buckets":[
				{"bucketId":"3p-weekly","remainingFraction":0.01,"resetTime":"2026-08-27T06:56:51Z"},
				{"bucketId":"3p-5h","remainingFraction":0.02,"resetTime":"2026-08-20T11:56:51Z"}
			]}
		]
	}`, buckets))
}

func TestParseQuotaSummaryMapsOnlyGeminiBuckets(t *testing.T) {
	result, err := parseQuotaSummary(quotaFixture(validGeminiBuckets))
	if err != nil {
		t.Fatalf("parseQuotaSummary() error = %v", err)
	}
	if result.AccountID != "antigravity-cli" || !result.Active {
		t.Fatalf("identity = %q/%v, want antigravity-cli/true", result.AccountID, result.Active)
	}
	if result.Email != "" || result.Tier != "" {
		t.Fatalf("email/tier = %q/%q, want empty", result.Email, result.Tier)
	}
	if result.Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q", result.Status, quota.StatusOK)
	}
	if len(result.Windows) != 2 {
		t.Fatalf("window count = %d, want 2", len(result.Windows))
	}
	fiveHour := result.Windows[quota.Window5Hour]
	if fiveHour.RemainingPct != 43 {
		t.Fatalf("5h remaining = %d, want 43", fiveHour.RemainingPct)
	}
	wantFiveHourReset := time.Date(2026, time.August, 20, 11, 56, 51, 0, time.UTC).Unix()
	if fiveHour.ResetAtUnix != wantFiveHourReset {
		t.Fatalf("5h reset = %d, want %d", fiveHour.ResetAtUnix, wantFiveHourReset)
	}
	weekly := result.Windows[quota.Window7Day]
	if weekly.RemainingPct != 73 {
		t.Fatalf("7d remaining = %d, want 73", weekly.RemainingPct)
	}
	wantWeeklyReset := time.Date(2026, time.August, 27, 6, 56, 51, 0, time.UTC).Unix()
	if weekly.ResetAtUnix != wantWeeklyReset {
		t.Fatalf("7d reset = %d, want %d", weekly.ResetAtUnix, wantWeeklyReset)
	}
}

func TestParseQuotaSummaryRejectsMalformedJSON(t *testing.T) {
	if _, err := parseQuotaSummary([]byte(`{"groups":`)); err == nil {
		t.Fatal("parseQuotaSummary() error = nil, want malformed JSON error")
	}
}

func TestParseQuotaSummaryRequiresExactlyOneOfEachGeminiBucket(t *testing.T) {
	tests := []struct {
		name    string
		buckets string
	}{
		{
			name:    "missing weekly",
			buckets: `{"bucketId":"gemini-5h","remainingFraction":0.5,"resetTime":"2026-08-20T11:56:51Z"}`,
		},
		{
			name:    "missing five hour",
			buckets: `{"bucketId":"gemini-weekly","remainingFraction":0.5,"resetTime":"2026-08-27T06:56:51Z"}`,
		},
		{
			name: "duplicate weekly",
			buckets: validGeminiBuckets + `,
				{"bucketId":"gemini-weekly","remainingFraction":0.4,"resetTime":"2026-08-27T06:56:51Z"}`,
		},
		{
			name: "missing fraction",
			buckets: `
				{"bucketId":"gemini-weekly","resetTime":"2026-08-27T06:56:51Z"},
				{"bucketId":"gemini-5h","remainingFraction":0.5,"resetTime":"2026-08-20T11:56:51Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseQuotaSummary(quotaFixture(tt.buckets)); err == nil {
				t.Fatal("parseQuotaSummary() error = nil, want required bucket error")
			}
		})
	}
}

func TestParseQuotaSummaryClampsFractionsAndMarksExhausted(t *testing.T) {
	buckets := `
		{"bucketId":"gemini-weekly","remainingFraction":1.4,"resetTime":"2026-08-27T06:56:51Z"},
		{"bucketId":"gemini-5h","remainingFraction":-0.2,"resetTime":"2026-08-20T11:56:51Z"}`
	result, err := parseQuotaSummary(quotaFixture(buckets))
	if err != nil {
		t.Fatalf("parseQuotaSummary() error = %v", err)
	}
	if got := result.Windows[quota.Window7Day].RemainingPct; got != 100 {
		t.Fatalf("7d remaining = %d, want 100", got)
	}
	if got := result.Windows[quota.Window5Hour].RemainingPct; got != 0 {
		t.Fatalf("5h remaining = %d, want 0", got)
	}
	if result.Status != quota.StatusExhausted {
		t.Fatalf("status = %q, want %q", result.Status, quota.StatusExhausted)
	}
}

func TestParseQuotaSummaryUsesZeroForInvalidResetTime(t *testing.T) {
	buckets := `
		{"bucketId":"gemini-weekly","remainingFraction":0.5,"resetTime":"invalid"},
		{"bucketId":"gemini-5h","remainingFraction":0.5,"resetTime":""}`
	result, err := parseQuotaSummary(quotaFixture(buckets))
	if err != nil {
		t.Fatalf("parseQuotaSummary() error = %v", err)
	}
	if result.Windows[quota.Window7Day].ResetAtUnix != 0 || result.Windows[quota.Window5Hour].ResetAtUnix != 0 {
		t.Fatalf("reset times = %#v, want zero values", result.Windows)
	}
}
