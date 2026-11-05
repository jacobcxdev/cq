package zcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// LoginSession is a fresh OAuth login. Public JSON deliberately excludes every
// credential; SessionStore has a separate encrypted disk representation.
type LoginSession struct {
	ID                string `json:"id"`
	UserID            string `json:"user_id"`
	Email             string `json:"email,omitempty"`
	Name              string `json:"name,omitempty"`
	Label             string `json:"label,omitempty"`
	ZCodeJWT          string `json:"-"`
	RawOAuth          string `json:"-"`
	BusinessJWT       string `json:"-"`
	RefreshToken      string `json:"-"`
	BusinessExpiresAt int64  `json:"business_expires_at,omitempty"`
	DeviceMid         string `json:"-"`
}

func (s LoginSession) AsSession() Session {
	return Session{Family: "zai", ZCodeJWT: s.ZCodeJWT, BusinessJWT: s.BusinessJWT}
}

// OAuthClient implements ZCode's browser/polling flow. OpenURL belongs to the
// caller, so cancellation and browser interaction remain under CLI ownership.
type OAuthClient struct {
	HTTP      httputil.Doer
	Version   string
	DeviceMid string
	OpenURL   func(context.Context, string) error
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

type OAuthError struct {
	Stage      string
	Code       string
	HTTPStatus int
	VendorCode int
}

func (e *OAuthError) Error() string { return fmt.Sprintf("ZCode login failed: %s/%s", e.Stage, e.Code) }

func (c OAuthClient) Login(ctx context.Context) (LoginSession, error) {
	if err := ctx.Err(); err != nil {
		return LoginSession{}, err
	}
	if c.HTTP == nil || c.OpenURL == nil || !validHeaderValue(c.Version, 80) || c.DeviceMid != "" && !validHeaderValue(c.DeviceMid, 256) {
		return LoginSession{}, &OAuthError{Stage: "init", Code: "client_unavailable"}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return LoginSession{}, &OAuthError{Stage: "init", Code: "random_failed"}
	}
	pollToken := hex.EncodeToString(random[:])
	clear(random[:])
	data, err := c.request(ctx, "init", "https://zcode.z.ai/api/v1/oauth/cli/init", pollToken, []byte(`{"provider":"zai"}`))
	if err != nil {
		return LoginSession{}, err
	}
	var init struct {
		FlowID       string  `json:"flow_id"`
		AuthorizeURL string  `json:"authorize_url"`
		ExpiresAt    float64 `json:"expires_at"`
		PollInterval float64 `json:"poll_interval_sec"`
	}
	if json.Unmarshal(data, &init) != nil || !oauthFlowID(init.FlowID) || !oauthAuthorizeURL(init.AuthorizeURL) || math.IsNaN(init.ExpiresAt) || math.IsInf(init.ExpiresAt, 0) || init.ExpiresAt <= 0 || init.ExpiresAt > 253402300799 || math.IsNaN(init.PollInterval) || math.IsInf(init.PollInterval, 0) || init.PollInterval < 1 {
		return LoginSession{}, &OAuthError{Stage: "init", Code: "invalid_response"}
	}
	deadline := time.Unix(int64(init.ExpiresAt), int64((init.ExpiresAt-math.Floor(init.ExpiresAt))*1e9))
	lifetime := deadline.Sub(c.now())
	if lifetime <= 0 || init.PollInterval >= lifetime.Seconds() {
		return LoginSession{}, &OAuthError{Stage: "init", Code: "invalid_expiry"}
	}
	interval := time.Duration(init.PollInterval * float64(time.Second))
	flowCtx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	if err := c.OpenURL(flowCtx, init.AuthorizeURL); err != nil {
		if flowCtx.Err() != nil {
			return LoginSession{}, flowCtx.Err()
		}
		return LoginSession{}, &OAuthError{Stage: "browser", Code: "open_failed"}
	}
	for {
		if err := flowCtx.Err(); err != nil {
			return LoginSession{}, err
		}
		if !c.now().Before(deadline) {
			return LoginSession{}, &OAuthError{Stage: "poll", Code: "expired"}
		}
		data, err := c.request(flowCtx, "poll", "https://zcode.z.ai/api/v1/oauth/cli/poll/"+url.PathEscape(init.FlowID), pollToken, nil)
		if err != nil {
			return LoginSession{}, err
		}
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			User   struct {
				UserID string `json:"user_id"`
				Email  string `json:"email"`
				Name   string `json:"name"`
			} `json:"user"`
			ZAI struct {
				AccessToken       string `json:"access_token"`
				AccessTokenCamel  string `json:"accessToken"`
				RefreshToken      string `json:"refresh_token"`
				RefreshTokenCamel string `json:"refreshToken"`
			} `json:"zai"`
		}
		if json.Unmarshal(data, &poll) != nil {
			return LoginSession{}, &OAuthError{Stage: "poll", Code: "invalid_response"}
		}
		switch poll.Status {
		case "pending":
			wait := min(interval, deadline.Sub(c.now()))
			if wait <= 0 {
				return LoginSession{}, &OAuthError{Stage: "poll", Code: "expired"}
			}
			if err := c.wait(flowCtx, wait); err != nil {
				if flowCtx.Err() != nil {
					return LoginSession{}, flowCtx.Err()
				}
				return LoginSession{}, &OAuthError{Stage: "poll", Code: "wait_failed"}
			}
		case "failed":
			return LoginSession{}, &OAuthError{Stage: "poll", Code: "authorisation_failed"}
		case "ready":
			oauth := poll.ZAI.AccessToken
			if oauth == "" {
				oauth = poll.ZAI.AccessTokenCamel
			}
			refresh := poll.ZAI.RefreshToken
			if refresh == "" {
				refresh = poll.ZAI.RefreshTokenCamel
			}
			session := LoginSession{UserID: poll.User.UserID, Email: poll.User.Email, Name: poll.User.Name, ZCodeJWT: poll.Token, RawOAuth: oauth, RefreshToken: refresh, DeviceMid: c.DeviceMid}
			if !validLoginIdentity(session) || !validHeaderValue(session.ZCodeJWT, 16<<10) || !validHeaderValue(session.RawOAuth, 16<<10) || refresh != "" && !validHeaderValue(refresh, 16<<10) {
				return LoginSession{}, &OAuthError{Stage: "poll", Code: "invalid_credentials"}
			}
			session.ID = loginAccountID(session.UserID)
			if err := c.normaliseBusinessToken(flowCtx, &session); err != nil {
				return LoginSession{}, err
			}
			return session, nil
		default:
			return LoginSession{}, &OAuthError{Stage: "poll", Code: "invalid_status"}
		}
	}
}

