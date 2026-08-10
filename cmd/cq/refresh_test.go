package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

// testDoer is an httputil.Doer stub for refresh tests.
type testDoer func(*http.Request) (*http.Response, error)

func (f testDoer) Do(req *http.Request) (*http.Response, error) { return f(req) }

func profileJSON(email string) *http.Response {
	body := `{"account":{"email":"` + email + `","uuid":"uuid-test"}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func fakeRefreshCodexJWT(email, accountID, userID string, exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := map[string]any{
		"email": email,
		"exp":   float64(exp.Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
		},
	}
	body, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + encoded + ".fakesig"
}

func codexRefreshAuthJSON(accessToken, refreshToken, idToken, accountID string) []byte {
	m := map[string]any{
		"auth_mode": "chatgpt",
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

// TestSyncAnonymousToIdentifiedReturnsChangedTrue verifies that when
// syncAnonymousToIdentifiedWithChange successfully syncs an anonymous entry's
// fresh tokens into an identified account, it returns changed=true.
func TestSyncAnonymousToIdentifiedReturnsChangedTrue(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	origPersist := persistRefreshedTokenFn
	origStore := storeCQAccountFn
	defer func() {
		persistRefreshedTokenFn = origPersist
		storeCQAccountFn = origStore
	}()

	persisted := 0
	stored := 0
	persistRefreshedTokenFn = func(acct *keyring.ClaudeOAuth) {
		persisted++
	}
	storeCQAccountFn = func(acct *keyring.ClaudeOAuth) error {
		stored++
		return nil
	}

	identified := keyring.ClaudeOAuth{
		Email:        "user@example.com",
		AccountUUID:  "uuid-user",
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    nowMs - 1000, // stale
	}
	anon := keyring.ClaudeOAuth{
		AccessToken:  "new-at",
		RefreshToken: "new-rt",
		ExpiresAt:    nowMs + 999999,
		Scopes:       []string{"user:read"},
	}

	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("user@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	result, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if !changed {
		t.Fatal("expected changed=true when anon sync updated a stored account")
	}
	if persisted != 1 {
		t.Fatalf("persisted = %d, want 1", persisted)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1", stored)
	}
	found := false
	for _, a := range result {
		if a.Email == "user@example.com" && a.AccessToken == "new-at" {
			found = true
			if got := strings.Join(a.Scopes, ","); got != "user:read" {
				t.Fatalf("scopes = %q, want user:read", got)
			}
		}
	}
	if !found {
		t.Errorf("identified account should have new-at after sync; got %+v", result)
	}
}

func TestSyncAnonymousToIdentifiedSkipsOlderAnonymousToken(t *testing.T) {
	nowMs := time.Now().UnixMilli()

	identified := keyring.ClaudeOAuth{
		Email:        "user@example.com",
		AccountUUID:  "uuid-user",
		AccessToken:  "fresh-at",
		RefreshToken: "fresh-rt",
		ExpiresAt:    nowMs + 999999,
		Scopes:       []string{"existing:scope"},
	}
	anon := keyring.ClaudeOAuth{
		AccessToken:  "stale-at",
		RefreshToken: "stale-rt",
		ExpiresAt:    nowMs + 100,
		Scopes:       []string{"user:read"},
	}

	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("user@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	result, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if changed {
		t.Fatal("expected changed=false when anonymous token is older than identified account")
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2 when nothing was merged", len(result))
	}
	if result[0].AccessToken != "fresh-at" {
		t.Fatalf("identified token = %q, want fresh-at", result[0].AccessToken)
	}
	if got := strings.Join(result[0].Scopes, ","); got != "existing:scope" {
		t.Fatalf("scopes = %q, want existing:scope", got)
	}
}

func TestRunRefreshSkipsAnonymousExpiredReauth(t *testing.T) {
	origDiscover := discoverClaudeAccountsFn
	origNewClient := newHTTPClientFn
	origRefreshCodex := refreshCodexAccountsFn
	origInvalidate := invalidateProviderCacheFn
	origIsTerminal := isStdinTerminalFn
	defer func() {
		discoverClaudeAccountsFn = origDiscover
		newHTTPClientFn = origNewClient
		refreshCodexAccountsFn = origRefreshCodex
		invalidateProviderCacheFn = origInvalidate
		isStdinTerminalFn = origIsTerminal
	}()

	nowMs := time.Now().UnixMilli()
	discoverClaudeAccountsFn = func() []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{ExpiresAt: nowMs - 1000}}
	}
	newHTTPClientFn = func(timeout time.Duration, version string) httputil.Doer {
		return testDoer(func(req *http.Request) (*http.Response, error) { return nil, nil })
	}
	refreshCodexAccountsFn = func(ctx context.Context, client httputil.Doer, nowMs int64) (bool, error) {
		return false, nil
	}
	invalidateProviderCacheFn = func(id provider.ID) {}
	isStdinTerminalFn = func() bool { return false }

	if err := runRefresh(); err != nil {
		t.Fatalf("runRefresh returned error for anonymous expired account: %v", err)
	}
}

// TestSyncAnonymousToIdentifiedNoMatchReturnsChangedFalse verifies that when
// no anonymous entry can be resolved to an identified account, changed=false.
func TestSyncAnonymousToIdentifiedNoMatchReturnsChangedFalse(t *testing.T) {
	nowMs := time.Now().UnixMilli()

	identified := keyring.ClaudeOAuth{
		Email:       "other@example.com",
		AccessToken: "at",
		ExpiresAt:   nowMs + 999999,
	}
	anon := keyring.ClaudeOAuth{
		AccessToken: "anon-at",
		ExpiresAt:   nowMs + 999999,
	}

	// Profile resolves to an email not in identified list — no match.
	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("nobody@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	_, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if changed {
		t.Fatal("expected changed=false when no anonymous entry matched an identified account")
	}
}

func TestSyncAnonymousToIdentifiedPersistsAndRemovesMergedAnonymous(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	origPersist := persistRefreshedTokenFn
	origStore := storeCQAccountFn
	defer func() {
		persistRefreshedTokenFn = origPersist
		storeCQAccountFn = origStore
	}()

	persisted := 0
	stored := 0
	persistRefreshedTokenFn = func(acct *keyring.ClaudeOAuth) {
		persisted++
	}
	storeCQAccountFn = func(acct *keyring.ClaudeOAuth) error {
		stored++
		return nil
	}

	identified := keyring.ClaudeOAuth{
		Email:        "user@example.com",
		AccountUUID:  "uuid-user",
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    nowMs - 1000,
	}
	anon := keyring.ClaudeOAuth{
		AccessToken:  "new-at",
		RefreshToken: "new-rt",
		ExpiresAt:    nowMs + 999999,
	}

	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("user@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	result, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if !changed {
		t.Fatal("expected changed=true when anon sync updated a stored account")
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 after merged anonymous row removal", len(result))
	}
	if persisted != 1 {
		t.Fatalf("persisted = %d, want 1", persisted)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1", stored)
	}
}

// TestInvalidateProviderCacheRemovesFile verifies that invalidateProviderCache
// removes the cached result file for the given provider.
func TestInvalidateProviderCacheRemovesFile(t *testing.T) {
	dir := t.TempDir()
	// Point XDG_CACHE_HOME so DefaultDir() resolves to our temp tree.
	t.Setenv("XDG_CACHE_HOME", dir)

	// Compute what DefaultDir returns and write a placeholder cache file.
	cacheDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cachePath := filepath.Join(cacheDir, string(provider.Claude)+".json")
	if err := os.WriteFile(cachePath, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	invalidateProviderCache(provider.Claude)

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file should have been removed; stat err = %v", err)
	}
}

func TestInvalidateProviderCacheRemovesCodexFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	cacheDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cachePath := filepath.Join(cacheDir, string(provider.Codex)+".json")
	if err := os.WriteFile(cachePath, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	invalidateProviderCache(provider.Codex)

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file should have been removed; stat err = %v", err)
	}
}

func TestRunRefreshUsesBrokeredCodexRefreshPass(t *testing.T) {
	origDiscover := discoverClaudeAccountsFn
	origNewClient := newHTTPClientFn
	origRefreshCodex := refreshCodexAccountsFn
	origInvalidate := invalidateProviderCacheFn
	defer func() {
		discoverClaudeAccountsFn = origDiscover
		newHTTPClientFn = origNewClient
		refreshCodexAccountsFn = origRefreshCodex
		invalidateProviderCacheFn = origInvalidate
	}()

	discoverClaudeAccountsFn = func() []keyring.ClaudeOAuth { return nil }
	newHTTPClientFn = func(timeout time.Duration, version string) httputil.Doer {
		return testDoer(func(req *http.Request) (*http.Response, error) {
			return nil, nil
		})
	}

	codexCalled := false
	cacheInvalidated := false
	refreshCodexAccountsFn = func(ctx context.Context, client httputil.Doer, nowMs int64) (bool, error) {
		codexCalled = true
		return true, nil
	}
	invalidateProviderCacheFn = func(id provider.ID) {
		if id == provider.Codex {
			cacheInvalidated = true
		}
	}

	if err := runRefresh(); err != nil {
		t.Fatalf("runRefresh returned error: %v", err)
	}
	if !codexCalled {
		t.Fatal("Codex refresh pass did not run")
	}
	if !cacheInvalidated {
		t.Fatal("Codex cache was not invalidated after brokered credential change")
	}
}

func TestRunRefreshReturnsTypedCodexAuthorityErrorWithoutInvalidatingCache(t *testing.T) {
	origDiscover := discoverClaudeAccountsFn
	origNewClient := newHTTPClientFn
	origRefreshCodex := refreshCodexAccountsFn
	origInvalidate := invalidateProviderCacheFn
	defer func() {
		discoverClaudeAccountsFn = origDiscover
		newHTTPClientFn = origNewClient
		refreshCodexAccountsFn = origRefreshCodex
		invalidateProviderCacheFn = origInvalidate
	}()

	discoverClaudeAccountsFn = func() []keyring.ClaudeOAuth { return nil }
	newHTTPClientFn = func(time.Duration, string) httputil.Doer { return nil }
	refreshCodexAccountsFn = func(context.Context, httputil.Doer, int64) (bool, error) {
		return false, codexprov.ErrCredentialAuthorityUnavailable
	}
	cacheInvalidated := false
	invalidateProviderCacheFn = func(id provider.ID) {
		if id == provider.Codex {
			cacheInvalidated = true
		}
	}

	err := runRefresh()

	if !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("runRefresh error = %T %v, want typed credential authority unavailable", err, err)
	}
	if cacheInvalidated {
		t.Fatal("Codex cache was invalidated after credential authority failure")
	}
}

type testCodexRefreshAuthority struct {
	inventory codexprov.Inventory
	listErr   error
	calls     []testCodexRefreshCall
}

type testCodexRefreshCall struct {
	ref      codexprov.CandidateRef
	revision codexprov.Revision
}

func (a *testCodexRefreshAuthority) List(context.Context) (codexprov.Inventory, error) {
	return a.inventory, a.listErr
}

func (a *testCodexRefreshAuthority) Refresh(_ context.Context, ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.RefreshResult, error) {
	a.calls = append(a.calls, testCodexRefreshCall{ref: ref, revision: revision})
	return codexprov.RefreshResult{}, nil
}

func TestRefreshManagedCodexAuthorityFailsClosedWhenListUnavailable(t *testing.T) {
	authority := &testCodexRefreshAuthority{listErr: errors.New("read /secret/path: authority failed")}

	changed, err := refreshManagedCodexAuthority(context.Background(), authority, time.Now())

	if changed {
		t.Fatal("refresh pass reported a change after List failed")
	}
	if !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("refresh error = %T %v, want typed credential authority unavailable", err, err)
	}
	if got := err.Error(); got != codexprov.ErrCredentialAuthorityUnavailable.Error() {
		t.Fatalf("refresh error = %q, want sanitised %q", got, codexprov.ErrCredentialAuthorityUnavailable)
	}
	if len(authority.calls) != 0 {
		t.Fatalf("Refresh calls = %d, want 0 after List failed", len(authority.calls))
	}
}

func TestRefreshManagedCodexAuthorityUsesSanitisedExpiryForManagedCandidateOnly(t *testing.T) {
	now := time.Now()
	inventory := codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Candidates: []codexprov.CredentialCandidate{
			{
				Ref:      codexprov.CandidateRef{AccountKey: "managed", CandidateID: "managed-candidate"},
				Revision: "managed-revision", Source: codexprov.SourceManaged,
				AccessExpiresAt: now.Add(-time.Minute),
			},
			{
				Ref:      codexprov.CandidateRef{AccountKey: "system", CandidateID: "system-candidate"},
				Revision: "system-revision", Source: codexprov.SourceSystem,
				AccessExpiresAt: now.Add(-time.Minute),
			},
			{
				Ref:      codexprov.CandidateRef{AccountKey: "external", CandidateID: "external-candidate"},
				Revision: "external-revision", Source: codexprov.SourceExternal,
				AccessExpiresAt: now.Add(-time.Minute),
			},
			{
				Ref:      codexprov.CandidateRef{AccountKey: "fresh", CandidateID: "fresh-candidate"},
				Revision: "fresh-revision", Source: codexprov.SourceManaged,
				AccessExpiresAt: now.Add(time.Hour),
			},
		},
	}}}
	authority := &testCodexRefreshAuthority{inventory: inventory}

	changed, err := refreshManagedCodexAuthority(context.Background(), authority, now)

	if err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	if !changed {
		t.Fatal("refresh pass reported no change")
	}
	if len(authority.calls) != 1 {
		t.Fatalf("Refresh calls = %d, want 1", len(authority.calls))
	}
	if got := authority.calls[0]; got.ref != inventory.Accounts[0].Candidates[0].Ref || got.revision != "managed-revision" {
		t.Fatalf("Refresh call = %+v, want managed ref with revision fence", got)
	}
}

func TestRefreshManagedCodexAuthorityFailsClosedWhenConfiguredInventoryDegraded(t *testing.T) {
	now := time.Now()
	authority := &testCodexRefreshAuthority{inventory: codexprov.Inventory{
		Accounts: []codexprov.LogicalAccount{{Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "managed", CandidateID: "managed-candidate"},
			Revision: "managed-revision", Source: codexprov.SourceManaged, AccessExpiresAt: now.Add(-time.Minute),
		}}}},
		ExternalSources: []codexprov.ExternalSourceStatus{{Name: "configured", ErrorCode: "unavailable"}},
	}}

	changed, err := refreshManagedCodexAuthority(context.Background(), authority, now)

	if changed {
		t.Fatal("refresh pass reported a change for degraded configured inventory")
	}
	if !errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
		t.Fatalf("refresh error = %T %v, want typed credential inventory degraded", err, err)
	}
	if got := err.Error(); got != codexprov.ErrCredentialInventoryDegraded.Error() {
		t.Fatalf("refresh error = %q, want sanitised %q", got, codexprov.ErrCredentialInventoryDegraded)
	}
	if len(authority.calls) != 0 {
		t.Fatalf("Refresh calls = %d, want 0 for degraded configured inventory", len(authority.calls))
	}
}

func TestRefreshManagedCodexAuthorityAllowsOptionalAbsentExternalInventory(t *testing.T) {
	now := time.Now()
	managed := codexprov.CredentialCandidate{
		Ref:      codexprov.CandidateRef{AccountKey: "managed", CandidateID: "managed-candidate"},
		Revision: "managed-revision", Source: codexprov.SourceManaged, AccessExpiresAt: now.Add(-time.Minute),
	}
	authority := &testCodexRefreshAuthority{inventory: codexprov.Inventory{
		Accounts: []codexprov.LogicalAccount{{Candidates: []codexprov.CredentialCandidate{managed}}},
		ExternalSources: []codexprov.ExternalSourceStatus{{
			Name: "optional", ErrorCode: "unavailable", OptionalAbsent: true,
		}},
	}}

	changed, err := refreshManagedCodexAuthority(context.Background(), authority, now)

	if err != nil {
		t.Fatalf("refresh error = %v, want optional absence to remain non-degraded", err)
	}
	if !changed {
		t.Fatal("refresh pass reported no change with only an optional absent source")
	}
	if len(authority.calls) != 1 || authority.calls[0].ref != managed.Ref || authority.calls[0].revision != managed.Revision {
		t.Fatalf("Refresh calls = %+v, want one revision-fenced managed refresh", authority.calls)
	}
}

func TestRefreshCodexAccountsWithoutIDTokenKeepsDiscoveredExpiryFresh(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	now := time.Now()
	expiredIDToken := fakeRefreshCodexJWT("refresh@example.com", "acct-1", "user-1", now.Add(-time.Hour))
	accountPath := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	fs := &refreshCodexFS{base: fsutil.OSFileSystem{}, home: "/fake/home", files: map[string][]byte{
		accountPath: codexRefreshAuthJSON("old-tok", "old-ref", expiredIDToken, "acct-1"),
	}}
	fs.dirEntries = map[string][]os.DirEntry{
		"/fake/home/.codex/accounts": {refreshDirEntry{name: "user-1::acct-1.auth.json"}},
	}

	origNewClient := newHTTPClientFn
	defer func() { newHTTPClientFn = origNewClient }()
	newHTTPClientFn = func(timeout time.Duration, version string) httputil.Doer {
		return testDoer(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/oauth/token":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-tok","refresh_token":"new-ref","expires_in":7200}`)),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})
	}

	client := newHTTPClientFn(10*time.Second, version)
	origFS := codexRefreshFSFactory
	defer func() { codexRefreshFSFactory = origFS }()
	codexRefreshFSFactory = func() fsutil.DurableFileSystem { return fs }

	changed, err := refreshCodexAccounts(context.Background(), client, now.UnixMilli())
	if err != nil {
		t.Fatalf("refreshCodexAccounts error = %v", err)
	}
	if !changed {
		t.Fatal("expected refreshCodexAccounts to report changed=true")
	}

	accts := codexprov.DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if got := accts[0].ExpiresAt; got <= now.UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want > %d after background refresh without id_token", got, now.UnixMilli())
	}
}

