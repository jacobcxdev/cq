package claude

import (
	"context"
	"errors"
	"testing"

	"github.com/jacobcxdev/cq/internal/keyring"
)

func TestAccountsRemove(t *testing.T) {
	t.Run("removes matching Claude storage", func(t *testing.T) {
		oldDiscover := discoverClaudeAccounts
		oldRemoveCQ := removeCQClaudeAccountsByEmail
		oldRemoveActive := removeActiveClaudeCredentialsByEmail
		oldRemovePlatform := removePlatformClaudeKeychainAccountsByEmail
		defer func() {
			discoverClaudeAccounts = oldDiscover
			removeCQClaudeAccountsByEmail = oldRemoveCQ
			removeActiveClaudeCredentialsByEmail = oldRemoveActive
			removePlatformClaudeKeychainAccountsByEmail = oldRemovePlatform
		}()

		discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "user@example.com", AccountUUID: "uuid-1"},
				{Email: "user@example.com", AccountUUID: "uuid-2"},
				{Email: "other@example.com", AccountUUID: "uuid-3"},
			}
		}

		var calls []string
		removeCQClaudeAccountsByEmail = func(email string) error {
			calls = append(calls, "cq:"+email)
			return nil
		}
		removeActiveClaudeCredentialsByEmail = func(email string) error {
			calls = append(calls, "active:"+email)
			return nil
		}
		removePlatformClaudeKeychainAccountsByEmail = func(email string) error {
			calls = append(calls, "platform:"+email)
			return nil
		}

		mgr := &Accounts{}
		if err := mgr.Remove(context.Background(), "user@example.com"); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		want := []string{"cq:user@example.com", "active:user@example.com", "platform:user@example.com"}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("calls[%d] = %q, want %q (all helpers should run once)", i, calls[i], want[i])
			}
		}
	})

	t.Run("returns clear error for unknown email", func(t *testing.T) {
		oldDiscover := discoverClaudeAccounts
		defer func() { discoverClaudeAccounts = oldDiscover }()

		discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "other@example.com", AccountUUID: "uuid-1"}}
		}

		mgr := &Accounts{}
		err := mgr.Remove(context.Background(), "user@example.com")
		if err == nil {
			t.Fatal("expected error for unknown email")
		}
		if got, want := err.Error(), `no account found with email "user@example.com"`; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	})

	t.Run("returns helper error", func(t *testing.T) {
		oldDiscover := discoverClaudeAccounts
		oldRemoveCQ := removeCQClaudeAccountsByEmail
		defer func() {
			discoverClaudeAccounts = oldDiscover
			removeCQClaudeAccountsByEmail = oldRemoveCQ
		}()

		discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "user@example.com", AccountUUID: "uuid-1"}}
		}
		removeCQClaudeAccountsByEmail = func(email string) error {
			return errors.New("boom")
		}

		mgr := &Accounts{}
		err := mgr.Remove(context.Background(), "user@example.com")
		if err == nil {
			t.Fatal("expected helper error")
		}
		if got, want := err.Error(), "remove cq-managed Claude accounts: boom"; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	})
}
