package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const projectCustomerFixture = `{"code":200,"data":{"organizations":[{"organizationId":"own-org","organizationName":"默认机构","projects":[{"projectId":"team","projectType":2},{"projectId":"own-project","projectName":"默认项目","projectType":1}]}]}}`
const projectKeysFixture = `{"code":"200","data":[null,{"name":"unrelated"},{"name":"zcode-api-key","apiKey":"key-id"}]}`

func projectLoginFixture() LoginSession {
	return LoginSession{ID: "account-user", UserID: "user", ZCodeJWT: "zcode-secret", BusinessJWT: "business-secret", RawOAuth: "oauth-secret"}
}

func TestProjectTokenResolvedWithoutCopyingOrPersistingSecret(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint("create=", create), func(t *testing.T) {
			var calls atomic.Int32
			now := time.Unix(1800000000, 0)
			client := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return now }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.Scheme != "https" || r.URL.Host != "api.z.ai" || r.Header.Get("User-Agent") != "ZCode/3.17.2" {
					t.Error("wrong fixed origin or source identity")
				}
				if strings.Contains(r.URL.Path, "/copy/") || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
					t.Error("legacy secret or unrelated authentication used")
				}
				switch r.URL.Path {
				case "/api/biz/customer/getCustomerInfo":
					if r.Method != "GET" || r.Header.Get("Authorization") != "business-secret" {
						t.Error("customerInfo did not use raw business JWT")
					}
					return jsonResponse(projectCustomerFixture), nil
				case "/api/biz/v1/organization/own-org/projects/own-project/api_keys":
					if r.Header.Get("Authorization") != "Bearer business-secret" {
						t.Error("personal key metadata did not use Bearer business JWT")
					}
					if r.Method == "GET" {
						if create {
							return jsonResponse(`{"code":0,"data":[]}`), nil
						}
						return jsonResponse(projectKeysFixture), nil
					}
					var body map[string]any
					if !create || r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["name"] != "zcode-api-key" || body["usageScene"] != float64(1) {
						t.Error("created wrong key type or scope")
					}
					return jsonResponse(`{"code":200,"data":{"apiKey":"key-id","name":"zcode-api-key"}}`), nil
				case "/api/biz/v1/organization/own-org/projects/own-project/api_keys/key-id/access_tokens":
					var body map[string]string
					if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer business-secret" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["clientType"] != "zcode" || body["clientVersion"] != "3.17.2" {
						t.Error("PAT issue contract changed")
					}
					return jsonResponse(`{"code":"0","data":{"accessToken":"project-secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`), nil
				default:
					t.Error("unexpected token exchange or secret-copy request")
					return nil, errors.New("business-secret")
				}
			})}
			for i := 0; i < 2; i++ {
				got, err := client.Resolve(context.Background(), projectLoginFixture())
				if err != nil || got != "project-secret" {
					t.Fatalf("resolve failed: %v", err)
				}
			}
			want := int32(3)
			if create {
				want++
			}
			if calls.Load() != want {
				t.Fatalf("calls = %d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestProjectTokenCacheUsesEarliestExpiryAndRefreshesOnlyToken(t *testing.T) {
	var now atomic.Int64
	now.Store(1800000000)
	var issues atomic.Int32
	var metadata atomic.Int32
	c := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(now.Load(), 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getCustomerInfo"):
			metadata.Add(1)
			return jsonResponse(projectCustomerFixture), nil
		case strings.HasSuffix(r.URL.Path, "/api_keys"):
			metadata.Add(1)
			return jsonResponse(projectKeysFixture), nil
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			issue := issues.Add(1)
			return jsonResponse(fmt.Sprintf(`{"code":0,"data":{"enable":true,"accessToken":"token-%d","tokenType":"Bearer","expiresIn":300,"expiresAt":%d}}`, issue, now.Load()+600)), nil
		}
		return nil, errors.New("unexpected request")
	})}
	first, err := c.Resolve(context.Background(), projectLoginFixture())
	if err != nil {
		t.Fatal(err)
	}
	now.Store(1800000179)
	if token, err := c.Resolve(context.Background(), projectLoginFixture()); err != nil || token != first || issues.Load() != 1 {
		t.Fatal("cache refreshed before source lead time")
	}
	now.Store(1800000180)
	if token, err := c.Resolve(context.Background(), projectLoginFixture()); err != nil || token == first || issues.Load() != 2 || metadata.Load() != 2 {
		t.Fatal("early refresh re-resolved metadata or ignored earliest expiry")
	}
}

