package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/opencodego"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestSharedBudgetPrefersOAuthWithExplicitKeyOption(t *testing.T) {
	fsys, configDir := privateSharedBudgetConfig(t)
	legacyKey := func(string) string { return "headless-key" }
	for _, tc := range []struct {
		name, mode string
		session    sharedBudgetLogin
		oauthErr   error
		want       string
	}{
		{"prefer browser login", "", sharedBudgetLogin{SharedBudgetCredential: proxy.SharedBudgetCredential{Bearer: "oauth-access", Account: "member", WorkspaceID: "wrk_oauth"}, Email: "verified@example.com"}, nil, "oauth-access"},
		{"explicit headless key", "api_key", sharedBudgetLogin{SharedBudgetCredential: proxy.SharedBudgetCredential{Bearer: "oauth-access"}}, nil, "headless-key"},
		{"existing key only", "", sharedBudgetLogin{}, os.ErrNotExist, "headless-key"},
		{"expired login retains ownership", "", sharedBudgetLogin{}, errors.New("sign in again"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := sharedBudgetAdapter{
				ID: modelregistry.ProviderOpenCodeGo, KeyEnv: "OPENCODE_API_KEY",
				OAuth: func(_ fsutil.FileSystem, _ string, _ httputil.Doer) sharedBudgetAuthorization {
					return sharedBudgetAuthorization{List: func(context.Context, bool) ([]sharedBudgetLogin, error) {
						return []sharedBudgetLogin{tc.session}, tc.oauthErr
					}, For: func(context.Context, modelregistry.SharedBudgetCredentialScope) (sharedBudgetLogin, error) {
						return tc.session, tc.oauthErr
					}}
				},
			}
			resolve := adapter.resolve(fsys, legacyKey, configDir, nil, sharedBudgetSettings{Auth: tc.mode})
			login, err := resolve.first(context.Background(), true)
			if login.Bearer != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("unexpected credential selection (available=%t, error=%t)", login.Bearer != "", err != nil)
			}
			if tc.want == "oauth-access" && (login.Account != "member" || login.WorkspaceID != "wrk_oauth" || login.Email != "verified@example.com") {
				t.Fatal("verified OAuth identity was lost")
			}
		})
	}
}

type openCodeLoginHTTP func(*http.Request) (*http.Response, error)

func (f openCodeLoginHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenCodeLoginRequiresGoAllowanceBeforeSaving(t *testing.T) {
	fsys, dir := privateSharedBudgetConfig(t)
	ctx := context.Background()
	session := opencodego.OAuthSession{
		AccessToken: "oauth-access", RefreshToken: "oauth-refresh", MemberID: "member", WorkspaceID: "wrk_go",
		Email: "registered@example.com", ExpiresAt: time.Now().Add(time.Hour),
	}
	status := http.StatusForbidden
	client := openCodeLoginHTTP(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != opencodego.UsageURL ||
			r.Header.Get("Authorization") != "Bearer oauth-access" || r.Header.Get("X-OpenCode-Org-Id") != "wrk_go" {
			t.Fatal("allowance verification used different identity")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"status":"ok","percent":20}}}`))}, nil
	})
	authority := openCodeSessionAuthority(fsys, dir, client)
	if err := saveOpenCodeGoLogin(ctx, client, authority, session); err == nil {
		t.Fatal("accepted a workspace with unavailable Go allowance")
	}
	if _, err := authority.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected login modified the credential store: %v", err)
	}
	status = http.StatusOK
	if err := saveOpenCodeGoLogin(ctx, client, authority, session); err != nil {
		t.Fatal(err)
	}
	_, checks := sharedBudgetChecks(fsys, func(string) string { return "" }, dir, []string{"opencode-go"})
	if len(checks) != 1 || checks[0].Report.Email != session.Email {
		t.Fatal("quota report did not use the saved login identity")
	}
}

func TestOpenCodeAccountsAppearSeparately(t *testing.T) {
	fsys, dir := privateSharedBudgetConfig(t)
	if err := fsys.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"extra_usage":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := openCodeSessionAuthority(fsys, dir, nil)
	first := opencodego.OAuthSession{AccessToken: "first-token", RefreshToken: "first-refresh", MemberID: "first", WorkspaceID: "wrk_first", Email: "first@example.test", ExpiresAt: time.Now().Add(time.Hour)}
	second := opencodego.OAuthSession{AccessToken: "second-token", RefreshToken: "second-refresh", MemberID: "second", WorkspaceID: "wrk_second", Email: "second@example.test", ExpiresAt: time.Now().Add(time.Hour)}
	for _, session := range []opencodego.OAuthSession{first, second} {
		if err := authority.Save(t.Context(), session); err != nil {
			t.Fatal(err)
		}
	}
	_, checks := sharedBudgetChecks(fsys, func(string) string { return "" }, dir, []string{"opencode-go"})
	if len(checks) != 2 || checks[0].Report.Email != first.Email || checks[1].Report.Email != second.Email {
		t.Fatalf("account reports: count=%d", len(checks))
	}
	if checks[0].Report.ExtraUsage != nil || checks[1].Report.ExtraUsage != nil {
		t.Fatal("provider-wide Extra Usage shown as per-account state")
	}
	route := sharedBudgetProxyRoutes(fsys, func(string) string { return "" }, dir)[modelregistry.ProviderOpenCodeGo]
	credentials, err := route.Credentials(t.Context())
	if err != nil || len(credentials) != 2 || credentials[0].Account != first.MemberID || credentials[1].Account != second.MemberID {
		t.Fatalf("account routes: count=%d, err=%v", len(credentials), err)
	}
	for _, credential := range credentials {
		scope := modelregistry.FingerprintSharedBudgetCredential(credential.Bearer, credential.Account, credential.WorkspaceID)
		resolved, err := route.CredentialFor(t.Context(), scope)
		if err != nil || resolved.Account != credential.Account {
			t.Fatalf("account-specific credential: %v", err)
		}
	}
}