func TestRefreshCodexAccountsWithIDTokenMissingExpFallsBackToExpiresIn(t *testing.T) {
	t.Skip("legacy direct shared-credential refresh contract is permanently retired")
	now := time.Now()
	expiredIDToken := fakeRefreshCodexJWT("refresh@example.com", "acct-1", "user-1", now.Add(-time.Hour))
	refreshedBody, _ := json.Marshal(map[string]any{
		"email": "refresh@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-1",
			"chatgpt_user_id":    "user-1",
		},
	})
	refreshedIDToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(refreshedBody) + ".fakesig"
	accountPath := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	fs := &refreshCodexFS{base: fsutil.OSFileSystem{}, home: "/fake/home", files: map[string][]byte{
		accountPath: codexRefreshAuthJSON("old-tok", "old-ref", expiredIDToken, "acct-1"),
	}}
	fs.dirEntries = map[string][]os.DirEntry{
		"/fake/home/.codex/accounts": {refreshDirEntry{name: "user-1::acct-1.auth.json"}},
	}

	origNewClient := newHTTPClientFn
	defer func() { newHTTPClientFn = origNewClient }()
	newHTTPClientFn = func(timeout time.Duration, version string) httputil.Doer {
		return testDoer(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/oauth/token":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-tok","refresh_token":"new-ref","id_token":"` + refreshedIDToken + `","expires_in":7200}`)),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})
	}

	client := newHTTPClientFn(10*time.Second, version)
	origFS := codexRefreshFSFactory
	defer func() { codexRefreshFSFactory = origFS }()
	codexRefreshFSFactory = func() fsutil.DurableFileSystem { return fs }

	changed, err := refreshCodexAccounts(context.Background(), client, now.UnixMilli())
	if err != nil {
		t.Fatalf("refreshCodexAccounts error = %v", err)
	}
	if !changed {
		t.Fatal("expected refreshCodexAccounts to report changed=true")
	}

	accts := codexprov.DiscoverAccounts(fs)
	if len(accts) != 1 {
		t.Fatalf("len(accts) = %d, want 1", len(accts))
	}
	if got := accts[0].ExpiresAt; got <= now.UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want > %d after background refresh with id_token missing exp", got, now.UnixMilli())
	}
}

type refreshCodexFS struct {
	base       fsutil.OSFileSystem
	home       string
	files      map[string][]byte
	dirEntries map[string][]os.DirEntry
}

func (f *refreshCodexFS) Stat(name string) (os.FileInfo, error) {
	if _, ok := f.files[name]; ok {
		return refreshFileInfo{name: filepath.Base(name)}, nil
	}
	return nil, os.ErrNotExist
}

// refreshFileInfo is a minimal os.FileInfo implementation for refreshCodexFS.
type refreshFileInfo struct{ name string }

func (fi refreshFileInfo) Name() string                                { return fi.name }
func (fi refreshFileInfo) Size() int64                                 { return 0 }
func (fi refreshFileInfo) Mode() os.FileMode                           { return 0o600 }
func (fi refreshFileInfo) ModTime() time.Time                          { return time.Time{} }
func (fi refreshFileInfo) IsDir() bool                                 { return false }
func (fi refreshFileInfo) Sys() any                                    { return nil }
func (f *refreshCodexFS) MkdirAll(path string, perm os.FileMode) error { return nil }
func (f *refreshCodexFS) UserHomeDir() (string, error)                 { return f.home, nil }
func (f *refreshCodexFS) ReadDir(name string) ([]os.DirEntry, error) {
	entries, ok := f.dirEntries[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return entries, nil
}
func (f *refreshCodexFS) ReadFile(name string) ([]byte, error) {
	data, ok := f.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}
func (f *refreshCodexFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	copied := append([]byte(nil), data...)
	f.files[name] = copied
	return nil
}
func (f *refreshCodexFS) Rename(oldpath, newpath string) error {
	data, ok := f.files[oldpath]
	if !ok {
		return os.ErrNotExist
	}
	f.files[newpath] = append([]byte(nil), data...)
	delete(f.files, oldpath)
	return nil
}
func (f *refreshCodexFS) Remove(name string) error {
	delete(f.files, name)
	return nil
}
func (f *refreshCodexFS) Chmod(name string, mode os.FileMode) error { return nil }
func (f *refreshCodexFS) SyncFile(name string) error                { return nil }
func (f *refreshCodexFS) SyncDir(name string) error                 { return nil }

type refreshDirEntry struct{ name string }

func (e refreshDirEntry) Name() string               { return e.name }
func (e refreshDirEntry) IsDir() bool                { return false }
func (e refreshDirEntry) Type() os.FileMode          { return 0 }
func (e refreshDirEntry) Info() (os.FileInfo, error) { return nil, nil }

// TestRefreshCodexFSStatIsHermetic proves that refreshCodexFS.Stat uses the
// in-memory file map instead of delegating to the real OS, so tests are fully
// isolated from the host filesystem.
func TestRefreshCodexFSStatIsHermetic(t *testing.T) {
	fs := &refreshCodexFS{
		home:  "/fake/home",
		files: map[string][]byte{"/fake/home/.codex/auth.json": []byte(`{}`)},
	}

	// File present in map — should succeed without touching the real OS.
	fi, err := fs.Stat("/fake/home/.codex/auth.json")
	if err != nil {
		t.Fatalf("Stat existing in-memory file: %v", err)
	}
	if fi.Name() != "auth.json" {
		t.Fatalf("fi.Name() = %q, want auth.json", fi.Name())
	}

	// File absent from map — should return ErrNotExist without touching the real OS.
	_, err = fs.Stat("/fake/home/.codex/nonexistent.json")
	if !os.IsNotExist(err) {
		t.Fatalf("Stat absent file: got %v, want ErrNotExist", err)
	}
}
