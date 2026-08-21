package main

import (
	"context"
	"fmt"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func enrichCodexProxyEligibility(ctx context.Context, report *app.Report, codexProvider *codexprov.Provider) error {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	accounts, err := codexProvider.RoutingAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list Codex routing accounts: %w", err)
	}
	addCodexProxyEligibility(report, cfg, accounts)
	return nil
}

func addCodexProxyEligibility(report *app.Report, cfg *proxy.Config, accounts []codexprov.RoutingAccount) {
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

	app.AddProxyEligibility(report, provider.Codex, func(result quota.Result) bool {
		if unrestricted {
			return true
		}
		account, found := byAccountID[result.AccountID]
		if !found && result.Email != "" && !duplicateEmail[result.Email] {
			account, found = byEmail[result.Email]
		}
		return found && (allowed[account.Key] || (includeActive && account.Active))
	})
}
