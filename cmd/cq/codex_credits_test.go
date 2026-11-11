package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexCreditsChangesOnlyResolvedLogicalAccount(t *testing.T) {
	policy := &codexprov.ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/policy.json"}
	var output bytes.Buffer
	deps := codexCreditsDependencies{
		Policy: policy, Out: &output,
		Inventory: func(context.Context) (codexprov.Inventory, error) {
			return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: "business", Identity: codexprov.AccountIdentity{AccountID: "business-workspace", UserID: "user", Email: "business@example.com"}},
				{Key: "personal", Identity: codexprov.AccountIdentity{AccountID: "personal-workspace", UserID: "user", Email: "personal@example.com"}},
			}}, nil
		},
		Aliases: func() (codexprov.AccountAliasIndex, error) { return codexprov.AccountAliasIndex{}, nil },
	}
	if err := runCodexCredits(context.Background(), "disable", "BUSINESS@example.com", true, deps); err != nil {
		t.Fatal(err)
	}
	var status codexCreditsStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.AccountKey != "business" || status.Enabled {
		t.Fatalf("status = %+v", status)
	}
	if allowed, err := policy.Allowed("personal"); err != nil || !allowed {
		t.Fatalf("personal changed: %v, %v", allowed, err)
	}
	output.Reset()
	if err := runCodexCredits(context.Background(), "status", "business", false, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "disabled") {
		t.Fatalf("output = %q", output.String())
	}
	if err := runCodexCredits(context.Background(), "enable", "business", false, deps); err != nil {
		t.Fatal(err)
	}
	if allowed, err := policy.Allowed("business"); err != nil || !allowed {
		t.Fatalf("enable = %v, %v", allowed, err)
	}
}

func TestCodexCreditsRejectsAmbiguousAndFailedInventory(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &codexprov.ExtraCreditsPolicy{FS: fs, Path: "/state/policy.json"}
	deps := codexCreditsDependencies{Policy: policy, Out: new(bytes.Buffer),
		Aliases: func() (codexprov.AccountAliasIndex, error) { return codexprov.AccountAliasIndex{}, nil },
		Inventory: func(context.Context) (codexprov.Inventory, error) {
			return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				{Key: "one", Identity: codexprov.AccountIdentity{Email: "same@example.com"}},
				{Key: "two", Identity: codexprov.AccountIdentity{Email: "same@example.com"}},
			}}, nil
		},
	}
	if err := runCodexCredits(context.Background(), "disable", "same@example.com", false, deps); err == nil {
		t.Fatal("accepted ambiguous reference")
	}
	if restricted, err := policy.Restricted(); err != nil || restricted {
		t.Fatalf("changed state: %v, %v", restricted, err)
	}
	deps.Inventory = func(context.Context) (codexprov.Inventory, error) {
		return codexprov.Inventory{}, errors.New("unavailable")
	}
	if err := runCodexCredits(context.Background(), "enable", "one", false, deps); err == nil {
		t.Fatal("accepted unavailable inventory")
	}
}

func TestCodexCreditsCommandHelpAndSelectors(t *testing.T) {
	for _, action := range []string{"enable", "disable", "status"} {
		var cli CLI
		parser, err := kong.New(&cli, kong.Writers(new(bytes.Buffer), new(bytes.Buffer)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := parser.Parse([]string{"codex", "credits", action, "business@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Command() != "codex credits "+action+" <account-reference>" {
			t.Fatalf("command = %q", ctx.Command())
		}
	}
}

func TestCodexCreditsDisabledPermissionSurvivesLogicalKeyChange(t *testing.T) {
	policy := &codexprov.ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/policy.json"}
	key := codexprov.AccountKey("gen:external")
	identity := codexprov.AccountIdentity{AccountID: "workspace", UserID: "user", Email: "business@example.com"}
	var output bytes.Buffer
	deps := codexCreditsDependencies{Policy: policy, Out: &output,
		Inventory: func(context.Context) (codexprov.Inventory, error) {
			return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: key, Identity: identity}}}, nil
		},
		Aliases: func() (codexprov.AccountAliasIndex, error) { return codexprov.AccountAliasIndex{}, nil },
	}
	if err := runCodexCredits(context.Background(), "disable", identity.Email, true, deps); err != nil {
		t.Fatal(err)
	}
	key = "acct:managed"
	output.Reset()
	if err := runCodexCredits(context.Background(), "status", identity.Email, true, deps); err != nil {
		t.Fatal(err)
	}
	var status codexCreditsStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.AccountKey != key {
		t.Fatalf("adoption lost policy: %+v", status)
	}
	if err := runCodexCredits(context.Background(), "enable", string(key), false, deps); err != nil {
		t.Fatal(err)
	}
	canonical, err := codexprov.ExtraCreditsIdentityKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := policy.Allowed(canonical); err != nil || !allowed {
		t.Fatalf("enable did not clear canonical policy: %v, %v", allowed, err)
	}
}

