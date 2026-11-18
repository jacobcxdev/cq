package opencodego

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	consoleHost = "opencode.ai"
	consoleURL  = "https://" + consoleHost + "/console"
	// Public device client and wire contract from the Console CLI.
	// https://github.com/anomalyco/opencode-console-cli/blob/2db1246ab9811679944097ae7cb4506578a81eee/src/console-client.ts
	consoleClientID = "opencode-console-cli"
	deviceTokenPath = "/auth/device/token"
	deviceGrant     = "urn:ietf:params:oauth:grant-type:device_code"
)

var ErrOAuthRejected = errors.New("OpenCode Console sign-in rejected")

// OAuthSession binds rotating Console credentials to one member and workspace.
type OAuthSession struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	MemberID     string    `json:"member_id"`
	WorkspaceID  string    `json:"workspace_id"`
	Email        string    `json:"email"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type OAuthClient struct {
	Client httputil.Doer
}

type consoleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	WorkspaceID  string `json:"org_id"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

// Login prompts the member to approve access to their Go workspace.
func (c OAuthClient) Login(ctx context.Context, prompt func(verificationURL, userCode string) error) (OAuthSession, error) {
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	reply, err := c.request(ctx, "/auth/device/code", map[string]any{
		"client_id": consoleClientID, "supports_org_scope": true,
	}, "")
	if err != nil {
		return OAuthSession{}, err
	}
	if err := reply.decode(&device); err != nil {
		return OAuthSession{}, err
	}
	expiry := seconds(device.ExpiresIn)
	interval := seconds(device.Interval)
	if !credentialText(device.DeviceCode, 8192) || !credentialText(device.UserCode, 128) ||
		expiry == 0 || device.Interval != 0 && interval == 0 {
		return OAuthSession{}, errors.New("invalid OpenCode Console device response")
	}
	if interval == 0 {
		interval = 5 * time.Second
	}
	verificationURI := device.VerificationURIComplete
	if verificationURI == "" {
		verificationURI = device.VerificationURI
	}
	verification, err := consoleVerificationURL(verificationURI)
	if err != nil {
		return OAuthSession{}, err
	}
	pollCtx, cancel := context.WithTimeout(ctx, min(expiry, 30*time.Minute))
	defer cancel()
	if err := prompt(verification, device.UserCode); err != nil {
		return OAuthSession{}, err
	}
	token, err := c.poll(pollCtx, device.DeviceCode, interval)
	if err != nil {
		return OAuthSession{}, err
	}
	return c.identify(ctx, token)
}

func consoleVerificationURL(raw string) (string, error) {
	base, _ := url.Parse(consoleURL + "/")
	if raw != "" && len(raw) <= 8192 {
		u, err := base.Parse(raw)
		if err == nil && u.Scheme == "https" && u.Host == consoleHost && u.User == nil && u.Opaque == "" {
			return u.String(), nil
		}
	}
	return "", errors.New("invalid OpenCode Console verification URL")
}

func (c OAuthClient) poll(ctx context.Context, deviceCode string, interval time.Duration) (consoleToken, error) {
	wait := interval
	for {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return consoleToken{}, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return consoleToken{}, err
		}
		reply, err := c.request(ctx, deviceTokenPath, map[string]string{
			"client_id": consoleClientID, "grant_type": deviceGrant, "device_code": deviceCode,
		}, "")
		if err != nil {
			return consoleToken{}, err
		}
		var token consoleToken
		decoded := json.Unmarshal(reply.body, &token) == nil
		polling := decoded && (reply.status == http.StatusBadRequest || reply.status == http.StatusOK)
		switch {
		case reply.status == http.StatusTooManyRequests ||
			polling && token.Error == "slow_down":
			interval = min(interval, time.Duration(math.MaxInt64)-5*time.Second) + 5*time.Second
			wait = max(interval, reply.retryAfter)
		case polling && token.Error == "authorization_pending":
			wait = interval
		case (polling || decoded && reply.status == http.StatusForbidden) &&
			(token.Error == "expired_token" || token.Error == "access_denied"):
			return consoleToken{}, ErrOAuthRejected
		default:
			if !reply.success() {
				return consoleToken{}, reply.statusError()
			}
			if !decoded || !token.valid() {
				return consoleToken{}, errors.New("invalid OpenCode Console token response")
			}
			return token, nil
		}
	}
}

