package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

type stubQuotaReader map[string]QuotaSnapshot

func (s stubQuotaReader) Snapshot(identifier string) (QuotaSnapshot, bool) {
	snap, ok := s[identifier]
	return snap, ok
}

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
			name: "exclude parameter skips anonymous account by access token",
			accounts: []keyring.ClaudeOAuth{
				{AccessToken: "anon-token", ExpiresAt: future},
				{Email: "named@test.com", AccountUUID: "uuid-named", AccessToken: "named-token", ExpiresAt: future},
			},
			exclude:   []string{"anon-token"},
			wantEmail: "named@test.com",
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
			}, nil, nil)

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
	}, nil, nil)

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
		}, func() string { return "active@test.com" }, nil)

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
		}, func() string { return "active@test.com" }, nil)

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
		}, nil, nil)

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
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "newer@test.com", AccessToken: "t1", ExpiresAt: past, RefreshToken: "r1"},
				{Email: "active@test.com", AccessToken: "t2", ExpiresAt: past - 1000, RefreshToken: "r2"},
			}
		}, func() string { return "active@test.com" }, nil)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "active@test.com" {
			t.Errorf("got %q, want active@test.com", acct.Email)
		}
	})
}

func TestAccountSelector_Select_QuotaAware(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000

	buildQuotaReader := func(snaps map[string]QuotaSnapshot) QuotaReader {
		return stubQuotaReader(snaps)
	}

	t.Run("prefers account with higher remaining quota", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-low":  {Result: quotaResult("uuid-low", "low@test.com", 10)},
			"uuid-high": {Result: quotaResult("uuid-high", "high@test.com", 80)},
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "low@test.com", AccountUUID: "uuid-low", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "high@test.com", AccountUUID: "uuid-high", AccessToken: "t2", ExpiresAt: future},
			}
		}, nil, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "high@test.com" {
			t.Errorf("got %q, want high@test.com (higher quota)", acct.Email)
		}
	})

	t.Run("quota tiebreak uses active preference", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-a": {Result: quotaResult("uuid-a", "a@test.com", 50)},
			"uuid-b": {Result: quotaResult("uuid-b", "b@test.com", 50)},
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "t2", ExpiresAt: future},
			}
		}, func() string { return "b@test.com" }, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "b@test.com" {
			t.Errorf("got %q, want b@test.com (active, tied quota)", acct.Email)
		}
	})

	t.Run("quota tiebreak uses latest expiry when no active", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-a": {Result: quotaResult("uuid-a", "a@test.com", 50)},
			"uuid-b": {Result: quotaResult("uuid-b", "b@test.com", 50)},
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "t1", ExpiresAt: future},
				{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "t2", ExpiresAt: future + 5000},
			}
		}, nil, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "b@test.com" {
			t.Errorf("got %q, want b@test.com (later expiry, tied quota)", acct.Email)
		}
	})

	t.Run("known quota beats unknown", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-known": {Result: quotaResult("uuid-known", "known@test.com", 30)},
			// uuid-unknown has no snapshot
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "unknown@test.com", AccountUUID: "uuid-unknown", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "known@test.com", AccountUUID: "uuid-known", AccessToken: "t2", ExpiresAt: future},
			}
		}, nil, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "known@test.com" {
			t.Errorf("got %q, want known@test.com (known state)", acct.Email)
		}
	})

	t.Run("unknown quota beats known exhausted", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-exhausted": {Result: quotaResult("uuid-exhausted", "exhausted@test.com", 0)},
			// uuid-unknown has no snapshot
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "unknown@test.com", AccountUUID: "uuid-unknown", AccessToken: "t1", ExpiresAt: future},
				{Email: "exhausted@test.com", AccountUUID: "uuid-exhausted", AccessToken: "t2", ExpiresAt: future + 5000},
			}
		}, nil, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "unknown@test.com" {
			t.Errorf("got %q, want unknown@test.com (unknown beats confirmed exhausted)", acct.Email)
		}
	})

	t.Run("nil quota falls back to existing logic", func(t *testing.T) {
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "old@test.com", AccessToken: "t1", ExpiresAt: future},
				{Email: "new@test.com", AccessToken: "t2", ExpiresAt: future + 5000},
			}
		}, nil, nil)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "new@test.com" {
			t.Errorf("got %q, want new@test.com (latest expiry fallback)", acct.Email)
		}
	})

	t.Run("exhausted account deprioritised", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-dead": {Result: quotaResult("uuid-dead", "dead@test.com", 0)},
			"uuid-live": {Result: quotaResult("uuid-live", "live@test.com", 45)},
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "dead@test.com", AccountUUID: "uuid-dead", AccessToken: "t1", ExpiresAt: future + 5000},
				{Email: "live@test.com", AccountUUID: "uuid-live", AccessToken: "t2", ExpiresAt: future},
			}
		}, func() string { return "dead@test.com" }, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "live@test.com" {
			t.Errorf("got %q, want live@test.com (dead account at 0%%)", acct.Email)
		}
	})

	t.Run("quota overrides active preference", func(t *testing.T) {
		quotaReader := buildQuotaReader(map[string]QuotaSnapshot{
			"uuid-active": {Result: quotaResult("uuid-active", "active@test.com", 5)},
			"uuid-other":  {Result: quotaResult("uuid-other", "other@test.com", 80)},
		})
		sel := NewAccountSelector(func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{
				{Email: "active@test.com", AccountUUID: "uuid-active", AccessToken: "t1", ExpiresAt: future},
				{Email: "other@test.com", AccountUUID: "uuid-other", AccessToken: "t2", ExpiresAt: future},
			}
		}, func() string { return "active@test.com" }, quotaReader)

		acct, err := sel.Select(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if acct.Email != "other@test.com" {
			t.Errorf("got %q, want other@test.com (80%% vs 5%%)", acct.Email)
		}
	})
}

func quotaResult(id, email string, remainingPct int) quota.Result {
	return quota.Result{
		AccountID: id,
		Email:     email,
		Status:    quota.StatusOK,
		Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: remainingPct},
		},
	}
}
