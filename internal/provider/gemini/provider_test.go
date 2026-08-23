package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestDiscoverAccountsReturnsNoneWithoutCredentials(t *testing.T) {
	p := &Provider{credentials: staticCredentialReader{err: os.ErrNotExist}}
	accounts, err := p.DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("DiscoverAccounts() count = %d, want 0", len(accounts))
	}
}

func TestDiscoverAccountsUsesCredentialPresence(t *testing.T) {
	p := &Provider{credentials: staticCredentialReader{raw: "present"}}
	accounts, err := p.DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("DiscoverAccounts() count = %d, want 1", len(accounts))
	}
	want := struct {
		id     string
		label  string
		active bool
	}{antigravityAccountID, "Antigravity CLI", true}
	got := struct {
		id     string
		label  string
		active bool
	}{accounts[0].AccountID, accounts[0].Label, accounts[0].Active}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("account = %#v, want %#v", got, want)
	}
}

type blockingCredentialReader struct {
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
	raw     string
}

func (r *blockingCredentialReader) Get(_, _ string) (string, error) {
	r.once.Do(func() { r.started <- struct{}{} })
	<-r.release
	return r.raw, nil
}

type blockingFileSystem struct {
	fsutil.FileSystem
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (f *blockingFileSystem) ReadFile(name string) ([]byte, error) {
	f.once.Do(func() { f.started <- struct{}{} })
	<-f.release
	return f.FileSystem.ReadFile(name)
}

func TestFetchReadsLocalInputsConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	fsys := fsutil.NewMemFS()
	path := filepath.Join("/home/test", antigravityProjectCachePath)
	if err := fsys.WriteFile(path, []byte("project-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		credentials: &blockingCredentialReader{
			started: started,
			release: release,
			raw:     credentialFixture("access", "refresh", "2026-08-24T00:00:00Z"),
		},
		fs: &blockingFileSystem{
			FileSystem: fsys,
			started:    started,
			release:    release,
		},
	}

	type response struct {
		inputs localInputs
		err    error
	}
	done := make(chan response, 1)
	go func() {
		inputs, err := p.readLocalInputs()
		done <- response{inputs: inputs, err: err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("local reads did not start concurrently")
		}
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatalf("readLocalInputs() error = %v", got.err)
	}
	if got.inputs.ProjectID != "project-123" || got.inputs.Credentials.AccessToken != "access" {
		t.Fatalf("readLocalInputs() = %#v, want both local inputs", got.inputs)
	}
}

type panickingCredentialReader struct{}

func (panickingCredentialReader) Get(_, _ string) (string, error) {
	panic("credential payload")
}

func TestReadLocalInputsRecoversCredentialPanic(t *testing.T) {
	p := &Provider{credentials: panickingCredentialReader{}, fs: fsutil.NewMemFS()}
	_, err := p.readLocalInputs()
	if !errors.Is(err, errLocalInputPanic) {
		t.Fatalf("readLocalInputs() error = %v, want %v", err, errLocalInputPanic)
	}
}

func TestProviderFetchUsesFreshTokenAndCachedProject(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	requests := 0
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.String() != retrieveQuotaURL {
			t.Fatalf("URL = %s, want quota endpoint", req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer stored-access" {
			t.Fatalf("Authorization = %q, want stored access token", got)
		}
		return httpResponse(http.StatusOK, string(quotaFixture(validGeminiBuckets))), nil
	})
	p := testProvider(t, doer, credentialFixture("stored-access", "stored-refresh", now.Add(time.Hour).Format(time.RFC3339)), "project-123", true, "client-secret")

	result := fetchSingleResult(t, p, context.Background(), now)
	if result.Status != quota.StatusOK || result.AccountID != antigravityAccountID || !result.Active {
		t.Fatalf("Fetch() result = %#v, want active Gemini quota", result)
	}
	if requests != 1 {
		t.Fatalf("request count = %d, want quota request only", requests)
	}
}

