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