func TestProjectTokenSingleflightDoesNotInheritCallerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	c := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(1800000000, 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return jsonResponse(projectCustomerFixture), nil
		}
		if strings.HasSuffix(r.URL.Path, "/api_keys") {
			return jsonResponse(projectKeysFixture), nil
		}
		return jsonResponse(`{"code":0,"data":{"accessToken":"project-secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.Resolve(ctx, projectLoginFixture()); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("caller did not cancel independently")
	}
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			token, err := c.Resolve(context.Background(), projectLoginFixture())
			if err == nil && token != "project-secret" {
				err = errors.New("wrong token")
			}
			results <- err
		}()
	}
	close(release)
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("singleflight issued %d calls, want 3", calls.Load())
	}
}

func TestProjectTokenRejectsInvalidServerContracts(t *testing.T) {
	for _, bad := range []string{
		`{"code":0,"data":{"enable":false}}`,
		`{"code":0,"data":{"enable":null,"accessToken":"secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`,
		`{"code":0,"data":{"accessToken":"","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`,
		`{"code":0,"data":{"accessToken":"bad\nsecret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`,
		`{"code":0,"data":{"accessToken":"secret","tokenType":"Basic","expiresIn":600,"expiresAt":1800000600}}`,
		`{"code":0,"data":{"accessToken":"secret","tokenType":"Bearer","expiresIn":"600","expiresAt":1800000600}}`,
		`{"code":0,"data":{"accessToken":"secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1799999999}}`,
		`{"code":0,"data":{"accessToken":"secret","tokenType":"Bearer","expiresIn":0,"expiresAt":1800000600}}`,
		`{"code":201,"msg":"secret","data":{}}`,
	} {
		c := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(1800000000, 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
				return jsonResponse(projectCustomerFixture), nil
			}
			if strings.HasSuffix(r.URL.Path, "/api_keys") {
				return jsonResponse(projectKeysFixture), nil
			}
			return jsonResponse(bad), nil
		})}
		if token, err := c.Resolve(context.Background(), projectLoginFixture()); err == nil || token != "" || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid PAT accepted or credential exposed")
		}
	}
}

func TestProjectTokenDoesNotCreateForMissingProjectOrMalformedNamedKey(t *testing.T) {
	for _, test := range []struct{ customer, keys string }{
		{`{"code":0,"data":{"organizations":[]}}`, ""},
		{strings.Replace(projectCustomerFixture, `"projectType":1`, `"projectType":"2"`, 1), ""},
		{projectCustomerFixture, `{"code":0,"data":[{"name":"zcode-api-key"}]}`},
		{projectCustomerFixture, `{"code":0,"data":[{"name":"zcode-api-key","apiKey":""}]}`},
	} {
		c := ProjectTokenClient{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Error("invalid metadata caused mutation")
			}
			if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
				return jsonResponse(test.customer), nil
			}
			return jsonResponse(test.keys), nil
		})}
		if _, err := c.Resolve(context.Background(), projectLoginFixture()); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}

func TestProjectTokenBusinessExchangeOnlyWhenNeeded(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			var exchanges, issues atomic.Int32
			session := projectLoginFixture()
			if mode == "missing" {
				session.BusinessJWT = ""
			}
			if mode == "expired" {
				session.BusinessExpiresAt = 1799999999
			}
			client := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(1800000000, 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/auth/z/login" {
					exchanges.Add(1)
					var body struct {
						Token string `json:"token"`
					}
					if r.Header.Get("Authorization") != "" || r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != session.RawOAuth {
						t.Error("business exchange changed OAuth contract")
					}
					return jsonResponse(`{"code":"200","data":{"access_token":"refreshed-business"}}`), nil
				}
				if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
					return jsonResponse(projectCustomerFixture), nil
				}
				if strings.HasSuffix(r.URL.Path, "/api_keys") {
					return jsonResponse(projectKeysFixture), nil
				}
				if mode == "rejected" && issues.Add(1) == 1 {
					r := jsonResponse("business-secret")
					r.StatusCode = 401
					return r, nil
				}
				if r.Header.Get("Authorization") != "Bearer refreshed-business" {
					t.Error("PAT used stale business JWT")
				}
				return jsonResponse(`{"code":0,"data":{"accessToken":"project-secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`), nil
			})}
			if token, err := client.Resolve(context.Background(), session); err != nil || token != "project-secret" || exchanges.Load() != 1 {
				t.Fatalf("exchange result failed: %v", err)
			}
		})
	}
}

