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

func TestOpportunityAndUseResetAreExplicitPersonalMutations(t *testing.T) {
	calls := 0
	c := Client{Version: "3.17.2", DeviceMid: "cq-device-fixture", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "zcode.z.ai" || r.URL.RawQuery != "" {
			t.Fatal("mutation used wrong method or origin")
		}
		if r.Header.Get("Authorization") != "Bearer zcode-fixture-token" || r.Header.Get("X-Bigmodel-Authorization") != "business-fixture-token" || r.Header.Get("Bigmodel-Target-Type") != "PERSONAL" {
			t.Fatal("mutation used wrong credentials or scope")
		}
		if r.Header.Get("User-Agent") != "ZCode/3.17.2" || r.Header.Get("X-Device-Mid") != "cq-device-fixture" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Title") != "Z Code@electron" || r.Header.Get("HTTP-Referer") != "https://zcode.z.ai" || r.Header.Get("X-ZCode-App-Version") != "3.17.2" || r.Header.Get("X-App-Id") != "zcode" {
			t.Fatal("missing ZCode source identity or JSON content type")
		}
		if r.Header.Get("Bigmodel-Organization") != "" || r.Header.Get("Bigmodel-Project") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("unrelated identity crossed the mutation boundary")
		}
		body, err := httputil.ReadBody(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		if json.Unmarshal(body, &fields) != nil || fields["idempotency_key"] != "request-fixture" {
			t.Fatal("caller idempotency key not preserved")
		}
		switch calls {
		case 1:
			if r.URL.Path != "/api/v1/coding-plan/reset/opportunity" || len(fields) != 1 {
				t.Fatal("opportunity included unrelated fields")
			}
			return jsonResponse(`{"code":0,"data":{"granted":true}}`), nil
		case 2:
			if r.URL.Path != "/api/v1/coding-plan/reset/use" || len(fields) != 2 || fields["reset_type"] != "WEEK" {
				t.Fatal("reset did not preserve explicit window")
			}
			return jsonResponse(`{"code":0,"data":{"used":true}}`), nil
		default:
			t.Fatal("mutation made an implicit additional request")
			return nil, nil
		}
	})}
	got, err := c.Opportunity(context.Background(), testSession(), "request-fixture")
	if err != nil || !got.Granted || got.NextTryAt != nil {
		t.Fatalf("opportunity = %+v, err = %v", got, err)
	}
	if err := c.UseReset(context.Background(), testSession(), "WEEK", "request-fixture"); err != nil || calls != 2 {
		t.Fatalf("reset calls = %d, err = %v", calls, err)
	}
}

func TestOpportunityPreservesServerCooldown(t *testing.T) {
	for _, body := range []string{
		`{"code":3301,"data":{"granted":false,"next_try_at":1800001000123}}`,
		`{"code":3301,"success":false,"data":{"granted":false,"next_try_at":1800001000123}}`,
		`{"code":0,"data":{"granted":false,"next_try_at":1800001000123}}`,
	} {
		c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
		got, err := c.Opportunity(context.Background(), testSession(), "fixture")
		if err != nil || got.Granted || got.NextTryAt == nil || !got.NextTryAt.Equal(time.UnixMilli(1800001000123)) {
			t.Fatalf("cooldown = %+v, err = %v", got, err)
		}
	}
}

func TestUseResetAcceptsFiveHourAndMaximumKeyLength(t *testing.T) {
	key := strings.Repeat("a", 64)
	c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Key  string `json:"idempotency_key"`
			Type string `json:"reset_type"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Key != key || body.Type != "FIVE_HOUR" {
			t.Fatal("valid maximum key or explicit five-hour type changed")
		}
		return jsonResponse(`{"code":0,"data":{"used":true}}`), nil
	})}
	if err := c.UseReset(context.Background(), testSession(), "FIVE_HOUR", key); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsRejectMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		use  bool
	}{
		{"missing code", `{"data":{"granted":true}}`, false},
		{"missing data", `{"code":0}`, false},
		{"null data", `{"code":0,"data":null}`, false},
		{"string code", `{"code":"0","data":{"granted":true}}`, false},
		{"missing grant", `{"code":0,"data":{}}`, false},
		{"null grant", `{"code":0,"data":{"granted":null}}`, false},
		{"string grant", `{"code":0,"data":{"granted":"true"}}`, false},
		{"success false", `{"code":0,"success":false,"data":{"granted":true}}`, false},
		{"denied without cooldown", `{"code":3301,"data":{"granted":false}}`, false},
		{"denied with null cooldown", `{"code":3301,"data":{"granted":false,"next_try_at":null}}`, false},
		{"denied with string cooldown", `{"code":3301,"data":{"granted":false,"next_try_at":"1800001000123"}}`, false},
		{"denied with fractional cooldown", `{"code":3301,"data":{"granted":false,"next_try_at":1800001000123.5}}`, false},
		{"denied with negative cooldown", `{"code":3301,"data":{"granted":false,"next_try_at":-1}}`, false},
		{"denied with unrepresentable cooldown", `{"code":3301,"data":{"granted":false,"next_try_at":253402300800000}}`, false},
		{"denial code granting", `{"code":3301,"data":{"granted":true,"next_try_at":1800001000123}}`, false},
		{"grant with cooldown", `{"code":0,"data":{"granted":true,"next_try_at":1800001000123}}`, false},
		{"missing used", `{"code":0,"data":{}}`, true},
		{"not used", `{"code":0,"data":{"used":false}}`, true},
		{"string used", `{"code":0,"data":{"used":"true"}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(test.body), nil })}
			var err error
			if test.use {
				err = c.UseReset(context.Background(), testSession(), "FIVE_HOUR", "fixture")
			} else {
				_, err = c.Opportunity(context.Background(), testSession(), "fixture")
			}
			if err == nil {
				t.Fatal("malformed mutation response accepted")
			}
		})
	}
}

