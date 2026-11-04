package zcode

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
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failureReader struct{}

func (failureReader) Read([]byte) (int, error) { return 0, errors.New("zcode-fixture-token") }

func testSession() Session {
	return Session{Family: "zai", ZCodeJWT: "zcode-fixture-token", BusinessJWT: "business-fixture-token"}
}

func jsonResponse(data string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(data))}
}

const balanceFixture = `{"code":0,"data":{"server_time":1800000000,"plans":[{"user_plan_id":"p1","plan_id":"start","status":"active","ends_at":"1799999999"}],"balances":[{"bucket_id":"b1","user_plan_id":"p1","show_name":"GLM-5.3","capabilities":["model:glm-5.3"],"total_units":"3000000","remaining_units":null,"available_units":"9007199254740993","expires_at":1800001000}]}}`

const resetFixture = `{"code":0,"data":{"available_five_hour_resets":[{"expire_at":1800001000123}],"available_week_resets":[],"latest_five_hour_reset_history":null,"latest_week_reset_history":{"used_at":1799999999123},"has_unread_history":true}}`

func TestClientBalanceAndResetAuthenticationSeparation(t *testing.T) {
	calls := 0
	c := Client{Version: "3.17.2", DeviceMid: "cq-device-fixture", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "zcode.z.ai" || r.Body != nil {
			t.Fatal("unexpected request method, host or body")
		}
		if r.Header.Get("Authorization") != "Bearer zcode-fixture-token" || r.Header.Get("User-Agent") != "ZCode/3.17.2" || r.Header.Get("X-Title") != "Z Code@electron" || r.Header.Get("X-Device-Mid") != "cq-device-fixture" || r.Header.Get("HTTP-Referer") != "https://zcode.z.ai" || r.Header.Get("X-ZCode-App-Version") != "3.17.2" {
			t.Fatal("missing ZCode source identity or authentication")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Fatal("unrelated authentication crossed the request boundary")
		}
		switch calls {
		case 1:
			if r.URL.Path != "/api/v1/zcode-plan/billing/balance" || r.URL.RawQuery != "app_version=3.17.2" || r.Header.Get("X-Bigmodel-Authorization") != "" || r.Header.Get("Bigmodel-Target-Type") != "" {
				t.Fatal("balance request leaked business authentication or used wrong endpoint")
			}
			return jsonResponse(balanceFixture), nil
		case 2:
			if r.URL.Path != "/api/v1/coding-plan/reset/status" || r.URL.RawQuery != "" || r.Header.Get("X-Bigmodel-Authorization") != "business-fixture-token" || r.Header.Get("Bigmodel-Target-Type") != "PERSONAL" {
				t.Fatal("reset request used wrong scope or business authentication")
			}
			return jsonResponse(resetFixture), nil
		}
		t.Fatal("unexpected additional request")
		return nil, nil
	})}
	balance, err := c.Balance(context.Background(), testSession())
	if err != nil || string(balance.Plans[0].EndsAt) != `"1799999999"` || string(balance.Balances[0].RemainingUnits) != "null" || string(balance.Balances[0].AvailableUnits) != `"9007199254740993"` {
		t.Fatal("balance changed unknown amounts or expiry metadata")
	}
	status, err := c.ResetStatus(context.Background(), testSession())
	if err != nil || !status.HasUnreadHistory || status.AvailableFiveHourResets[0].ExpireAt.String() != "1800001000123" || status.LatestWeekResetHistory.UsedAt.String() != "1799999999123" || status.LatestFiveHourResetHistory != nil {
		t.Fatal("reset status did not preserve schema and timestamp precision")
	}
}

func TestBalanceReferenceTimePreservesServerAndExpiry(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, test := range []struct {
		name, body, date string
		want             time.Time
	}{
		{"server before HTTP date", balanceFixture, "Wed, 16 Sep 2026 12:00:00 GMT", time.Unix(1800000000, 0)},
		{"HTTP fallback", `{"code":0,"data":{"plans":[],"balances":[]}}`, "Wed, 16 Sep 2026 12:00:00 GMT", time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)},
		{"local fallback", `{"code":0,"data":{"server_time":null}}`, "invalid", now},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Client{Version: "dev", DeviceMid: "fixture", Now: func() time.Time { return now }, HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
				r := jsonResponse(test.body)
				r.Header.Set("Date", test.date)
				return r, nil
			})}
			got, err := c.Balance(context.Background(), testSession())
			if err != nil || !got.ReferenceTime.Equal(test.want) {
				t.Fatalf("reference time = %v, err = %v", got.ReferenceTime, err)
			}
			if test.name == "server before HTTP date" && got.Plans[0].Status != "active" {
				t.Fatal("read-only client invented plan expiry state")
			}
		})
	}
}

