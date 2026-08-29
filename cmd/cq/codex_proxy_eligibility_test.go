package main

import (
	"context"
	"encoding/json"
	"errors"
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

func TestAddCodexProxyEligibilityIncludesActiveAccountOutsideAllowlist(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex,
		Results: []quota.Result{
			{Status: quota.StatusOK, AccountID: "account-a"},
			{Status: quota.StatusOK, AccountID: "account-b"},
			{Status: quota.StatusOK, AccountID: "account-c"},
		},
	}}}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a", Active: true},
		{Key: "route-b", AccountID: "account-b"},
		{Key: "route-c", AccountID: "account-c"},
	}
	cfg := &proxy.Config{CodexRoutingAccountKeys: []codexprov.AccountKey{"route-b", "route-c"}}

	addCodexProxyEligibility(&report, cfg, accounts)

	eligibility := report.Providers[0].ProxyEligibility
	if eligibility.EligibleCount != 3 || eligibility.ExcludedCount != 0 {
		t.Fatalf("proxy eligibility counts = %#v, want active account plus allowlist", eligibility)
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
		{Key: "route-a", AccountID: "account-a", Active: true},
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

func TestAddCodexProxyEligibilityAddsDistinctBoundSubsetPools(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex,
		Results: []quota.Result{
			{Status: quota.StatusOK, AccountID: "account-a"},
			{Status: quota.StatusOK, AccountID: "account-b"},
			{Status: quota.StatusOK, AccountID: "account-c"},
		},
	}}}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a"},
		{Key: "route-b", AccountID: "account-b"},
		{Key: "route-c", AccountID: "account-c"},
	}
	policy := proxy.RoutingPolicyDocument{
		Pools: []proxy.AccountPoolDocument{
			{Name: "zeta", Members: []codexprov.AccountKey{"route-b"}},
			{Name: "cyber", Members: []codexprov.AccountKey{"route-a", "route-c"}},
			{Name: "all", Members: []codexprov.AccountKey{"route-a", "route-b", "route-c"}},
			{Name: "unused", Members: []codexprov.AccountKey{"route-a"}},
		},
		SessionBindings: []proxy.SessionBindingDocument{
			{SessionDigest: "binding-zeta-a", Pool: "zeta"},
			{SessionDigest: "binding-cyber", Pool: "cyber"},
			{SessionDigest: "binding-zeta-b", Pool: "zeta"},
			{SessionDigest: "binding-all", Pool: "all"},
		},
	}

	addCodexProxyEligibility(&report, &proxy.Config{}, accounts, policy)

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var got struct {
		Providers []struct {
			ProxyPools []struct {
				Name            string `json:"name"`
				DiscoveredCount int    `json:"discovered_count"`
				EligibleCount   int    `json:"eligible_count"`
				ExcludedCount   int    `json:"excluded_count"`
			} `json:"proxy_pools"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(got.Providers) != 1 || len(got.Providers[0].ProxyPools) != 2 {
		t.Fatalf("proxy pools = %s, want two bound subsets", body)
	}
	pools := got.Providers[0].ProxyPools
	if pools[0].Name != "cyber" || pools[0].DiscoveredCount != 3 || pools[0].EligibleCount != 2 || pools[0].ExcludedCount != 1 {
		t.Fatalf("first proxy pool = %#v, want cyber 2/3", pools[0])
	}
	if pools[1].Name != "zeta" || pools[1].DiscoveredCount != 3 || pools[1].EligibleCount != 1 || pools[1].ExcludedCount != 2 {
		t.Fatalf("second proxy pool = %#v, want zeta 1/3", pools[1])
	}
}

func TestEnrichCodexProxyEligibilityUsesOptionalLivePolicy(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex,
		Results: []quota.Result{
			{Status: quota.StatusOK, AccountID: "account-a"},
			{Status: quota.StatusOK, AccountID: "account-b"},
			{Status: quota.StatusOK, AccountID: "account-c"},
		},
	}}}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a"},
		{Key: "route-b", AccountID: "account-b"},
		{Key: "route-c", AccountID: "account-c"},
	}
	policy := proxy.RoutingPolicyDocument{
		Pools:           []proxy.AccountPoolDocument{{Name: "cyber", Members: []codexprov.AccountKey{"route-a", "route-c"}}},
		SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: "binding-cyber", Pool: "cyber"}},
	}

	err := enrichCodexProxyEligibilityWithDependencies(context.Background(), &report, codexProxyEligibilityDependencies{
		LoadConfig:      func() (*proxy.Config, error) { return &proxy.Config{}, nil },
		RoutingAccounts: func(context.Context) ([]codexprov.RoutingAccount, error) { return accounts, nil },
		LoadPolicy:      func(context.Context, *proxy.Config) (proxy.RoutingPolicyDocument, error) { return policy, nil },
	})
	if err != nil {
		t.Fatalf("enrich proxy eligibility: %v", err)
	}
	if pools := report.Providers[0].ProxyPools; len(pools) != 1 || pools[0].Name != "cyber" || pools[0].EligibleCount != 2 {
		t.Fatalf("proxy pools = %#v, want bound cyber 2/3", pools)
	}
}

func TestEnrichCodexProxyEligibilityIgnoresUnavailableLivePolicy(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID:      provider.Codex,
		Results: []quota.Result{{Status: quota.StatusOK, AccountID: "account-a"}},
	}}}
	accounts := []codexprov.RoutingAccount{{Key: "route-a", AccountID: "account-a"}}

	err := enrichCodexProxyEligibilityWithDependencies(context.Background(), &report, codexProxyEligibilityDependencies{
		LoadConfig:      func() (*proxy.Config, error) { return &proxy.Config{}, nil },
		RoutingAccounts: func(context.Context) ([]codexprov.RoutingAccount, error) { return accounts, nil },
		LoadPolicy: func(context.Context, *proxy.Config) (proxy.RoutingPolicyDocument, error) {
			return proxy.RoutingPolicyDocument{}, errors.New("proxy offline")
		},
	})
	if err != nil {
		t.Fatalf("enrich proxy eligibility: %v", err)
	}
	if eligibility := report.Providers[0].ProxyEligibility; eligibility == nil || eligibility.EligibleCount != 1 {
		t.Fatalf("global proxy eligibility = %#v, want preserved 1/1", eligibility)
	}
}
