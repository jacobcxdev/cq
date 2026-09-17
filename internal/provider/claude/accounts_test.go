package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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

		want := []string{"platform:user@example.com", "cq:user@example.com", "active:user@example.com"}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("calls[%d] = %q, want %q (all helpers should run once)", i, calls[i], want[i])
			}
		}
	})

	// Regression: platform removal must run BEFORE cq/active cleanup so that
	// keyring.DiscoverClaudeAccounts (called inside the platform helper) can
	// still pre-seed anonymous-entry token affinity from the cq keyring and
	// credentials file, which are deleted in the later two steps.
	t.Run("platform removal runs before cq and active cleanup", func(t *testing.T) {
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
			return []keyring.ClaudeOAuth{{Email: "user@example.com", AccountUUID: "uuid-1"}}
		}

		var calls []string
		removePlatformClaudeKeychainAccountsByEmail = func(email string) error {
			calls = append(calls, "platform:"+email)
			return nil
		}
		removeCQClaudeAccountsByEmail = func(email string) error {
			calls = append(calls, "cq:"+email)
			return nil
		}
		removeActiveClaudeCredentialsByEmail = func(email string) error {
			calls = append(calls, "active:"+email)
			return nil
		}

		mgr := &Accounts{}
		if err := mgr.Remove(context.Background(), "user@example.com"); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		// Correct order: platform first (pre-seed sources still present),
		// then cq-managed, then active credentials.
		want := []string{
			"platform:user@example.com",
			"cq:user@example.com",
			"active:user@example.com",
		}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("calls[%d] = %q, want %q — platform removal must precede cq/active cleanup", i, calls[i], want[i])
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
		oldRemovePlatform := removePlatformClaudeKeychainAccountsByEmail
		oldRemoveCQ := removeCQClaudeAccountsByEmail
		defer func() {
			discoverClaudeAccounts = oldDiscover
			removePlatformClaudeKeychainAccountsByEmail = oldRemovePlatform
			removeCQClaudeAccountsByEmail = oldRemoveCQ
		}()

		discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "user@example.com", AccountUUID: "uuid-1"}}
		}
		removePlatformClaudeKeychainAccountsByEmail = func(email string) error {
			return nil
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

func TestAccountsDiscoverUsesInjectedDiscovery(t *testing.T) {
	oldDiscover := discoverClaudeAccounts
	defer func() { discoverClaudeAccounts = oldDiscover }()

	discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{
			Email:            "stubbed@example.invalid",
			AccountUUID:      "uuid-stub",
			SubscriptionType: "max",
			RateLimitTier:    "tier-1",
		}}
	}

	mgr := &Accounts{}
	got, err := mgr.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Email != "stubbed@example.invalid" || got[0].AccountID != "uuid-stub" {
		t.Fatalf("identity = (%q, %q), want (stubbed@example.invalid, uuid-stub)", got[0].Email, got[0].AccountID)
	}
	if got[0].Label != "max" || got[0].RateLimitTier != "tier-1" {
		t.Fatalf("metadata = (%q, %q), want (max, tier-1)", got[0].Label, got[0].RateLimitTier)
	}
}

