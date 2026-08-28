package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
)

// fakeJWT builds a JWT with the given payload (no signature verification needed).
func fakeJWT(payload any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	body, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + encoded + ".fakesig"
}

// fakeFS is a test FileSystem implementation backed by an in-memory map.
type fakeFS struct {
	files      map[string][]byte
	dirEntries map[string][]fakeDirEntry
	readDirErr map[string]error
	homeDirErr error
	writeErr   error
	renameErr  error
}

// fakeDirEntry implements os.DirEntry for tests.
type fakeDirEntry struct {
	name  string
	isDir bool
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.isDir }
func (e fakeDirEntry) Type() os.FileMode          { return 0 }
func (e fakeDirEntry) Info() (os.FileInfo, error) { return nil, nil }

func newFakeFS() *fakeFS {
	return &fakeFS{files: make(map[string][]byte)}
}

type failSecondHomeDirFS struct {
	*fakeFS
	homeDirCalls atomic.Int32
}

func (f *fakeFS) UserHomeDir() (string, error) {
	if f.homeDirErr != nil {
		return "", f.homeDirErr
	}
	return "/fake/home", nil
}

func (f *failSecondHomeDirFS) UserHomeDir() (string, error) {
	if f.homeDirCalls.Add(1) > 1 {
		return "", os.ErrPermission
	}
	return f.fakeFS.UserHomeDir()
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	data, ok := f.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (f *fakeFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[name] = data
	return nil
}

func (f *fakeFS) Rename(oldpath, newpath string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	data, ok := f.files[oldpath]
	if !ok {
		return os.ErrNotExist
	}
	f.files[newpath] = data
	delete(f.files, oldpath)
	return nil
}

type fakeFileInfo struct {
	name string
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Now() }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(name string) (os.FileInfo, error) {
	_, ok := f.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return fakeFileInfo{name: name}, nil
}

func (f *fakeFS) Remove(name string) error {
	if _, ok := f.files[name]; !ok {
		return os.ErrNotExist
	}
	delete(f.files, name)
	return nil
}

func (f *fakeFS) MkdirAll(_ string, _ os.FileMode) error { return nil }

func (f *fakeFS) ReadDir(name string) ([]os.DirEntry, error) {
	if err := f.readDirErr[name]; err != nil {
		return nil, err
	}
	if f.dirEntries == nil {
		return nil, nil
	}
	entries, ok := f.dirEntries[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	out := make([]os.DirEntry, len(entries))
	for i, e := range entries {
		out[i] = e
	}
	return out, nil
}

// urlRewriter rewrites request URLs to a local httptest.Server.
type urlRewriter struct {
	client  *http.Client
	baseURL string
}

type staticCredentialInventory struct{ inventory Inventory }

func (s staticCredentialInventory) List(context.Context) (Inventory, error) { return s.inventory, nil }

func TestRoutingAccountsPreserveOpaqueKeyAndIdentity(t *testing.T) {
	provider := &Provider{inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{
		{Key: "route-a", Identity: AccountIdentity{AccountID: "account-a", Email: "a@example.com"}, Active: true},
		{Key: "route-b", Identity: AccountIdentity{AccountID: "account-b", Email: "b@example.com"}},
	}}}}

	accounts, err := provider.RoutingAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("routing accounts = %d, want 2", len(accounts))
	}
	if accounts[0].Key != "route-a" || accounts[0].AccountID != "account-a" || accounts[0].Email != "a@example.com" || !accounts[0].Active {
		t.Fatalf("first routing account = %#v", accounts[0])
	}
}

type staticSecretResolver struct{ material CredentialMaterial }

func (s staticSecretResolver) Resolve(context.Context, CandidateRef) (CredentialMaterial, error) {
	return s.material, nil
}

func (s staticSecretResolver) ResolveExact(context.Context, PlannedCandidate) (CredentialMaterial, error) {
	return s.material, nil
}

type fixedHomeDurableFS struct {
	fsutil.OSFileSystem
	home string
}

