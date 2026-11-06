package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

// This read-only check reuses an explicitly selected, already saved CQ login.
func TestGrokLiveSavedSubscription(t *testing.T) {
	if os.Getenv("CQ_GROK_LIVE_SAVED") != "1" {
		t.Skip("requires an authenticated Grok subscription and explicit opt-in")
	}
	dir := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if !filepath.IsAbs(dir) {
		t.Fatal("absolute CQ_GROK_LIVE_CONFIG required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	authority := newGrokAuthority(fsutil.OSFileSystem{}, dir, grokHTTPClient(30*time.Second))
	accounts, err := authority.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatal("saved Grok subscription unavailable")
	}
	results, err := grok.New(authority).Fetch(ctx, time.Now())
	if err != nil || len(results) != len(accounts) {
		t.Fatal("live subscription quota failed")
	}
	for _, result := range results {
		if !result.IsUsable() {
			t.Fatalf("live quota unavailable: %v", result.Error)
		}
		for name, window := range result.Windows {
			t.Logf("quota window=%s remaining=%g reset=%s", name, window.RemainingPercent(), time.Unix(window.ResetAtUnix, 0).UTC().Format(time.RFC3339))
		}
	}
	for _, account := range accounts {
		models, err := grok.FetchModels(ctx, authority, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, model := range models {
			t.Logf("model=%q backend=%q context=%d", model.ID, model.Backend, model.ContextWindow)
		}
		t.Logf("plan=%q models=%d", account.Plan, len(models))
	}
}

// The opt-in test owns an isolated profile. It does not run inference or change billing.
func TestGrokLiveSubscriptionLogin(t *testing.T) {
	if os.Getenv("CQ_GROK_LIVE_LOGIN") != "1" {
		t.Skip("requires interactive Grok browser sign-in")
	}
	dir := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if !filepath.IsAbs(dir) {
		t.Fatal("isolated absolute CQ_GROK_LIVE_CONFIG required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	client := grokHTTPClient(30 * time.Second)
	oauth := auth.GrokOAuth{Client: client, OpenURL: func(ctx context.Context, raw string) error {
		t.Logf("Grok sign-in: %s", raw)
		return auth.OpenBrowserContext(ctx, raw)
	}}
	verified, err := oauth.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login := grok.Login{Account: grok.Account{UserID: verified.UserID, Email: verified.Email, PrincipalType: verified.PrincipalType, PrincipalID: verified.PrincipalID, TeamID: verified.TeamID}, AccessToken: verified.Tokens.AccessToken, RefreshToken: verified.Tokens.RefreshToken, IDToken: verified.Tokens.IDToken, ExpiresAt: verified.Tokens.ExpiresAt}
	login, err = grok.ValidateLogin(ctx, client, login)
	if err != nil {
		t.Fatal(err)
	}
	authority := newGrokAuthority(fsutil.OSFileSystem{}, dir, client)
	account, err := authority.SaveLogin(ctx, login, "Qualification", true)
	if err != nil {
		t.Fatal(err)
	}
	results, err := grok.New(authority).Fetch(ctx, time.Now())
	if err != nil || len(results) != 1 || !results[0].IsUsable() {
		t.Fatal("live subscription quota failed")
	}
	models, err := grok.FetchModels(ctx, authority, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		t.Logf("model=%q backend=%q context=%d", m.ID, m.Backend, m.ContextWindow)
	}
	t.Logf("Verified subscription login, quota and %d models. No inference dispatched.", len(models))
}
