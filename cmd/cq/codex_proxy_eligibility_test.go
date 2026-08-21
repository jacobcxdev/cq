package main

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestAddCodexProxyEligibilityUsesEffectiveRoutingAllowlist(t *testing.T) {
	report := app.Report{
		GeneratedAt: time.Unix(1_000, 0),
		Providers: []app.ProviderReport{{
			ID: provider.Codex,
			Results: []quota.Result{
				{Status: quota.StatusOK, AccountID: "account-a", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 97, ResetAtUnix: 303_400}}},
				{Status: quota.StatusOK, AccountID: "account-b", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 54, ResetAtUnix: 303_400}}},
				{Status: quota.StatusOK, AccountID: "account-c", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 55, ResetAtUnix: 303_400}}},
			},
		}},
	}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a"},
		{Key: "route-b", AccountID: "account-b"},
		{Key: "route-c", AccountID: "account-c"},
	}
	cfg := &proxy.Config{CodexRoutingAccountKeys: []codexprov.AccountKey{"route-b", "route-c"}}

	addCodexProxyEligibility(&report, cfg, accounts)

	eligibility := report.Providers[0].ProxyEligibility
	if eligibility == nil {
		t.Fatal("proxy eligibility missing")
	}
	if eligibility.DiscoveredCount != 3 || eligibility.EligibleCount != 2 || eligibility.ExcludedCount != 1 {
		t.Fatalf("proxy eligibility counts = %#v", eligibility)
	}
	if remaining := eligibility.Aggregate.Windows[quota.Window7Day].RemainingPct; remaining != 55 {
		t.Fatalf("proxy-eligible remaining = %d, want 55", remaining)
	}
}

func TestAddCodexProxyEligibilityUsesPinForNewWork(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex,
		Results: []quota.Result{
			{Status: quota.StatusOK, AccountID: "account-a"},
			{Status: quota.StatusOK, AccountID: "account-b"},
		},
	}}}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a"},
		{Key: "route-b", AccountID: "account-b"},
	}
	cfg := &proxy.Config{
		CodexRoutingAccountKeys:      []codexprov.AccountKey{"route-a", "route-b"},
		CodexRoutingPinnedAccountKey: "route-b",
	}

	addCodexProxyEligibility(&report, cfg, accounts)

	eligibility := report.Providers[0].ProxyEligibility
	if eligibility.EligibleCount != 1 || eligibility.ExcludedCount != 1 {
		t.Fatalf("pinned proxy eligibility counts = %#v", eligibility)
	}
}
