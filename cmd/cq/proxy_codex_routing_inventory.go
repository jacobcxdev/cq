package main

import (
	"context"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

type proxyCodexRoutingInventory struct {
	source  codexprov.CredentialInventory
	allowed map[codexprov.AccountKey]bool
}

func newProxyCodexRoutingInventories(source codexprov.CredentialInventory, allowed []codexprov.AccountKey, pinned codexprov.AccountKey) (codexprov.CredentialInventory, codexprov.CredentialInventory) {
	if pinned == "" {
		inventory := newProxyCodexRoutingInventory(source, allowed)
		return inventory, inventory
	}
	selection := newProxyCodexRoutingInventory(source, []codexprov.AccountKey{pinned})
	if len(allowed) == 0 {
		return selection, source
	}
	continuityKeys := append([]codexprov.AccountKey(nil), allowed...)
	found := false
	for _, key := range continuityKeys {
		if key == pinned {
			found = true
			break
		}
	}
	if !found {
		continuityKeys = append(continuityKeys, pinned)
	}
	return selection, newProxyCodexRoutingInventory(source, continuityKeys)
}

func newProxyCodexRoutingInventory(source codexprov.CredentialInventory, allowed []codexprov.AccountKey) codexprov.CredentialInventory {
	if len(allowed) == 0 {
		return source
	}
	set := make(map[codexprov.AccountKey]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	return &proxyCodexRoutingInventory{source: source, allowed: set}
}

func (inventory *proxyCodexRoutingInventory) List(ctx context.Context) (codexprov.Inventory, error) {
	view, err := inventory.source.List(ctx)
	if err != nil {
		return codexprov.Inventory{}, err
	}
	accounts := make([]codexprov.LogicalAccount, 0, len(view.Accounts))
	for _, account := range view.Accounts {
		if inventory.allowed[account.Key] {
			accounts = append(accounts, account)
		}
	}
	view.Accounts = accounts
	return view, nil
}
