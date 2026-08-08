package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
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
		"auth_mode":      "chatgpt",
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
	// Automatic discovery keeps the live system credential authoritative.
	if accts[0].FilePath != "/fake/home/.codex/auth.json" {
		t.Errorf("accts[0].FilePath = %q, want live system path", accts[0].FilePath)
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

func TestAccountsMutationRequiresCoordinator(t *testing.T) {
	mgr := &Accounts{FS: newFakeFS()}
	if _, err := mgr.Switch(context.Background(), "user@test.com"); err == nil {
		t.Fatal("Switch error = nil")
	}
	if err := mgr.Remove(context.Background(), "user@test.com"); err == nil {
		t.Fatal("Remove error = nil")
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

func TestAccountsSwitchThroughCoordinatorActivatesExactManagedCandidate(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("target@test.com", "acct-1", "user-1", "plus")
	ref, _, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	mgr := &Accounts{FS: fs, Admin: coordinator}
	account, err := mgr.Switch(context.Background(), "target@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if !account.Active || account.Email != "target@test.com" {
		t.Fatalf("account = %+v", account)
	}
	active, err := coordinator.Activator.Active(context.Background())
	if err != nil || !active.Present {
		t.Fatalf("active = %+v, %v", active, err)
	}
}

func TestAccountsRemoveThroughCoordinatorDeactivatesExactActiveAccount(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("target@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	if _, err := coordinator.Activate(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	mgr := &Accounts{FS: fs, Admin: coordinator}
	if err := mgr.Remove(context.Background(), "target@test.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.files["/fake/home/.codex/auth.json"]; ok {
		t.Fatal("active system credential remains")
	}
	if _, ok := fs.files[ref.path]; ok {
		t.Fatal("managed candidate remains")
	}
}

func TestAccountsCoordinatorSupportsRefreshSuspendedLegacyCandidate(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	jwt := fakeCodexJWT("legacy@test.com", "acct-legacy", "user-legacy", "plus")
	path := "/fake/home/.codex/accounts/user-legacy::acct-legacy.auth.json"
	fs.files[path] = codexAuthJSON("legacy", "acct-legacy", jwt)
	fs.modes[path] = 0o600
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "user-legacy::acct-legacy.auth.json"}},
	}
	mgr := &Accounts{FS: fs, Admin: coordinator}
	if _, err := mgr.Switch(context.Background(), "legacy@test.com"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Remove(context.Background(), "legacy@test.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.files[path]; ok {
		t.Fatal("legacy managed candidate remains")
	}
}

func TestAccountsSwitchRefusesAmbiguousEmail(t *testing.T) {
	fs := newFakeFS()
	jwt1 := fakeCodexJWT("same@test.com", "acct-1", "user-1", "plus")
	jwt2 := fakeCodexJWT("same@test.com", "acct-2", "user-2", "pro")
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = codexAuthJSON("tok-1", "acct-1", jwt1)
	fs.files["/fake/home/.codex/accounts/user-2::acct-2.auth.json"] = codexAuthJSON("tok-2", "acct-2", jwt2)
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {
			{name: "user-1::acct-1.auth.json"},
			{name: "user-2::acct-2.auth.json"},
		},
	}

	mgr := &Accounts{FS: fs}
	if _, err := mgr.Switch(context.Background(), "same@test.com"); err == nil {
		t.Fatal("Switch error = nil, want ambiguous email rejection")
	}
	if _, ok := fs.files["/fake/home/.codex/auth.json"]; ok {
		t.Fatal("ambiguous switch wrote system auth")
	}
}

func TestDiscoverAccountsIncludesRefreshMetadata(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("refresh@test.com", "acct-refresh", "user-refresh", "plus")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok-refresh", "acct-refresh", jwt)

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	refreshField := reflect.ValueOf(accts[0]).FieldByName("RefreshToken")
	if !refreshField.IsValid() {
		t.Fatal("RefreshToken field missing")
	}
	if got := refreshField.String(); got != "ref-tok" {
		t.Fatalf("RefreshToken = %q, want ref-tok", got)
	}

	expiresField := reflect.ValueOf(accts[0]).FieldByName("ExpiresAt")
	if !expiresField.IsValid() {
		t.Fatal("ExpiresAt field missing")
	}
	if got := expiresField.Int(); got != 1774076490000 {
		t.Fatalf("ExpiresAt = %d, want %d", got, int64(1774076490000))
	}
}

func TestPersistCodexAccountPreservesUnknownFields(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	path := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	fs.files[path] = []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-test","extra":{"keep":true},"tokens":{"access_token":"old-tok","refresh_token":"old-ref","id_token":"` + jwt + `","account_id":"acct-1"},"last_refresh":"2026-03-21T06:56:43.237634Z"}`)

	acct, ok := parseAccountFile(fs, path)
	if !ok {
		t.Fatal("parseAccountFile returned false")
	}
	acct.AccessToken = "new-tok"
	acct.RefreshToken = "new-ref"

	if err := PersistCodexAccount(fs, acct, "/fake/home"); err != nil {
		t.Fatalf("PersistCodexAccount: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(fs.files[path], &doc); err != nil {
		t.Fatalf("unmarshal updated file: %v", err)
	}
	if got := doc["OPENAI_API_KEY"]; got != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %#v, want sk-test", got)
	}
	extra, ok := doc["extra"].(map[string]any)
	if !ok || extra["keep"] != true {
		t.Fatalf("extra.keep = %#v, want true", doc["extra"])
	}
	tokens, ok := doc["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v, want object", doc["tokens"])
	}
	if got := tokens["access_token"]; got != "new-tok" {
		t.Fatalf("access_token = %#v, want new-tok", got)
	}
	if got := tokens["refresh_token"]; got != "new-ref" {
		t.Fatalf("refresh_token = %#v, want new-ref", got)
	}
}

