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

func TestProxyCodexRoutingInventoriesPinNewWorkAndPreserveContinuityAccounts(t *testing.T) {
	source := &staticProxyCodexRoutingInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		{Key: "account-a"},
		{Key: "account-b"},
		{Key: "account-c"},
	}}}
	selection, continuity := newProxyCodexRoutingInventories(
		source,
		[]codexprov.AccountKey{"account-a", "account-b"},
		"account-c",
	)

	selected, err := selection.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Accounts) != 1 || selected.Accounts[0].Key != "account-c" {
		t.Fatalf("selection accounts = %#v, want pinned account only", selected.Accounts)
	}
	retained, err := continuity.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.Accounts) != 3 || retained.Accounts[0].Key != "account-a" || retained.Accounts[1].Key != "account-b" || retained.Accounts[2].Key != "account-c" {
		t.Fatalf("continuity accounts = %#v, want allowlist plus pin", retained.Accounts)
	}
	if len(source.inventory.Accounts) != 3 {
		t.Fatal("routing views mutated source inventory")
	}
}

func TestProxyCodexRoutingInventoriesPreserveAllContinuityAccountsWithoutAllowlist(t *testing.T) {
	source := &staticProxyCodexRoutingInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		{Key: "account-a"},
		{Key: "account-c"},
	}}}
	selection, continuity := newProxyCodexRoutingInventories(source, nil, "account-c")

	selected, err := selection.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Accounts) != 1 || selected.Accounts[0].Key != "account-c" {
		t.Fatalf("selection accounts = %#v, want pinned account only", selected.Accounts)
	}
	retained, err := continuity.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.Accounts) != 2 || retained.Accounts[0].Key != "account-a" || retained.Accounts[1].Key != "account-c" {
		t.Fatalf("continuity accounts = %#v, want complete source inventory", retained.Accounts)
	}
}

type staticProxyCodexRoutingInventory struct {
	inventory codexprov.Inventory
}

func (inventory *staticProxyCodexRoutingInventory) List(context.Context) (codexprov.Inventory, error) {
	return inventory.inventory, nil
}
