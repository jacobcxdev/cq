package zcode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const marketingOfferFixture = `{"code":0,"data":{"server_time":1800000000,"language":"en-US","deliveries":[{"campaign_id":"gift-campaign","priority":2,"resource_position":"popup","popup":{"layout":"v1","title":{"format":"plaintext","content":"Gift"},"description":{"format":"plaintext","content":"Daily tokens"},"hero":{"type":"image","args":{"amountValue":"3000000","amountUnit":"Tokens"},"image":{"default":{"src":"https://example.invalid/offer.png","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},"buttons":[{"text":{"format":"plaintext","content":"Claim"},"action":{"type":"claim_zcode_plan","args":{"plan_id":"offered-plan"}}}]}}]}}`

func TestClaimConfigOffersAndExplicitSubmission(t *testing.T) {
	calls := 0
	c := Client{Version: "3.17.2", DeviceMid: "11111111-1111-4111-8111-111111111111", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "zcode.z.ai" || r.Header.Get("User-Agent") != "ZCode/3.17.2" || r.Header.Get("X-Device-Mid") != "11111111-1111-4111-8111-111111111111" || r.Header.Get("X-Platform") != runtime.GOOS+"-"+runtime.GOARCH {
			t.Fatal("wrong source identity or origin")
		}
		if r.Header.Get("X-Bigmodel-Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Fatal("unrelated credentials entered claim flow")
		}
		switch calls {
		case 1:
			if r.Method != "GET" || r.URL.Path != "/api/v1/client/configs" || r.URL.Query().Get("app_version") != "3.17.2" || r.URL.Query().Get("platform") != runtime.GOOS+"-"+runtime.GOARCH || r.Header.Get("Authorization") != "" {
				t.Fatal("config was not a public, versioned GET")
			}
			return jsonResponse(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"fixture-prefix","region":"sgp","sceneId":"fixture-scene","skip_model_request":true}}}}`), nil
		case 2:
			if r.Method != "GET" || r.URL.Path != "/api/v1/marketing/touch" || r.URL.Query().Get("seq") == "" || r.Header.Get("X-Client-Language") != "en-US" || r.Header.Get("Authorization") != "Bearer zcode-fixture-token" {
				t.Fatal("offer read used wrong endpoint or account")
			}
			return jsonResponse(marketingOfferFixture), nil
		case 3:
			if r.Method != "POST" || r.URL.Path != "/api/v1/zcode-plan/billing/claim" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer zcode-fixture-token" || r.Header.Get("X-Aliyun-Captcha-Verify-Param") != "fixture-sdk-result" || r.Header.Get("X-Aliyun-Captcha-Verify-Region") != "sgp" {
				t.Fatal("claim did not preserve the explicit account, plan and proof")
			}
			body, err := httputil.ReadBody(r.Body)
			if err != nil || string(body) != `{"plan_id":"offered-plan"}` {
				t.Fatal("claim sent extra or incorrect fields")
			}
			return jsonResponse(`{"code":"0","data":{"server_time":1800000000,"plan":{"plan_id":"offered-plan","user_plan_id":"owned-plan","status":"active","ends_at":1800086400}}}`), nil
		default:
			t.Fatal("claim flow made an implicit additional request")
			return nil, nil
		}
	})}
	cfg, err := c.CaptchaConfig(context.Background())
	if err != nil || !cfg.Enabled || cfg.Prefix != "fixture-prefix" || cfg.SceneID != "fixture-scene" || cfg.Region != "sgp" || !cfg.SkipModelRequest {
		t.Fatalf("config = %+v, err = %v", cfg, err)
	}
	offers, err := c.PlanOffers(context.Background(), testSession())
	if err != nil || len(offers) != 1 || offers[0].PlanID != "offered-plan" || offers[0].CampaignID != "gift-campaign" || !strings.Contains(string(offers[0].Details), `"amountValue":"3000000"`) {
		t.Fatalf("offers = %+v, err = %v", offers, err)
	}
	result, err := c.ClaimPlan(context.Background(), testSession(), offers[0].PlanID, VerificationProof{Param: "fixture-sdk-result", Region: cfg.Region})
	if err != nil || result.Plan.UserPlanID != "owned-plan" || string(result.ServerTime) != "1800000000" || calls != 3 {
		t.Fatalf("claim = %+v, calls = %d, err = %v", result, calls, err)
	}
}

func TestClaimRejectsInvalidInputsWithoutDispatch(t *testing.T) {
	c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid claim reached network")
		return nil, nil
	})}
	for _, proof := range []VerificationProof{{}, {Param: " ", Region: "sgp"}, {Param: "bad\nproof", Region: "sgp"}, {Param: strings.Repeat("p", 16385), Region: "sgp"}, {Param: "fixture", Region: "wrong"}} {
		if _, err := c.ClaimPlan(context.Background(), testSession(), "offered", proof); err == nil {
			t.Fatal("invalid proof accepted")
		}
	}
	for _, id := range []string{"", " ", "bad\nplan", strings.Repeat("p", 257)} {
		if _, err := c.ClaimPlan(context.Background(), testSession(), id, VerificationProof{Param: "fixture", Region: "sgp"}); err == nil {
			t.Fatal("invalid plan accepted")
		}
	}
	if _, err := c.ClaimPlan(context.Background(), Session{Family: "bigmodel", ZCodeJWT: "fixture"}, "offered", VerificationProof{Param: "fixture", Region: "sgp"}); err == nil {
		t.Fatal("cross-family claim accepted")
	}
}

func TestClaimDoesNotRetryAndRequiresActualSuccessEnvelope(t *testing.T) {
	for _, body := range []string{`{}`, `{"code":0}`, `{"code":0,"data":{"plan":null}}`, `{"code":0,"data":{"plan":[]}}`, `{"code":0,"success":false,"data":{"plan":{}}}`, `{"code":3007,"msg":"fixture-sdk-result","data":{}}`, `{"code":"200","data":{"plan":{}}}`, `{"code":0.0,"data":{"plan":{}}}`} {
		calls := 0
		c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return jsonResponse(body), nil
		})}
		_, err := c.ClaimPlan(context.Background(), testSession(), "offered", VerificationProof{Param: "fixture-sdk-result", Region: "sgp"})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "fixture-sdk-result") {
			t.Fatal("invalid claim response accepted, retried or exposed")
		}
	}
	calls := 0
	c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("fixture-sdk-result zcode-fixture-token")
	})}
	_, err := c.ClaimPlan(context.Background(), testSession(), "offered", VerificationProof{Param: "fixture-sdk-result", Region: "sgp"})
	var operation *OperationError
	if !errors.As(err, &operation) || !operation.Ambiguous || calls != 1 || strings.Contains(err.Error(), "fixture") {
		t.Fatal("uncertain claim was not retained as an ambiguous single attempt")
	}
}

func TestCaptchaConfigRejectsMalformedOrUnusableFields(t *testing.T) {
	for _, captcha := range []string{`null`, `{}`, `{"enabled":null,"region":"sgp","prefix":"fixture","sceneId":"scene"}`, `{"enabled":true,"region":"sgp","prefix":"fixture","sceneId":"scene","skip_model_request":null}`, `{"enabled":true,"region":"sgp","prefix":"fixture"}`, `{"enabled":"true","region":"sgp","prefix":"fixture","sceneId":"scene"}`} {
		c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"data":{"configs":{"captcha":` + captcha + `}}}`), nil
		})}
		if _, err := c.CaptchaConfig(context.Background()); err == nil {
			t.Fatalf("malformed captcha config accepted: %s", captcha)
		}
	}
	for _, captcha := range []string{`{"enabled":false}`, `{"region":"sgp","prefix":"fixture","sceneId":"scene"}`} {
		c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"code":0,"data":{"configs":{"captcha":` + captcha + `}}}`), nil
		})}
		if _, err := c.CaptchaConfig(context.Background()); err != nil {
			t.Fatalf("valid disabled or default-enabled config rejected: %v", err)
		}
	}
}

func TestClaimHTTPRejectionPreservesVendorCode(t *testing.T) {
	c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		r := jsonResponse(`{"code":3007,"msg":"fixture-sdk-result"}`)
		r.StatusCode = http.StatusBadRequest
		return r, nil
	})}
	_, err := c.ClaimPlan(context.Background(), testSession(), "offered", VerificationProof{Param: "fixture-sdk-result", Region: "sgp"})
	var operation *OperationError
	if !errors.As(err, &operation) || operation.VendorCode != 3007 || operation.HTTPStatus != 400 || operation.Ambiguous || strings.Contains(err.Error(), "fixture") {
		t.Fatalf("claim rejection lost its safe classification: %v", err)
	}
}

func TestClaimOffersUseOnlyCurrentMarketingActions(t *testing.T) {
	for _, test := range []struct {
		body string
		want int
		bad  bool
	}{
		{`{"code":0,"data":{"server_time":1800000000,"language":"en-US","deliveries":[]}}`, 0, false},
		{`{"code":0,"data":{"plans":[{"plan_id":"retired-preview"}]}}`, 0, true},
		{`{"code":0,"data":{"server_time":1800000000,"language":"en-US","deliveries":null}}`, 0, true},
		{strings.ReplaceAll(marketingOfferFixture, `"claim_zcode_plan"`, `"claim_plan"`), 0, false},
		{strings.ReplaceAll(marketingOfferFixture, `"plan_id":"offered-plan"`, `"plan_id":""`), 0, false},
		{strings.ReplaceAll(marketingOfferFixture, `"resource_position":"popup"`, `"resource_position":"card"`), 0, false},
		{marketingOfferFixture, 1, false},
	} {
		c := Client{Version: "3.17.2", DeviceMid: "11111111-1111-4111-8111-111111111111", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(test.body), nil })}
		got, err := c.PlanOffers(context.Background(), testSession())
		if (err != nil) != test.bad || len(got) != test.want {
			t.Fatalf("marketing action qualification mismatch: count=%d err=%v", len(got), err)
		}
	}
}

func TestClaimRequestBoundsCancellationAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name string
		do   doerFunc
	}{
		{"oversized", func(*http.Request) (*http.Response, error) {
			return jsonResponse(strings.Repeat("x", httputil.MaxResponseBody+1)), nil
		}},
		{"missing response", func(*http.Request) (*http.Response, error) { return nil, nil }},
		{"missing body", func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200}, nil }},
		{"read error", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(failureReader{})}, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: test.do}
			if _, err := c.CaptchaConfig(context.Background()); err == nil || strings.Contains(err.Error(), "fixture-token") {
				t.Fatal("unbounded or unredacted config response")
			}
		})
	}
	calls := 0
	c := Client{Version: "3.17.2", DeviceMid: "fixture", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("claim request followed a redirect")
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("claim request has no bounded deadline")
		}
		rsp := jsonResponse(`{}`)
		rsp.StatusCode = 302
		rsp.Header.Set("Location", "https://example.invalid/other")
		return rsp, nil
	})}}
	if _, err := c.ClaimPlan(context.Background(), testSession(), "offered", VerificationProof{Param: "fixture", Region: "sgp"}); err == nil || calls != 1 {
		t.Fatal("redirected claim accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CaptchaConfig(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancelled config request reached network")
	}
}