func TestPersistCodexAccountOverwritesHigherStoredExpiresAt(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	path := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	// Pre-existing file has a large cq_expires_at (far future).
	fs.files[path] = []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"old-tok","refresh_token":"old-ref","id_token":"` + jwt + `","account_id":"acct-1"},"cq_expires_at":9999999999999}`)

	acct, ok := parseAccountFile(fs, path)
	if !ok {
		t.Fatal("parseAccountFile returned false")
	}
	// Assign a lower (newer real) ExpiresAt — simulates a refresh that returned
	// a shorter-lived token (e.g. expires_in from server).
	const lowerExpiresAt = int64(1000000)
	acct.AccessToken = "new-tok"
	acct.ExpiresAt = lowerExpiresAt

	if err := PersistCodexAccount(fs, acct, "/fake/home"); err != nil {
		t.Fatalf("PersistCodexAccount: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(fs.files[path], &doc); err != nil {
		t.Fatalf("unmarshal updated file: %v", err)
	}
	// cq_expires_at must reflect the new (lower) value, not the old higher one.
	got, ok := doc["cq_expires_at"].(float64)
	if !ok {
		t.Fatalf("cq_expires_at = %#v, want numeric", doc["cq_expires_at"])
	}
	if int64(got) != lowerExpiresAt {
		t.Fatalf("cq_expires_at = %d, want %d (lower value must overwrite higher stored value)", int64(got), lowerExpiresAt)
	}
	tokens, ok := doc["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v, want object", doc["tokens"])
	}
	if got := tokens["access_token"]; got != "new-tok" {
		t.Fatalf("access_token = %#v, want new-tok", got)
	}
}

func TestDiscoverAccountsPrefersLiveCopyForActiveDuplicate(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("tok-old", "acct-1", jwt)
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = codexAuthJSON("tok-new", "acct-1", jwt)
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {
			{name: "user-1::acct-1.auth.json"},
		},
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if !accts[0].IsActive {
		t.Fatal("expected deduped account to stay active")
	}
	if got := accts[0].AccessToken; got != "tok-old" {
		t.Fatalf("AccessToken = %q, want live token", got)
	}
	if got := accts[0].FilePath; got != "/fake/home/.codex/auth.json" {
		t.Fatalf("FilePath = %q, want live system path", got)
	}
}

func TestDiscoverAccountsMalformedClaimsDoNotCollapseAtEmptyRecordKey(t *testing.T) {
	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("system-token", "", "")
	fs.files["/fake/home/.codex/accounts/first.auth.json"] = codexAuthJSON("managed-token", "", "")
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "first.auth.json"}},
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 2 {
		t.Fatalf("len(accts) = %d, want 2 distinct malformed candidates", len(accts))
	}
	for i, acct := range accts {
		if acct.RecordKey != "" {
			t.Fatalf("accts[%d].RecordKey = %q, want empty", i, acct.RecordKey)
		}
	}
}

func TestPersistCodexAccountNeverMirrorsManagedRefreshToSystemAuth(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	managedPath := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	fs.files[managedPath] = codexAuthJSON("managed-old", "acct-1", jwt)
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("system-current", "acct-1", jwt)

	acct, ok := parseAccountFile(fs, managedPath)
	if !ok {
		t.Fatal("parseAccountFile returned false")
	}
	acct.IsActive = true // stale discovery metadata must not grant write authority.
	acct.AccessToken = "managed-new"

	if err := PersistCodexAccount(fs, acct, "/fake/home"); err != nil {
		t.Fatalf("PersistCodexAccount: %v", err)
	}
	var system codexAuthFile
	if err := json.Unmarshal(fs.files["/fake/home/.codex/auth.json"], &system); err != nil {
		t.Fatalf("parse system auth: %v", err)
	}
	if got := system.Tokens.AccessToken; got != "system-current" {
		t.Fatalf("system access token changed to %q", got)
	}
}

func TestPersistCodexAccountRejectsSystemAuthPath(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	before := codexAuthJSON("system-current", "acct-1", jwt)
	fs.files[systemPath] = before

	acct, ok := parseAccountFile(fs, systemPath)
	if !ok {
		t.Fatal("parseAccountFile returned false")
	}
	acct.AccessToken = "replacement"

	if err := PersistCodexAccount(fs, acct, "/fake/home"); err == nil {
		t.Fatal("PersistCodexAccount() error = nil, want system auth rejection")
	}
	if got := string(fs.files[systemPath]); got != string(before) {
		t.Fatal("system auth changed after rejected persistence")
	}
}
