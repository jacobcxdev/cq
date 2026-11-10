package zai

import (
	"fmt"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestQuotaCapacityUsesCurrentPaidAllowance(t *testing.T) {
	for _, tt := range []struct {
		level, wantPlan string
		fiveHour, week  int
		wantMultiplier  int
	}{
		{"lite", "Lite", 2000, 10000, 1},
		{"pro", "Pro", 12000, 60000, 6},
		{"max", "Max", 28000, 140000, 14},
	} {
		t.Run(tt.level, func(t *testing.T) {
			body := fmt.Sprintf(`{"code":200,"data":{"level":%q,"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":%d,"percentage":25},{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":%d,"percentage":50}]}}`, tt.level, tt.fiveHour, tt.week)
			result, code := parseQuota([]byte(body))
			multiplier, known := quota.ParseMultiplier(result.RateLimitTier)
			if code != "" || result.Plan != tt.wantPlan || !known || multiplier != tt.wantMultiplier || result.Windows[quota.Window5Hour].RemainingPct != 75 || result.Windows[quota.Window7Day].RemainingPct != 50 {
				t.Fatalf("current paid allowance lost: plan=%q multiplier=%d known=%t code=%q", result.Plan, multiplier, known, code)
			}
		})
	}
}

func TestQuotaCapacityDoesNotInventUnknownAllowance(t *testing.T) {
	for _, tt := range []struct {
		name, level, limits string
	}{
		{"unknown plan", "future", `{"type":"CREDIT_LIMIT","unit":3,"usage":28000,"percentage":25}`},
		{"absent plan", "", `{"type":"CREDIT_LIMIT","unit":3,"usage":28000,"percentage":25}`},
		{"contradictory credit cap", "max", `{"type":"CREDIT_LIMIT","unit":3,"usage":12000,"percentage":25}`},
		{"contradictory weekly cap", "max", `{"type":"CREDIT_LIMIT","unit":3,"usage":28000,"percentage":25},{"type":"CREDIT_LIMIT","unit":6,"usage":60000,"percentage":50}`},
		{"legacy token cap", "max", `{"type":"TOKENS_LIMIT","unit":3,"usage":1600,"percentage":25}`},
		{"legacy tokens omitted cap", "max", `{"type":"TOKENS_LIMIT","unit":3,"percentage":25}`},
		{"legacy tokens null cap", "max", `{"type":"TOKENS_LIMIT","unit":3,"usage":null,"percentage":25}`},
		{"legacy weekly tokens omitted cap", "max", `{"type":"CREDIT_LIMIT","unit":3,"usage":28000,"percentage":25},{"type":"TOKENS_LIMIT","unit":6,"percentage":50}`},
		{"legacy weekly tokens null cap", "max", `{"type":"CREDIT_LIMIT","unit":3,"usage":28000,"percentage":25},{"type":"TOKENS_LIMIT","unit":6,"usage":null,"percentage":50}`},
		{"invalid credit cap", "max", `{"type":"CREDIT_LIMIT","unit":3,"usage":-1,"percentage":25}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, code := parseQuota([]byte(fmt.Sprintf(`{"data":{"level":%q,"limits":[%s]}}`, tt.level, tt.limits)))
			if code != "" || !result.IsUsable() || result.Windows[quota.Window5Hour].RemainingPct != 75 {
				t.Fatal("unknown capacity hid otherwise valid quota percentages")
			}
			if _, known := quota.ParseMultiplier(result.RateLimitTier); known {
				t.Fatal("unknown or conflicting allowance became paid capacity")
			}
		})
	}
}
