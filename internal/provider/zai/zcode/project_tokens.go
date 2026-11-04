package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// ProjectTokenClient resolves OAuth accounts to short-lived project tokens.
// Keep one instance per runtime; tokens and key locations stay in memory only.
// Do not copy a client after its first use.
type ProjectTokenClient struct {
	HTTP    httputil.Doer
	Version string

	mu       sync.Mutex
	accounts map[string]*projectTokenEntry
	now      func() time.Time
}

type ProjectTokenError struct {
	Code       string
	HTTPStatus int
}

func (e *ProjectTokenError) Error() string {
	return fmt.Sprintf("ZCode project token failed: %s", e.Code)
}

type projectTokenLocation struct{ organization, project, key string }
type projectTokenFlight struct {
	done  chan struct{}
	token string
	err   error
}
type projectTokenEntry struct {
	businessJWT, rawOAuth string
	location              projectTokenLocation
	token                 string
	expiresAt, refreshAt  time.Time
	pending               *projectTokenFlight
}

// Resolve uses the account's business JWT directly. Only an absent or known
// expiring business JWT is exchanged from RawOAuth; existing API secrets are
// never copied or used as a fallback.
func (c *ProjectTokenClient) Resolve(ctx context.Context, session LoginSession) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.HTTP == nil || !validHeaderValue(c.Version, 80) {
		return "", &ProjectTokenError{Code: "client_unavailable"}
	}
	if !validHeaderValue(session.UserID, 512) || session.BusinessJWT == "" && !validHeaderValue(session.RawOAuth, 16<<10) || session.BusinessJWT != "" && !validHeaderValue(session.BusinessJWT, 16<<10) {
		return "", &ProjectTokenError{Code: "login_required"}
	}
	c.mu.Lock()
	if c.accounts == nil {
		c.accounts = make(map[string]*projectTokenEntry)
	}
	entry := c.accounts[session.UserID]
	if entry == nil || entry.businessJWT != session.BusinessJWT || entry.rawOAuth != session.RawOAuth {
		entry = &projectTokenEntry{businessJWT: session.BusinessJWT, rawOAuth: session.RawOAuth}
		c.accounts[session.UserID] = entry
	}
	if entry.token != "" && c.clock().Before(entry.refreshAt) && c.clock().Before(entry.expiresAt) {
		token := entry.token
		c.mu.Unlock()
		return token, nil
	}
	flight := entry.pending
	if flight == nil {
		flight = &projectTokenFlight{done: make(chan struct{})}
		entry.pending = flight
		go c.issue(session, entry, flight)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return flight.token, flight.err
	}
}

// Clear invalidates cached credentials and any late results after logout.
func (c *ProjectTokenClient) Clear() {
	c.mu.Lock()
	c.accounts = nil
	c.mu.Unlock()
}

func (c *ProjectTokenClient) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *ProjectTokenClient) issue(session LoginSession, entry *projectTokenEntry, flight *projectTokenFlight) {
	var token string
	var expires time.Time
	var err error
	location := entry.location
	defer func() {
		if recover() != nil {
			err = &ProjectTokenError{Code: "request_failed"}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.accounts[session.UserID] != entry {
			flight.err = &ProjectTokenError{Code: "scope_invalidated"}
		} else {
			entry.location = location
			if err == nil {
				entry.token, entry.expiresAt, entry.refreshAt = token, expires, expires.Add(-120*time.Second)
			} else if projectTokenTransient(err) && entry.token != "" && c.clock().Before(entry.expiresAt) {
				// An early refresh may reuse an unexpired token, never extend it.
				entry.refreshAt = minTime(c.clock().Add(5*time.Second), entry.expiresAt)
				token, err = entry.token, nil
			} else {
				entry.token = ""
				var failure *ProjectTokenError
				if errors.As(err, &failure) && failure.HTTPStatus == http.StatusNotFound {
					entry.location = projectTokenLocation{}
				}
			}
			entry.pending = nil
			flight.token, flight.err = token, err
		}
		close(flight.done)
	}()
	current := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.accounts[session.UserID] == entry
	}
	business := session.BusinessJWT
	exchange := business == "" || session.BusinessExpiresAt > 0 && !c.clock().Before(time.Unix(session.BusinessExpiresAt, 0).Add(-120*time.Second))
	for attempt := 0; attempt < 2; attempt++ {
		if exchange {
			business, err = c.exchangeProjectBusinessJWT(session.RawOAuth, current)
			if err != nil {
				return
			}
		}
		if location.key == "" {
			location, err = c.projectLocation(business, current)
		}
		if err == nil {
			body, _ := json.Marshal(struct {
				ClientType string `json:"clientType"`
				Version    string `json:"clientVersion"`
			}{"zcode", c.Version})
			started := c.clock()
			var data json.RawMessage
			data, err = c.projectRequest("POST", projectKeysPath(location)+"/"+url.PathEscape(location.key)+"/access_tokens", "Bearer "+business, body, current)
			if err == nil {
				token, expires, err = decodeProjectToken(data, started, c.clock())
			}
		}
		var failure *ProjectTokenError
		if attempt == 0 && errors.As(err, &failure) && failure.HTTPStatus == http.StatusUnauthorized && validHeaderValue(session.RawOAuth, 16<<10) {
			// Recover one rejected business login; never replay model inference.
			location, err, exchange = projectTokenLocation{}, nil, true
			continue
		}
		return
	}
}

