package zai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func oauthAccountFixture(userID, generation string) zcode.LoginSession {
	return zcode.LoginSession{UserID: userID, Email: userID + "@example.com", Name: userID,
		ZCodeJWT: "zcode-secret-" + generation, RawOAuth: "oauth-secret-" + generation,
		BusinessJWT: "business-secret-" + generation, RefreshToken: "refresh-secret-" + generation,
		DeviceMid: "device-" + generation}
}

func TestOAuthAccountsKeptStableIdentityAndSecretReferences(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/config", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/config/zai.json", []byte(`{"api_key":"legacy-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "initial"), "OAuth primary", false)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].APIKey != "legacy-key" || accounts[1].ID != first.ID || accounts[1].OAuth == nil {
		t.Fatal("OAuth login replaced legacy account or lost reference")
	}
	if accounts[1].APIKey != "" || accounts[1].OAuth.BusinessJWT != "business-secret-initial" || accounts[1].OAuth.DeviceMid != "device-initial" {
		t.Fatal("OAuth account credentials were not resolved independently")
	}
	if err := SetTransport(fsys, "/config", first.ID, "zcode-start"); err != nil {
		t.Fatal(err)
	}
	rotated, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "rotated"), "", true)
	if err != nil || rotated.ID != first.ID || rotated.Label != "OAuth primary" || rotated.Transport != "zcode-start" {
		t.Fatal("token rotation changed stable account, label or route")
	}
	accounts, err = ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].ID != first.ID || accounts[0].OAuth.RawOAuth != "oauth-secret-rotated" {
		t.Fatal("rotated preferred session did not reload")
	}
	public, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := fsys.ReadFile("/config/zai.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"zcode-secret-rotated", "oauth-secret-rotated", "business-secret-rotated", "refresh-secret-rotated", "device-rotated"} {
		if strings.Contains(string(public), secret) || strings.Contains(string(disk), secret) {
			t.Fatal("OAuth credential leaked through account output or reference file")
		}
	}
	if strings.Contains(string(public), "legacy-key") {
		t.Fatal("public accounts leaked API key")
	}
	var saved struct {
		Accounts []struct {
			APIKey    string `json:"api_key"`
			Reference string `json:"zcode_account_id"`
		} `json:"accounts"`
	}
	if json.Unmarshal(disk, &saved) != nil || len(saved.Accounts) != 2 || saved.Accounts[0].APIKey != "" || saved.Accounts[0].Reference != accounts[0].OAuth.ID {
		t.Fatal("OAuth reference was persisted as an API key")
	}
}

func TestOAuthAccountsSurvivedNativeAccountOperations(t *testing.T) {
	fsys := fsutil.NewMemFS()
	native, err := StoreAccount(fsys, "/config", "native-key", "native", false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "one"), "first OAuth", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-two", "two"), "second OAuth", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StoreAccount(fsys, "/config", "native-key", "renamed native", true); err != nil {
		t.Fatal(err)
	}
	if err := SetActiveAccount(fsys, "/config", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccount(fsys, "/config", native.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].ID != second.ID || accounts[1].ID != first.ID {
		t.Fatal("native operation lost OAuth account or preferred ordering")
	}
	if accounts[0].OAuth == nil || accounts[1].OAuth == nil || accounts[0].OAuth.BusinessJWT != "business-secret-two" || accounts[1].OAuth.BusinessJWT != "business-secret-one" {
		t.Fatal("native operation changed another account's OAuth identity")
	}
	if err := RemoveAccount(fsys, "/config", first.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err = ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].ID != second.ID || accounts[0].OAuth.ZCodeJWT != "zcode-secret-two" {
		t.Fatal("removing one OAuth reference damaged another")
	}
}

func TestOAuthAccountRoutesStayedWithinCredentialKind(t *testing.T) {
	fsys := fsutil.NewMemFS()
	native, _ := StoreAccount(fsys, "/config", "native-key", "native", false)
	oauth, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "one"), "oauth", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"zcode-start", "zcode"} {
		if err := SetTransport(fsys, "/config", oauth.ID, route); err != nil {
			t.Fatal(err)
		}
		accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
		if err != nil || accounts[1].Transport != route {
			t.Fatal("OAuth route did not persist")
		}
	}
	for _, invalid := range []struct{ id, route string }{{oauth.ID, "responses"}, {native.ID, "zcode-start"}, {oauth.ID, "unknown"}} {
		before, _ := fsys.ReadFile("/config/zai.json")
		if err := SetTransport(fsys, "/config", invalid.id, invalid.route); err == nil {
			t.Fatal("incompatible credential route accepted")
		}
		after, _ := fsys.ReadFile("/config/zai.json")
		if string(before) != string(after) {
			t.Fatal("invalid route changed account references")
		}
	}
	if err := SetTransport(fsys, "/config", native.ID, "zcode"); err != nil {
		t.Fatal(err)
	}
	if err := SetTransport(fsys, "/config", native.ID, "responses"); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthAccountsPreservedEnvironmentOverrideWithoutDiscovery(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/config", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "one"), "oauth", true); err != nil {
		t.Fatal(err)
	}
	// A damaged stored login must not block the established environment override.
	if err := fsys.WriteFile("/config/zcode-accounts.key", []byte("corrupt-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, func(key string) string {
		if key == "ZAI_API_KEY" {
			return "environment-key"
		}
		return ""
	}, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].APIKey != "environment-key" || accounts[0].OAuth != nil || accounts[0].Transport != "" {
		t.Fatal("environment override entered OAuth discovery or changed route")
	}
	if _, err := ReadAccounts(fsys, noEnvironment, "/config"); err == nil {
		t.Fatal("damaged OAuth key was ignored without environment override")
	}
}

func TestOAuthReferencesRejectedMissingOrMixedIdentity(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := StoreAccount(fsys, "/config", "native-key", "native", false); err != nil {
		t.Fatal(err)
	}
	oauth, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("user-one", "one"), "oauth", false)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := fsys.ReadFile("/config/zai.json")
	for _, mutate := range []func(*storedAccount){
		func(account *storedAccount) { account.ZCodeAccountID = "missing-session" },
		func(account *storedAccount) { account.APIKey = "private-mixed-key" },
		func(account *storedAccount) { account.ID = "another-account" },
		func(account *storedAccount) { account.Transport = "" },
	} {
		var file credentialFile
		if json.Unmarshal(original, &file) != nil {
			t.Fatal("invalid test fixture")
		}
		for i := range *file.Accounts {
			if (*file.Accounts)[i].ID == oauth.ID {
				mutate(&(*file.Accounts)[i])
			}
		}
		data, _ := json.Marshal(file)
		if err := fsys.WriteFile("/config/zai.json", data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccounts(fsys, noEnvironment, "/config"); err == nil || strings.Contains(err.Error(), "private-mixed-key") {
			t.Fatal("invalid OAuth reference accepted, replaced by native key or leaked")
		}
	}
}
