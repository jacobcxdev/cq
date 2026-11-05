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
)

type oauthDoer func(*http.Request) (*http.Response, error)

func (f oauthDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type oauthRoundTripper func(*http.Request) (*http.Response, error)

func (f oauthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oauthResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestOAuthLoginKeptPollingAndAccountCredentialsSeparate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var pollBearer string
	calls, waits, opens := 0, 0, 0
	client := OAuthClient{
		Version: "3.17.2", DeviceMid: "local-device", Now: func() time.Time { return now },
		OpenURL: func(_ context.Context, value string) error {
			opens++
			if value != "https://chat.z.ai/api/oauth/authorize?state=flow-state" {
				t.Fatal("wrong authorisation URL")
			}
			return nil
		},
		Wait: func(_ context.Context, duration time.Duration) error { waits++; now = now.Add(duration); return nil },
	}
	client.HTTP = oauthDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != "ZCode/3.17.2" {
			t.Fatal("wrong client identity")
		}
		if r.URL.Hostname() == "zcode.z.ai" && r.Header.Get("X-Device-Mid") != "local-device" {
			t.Fatal("missing device context")
		}
		if r.URL.Hostname() == "api.z.ai" && r.Header.Get("X-Device-Mid") != "" {
			t.Fatal("device context crossed endpoint boundary")
		}
		switch calls {
		case 1:
			pollBearer = r.Header.Get("Authorization")
			if !strings.HasPrefix(pollBearer, "Bearer ") || len(pollBearer) != 71 {
				t.Fatal("invalid poll credential")
			}
			if r.Method != "POST" || r.URL.String() != "https://zcode.z.ai/api/v1/oauth/cli/init" {
				t.Fatal("wrong init request")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"provider":"zai"}` {
				t.Fatal("poll credential appeared in body or provider changed")
			}
			return oauthResponse(200, `{"code":0,"data":{"flow_id":"flow-id","authorize_url":"https://chat.z.ai/api/oauth/authorize?state=flow-state","expires_at":1800000300,"poll_interval_sec":2}}`), nil
		case 2, 3:
			if r.Header.Get("Authorization") != pollBearer || r.URL.String() != "https://zcode.z.ai/api/v1/oauth/cli/poll/flow-id" {
				t.Fatal("poll credential/flow changed")
			}
			if calls == 2 {
				return oauthResponse(200, `{"code":0,"data":{"status":"pending"}}`), nil
			}
			return oauthResponse(200, `{"code":0,"data":{"status":"ready","token":"zcode-secret","user":{"user_id":"stable-user","email":"user@example.com","name":"User"},"zai":{"access_token":"oauth-secret","refresh_token":"refresh-secret"}}}`), nil
		case 4:
			if r.URL.String() != "https://api.z.ai/api/auth/z/login" || r.Header.Get("Authorization") != "" {
				t.Fatal("credentials crossed business-login boundary")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"token":"oauth-secret"}` {
				t.Fatal("wrong OAuth normalisation")
			}
			return oauthResponse(200, `{"code":200,"data":{"access_token":"business-secret","expires_in":3600}}`), nil
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	session, err := client.Login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || opens != 1 || waits != 1 || session.UserID != "stable-user" || session.RawOAuth != "oauth-secret" || session.ZCodeJWT != "zcode-secret" || session.BusinessJWT != "business-secret" || session.RefreshToken != "refresh-secret" {
		t.Fatal("login result lost credentials or identity")
	}
	encoded, _ := json.Marshal(session)
	for _, secret := range []string{"oauth-secret", "zcode-secret", "business-secret", "refresh-secret", pollBearer} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("public JSON leaked credential")
		}
	}
	if session.AsSession().Family != "zai" || session.AsSession().BusinessJWT != "business-secret" {
		t.Fatal("status session lost account binding")
	}
}

func TestOAuthClientDidNotFollowCredentialRedirect(t *testing.T) {
	calls := 0
	httpClient := &http.Client{Transport: oauthRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		response := oauthResponse(302, "")
		response.Header.Set("Location", "https://attacker.example/collect")
		return response, nil
	})}
	client := OAuthClient{HTTP: httpClient, Version: "3.17.2", OpenURL: func(context.Context, string) error { t.Fatal("opened redirected URL"); return nil }}
	if _, err := client.Login(context.Background()); err == nil || calls != 1 {
		t.Fatal("OAuth bearer followed redirect")
	}
	if httpClient.CheckRedirect != nil {
		t.Fatal("injected client was mutated")
	}
}

func TestOAuthRefreshBusinessUsedProviderOAuthAndReturnedCopy(t *testing.T) {
	old := LoginSession{ID: "stable-account", UserID: "stable-user", ZCodeJWT: "private-zcode", RawOAuth: "private-oauth", BusinessJWT: "private-old", BusinessExpiresAt: 1, DeviceMid: "local-device"}
	now := time.Unix(1_800_000_000, 0)
	client := OAuthClient{Version: ClientVersion, Now: func() time.Time { return now }, HTTP: oauthDoer(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Token string `json:"token"`
		}
		if r.URL.String() != "https://api.z.ai/api/auth/z/login" || r.Method != "POST" || r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "ZCode/"+ClientVersion || json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != "private-oauth" {
			t.Fatal("business exchange crossed identity boundaries")
		}
		return oauthResponse(200, `{"code":200,"data":{"access_token":"private-fresh","expires_in":3600}}`), nil
	})}
	fresh, err := client.RefreshBusiness(context.Background(), old)
	if err != nil || fresh.BusinessJWT != "private-fresh" || fresh.BusinessExpiresAt != now.Add(time.Hour).Unix() || old.BusinessJWT != "private-old" || fresh.ID != old.ID || fresh.DeviceMid != old.DeviceMid {
		t.Fatal("business refresh lost identity or modified its input")
	}
}

