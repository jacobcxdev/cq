package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

func TestGrokRefreshVerifiesReturnedIdentity(t *testing.T) {
	client := testDoer(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.WriteString(`{"issuer":"https://auth.x.ai","authorization_endpoint":"https://auth.x.ai/oauth2/authorize","token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`)
		case "/oauth2/token":
			w.WriteString(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`)
		case "/v1/user":
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Fatal("wrong refreshed identity credential")
			}
			w.WriteString(`{"userId":"verified-user","subscriptionTier":"SuperGrok"}`)
		default:
			t.Fatal("unexpected request")
		}
		return w.Result(), nil
	})
	a := newGrokAuthority(fsutil.NewMemFS(), "/config", client)
	tokens, err := a.Exchange(context.Background(), "old-refresh")
	if err != nil || tokens.UserID != "verified-user" {
		t.Fatalf("refresh lacks authenticated identity: user=%q err=%v", tokens.UserID, err)
	}
}

func TestGrokRefreshPreservesOptionalPrincipalIdentity(t *testing.T) {
	access := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"principal_type":"Team","principal_id":"team-123"}`)) + ".signature"
	for _, tc := range []struct {
		name, profile string
		wantError     bool
	}{
		{"omitted", `{"userId":"team-123","subscriptionTier":"SuperGrok"}`, false},
		{"partial", `{"userId":"team-123","principalId":"team-123","subscriptionTier":"SuperGrok"}`, false},
		{"matching", `{"userId":"team-123","principalType":"Team","principalId":"team-123","teamId":"team-123","subscriptionTier":"SuperGrok"}`, false},
		{"changed_user", `{"userId":"other","subscriptionTier":"SuperGrok"}`, true},
		{"changed_type", `{"userId":"team-123","principalType":"User","subscriptionTier":"SuperGrok"}`, true},
		{"changed_principal", `{"userId":"team-123","principalId":"other","subscriptionTier":"SuperGrok"}`, true},
		{"changed_team", `{"userId":"team-123","teamId":"other","subscriptionTier":"SuperGrok"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testDoer(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					w.WriteString(`{"issuer":"https://auth.x.ai","authorization_endpoint":"https://auth.x.ai/oauth2/authorize","token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`)
				case "/oauth2/token":
					fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`, access)
				case "/v1/user":
					w.WriteString(tc.profile)
				default:
					t.Fatal("unexpected request")
				}
				return w.Result(), nil
			})
			a := newGrokAuthority(fsutil.NewMemFS(), "/config", client)
			tokens, err := a.Exchange(context.Background(), "old-refresh")
			if (err != nil) != tc.wantError {
				t.Fatalf("refresh identity error: %v", err)
			}
			if !tc.wantError && (tokens.UserID != "team-123" || tokens.PrincipalType != "Team" || tokens.PrincipalID != "team-123" || tokens.TeamID != "team-123") {
				t.Fatal("refresh lost authenticated Team principal hints")
			}
		})
	}
}

func TestGrokCommandsManageOnlyCQAccounts(t *testing.T) {
	var output bytes.Buffer
	a := grok.NewAuthority(grok.Store{FS: fsutil.OSFileSystem{}, ConfigDir: filepath.Join(t.TempDir(), "cq")}, nil, nil)
	deps := grokCommandDependencies{authority: a, output: &output, login: func(context.Context) (auth.GrokLogin, error) {
		return auth.GrokLogin{UserID: "user-1", Email: "test@example.com", Tokens: auth.GrokTokens{AccessToken: "access-test", RefreshToken: "refresh-test", IDToken: "id-test", ExpiresAt: time.Now().Add(time.Hour)}}, nil
	}, validate: func(_ context.Context, login grok.Login) (grok.Login, error) {
		login.Account.Plan = "SuperGrok"
		return login, nil
	}, invalidate: func() {}}
	if err := runGrokCommand(context.Background(), "login", GrokCmd{}, true, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "access-test") || strings.Contains(output.String(), "refresh-test") {
		t.Fatal("secret in output")
	}
	output.Reset()
	if err := runGrokCommand(context.Background(), "accounts", GrokCmd{}, true, deps); err != nil {
		t.Fatal(err)
	}
	var accounts []grok.Account
	if err := json.Unmarshal(output.Bytes(), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].Active || accounts[0].Plan != "SuperGrok" {
		t.Fatalf("accounts: %+v", accounts)
	}
	if err := runGrokCommand(context.Background(), "remove <reference>", GrokCmd{Remove: GrokReferenceCmd{Reference: string(accounts[0].ID)}}, false, deps); err != nil {
		t.Fatal(err)
	}
	remaining, err := a.Accounts(context.Background())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remove: %v %v", remaining, err)
	}
}

func TestRefreshIncludesGrokSubscriptions(t *testing.T) {
	discover, codexRefresh, grokRefresh, invalidate := discoverClaudeAccountsFn, refreshCodexAccountsFn, refreshGrokAccountsFn, invalidateProviderCacheFn
	t.Cleanup(func() {
		discoverClaudeAccountsFn, refreshCodexAccountsFn, refreshGrokAccountsFn, invalidateProviderCacheFn = discover, codexRefresh, grokRefresh, invalidate
	})
	discoverClaudeAccountsFn = func() []keyring.ClaudeOAuth { return nil }
	refreshCodexAccountsFn = func(context.Context, httputil.Doer, int64) (bool, error) { return false, nil }
	called, invalidated := false, false
	refreshGrokAccountsFn = func(context.Context) (bool, error) { called = true; return true, nil }
	invalidateProviderCacheFn = func(id provider.ID) {
		if id == provider.Grok {
			invalidated = true
		}
	}
	if err := refreshAccounts(); err != nil {
		t.Fatal(err)
	}
	if !called || !invalidated {
		t.Fatal("Grok subscriptions skipped refresh or cache invalidation")
	}
}

func TestGrokCommandAuthority(t *testing.T) {
	for _, args := range [][]string{{"grok", "login"}, {"grok", "accounts"}, {"grok", "switch", "account"}, {"grok", "remove", "account"}, {"check", "grok"}, {"grok", "resets", "list"}} {
		a, err := ClassifyProxyCommand(args)
		if err != nil || a.Terminating {
			t.Fatalf("%v: %+v %v", args, a, err)
		}
	}
	for _, args := range [][]string{{"grok", "login", "--api-key"}, {"grok", "credits"}, {"grok", "switch"}} {
		a, err := ClassifyProxyCommand(args)
		if err == nil && !a.Terminating {
			t.Fatalf("accepted %v", args)
		}
	}
}