func TestOperationsValidateBeforeDispatch(t *testing.T) {
	for _, key := range []string{"", " ", "key\nvalue", strings.Repeat("a", 65)} {
		c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid idempotency key reached network")
			return nil, nil
		})}
		if _, err := c.Opportunity(context.Background(), testSession(), key); err == nil {
			t.Fatal("invalid opportunity key accepted")
		}
		if err := c.UseReset(context.Background(), testSession(), "FIVE_HOUR", key); err == nil {
			t.Fatal("invalid reset key accepted")
		}
	}
	for _, session := range []Session{{}, {Family: "bigmodel", ZCodeJWT: "fixture", BusinessJWT: "fixture"}, {Family: "zai", ZCodeJWT: "fixture"}, {Family: "zai", ZCodeJWT: "fixture", BusinessJWT: "bad\nheader"}} {
		c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid session reached network")
			return nil, nil
		})}
		if _, err := c.Opportunity(context.Background(), session, "fixture"); err == nil {
			t.Fatal("invalid opportunity session accepted")
		}
		if err := c.UseReset(context.Background(), session, "WEEK", "fixture"); err == nil {
			t.Fatal("invalid reset session accepted")
		}
	}
	c := Client{Version: "dev", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid window or cancelled context reached network")
		return nil, nil
	})}
	if err := c.UseReset(context.Background(), testSession(), "5h", "fixture"); err == nil {
		t.Fatal("invalid reset type accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Opportunity(ctx, testSession(), "fixture"); err == nil {
		t.Fatal("cancelled opportunity dispatched")
	}
}

func TestOperationErrorsAreBoundedAndRetainAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name      string
		do        doerFunc
		ambiguous bool
	}{
		{"transport", func(*http.Request) (*http.Response, error) { return nil, errors.New("zcode-fixture-token") }, true},
		{"body read", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(failureReader{})}, nil
		}, true},
		{"oversized", func(*http.Request) (*http.Response, error) {
			return jsonResponse(strings.Repeat("x", httputil.MaxResponseBody+1)), nil
		}, true},
		{"malformed", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"data":{"used":false},"msg":"business-fixture-token"}`), nil
		}, true},
		{"rejected", func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":2007,"msg":"business-fixture-token","data":{}}`), nil
		}, true},
		{"unavailable", func(*http.Request) (*http.Response, error) {
			r := jsonResponse("business-fixture-token")
			r.StatusCode = http.StatusServiceUnavailable
			return r, nil
		}, true},
		{"unauthorised", func(*http.Request) (*http.Response, error) {
			r := jsonResponse("business-fixture-token")
			r.StatusCode = http.StatusUnauthorized
			return r, nil
		}, false},
		{"missing response", func(*http.Request) (*http.Response, error) { return nil, nil }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Client{Version: "dev", DeviceMid: "fixture", HTTP: test.do}
			err := c.UseReset(context.Background(), testSession(), "FIVE_HOUR", "fixture")
			var operationErr *OperationError
			if !errors.As(err, &operationErr) || operationErr.Ambiguous != test.ambiguous || strings.Contains(err.Error(), "fixture-token") {
				t.Fatalf("unexpected error classification: %v", err)
			}
		})
	}
}

func TestOperationsNeverFollowAuthenticatedRedirect(t *testing.T) {
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("authenticated mutation redirect followed")
		}
		r := jsonResponse("")
		r.StatusCode = http.StatusTemporaryRedirect
		r.Header.Set("Location", "https://other.example/use")
		return r, nil
	})}
	c := Client{Version: "dev", DeviceMid: "fixture", HTTP: httpClient}
	if err := c.UseReset(context.Background(), testSession(), "WEEK", "fixture"); err == nil || calls != 1 || httpClient.CheckRedirect != nil {
		t.Fatal("redirect did not fail closed or caller client changed")
	}
}
