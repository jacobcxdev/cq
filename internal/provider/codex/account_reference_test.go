package codex

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestRegistryAccountAliasIndexResolvesValidRowsReadOnly(t *testing.T) {
	fs := newFakeFS()
	registry := Registry{FS: fs, Home: "/fake/home"}
	registryJSON := []byte(`{
  "schema_version": 3,
  "active_account_key": "acct-system",
  "accounts": [
    {"account_key": "acct-work", "alias": "  Work  "},
    {"account_key": "acct-work", "alias": "WORK"},
    {"account_key": "acct-orphan", "alias": "orphan"},
    {"account_key": " acct-work ", "alias": "padded-key"},
    {"account_key": "", "alias": "empty-key"},
    {"account_key": "acct-work", "alias": "   "},
    {"account_key": 7, "alias": "number-key"},
    {"account_key": "acct-work", "alias": false},
    {"alias": "missing-key"},
    {"account_key": "acct-work"},
    "not-an-account"
  ]
}`)
	fs.files[registry.path()] = append([]byte(nil), registryJSON...)
	inventory := Inventory{Accounts: []LogicalAccount{{
		Key:      "acct-work",
		Identity: AccountIdentity{Email: "person@example.com"},
		Routable: true,
	}}}
	inventoryBefore := cloneReferenceTestInventory(inventory)

	aliases, err := registry.AccountAliasIndex()
	if err != nil {
		t.Fatalf("AccountAliasIndex() error = %v", err)
	}
	got, err := ResolveAccountReference(inventory, aliases, " work ")
	if err != nil {
		t.Fatalf("ResolveAccountReference(valid alias) error = %v", err)
	}
	if got != "acct-work" {
		t.Fatalf("ResolveAccountReference(valid alias) = %q, want %q", got, AccountKey("acct-work"))
	}

	_, err = ResolveAccountReference(inventory, aliases, "orphan")
	requireAccountReferenceError(t, err, AccountReferenceMissing)
	_, err = ResolveAccountReference(inventory, aliases, "padded-key")
	requireAccountReferenceError(t, err, AccountReferenceMissing)
	if !bytes.Equal(fs.files[registry.path()], registryJSON) {
		t.Fatal("alias lookup mutated registry.json")
	}
	if !reflect.DeepEqual(inventory, inventoryBefore) {
		t.Fatal("alias resolution mutated or re-keyed inventory")
	}
}

func TestRegistryAccountAliasIndexMissingRegistryIsEmpty(t *testing.T) {
	registry := Registry{FS: newFakeFS(), Home: "/fake/home"}
	aliases, err := registry.AccountAliasIndex()
	if err != nil {
		t.Fatalf("AccountAliasIndex() error = %v", err)
	}

	_, err = ResolveAccountReference(
		Inventory{Accounts: []LogicalAccount{{Key: "acct-work"}}},
		aliases,
		"work",
	)
	requireAccountReferenceError(t, err, AccountReferenceMissing)
}

func TestRegistryAccountAliasIndexReturnsRegistryParseError(t *testing.T) {
	fs := newFakeFS()
	registry := Registry{FS: fs, Home: "/fake/home"}
	fs.files[registry.path()] = []byte(`{"accounts":`)

	_, err := registry.AccountAliasIndex()
	if err == nil || err.Error() != "parse registry: invalid JSON" {
		t.Fatalf("AccountAliasIndex() error = %v, want registry parse error", err)
	}
}

func TestResolveAccountReferenceExactStableKeyPrecedesMetadata(t *testing.T) {
	inventory := Inventory{Accounts: []LogicalAccount{
		{Key: "acct-primary", Identity: AccountIdentity{Email: "acct-collision"}},
		{Key: "acct-collision", Identity: AccountIdentity{Email: "other@example.com"}},
	}}
	aliases := AccountAliasIndex{rows: []accountAliasRow{{
		Alias:      "acct-collision",
		AccountKey: "acct-primary",
	}}}

	got, err := ResolveAccountReference(inventory, aliases, "acct-collision")
	if err != nil {
		t.Fatalf("ResolveAccountReference() error = %v", err)
	}
	if got != "acct-collision" {
		t.Fatalf("ResolveAccountReference() = %q, want exact key %q", got, AccountKey("acct-collision"))
	}
}

