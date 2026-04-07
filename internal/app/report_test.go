package app

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestBuildReportSingleProvider(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Codex: {{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
		}}},
	}
	r := buildReport(now, []provider.ID{provider.Codex}, fetched, nil)
	if len(r.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(r.Providers))
	}
	if r.Providers[0].Aggregate != nil {
		t.Fatal("single-account provider should not have aggregate")
	}
}

func TestBuildReportClaudeAggregate(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 1000 + 9000},
				}},
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
				}},
		},
	}
	r := buildReport(now, []provider.ID{provider.Claude}, fetched, nil)
	if r.Providers[0].Aggregate == nil {
		t.Fatal("Claude with 2 accounts should have aggregate")
	}
	if len(r.Providers[0].Aggregate.Windows) == 0 {
		t.Fatal("aggregate should have windows")
	}
}

func TestBuildReportPreservesOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	order := []provider.ID{provider.Gemini, provider.Claude, provider.Codex}
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {{Status: quota.StatusOK}},
		provider.Codex:  {{Status: quota.StatusOK}},
		provider.Gemini: {{Status: quota.StatusOK}},
	}
	r := buildReport(now, order, fetched, nil)
	for i, id := range order {
		if r.Providers[i].ID != id {
			t.Errorf("provider[%d] = %q, want %q", i, r.Providers[i].ID, id)
		}
	}
}

func TestBuildReportNoAggregateForSingleClaude(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {{Status: quota.StatusOK}},
	}
	r := buildReport(now, []provider.ID{provider.Claude}, fetched, nil)
	if r.Providers[0].Aggregate != nil {
		t.Fatal("single Claude account should not have aggregate")
	}
}
