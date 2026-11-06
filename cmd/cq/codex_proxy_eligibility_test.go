package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestAddCodexProxyEligibilityUsesFreshDisplayActiveWithoutPolicy(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{
		{AccountID: "old", Active: true}, {AccountID: "current"}, {AccountID: "missing"},
	}, DisplayActiveAccounts: map[int]bool{2: true}}}}
	accounts := []codexprov.RoutingAccount{{Key: "old", AccountID: "old"}, {Key: "current", AccountID: "current", Active: true}}
	addCodexProxyEligibility(&report, &proxy.Config{}, accounts)
	if got, want := report.Providers[0].DisplayActiveAccounts, (map[int]bool{0: false, 1: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("display active accounts = %v, want %v", got, want)
	}
	if !report.Providers[0].Results[0].Active || report.Providers[0].Results[1].Active {
		t.Fatal("fresh display identity rewrote cached result identity")
	}
}

func TestAddCodexProxyEligibilityAttachesDisplayValuesFromAllPools(t *testing.T) {
	results := []quota.Result{
		{Status: quota.StatusOK, AccountID: "account-c"},
		{Status: quota.StatusOK, AccountID: "account-a"},
		{Status: quota.StatusOK, AccountID: "account-b"},
		{Status: quota.StatusOK, AccountID: "active", Active: true},
		{Status: quota.StatusOK, AccountID: "zero"},
		{Status: quota.StatusOK, AccountID: "missing"},
	}
	report := app.Report{Providers: []app.ProviderReport{
		{ID: provider.Claude, DisplayAccountValues: map[int]uint32{0: 55}},
		{ID: provider.Codex, Results: results},
	}}
	before, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	accounts := []codexprov.RoutingAccount{
		{Key: "route-a", AccountID: "account-a"},
		{Key: "route-b", AccountID: "account-b"},
		{Key: "route-c", AccountID: "account-c"},
		{Key: "active", AccountID: "active", Active: true},
		{Key: "zero", AccountID: "zero"},
	}
	policy := proxy.RoutingPolicyDocument{
		Pools: []proxy.AccountPoolDocument{
			{Name: "bound", Value: 3, Members: []codexprov.AccountKey{"route-a", "route-b"}},
			{Name: "unbound", Value: 11, Members: []codexprov.AccountKey{"route-a", "route-c"}},
			{Name: "maximum", Value: proxy.PoolValue(^uint32(0)), Members: []codexprov.AccountKey{"route-c"}},
			{Name: "zero", Members: []codexprov.AccountKey{"route-b", "zero"}},
		},
		SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: "session", Pool: "bound"}},
	}
	addCodexProxyEligibility(&report, &proxy.Config{CodexRoutingAccountKeys: []codexprov.AccountKey{"route-a"}}, accounts, policy)
	pr := report.Providers[1]
	want := map[int]uint32{0: ^uint32(0), 1: 11, 2: 3, 3: 0, 4: 0}
	if !reflect.DeepEqual(pr.DisplayAccountValues, want) {
		t.Fatalf("display values = %v, want %v", pr.DisplayAccountValues, want)
	}
	after, err := json.Marshal(pr.Results)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("display metadata changed result order or contents: %s", after)
	}
	if !reflect.DeepEqual(report.Providers[0].DisplayAccountValues, map[int]uint32{0: 55}) {
		t.Fatal("Codex enrichment changed another provider's metadata")
	}
	if pr.ProxyEligibility == nil || pr.ProxyEligibility.EligibleCount != 2 || len(pr.ProxyPools) != 1 || pr.ProxyPools[0].Name != "bound" {
		t.Fatalf("display values changed routing eligibility or bound pool views: %+v", pr)
	}
}

func TestAddCodexProxyEligibilityDisplayValueIdentityMapping(t *testing.T) {
	accounts := []codexprov.RoutingAccount{
		{Key: "a", AccountID: "account-a", Email: "shared@example.com"},
		{Key: "b", AccountID: "account-b", Email: "shared@example.com"},
		{Key: "c", AccountID: "account-c", Email: "unique@example.com"},
		{Key: "d", AccountID: "collision", Email: "first@example.com"},
		{Key: "e", AccountID: "collision", Email: "second@example.com"},
	}
	report := app.Report{Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{
		{AccountID: "account-a", Email: "unique@example.com"},
		{Email: "unique@example.com"},
		{Email: "shared@example.com"},
		{AccountID: "old-id", Email: "unique@example.com"},
		{AccountID: "unknown", Email: "unknown@example.com"},
		{AccountID: "collision", Email: "first@example.com"},
		{Email: "first@example.com"},
		{AccountID: "account-b", Email: "shared@example.com"},
	}}}}
	policy := proxy.RoutingPolicyDocument{Pools: []proxy.AccountPoolDocument{
		{Name: "a", Value: 2, Members: []codexprov.AccountKey{"a"}},
		{Name: "b", Value: 4, Members: []codexprov.AccountKey{"b"}},
		{Name: "c", Value: 7, Members: []codexprov.AccountKey{"c"}},
		{Name: "d", Value: 9, Members: []codexprov.AccountKey{"d"}},
		{Name: "e", Value: 12, Members: []codexprov.AccountKey{"e"}},
	}}
	addCodexProxyEligibility(&report, &proxy.Config{}, accounts, policy)
	want := map[int]uint32{0: 2, 1: 7, 3: 7, 6: 9, 7: 4}
	if got := report.Providers[0].DisplayAccountValues; !reflect.DeepEqual(got, want) {
		t.Fatalf("identity-mapped values = %v, want %v", got, want)
	}
}

