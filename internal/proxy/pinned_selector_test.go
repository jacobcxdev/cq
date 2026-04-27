package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

// innerSelectorFunc adapts a function to the ClaudeSelector interface.
type innerSelectorFunc func(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error)

func (f innerSelectorFunc) Select(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
	return f(ctx, exclude...)
}

func makePinnedSelector(accounts []keyring.ClaudeOAuth, pin string) (*PinnedClaudeSelector, *innerSelectorFunc) {
	inner := innerSelectorFunc(func(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
		// Simple inner: return first non-excluded account.
		for i := range accounts {
			a := &accounts[i]
			excludeSet := make(map[string]bool, len(exclude))
			for _, e := range exclude {
				excludeSet[e] = true
			}
			if !isExcluded(a, excludeSet) && a.AccessToken != "" {
				result := *a
				return &result, nil
			}
		}
		return nil, errors.New("no accounts available")
	})
	discover := ClaudeDiscoverer(func() []keyring.ClaudeOAuth {
		return accounts
	})
	sel := NewPinnedClaudeSelector(inner, discover, pin)
	return sel, &inner
}

func TestPinnedClaudeSelector_NoPinDelegatesToInner(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccessToken: "tok-b", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// inner returns first non-excluded — a@test.com
	if acct.Email != "a@test.com" {
		t.Errorf("email = %q, want a@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_PinByEmailOverBetterQuota(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "tok-pin", ExpiresAt: future},
		{Email: "other@test.com", AccessToken: "tok-other", ExpiresAt: future + 9999},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Email != "pinned@test.com" {
		t.Errorf("email = %q, want pinned@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_PinByUUID(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccountUUID: "uuid-a", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccountUUID: "uuid-b", AccessToken: "tok-b", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "uuid-b")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.AccountUUID != "uuid-b" {
		t.Errorf("AccountUUID = %q, want uuid-b", acct.AccountUUID)
	}
}

func TestPinnedClaudeSelector_ExcludedPinDelegatesToInner(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "tok-pin", ExpiresAt: future},
		{Email: "fallback@test.com", AccessToken: "tok-fb", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	// Exclude the pinned account — should fall back to inner selector.
	acct, err := sel.Select(context.Background(), "pinned@test.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Email != "fallback@test.com" {
		t.Errorf("email = %q, want fallback@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_NotFoundReturnsError(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "missing@test.com")

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for missing pin, got nil")
	}
	if !strings.Contains(err.Error(), `pinned Claude account "missing@test.com" not found`) {
		t.Errorf("error = %q, want to contain pin-not-found message", err.Error())
	}
}

func TestPinnedClaudeSelector_UnusableNoAccessToken(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "", ExpiresAt: future}, // no token
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for unusable pin (no token), got nil")
	}
	if !strings.Contains(err.Error(), "no access token") {
		t.Errorf("error = %q, want to contain 'no access token'", err.Error())
	}
}

func TestPinnedClaudeSelector_UnusableExpiredNoRefresh(t *testing.T) {
	past := time.Now().UnixMilli() - 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "tok", ExpiresAt: past, RefreshToken: ""},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for expired token without refresh, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want to contain 'expired'", err.Error())
	}
}

func TestPinnedClaudeSelector_ExpiredWithRefreshTokenIsUsable(t *testing.T) {
	past := time.Now().UnixMilli() - 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "tok", ExpiresAt: past, RefreshToken: "rt"},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Email != "pinned@test.com" {
		t.Errorf("email = %q, want pinned@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_NoExpiryIsUsable(t *testing.T) {
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "tok", ExpiresAt: 0},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Email != "pinned@test.com" {
		t.Errorf("email = %q, want pinned@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_ReturnsCopy(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "pinned@test.com", AccessToken: "original", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "pinned@test.com")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acct.AccessToken = "mutated"

	acct2, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct2.AccessToken != "original" {
		t.Error("selector returned reference instead of copy")
	}
}

func TestPinnedClaudeSelector_SetPinUpdatesPin(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
		{Email: "b@test.com", AccessToken: "tok-b", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "a@test.com")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "a@test.com" {
		t.Errorf("before SetPin: email = %q, want a@test.com", acct.Email)
	}

	sel.SetPin("b@test.com")

	acct, err = sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("after SetPin: email = %q, want b@test.com", acct.Email)
	}
}

func TestPinnedClaudeSelector_ClearPinDelegatesToInner(t *testing.T) {
	future := time.Now().UnixMilli() + 3600_000
	accounts := []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "tok-a", ExpiresAt: future},
	}
	sel, _ := makePinnedSelector(accounts, "a@test.com")
	sel.SetPin("")

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// inner returns a@test.com (only account)
	if acct.Email != "a@test.com" {
		t.Errorf("email = %q, want a@test.com", acct.Email)
	}
}