func (c OAuthClient) normaliseBusinessToken(ctx context.Context, session *LoginSession) error {
	body, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{session.RawOAuth})
	defer clear(body)
	data, err := c.request(ctx, "business_login", "https://api.z.ai/api/auth/z/login", "", body)
	if err != nil {
		return err
	}
	var result struct {
		AccessToken      string  `json:"access_token"`
		AccessTokenCamel string  `json:"accessToken"`
		ExpiresIn        float64 `json:"expires_in"`
	}
	if json.Unmarshal(data, &result) != nil {
		return &OAuthError{Stage: "business_login", Code: "invalid_response"}
	}
	if result.AccessToken == "" {
		result.AccessToken = result.AccessTokenCamel
	}
	if !validHeaderValue(result.AccessToken, 16<<10) {
		return &OAuthError{Stage: "business_login", Code: "invalid_credentials"}
	}
	session.BusinessJWT = result.AccessToken
	if result.ExpiresIn > 0 && result.ExpiresIn < float64((365*24*time.Hour)/time.Second) && !math.IsNaN(result.ExpiresIn) && !math.IsInf(result.ExpiresIn, 0) {
		session.BusinessExpiresAt = c.now().Add(time.Duration(result.ExpiresIn * float64(time.Second))).Unix()
	}
	return nil
}

// RefreshBusiness exchanges this login's provider OAuth credential for a fresh
// business identity. It never opens a browser, requests a project token or
// changes persisted credentials. The returned session is a copy.
func (c OAuthClient) RefreshBusiness(ctx context.Context, session LoginSession) (LoginSession, error) {
	if err := ctx.Err(); err != nil {
		return LoginSession{}, err
	}
	if c.HTTP == nil || !validHeaderValue(c.Version, 80) || !validHeaderValue(session.RawOAuth, 16<<10) {
		return LoginSession{}, &OAuthError{Stage: "business_login", Code: "client_unavailable"}
	}
	session.BusinessJWT, session.BusinessExpiresAt = "", 0
	if err := c.normaliseBusinessToken(ctx, &session); err != nil {
		return LoginSession{}, err
	}
	return session, nil
}

func (c OAuthClient) request(ctx context.Context, stage, endpoint, bearer string, body []byte) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &OAuthError{Stage: stage, Code: "invalid_request"}
	}
	SetSourceHeaders(req.Header, c.Version)
	if c.DeviceMid != "" && req.URL.Hostname() == "zcode.z.ai" {
		req.Header.Set("X-Device-Mid", c.DeviceMid)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	doer := c.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	response, err := doer.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &OAuthError{Stage: stage, Code: "request_failed"}
	}
	if response == nil || response.Body == nil {
		return nil, &OAuthError{Stage: stage, Code: "invalid_response"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &OAuthError{Stage: stage, Code: "http_error", HTTPStatus: response.StatusCode}
	}
	data, err := httputil.ReadBody(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, &OAuthError{Stage: stage, Code: "read_failed"}
	}
	defer clear(data)
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || envelope.Data[0] != '{' {
		return nil, &OAuthError{Stage: stage, Code: "invalid_response"}
	}
	var code int
	if stage == "business_login" && len(envelope.Code) > 0 && envelope.Code[0] == '"' {
		var text string
		_ = json.Unmarshal(envelope.Code, &text)
		if text == "0" {
			code = 0
		} else if text == "200" {
			code = 200
		} else {
			return nil, &OAuthError{Stage: stage, Code: "business_error"}
		}
	} else if stage == "business_login" && (len(envelope.Code) == 0 || string(envelope.Code) == "null") {
		// The business-login contract also accepts an omitted success code.
		code = 0
	} else if len(envelope.Code) == 0 || string(envelope.Code) == "null" || json.Unmarshal(envelope.Code, &code) != nil {
		return nil, &OAuthError{Stage: stage, Code: "invalid_response"}
	}
	if code != 0 && !(stage == "business_login" && code == 200) || envelope.Success != nil && !*envelope.Success {
		return nil, &OAuthError{Stage: stage, Code: "business_error", VendorCode: code}
	}
	return envelope.Data, nil
}

func (c OAuthClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c OAuthClient) wait(ctx context.Context, duration time.Duration) error {
	if c.Wait != nil {
		return c.Wait(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func oauthAuthorizeURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "chat.z.ai") && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.Fragment == "" && u.Path == "/api/oauth/authorize"
}

func oauthFlowID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func loginAccountID(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return "zcode_" + hex.EncodeToString(sum[:8])
}

func validLoginIdentity(session LoginSession) bool {
	return validHeaderValue(session.UserID, 512) && validProfileText(session.Email, 512) && validProfileText(session.Name, 512) && validProfileText(session.Label, 128)
}

func validProfileText(value string, limit int) bool {
	if len(value) > limit {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