func TestResolveAccountReferenceUniqueEmailAndAlias(t *testing.T) {
	inventory := Inventory{Accounts: []LogicalAccount{
		{Key: "acct-email", Identity: AccountIdentity{Email: "Person@Example.com"}, Routable: true},
		{Key: "acct-alias", Identity: AccountIdentity{Email: "other@example.com"}},
	}}
	aliases := AccountAliasIndex{rows: []accountAliasRow{{
		Alias:      "My Work Account",
		AccountKey: "acct-alias",
	}, {
		Alias:      "person@example.com",
		AccountKey: "acct-email",
	}}}

	tests := []struct {
		name      string
		reference string
		want      AccountKey
	}{
		{name: "email", reference: "  person@example.COM\t", want: "acct-email"},
		{name: "alias", reference: " my work ACCOUNT ", want: "acct-alias"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAccountReference(inventory, aliases, tt.reference)
			if err != nil {
				t.Fatalf("ResolveAccountReference() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveAccountReference() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveAccountReferenceDoesNotNormalizeOpaqueKey(t *testing.T) {
	inventory := Inventory{Accounts: []LogicalAccount{{Key: "acct-exact"}}}

	_, err := ResolveAccountReference(inventory, AccountAliasIndex{}, " acct-exact ")
	requireAccountReferenceError(t, err, AccountReferenceMissing)
}

func TestResolveAccountReferenceTypedFailures(t *testing.T) {
	tests := []struct {
		name      string
		inventory Inventory
		aliases   AccountAliasIndex
		reference string
		wantCode  AccountReferenceErrorCode
	}{
		{
			name:      "empty",
			reference: " \t\n ",
			wantCode:  AccountReferenceEmpty,
		},
		{
			name: "missing",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key:      "acct-one",
				Identity: AccountIdentity{Email: "one@example.com"},
			}}},
			reference: "private-missing@example.com",
			wantCode:  AccountReferenceMissing,
		},
		{
			name: "duplicate email",
			inventory: Inventory{Accounts: []LogicalAccount{
				{Key: "acct-one", Identity: AccountIdentity{Email: "same@example.com"}},
				{Key: "acct-two", Identity: AccountIdentity{Email: "SAME@example.com"}},
			}},
			reference: "same@example.com",
			wantCode:  AccountReferenceAmbiguous,
		},
		{
			name: "duplicate alias",
			inventory: Inventory{Accounts: []LogicalAccount{
				{Key: "acct-one"},
				{Key: "acct-two"},
			}},
			aliases: AccountAliasIndex{rows: []accountAliasRow{
				{Alias: "work", AccountKey: "acct-one"},
				{Alias: "WORK", AccountKey: "acct-two"},
			}},
			reference: "work",
			wantCode:  AccountReferenceAmbiguous,
		},
		{
			name: "email alias collision",
			inventory: Inventory{Accounts: []LogicalAccount{
				{Key: "acct-email", Identity: AccountIdentity{Email: "shared reference"}},
				{Key: "acct-alias"},
			}},
			aliases: AccountAliasIndex{rows: []accountAliasRow{{
				Alias:      "shared reference",
				AccountKey: "acct-alias",
			}}},
			reference: "shared reference",
			wantCode:  AccountReferenceAmbiguous,
		},
		{
			name: "duplicate exact key",
			inventory: Inventory{Accounts: []LogicalAccount{
				{Key: "acct-duplicate"},
				{Key: "acct-duplicate"},
			}},
			reference: "acct-duplicate",
			wantCode:  AccountReferenceAmbiguous,
		},
		{
			name: "unstable exact key",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key:      "acct-unstable",
				Unstable: true,
			}}},
			reference: "acct-unstable",
			wantCode:  AccountReferenceUnstable,
		},
		{
			name: "unstable email",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key:      "acct-unstable",
				Identity: AccountIdentity{Email: "unstable@example.com"},
				Unstable: true,
			}}},
			reference: "unstable@example.com",
			wantCode:  AccountReferenceUnstable,
		},
		{
			name: "unstable alias",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key:      "acct-unstable",
				Unstable: true,
			}}},
			aliases: AccountAliasIndex{rows: []accountAliasRow{{
				Alias:      "private alias",
				AccountKey: "acct-unstable",
			}}},
			reference: "private alias",
			wantCode:  AccountReferenceUnstable,
		},
		{
			name: "ambiguous before unstable",
			inventory: Inventory{Accounts: []LogicalAccount{
				{Key: "acct-stable", Identity: AccountIdentity{Email: "shared@example.com"}},
				{Key: "acct-unstable", Identity: AccountIdentity{Email: "shared@example.com"}, Unstable: true},
			}},
			reference: "shared@example.com",
			wantCode:  AccountReferenceAmbiguous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventoryBefore := cloneReferenceTestInventory(tt.inventory)
			aliasesBefore := append([]accountAliasRow(nil), tt.aliases.rows...)
			_, err := ResolveAccountReference(tt.inventory, tt.aliases, tt.reference)
			referenceErr := requireAccountReferenceError(t, err, tt.wantCode)
			if tt.reference != "" && bytes.Contains([]byte(referenceErr.Error()), []byte(tt.reference)) {
				t.Fatalf("error exposed account reference %q: %v", tt.reference, referenceErr)
			}
			if !reflect.DeepEqual(tt.inventory, inventoryBefore) {
				t.Fatal("failed resolution mutated or re-keyed inventory")
			}
			if !reflect.DeepEqual(tt.aliases.rows, aliasesBefore) {
				t.Fatal("failed resolution mutated alias index")
			}
		})
	}
}