func TestProviderFetchRefreshesExpiredTokenInMemory(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var paths []string
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.String() {
		case oauthTokenURL:
			return httpResponse(http.StatusOK, "{\"access_token\":\"new-access\",\"refresh_token\":\"rotated-refresh\",\"expires_in\":3600}"), nil
		case retrieveQuotaURL:
			if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("Authorization = %q, want refreshed token", got)
			}
			return httpResponse(http.StatusOK, string(quotaFixture(validGeminiBuckets))), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	p := testProvider(t, doer, credentialFixture("expired-access", "stored-refresh", now.Add(-time.Hour).Format(time.RFC3339)), "project-123", true, "client-secret")

	result := fetchSingleResult(t, p, context.Background(), now)
	if result.Status != quota.StatusOK {
		t.Fatalf("Fetch() result = %#v, want quota", result)
	}
	if want := []string{"/token", "/v1internal:retrieveUserQuotaSummary"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %q, want %q", paths, want)
	}
}

func TestProviderFetchLoadsMissingProjectBeforeQuota(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var paths []string
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.String() {
		case loadCodeAssistURL:
			return httpResponse(http.StatusOK, "{\"cloudaicompanionProject\":\"loaded-project\"}"), nil
		case retrieveQuotaURL:
			body, _ := io.ReadAll(req.Body)
			if string(body) != "{\"project\":\"loaded-project\"}" {
				t.Fatalf("quota body = %q, want loaded project", body)
			}
			return httpResponse(http.StatusOK, string(quotaFixture(validGeminiBuckets))), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	p := testProvider(t, doer, credentialFixture("stored-access", "stored-refresh", now.Add(time.Hour).Format(time.RFC3339)), "", false, "client-secret")

	result := fetchSingleResult(t, p, context.Background(), now)
	if result.Status != quota.StatusOK {
		t.Fatalf("Fetch() result = %#v, want quota", result)
	}
	if want := []string{"/v1internal:loadCodeAssist", "/v1internal:retrieveUserQuotaSummary"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %q, want %q", paths, want)
	}
}

func TestProviderFetchClassifiesLocalInputFailures(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		credential staticCredentialReader
		project    string
		write      bool
		wantCode   string
	}{
		{name: "missing credentials", credential: staticCredentialReader{err: os.ErrNotExist}, wantCode: "not_configured"},
		{name: "malformed credentials", credential: staticCredentialReader{raw: "not-json"}, wantCode: "parse_error"},
		{name: "missing access token", credential: staticCredentialReader{raw: credentialFixture("", "refresh", now.Add(time.Hour).Format(time.RFC3339))}, project: "project", write: true, wantCode: "no_token"},
		{name: "malformed project", credential: staticCredentialReader{raw: credentialFixture("access", "refresh", now.Add(time.Hour).Format(time.RFC3339))}, project: "project\x00id", write: true, wantCode: "parse_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if tt.write {
				writeProject(t, fsys, tt.project)
			}
			p := newProvider(doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("HTTP must not run after local input failure")
				return nil, nil
			}), fsys, tt.credential, "client-secret")
			result := fetchSingleResult(t, p, context.Background(), now)
			assertResultError(t, result, tt.wantCode, 0)
		})
	}
}

func TestProviderFetchRequiresRefreshMaterial(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		refresh string
		secret  string
	}{
		{name: "missing refresh token", secret: "client-secret"},
		{name: "missing client secret", refresh: "refresh-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProvider(t, doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("HTTP must not run without refresh material")
				return nil, nil
			}), credentialFixture("expired", tt.refresh, now.Add(-time.Hour).Format(time.RFC3339)), "project", true, tt.secret)
			assertResultError(t, fetchSingleResult(t, p, context.Background(), now), "auth_expired", 0)
		})
	}
}

