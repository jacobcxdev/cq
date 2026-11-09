package grok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
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
	if err != nil || len(accounts) != 1 || accounts[0] != owned {
		t.Fatalf("unverified source counted or healthy account lost: %d accounts, %v", len(accounts), err)
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
	owned.ID = "legacy-owned-id"
	owned.Source = ""
	owned.Active = true
	a = WithCodexBar(codexBarOwned{accounts: []Account{owned}}, fs, func(string) string { return "" }, "/home", client)
	accounts, err = a.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0] != owned {
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
		if r.Header.Get("Cookie") != "" || r.URL.String() != BaseURL+"/user?include=subscription" {
			t.Fatal("unverified credential reached a non-identity endpoint")
		}
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	a := WithCodexBar(codexBarOwned{}, fs, func(string) string { return "" }, "/home", client)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("count=%d err=%v", len(accounts), err)
	}
	for _, sourceID := range []string{"cookie", "key", "expired"} {
		// Old reporting-only IDs may still be selected by a caller after the
		// source is excluded from the public account inventory.
		digest := sha256.Sum256([]byte("codexbar:grok:" + sourceID))
		id := AccountID("grok_codexbar_" + hex.EncodeToString(digest[:16]))
		req, _ := http.NewRequest(http.MethodPost, BaseURL+"/responses", nil)
		if _, err := a.Do(context.Background(), id, req); err == nil {
			t.Fatal("unverified dispatch accepted")
		}
		reset, _ := http.NewRequest(http.MethodPost, resetInventoryURL, nil)
		if _, err := a.(resetReadAuthority).DoResetRead(context.Background(), id, reset); err == nil {
			t.Fatal("unverified reset read accepted")
		}
	}
}

func TestCodexBarGrokDeduplicatesVerifiedIdentityWithoutMergingSeats(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		user, principalType, principal, team string
		want                                 int
	}{
		{"same identity", "user-one", "Team", "seat-one", "team-one", 1},
		{"different user", "user-two", "Team", "seat-one", "team-one", 2},
		{"different principal type", "user-one", "User", "seat-one", "team-one", 2},
		{"different seat", "user-one", "Team", "seat-two", "team-one", 2},
		{"different team", "user-one", "Team", "seat-one", "team-two", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			_ = fs.MkdirAll("/home/.codexbar", 0o700)
			path := "/home/.codexbar/config.json"
			data := []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"external","label":"same-label","token":"borrowed-token"}]}}]}`)
			_ = fs.WriteFile(path, data, 0o600)
			owned := []Account{{ID: "legacy-owned-id", UserID: "user-one", PrincipalType: "Team", PrincipalID: "seat-one", TeamID: "team-one", Email: "same@example.test", Label: "same-label", Plan: "SuperGrok", Active: true}}
			before := append([]Account(nil), owned...)
			client := codexBarDoer(func(*http.Request) (*http.Response, error) {
				body := `{"userId":"` + test.user + `","principalType":"` + test.principalType + `","principalId":"` + test.principal + `","teamId":"` + test.team + `","email":"same@example.test","subscriptionTier":"SuperGrok"}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			a := WithCodexBar(codexBarOwned{accounts: owned}, fs, func(string) string { return "" }, "/home", client)
			accounts, err := a.Accounts(context.Background())
			if err != nil || len(accounts) != test.want || accounts[0] != before[0] || !reflect.DeepEqual(owned, before) {
				t.Fatalf("identity/seat distinction or owned preference lost: count=%d err=%v", len(accounts), err)
			}
			if test.want == 2 && (accounts[1].Source != "codexbar" || accounts[1].Active) {
				t.Fatal("borrowed source replaced owned preference")
			}
			after, _ := fs.ReadFile(path)
			if string(after) != string(data) {
				t.Fatal("borrowed configuration mutated")
			}
		})
	}
}

func TestCodexBarGrokUnverifiedSourcesEmitOneNonAccountDiagnostic(t *testing.T) {
	for _, scenario := range []string{"source read", "unknown identity", "auth expired", "profile read", "no subscription"} {
		t.Run(scenario, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			_ = fs.MkdirAll("/home/.codexbar", 0o700)
			data := []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"external","label":"private-label","token":"private-token"}]}}]}`)
			if scenario == "source read" {
				data = []byte(`{private-source-error`)
			}
			_ = fs.WriteFile("/home/.codexbar/config.json", data, 0o600)
			client := codexBarDoer(func(*http.Request) (*http.Response, error) {
				status, body := 200, `{"subscriptionTier":"SuperGrok"}`
				switch scenario {
				case "auth expired":
					status, body = 401, "private-error"
				case "profile read":
					return nil, errors.New("private-error")
				case "no subscription":
					body = `{"userId":"own-user","subscriptionTier":"free"}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			a := WithCodexBar(codexBarOwned{}, fs, func(string) string { return "" }, "/home", client)
			capture, err := os.CreateTemp(t.TempDir(), "diagnostic")
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Close()
			original := os.Stderr
			os.Stderr = capture
			defer func() { os.Stderr = original }()
			for range 2 {
				accounts, err := a.Accounts(context.Background())
				if err != nil || len(accounts) != 0 {
					t.Fatalf("unverified source became an account: count=%d err=%v", len(accounts), err)
				}
			}
			os.Stderr = original
			if _, err := capture.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(capture)
			if err != nil || strings.Count(string(output), "\n") != 1 || !strings.Contains(string(output), "CodexBar") || strings.Contains(string(output), "private-") {
				t.Fatal("ignored source diagnostic missing, repeated or disclosed private data")
			}
		})
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
