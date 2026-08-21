package main

import (
	"context"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestBuildCodexPrimerRequiresEnabledOwner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	catalog := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Visibility: "list"},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4", Visibility: "list"},
	}})
	fsys := fsutil.NewMemFS()
	inventory := &staticProxyCodexRoutingInventory{}
	cfg := &proxy.Config{CodexUpstream: "https://chatgpt.example/backend-api/codex"}
	primer, err := buildCodexPrimer(cfg, true, nil, inventory, catalog, fsys)
	if err != nil || primer != nil {
		t.Fatalf("disabled primer = %v, %v", primer, err)
	}
	cfg.CodexWindowPriming.Enabled = true
	primer, err = buildCodexPrimer(cfg, false, nil, inventory, catalog, fsys)
	if err != nil || primer != nil {
		t.Fatalf("delegate primer = %v, %v", primer, err)
	}
	primer, err = buildCodexPrimer(cfg, true, &proxy.CodexRequestRouter{}, inventory, catalog, fsys)
	if err != nil || primer == nil {
		t.Fatalf("owner primer = %v, %v", primer, err)
	}
}

func TestBuildCodexPrimerRejectsUnavailableOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	cfg := &proxy.Config{
		CodexUpstream: "https://chatgpt.example/backend-api/codex",
		CodexWindowPriming: proxy.CodexWindowPrimingConfig{
			Enabled: true, ModelOverrides: map[string]string{"spark": "missing"},
		},
	}
	catalog := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Visibility: "list"},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4", Visibility: "list"},
	}})
	if _, err := buildCodexPrimer(cfg, true, &proxy.CodexRequestRouter{}, &staticProxyCodexRoutingInventory{}, catalog, fsutil.NewMemFS()); err == nil {
		t.Fatal("unavailable override accepted")
	}
}

func TestBuildCodexPrimerEnumeratesAccountsOutsideRoutingAllowlist(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	source := &staticProxyCodexRoutingInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		{Key: "account-a"},
		{Key: "account-b"},
		{Key: "account-c"},
	}}}
	routing := newProxyCodexRoutingInventory(source, []codexprov.AccountKey{"account-b", "account-c"})
	router := &proxy.CodexRequestRouter{Scope: &proxy.CodexRequestScope{Inventory: routing}}
	catalog := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Visibility: "list"},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4", Visibility: "list"},
	}})
	cfg := &proxy.Config{
		CodexUpstream:      "https://chatgpt.example/backend-api/codex",
		CodexWindowPriming: proxy.CodexWindowPrimingConfig{Enabled: true},
	}

	primer, err := buildCodexPrimer(cfg, true, router, source, catalog, fsutil.NewMemFS())
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := primer.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("primer accounts = %d, want complete inventory of 3", len(accounts))
	}
	routingAccounts, err := router.AccountKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(routingAccounts) != 2 {
		t.Fatalf("routing accounts = %d, want allowlist of 2", len(routingAccounts))
	}
}
