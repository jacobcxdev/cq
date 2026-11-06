package grok

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// TestGrokLiveOwnedRenewal performs one real rotating exchange on an explicitly
// selected CQ profile. It is intentionally separate from automatic expiry
// detection: qualification invokes the production transaction without changing
// token expiry or using a fake clock. Never enable it for routine package runs.
func TestGrokLiveOwnedRenewal(t *testing.T) {
	if os.Getenv("CQ_GROK_LIVE_RENEWAL") != "1" {
		t.Skip("set CQ_GROK_LIVE_RENEWAL=1 with an isolated CQ profile to qualify real renewal")
	}
	configDir := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if !filepath.IsAbs(configDir) || filepath.Clean(configDir) != configDir {
		t.Fatal("live renewal requires an absolute canonical CQ profile path")
	}
	if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
		t.Fatal("live renewal requires an existing CQ profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := httputil.NewClient(10*time.Second, "cq-grok-live-renewal")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	oauth := auth.GrokOAuth{Client: client}
	exchange := func(ctx context.Context, refresh string) (TokenSet, error) {
		tokens, err := oauth.Refresh(ctx, refresh)
		if err != nil {
			return TokenSet{}, errors.New("live OAuth renewal failed")
		}
		identity, err := SubscriptionIdentity(ctx, client, tokens.AccessToken)
		if err != nil {
			return TokenSet{}, errors.New("live renewed identity verification failed")
		}
		if identity.PrincipalType == "" {
			identity.PrincipalType = tokens.PrincipalType
		}
		if identity.PrincipalID == "" {
			identity.PrincipalID = tokens.PrincipalID
		}
		if identity.TeamID == "" {
			identity.TeamID = tokens.TeamID
		}
		if tokens.UserID != "" && tokens.UserID != identity.UserID || tokens.PrincipalType != "" && tokens.PrincipalType != identity.PrincipalType || tokens.PrincipalID != "" && tokens.PrincipalID != identity.PrincipalID || tokens.TeamID != "" && tokens.TeamID != identity.TeamID {
			return TokenSet{}, errors.New("live renewed identity mismatch")
		}
		return TokenSet{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, ExpiresAt: tokens.ExpiresAt, UserID: identity.UserID, PrincipalType: identity.PrincipalType, PrincipalID: identity.PrincipalID, TeamID: identity.TeamID}, nil
	}
	store := Store{FS: fsutil.OSFileSystem{}, ConfigDir: configDir}
	authority := NewAuthority(store, client, exchange)
	lock, err := store.lock(ctx)
	if err != nil {
		t.Fatal("live credential owner lock unavailable")
	}
	closed := false
	defer func() {
		if !closed {
			_ = lock.Close()
		}
	}()
	records, err := lock.store.read()
	if err != nil || len(records.Accounts) == 0 {
		t.Fatal("live profile has no readable CQ-owned Grok login")
	}
	before := records.Accounts[0]
	if before.Operation != nil || before.Tokens.Refresh == "" {
		t.Fatal("live profile requires a fresh CQ-owned renewable login with no pending operation")
	}
	if err := authority.refreshLocked(ctx, lock.store, &records, 0); err != nil {
		t.Fatal("live renewal transaction failed; inspect private profile state without replaying an uncertain exchange")
	}
	if err := lock.Close(); err != nil {
		closed = true
		t.Fatal("live credential owner lock release failed")
	}
	closed = true
	reopened := NewAuthority(store, client, exchange)
	afterRecords, err := reopened.Store.read()
	if err != nil || len(afterRecords.Accounts) == 0 {
		t.Fatal("live renewed profile could not be reopened")
	}
	after := afterRecords.Accounts[0]
	identityStable := after.Account.ID == before.Account.ID && after.Account.UserID == before.Account.UserID && after.Account.PrincipalType == before.Account.PrincipalType && after.Account.PrincipalID == before.Account.PrincipalID && after.Account.TeamID == before.Account.TeamID && after.Generation == before.Generation
	accessChanged := after.Tokens.Access != before.Tokens.Access
	refreshChanged := after.Tokens.Refresh != before.Tokens.Refresh
	expiryFuture := after.Tokens.Expiry.After(time.Now())
	if !identityStable || (!accessChanged && !refreshChanged) || !expiryFuture || after.Operation != nil {
		t.Fatalf("live renewal proof failed: identity_stable=%t access_changed=%t refresh_changed=%t expiry_future=%t pending_operation=%t", identityStable, accessChanged, refreshChanged, expiryFuture, after.Operation != nil)
	}
	accounts, err := reopened.Accounts(ctx)
	if err != nil || len(accounts) == 0 || accounts[0].ID != before.Account.ID {
		t.Fatal("live preferred account changed after renewal")
	}
	quotaResult := New(reopened).fetchAccount(ctx, accounts[0], time.Now())
	if !quotaResult.IsUsable() || len(quotaResult.Windows) == 0 {
		t.Fatal("live post-renewal included-quota read failed")
	}
	t.Logf("live renewal qualified: identity_stable=%t access_changed=%t refresh_changed=%t expiry_future=%t included_quota_read=true", identityStable, accessChanged, refreshChanged, expiryFuture)
}