func (c OAuthClient) identify(ctx context.Context, token consoleToken) (OAuthSession, error) {
	expiresAt := time.Now().Add(seconds(token.ExpiresIn))
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	var workspaces []struct {
		ID string `json:"id"`
	}
	for _, resource := range []struct {
		path   string
		target any
	}{
		{"/api/user", &user},
		{"/api/orgs", &workspaces},
	} {
		reply, err := c.request(ctx, resource.path, nil, token.AccessToken)
		if err != nil {
			return OAuthSession{}, err
		}
		if reply.status == http.StatusUnauthorized {
			return OAuthSession{}, ErrOAuthRejected
		}
		if err := reply.decode(resource.target); err != nil {
			return OAuthSession{}, err
		}
	}
	email, err := mail.ParseAddress(user.Email)
	if err != nil || email.Address != user.Email || len(user.Email) > 320 ||
		strings.ContainsFunc(user.Email, unicode.IsControl) || !credentialText(user.ID, 256) {
		return OAuthSession{}, errors.New("invalid OpenCode Console member identity")
	}
	workspaceID := token.WorkspaceID
	if workspaceID == "" && len(workspaces) == 1 {
		workspaceID = workspaces[0].ID
	}
	if !credentialText(workspaceID, 256) {
		return OAuthSession{}, errors.New("select an OpenCode Go workspace in the browser")
	}
	for _, workspace := range workspaces {
		if workspace.ID == workspaceID {
			return OAuthSession{
				AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: expiresAt,
				MemberID: user.ID, WorkspaceID: workspaceID, Email: user.Email,
			}, nil
		}
	}
	return OAuthSession{}, errors.New("OpenCode Console sign-in workspace is unavailable")
}

// Refresh rotates credentials for the same login. The caller owns
// serialisation and persistence of the refresh token.
func (c OAuthClient) Refresh(ctx context.Context, current OAuthSession) (OAuthSession, error) {
	if !credentialText(current.RefreshToken, 8192) || !credentialText(current.WorkspaceID, 256) ||
		!credentialText(current.MemberID, 256) {
		return OAuthSession{}, errors.New("invalid OpenCode Console session")
	}
	reply, err := c.request(ctx, deviceTokenPath, map[string]string{
		"client_id": consoleClientID, "grant_type": "refresh_token", "refresh_token": current.RefreshToken,
	}, "")
	if err != nil {
		return OAuthSession{}, err
	}
	var token consoleToken
	if (reply.status == http.StatusBadRequest || reply.status == http.StatusUnauthorized) &&
		json.Unmarshal(reply.body, &token) == nil && token.Error == "invalid_grant" {
		return OAuthSession{}, ErrOAuthRejected
	}
	if err := reply.decode(&token); err != nil {
		return OAuthSession{}, err
	}
	if !token.valid() {
		return OAuthSession{}, errors.New("invalid OpenCode Console token response")
	}
	if token.WorkspaceID != "" && token.WorkspaceID != current.WorkspaceID {
		return OAuthSession{}, errors.New("OpenCode Console changed the sign-in workspace")
	}
	current.AccessToken = token.AccessToken
	current.RefreshToken = token.RefreshToken
	current.ExpiresAt = time.Now().Add(seconds(token.ExpiresIn))
	return current, nil
}

func (t consoleToken) valid() bool {
	return t.Error == "" && credentialText(t.AccessToken, 8192) && credentialText(t.RefreshToken, 8192) &&
		seconds(t.ExpiresIn) != 0 && (t.WorkspaceID == "" || credentialText(t.WorkspaceID, 256))
}

func credentialText(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r >= 127 })
}

func seconds(value int64) time.Duration {
	if value <= 0 || value > math.MaxInt64/int64(time.Second) {
		return 0
	}
	return time.Duration(value) * time.Second
}

type consoleReply struct {
	status     int
	retryAfter time.Duration
	body       []byte
}

func (r consoleReply) success() bool {
	return r.status >= http.StatusOK && r.status < http.StatusMultipleChoices
}

func (r consoleReply) statusError() error {
	return fmt.Errorf("OpenCode Console request failed (HTTP %d)", r.status)
}

func (r consoleReply) decode(target any) error {
	if !r.success() {
		return r.statusError()
	}
	if json.Unmarshal(r.body, target) != nil {
		return errors.New("invalid OpenCode Console response")
	}
	return nil
}

func (c OAuthClient) request(ctx context.Context, path string, payload any, bearer string) (consoleReply, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return consoleReply{}, errors.New("prepare OpenCode Console request")
		}
		method, body = http.MethodPost, bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, consoleURL+path, body)
	if err != nil {
		return consoleReply{}, errors.New("prepare OpenCode Console request")
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := c.Client
	if client == nil {
		return consoleReply{}, errors.New("OpenCode Console client unavailable")
	}
	if original, ok := client.(*http.Client); ok {
		pinned := *original
		pinned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &pinned
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return consoleReply{}, ctx.Err()
		}
		return consoleReply{}, errors.New("OpenCode Console request failed")
	}
	defer response.Body.Close()
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		if ctx.Err() != nil {
			return consoleReply{}, ctx.Err()
		}
		return consoleReply{}, errors.New("read OpenCode Console response")
	}
	var retryAfter time.Duration
	header := response.Header.Get("Retry-After")
	if delay, err := strconv.ParseInt(header, 10, 64); err == nil {
		retryAfter = seconds(delay)
	} else if date, err := http.ParseTime(header); err == nil {
		retryAfter = time.Until(date)
	}
	return consoleReply{status: response.StatusCode, retryAfter: retryAfter, body: data}, nil
}
