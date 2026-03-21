package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// fakeCodexJWT builds a Codex-style JWT with the given claims.
func fakeCodexJWT(email, accountID, userID, planType string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := map[string]any{
		"email": email,
		"exp":   1774076490.0,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  planType,
		},
	}
	body, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + encoded + ".fakesig"
}

func codexAuthJSON(accessToken, accountID, idToken string) []byte {
	m := map[string]any{
		"auth_mode":   "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "ref-tok",
			"id_token":      idToken,
			"account_id":    accountID,
		},
		"last_refresh": "2026-03-21T06:56:43.237634Z",
	}
	b, _ := json.Marshal(m)
	return b
}

func TestDiscoverAccountsSingleActive(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@example.com", "acct-123", "user-456", "plus")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok-abc", "acct-123", jwt)

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if accts[0].Email != "user@example.com" {
		t.Errorf("Email = %q, want user@example.com", accts[0].Email)
	}
	if accts[0].AccountID != "acct-123" {
		t.Errorf("AccountID = %q, want acct-123", accts[0].AccountID)
	}
	if accts[0].PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", accts[0].PlanType)
	}
	if !accts[0].IsActive {
		t.Error("expected IsActive=true for auth.json account")
	}
	if accts[0].RecordKey != "user-456::acct-123" {
		t.Errorf("RecordKey = %q, want user-456::acct-123", accts[0].RecordKey)
	}
}

func TestDiscoverAccountsMultiple(t *testing.T) {
	fs := newFakeFS()

	jwt1 := fakeCodexJWT("alice@test.com", "acct-aaa", "user-aaa", "plus")
	jwt2 := fakeCodexJWT("bob@test.com", "acct-bbb", "user-bbb", "pro")

	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok-alice", "acct-aaa", jwt1)
	// Simulate codex-auth accounts directory
	fs.files["/fake/home/.codex/accounts/user-aaa::acct-aaa.auth.json"] = codexAuthJSON("tok-alice", "acct-aaa", jwt1)
	fs.files["/fake/home/.codex/accounts/user-bbb::acct-bbb.auth.json"] = codexAuthJSON("tok-bob", "acct-bbb", jwt2)
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {
			{name: "user-aaa::acct-aaa.auth.json"},
			{name: "user-bbb::acct-bbb.auth.json"},
			{name: "registry.json"}, // should be skipped (not .auth.json)
		},
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 2 {
		t.Fatalf("len(accts) = %d, want 2", len(accts))
	}

	// First should be Alice (active), second Bob
	if accts[0].Email != "alice@test.com" {
		t.Errorf("accts[0].Email = %q, want alice@test.com", accts[0].Email)
	}
	if !accts[0].IsActive {
		t.Error("accts[0] should be active")
	}
	// Active account's FilePath should be updated to accounts/ copy
	if accts[0].FilePath != "/fake/home/.codex/accounts/user-aaa::acct-aaa.auth.json" {
		t.Errorf("accts[0].FilePath = %q, want accounts/ path", accts[0].FilePath)
	}

	if accts[1].Email != "bob@test.com" {
		t.Errorf("accts[1].Email = %q, want bob@test.com", accts[1].Email)
	}
	if accts[1].IsActive {
		t.Error("accts[1] should not be active")
	}
	if accts[1].PlanType != "pro" {
		t.Errorf("accts[1].PlanType = %q, want pro", accts[1].PlanType)
	}
}

func TestDiscoverAccountsNoAuthFile(t *testing.T) {
	fs := newFakeFS()
	accts := DiscoverAccounts(fs)
	if len(accts) != 0 {
		t.Fatalf("len(accts) = %d, want 0", len(accts))
	}
}

func TestDiscoverAccountsDedup(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-111", "user-111", "plus")
	authData := codexAuthJSON("tok-same", "acct-111", jwt)

	fs.files["/fake/home/.codex/auth.json"] = authData
	fs.files["/fake/home/.codex/accounts/user-111::acct-111.auth.json"] = authData
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {
			{name: "user-111::acct-111.auth.json"},
		},
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1 (deduped)", len(accts))
	}
	if !accts[0].IsActive {
		t.Error("deduped account should be active")
	}
}

func TestAccountsDiscover(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-x", "user-x", "team")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok", "acct-x", jwt)

	mgr := &Accounts{FS: fs}
	accts, err := mgr.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if accts[0].Label != "team" {
		t.Errorf("Label = %q, want team", accts[0].Label)
	}
	if !accts[0].Active {
		t.Error("expected Active=true")
	}
}

func TestAccountsSwitch(t *testing.T) {
	fs := newFakeFS()

	jwt1 := fakeCodexJWT("active@test.com", "acct-1", "user-1", "plus")
	jwt2 := fakeCodexJWT("other@test.com", "acct-2", "user-2", "pro")

	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok-1", "acct-1", jwt1)
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = codexAuthJSON("tok-1", "acct-1", jwt1)
	fs.files["/fake/home/.codex/accounts/user-2::acct-2.auth.json"] = codexAuthJSON("tok-2", "acct-2", jwt2)
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {
			{name: "user-1::acct-1.auth.json"},
			{name: "user-2::acct-2.auth.json"},
		},
	}

	mgr := &Accounts{FS: fs}
	acct, err := mgr.Switch(context.Background(), "other@test.com")
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if acct.Email != "other@test.com" {
		t.Errorf("Email = %q, want other@test.com", acct.Email)
	}
	if !acct.Active {
		t.Error("expected Active=true after switch")
	}

	// Verify auth.json was overwritten
	data, ok := fs.files["/fake/home/.codex/auth.json"]
	if !ok {
		t.Fatal("auth.json should exist after switch")
	}
	var af codexAuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		t.Fatalf("parse switched auth.json: %v", err)
	}
	if af.Tokens.AccessToken != "tok-2" {
		t.Errorf("switched auth.json token = %q, want tok-2", af.Tokens.AccessToken)
	}
}

func TestAccountsSwitchNotFound(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok", "acct-1", jwt)

	mgr := &Accounts{FS: fs}
	_, err := mgr.Switch(context.Background(), "nonexistent@test.com")
	if err == nil {
		t.Fatal("expected error for nonexistent email")
	}
}
