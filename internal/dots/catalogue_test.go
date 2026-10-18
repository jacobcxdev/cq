package dots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type catalogueTestDoer func(*http.Request) (*http.Response, error)

func (f catalogueTestDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func catalogueTestResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data))}
}

func catalogueTestDocument(owner string, expiry int64) map[string]any {
	claims := map[string]any{"chatgpt_account_id": owner, "chatgpt_user_id": "viewer-" + owner}
	token := func(value any) string {
		body, _ := json.Marshal(value)
		return "synthetic." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
	}
	return map[string]any{"tokens": map[string]any{
		"account_id":    owner,
		"id_token":      token(map[string]any{"https://api.openai.com/auth": claims, "email": owner + "@example.invalid"}),
		"access_token":  token(map[string]any{"https://api.openai.com/auth": claims, "exp": expiry}),
		"refresh_token": "synthetic-never-used",
	}}
}

func catalogueTestWrite(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func catalogueTestClient(t *testing.T) *CatalogueClient {
	t.Helper()
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	if err := os.Mkdir(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(root, "auth.json")
	catalogueTestWrite(t, path, catalogueTestDocument("system", now.Unix()+3600))
	return &CatalogueClient{Settings: &Settings{SystemAuth: path, SystemAlias: AccountAlias("system"), CQAccounts: accounts, Dots: map[string]Dot{}}, Now: func() time.Time { return now }}
}

func catalogueTestPrimary(owner, name, root string) map[string]any {
	return map[string]any{"selection": map[string]any{"available": true, "thread_id": root}, "profile": map[string]any{"id": "profile-" + owner, "active_root_thread_id": root, "display_name": name, "status": "active"}}
}

func TestCatalogueInventoryRereadsWithoutWritingCredentials(t *testing.T) {
	c := catalogueTestClient(t)
	before, err := os.Stat(c.Settings.SystemAuth)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(c.Settings.SystemAuth)
	first, err := c.Inventory()
	if err != nil || len(first.Accounts) != 1 {
		t.Fatalf("first inventory: %+v %v", first, err)
	}
	path := filepath.Join(c.Settings.CQAccounts, "added.auth.json")
	document := catalogueTestDocument("added", c.now().Unix()+3600)
	catalogueTestWrite(t, path, document)
	second, err := c.Inventory()
	if err != nil || len(second.Accounts) != 2 {
		t.Fatalf("added inventory: %+v %v", second, err)
	}
	document = catalogueTestDocument("added", c.now().Unix()+7200)
	catalogueTestWrite(t, path, document)
	third, err := c.Inventory()
	if err != nil || third.Accounts[1].Token == second.Accounts[1].Token {
		t.Fatalf("rotation was not observed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fourth, err := c.Inventory()
	if err != nil || len(fourth.Accounts) != 1 {
		t.Fatalf("removed inventory: %+v %v", fourth, err)
	}
	after, _ := os.Stat(c.Settings.SystemAuth)
	current, _ := os.ReadFile(c.Settings.SystemAuth)
	if !bytes.Equal(original, current) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("system credential was changed")
	}
}

func TestCatalogueCredentialClaimsFailClosed(t *testing.T) {
	c := catalogueTestClient(t)
	for _, owner := range []string{"foreign", "system"} {
		expiry := c.now().Unix() + 3600
		if owner == "system" {
			expiry = 0
		}
		catalogueTestWrite(t, c.Settings.SystemAuth, catalogueTestDocument(owner, expiry))
		if _, err := c.Inventory(); err == nil {
			t.Fatalf("accepted %s credential", owner)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"tokens":[]}`, `{"tokens":{"account_id":"x","id_token":"bad","access_token":"bad"}}`} {
		if _, err := ParseCredential([]byte(raw), c.now()); err == nil {
			t.Fatalf("accepted malformed credential %s", raw)
		}
	}
}

func TestCatalogueInventoryExcludesUnsafeAndExpiredSources(t *testing.T) {
	c := catalogueTestClient(t)
	catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, "healthy.auth.json"), catalogueTestDocument("healthy", c.now().Unix()+3600))
	catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, "expired.auth.json"), catalogueTestDocument("expired", 0))
	catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, "broken.auth.json"), map[string]any{"tokens": []any{}})
	if err := os.Symlink(c.Settings.SystemAuth, filepath.Join(c.Settings.CQAccounts, "linked.auth.json")); err != nil {
		t.Fatal(err)
	}
	got, err := c.Inventory()
	if err != nil || len(got.Accounts) != 2 || len(got.Failures) != 3 {
		t.Fatalf("inventory counts: accounts=%d failures=%d err=%v", len(got.Accounts), len(got.Failures), err)
	}
	if err := os.Chmod(c.Settings.CQAccounts, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = c.Inventory()
	if err != nil || len(got.Accounts) != 1 || len(got.Failures) != 1 {
		t.Fatalf("unsafe source: %+v %v", got, err)
	}
	info, _ := os.Stat(c.Settings.CQAccounts)
	if info.Mode().Perm() != 0o755 {
		t.Fatal("source permissions were changed")
	}
}

func TestCatalogueDeclaredManifestFingerprintAndIdentity(t *testing.T) {
	c := catalogueTestClient(t)
	source := filepath.Join(filepath.Dir(c.Settings.SystemAuth), "declared")
	managed := filepath.Join(source, "managed")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(managed, "auth.json")
	catalogueTestWrite(t, path, catalogueTestDocument("external", c.now().Unix()+3600))
	data, _ := os.ReadFile(path)
	digest := sha256.Sum256(data)
	row := map[string]any{"managedHomePath": managed, "providerAccountID": "external", "workspaceAccountID": "external", "authFingerprint": hex.EncodeToString(digest[:])}
	manifest := filepath.Join(source, "managed-codex-accounts.json")
	c.Settings.AccountSource = source
	catalogueTestWrite(t, manifest, map[string]any{"version": 3, "accounts": []any{row}})
	got, err := c.Inventory()
	if err != nil || len(got.Accounts) != 2 {
		t.Fatalf("declared source: %+v %v", got, err)
	}
	for _, field := range []string{"authFingerprint", "workspaceAccountID"} {
		original := row[field]
		row[field] = "changed"
		catalogueTestWrite(t, manifest, map[string]any{"version": 3, "accounts": []any{row}})
		got, err := c.Inventory()
		if err != nil || len(got.Accounts) != 1 || len(got.Failures) != 1 {
			t.Fatalf("accepted changed %s: %+v %v", field, got, err)
		}
		row[field] = original
	}
	if err := os.Chmod(source, 0o777); err != nil {
		t.Fatal(err)
	}
	got, err = c.Inventory()
	if err != nil || len(got.Accounts) != 1 || len(got.Failures) != 1 {
		t.Fatalf("writable source: %+v %v", got, err)
	}
}

func TestCatalogueManagedRevisionAndOperationState(t *testing.T) {
	const fixture = `{"tokens":{"account_id":"synthetic-owner","access_token":"synthetic-access","id_token":"synthetic-id","refresh_token":""},"_cq":{"version":1,"account_key":"acct_fixture","candidate_id":"cand_fixture","lineage_id":"lineage_fixture","generation":1,"revision":"7be84dd950cd8964793320b5d1a5a1b85dc36e954714fbbb3fcd9cdd58d989f6","provenance":"cq_oauth","refresh_ownership":"cq_owned_never_exported","operation_state":"ready"}}`
	var value map[string]any
	if err := json.Unmarshal([]byte(fixture), &value); err != nil {
		t.Fatal(err)
	}
	if ready, err := managedReady(value); !ready || err != nil {
		t.Fatalf("Go revision rejected: %v", err)
	}
	metadata := value["_cq"].(map[string]any)
	for _, state := range []string{"removing", "refreshing", "rotation_uncertain", "activation_pending", "unknown"} {
		metadata["operation_state"], metadata["revision"] = state, ""
		encoded, _ := json.Marshal(value)
		digest := sha256.Sum256(encoded)
		metadata["revision"] = hex.EncodeToString(digest[:])
		before, _ := json.Marshal(value)
		ready, err := managedReady(value)
		if ready || (state == "removing") != (err == nil) {
			t.Fatalf("state %s: ready=%v err=%v", state, ready, err)
		}
		after, _ := json.Marshal(value)
		if !bytes.Equal(before, after) {
			t.Fatal("revision validation changed document")
		}
	}
	metadata["revision"] = strings.Repeat("0", 64)
	if _, err := managedReady(value); err == nil {
		t.Fatal("accepted revision tampering")
	}
	if _, err := managedReady(nil); err == nil {
		t.Fatal("accepted null document")
	}
}

func TestCatalogueGetOwnsIdentityAndBoundsResponses(t *testing.T) {
	for _, agent := range []string{"", "verified-native-agent"} {
		c := &CatalogueClient{HTTP: catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("ChatGPT-Account-Id") != "owner" || r.Header.Get("X-Openai-Account-Routing-Override") != "" {
				t.Fatal("incoming identity was retained")
			}
			want := agent
			if want == "" {
				want = NativeUserAgent
			}
			if r.Header.Get("User-Agent") != want || r.Header.Get("Accept-Encoding") != "identity" {
				t.Fatal("incorrect native request headers")
			}
			return catalogueTestResponse(200, map[string]any{}), nil
		})}
		headers := http.Header{"authorization": {"Bearer foreign"}, "chatgpt-account-id": {"foreign"}, "x-openai-account-routing-override": {"foreign"}}
		if agent != "" {
			headers["user-agent"] = []string{agent}
		}
		before := headers.Clone()
		if _, err := c.Get(context.Background(), Account{AccountID: "owner", Token: "synthetic"}, "/backend-api/tbo/primary", headers); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(headers, before) {
			t.Fatal("request headers were changed")
		}
	}
	for _, test := range []struct {
		status   int
		body     string
		wantHTTP bool
	}{{403, `private-error`, true}, {200, `[]`, false}, {200, strings.Repeat("x", (1<<20)+1), false}} {
		c := &CatalogueClient{HTTP: catalogueTestDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})}
		_, err := c.Get(context.Background(), Account{}, "/backend-api/tbo", nil)
		var status *CatalogueHTTPError
		if err == nil || errors.As(err, &status) != test.wantHTTP || strings.Contains(err.Error(), "private-error") {
			t.Fatalf("response validation failed: %v", err)
		}
	}
	c := &CatalogueClient{}
	for _, path := range []string{"https://foreign.invalid/", "//foreign.invalid/", "relative", "/x#fragment"} {
		if _, err := c.Get(context.Background(), Account{}, path, nil); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}

func TestCatalogueDiscoveryStableIdentityAndSafeAvailability(t *testing.T) {
	c := catalogueTestClient(t)
	for _, owner := range []string{"second", "none", "restricted", "expired"} {
		expiry := c.now().Unix() + 3600
		if owner == "expired" {
			expiry = 0
		}
		catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, owner+".auth.json"), catalogueTestDocument(owner, expiry))
	}
	rootPrefix := "root-"
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		owner := r.Header.Get("ChatGPT-Account-Id")
		if owner == "none" {
			return catalogueTestResponse(200, map[string]any{"selection": nil}), nil
		}
		if owner == "restricted" {
			return catalogueTestResponse(403, map[string]any{}), nil
		}
		return catalogueTestResponse(200, catalogueTestPrimary(owner, "Same", rootPrefix+owner)), nil
	})
	first, err := c.Discover(context.Background())
	if err != nil || len(first.Dots) != 2 || len(first.Accounts) != 5 {
		t.Fatalf("discovery counts: %+v %v", first, err)
	}
	statuses := map[string]string{}
	for _, outcome := range first.Accounts {
		statuses[outcome.OwnerAccountID] = outcome.Status
		if outcome.OwnerAccountID == "restricted" && outcome.HTTPStatus != 403 {
			t.Fatal("missing safe HTTP status")
		}
	}
	if statuses["none"] != "no_dot" || statuses["restricted"] != "unavailable" || statuses["expired"] != "unavailable" {
		t.Fatalf("availability: %v", statuses)
	}
	rootPrefix = "changed-"
	second, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, dot := range first.Dots {
		if dot.Handle != second.Dots[i].Handle || dot.RootThreadID == second.Dots[i].RootThreadID {
			t.Fatal("handle followed mutable root")
		}
	}
	encoded, _ := json.Marshal(first)
	if !strings.Contains(string(encoded), `"handle":"`+first.Dots[0].Handle+`"`) {
		t.Fatal("discovered handle was omitted from JSON")
	}
	if strings.Contains(string(encoded), "signature") || strings.Contains(string(encoded), "synthetic-never-used") {
		t.Fatal("credential leaked in discovery")
	}
	c.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
		return catalogueTestResponse(200, catalogueTestPrimary("shared", "Same", "shared-root")), nil
	})
	if _, err := c.Discover(context.Background()); err == nil {
		t.Fatal("accepted profile ownership collision")
	}
}

func TestCataloguePaginationAndPrimaryOwnership(t *testing.T) {
	c := catalogueTestClient(t)
	page := 0
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		page++
		if r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("include_room_preview") != "false" {
			t.Fatal("missing bounded query")
		}
		if page == 1 {
			return catalogueTestResponse(200, map[string]any{"items": []any{map[string]any{"id": "one"}}, "cursor": "next"}), nil
		}
		if r.URL.Query().Get("cursor") != "next" {
			t.Fatal("cursor omitted")
		}
		return catalogueTestResponse(200, map[string]any{"items": []any{map[string]any{"id": "two"}}}), nil
	})
	items, err := c.Catalogue(context.Background(), Account{}, nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("pagination: %v %v", items, err)
	}
	for _, mode := range []string{"repeat", "limit", "invalid"} {
		page = 0
		c.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
			page++
			cursor := any("same")
			if mode == "limit" {
				cursor = fmt.Sprint(page)
			}
			if mode == "invalid" {
				cursor = true
			}
			return catalogueTestResponse(200, map[string]any{"items": []any{}, "cursor": cursor}), nil
		})
		if _, err := c.Catalogue(context.Background(), Account{}, nil); err == nil {
			t.Fatalf("accepted %s pagination", mode)
		}
		if mode == "limit" && page != 20 {
			t.Fatalf("page limit = %d", page)
		}
	}
	c.Settings.Dots["System"] = Dot{OwnerAlias: c.Settings.SystemAlias, ProfileID: "profile-system", RootThreadID: "configured-root"}
	c.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
		return catalogueTestResponse(200, catalogueTestPrimary("system", "System", "current-root")), nil
	})
	account := Account{Alias: c.Settings.SystemAlias, AccountID: "system"}
	roots, err := c.PrimaryRoots(context.Background(), map[string]Account{account.Alias: account}, nil)
	if err != nil || roots["current-root"].OwnerAlias != account.Alias {
		t.Fatalf("current roots: %v %v", roots, err)
	}
	if _, err := c.PrimaryRoots(context.Background(), map[string]Account{}, nil); err == nil {
		t.Fatal("accepted missing primary")
	}
}

func TestCatalogueFixtureFiltersProfilesAndRejectsResourceCollision(t *testing.T) {
	c := catalogueTestClient(t)
	secondary := catalogueTestDocument("secondary", c.now().Unix()+3600)
	catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, "secondary.auth.json"), secondary)
	for _, owner := range []string{"system", "secondary"} {
		c.Settings.Dots[owner] = Dot{OwnerAlias: AccountAlias(owner), ProfileID: "profile-" + owner, RootThreadID: "root-" + owner}
	}
	shared := false
	headers := http.Header{
		"authorization":                     {"Bearer native"},
		"chatgpt-account-id":                {"system"},
		"Cookie":                            {"native-session=synthetic"},
		"x-openai-expected-account-id":      {"system"},
		"X-OpenAI-Orbit-Selection":          {"native-workspace"},
		"x-openai-account-routing-override": {"system"},
		"Accept-Language":                   {"en-GB"},
		"x-client-version":                  {"qualified-native-client"},
	}
	originalHeaders := headers.Clone()
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("ChatGPT-Account-Id") != "secondary" || r.Header.Get("Authorization") != "Bearer "+secondary["tokens"].(map[string]any)["access_token"].(string) {
			t.Fatal("catalogue request did not use secondary identity")
		}
		for _, key := range []string{"Cookie", "X-OpenAI-Expected-Account-Id", "X-OpenAI-Orbit-Selection", "X-OpenAI-Account-Routing-Override"} {
			if r.Header.Get(key) != "" {
				t.Fatalf("catalogue request retained native authority header %s", key)
			}
		}
		if r.Header.Get("Accept-Language") != "en-GB" || r.Header.Get("X-Client-Version") != "qualified-native-client" {
			t.Fatal("catalogue request dropped permitted language/client headers")
		}
		root := "root-secondary"
		if shared {
			root = "root-system"
		}
		return catalogueTestResponse(200, map[string]any{"items": []any{map[string]any{"id": "profile-secondary", "display_name": "Secondary", "active_root_thread_id": root}, map[string]any{"id": "unselected", "display_name": "Other"}}}), nil
	})
	primary := []map[string]any{{"id": "profile-system", "display_name": "System", "active_root_thread_id": "root-system"}}
	got, err := c.Fixture(context.Background(), headers, primary)
	if err != nil || len(got.Profiles) != 2 || len(got.Summary) != 2 || got.Owners["root-secondary"].AccountID != "secondary" {
		t.Fatalf("fixture ownership: %+v %v", got, err)
	}
	if got.Summary[1]["requested_secondary"] != true {
		t.Fatal("secondary request missing")
	}
	if !reflect.DeepEqual(headers, originalHeaders) {
		t.Fatal("fixture changed native request headers")
	}
	shared = true
	if _, err := c.Fixture(context.Background(), headers, primary); err == nil {
		t.Fatal("accepted resource collision")
	}
}
