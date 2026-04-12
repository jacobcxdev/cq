package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
func (e fakeDirEntry) IsDir() bool                 { return e.isDir }
func (e fakeDirEntry) Type() os.FileMode           { return 0 }
func (e fakeDirEntry) Info() (os.FileInfo, error)   { return nil, nil }

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

func (fi fakeFileInfo) Name() string      { return fi.name }
func (fi fakeFileInfo) Size() int64       { return 0 }
func (fi fakeFileInfo) Mode() os.FileMode { return 0o644 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Now() }
func (fi fakeFileInfo) IsDir() bool       { return false }
func (fi fakeFileInfo) Sys() any          { return nil }

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
const happyUsageBody = `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":20.0,"reset_at":1774051200}}}`

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
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("tok-abc", "ref-abc", "", "")

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
	fs.files["/fake/home/.codex/auth.json"] = validAuthJSON("old-tok", "", "", "")

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

func TestFetch401RefreshesAndRetriesUsage(t *testing.T) {
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
	if usageCalls.Load() != 2 {
		t.Fatalf("usageCalls = %d, want 2", usageCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
}

func TestFetch401RetryPreservesAPIError(t *testing.T) {
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
