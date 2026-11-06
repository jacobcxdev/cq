package grok

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/quota"
)

type fakeAuthority struct {
	accounts []Account
	err      error
	do       func(context.Context, AccountID, *http.Request) (*http.Response, error)
}

func (f fakeAuthority) Accounts(context.Context) ([]Account, error) { return f.accounts, f.err }
func (f fakeAuthority) Do(ctx context.Context, id AccountID, r *http.Request) (*http.Response, error) {
	return f.do(ctx, id, r)
}
func quotaNow() time.Time { return time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC) }

const goodQuota = `{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-01T00:00:00Z","end":"2026-11-08T00:00:00Z"},"prepaidBalance":{"val":999}}}`

func TestSubscriptionReadsBoundRequestContext(t *testing.T) {
	a := fakeAuthority{accounts: []Account{{ID: "a"}}, do: func(ctx context.Context, _ AccountID, r *http.Request) (*http.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Error("subscription metadata read has no bounded deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(goodQuota))}, nil
	}}
	_, _ = New(a).Fetch(context.Background(), quotaNow())
}

func TestFetchSubscriptionQuota(t *testing.T) {
	a := fakeAuthority{accounts: []Account{{ID: "a", UserID: "user-a", Email: "a@example.test", Plan: "SuperGrok Heavy", Active: true}}, do: func(_ context.Context, id AccountID, r *http.Request) (*http.Response, error) {
		if id != "a" || r.Method != "GET" || r.URL.String() != BaseURL+"/billing?format=credits" {
			t.Errorf("wrong request: %s %s %s", id, r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(goodQuota))}, nil
	}}
	rows, err := New(a).Fetch(t.Context(), quotaNow())
	if err != nil || len(rows) != 1 {
		t.Fatalf("%#v %v", rows, err)
	}
	r := rows[0]
	if r.AccountID != "a" || r.Email != "a@example.test" || r.Plan != "SuperGrok Heavy" || !r.Active || r.Status != quota.StatusOK || r.Windows[quota.Window7Day].RemainingPct != 75 {
		t.Fatalf("row=%#v", r)
	}
	encoded, _ := json.Marshal(rows)
	for _, secret := range []string{"prepaid", "balance", "999", "token"} {
		if strings.Contains(strings.ToLower(string(encoded)), secret) {
			t.Fatalf("out of scope data in public quota: %s", encoded)
		}
	}
}

func TestFetchClassifiesFailuresWithoutProviderSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		body       string
		err        error
		panicDo    bool
	}{
		{name: "network", code: "fetch_error", err: errors.New("secret-value")},
		{name: "authentication", code: "auth_expired", status: 401, body: "secret-value"},
		{name: "permission", code: "api_error", status: 403, body: "secret-value"},
		{name: "rate limit", code: "api_error", status: 429, body: "secret-value"},
		{name: "parse", code: "parse_error", status: 200, body: "secret-value"},
		{name: "oversized", code: "fetch_error", status: 200, body: strings.Repeat("x", httputil.MaxResponseBody+1)},
		{name: "panic", code: "fetch_panic", panicDo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fakeAuthority{accounts: []Account{{ID: "a"}}, do: func(context.Context, AccountID, *http.Request) (*http.Response, error) {
				if tc.panicDo {
					panic("secret-value")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, tc.err
			}}
			rows, err := New(a).Fetch(t.Context(), quotaNow())
			if err != nil || len(rows) != 1 || rows[0].Error == nil || rows[0].Error.Code != tc.code {
				t.Fatalf("%#v %v", rows, err)
			}
			data, _ := json.Marshal(rows)
			if strings.Contains(string(data), "secret-value") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestFetchDeduplicatesSharedPoolWithoutDroppingIndependentFailures(t *testing.T) {
	a := fakeAuthority{accounts: []Account{{ID: "a", UserID: "same", PrincipalType: "User", PrincipalID: "same"}, {ID: "alias", UserID: "same", PrincipalType: "User", PrincipalID: "same"}, {ID: "b", UserID: "other"}}, do: func(_ context.Context, id AccountID, _ *http.Request) (*http.Response, error) {
		if id == "alias" {
			t.Error("duplicate shared pool fetched")
		}
		if id == "b" {
			return nil, errors.New("failed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(goodQuota))}, nil
	}}
	rows, err := New(a).Fetch(t.Context(), quotaNow())
	if err != nil || len(rows) != 2 || rows[0].AccountID != "a" || rows[1].AccountID != "b" || rows[1].Error == nil {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

func TestFetchNoConfigurationAndAccountReaderFailures(t *testing.T) {
	for _, tc := range []struct {
		authority Authority
		code      string
	}{{fakeAuthority{}, "not_configured"}, {fakeAuthority{err: errors.New("secret-value")}, "fetch_error"}, {nil, "not_configured"}} {
		rows, err := New(tc.authority).Fetch(t.Context(), quotaNow())
		if err != nil || len(rows) != 1 || rows[0].Error == nil || rows[0].Error.Code != tc.code {
			t.Fatalf("rows=%#v err=%v", rows, err)
		}
	}
}

func TestFetchReportingOnlySourceDoesNotImplyUsableQuota(t *testing.T) {
	a := fakeAuthority{accounts: []Account{{ID: "unverified", Label: "CodexBar", ReportingOnly: true}}, do: func(context.Context, AccountID, *http.Request) (*http.Response, error) {
		t.Fatal("unverified source reached subscription endpoint")
		return nil, nil
	}}
	rows, err := New(a).Fetch(t.Context(), quotaNow())
	if err != nil || len(rows) != 1 || rows[0].IsUsable() || rows[0].Error == nil || len(rows[0].Windows) > 0 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}
