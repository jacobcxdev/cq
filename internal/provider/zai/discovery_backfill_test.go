package zai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestRefreshAccountsBackfilledExactOwnershipWithoutAdoption(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	oauth, err := StoreZCodeAccount(fs, "/config", session, "Preferred", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTransport(fs, "/config", oauth.ID, "zcode-start"); err != nil {
		t.Fatal(err)
	}
	other, err := StoreZCodeAccount(fs, "/config", oauthAccountFixture("two", "initial"), "Other", false)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := fs.UserHomeDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	_ = fs.MkdirAll(filepath.Dir(path), 0o700)
	external := []byte(`{"version":1,"providers":[{"id":"zai","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"borrowed","label":"Preferred","token":"key-id.secret"}]}}]}`)
	_ = fs.WriteFile(path, external, 0o600)
	sessionsBefore, _ := fs.ReadFile("/config/zcode-accounts.json")
	keyBefore, _ := fs.ReadFile("/config/zcode-accounts.key")
	reads := 0
	fixture := ownershipFixture(t, "secret")
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) { reads++; return fixture.Do(r) })
	accounts, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].ID != oauth.ID || accounts[1].ID != other.ID || accounts[0].Transport != "zcode-start" {
		t.Fatalf("account preference or route changed: count=%d err=%v", len(accounts), err)
	}
	if reads != 3 {
		t.Fatalf("exact provider ownership not read: requests=%d", reads)
	}
	saved, _ := fs.ReadFile("/config/zai.json")
	var record struct {
		Accounts []struct {
			ID      string   `json:"id"`
			Aliases []string `json:"verified_api_key_ids"`
		}
	}
	if json.Unmarshal(saved, &record) != nil || len(record.Accounts) != 2 || len(record.Accounts[0].Aliases) != 1 || record.Accounts[0].ID != oauth.ID || len(record.Accounts[1].Aliases) != 0 {
		t.Fatal("verified alias not backfilled onto its owner")
	}
	if bytes.Contains(saved, []byte("key-id.secret")) {
		t.Fatal("borrowed secret adopted")
	}
	after, _ := fs.ReadFile(path)
	sessionsAfter, _ := fs.ReadFile("/config/zcode-accounts.json")
	keyAfter, _ := fs.ReadFile("/config/zcode-accounts.key")
	if !bytes.Equal(after, external) || !bytes.Equal(sessionsBefore, sessionsAfter) || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("external source or encrypted login changed")
	}
	_, err = RefreshAccounts(t.Context(), ownershipDoer(func(*http.Request) (*http.Response, error) {
		t.Fatal("known alias caused another ownership read")
		return nil, nil
	}), fs, noEnvironment, "/config")
	if err != nil {
		t.Fatal(err)
	}
}
