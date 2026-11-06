package zai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ownershipDoer func(*http.Request) (*http.Response, error)

func (f ownershipDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func ownershipFixture(t *testing.T, secret string) ownershipDoer {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Host != "api.z.ai" {
			t.Fatal("ownership probe escaped read scope")
		}
		data := ""
		switch r.URL.Path {
		case "/api/biz/customer/getCustomerInfo":
			if r.Header.Get("Authorization") != "business-one" {
				t.Fatal("customer token shape")
			}
			data = `{"organizations":[{"organizationId":"org","projects":[{"projectId":"personal","projectType":1},{"projectId":"team","projectType":2}]}]}`
		case "/api/biz/v1/organization/org/projects/personal/api_keys":
			data = `[{"apiKey":"key-id"}]`
		case "/api/biz/v1/organization/org/projects/personal/api_keys/copy/key-id":
			data = `{"secretKey":"` + secret + `"}`
		default:
			t.Fatal("unexpected read endpoint")
		}
		if r.URL.Path != "/api/biz/customer/getCustomerInfo" && r.Header.Get("Authorization") != "Bearer business-one" {
			t.Fatal("key token shape")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":200,"data":` + data + `}`))}, nil
	}
}

func TestVerifiedOwnershipUpgradeRemovedOnlyMatchingOwnedKeyAndSuppressedExternal(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "one")
	session.BusinessJWT = "business-one"
	owned, err := StoreAccount(fs, "/config", "key-id.secret", "same", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StoreAccount(fs, "/config", "different.secret", "other", false); err != nil {
		t.Fatal(err)
	}
	home, _ := fs.UserHomeDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	_ = fs.MkdirAll(filepath.Dir(path), 0o700)
	external := []byte(`{"version":1,"providers":[{"id":"zai","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"borrowed","token":"key-id.secret"}]}}]}`)
	_ = fs.WriteFile(path, external, 0o600)
	proof, err := VerifyAPIKeyOwnership(context.Background(), ownershipFixture(t, "secret"), session, []Account{owned})
	if err != nil {
		t.Fatal(err)
	}
	oauth, err := StoreVerifiedZCodeAccount(fs, "/config", session, "OAuth", true, proof)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 || got[0].ID != oauth.ID || got[1].APIKey != "different.secret" {
		t.Fatalf("wrong upgraded view count=%d err=%v", len(got), err)
	}
	saved, _ := fs.ReadFile("/config/zai.json")
	if strings.Contains(string(saved), "key-id.secret") {
		t.Fatal("matched owned key retained")
	}
	returned, err := StoreAccount(fs, "/config", "key-id.secret", "key again", true)
	if err != nil || returned.ID != oauth.ID || returned.OAuth == nil {
		t.Fatal("verified key login downgraded OAuth")
	}
	if _, err := StoreZCodeAccount(fs, "/config", session, "rotated", true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(saved, &decoded) != nil {
		t.Fatal("invalid persistence")
	}
	after, _ := fs.ReadFile(path)
	if string(after) != string(external) {
		t.Fatal("external source changed")
	}
	if err := RemoveAccount(fs, "/config", oauth.ID); err != nil {
		t.Fatal(err)
	}
	got, err = ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 {
		t.Fatal("borrowed key not restored after OAuth removal")
	}
}

func TestOwnershipRejectedResponsesNeverProducedProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"forbidden", 403, `sensitive-secret`}, {"malformed", 200, `sensitive-secret`}, {"null", 200, `{"code":200,"data":null}`}, {"vendor", 200, `{"code":401,"message":"sensitive-secret"}`}, {"missing organizations", 200, `{"code":200,"data":{}}`}} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := VerifyAPIKeyOwnership(t.Context(), ownershipDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}), oauthAccountFixture("one", "one"), []Account{newAccount("key-id.secret", "")})
			if err == nil || len(proof.keyIDs) != 0 || strings.Contains(err.Error(), "sensitive-secret") {
				t.Fatal("rejected response produced proof or exposed response")
			}
		})
	}
}

