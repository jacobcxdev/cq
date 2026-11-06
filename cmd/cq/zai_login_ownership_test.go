package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestZAILoginUpgradedOnlyVerifiedOwnedKey(t *testing.T) {
	fsys := fsutil.NewMemFS()
	for _, key := range []string{"key-id.secret", "other-id.other-secret"} {
		if _, err := zaiprov.StoreAccount(fsys, "/config", key, "", false); err != nil {
			t.Fatal(err)
		}
	}
	var out, diagnostics bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	deps.errOutput = &diagnostics
	deps.deviceMid = func() (string, error) { return "device", nil }
	deps.oauthLogin = func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error) {
		return zcode.LoginSession{UserID: "user-one", Email: "one@example.com", ZCodeJWT: "zcode-private", RawOAuth: "oauth-private", BusinessJWT: "business-private"}, nil
	}
	reads := 0
	deps.client = zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != http.MethodGet {
			t.Fatal("ownership verification changed provider state")
		}
		var data string
		switch r.URL.Path {
		case "/api/biz/customer/getCustomerInfo":
			data = `{"organizations":[{"organizationId":"org","projects":[{"projectId":"personal","projectType":1}]}]}`
		case "/api/biz/v1/organization/org/projects/personal/api_keys":
			data = `[{"apiKey":"key-id"}]`
		case "/api/biz/v1/organization/org/projects/personal/api_keys/copy/key-id":
			data = `{"secretKey":"secret"}`
		default:
			t.Fatalf("unexpected ownership request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":` + data + `}`)), Header: make(http.Header)}, nil
	})
	if err := runZAIOAuthLogin(t.Context(), ZAILoginCmd{}, "/config", true, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].OAuth == nil || accounts[1].APIKey != "other-id.other-secret" || reads != 3 {
		t.Fatalf("same-account upgrade failed: count=%d reads=%d err=%v", len(accounts), reads, err)
	}
	stored, err := fsys.ReadFile("/config/zai.json")
	if err != nil || bytes.Contains(stored, []byte("key-id.secret")) {
		t.Fatal("upgraded CQ key remained in credential file")
	}
	if diagnostics.Len() != 0 || strings.Contains(out.String(), "private") || strings.Contains(out.String(), "secret") {
		t.Fatal("verified upgrade reported a warning or exposed credentials")
	}
}

func TestZAILoginPreservedKeysWhenOwnershipUnavailable(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := zaiprov.StoreAccount(fsys, "/config", "key-id.secret", "", false); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	deps.errOutput = &diagnostics
	deps.deviceMid = func() (string, error) { return "device", nil }
	deps.oauthLogin = func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error) {
		return zcode.LoginSession{UserID: "user-one", ZCodeJWT: "zcode-private", RawOAuth: "oauth-private", BusinessJWT: "business-private"}, nil
	}
	deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("rejected")), Header: make(http.Header)}, nil
	})
	if err := runZAIOAuthLogin(t.Context(), ZAILoginCmd{}, "/config", true, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].OAuth == nil || accounts[1].APIKey != "key-id.secret" {
		t.Fatal("unavailable ownership proof lost login or existing key")
	}
	if !strings.Contains(diagnostics.String(), "could not verify") || strings.Contains(diagnostics.String(), "private") || strings.Contains(diagnostics.String(), "secret") {
		t.Fatal("missing safe ownership warning")
	}
}
