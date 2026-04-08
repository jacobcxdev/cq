package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	// Hardcoded OAuth client credentials from the gemini CLI (v0.36.0).
	// These are public credentials embedded in the installed application.
	geminiClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiClientSecret = "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"

	googleTokenURL = "https://oauth2.googleapis.com/token"
)

// tokenResponse holds the fields returned by Google's token endpoint.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	IDToken     string `json:"id_token"`
}

// refreshAccessToken exchanges a refresh token for a new access token via
// Google's OAuth2 token endpoint.
func refreshAccessToken(ctx context.Context, client httputil.Doer, refreshToken string) (*tokenResponse, error) {
	form := url.Values{
		"client_id":     {geminiClientID},
		"client_secret": {geminiClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, httputil.TruncateBody(body, httputil.MaxErrorBodyLen))
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("empty access_token in refresh response")
	}
	return &tok, nil
}

// fetchTier calls the Code Assist loadCodeAssist endpoint and returns the raw
// response body.
func fetchTier(ctx context.Context, client httputil.Doer, token string) ([]byte, error) {
	body := `{"metadata":{"ideType":"GEMINI_CLI","pluginType":"GEMINI"}}`
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
		strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create tier request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tier request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tier request: HTTP %d", resp.StatusCode)
	}
	data, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tier body: %w", err)
	}
	return data, nil
}

// fetchQuota calls the Code Assist retrieveUserQuota endpoint and returns the
// response body and HTTP status code. projectID, when non-empty, is included in
// the request body for accurate per-project quota data.
func fetchQuota(ctx context.Context, client httputil.Doer, token, projectID string) ([]byte, int, error) {
	body := "{}"
	if projectID != "" {
		body = `{"project":"` + projectID + `"}`
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create quota request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("quota request: %w", err)
	}
	defer resp.Body.Close()
	data, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read quota body: %w", err)
	}
	return data, resp.StatusCode, nil
}
