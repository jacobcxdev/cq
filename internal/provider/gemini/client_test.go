package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/httputil"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRefreshAccessTokenSendsExactRequest(t *testing.T) {
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != oauthTokenURL {
			t.Fatalf("request = %s %s, want POST %s", req.Method, req.URL, oauthTokenURL)
		}
		if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want form encoding", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		want := url.Values{
			"client_id":     {"client-id"},
			"client_secret": {"client-secret"},
			"refresh_token": {"refresh-token"},
			"grant_type":    {"refresh_token"},
		}
		if got.Encode() != want.Encode() {
			t.Fatalf("form = %q, want %q", got.Encode(), want.Encode())
		}
		return httpResponse(http.StatusOK, "{\"access_token\":\"new-access\",\"refresh_token\":\"new-refresh\",\"expires_in\":3600,\"token_type\":\"Bearer\"}"), nil
	})

	got, status, err := refreshAccessToken(context.Background(), doer, "client-id", "client-secret", "refresh-token")
	if err != nil {
		t.Fatalf("refreshAccessToken() error = %v", err)
	}
	if status != http.StatusOK || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.ExpiresIn != 3600 {
		t.Fatalf("refreshAccessToken() = %#v, %d, want decoded response", got, status)
	}
}

func TestLoadCodeAssistSendsExactRequest(t *testing.T) {
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		assertAntigravityRequest(t, req, loadCodeAssistURL, "access-token", "{\"metadata\":{\"ideType\":\"ANTIGRAVITY\"}}")
		return httpResponse(http.StatusOK, "{\"cloudaicompanionProject\":{\"projectId\":\"project-123\"}}"), nil
	})

	projectID, status, err := loadCodeAssist(context.Background(), doer, "access-token")
	if err != nil {
		t.Fatalf("loadCodeAssist() error = %v", err)
	}
	if status != http.StatusOK || projectID != "project-123" {
		t.Fatalf("loadCodeAssist() = %q, %d, want project-123/200", projectID, status)
	}
}

func TestLoadCodeAssistAcceptsStringProject(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, "{\"cloudaicompanionProject\":\"project-123\"}"), nil
	})
	projectID, _, err := loadCodeAssist(context.Background(), doer, "access-token")
	if err != nil || projectID != "project-123" {
		t.Fatalf("loadCodeAssist() = %q, %v, want project-123", projectID, err)
	}
}

func TestRetrieveUserQuotaSummarySendsExactRequest(t *testing.T) {
	wantBody := "{\"groups\":[]}"
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		assertAntigravityRequest(t, req, retrieveQuotaURL, "access-token", "{\"project\":\"project-123\"}")
		return httpResponse(http.StatusOK, wantBody), nil
	})

	body, status, err := retrieveUserQuotaSummary(context.Background(), doer, "access-token", "project-123")
	if err != nil {
		t.Fatalf("retrieveUserQuotaSummary() error = %v", err)
	}
	if status != http.StatusOK || string(body) != wantBody {
		t.Fatalf("retrieveUserQuotaSummary() = %q, %d", body, status)
	}
}

func TestAntigravityClientPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})
	_, _, err := retrieveUserQuotaSummary(ctx, doer, "access", "project")
	if err == nil {
		t.Fatal("retrieveUserQuotaSummary() error = nil, want cancellation")
	}
}

func TestAntigravityClientBoundsResponseBodies(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, strings.Repeat("x", httputil.MaxResponseBody+1)), nil
	})
	_, _, err := retrieveUserQuotaSummary(context.Background(), doer, "access", "project")
	if err == nil {
		t.Fatal("retrieveUserQuotaSummary() error = nil, want oversized-body error")
	}
}

func TestAntigravityClientErrorsDoNotExposeSecrets(t *testing.T) {
	secret := "secret-in-transport-error"
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	})
	_, _, err := refreshAccessToken(context.Background(), doer, "client", "client-secret", "refresh-token")
	if err == nil {
		t.Fatal("refreshAccessToken() error = nil, want request error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed transport detail: %q", err)
	}
}

func assertAntigravityRequest(t *testing.T, req *http.Request, wantURL, wantToken, wantBody string) {
	t.Helper()
	if req.Method != http.MethodPost || req.URL.String() != wantURL {
		t.Fatalf("request = %s %s, want POST %s", req.Method, req.URL, wantURL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+wantToken {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("User-Agent"); got != antigravityUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, antigravityUserAgent)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != wantBody {
		t.Fatalf("body = %q, want %q", body, wantBody)
	}
}