func TestProviderFetchClassifiesHTTPFailures(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		status     int
		body       string
		transport  error
		wantCode   string
		wantStatus int
	}{
		{name: "transport", transport: errors.New("private-token"), wantCode: "fetch_error"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "{\"error\":\"private-token\"}", wantCode: "auth_expired", wantStatus: http.StatusUnauthorized},
		{name: "api error", status: http.StatusTooManyRequests, body: "{\"error\":\"private-token\"}", wantCode: "api_error", wantStatus: http.StatusTooManyRequests},
		{name: "malformed quota", status: http.StatusOK, body: "{\"groups\":[]}", wantCode: "parse_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doer := doerFunc(func(*http.Request) (*http.Response, error) {
				if tt.transport != nil {
					return nil, tt.transport
				}
				return httpResponse(tt.status, tt.body), nil
			})
			p := testProvider(t, doer, credentialFixture("access", "refresh", now.Add(time.Hour).Format(time.RFC3339)), "project", true, "client-secret")
			result := fetchSingleResult(t, p, context.Background(), now)
			assertResultError(t, result, tt.wantCode, tt.wantStatus)
			if strings.Contains(result.Error.Message, "private-token") {
				t.Fatalf("error message exposed private data: %q", result.Error.Message)
			}
		})
	}
}

func TestProviderFetchClassifiesRefreshRejection(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	p := testProvider(t, doerFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusBadRequest, "{\"error\":\"invalid_grant\",\"secret\":\"private\"}"), nil
	}), credentialFixture("expired", "refresh", now.Add(-time.Hour).Format(time.RFC3339)), "project", true, "client-secret")
	assertResultError(t, fetchSingleResult(t, p, context.Background(), now), "auth_expired", http.StatusBadRequest)
}

func TestProviderFetchClassifiesProjectFailures(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantCode: "auth_expired", wantStatus: http.StatusUnauthorized},
		{name: "api error", status: http.StatusServiceUnavailable, wantCode: "api_error", wantStatus: http.StatusServiceUnavailable},
		{name: "malformed", status: http.StatusOK, body: "{}", wantCode: "parse_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProvider(t, doerFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(tt.status, tt.body), nil
			}), credentialFixture("access", "refresh", now.Add(time.Hour).Format(time.RFC3339)), "", false, "client-secret")
			assertResultError(t, fetchSingleResult(t, p, context.Background(), now), tt.wantCode, tt.wantStatus)
		})
	}
}

func TestProviderFetchPropagatesCancellation(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := testProvider(t, doerFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	}), credentialFixture("access", "refresh", now.Add(time.Hour).Format(time.RFC3339)), "project", true, "client-secret")
	assertResultError(t, fetchSingleResult(t, p, ctx, now), "fetch_error", 0)
}

func TestProviderFetchRecoversHTTPPanic(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	p := testProvider(t, doerFunc(func(*http.Request) (*http.Response, error) {
		panic("private-token")
	}), credentialFixture("access", "refresh", now.Add(time.Hour).Format(time.RFC3339)), "project", true, "client-secret")
	result := fetchSingleResult(t, p, context.Background(), now)
	assertResultError(t, result, "fetch_panic", 0)
	if strings.Contains(result.Error.Message, "private-token") {
		t.Fatalf("panic result exposed private data: %q", result.Error.Message)
	}
}

func testProvider(t *testing.T, doer doerFunc, rawCredential, project string, writeProjectCache bool, clientSecret string) *Provider {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if writeProjectCache {
		writeProject(t, fsys, project)
	}
	return newProvider(doer, fsys, staticCredentialReader{raw: rawCredential}, clientSecret)
}

func writeProject(t *testing.T, fsys fsutil.FileSystem, project string) {
	t.Helper()
	home, err := fsys.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, antigravityProjectCachePath)
	if err := fsys.WriteFile(path, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
}

type quotaProvider interface {
	Fetch(context.Context, time.Time) ([]quota.Result, error)
}

func fetchSingleResult(t *testing.T, p quotaProvider, ctx context.Context, now time.Time) quota.Result {
	t.Helper()
	results, err := p.Fetch(ctx, now)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Fetch() count = %d, want 1", len(results))
	}
	return results[0]
}

func assertResultError(t *testing.T, result quota.Result, code string, status int) {
	t.Helper()
	if result.Status != quota.StatusError || result.Error == nil {
		t.Fatalf("result = %#v, want error", result)
	}
	if result.Error.Code != code || result.Error.HTTPStatus != status {
		t.Fatalf("error = %#v, want code/status %q/%d", result.Error, code, status)
	}
}