func TestOAuthRefreshBusinessCancelledInvalidAndRedirectedRequestsFailedClosed(t *testing.T) {
	for _, stage := range []string{"cancelled", "invalid", "redirect"} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			httpClient := &http.Client{Transport: oauthRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				response := oauthResponse(307, "private-oauth")
				response.Header.Set("Location", "https://untrusted.invalid/collect")
				return response, nil
			})}
			client := OAuthClient{HTTP: httpClient, Version: ClientVersion}
			session := LoginSession{RawOAuth: "private-oauth"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "cancelled" {
				cancel()
			}
			if stage == "invalid" {
				session.RawOAuth = "invalid\ncredential"
			}
			_, err := client.RefreshBusiness(ctx, session)
			want := 0
			if stage == "redirect" {
				want = 1
			}
			if err == nil || calls != want || strings.Contains(err.Error(), "private-") || httpClient.CheckRedirect != nil {
				t.Fatal("unsafe business refresh accepted or exposed a credential")
			}
		})
	}
}

func TestOAuthLoginRejectedUntrustedInitBeforeOpeningBrowser(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, data := range []string{
		`{"flow_id":"flow","authorize_url":"http://chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":1}`,
		`{"flow_id":"flow","authorize_url":"https://attacker.example/","expires_at":1800000300,"poll_interval_sec":1}`,
		`{"flow_id":"flow","authorize_url":"https://chat.z.ai.attacker.example/","expires_at":1800000300,"poll_interval_sec":1}`,
		`{"flow_id":"flow","authorize_url":"https://user@chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":1}`,
		`{"flow_id":"flow","authorize_url":"https://chat.z.ai/api/oauth/authorize","expires_at":1800000000,"poll_interval_sec":1}`,
		`{"flow_id":"flow","authorize_url":"https://chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":0}`,
		`{"flow_id":"../flow","authorize_url":"https://chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":1}`,
	} {
		opened := false
		client := OAuthClient{Version: "3.17.2", Now: func() time.Time { return now }, OpenURL: func(context.Context, string) error { opened = true; return nil }, HTTP: oauthDoer(func(*http.Request) (*http.Response, error) {
			return oauthResponse(200, `{"code":0,"data":`+data+`}`), nil
		})}
		if _, err := client.Login(context.Background()); err == nil || opened {
			t.Fatal("untrusted init accepted")
		}
	}
}

func TestOAuthLoginFailedSafelyOnTerminalOrMalformedPoll(t *testing.T) {
	for _, poll := range []string{
		`{"code":0,"data":{"status":"failed"}}`,
		`{"code":0,"data":{"status":"unknown","token":"private-secret"}}`,
		`{"code":0,"data":{"status":"ready","token":"private-secret","user":{"user_id":"u"},"bigmodel":{"access_token":"wrong-family"}}}`,
		`{"code":1001,"msg":"private-secret","data":{}}`,
		`{"code":"0","data":{"status":"pending"}}`,
		`{"code":null,"data":{"status":"pending"}}`,
	} {
		calls := 0
		client := OAuthClient{Version: "3.17.2", Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, OpenURL: func(context.Context, string) error { return nil }, HTTP: oauthDoer(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return oauthResponse(200, `{"code":0,"data":{"flow_id":"f","authorize_url":"https://chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":1}}`), nil
			}
			return oauthResponse(200, poll), nil
		})}
		_, err := client.Login(context.Background())
		if err == nil || strings.Contains(err.Error(), "private-secret") || calls != 2 {
			t.Fatal("terminal login failure was not safely retained")
		}
	}
}

func TestOAuthHTTPBoundaryRejectedRedirectsOversizeAndSecretErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"redirect", 302, "", nil},
		{"oversized", 200, strings.Repeat("x", (64<<10)+1), nil},
		{"transport", 0, "", errors.New("private-secret")},
		{"malformed", 200, `{"code":0,"data":"private-secret"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			client := OAuthClient{Version: "3.17.2", OpenURL: func(context.Context, string) error { opened = true; return nil }, HTTP: oauthDoer(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return oauthResponse(tc.status, tc.body), nil
			})}
			_, err := client.Login(context.Background())
			if err == nil || opened || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("unsafe OAuth boundary accepted")
			}
		})
	}
}

func TestOAuthLoginHonouredCancellationAndExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := OAuthClient{Version: "3.17.2", OpenURL: func(context.Context, string) error { t.Fatal("opened after cancellation"); return nil }, HTTP: oauthDoer(func(*http.Request) (*http.Response, error) { t.Fatal("requested after cancellation"); return nil, nil })}
	if _, err := client.Login(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	now := time.Unix(1_800_000_000, 0)
	calls := 0
	client.Now = func() time.Time { return now }
	client.OpenURL = func(context.Context, string) error { return nil }
	client.Wait = func(context.Context, time.Duration) error { now = now.Add(10 * time.Minute); return nil }
	client.HTTP = oauthDoer(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return oauthResponse(200, `{"code":0,"data":{"flow_id":"f","authorize_url":"https://chat.z.ai/api/oauth/authorize","expires_at":1800000300,"poll_interval_sec":1}}`), nil
		}
		return oauthResponse(200, `{"code":0,"data":{"status":"pending"}}`), nil
	})
	if _, err := client.Login(context.Background()); err == nil || calls != 2 {
		t.Fatal("expired flow continued polling")
	}
}
