package aggregate

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestComputeZAIExcludesUnknownAndSeparateStartCapacity(t *testing.T) {
	row := func(plan, tier string) quota.Result {
		return quota.Result{Plan: plan, RateLimitTier: tier, Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 18000}}}
	}
	unknown := row("Max", "")
	start := row("ZCode Start", "")
	known := row("Max", "zai_coding_14x")
	for _, members := range [][]quota.Result{{unknown, unknown}, {unknown, known}, {start, known}} {
		windows, summary := Compute(members, 1000, "zai", nil)
		if windows != nil || summary != nil {
			t.Fatal("unknown or Start capacity became paid subscription capacity")
		}
	}
	windows, summary := Compute([]quota.Result{unknown, start, known, known}, 1000, "zai", nil)
	if len(windows) != 1 || summary == nil || summary.Count != 2 || summary.TotalMulti != 28 || summary.Label != "2 × Max 14x = 28x" {
		t.Fatalf("paid Max aggregate included unknown capacity: %+v", summary)
	}
	if unknown.RateLimitTier != "" || start.RateLimitTier != "" {
		t.Fatal("aggregation changed source metadata")
	}
}
