package proxy

import (
	"context"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexSelector_PrefersActive(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "inactive@test.com", AccessToken: "tok-1", IsActive: false},
			{Email: "active@test.com", AccessToken: "tok-2", IsActive: true, AccountID: "acct-2"},
		}
	})

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "active@test.com" {
		t.Errorf("email = %q, want active@test.com", acct.Email)
	}
	if acct.AccessToken != "tok-2" {
		t.Errorf("token = %q, want tok-2", acct.AccessToken)
	}
}

func TestCodexSelector_FallsBackToFirstWithToken(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "", IsActive: false},
			{Email: "b@test.com", AccessToken: "tok-b", IsActive: false},
		}
	})

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_NoAccounts(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return nil
	})

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for no accounts")
	}
}

func TestCodexSelector_NoValidTokens(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "", IsActive: true},
		}
	})

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for no valid tokens")
	}
}

func TestCodexSelector_ReturnsCopy(t *testing.T) {
	accounts := []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok", IsActive: true},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return accounts
	})

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Mutate returned account — should not affect source.
	acct.AccessToken = "mutated"
	if accounts[0].AccessToken == "mutated" {
		t.Error("selector returned reference instead of copy")
	}
}

func TestCodexSelector_ExcludeByEmail(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", IsActive: false},
		}
	})

	acct, err := sel.Select(context.Background(), "a@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeByAccountID(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2", IsActive: false},
		}
	})

	acct, err := sel.Select(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeByRecordKey(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", RecordKey: "uid1::acct1", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", RecordKey: "uid2::acct2", IsActive: false},
		}
	})

	acct, err := sel.Select(context.Background(), "uid1::acct1")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeAll(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", IsActive: true},
		}
	})

	_, err := sel.Select(context.Background(), "a@test.com")
	if err == nil {
		t.Fatal("expected error when all accounts are excluded")
	}
}
