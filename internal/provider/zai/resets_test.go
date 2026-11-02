package zai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const resetFixture = `{"code":200,"success":true,"data":{"customerId":900719925474099312,"targetType":"PERSONAL","fiveHourResets":[{"recordId":900719925474099313,"grantType":"CAMPAIGN","available":true,"expireTime":"2026-11-04T12:00:00Z"},{"recordId":"earlier","grantType":2,"available":true,"expireTime":"2026-11-03T12:00:00Z"},{"recordId":"expired","available":true,"expireTime":"2026-11-01T12:00:00Z"},{"available":true,"expireTime":"2026-11-05T12:00:00Z"}],"weekResets":[{"recordId":"weekly","available":true,"expireTime":"2026-11-06T12:00:00Z"}]}}`

var resetNow = time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)

func resetResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestResetInventoryPreservesExactCardIDsAndAvailability(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != resetListURL || r.Header.Get("Authorization") != "private-key" {
			t.Fatal("unexpected inventory request")
		}
		return resetResponse(200, resetFixture), nil
	})}
	inventory, err := client.List(context.Background(), "private-key", resetNow)
	if err != nil || len(inventory.Credits) != 5 {
		t.Fatalf("credits=%d err=%v", len(inventory.Credits), err)
	}
	credit, err := SelectResetCredit(inventory, "5h", "", resetNow)
	if err != nil || credit.ID != "5h:earlier" {
		t.Fatalf("wrong earliest card: %s %v", credit.ID, err)
	}
	for _, card := range inventory.Credits {
		if card.ID == "" || card.ID == "5h:expired" {
			if card.Available {
				t.Fatal("missing-ID or expired card usable")
			}
		}
	}
	large, err := SelectResetCredit(inventory, "5h", "5h:900719925474099313", resetNow)
	if err != nil || string(large.recordID) != "900719925474099313" {
		t.Fatal("numeric record ID lost precision")
	}
	public, _ := json.Marshal(inventory)
	if strings.Contains(string(public), "private-key") || strings.Contains(string(public), "CAMPAIGN") {
		t.Fatal("private request data exposed in public inventory")
	}
}

func TestResetUseMatchesOfficialClientRequest(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				return resetResponse(200, resetFixture), nil
			}
			if r.Method != http.MethodPost || r.URL.String() != resetUseURL || r.Header.Get("Authorization") != "private-key" {
				t.Fatal("unexpected activation request")
			}
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["targetType"]) != `"PERSONAL"` || string(body["requestId"]) != `"same-request"` {
				t.Fatal("invalid activation body")
			}
			if window == "5h" && (string(body["recordId"]) != "900719925474099313" || string(body["grantType"]) != `"CAMPAIGN"` || string(body["resetType"]) != `"FIVE_HOUR"`) {
				t.Fatal("5h activation changed card metadata")
			}
			if window == "7d" && (string(body["recordId"]) != `"weekly"` || string(body["resetType"]) != `"WEEK"`) {
				t.Fatal("weekly activation changed card metadata")
			}
			return resetResponse(200, `{"code":200,"success":true}`), nil
		})}
		inventory, _ := client.List(context.Background(), "private-key", resetNow)
		id := "5h:900719925474099313"
		if window == "7d" {
			id = "7d:weekly"
		}
		card, _ := SelectResetCredit(inventory, window, id, resetNow)
		if err := client.Use(context.Background(), "private-key", card, "same-request", resetNow); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResetErrorsAreBoundedAndDoNotExposeSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		requestErr error
		ambiguous  bool
	}{
		{name: "auth", status: 401, body: "private-key"},
		{name: "limit", status: 429, body: "private-key"},
		{name: "business rejection", status: 200, body: `{"code":500,"success":false,"msg":"private-key"}`},
		{name: "server failure", status: 500, body: "private-key", ambiguous: true},
		{name: "request timeout", status: 408, body: "private-key", ambiguous: true},
		{name: "malformed success", status: 200, body: "private-key", ambiguous: true},
		{name: "inconsistent success", status: 200, body: `{"code":500,"success":true}`, ambiguous: true},
		{name: "oversized", status: 200, body: strings.Repeat("x", (1<<20)+1), ambiguous: true},
		{name: "transport", requestErr: errors.New("private-key"), ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return resetResponse(200, resetFixture), nil
				}
				return resetResponse(tc.status, tc.body), tc.requestErr
			})}
			inventory, _ := client.List(context.Background(), "private-key", resetNow)
			card, _ := SelectResetCredit(inventory, "5h", "5h:earlier", resetNow)
			err := client.Use(context.Background(), "private-key", card, "same-request", resetNow)
			var resetErr *ResetError
			if !errors.As(err, &resetErr) || resetErr.Ambiguous != tc.ambiguous || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("unsafe or incorrectly classified error: %v", err)
			}
		})
	}
}

func TestResetInventoryRejectsMalformedOrUnsuccessfulEnvelope(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"code":500,"success":false,"msg":"private-key"}`, `{"code":200,"success":true,"data":null}`, `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":"card","available":true,"expireTime":"invalid\u001bdate"}]}}`} {
		client := ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return resetResponse(200, body), nil })}
		_, err := client.List(context.Background(), "private-key", resetNow)
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatalf("malformed inventory accepted or leaked: %v", err)
		}
	}
}

func TestResetUnknownExpiryRequiresExactSelection(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		return resetResponse(200, `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":"local-time","available":true,"expireTime":"2026-11-04 12:00:00"}]}}`), nil
	})}
	inventory, err := client.List(context.Background(), "private-key", resetNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectResetCredit(inventory, "5h", "", resetNow); err == nil {
		t.Fatal("unknown timezone used for automatic card priority")
	}
	if card, err := SelectResetCredit(inventory, "5h", "5h:local-time", resetNow); err != nil || card.ExpiresAt != nil {
		t.Fatal("exact selection lost vendor availability or invented timezone")
	}
}