func TestAccountsSwitchUsesInjectedPersistence(t *testing.T) {
	oldDiscover := discoverClaudeAccounts
	oldWriteCredentialsFile := writeCredentialsFile
	oldUpdateKeychainEntry := updateKeychainEntry
	oldStoreCQAccount := storeCQAccount
	defer func() {
		discoverClaudeAccounts = oldDiscover
		writeCredentialsFile = oldWriteCredentialsFile
		updateKeychainEntry = oldUpdateKeychainEntry
		storeCQAccount = oldStoreCQAccount
	}()

	discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{
			Email:            "user@example.com",
			AccountUUID:      "uuid-1",
			AccessToken:      "token-1",
			RefreshToken:     "refresh-1",
			SubscriptionType: "max",
			RateLimitTier:    "tier-1",
		}}
	}

	wrote := 0
	updated := 0
	stored := 0
	writeCredentialsFile = func(creds *keyring.ClaudeCredentials) error {
		wrote++
		if creds == nil || creds.ClaudeAiOauth == nil {
			t.Fatal("expected Claude credentials payload")
		}
		if creds.ClaudeAiOauth.Email != "user@example.com" {
			t.Fatalf("credentials email = %q, want user@example.com", creds.ClaudeAiOauth.Email)
		}
		return nil
	}
	updateKeychainEntry = func(service string, creds *keyring.ClaudeCredentials) error {
		updated++
		if service != "Claude Code-credentials" {
			t.Fatalf("service = %q, want Claude Code-credentials", service)
		}
		return nil
	}
	storeCQAccount = func(acct *keyring.ClaudeOAuth) error {
		stored++
		if acct.Email != "user@example.com" || acct.AccountUUID != "uuid-1" {
			t.Fatalf("stored account = (%q, %q), want (user@example.com, uuid-1)", acct.Email, acct.AccountUUID)
		}
		return nil
	}

	mgr := &Accounts{}
	got, err := mgr.Switch(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if !got.Active {
		t.Fatalf("got.Active = %v, want true", got.Active)
	}
	if got.Email != "user@example.com" || got.AccountID != "uuid-1" {
		t.Fatalf("identity = (%q, %q), want (user@example.com, uuid-1)", got.Email, got.AccountID)
	}
	if wrote != 1 || updated != 1 || stored != 1 {
		t.Fatalf("calls = (write=%d update=%d store=%d), want (1,1,1)", wrote, updated, stored)
	}
}

func TestAccountsSwitchFailsWhenRefreshAndProfileBothFail(t *testing.T) {
	oldDiscover := discoverClaudeAccounts
	oldWriteCredentialsFile := writeCredentialsFile
	oldUpdateKeychainEntry := updateKeychainEntry
	oldStoreCQAccount := storeCQAccount
	defer func() {
		discoverClaudeAccounts = oldDiscover
		writeCredentialsFile = oldWriteCredentialsFile
		updateKeychainEntry = oldUpdateKeychainEntry
		storeCQAccount = oldStoreCQAccount
	}()

	discoverClaudeAccounts = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{
			Email:            "user@example.com",
			AccountUUID:      "uuid-1",
			AccessToken:      "dead-at",
			RefreshToken:     "dead-rt",
			ExpiresAt:        1,
			SubscriptionType: "max",
			RateLimitTier:    "tier-1",
		}}
	}

	wrote := 0
	updated := 0
	stored := 0
	writeCredentialsFile = func(*keyring.ClaudeCredentials) error {
		wrote++
		return nil
	}
	updateKeychainEntry = func(string, *keyring.ClaudeCredentials) error {
		updated++
		return nil
	}
	storeCQAccount = func(*keyring.ClaudeOAuth) error {
		stored++
		return nil
	}

	mgr := &Accounts{HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth/token":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			}, nil
		case "/api/oauth/profile":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
			}, nil
		default:
			t.Fatalf("unexpected request to %q", req.URL.Path)
			return nil, nil
		}
	})}

	_, err := mgr.Switch(context.Background(), "user@example.com")
	if err == nil {
		t.Fatal("expected error when refresh and profile both fail")
	}
	if wrote != 0 || updated != 0 || stored != 0 {
		t.Fatalf("calls = (write=%d update=%d store=%d), want (0,0,0)", wrote, updated, stored)
	}
}

