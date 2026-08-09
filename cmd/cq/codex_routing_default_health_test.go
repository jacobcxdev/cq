package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type staticCodexHealthInventory struct {
	inventory codexprov.Inventory
	err       error
}

func (i staticCodexHealthInventory) List(context.Context) (codexprov.Inventory, error) {
	return i.inventory, i.err
}

func TestCodexRoutingDefaultHealth(t *testing.T) {
	key := codexprov.AccountKey("opaque-routing-default")
	tests := []struct {
		name      string
		inventory codexprov.Inventory
		key       codexprov.AccountKey
		want      proxy.CodexRoutingDefaultHealth
	}{
		{
			name: "empty key is unconfigured",
			want: proxy.CodexRoutingDefaultHealth{Status: proxy.CodexRoutingDefaultStatusUnconfigured},
		},
		{
			name: "missing key is unresolved",
			key:  key,
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnresolved},
		},
		{
			name: "duplicate key is unresolved",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Routable: true},
				{Key: key},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnresolved},
		},
		{
			name: "duplicate key with one unstable row is unknown",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Routable: true},
				{Key: key, Unstable: true},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown},
		},
		{
			name: "duplicate key with both unstable rows is unknown",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Routable: true, Unstable: true},
				{Key: key, Unstable: true},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown},
		},
		{
			name: "stable unroutable key is unroutable",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Resolved: true, Status: proxy.CodexRoutingDefaultStatusUnroutable},
		},
		{
			name: "stable routable key is resolved",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Routable: true},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Resolved: true, Routable: true, Status: proxy.CodexRoutingDefaultStatusResolved},
		},
		{
			name: "unstable routable key is unknown",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Routable: true, Unstable: true},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown},
		},
		{
			name: "unstable unroutable key is unknown",
			key:  key,
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: key, Unstable: true},
			}},
			want: proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexRoutingDefaultHealth(tt.inventory, tt.key); got != tt.want {
				t.Fatalf("codexRoutingDefaultHealth() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCodexHealthTrackerRefreshesRoutingDefault(t *testing.T) {
	key := codexprov.AccountKey("opaque-routing-default")
	tracker := newCodexHealthTracker(staticCodexHealthInventory{inventory: codexprov.Inventory{
		Accounts: []codexprov.LogicalAccount{{Key: key, Routable: true}},
	}}, key, codexHealthFromInventory(codexprov.Inventory{}))

	health := tracker.Health(context.Background())
	want := proxy.CodexRoutingDefaultHealth{
		Configured: true,
		Resolved:   true,
		Routable:   true,
		Status:     proxy.CodexRoutingDefaultStatusResolved,
	}
	if health.RoutingDefault != want {
		t.Fatalf("routing default = %+v, want %+v", health.RoutingDefault, want)
	}
}

func TestCodexHealthTrackerListFailureDoesNotRetainRoutingDefaultClassification(t *testing.T) {
	key := codexprov.AccountKey("opaque-routing-default")
	last := codexHealthFromInventory(codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{}}})
	last.RoutingDefault = proxy.CodexRoutingDefaultHealth{
		Configured: true,
		Resolved:   true,
		Routable:   true,
		Status:     proxy.CodexRoutingDefaultStatusResolved,
	}
	tracker := newCodexHealthTracker(staticCodexHealthInventory{err: errors.New("unavailable")}, key, last)

	health := tracker.Health(context.Background())
	if health.AccountCount != last.AccountCount || !health.AccountCountKnown || health.HealthCode != "fetch_error" {
		t.Fatalf("failure health = %+v, want retained fields with fetch_error", health)
	}
	want := proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown}
	if health.RoutingDefault != want {
		t.Fatalf("routing default = %+v, want %+v", health.RoutingDefault, want)
	}

	emptyDefault := newCodexHealthTracker(staticCodexHealthInventory{err: errors.New("unavailable")}, "", last)
	if got := emptyDefault.Health(context.Background()).RoutingDefault; got != (proxy.CodexRoutingDefaultHealth{Status: proxy.CodexRoutingDefaultStatusUnconfigured}) {
		t.Fatalf("empty-key routing default = %+v, want unconfigured", got)
	}
}

func TestCodexRoutingDefaultHealthDoesNotExposeOpaqueIdentity(t *testing.T) {
	key := codexprov.AccountKey("opaque-key-secret")
	inventory := codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Key:      key,
		Routable: true,
		Identity: codexprov.AccountIdentity{Email: "private@example.test", AccountID: "account-id-secret"},
		Candidates: []codexprov.CredentialCandidate{{
			Ref: codexprov.CandidateRef{AccountKey: key, CandidateID: "candidate-id-secret"},
		}},
	}}}

	health := codexHealthFromInventory(inventory)
	health.RoutingDefault = codexRoutingDefaultHealth(inventory, key)
	encoded, err := json.Marshal(struct {
		RoutingDefault proxy.CodexRoutingDefaultHealth `json:"routing_default"`
	}{RoutingDefault: health.RoutingDefault})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"opaque-key-secret", "private@example.test", "account-id-secret", "candidate-id-secret"} {
		if strings.Contains(string(encoded), forbidden) || strings.Contains(health.RoutingDefault.Status, forbidden) {
			t.Fatalf("routing-default health exposed %q: %s", forbidden, encoded)
		}
	}
}