func TestAddCodexProxyEligibilityDisplayValuesDoNotChangeReportJSON(t *testing.T) {
	makeReport := func(value proxy.PoolValue) app.Report {
		report := app.Report{GeneratedAt: time.Unix(1000, 0), Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{
			{Status: quota.StatusOK, Plan: "plus", AccountID: "account-a"},
			{Status: quota.StatusOK, Plan: "plus", AccountID: "account-b", Active: true},
			{Status: quota.StatusOK, Plan: "plus", AccountID: "account-c"},
		}}}}
		accounts := []codexprov.RoutingAccount{{Key: "a", AccountID: "account-a"}, {Key: "b", AccountID: "account-b"}, {Key: "c", AccountID: "account-c"}}
		policy := proxy.RoutingPolicyDocument{
			Pools:           []proxy.AccountPoolDocument{{Name: "pool", Value: value, Members: []codexprov.AccountKey{"a", "b"}}},
			SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: "session", Pool: "pool"}},
		}
		addCodexProxyEligibility(&report, &proxy.Config{}, accounts, policy)
		return report
	}
	before, after := makeReport(1), makeReport(33)
	if before.Providers[0].DisplayAccountValues[0] != 1 || after.Providers[0].DisplayAccountValues[0] != 33 {
		t.Fatal("policy value change did not reach display metadata")
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("value-only policy change altered report JSON:\nbefore: %s\nafter: %s", beforeJSON, afterJSON)
	}
}

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
			{Name: "Zeta", Members: []codexprov.AccountKey{"route-b"}},
			{Name: "Cyber", Value: 10, Members: []codexprov.AccountKey{"route-a", "route-c"}},
			{Name: "all", Members: []codexprov.AccountKey{"route-a", "route-b", "route-c"}},
			{Name: "unused", Members: []codexprov.AccountKey{"route-a"}},
		},
		SessionBindings: []proxy.SessionBindingDocument{
			{SessionDigest: "binding-zeta-a", Pool: "Zeta"},
			{SessionDigest: "binding-cyber", Pool: "Cyber"},
			{SessionDigest: "binding-zeta-b", Pool: "Zeta"},
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
	if pools[0].Name != "Cyber" || pools[0].DiscoveredCount != 3 || pools[0].EligibleCount != 2 || pools[0].ExcludedCount != 1 {
		t.Fatalf("first proxy pool = %#v, want Cyber 2/3", pools[0])
	}
	if pools[1].Name != "Zeta" || pools[1].DiscoveredCount != 3 || pools[1].EligibleCount != 1 || pools[1].ExcludedCount != 2 {
		t.Fatalf("second proxy pool = %#v, want Zeta 1/3", pools[1])
	}
	if bytes.Contains(body, []byte(`"value"`)) {
		t.Fatalf("quota report exposed pool value: %s", body)
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
		Pools:           []proxy.AccountPoolDocument{{Name: "Cyber", Value: 10, Members: []codexprov.AccountKey{"route-a", "route-c"}}},
		SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: "binding-cyber", Pool: "Cyber"}},
	}

	err := enrichCodexProxyEligibilityWithDependencies(context.Background(), &report, codexProxyEligibilityDependencies{
		LoadConfig:      func() (*proxy.Config, error) { return &proxy.Config{}, nil },
		RoutingAccounts: func(context.Context) ([]codexprov.RoutingAccount, error) { return accounts, nil },
		LoadPolicy:      func(context.Context, *proxy.Config) (proxy.RoutingPolicyDocument, error) { return policy, nil },
	})
	if err != nil {
		t.Fatalf("enrich proxy eligibility: %v", err)
	}
	if pools := report.Providers[0].ProxyPools; len(pools) != 1 || pools[0].Name != "Cyber" || pools[0].EligibleCount != 2 {
		t.Fatalf("proxy pools = %#v, want bound Cyber 2/3", pools)
	}
}

func TestEnrichCodexProxyEligibilityIgnoresUnavailableLivePolicy(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{
		ID:                   provider.Codex,
		Results:              []quota.Result{{Status: quota.StatusOK, AccountID: "account-a"}},
		DisplayAccountValues: map[int]uint32{0: 99},
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
	if got := report.Providers[0].DisplayAccountValues; len(got) != 0 {
		t.Fatalf("unavailable policy retained known display values: %v", got)
	}
}