func TestOwnershipRequiresExactSecretAndMatchingUser(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "one")
	session.BusinessJWT = "business-one"
	owned, err := StoreAccount(fs, "/config", "key-id.secret", "same label", false)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := VerifyAPIKeyOwnership(context.Background(), ownershipFixture(t, "other-secret"), session, []Account{owned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StoreVerifiedZCodeAccount(fs, "/config", session, "same label", true, proof); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 {
		t.Fatal("key ID or label used as identity")
	}
	proof, err = VerifyAPIKeyOwnership(context.Background(), ownershipFixture(t, "secret"), session, []Account{owned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StoreVerifiedZCodeAccount(fs, "/config", oauthAccountFixture("two", "two"), "", true, proof); err == nil {
		t.Fatal("cross-user proof accepted")
	}
}

func TestOwnershipSkippedOpaqueKeysWithoutHTTP(t *testing.T) {
	_, err := VerifyAPIKeyOwnership(t.Context(), ownershipDoer(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil }), oauthAccountFixture("one", "one"), []Account{newAccount("legacy-private", "")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedOAuthUpgradePreservedPreferredIdentityWithoutActivation(t *testing.T) {
	for _, existingOAuth := range []bool{false, true} {
		t.Run(fmt.Sprint(existingOAuth), func(t *testing.T) {
			fs := fsutil.NewMemFS()
			session := oauthAccountFixture("one", "one")
			session.BusinessJWT = "business-one"
			key, err := StoreAccount(fs, "/config", "key-id.secret", "preferred", false)
			if err != nil {
				t.Fatal(err)
			}
			other, err := StoreAccount(fs, "/config", "other.secret", "other", false)
			if err != nil {
				t.Fatal(err)
			}
			if existingOAuth {
				if _, err := StoreZCodeAccount(fs, "/config", session, "OAuth", false); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := VerifyAPIKeyOwnership(t.Context(), ownershipFixture(t, "secret"), session, []Account{key})
			if err != nil {
				t.Fatal(err)
			}
			oauth, err := StoreVerifiedZCodeAccount(fs, "/config", session, "OAuth", false, proof)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadAccounts(fs, noEnvironment, "/config")
			if err != nil || len(got) != 2 || got[0].ID != oauth.ID || got[1].ID != other.ID {
				t.Fatal("upgrade changed preferred logical account")
			}
		})
	}
}

func TestEnvironmentKnownAliasPreferredOAuthWithoutOtherDiscovery(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "one")
	key := newAccount("key-id.secret", "")
	oauth, err := StoreVerifiedZCodeAccount(fs, "/config", session, "OAuth", true, VerifiedAPIKeyOwnership{userID: session.UserID, keyIDs: []string{key.ID}})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := fs.UserHomeDir()
	source := filepath.Join(home, ".codexbar", "config.json")
	_ = fs.MkdirAll(filepath.Dir(source), 0o700)
	_ = fs.WriteFile(source, []byte(`invalid`), 0o600)
	before, _ := fs.ReadFile("/config/zai.json")
	for _, tc := range []struct {
		key       string
		wantOAuth bool
	}{{"key-id.secret", true}, {"different.secret", false}} {
		t.Run(tc.key, func(t *testing.T) {
			accounts, err := ReadAccounts(fs, func(name string) string {
				if name == "ZAI_API_KEY" {
					return tc.key
				}
				return ""
			}, "/config")
			if err != nil || len(accounts) != 1 {
				t.Fatalf("environment discovery failed: count=%d err=%v", len(accounts), err)
			}
			if tc.wantOAuth {
				if accounts[0].ID != oauth.ID || accounts[0].OAuth == nil {
					t.Fatal("verified environment alias bypassed OAuth")
				}
			} else if accounts[0].APIKey != tc.key || accounts[0].OAuth != nil {
				t.Fatal("distinct environment override lost")
			}
		})
	}
	after, _ := fs.ReadFile("/config/zai.json")
	if string(after) != string(before) {
		t.Fatal("environment discovery mutated store")
	}
}

func TestEnvironmentKeySurvivedUnavailableOwnedStore(t *testing.T) {
	for _, broken := range []string{`invalid`, `{"accounts":[{"zcode_account_id":"missing","transport":"zcode"}]}`} {
		fs := fsutil.NewMemFS()
		_ = fs.MkdirAll("/config", 0o700)
		_ = fs.WriteFile("/config/zai.json", []byte(broken), 0o600)
		accounts, err := ReadAccounts(fs, func(name string) string {
			if name == "ZAI_API_KEY" {
				return "key-id.secret"
			}
			return ""
		}, "/config")
		if err != nil || len(accounts) != 1 || accounts[0].APIKey != "key-id.secret" {
			t.Fatal("unavailable store broke valid environment key")
		}
	}
}

func TestOwnershipSkippedUnverifiedAndTeamProjectTypes(t *testing.T) {
	for _, kind := range []string{`null`, `2`, `"2"`, `0`, `3`, `{}`, `"unknown"`} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/api/biz/customer/getCustomerInfo" {
					t.Fatal("unverified project read")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"organizations":[{"organizationId":"org","projects":[{"projectId":"project","projectType":` + kind + `}]}]}}`))}, nil
			})
			proof, err := VerifyAPIKeyOwnership(t.Context(), client, oauthAccountFixture("one", "one"), []Account{newAccount("key-id.secret", "")})
			if err != nil || len(proof.keyIDs) != 0 || calls != 1 {
				t.Fatal("unverified project established ownership")
			}
		})
	}
}

func TestOwnershipLiveReadOnly(t *testing.T) {
	if os.Getenv("CQ_ZAI_OWNERSHIP_LIVE") != "1" {
		t.Skip("explicit read-only opt-in")
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal("roots unavailable")
	}
	dir := roots.Config
	if override := os.Getenv("CQ_ZAI_OWNERSHIP_CONFIG"); override != "" {
		dir = override
	}
	fs := fsutil.OSFileSystem{}
	store, err := zcode.OpenSessionStore(fs, dir, false)
	if err != nil {
		t.Skip("existing CQ OAuth store unavailable")
	}
	sessions, err := store.Accounts()
	if err != nil || len(sessions) == 0 {
		t.Skip("existing CQ OAuth login unavailable")
	}
	accounts, err := ReadAccounts(fs, os.Getenv, roots.Config)
	if err != nil {
		t.Fatal("key discovery unavailable")
	}
	for _, session := range sessions {
		proof, err := VerifyAPIKeyOwnership(t.Context(), &http.Client{}, session, accounts)
		if err != nil {
			t.Log(err)
			t.Fatal("read-only ownership verification unavailable")
		}
		t.Logf("verified_exact_key_count=%d", len(proof.keyIDs))
	}
}
