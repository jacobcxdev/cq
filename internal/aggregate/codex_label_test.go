package aggregate

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestComputeCodexRequiresKnownUsageMultipliers(t *testing.T) {
	for _, tt := range []struct {
		name      string
		members   []quota.Result
		wantLabel string
		wantTotal int
	}{
		{name: "unknown business variant suppresses aggregate", members: []quota.Result{{Plan: "self_serve_business_promax"}}},
		{name: "explicit nonnumeric business tier stays unknown", members: []quota.Result{{Plan: "self_serve_business_prolite", RateLimitTier: "business_custom"}}},
		{name: "invalid numeric business tier stays unknown", members: []quota.Result{{Plan: "self_serve_business_prolite", RateLimitTier: "business_0x"}}},
		{name: "explicit business multiplier", members: []quota.Result{{Plan: "self_serve_business_prolite", RateLimitTier: "business_8x"}}, wantLabel: "1 × business premium 8x + 1 × pro 50x = 58x", wantTotal: 58},
		{name: "explicit business baseline", members: []quota.Result{{Plan: "self_serve_business_prolite", RateLimitTier: "business_1x"}}, wantLabel: "1 × business premium + 1 × pro 50x = 51x", wantTotal: 51},
		{name: "Business Premium documented allowance", members: []quota.Result{{Plan: "self_serve_business_prolite"}}, wantLabel: "1 × business premium 5x + 1 × pro 50x = 55x", wantTotal: 55},
		{name: "Business Standard known baseline", members: []quota.Result{{Plan: "team"}}, wantLabel: "1 × business + 1 × pro 50x = 51x", wantTotal: 51},
		{name: "Plus known baseline", members: []quota.Result{{Plan: "plus"}}, wantLabel: "1 × plus + 1 × pro 50x = 51x", wantTotal: 51},
		{name: "explicit nonnumeric Plus tier stays unknown", members: []quota.Result{{Plan: "plus", RateLimitTier: "custom"}}},
		{name: "fully known personal usage", members: []quota.Result{{Plan: "prolite", RateLimitTier: "codex_prolite_10x"}, {Plan: "pro", RateLimitTier: "codex_pro_20x"}}, wantLabel: "1 × pro 10x + 1 × pro 20x + 1 × pro 50x = 80x", wantTotal: 80},
	} {
		t.Run(tt.name, func(t *testing.T) {
			results := append(tt.members, quota.Result{Plan: "promax", RateLimitTier: "codex_promax_50x"})
			for i := range results {
				results[i].Status = quota.StatusOK
				results[i].Windows = map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 18000}}
			}
			rawPlans := make([]string, len(results))
			for i := range results {
				rawPlans[i] = results[i].Plan
			}
			windows, summary := Compute(results, 1000, "codex", nil)
			if tt.wantLabel == "" {
				if windows != nil || summary != nil {
					t.Fatalf("unknown allowance produced windows=%v summary=%+v", windows, summary)
				}
			} else if len(windows) == 0 || summary == nil || summary.Label != tt.wantLabel || summary.TotalMulti != tt.wantTotal {
				t.Fatalf("summary = %+v, want %q with %dx usage", summary, tt.wantLabel, tt.wantTotal)
			}
			for i := range results {
				if results[i].Plan != rawPlans[i] {
					t.Fatalf("raw plan metadata changed: %+v", results)
				}
			}
		})
	}
}