func (f fixedHomeDurableFS) UserHomeDir() (string, error) { return f.home, nil }

func (u *urlRewriter) Do(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = u.baseURL[len("http://"):]
	return u.client.Do(req)
}

// validAuthJSON returns a well-formed auth.json payload.
func validAuthJSON(accessToken, refreshToken, idToken, accountID string) []byte {
	m := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"id_token":      idToken,
			"account_id":    accountID,
		},
	}
	b, _ := json.Marshal(m)
	return b
}

// happyUsageBody is a minimal valid usage API response.
const happyUsageBody = `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":20.0,"limit_window_seconds":18000,"reset_at":1774051200}}}`

func TestFetchMissingAuthFile(t *testing.T) {
	fs := newFakeFS()
	// No auth.json written — ReadFile will return os.ErrNotExist.
	p := &Provider{client: http.DefaultClient, fs: fs}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("status = %q, want %q", results[0].Status, quota.StatusError)
	}
	if results[0].Error == nil {
		t.Fatal("expected non-nil Error info")
	}
	if results[0].Error.Code != "not_configured" {
		t.Errorf("error code = %q, want not_configured", results[0].Error.Code)
	}
}

func TestFetchStaleDefaultCredentialCoordinatorReturnsFetchErrorWithoutDispatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	fs := fixedHomeDurableFS{home: home}
	authDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(authDir, "auth.json")
	original := validAuthJSON("stale-access", "", "", "stale-account")
	if err := os.WriteFile(authPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	controlPath := DefaultCredentialControlPath(home)
	if err := os.MkdirAll(filepath.Dir(controlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	staleEndpoint := []byte("stale endpoint")
	if err := os.WriteFile(controlPath, staleEndpoint, 0o600); err != nil {
		t.Fatal(err)
	}

	var usageCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		usageCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" {
		t.Fatalf("results = %+v, want one fetch_error result", results)
	}
	if got := results[0].Error.Message; !strings.Contains(got, "credential coordinator") || strings.Contains(got, home) {
		t.Fatalf("message = %q, want privacy-safe coordinator failure", got)
	}
	if got := usageCalls.Load(); got != 0 {
		t.Fatalf("usage calls = %d, want no stale credential dispatch", got)
	}
	if got, err := os.ReadFile(authPath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("system auth changed: data equal = %v, error = %v", bytes.Equal(got, original), err)
	}
	if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, staleEndpoint) {
		t.Fatalf("stale endpoint changed: data equal = %v, error = %v", bytes.Equal(got, staleEndpoint), err)
	}
}

func TestFetchParseError(t *testing.T) {
	// DiscoverAccounts silently skips unparseable files, so invalid JSON
	// in auth.json results in not_configured (no accounts found).
	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = []byte(`not valid json`)
	p := &Provider{client: http.DefaultClient, fs: fs}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("status = %q, want %q", results[0].Status, quota.StatusError)
	}
	if results[0].Error == nil {
		t.Fatal("expected non-nil Error info")
	}
	if results[0].Error.Code != "not_configured" {
		t.Errorf("error code = %q, want not_configured", results[0].Error.Code)
	}
}

func TestFetchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(happyUsageBody))
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON(
		"tok-abc",
		"ref-abc",
		fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"),
		"acct-1",
	)

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Errorf("status = %q, want %q", results[0].Status, quota.StatusOK)
	}
	if results[0].Plan != "plus" {
		t.Errorf("plan = %q, want plus", results[0].Plan)
	}
	if !results[0].Active {
		t.Error("expected Active=true for auth.json account")
	}
}

