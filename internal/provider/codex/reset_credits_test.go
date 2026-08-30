package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

type resetDoerFunc func(*http.Request) (*http.Response, error)

func (f resetDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func resetJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func resetMaterial() CredentialMaterial {
	return CredentialMaterial{AccessToken: "access-token", AccountID: "account-id"}
}

func TestResetCreditClientListUsesWhamContract(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", req.Method)
		}
		if got := req.URL.String(); got != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
			t.Fatalf("URL = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-Id"); got != "account-id" {
			t.Fatalf("ChatGPT-Account-Id = %q", got)
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("list request has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining < 4*time.Second || remaining > 5*time.Second {
			t.Fatalf("list deadline remaining = %s", remaining)
		}
		return resetJSONResponse(http.StatusOK, `{
			"credits": [{
				"id": "credit-1",
				"reset_type": "codex_rate_limits",
				"status": "available",
				"granted_at": "2026-08-30T08:00:00Z",
				"expires_at": "2026-08-31T08:00:00Z",
				"title": "Full reset",
				"description": "Reset usage"
			}],
			"available_count": 1
		}`), nil
	})}

	got, err := client.List(context.Background(), resetMaterial())
	if err != nil {
		t.Fatal(err)
	}
	if got.AvailableCount != 1 || len(got.Credits) != 1 {
		t.Fatalf("inventory = %+v", got)
	}
	credit := got.Credits[0]
	if credit.ID != "credit-1" || credit.ResetType != ResetTypeCodexRateLimits || credit.Status != ResetCreditAvailable {
		t.Fatalf("credit = %+v", credit)
	}
	if want := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC); !credit.GrantedAt.Equal(want) {
		t.Fatalf("GrantedAt = %s, want %s", credit.GrantedAt, want)
	}
	if credit.ExpiresAt == nil || !credit.ExpiresAt.Equal(time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("ExpiresAt = %v", credit.ExpiresAt)
	}
}

func TestResetCreditClientListPreservesUnknownValuesAndNilExpiry(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		return resetJSONResponse(http.StatusOK, `{
			"credits": [{
				"id": "credit-unknown",
				"reset_type": "future_reset",
				"status": "future_status",
				"granted_at": "2026-08-30T08:00:00Z",
				"expires_at": null,
				"title": null,
				"description": null
			}],
			"available_count": 0
		}`), nil
	})}

	got, err := client.List(context.Background(), resetMaterial())
	if err != nil {
		t.Fatal(err)
	}
	credit := got.Credits[0]
	if credit.ResetType != "future_reset" || credit.Status != "future_status" || credit.ExpiresAt != nil {
		t.Fatalf("credit = %+v", credit)
	}
	if credit.Title != "" || credit.Description != "" {
		t.Fatalf("optional text = %q/%q", credit.Title, credit.Description)
	}
}

func TestResetCreditClientListReturnsValidEntriesWithEntryErrors(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		return resetJSONResponse(http.StatusOK, `{
			"credits": [
				{"id":"good","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-30T08:00:00Z"},
				{"id":"bad","reset_type":"codex_rate_limits","status":"available","granted_at":"not-a-time"}
			],
			"available_count": 2
		}`), nil
	})}

	got, err := client.List(context.Background(), resetMaterial())
	if err == nil {
		t.Fatal("List() error = nil, want validation error")
	}
	if len(got.Credits) != 1 || got.Credits[0].ID != "good" {
		t.Fatalf("valid credits = %+v", got.Credits)
	}
	if len(got.EntryErrors) != 1 || got.EntryErrors[0].Index != 1 || got.EntryErrors[0].Code != "invalid_granted_at" {
		t.Fatalf("entry errors = %+v", got.EntryErrors)
	}
}

func TestResetCreditClientListRejectsInvalidInventory(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "negative count", body: `{"credits":[],"available_count":-1}`},
		{name: "contradictory count", body: `{"credits":[],"available_count":1}`},
		{name: "empty ID", body: `{"credits":[{"id":"","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-30T08:00:00Z"}],"available_count":1}`},
		{name: "invalid expiry", body: `{"credits":[{"id":"bad","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-30T08:00:00Z","expires_at":"bad"}],"available_count":1}`},
		{name: "unknown field", body: `{"credits":[],"available_count":0,"new_contract":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
				return resetJSONResponse(http.StatusOK, tt.body), nil
			})}
			if _, err := client.List(context.Background(), resetMaterial()); err == nil {
				t.Fatal("List() error = nil")
			}
		})
	}
}

func TestResetCreditClientConsumeUsesWhamContract(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if got := req.URL.String(); got != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume" {
			t.Fatalf("URL = %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-Id"); got != "account-id" {
			t.Fatalf("ChatGPT-Account-Id = %q", got)
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("consume request has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining < 9*time.Second || remaining > 10*time.Second {
			t.Fatalf("consume deadline remaining = %s", remaining)
		}
		var body struct {
			RedeemRequestID string `json:"redeem_request_id"`
			CreditID        string `json:"credit_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.RedeemRequestID != "request-key" || body.CreditID != "credit-1" {
			t.Fatalf("body = %+v", body)
		}
		return resetJSONResponse(http.StatusOK, `{"code":"reset","windows_reset":2}`), nil
	})}

	got, err := client.Consume(context.Background(), resetMaterial(), "credit-1", "request-key")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != ConsumeReset || got.WindowsReset != 2 {
		t.Fatalf("result = %+v", got)
	}
}

func TestResetCreditClientConsumeMapsKnownCodes(t *testing.T) {
	tests := []struct {
		code string
		want ConsumeResetOutcome
	}{
		{code: "reset", want: ConsumeReset},
		{code: "already_redeemed", want: ConsumeAlreadyRedeemed},
		{code: "nothing_to_reset", want: ConsumeNothingToReset},
		{code: "no_credit", want: ConsumeNoCredit},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
				return resetJSONResponse(http.StatusOK, `{"code":"`+tt.code+`"}`), nil
			})}
			got, err := client.Consume(context.Background(), resetMaterial(), "credit", "request")
			if err != nil || got.Outcome != tt.want {
				t.Fatalf("Consume() = %+v, %v", got, err)
			}
		})
	}
}

func TestResetCreditClientConsumeRejectsMalformedSuccess(t *testing.T) {
	for _, body := range []string{`{}`, `{"code":"future"}`, `{"code":"reset","extra":true}`, `not-json`} {
		client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
			return resetJSONResponse(http.StatusOK, body), nil
		})}
		if _, err := client.Consume(context.Background(), resetMaterial(), "credit", "request"); err == nil {
			t.Fatalf("Consume(%q) error = nil", body)
		}
	}
}

func TestResetCreditClientReturnsTypedHTTPStatus(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		return resetJSONResponse(http.StatusUnauthorized, `{"secret":"must not leak"}`), nil
	})}
	_, err := client.List(context.Background(), resetMaterial())
	var statusErr *ResetHTTPError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusUnauthorized {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "access-token") {
		t.Fatalf("error leaked response or token: %v", err)
	}
}

func TestResetCreditClientBoundsResponseBodies(t *testing.T) {
	client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		body := bytes.Repeat([]byte{'x'}, httputil.MaxResponseBody+1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	if _, err := client.List(context.Background(), resetMaterial()); err == nil {
		t.Fatal("List() error = nil")
	}
}

func TestResetCreditClientPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	client := ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected")
	})}
	_, err := client.List(ctx, resetMaterial())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("HTTP called after cancellation")
	}
}
