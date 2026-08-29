//go:build !windows

package proxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadCodexLiveAcceptanceCredentialIsAccessOnly(t *testing.T) {
	path := writeCodexLiveAcceptanceTestAuth(t, time.Now().Add(30*time.Minute).Unix())

	credential, err := readCodexLiveAcceptanceCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if credential.material.AccessToken == "" || credential.material.IDToken == "" {
		t.Fatal("live acceptance credential lost required access material")
	}
	if credential.material.RefreshToken != "" {
		t.Fatal("live acceptance credential retained refresh authority")
	}
}

func TestReadCodexLiveAcceptanceCredentialRequiresKnownLifetime(t *testing.T) {
	for _, expiresAt := range []int64{0, time.Now().Add(time.Minute).Unix()} {
		path := writeCodexLiveAcceptanceTestAuth(t, expiresAt)
		if _, err := readCodexLiveAcceptanceCredential(path); err == nil {
			t.Fatalf("credential with expiry %d was accepted", expiresAt)
		}
	}
}

func writeCodexLiveAcceptanceTestAuth(t *testing.T, expiresAt int64) string {
	t.Helper()
	idToken := codexLiveAcceptanceTestJWT(t, map[string]any{
		"email": "validation@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "validation-account",
			"chatgpt_user_id":    "validation-user",
			"chatgpt_plan_type":  "plus",
		},
	})
	accessClaims := map[string]any{}
	if expiresAt != 0 {
		accessClaims["exp"] = expiresAt
	}
	accessToken := codexLiveAcceptanceTestJWT(t, accessClaims)
	document := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "must-not-leave-source",
			"id_token":      idToken,
			"account_id":    "validation-account",
		},
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexLiveAcceptanceTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