func TestFetchResolvesMetadataOnlyExternalCandidate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer external-secret" || r.Header.Get("ChatGPT-Account-ID") != "acct-1" {
			t.Fatalf("credential headers = %q/%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(happyUsageBody))
	}))
	defer srv.Close()

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     newFakeFS(),
		inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{
			Key: "account-1", Identity: AccountIdentity{AccountID: "acct-1", UserID: "user-1", Email: "user@example.test"}, Routable: true,
			Candidates: []CredentialCandidate{{
				Ref: CandidateRef{AccountKey: "account-1", CandidateID: "external-1"}, Revision: "revision-1",
				Source: SourceExternal, AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
			}},
		}}}},
		secrets: staticSecretResolver{material: testCredentialMaterial(
			AccountIdentity{AccountID: "acct-1", UserID: "user-1", Email: "user@example.test"},
			"external-secret",
		)},
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() || results[0].AccountID != "acct-1" {
		t.Fatalf("results = %+v", results)
	}
}

func TestNewCredentialCoordinatorIncludesDefaultCodexBarSource(t *testing.T) {
	coordinator, err := NewCredentialCoordinator(testManagedStore(t, newDurableFakeFS()))
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinator.ExternalSources) != 1 || coordinator.ExternalSources[0].Name() != codexBarSourceName {
		t.Fatalf("external sources = %+v", coordinator.ExternalSources)
	}
}

func TestDiscoverAccountsUsesCoordinatorInventoryForExternalSourceStates(t *testing.T) {
	tests := []struct {
		name      string
		inventory Inventory
		wantID    string
	}{
		{
			name: "present",
			inventory: Inventory{
				Accounts: []LogicalAccount{{
					Identity: AccountIdentity{AccountID: "external-account", Email: "external@example.test", PlanType: "plus"},
				}},
				ExternalSources: []ExternalSourceStatus{{Name: "codexbar", CandidateCount: 1}},
			},
			wantID: "external-account",
		},
		{
			name: "absent",
			inventory: Inventory{
				Accounts: []LogicalAccount{{
					Identity: AccountIdentity{AccountID: "system-account"},
				}},
				ExternalSources: []ExternalSourceStatus{{Name: "codexbar", ErrorCode: "unavailable"}},
			},
			wantID: "system-account",
		},
		{
			name: "invalid",
			inventory: Inventory{
				Accounts: []LogicalAccount{{
					Identity: AccountIdentity{AccountID: "managed-account"},
				}},
				ExternalSources: []ExternalSourceStatus{{Name: "codexbar", ErrorCode: "invalid"}},
			},
			wantID: "managed-account",
		},
		{
			name: "zero sources",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Identity: AccountIdentity{AccountID: "local-account"},
			}}},
			wantID: "local-account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{
				fs:        newFakeFS(),
				inventory: staticCredentialInventory{inventory: tt.inventory},
			}

			accounts, err := p.DiscoverAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(accounts) != 1 || accounts[0].AccountID != tt.wantID {
				t.Fatalf("accounts = %+v, want coordinator account %q", accounts, tt.wantID)
			}
		})
	}
}

func TestFetchMultiAccountActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(happyUsageBody))
	}))
	defer srv.Close()

	idToken1 := fakeJWT(map[string]any{
		"email": "active@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
		},
	})
	idToken2 := fakeJWT(map[string]any{
		"email": "other@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-2",
			"chatgpt_account_id": "acct-2",
		},
	})

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("tok-active", "ref-active", idToken1, "acct-1")
	fs.files["/fake/home/.codex/accounts/other.auth.json"] = validAuthJSON("tok-other", "ref-other", idToken2, "acct-2")
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "other.auth.json"}},
	}

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	activeCount := 0
	for _, r := range results {
		if r.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("active count = %d, want 1", activeCount)
	}
	// The first result corresponds to auth.json (the active account).
	if !results[0].Active {
		t.Error("expected first result (auth.json) to be active")
	}
	if results[1].Active {
		t.Error("expected second result (accounts/ file) to not be active")
	}
}

