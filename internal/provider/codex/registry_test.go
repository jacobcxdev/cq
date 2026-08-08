package codex

import (
	"encoding/json"
	"testing"
)

func TestRegistryUpsertPreservesActiveAndUnknownFields(t *testing.T) {
	fs := newFakeFS()
	path := "/fake/home/.codex/accounts/registry.json"
	fs.files[path] = []byte(`{
		"schema_version": 3,
		"active_account_key": "existing::active",
		"unknown_top": {"keep": true},
		"accounts": [{
			"account_key": "user-1::acct-1",
			"email": "old@test.com",
			"alias": "work",
			"unknown_record": 42,
			"created_at": 7
		}]
	}`)

	registry := Registry{FS: fs, Home: "/fake/home"}
	if err := registry.UpsertAccount(RegistryAccount{
		AccountKey: "user-1::acct-1", AccountID: "acct-1", UserID: "user-1",
		Email: "new@test.com", Plan: "pro", CreatedAt: 99,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(fs.files[path], &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["active_account_key"]; got != "existing::active" {
		t.Fatalf("active_account_key = %#v, want preserved", got)
	}
	if _, ok := doc["unknown_top"]; !ok {
		t.Fatal("unknown top-level field was lost")
	}
	record := doc["accounts"].([]any)[0].(map[string]any)
	if record["unknown_record"] != float64(42) || record["alias"] != "work" || record["created_at"] != float64(7) {
		t.Fatalf("unknown/stable record fields changed: %#v", record)
	}
	if record["email"] != "new@test.com" || record["plan"] != "pro" {
		t.Fatalf("known fields not updated: %#v", record)
	}
}

func TestRegistryProjectActiveChangesOnlyProjection(t *testing.T) {
	fs := newFakeFS()
	path := "/fake/home/.codex/accounts/registry.json"
	beforeAccounts := []any{map[string]any{"account_key": "user-1::acct-1", "custom": true}}
	data, _ := json.Marshal(map[string]any{"schema_version": 3, "accounts": beforeAccounts, "custom": "keep"})
	fs.files[path] = data

	registry := Registry{FS: fs, Home: "/fake/home"}
	if err := registry.ProjectActive("user-1::acct-1"); err != nil {
		t.Fatalf("ProjectActive: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(fs.files[path], &doc); err != nil {
		t.Fatal(err)
	}
	if doc["active_account_key"] != "user-1::acct-1" || doc["custom"] != "keep" {
		t.Fatalf("projection changed unrelated state: %#v", doc)
	}
}
