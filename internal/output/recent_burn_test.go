package output

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type forecastClock struct{ now time.Time }

func (c *forecastClock) Now() time.Time { return c.now }

type forecastProvider struct{ results []quota.Result }

func (p *forecastProvider) Fetch(context.Context, time.Time) ([]quota.Result, error) {
	return p.results, nil
}

func TestRecentBurnETAFlowsFromHistoryToAccountAndAggregate(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	clock := &forecastClock{now: now}
	source := &forecastProvider{}
	store, err := history.New(fsutil.NewMemFS(), "/history")
	if err != nil {
		t.Fatal(err)
	}
	runner := app.Runner{Clock: clock, History: store, Services: map[provider.ID]provider.Services{provider.Codex: {Usage: source}}}
	var report app.Report
	for i, pct := range []int{24, 21, 18} {
		clock.now = now.Add(time.Duration(i-2) * 30 * time.Minute)
		source.results = nil
		for _, account := range []string{"one", "two", "three"} {
			source.results = append(source.results, quota.Result{AccountID: account, Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: pct, ResetAtUnix: now.Unix() + 44*3600},
			}})
		}
		report, err = runner.BuildReport(ctx, app.RunRequest{Providers: []provider.ID{provider.Codex}, Refresh: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Six percentage points/hour leaves 18% lasting three hours. Lifetime
	// consumption instead predicts roughly 27 hours for this five-day-old window.
	model := BuildTTYModel(report, now)
	for _, section := range model.Sections {
		if got := stripANSI(section.WindowRows[0].Burndown); !strings.Contains(got, "     3h") {
			t.Errorf("account burndown = %q, want 3h", got)
		}
		if got := stripANSI(section.WindowRows[0].PaceDiff); !strings.Contains(got, "-8") {
			t.Errorf("budget pace changed: %q, want -8", got)
		}
	}
	if got := report.Providers[0].Aggregate.Windows[quota.Window7Day].Burndown; got != 10800 {
		t.Errorf("aggregate burndown = %d, want 10800", got)
	}
	// Forecast annotation must not mutate provider inputs or leak into cache JSON.
	for _, result := range source.results {
		if result.Windows[quota.Window7Day].RecentBurnRate != nil {
			t.Fatal("provider input mutated")
		}
	}
	encoded, err := json.Marshal(report.Providers[0].Results)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "RecentBurnRate") || strings.Contains(string(encoded), "recent_burn") {
		t.Fatalf("derived rate leaked into result JSON: %s", encoded)
	}
	eligible := func(r quota.Result) bool { return r.AccountID != "three" }
	app.AddProxyEligibility(&report, provider.Codex, eligible)
	app.AddProxyPool(&report, provider.Codex, "work", eligible)
	pr := report.Providers[0]
	if got := pr.ProxyEligibility.Aggregate.Windows[quota.Window7Day].Burndown; got != 10800 {
		t.Errorf("proxy burndown = %d, want 10800", got)
	}
	if got := pr.ProxyPools[0].Aggregate.Windows[quota.Window7Day].Burndown; got != 10800 {
		t.Errorf("pool burndown = %d, want 10800", got)
	}
}