func TestResolveAccountReferenceDoesNotRequireRoutability(t *testing.T) {
	inventory := Inventory{Accounts: []LogicalAccount{{
		Key:      "acct-unroutable",
		Identity: AccountIdentity{Email: "unroutable@example.com"},
		Routable: false,
	}}}

	got, err := ResolveAccountReference(inventory, AccountAliasIndex{}, "unroutable@example.com")
	if err != nil {
		t.Fatalf("ResolveAccountReference() error = %v", err)
	}
	if got != "acct-unroutable" {
		t.Fatalf("ResolveAccountReference() = %q, want %q", got, AccountKey("acct-unroutable"))
	}
}

func TestAccountKeyState(t *testing.T) {
	inventory := Inventory{Accounts: []LogicalAccount{
		{Key: "acct-routable", Routable: true},
		{Key: "acct-unroutable"},
		{Key: "acct-unstable", Routable: true, Unstable: true},
		{Key: "acct-duplicate", Routable: true},
		{Key: "acct-duplicate"},
	}}
	tests := []struct {
		name     string
		key      AccountKey
		resolved bool
		routable bool
		unstable bool
	}{
		{name: "routable", key: "acct-routable", resolved: true, routable: true},
		{name: "unroutable", key: "acct-unroutable", resolved: true},
		{name: "unstable", key: "acct-unstable", resolved: true, routable: true, unstable: true},
		{name: "missing", key: "acct-missing"},
		{name: "empty"},
		{name: "duplicate", key: "acct-duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, routable, unstable := AccountKeyState(inventory, tt.key)
			if resolved != tt.resolved || routable != tt.routable || unstable != tt.unstable {
				t.Fatalf(
					"AccountKeyState() = (%t, %t, %t), want (%t, %t, %t)",
					resolved, routable, unstable,
					tt.resolved, tt.routable, tt.unstable,
				)
			}
		})
	}
}

func requireAccountReferenceError(
	t *testing.T,
	err error,
	wantCode AccountReferenceErrorCode,
) *AccountReferenceError {
	t.Helper()
	var referenceErr *AccountReferenceError
	if !errors.As(err, &referenceErr) {
		t.Fatalf("error = %v, want *AccountReferenceError", err)
	}
	if referenceErr.Code != wantCode {
		t.Fatalf("error code = %q, want %q", referenceErr.Code, wantCode)
	}
	return referenceErr
}

func cloneReferenceTestInventory(inventory Inventory) Inventory {
	clone := inventory
	clone.Accounts = append([]LogicalAccount(nil), inventory.Accounts...)
	for i := range clone.Accounts {
		clone.Accounts[i].Candidates = append(
			[]CredentialCandidate(nil),
			inventory.Accounts[i].Candidates...,
		)
	}
	clone.Intents = append([]InventoryIntent(nil), inventory.Intents...)
	clone.ExternalSources = append([]ExternalSourceStatus(nil), inventory.ExternalSources...)
	return clone
}