func TestProjectTokenTransientRefreshNeverExtendsExpiry(t *testing.T) {
	var now atomic.Int64
	now.Store(1800000000)
	var issues atomic.Int32
	client := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(now.Load(), 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
			return jsonResponse(projectCustomerFixture), nil
		}
		if strings.HasSuffix(r.URL.Path, "/api_keys") {
			return jsonResponse(projectKeysFixture), nil
		}
		if issues.Add(1) > 1 {
			rsp := jsonResponse("project-secret")
			rsp.StatusCode = 503
			return rsp, nil
		}
		return jsonResponse(`{"code":0,"data":{"accessToken":"project-secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`), nil
	})}
	if _, err := client.Resolve(context.Background(), projectLoginFixture()); err != nil {
		t.Fatal(err)
	}
	now.Store(1800000480)
	if token, err := client.Resolve(context.Background(), projectLoginFixture()); err != nil || token != "project-secret" {
		t.Fatal("transient early refresh discarded unexpired PAT")
	}
	now.Store(1800000481)
	if _, err := client.Resolve(context.Background(), projectLoginFixture()); err != nil || issues.Load() != 2 {
		t.Fatal("transient refresh backoff not respected")
	}
	now.Store(1800000600)
	if token, err := client.Resolve(context.Background(), projectLoginFixture()); err == nil || token != "" {
		t.Fatal("expired PAT survived transient failure")
	}
}

func TestProjectTokenRotationRejectsLateCredentialResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := ProjectTokenClient{Version: "3.17.2", now: func() time.Time { return time.Unix(1800000000, 0) }, HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
			return jsonResponse(projectCustomerFixture), nil
		}
		if strings.HasSuffix(r.URL.Path, "/api_keys") {
			return jsonResponse(projectKeysFixture), nil
		}
		if r.Header.Get("Authorization") == "Bearer business-secret" {
			once.Do(func() { close(started) })
			<-release
		}
		return jsonResponse(`{"code":0,"data":{"accessToken":"project-secret","tokenType":"Bearer","expiresIn":600,"expiresAt":1800000600}}`), nil
	})}
	old := make(chan error, 1)
	go func() { _, err := c.Resolve(context.Background(), projectLoginFixture()); old <- err }()
	<-started
	current := projectLoginFixture()
	current.BusinessJWT = "new-business"
	if _, err := c.Resolve(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-old; err == nil {
		t.Fatal("late result crossed login generation")
	}
	if _, err := c.Resolve(context.Background(), current); err != nil {
		t.Fatal("late old result invalidated current token")
	}
}

func TestProjectTokenPanicIsRecoveredAndErrorsRedacted(t *testing.T) {
	c := ProjectTokenClient{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { panic("business-secret") })}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.Resolve(ctx, projectLoginFixture())
		cancel()
		if err == nil || strings.Contains(err.Error(), "secret") || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("panic leaked credential or left flight blocked")
		}
	}
}

func TestProjectTokenRequestBoundaryRejectsRedirectsAndOversizedResponses(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized", "malformed", "missing body"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				switch mode {
				case "redirect":
					r := jsonResponse("")
					r.StatusCode = http.StatusTemporaryRedirect
					r.Header.Set("Location", "https://other.example/customer")
					return r, nil
				case "oversized":
					return jsonResponse(strings.Repeat("x", (64<<10)+1)), nil
				case "malformed":
					return jsonResponse(`{"code":null,"data":{}}`), nil
				default:
					return &http.Response{StatusCode: 200}, nil
				}
			})}
			c := ProjectTokenClient{Version: "3.17.2", HTTP: httpClient}
			if token, err := c.Resolve(context.Background(), projectLoginFixture()); err == nil || token != "" || calls.Load() != 1 || httpClient.CheckRedirect != nil {
				t.Fatal("invalid boundary response accepted or caller HTTP policy changed")
			}
		})
	}
}

func TestProjectTokenInvalidLoginOrCancelledCallerDoesNotDispatch(t *testing.T) {
	client := ProjectTokenClient{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { t.Error("invalid login reached network"); return nil, nil })}
	for _, session := range []LoginSession{{}, {UserID: "user"}, {UserID: "user", BusinessJWT: "bad\nheader"}} {
		if _, err := client.Resolve(context.Background(), session); err == nil {
			t.Fatal("invalid login accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Resolve(ctx, projectLoginFixture()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled caller started credential resolution")
	}
}