func (c *ProjectTokenClient) exchangeProjectBusinessJWT(oauth string, current func() bool) (string, error) {
	if !validHeaderValue(oauth, 16<<10) {
		return "", &ProjectTokenError{Code: "login_required"}
	}
	body, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{oauth})
	defer clear(body)
	data, err := c.projectRequest("POST", "/api/auth/z/login", "", body, current)
	if err != nil {
		return "", err
	}
	var login struct {
		AccessToken string `json:"access_token"`
		CamelToken  string `json:"accessToken"`
	}
	if json.Unmarshal(data, &login) != nil {
		return "", &ProjectTokenError{Code: "invalid_response"}
	}
	token := login.AccessToken
	if token == "" {
		token = login.CamelToken
	}
	if !validHeaderValue(token, 16<<10) {
		return "", &ProjectTokenError{Code: "invalid_response"}
	}
	return token, nil
}

func (c *ProjectTokenClient) projectLocation(business string, current func() bool) (projectTokenLocation, error) {
	data, err := c.projectRequest("GET", "/api/biz/customer/getCustomerInfo", business, nil, current)
	if err != nil {
		return projectTokenLocation{}, err
	}
	location, err := selectPersonalProject(data)
	if err != nil {
		return projectTokenLocation{}, err
	}
	data, err = c.projectRequest("GET", projectKeysPath(location), "Bearer "+business, nil, current)
	if err != nil {
		return projectTokenLocation{}, err
	}
	var keys []json.RawMessage
	if len(data) == 0 || data[0] != '[' || json.Unmarshal(data, &keys) != nil {
		return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
	}
	var candidate json.RawMessage
	for _, raw := range keys {
		var probe struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Name == "zcode-api-key" {
			candidate = raw
			break
		}
	}
	if candidate == nil {
		candidate, err = c.projectRequest("POST", projectKeysPath(location), "Bearer "+business, []byte(`{"name":"zcode-api-key","usageScene":1}`), current)
		if err != nil {
			return projectTokenLocation{}, err
		}
	}
	var key struct {
		ID      *string  `json:"apiKey"`
		Name    *string  `json:"name"`
		KeyType *float64 `json:"keyType"`
	}
	if json.Unmarshal(candidate, &key) != nil || key.ID == nil || !validHeaderValue(strings.TrimSpace(*key.ID), 512) {
		return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
	}
	location.key = strings.TrimSpace(*key.ID)
	return location, nil
}

func selectPersonalProject(data json.RawMessage) (projectTokenLocation, error) {
	type project struct {
		ID   *string         `json:"projectId"`
		Name *string         `json:"projectName"`
		Type json.RawMessage `json:"projectType"`
	}
	type organization struct {
		ID       *string         `json:"organizationId"`
		Name     *string         `json:"organizationName"`
		Projects json.RawMessage `json:"projects"`
	}
	var customer struct {
		Organizations json.RawMessage `json:"organizations"`
	}
	if json.Unmarshal(data, &customer) != nil {
		return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
	}
	var organizations []organization
	if len(customer.Organizations) > 0 && (customer.Organizations[0] != '[' || json.Unmarshal(customer.Organizations, &organizations) != nil) {
		return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
	}
	type choice struct {
		org, name string
		projects  []project
	}
	var choices []choice
	for _, org := range organizations {
		if org.ID == nil {
			return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
		}
		var projects []project
		if len(org.Projects) > 0 && (org.Projects[0] != '[' || json.Unmarshal(org.Projects, &projects) != nil) {
			return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
		}
		var personal []project
		for _, p := range projects {
			if p.ID == nil {
				return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
			}
			team := false
			if len(p.Type) > 0 && string(p.Type) != "null" {
				if p.Type[0] == '"' {
					var kind string
					if json.Unmarshal(p.Type, &kind) != nil {
						return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
					}
					team = strings.TrimSpace(kind) == "2"
				} else {
					var kind float64
					if json.Unmarshal(p.Type, &kind) != nil {
						return projectTokenLocation{}, &ProjectTokenError{Code: "invalid_response"}
					}
					team = kind == 2
				}
			}
			if !team && validHeaderValue(*p.ID, 512) {
				personal = append(personal, p)
			}
		}
		if validHeaderValue(*org.ID, 512) && len(personal) > 0 {
			item := choice{org: *org.ID, projects: personal}
			if org.Name != nil {
				item.name = *org.Name
			}
			choices = append(choices, item)
		}
	}
	if len(choices) == 0 {
		return projectTokenLocation{}, &ProjectTokenError{Code: "project_unavailable"}
	}
	selected := choices[0]
	for _, org := range choices {
		if strings.Contains(org.name, "默认机构") {
			selected = org
			break
		}
	}
	p := selected.projects[0]
	for _, candidate := range selected.projects {
		if candidate.Name != nil && strings.Contains(*candidate.Name, "默认项目") {
			p = candidate
			break
		}
	}
	return projectTokenLocation{organization: selected.org, project: *p.ID}, nil
}