func TestDiscoverAccountsInspectionCancellationSkipsKeyring(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Accounts{}).Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func mutationAccounts(t *testing.T) (*Accounts, *keyring.ClaudeAccountInspection, *[]string) {
	t.Helper()
	row := &keyring.ClaudeAccountInspection{Account: keyring.ClaudeOAuth{AccountUUID: "uuid", Email: " Mixed@Example.com ", AccessToken: "secret"}, Sources: []string{"cq_managed"}}
	calls := new([]string)
	ops := &AccountMutationOperations{
		Inspect: func(ctx context.Context) ([]keyring.ClaudeAccountInspection, error) {
			return []keyring.ClaudeAccountInspection{*row}, ctx.Err()
		},
		Store: func(ctx context.Context, a *keyring.ClaudeOAuth) error {
			*calls = append(*calls, "store")
			return ctx.Err()
		},
		Write: func(ctx context.Context, c *keyring.ClaudeCredentials) error {
			*calls = append(*calls, "write")
			return ctx.Err()
		},
		Update: func(ctx context.Context, _ string, c *keyring.ClaudeCredentials) error {
			*calls = append(*calls, "update")
			return ctx.Err()
		},
		Remove: func(ctx context.Context, a keyring.ClaudeOAuth) (keyring.ClaudeRemovalResult, error) {
			*calls = append(*calls, "remove")
			return keyring.ClaudeRemovalResult{Changed: true}, ctx.Err()
		},
	}
	return &Accounts{Mutations: ops}, row, calls
}
func TestActivateSelectedClaudeIdentityAndPartial(t *testing.T) {
	for _, mode := range []string{"success", "already-active", "changed-uuid", "duplicate", "missing", "write-failure", "keychain-failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			accounts, row, calls := mutationAccounts(t)
			expected := *row
			switch mode {
			case "already-active":
				row.Active = true
				expected.Active = true
			case "changed-uuid":
				row.Account.AccountUUID = "other"
			case "duplicate":
				accounts.Mutations.Inspect = func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
					other := *row
					other.Account.AccountUUID = "other"
					return []keyring.ClaudeAccountInspection{*row, other}, nil
				}
			case "missing":
				accounts.Mutations.Inspect = func(context.Context) ([]keyring.ClaudeAccountInspection, error) { return nil, nil }
			case "write-failure":
				accounts.Mutations.Write = func(context.Context, *keyring.ClaudeCredentials) error { return errors.New("private details") }
			case "keychain-failure":
				accounts.Mutations.Update = func(context.Context, string, *keyring.ClaudeCredentials) error { return errors.New("private details") }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			result, err := accounts.ActivateSelected(ctx, expected)
			switch mode {
			case "success":
				if err != nil || !result.Changed || !result.Active || len(*calls) != 3 {
					t.Fatalf("result=%+v err=%v calls=%v", result, err, *calls)
				}
			case "already-active":
				if err != nil || result.Changed || !result.Active || len(*calls) != 0 {
					t.Fatal("active assertion wrote credentials")
				}
			case "keychain-failure":
				if err == nil || !result.Changed || !result.Active {
					t.Fatal("lost native write evidence")
				}
			default:
				if err == nil || result.Changed || len(*calls) != 0 {
					t.Fatalf("unexpected writes/result err=%v calls=%v", err, *calls)
				}
			}
		})
	}
}
func TestRemovalSelectedClaudeUniqueCaseInsensitiveIdentity(t *testing.T) {
	accounts, row, calls := mutationAccounts(t)
	selected, err := ResolveAccountReference([]keyring.ClaudeAccountInspection{*row}, " mixed@example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	result, err := accounts.RemoveSelected(context.Background(), selected.Account)
	if err != nil || !result.Changed || len(*calls) != 1 {
		t.Fatal("unique trimmed email did not remove exact identity")
	}
	row.Account.AccountUUID = "replacement"
	if _, err := accounts.RemoveSelected(context.Background(), selected.Account); err == nil || len(*calls) != 1 {
		t.Fatal("same-email replacement removed")
	}
}
func TestSaveLoginClaudeDefaultDoesNotActivate(t *testing.T) {
	accounts, row, calls := mutationAccounts(t)
	saved, err := accounts.SaveLogin(context.Background(), row.Account)
	if err != nil || !saved || len(*calls) != 1 || (*calls)[0] != "store" {
		t.Fatal("save changed native credentials")
	}
	accounts.Mutations.Store = func(context.Context, *keyring.ClaudeOAuth) error {
		return &keyring.ClaudeStoreError{Err: errors.New("manifest failure")}
	}
	if saved, err := accounts.SaveLogin(context.Background(), row.Account); !saved || err == nil {
		t.Fatal("manifest failure lost keyring persistence")
	}
}
