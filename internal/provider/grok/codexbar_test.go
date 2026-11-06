package grok

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type codexBarOwned struct{ accounts []Account }

func (o codexBarOwned) Accounts(context.Context) ([]Account, error) { return o.accounts, nil }
func (o codexBarOwned) Do(context.Context, AccountID, *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

type codexBarDoer func(*http.Request) (*http.Response, error)

func (f codexBarDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrokenCodexBarPreservesOwnedGrokAvailability(t *testing.T) {
	fs := fsutil.NewMemFS()
	_ = fs.MkdirAll("/home/.codexbar", 0o700)
	_ = fs.WriteFile("/home/.codexbar/config.json", []byte(`{broken`), 0o600)
	owned := Account{ID: "owned", UserID: "user", Active: true}
	a := WithCodexBar(codexBarOwned{accounts: []Account{owned}}, fs, func(string) string { return "" }, "/home", nil)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 2 || accounts[0].ID != "owned" || !accounts[1].ReportingOnly || accounts[1].Source != "codexbar" {
		t.Fatalf("healthy account or source diagnostic lost: %d accounts, %v", len(accounts), err)
	}
}

func TestCodexBarDiscoveryVerifiesIdentityAndNeverAdopts(t *testing.T) {
	fs := fsutil.NewMemFS()
	_ = fs.MkdirAll("/home/.codexbar", 0o700)
	data := []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"external","label":"Borrowed","token":"Bearer borrowed-token"}]}}]}`)
	_ = fs.WriteFile("/home/.codexbar/config.json", data, 0o600)
	client := codexBarDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != BaseURL+"/user?include=subscription" || r.Header.Get("Authorization") != "Bearer borrowed-token" {
			t.Fatal("wrong identity request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"userId":"user-one","email":"one@example.test","subscriptionTier":"SuperGrok"}`))}, nil
	})
	a := WithCodexBar(codexBarOwned{}, fs, func(string) string { return "" }, "/home", client)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].UserID != "user-one" || accounts[0].Source != "codexbar" || accounts[0].ReportingOnly {
		t.Fatalf("count=%d err=%v", len(accounts), err)
	}
	owned := accounts[0]
	owned.Source = ""
	owned.Active = true
	a = WithCodexBar(codexBarOwned{accounts: []Account{owned}}, fs, func(string) string { return "" }, "/home", client)
	accounts, err = a.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Source != "" {
		t.Fatal("verified duplicate not suppressed")
	}
	after, _ := fs.ReadFile("/home/.codexbar/config.json")
	if string(after) != string(data) {
		t.Fatal("borrowed file mutated")
	}
}

func TestCodexBarCookieAndUnverifiedAccountsCannotDispatch(t *testing.T) {
	fs := fsutil.NewMemFS()
	_ = fs.MkdirAll("/home/.codexbar", 0o700)
	_ = fs.WriteFile("/home/.codexbar/config.json", []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"cookie","token":"sso=private"},{"id":"key","token":"xai-private"},{"id":"expired","token":"expired-token"}]}}]}`), 0o600)
	client := codexBarDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" {
			t.Fatal("cookie sent as OAuth")
		}
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	a := WithCodexBar(codexBarOwned{}, fs, func(string) string { return "" }, "/home", client)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 3 {
		t.Fatalf("count=%d err=%v", len(accounts), err)
	}
	for _, account := range accounts {
		if !account.ReportingOnly {
			t.Fatal("unverified account routable")
		}
		req, _ := http.NewRequest(http.MethodPost, BaseURL+"/responses", nil)
		if _, err := a.Do(context.Background(), account.ID, req); err == nil {
			t.Fatal("unverified dispatch accepted")
		}
	}
}

func TestCodexBarDispatchPreservesBodyAndRereadsIdentity(t *testing.T) {
	fs := fsutil.NewMemFS()
	_ = fs.MkdirAll("/home/.codexbar", 0o700)
	path := "/home/.codexbar/config.json"
	_ = fs.WriteFile(path, []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"one","token":"borrowed-token"}]}}]}`), 0o600)
	user := "user-one"
	dispatches := 0
	client := codexBarDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/user" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"userId":"` + user + `","subscriptionTier":"SuperGrok"}`))}, nil
		}
		dispatches++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"grok","input":"keep"}` || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Cookie") != "" {
			t.Fatal("request changed or credential leaked")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	a := WithCodexBar(codexBarOwned{}, fs, func(string) string { return "" }, "/home", client)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, BaseURL+"/responses", strings.NewReader(`{"model":"grok","input":"keep"}`))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cookie", "do-not-forward")
	response, err := a.Do(context.Background(), accounts[0].ID, req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	user = "another-user"
	if _, err := a.Do(context.Background(), accounts[0].ID, req); err == nil {
		t.Fatal("changed external identity accepted")
	}
	if dispatches != 1 {
		t.Fatal("unexpected dispatch count")
	}
}
