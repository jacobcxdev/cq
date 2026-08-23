package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	oauthTokenURL        = "https://oauth2.googleapis.com/token"
	loadCodeAssistURL    = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	retrieveQuotaURL     = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	antigravityUserAgent = "antigravity/cli/cq"
)

var (
	errOAuthRejected          = errors.New("OAuth refresh rejected")
	errInvalidRefreshResponse = errors.New("invalid OAuth refresh response")
	errInvalidProjectResponse = errors.New("invalid Antigravity project response")
	errHTTPCreate             = errors.New("create Antigravity request")
	errHTTPDo                 = errors.New("Antigravity request failed")
	errHTTPRead               = errors.New("read Antigravity response")
)

type refreshResult struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int64
}

type refreshEnvelope struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func refreshAccessToken(
	ctx context.Context,
	client httputil.Doer,
	clientID string,
	clientSecret string,
	refreshToken string,
) (refreshResult, int, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return refreshResult{}, 0, errHTTPCreate
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, status, err := doBounded(client, req)
	if err != nil {
		return refreshResult{}, status, err
	}
	if status != http.StatusOK {
		return refreshResult{}, status, errOAuthRejected
	}
	var envelope refreshEnvelope
	if json.Unmarshal(body, &envelope) != nil || envelope.AccessToken == "" || envelope.ExpiresIn <= 0 {
		return refreshResult{}, status, errInvalidRefreshResponse
	}
	return refreshResult{
		AccessToken:  envelope.AccessToken,
		RefreshToken: envelope.RefreshToken,
		TokenType:    envelope.TokenType,
		ExpiresIn:    envelope.ExpiresIn,
	}, status, nil
}

func loadCodeAssist(ctx context.Context, client httputil.Doer, accessToken string) (string, int, error) {
	body := []byte("{\"metadata\":{\"ideType\":\"ANTIGRAVITY\"}}")
	responseBody, status, err := postAntigravityJSON(ctx, client, loadCodeAssistURL, accessToken, body)
	if err != nil || status != http.StatusOK {
		return "", status, err
	}
	var envelope struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if json.Unmarshal(responseBody, &envelope) != nil || len(envelope.Project) == 0 {
		return "", status, errInvalidProjectResponse
	}
	projectID, err := parseProjectValue(envelope.Project)
	if err != nil {
		return "", status, errInvalidProjectResponse
	}
	return projectID, status, nil
}

func parseProjectValue(data json.RawMessage) (string, error) {
	var direct string
	if json.Unmarshal(data, &direct) == nil {
		return normaliseProjectID(direct)
	}
	var object struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
	}
	if json.Unmarshal(data, &object) != nil {
		return "", errInvalidProjectID
	}
	if object.ProjectID != "" {
		return normaliseProjectID(object.ProjectID)
	}
	return normaliseProjectID(object.ID)
}

func retrieveUserQuotaSummary(
	ctx context.Context,
	client httputil.Doer,
	accessToken string,
	projectID string,
) ([]byte, int, error) {
	body, err := json.Marshal(struct {
		Project string `json:"project"`
	}{Project: projectID})
	if err != nil {
		return nil, 0, errHTTPCreate
	}
	return postAntigravityJSON(ctx, client, retrieveQuotaURL, accessToken, body)
}

func postAntigravityJSON(
	ctx context.Context,
	client httputil.Doer,
	endpoint string,
	accessToken string,
	body []byte,
) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errHTTPCreate
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityUserAgent)
	return doBounded(client, req)
}

func doBounded(client httputil.Doer, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		return nil, 0, errHTTPDo
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, errHTTPRead
	}
	return body, resp.StatusCode, nil
}