func TestFetch401ReturnsAuthExpiredNoRefreshToken(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	fs := newFakeFS()
	// Empty refresh token — no refresh should be attempted.
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON(
		"old-tok",
		"",
		fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"),
		"acct-1",
	)

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusError {
		t.Errorf("status = %q, want error", results[0].Status)
	}
	if results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Errorf("error code = %v, want auth_expired", results[0].Error)
	}
	// Only one HTTP call — no refresh attempted (no refresh token).
	if c := callCount.Load(); c != 1 {
		t.Errorf("callCount = %d, want 1 (no refresh attempted)", c)
	}
}

func TestFetchTriesSecondCandidateWithinLogicalAccountAfter401(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer system-stale" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(happyUsageBody))
	}))
	defer srv.Close()

	idToken := fakeJWT(map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id": "user-1", "chatgpt_account_id": "acct-1",
		},
	})
	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("system-stale", "", idToken, "acct-1")
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = validAuthJSON("managed-fresh", "", idToken, "acct-1")
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "user-1::acct-1.auth.json"}},
	}
	p := &Provider{client: &urlRewriter{client: srv.Client(), baseURL: srv.URL}, fs: fs}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != quota.StatusOK {
		t.Fatalf("results = %+v, want one usable logical row", results)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want two same-identity candidates", got)
	}
}

func TestFetch401WithRefreshTokenNeverCallsOAuthOrWritesCredentials(t *testing.T) {
	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	idToken := fakeJWT(map[string]any{
		"email": "synthetic@example.invalid",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "synthetic-user",
			"chatgpt_account_id": "synthetic-account",
		},
	})
	original := validAuthJSON("synthetic-access", "synthetic-refresh", idToken, "synthetic-account")
	fs.files["/fake/home/.codex/auth.json"] = append([]byte(nil), original...)

	p := &Provider{client: &urlRewriter{client: srv.Client(), baseURL: srv.URL}, fs: fs}
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want one auth_expired result", results)
	}
	if got := usageCalls.Load(); got != 1 {
		t.Fatalf("usage calls = %d, want 1", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
	if got := fs.files["/fake/home/.codex/auth.json"]; !bytes.Equal(got, original) {
		t.Fatal("system auth changed during quota fetch")
	}
}

func TestFetch401RequestsManagedRefreshBrokerThenRetriesUsage(t *testing.T) {
	coordinator, fs, _, _ := testRefreshRecord(t)
	var exchanges atomic.Int32
	coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
		exchanges.Add(1)
		if token != "managed-refresh" {
			t.Fatalf("refresh token mismatch")
		}
		return successfulRefresh(ctx, token)
	}
	var usageCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer refreshed-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(happyUsageBody))
	}))
	defer srv.Close()
	p := &Provider{
		client:        &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:            fs,
		inventory:     coordinator,
		secrets:       coordinator,
		refreshBroker: coordinator,
	}
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != quota.StatusOK {
		t.Fatalf("results = %+v", results)
	}
	if usageCalls.Load() != 2 || exchanges.Load() != 1 {
		t.Fatalf("usage calls = %d, exchanges = %d", usageCalls.Load(), exchanges.Load())
	}
}