func TestCodexCreditsSupportsCompleteSystemOnlyIdentity(t *testing.T) {
	fs := fsutil.NewMemFS()
	identity := codexprov.AccountIdentity{AccountID: "system-workspace", UserID: "system-user", Email: "system@example.com", PlanType: "pro"}
	document, err := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token": "synthetic-token", "account_id": identity.AccountID, "id_token": registryCredentialJWT(identity),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.MkdirAll("/home/test/.codex", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/home/test/.codex/auth.json", document, 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := codexprov.DiscoverInventory(fs)
	if len(inventory.Accounts) != 1 || !inventory.Accounts[0].Unstable || len(inventory.Accounts[0].Candidates) != 1 || inventory.Accounts[0].Candidates[0].Source != codexprov.SourceSystem {
		t.Fatalf("fixture is not a system-only generated identity: %+v", inventory.Accounts)
	}
	if _, err := codexprov.ResolveAccountReference(inventory, codexprov.AccountAliasIndex{}, identity.Email); err == nil {
		t.Fatal("generic resolver unexpectedly accepts unstable inventory")
	}
	policy := &codexprov.ExtraCreditsPolicy{FS: fs, Path: "/state/policy.json"}
	canonical, err := codexprov.ExtraCreditsIdentityKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	deps := codexCreditsDependencies{Policy: policy, Out: &output,
		Inventory: func(context.Context) (codexprov.Inventory, error) { return inventory, nil },
		Aliases:   func() (codexprov.AccountAliasIndex, error) { return codexprov.AccountAliasIndex{}, nil },
	}
	for _, action := range []string{"disable", "status", "enable", "status"} {
		output.Reset()
		if err := runCodexCredits(context.Background(), action, identity.Email, true, deps); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		var status codexCreditsStatus
		if err := json.Unmarshal(output.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		allowed, err := policy.Allowed(canonical)
		if err != nil || status.Enabled != allowed || status.AccountKey != inventory.Accounts[0].Key {
			t.Fatalf("%s status = %+v, allowed=%v, err=%v", action, status, allowed, err)
		}
		if action != "status" && allowed != (action == "enable") {
			t.Fatalf("%s allowed=%v", action, allowed)
		}
	}
	if !inventory.Accounts[0].Unstable {
		t.Fatal("mutated shared inventory")
	}
}

func TestCodexCreditsRejectsWeakAndAmbiguousSystemIdentity(t *testing.T) {
	for _, accounts := range [][]codexprov.LogicalAccount{
		{{Key: "gen:weak", Unstable: true, Identity: codexprov.AccountIdentity{AccountID: "workspace", Email: "system@example.com"}}},
		{{Key: "gen:first", Unstable: true, Identity: codexprov.AccountIdentity{AccountID: "first", UserID: "user", Email: "system@example.com"}}, {Key: "gen:second", Unstable: true, Identity: codexprov.AccountIdentity{AccountID: "second", UserID: "user", Email: "system@example.com"}}},
	} {
		policy := &codexprov.ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/policy.json"}
		deps := codexCreditsDependencies{Policy: policy, Out: new(bytes.Buffer),
			Inventory: func(context.Context) (codexprov.Inventory, error) {
				return codexprov.Inventory{Accounts: accounts}, nil
			},
			Aliases: func() (codexprov.AccountAliasIndex, error) { return codexprov.AccountAliasIndex{}, nil },
		}
		for _, action := range []string{"disable", "enable", "status"} {
			if err := runCodexCredits(context.Background(), action, "system@example.com", false, deps); err == nil {
				t.Fatalf("%s accepted weak/ambiguous account", action)
			}
		}
		if restricted, err := policy.Restricted(); err != nil || restricted {
			t.Fatalf("policy changed: %v, %v", restricted, err)
		}
	}
}
