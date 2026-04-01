package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

func TestAccountSelector_Select(t *testing.T) {
	now := time.Now().UnixMilli()
	future := now + 3600_000
	past := now - 3600_000

	tests := []struct {
		name      string
		accounts  []keyring.ClaudeOAuth
		exclude   []string
		wantEmail string
		wantErr   bool
	}{
		{
			name: "multiple accounts selects freshest non-expired",
			accounts: []keyring.ClaudeOAuth{
				{Email: "old@test.com", AccessToken: "t1", ExpiresAt: future},
				{Email: "new@test.com", AccessToken: "t2", ExpiresAt: future + 1000},
			},
			wantEmail: "new@test.com",
		},
		{
			name: "single account selected",
			accounts: []keyring.ClaudeOAuth{
				{Email: "only@test.com", AccessToken: "t1", ExpiresAt: future},
			},
			wantEmail: "only@test.com",
		},
		{
			name: "all expired but refreshable returns newest",
			accounts: []keyring.ClaudeOAuth{
				{Email: "older@test.com", AccessToken: "t1", ExpiresAt: past - 1000, RefreshToken: "r1"},
				{Email: "newer@test.com", AccessToken: "t2", ExpiresAt: past, RefreshToken: "r2"},
			},
			wantEmail: "newer@test.com",
		},
		{
			name: "all expired no refresh tokens returns error",
			accounts: []keyring.ClaudeOAuth{
				{Email: "dead@test.com", AccessToken: "t1", ExpiresAt: past},
			},
			wantErr: true,
		},
		{
			name:    "empty discovery returns error",
			wantErr: true,
		},
		{
			name: "exclude parameter skips account by email",
			accounts: []keyring.ClaudeOAuth{
				{Email: "skip@test.com", AccessToken: "t1", ExpiresAt: future},
				{Email: "keep@test.com", AccessToken: "t2", ExpiresAt: future},
			},
			exclude:   []string{"skip@test.com"},
			wantEmail: "keep@test.com",
		},
		{
			name: "exclude parameter skips account by UUID",
			accounts: []keyring.ClaudeOAuth{
				{Email: "a@test.com", AccountUUID: "uuid-1", AccessToken: "t1", ExpiresAt: future},
				{Email: "b@test.com", AccountUUID: "uuid-2", AccessToken: "t2", ExpiresAt: future},
			},
			exclude:   []string{"uuid-1"},
			wantEmail: "b@test.com",
		},
		{
			name: "prefers non-expired over expired-but-refreshable",
			accounts: []keyring.ClaudeOAuth{
				{Email: "expired@test.com", AccessToken: "t1", ExpiresAt: past, RefreshToken: "r1"},
				{Email: "fresh@test.com", AccessToken: "t2", ExpiresAt: future},
			},
			wantEmail: "fresh@test.com",
		},
		{
			name: "unknown expiry treated as non-expired",
			accounts: []keyring.ClaudeOAuth{
				{Email: "unknown@test.com", AccessToken: "t1", ExpiresAt: 0},
			},
			wantEmail: "unknown@test.com",
		},
		{
			name: "skips accounts with empty access token",
			accounts: []keyring.ClaudeOAuth{
				{Email: "empty@test.com", ExpiresAt: future},
				{Email: "valid@test.com", AccessToken: "t1", ExpiresAt: future},
			},
			wantEmail: "valid@test.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
				return tt.accounts
			}, nil)

			acct, err := sel.Select(context.Background(), tt.exclude...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if acct.Email != tt.wantEmail {
				t.Errorf("got email %q, want %q", acct.Email, tt.wantEmail)
			}
		})
	}
}

func TestAccountSelector_Select_ReturnsCopy(t *testing.T) {
	original := keyring.ClaudeOAuth{
		Email:       "test@test.com",
		AccessToken: "original",
		ExpiresAt:   time.Now().UnixMilli() + 3600_000,
	}
	sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{original}
	}, nil)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acct.AccessToken = "modified"

	acct2, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct2.AccessToken != "original" {
		t.Error("Select returned reference to internal state instead of copy")
	}
}

func TestAccountSelector_Select_PrefersActiveAccount(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000

	t.Run("active account preferred over fresher token", func(t *testing.T) {
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "fresh@test.com", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "active@test.com", AccessToken: "t2", ExpiresAt: future},
			}
		}, func() string { return "active@test.com" })

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "active@test.com" {
			t.Errorf("got %q, want active@test.com", acct.Email)
		}
	})

	t.Run("falls back to freshest when active is excluded", func(t *testing.T) {
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "fresh@test.com", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "active@test.com", AccessToken: "t2", ExpiresAt: future},
			}
		}, func() string { return "active@test.com" })

		acct, err := sel.Select(context.Background(), "active@test.com")
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "fresh@test.com" {
			t.Errorf("got %q, want fresh@test.com", acct.Email)
		}
	})

	t.Run("nil activeEmail behaves like before", func(t *testing.T) {
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "old@test.com", AccessToken: "t1", ExpiresAt: future},
				{Email: "new@test.com", AccessToken: "t2", ExpiresAt: future + 5000},
			}
		}, nil)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "new@test.com" {
			t.Errorf("got %q, want new@test.com (freshest)", acct.Email)
		}
	})

	t.Run("active account preferred even when expired but refreshable", func(t *testing.T) {
		past := time.Now().UnixMilli() - 3600_000
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth{
			return []keyring.ClaudeOAuth{
				{Email: "newer@test.com", AccessToken: "t1", ExpiresAt: past, RefreshToken: "r1"},
				{Email: "active@test.com", AccessToken: "t2", ExpiresAt: past - 1000, RefreshToken: "r2"},
			}
		}, func() string { return "active@test.com" })

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "active@test.com" {
			t.Errorf("got %q, want active@test.com", acct.Email)
		}
	})
}
