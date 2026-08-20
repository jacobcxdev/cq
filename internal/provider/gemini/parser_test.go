package gemini

import (
	"fmt"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

const validGeminiBuckets = `
	{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.734,"reset_time":"2026-08-27T06:56:51Z"},
	{"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.426,"reset_time":"2026-08-20T11:56:51Z"}`

func usageFixture(status string, numTurns, totalTokens int, commandName, buckets string) []byte {
	return []byte(fmt.Sprintf(`{
		"conversation_id":"",
		"status":%q,
		"response":"quota text not used",
		"duration_seconds":0,
		"num_turns":%d,
		"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":%d},
		"command":{"name":%q,"data":{"description":"quota description","groups":[
			{"name":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[%s]},
			{"name":"Claude and GPT models","description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS","buckets":[
				{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.01,"reset_time":"2026-08-27T06:56:51Z"},
				{"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.02,"reset_time":"2026-08-20T11:56:51Z"}
			]}
		]}}
	}`, status, numTurns, totalTokens, commandName, buckets))
}

func TestParseUsageMapsOnlyGeminiBuckets(t *testing.T) {
	result, err := parseUsage(usageFixture("SUCCESS", 0, 0, "usage", validGeminiBuckets))
	if err != nil {
		t.Fatalf("parseUsage() error = %v", err)
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

func TestParseUsageRejectsMalformedJSON(t *testing.T) {
	if _, err := parseUsage([]byte(`{"status":`)); err == nil {
		t.Fatal("parseUsage() error = nil, want malformed JSON error")
	}
}

func TestParseUsageRejectsUnsafeEnvelope(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "non-success status", data: usageFixture("ERROR", 0, 0, "usage", validGeminiBuckets)},
		{name: "model turn", data: usageFixture("SUCCESS", 1, 0, "usage", validGeminiBuckets)},
		{name: "token use", data: usageFixture("SUCCESS", 0, 1, "usage", validGeminiBuckets)},
		{name: "wrong command", data: usageFixture("SUCCESS", 0, 0, "status", validGeminiBuckets)},
		{name: "missing usage", data: []byte(`{"status":"SUCCESS","num_turns":0,"command":{"name":"usage","data":{"groups":[]}}}`)},
		{name: "missing command", data: []byte(`{"status":"SUCCESS","num_turns":0,"usage":{"total_tokens":0}}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseUsage(tt.data); err == nil {
				t.Fatal("parseUsage() error = nil, want safety validation error")
			}
		})
	}
}

func TestParseUsageRequiresExactlyOneOfEachGeminiBucket(t *testing.T) {
	tests := []struct {
		name    string
		buckets string
	}{
		{
			name:    "missing weekly",
			buckets: `{"id":"gemini-5h","remaining_fraction":0.5,"reset_time":"2026-08-20T11:56:51Z"}`,
		},
		{
			name:    "missing five hour",
			buckets: `{"id":"gemini-weekly","remaining_fraction":0.5,"reset_time":"2026-08-27T06:56:51Z"}`,
		},
		{
			name: "duplicate weekly",
			buckets: validGeminiBuckets + `,
				{"id":"gemini-weekly","remaining_fraction":0.4,"reset_time":"2026-08-27T06:56:51Z"}`,
		},
		{
			name: "missing fraction",
			buckets: `
				{"id":"gemini-weekly","reset_time":"2026-08-27T06:56:51Z"},
				{"id":"gemini-5h","remaining_fraction":0.5,"reset_time":"2026-08-20T11:56:51Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseUsage(usageFixture("SUCCESS", 0, 0, "usage", tt.buckets)); err == nil {
				t.Fatal("parseUsage() error = nil, want required bucket error")
			}
		})
	}
}

func TestParseUsageClampsFractionsAndMarksExhausted(t *testing.T) {
	buckets := `
		{"id":"gemini-weekly","remaining_fraction":1.4,"reset_time":"2026-08-27T06:56:51Z"},
		{"id":"gemini-5h","remaining_fraction":-0.2,"reset_time":"2026-08-20T11:56:51Z"}`
	result, err := parseUsage(usageFixture("SUCCESS", 0, 0, "usage", buckets))
	if err != nil {
		t.Fatalf("parseUsage() error = %v", err)
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