func TestClientErrorsBoundedAndRedacted(t *testing.T) {
	for _, test := range []struct {
		name string
		do   doerFunc
	}{
		{"transport", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("zcode-fixture-token business-fixture-token")
		}},
		{"HTTP rejection", func(*http.Request) (*http.Response, error) {
			r := jsonResponse("zcode-fixture-token")
			r.StatusCode = 401
			return r, nil
		}},
		{"oversized", func(*http.Request) (*http.Response, error) {
			return jsonResponse(strings.Repeat("x", httputil.MaxResponseBody+1)), nil
		}},
		{"read failed", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(failureReader{})}, nil
		}},
		{"missing response", func(*http.Request) (*http.Response, error) { return nil, nil }},
		{"missing body", func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: http.StatusOK}, nil }},
		{"bad JSON", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"data":"zcode-fixture-token"}`), nil
		}},
		{"missing code", func(*http.Request) (*http.Response, error) { return jsonResponse(`{"data":{}}`), nil }},
		{"string code", func(*http.Request) (*http.Response, error) { return jsonResponse(`{"code":"0","data":{}}`), nil }},
		{"unsuccessful", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":2007,"msg":"zcode-fixture-token","data":{}}`), nil
		}},
		{"success false", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"success":false,"data":{}}`), nil
		}},
		{"missing data", func(*http.Request) (*http.Response, error) { return jsonResponse(`{"code":0}`), nil }},
		{"invalid quantity", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"data":{"balances":[{"available_units":false}]}}`), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Client{Version: "dev", DeviceMid: "fixture", HTTP: test.do}
			_, err := c.Balance(context.Background(), testSession())
			if err == nil || strings.Contains(err.Error(), "fixture-token") {
				t.Fatal("expected bounded, secret-free error")
			}
		})
	}
}

func TestResetStatusRejectsMalformedSchema(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{}}`,
		strings.Replace(resetFixture, `"available_week_resets":[]`, `"available_week_resets":null`, 1),
		strings.Replace(resetFixture, `"has_unread_history":true`, `"has_unread_history":null`, 1),
		strings.Replace(resetFixture, `"latest_five_hour_reset_history":null,`, ``, 1),
		strings.Replace(resetFixture, `1800001000123`, `-1`, 1),
		strings.Replace(resetFixture, `1799999999123`, `"1799999999123"`, 1),
	} {
		c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
		if _, err := c.ResetStatus(context.Background(), testSession()); err == nil {
			t.Fatal("malformed reset status accepted")
		}
	}
}

func TestClientRejectsMissingCredentialsAndCrossFamilyBeforeRequest(t *testing.T) {
	for _, session := range []Session{{}, {Family: "bigmodel", ZCodeJWT: "fixture", BusinessJWT: "fixture"}, {Family: "zai", ZCodeJWT: "bad\nheader", BusinessJWT: "fixture"}} {
		c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid session reached network")
			return nil, nil
		})}
		if _, err := c.Balance(context.Background(), session); err == nil {
			t.Fatal("invalid balance session accepted")
		}
		if _, err := c.ResetStatus(context.Background(), session); err == nil {
			t.Fatal("invalid reset session accepted")
		}
	}
	s := testSession()
	s.BusinessJWT = ""
	c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing business token reached network")
		return nil, nil
	})}
	if _, err := c.ResetStatus(context.Background(), s); err == nil {
		t.Fatal("reset accepted missing business token")
	}
}

func TestClientNeverFollowsAuthenticatedRedirect(t *testing.T) {
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("authenticated redirect followed")
		}
		rsp := jsonResponse("")
		rsp.StatusCode = http.StatusTemporaryRedirect
		rsp.Header.Set("Location", "https://other.example/account")
		return rsp, nil
	})}
	c := Client{Version: "dev", DeviceMid: "fixture", HTTP: httpClient}
	if _, err := c.ResetStatus(context.Background(), testSession()); err == nil || calls != 1 {
		t.Fatal("redirect did not fail closed")
	}
	if httpClient.CheckRedirect != nil {
		t.Fatal("caller HTTP client was mutated")
	}
}

func TestBalancePublicJSONContainsNoAuthentication(t *testing.T) {
	c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(balanceFixture), nil })}
	b, err := c.Balance(context.Background(), testSession())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(b)
	if err != nil || strings.Contains(string(encoded), "fixture-token") {
		t.Fatal("balance JSON exposed authentication")
	}
}
