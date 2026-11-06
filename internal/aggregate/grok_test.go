package aggregate

import (
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestComputeGrokExcludesUnknownCapacity(t *testing.T) {
	row := func(plan, tier string) quota.Result {
		return quota.Result{Plan: plan, RateLimitTier: tier, Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 50, ResetAtUnix: 604800}}}
	}
	unknown := row("Unverified legacy tier", "")
	for _, members := range [][]quota.Result{{unknown, unknown}, {unknown, row("Known", "grok_4x")}} {
		windows, summary := Compute(members, 1000, "grok", nil)
		if windows != nil || summary != nil {
			t.Fatalf("unknown capacity became weighted quota: %#v %#v", windows, summary)
		}
	}
	results := []quota.Result{unknown, row("Known base", "grok_1x"), row("Known larger", "grok_4x")}
	windows, summary := Compute(results, 1000, "grok", nil)
	if len(windows) == 0 || summary == nil || summary.Count != 2 || summary.TotalMulti != 5 || strings.Contains(summary.Label, "Unverified") {
		t.Fatalf("unknown member remained: %#v %#v", windows, summary)
	}
	if results[0].Plan != unknown.Plan || results[0].RateLimitTier != "" {
		t.Fatal("input result mutated")
	}
}
