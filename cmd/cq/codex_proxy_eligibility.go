package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

type codexProxyEligibilityDependencies struct {
	LoadConfig      func() (*proxy.Config, error)
	RoutingAccounts func(context.Context) ([]codexprov.RoutingAccount, error)
	LoadPolicy      func(context.Context, *proxy.Config) (proxy.RoutingPolicyDocument, error)
}

func enrichCodexProxyEligibility(ctx context.Context, report *app.Report, codexProvider *codexprov.Provider) error {
	return enrichCodexProxyEligibilityWithDependencies(ctx, report, codexProxyEligibilityDependencies{
		LoadConfig:      proxy.LoadConfig,
		RoutingAccounts: codexProvider.RoutingAccounts,
		LoadPolicy:      loadLiveCodexRoutingPolicy,
	})
}

func enrichCodexProxyEligibilityWithDependencies(ctx context.Context, report *app.Report, deps codexProxyEligibilityDependencies) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	accounts, err := deps.RoutingAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list Codex routing accounts: %w", err)
	}
	policy, err := deps.LoadPolicy(ctx, cfg)
	if err == nil {
		addCodexProxyEligibility(report, cfg, accounts, policy)
		return nil
	}
	addCodexProxyEligibility(report, cfg, accounts)
	return nil
}

func loadLiveCodexRoutingPolicy(ctx context.Context, cfg *proxy.Config) (proxy.RoutingPolicyDocument, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy policy redirect refused")
		},
	}
	return proxyPolicyControl(ctx, proxyPolicyDependencies{
		LoadConfig: func() (*proxy.Config, error) { return cfg, nil },
		Doer:       client,
	}, http.MethodGet, proxy.RuntimePolicyPath, 0, nil)
}

func addCodexProxyEligibility(report *app.Report, cfg *proxy.Config, accounts []codexprov.RoutingAccount, policies ...proxy.RoutingPolicyDocument) {
	if report == nil || cfg == nil {
		return
	}
	allowed := make(map[codexprov.AccountKey]bool)
	includeActive := cfg.CodexRoutingPinnedAccountKey == ""
	if cfg.CodexRoutingPinnedAccountKey != "" {
		allowed[cfg.CodexRoutingPinnedAccountKey] = true
	} else {
		for _, key := range cfg.CodexRoutingAccountKeys {
			allowed[key] = true
		}
	}
	unrestricted := len(allowed) == 0

	byAccountID := make(map[string]codexprov.RoutingAccount, len(accounts))
	byEmail := make(map[string]codexprov.RoutingAccount, len(accounts))
	duplicateEmail := make(map[string]bool)
	for _, account := range accounts {
		if account.AccountID != "" {
			byAccountID[account.AccountID] = account
		}
		if account.Email != "" {
			if _, exists := byEmail[account.Email]; exists {
				duplicateEmail[account.Email] = true
			}
			byEmail[account.Email] = account
		}
	}

	accountForResult := func(result quota.Result) (codexprov.RoutingAccount, bool) {
		account, found := byAccountID[result.AccountID]
		if !found && result.Email != "" && !duplicateEmail[result.Email] {
			account, found = byEmail[result.Email]
		}
		return account, found
	}
	accountEligible := func(account codexprov.RoutingAccount) bool {
		return unrestricted || allowed[account.Key] || (includeActive && account.Active)
	}
	resultEligible := func(result quota.Result) bool {
		if unrestricted {
			return true
		}
		account, found := accountForResult(result)
		return found && accountEligible(account)
	}
	app.AddProxyEligibility(report, provider.Codex, resultEligible)

	if len(policies) == 0 {
		return
	}
	policy := policies[0]
	bound := make(map[string]bool, len(policy.SessionBindings))
	for _, binding := range policy.SessionBindings {
		bound[binding.Pool] = true
	}
	pools := make(map[string]map[codexprov.AccountKey]bool, len(bound))
	for _, pool := range policy.Pools {
		if !bound[pool.Name] {
			continue
		}
		members := make(map[codexprov.AccountKey]bool, len(pool.Members))
		for _, member := range pool.Members {
			members[member] = true
		}
		pools[pool.Name] = members
	}
	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		members := pools[name]
		app.AddProxyPool(report, provider.Codex, name, func(result quota.Result) bool {
			account, found := accountForResult(result)
			return found && accountEligible(account) && members[account.Key]
		})
	}
}
