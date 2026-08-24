package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

func appCodexJWT(email, accountID, userID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  "plus",
		},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func fakeAppCodexLogin(tokens auth.CodexTokenResponse, claims auth.CodexClaims) codexLoginFunc {
	return func(context.Context, httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error) {
		return &tokens, &claims, nil
	}
}

func TestRunCodexLoginWithoutActivatePreservesSystemAndActiveProjection(t *testing.T) {
	fs := fsutil.NewMemFS()
	systemPath := "/home/test/.codex/auth.json"
	registryPath := "/home/test/.codex/accounts/registry.json"
	systemBefore := []byte(`{"system":"untouched"}`)
	registryBefore := []byte(`{"schema_version":3,"active_account_key":"existing::active","accounts":[]}`)
	_ = fs.WriteFile(systemPath, systemBefore, 0o600)
	_ = fs.WriteFile(registryPath, registryBefore, 0o600)
	claims := auth.CodexClaims{Email: "new@test.com", AccountID: "acct-new", UserID: "user-new", PlanType: "plus"}
	tokens := auth.CodexTokenResponse{IDToken: appCodexJWT(claims.Email, claims.AccountID, claims.UserID), AccessToken: "new-access", RefreshToken: "new-refresh"}

	var output bytes.Buffer
	err := runCodexLogin(context.Background(), nil, false, fs, "/cq/state", fakeAppCodexLogin(tokens, claims), func() time.Time { return time.Unix(100, 0) }, &output)
	if err != nil {
		t.Fatalf("runCodexLogin: %v", err)
	}
	if got, _ := fs.ReadFile(systemPath); string(got) != string(systemBefore) {
		t.Fatalf("system auth changed: %s", got)
	}
	var registry map[string]any
	data, _ := fs.ReadFile(registryPath)
	_ = json.Unmarshal(data, &registry)
	if registry["active_account_key"] != "existing::active" {
		t.Fatalf("active projection changed: %#v", registry["active_account_key"])
	}
}

func TestRunCodexLoginActivateUsesExactSavedCandidate(t *testing.T) {
	fs := fsutil.NewMemFS()
	otherJWT := appCodexJWT("same@test.com", "acct-other", "user-other")
	other := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"wrong","id_token":"` + otherJWT + `","account_id":"acct-other"}}`)
	_ = fs.WriteFile("/home/test/.codex/accounts/user-other::acct-other.auth.json", other, 0o600)
	claims := auth.CodexClaims{Email: "same@test.com", AccountID: "acct-new", UserID: "user-new", PlanType: "plus"}
	tokens := auth.CodexTokenResponse{IDToken: appCodexJWT(claims.Email, claims.AccountID, claims.UserID), AccessToken: "exact", RefreshToken: "new-refresh"}

	var output bytes.Buffer
	err := runCodexLogin(context.Background(), nil, true, fs, "/cq/state", fakeAppCodexLogin(tokens, claims), func() time.Time { return time.Unix(100, 0) }, &output)
	if err != nil {
		t.Fatalf("runCodexLogin: %v", err)
	}
	data, _ := fs.ReadFile("/home/test/.codex/auth.json")
	var system map[string]any
	_ = json.Unmarshal(data, &system)
	if got := system["tokens"].(map[string]any)["access_token"]; got != "exact" {
		t.Fatalf("active access token = %#v, want exact saved candidate", got)
	}
}
