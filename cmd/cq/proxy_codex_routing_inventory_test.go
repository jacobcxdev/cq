package main

import (
	"context"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestProxyCodexRoutingInventoryAllowsOnlyConfiguredAccounts(t *testing.T) {
	source := &staticProxyCodexRoutingInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		{Key: "account-a"},
		{Key: "account-b"},
		{Key: "account-c"},
	}}}
	inventory := newProxyCodexRoutingInventory(source, []codexprov.AccountKey{"account-a", "account-b"})

	got, err := inventory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].Key != "account-a" || got.Accounts[1].Key != "account-b" {
		t.Fatalf("filtered accounts = %#v", got.Accounts)
	}
	if len(source.inventory.Accounts) != 3 {
		t.Fatal("filter mutated source inventory")
	}
}

func TestProxyCodexRoutingInventoryPreservesAllAccountsWithoutConfiguration(t *testing.T) {
	source := &staticProxyCodexRoutingInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "account-a"}, {Key: "account-b"}}}}
	got, err := newProxyCodexRoutingInventory(source, nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(got.Accounts))
	}
}

type staticProxyCodexRoutingInventory struct {
	inventory codexprov.Inventory
}

func (inventory *staticProxyCodexRoutingInventory) List(context.Context) (codexprov.Inventory, error) {
	return inventory.inventory, nil
}