func TestFetch401RefreshesAndRetriesUsage(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	idToken := fakeJWT(map[string]any{
		"email": "refresh@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(happyUsageBody))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if got := r.FormValue("grant_type"); got != "refresh_token" {
				t.Fatalf("grant_type = %q, want refresh_token", got)
			}
			if got := r.FormValue("refresh_token"); got != "old-ref" {
				t.Fatalf("refresh_token = %q, want old-ref", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"id_token":      idToken,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", idToken, "acct-1")

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q", results[0].Status, quota.StatusOK)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
	if got := string(fs.files["/fake/home/.codex/auth.json"]); !strings.Contains(got, "new-tok") || !strings.Contains(got, "new-ref") {
		t.Fatalf("auth.json was not updated with refreshed tokens: %s", got)
	}
}

func TestFetch401RefreshWithoutIDTokenPersistsRediscoverableExpiry(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	now := time.Now()
	expiredIDToken := fakeJWT(map[string]any{
		"email": "refresh@example.com",
		"exp":   float64(now.Add(-time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(happyUsageBody))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"expires_in":    7200,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", expiredIDToken, "acct-1")

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q", results[0].Status, quota.StatusOK)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if got := accts[0].ExpiresAt; got <= now.UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want > %d after refresh without id_token", got, now.UnixMilli())
	}
}

func TestFetch401RefreshWithIDTokenMissingExpFallsBackToExpiresIn(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	now := time.Now()
	expiredIDToken := fakeJWT(map[string]any{
		"email": "refresh@example.com",
		"exp":   float64(now.Add(-time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})
	refreshedIDToken := fakeJWT(map[string]any{
		"email": "refresh@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(happyUsageBody))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"id_token":      refreshedIDToken,
				"expires_in":    7200,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", expiredIDToken, "acct-1")

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q", results[0].Status, quota.StatusOK)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}

	accts := DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if got := accts[0].ExpiresAt; got <= now.UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want > %d after refresh with id_token missing exp", got, now.UnixMilli())
	}
}

func TestFetch401ReturnsAuthExpiredWithIdentity(t *testing.T) {
	idToken := fakeJWT(map[string]any{
		"email": "expired@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", idToken, "acct-1")

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("error = %+v, want auth_expired", results[0].Error)
	}
	if results[0].Email != "expired@example.com" {
		t.Fatalf("email = %q, want expired@example.com", results[0].Email)
	}
	if results[0].AccountID != "acct-1" {
		t.Fatalf("accountID = %q, want acct-1", results[0].AccountID)
	}
	if usageCalls.Load() != 1 {
		t.Fatalf("usageCalls = %d, want 1", usageCalls.Load())
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("refreshCalls = %d, want 0", refreshCalls.Load())
	}
}

func TestFetch401RetryPreservesAPIError(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	idToken := fakeJWT(map[string]any{
		"email": "retry@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"id_token":      idToken,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", idToken, "acct-1")

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Error == nil || results[0].Error.Code != "api_error" || results[0].Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error = %+v, want api_error with 429", results[0].Error)
	}
	if results[0].Email != "retry@example.com" {
		t.Fatalf("email = %q, want retry@example.com", results[0].Email)
	}
	if results[0].AccountID != "acct-1" {
		t.Fatalf("accountID = %q, want acct-1", results[0].AccountID)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
}

func TestFetch401RefreshSucceedsWhenPersistFails(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	// When PersistCodexAccount cannot write (writeErr / renameErr on fakeFS),
	// the refreshed access token must still be used for the retry and the
	// result must be usable quota — persistence failure must not break the
	// immediate fetch.
	idToken := fakeJWT(map[string]any{
		"email": "persist-fail@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-pf",
			"chatgpt_account_id": "acct-pf",
			"chatgpt_plan_type":  "plus",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(happyUsageBody))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"id_token":      idToken,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", idToken, "acct-pf")
	// Simulate a filesystem where writes always fail (e.g. disk full / read-only).
	fs.writeErr = os.ErrPermission

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	// Persistence failure must not prevent usable quota from being returned.
	if results[0].Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q — persistence failure should not break the immediate retry", results[0].Status, quota.StatusOK)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
}

func TestFetch401RefreshesAndRetriesWhenHomeDirLookupFails(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	idToken := fakeJWT(map[string]any{
		"email": "refresh@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})

	var usageCalls atomic.Int32
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			if got := r.Header.Get("Authorization"); got == "Bearer new-tok" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(happyUsageBody))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-tok",
				"refresh_token": "new-ref",
				"id_token":      idToken,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	baseFS := newFakeFS()
	baseFS.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "old-ref", idToken, "acct-1")
	fs := &failSecondHomeDirFS{fakeFS: baseFS}

	p := &Provider{
		client: &urlRewriter{client: srv.Client(), baseURL: srv.URL},
		fs:     fs,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Status != quota.StatusOK {
		t.Fatalf("status = %q, want %q", results[0].Status, quota.StatusOK)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
}