func projectKeysPath(location projectTokenLocation) string {
	return "/api/biz/v1/organization/" + url.PathEscape(location.organization) + "/projects/" + url.PathEscape(location.project) + "/api_keys"
}

func decodeProjectToken(data json.RawMessage, started, now time.Time) (string, time.Time, error) {
	invalid := &ProjectTokenError{Code: "invalid_response"}
	var payload struct {
		Enable    json.RawMessage `json:"enable"`
		Token     string          `json:"accessToken"`
		TokenType string          `json:"tokenType"`
		ExpiresIn float64         `json:"expiresIn"`
		ExpiresAt float64         `json:"expiresAt"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return "", time.Time{}, invalid
	}
	if len(payload.Enable) > 0 {
		if string(payload.Enable) == "false" {
			return "", time.Time{}, &ProjectTokenError{Code: "disabled"}
		}
		if string(payload.Enable) != "true" {
			return "", time.Time{}, invalid
		}
	}
	token := strings.TrimSpace(payload.Token)
	if !validHeaderValue(token, 16<<10) || payload.TokenType != "Bearer" || !validProjectSeconds(payload.ExpiresAt) || !validProjectSeconds(payload.ExpiresIn) {
		return "", time.Time{}, invalid
	}
	seconds, fraction := math.Modf(payload.ExpiresAt)
	absolute := time.Unix(int64(seconds), int64(fraction*1e9))
	seconds, fraction = math.Modf(payload.ExpiresIn)
	relative := time.Unix(started.Unix()+int64(seconds), int64(started.Nanosecond())+int64(fraction*1e9))
	expires := minTime(absolute, relative)
	if !expires.After(now) {
		return "", time.Time{}, invalid
	}
	return token, expires, nil
}

func validProjectSeconds(value float64) bool {
	return value > 0 && value <= 253402300799 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func projectTokenTransient(err error) bool {
	var failure *ProjectTokenError
	return errors.As(err, &failure) && (failure.Code == "network_error" || failure.Code == "timeout" || failure.HTTPStatus == 429 || failure.HTTPStatus >= 500 && failure.HTTPStatus < 600)
}

func (c *ProjectTokenClient) projectRequest(method, path, authorization string, body []byte, current func() bool) (json.RawMessage, error) {
	if !current() {
		return nil, &ProjectTokenError{Code: "scope_invalidated"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://api.z.ai"+path, bytes.NewReader(body))
	if err != nil {
		return nil, &ProjectTokenError{Code: "invalid_request"}
	}
	SetSourceHeaders(req.Header, c.Version)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
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
		code := "network_error"
		if ctx.Err() != nil {
			code = "timeout"
		}
		return nil, &ProjectTokenError{Code: code}
	}
	if response == nil || response.Body == nil {
		return nil, &ProjectTokenError{Code: "invalid_response"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &ProjectTokenError{Code: "http_error", HTTPStatus: response.StatusCode}
	}
	data, err := httputil.ReadBody(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, &ProjectTokenError{Code: "invalid_response"}
	}
	defer clear(data)
	if !current() {
		return nil, &ProjectTokenError{Code: "scope_invalidated"}
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || !projectSuccessCode(envelope.Code) || len(envelope.Data) == 0 {
		return nil, &ProjectTokenError{Code: "invalid_response"}
	}
	return bytes.Clone(envelope.Data), nil
}

func projectSuccessCode(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '"' {
		var code string
		return json.Unmarshal(raw, &code) == nil && (code == "0" || code == "200")
	}
	var code float64
	return json.Unmarshal(raw, &code) == nil && (code == 0 || code == 200) && string(raw) != "null"
}
