package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// RefreshResult holds the result of a token refresh.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// RefreshToken exchanges a refresh token for new access and refresh tokens.
// If scopes is empty a default set is used. The returned ExpiresIn is
// guaranteed to be at least 3600 (one hour).
func RefreshToken(ctx context.Context, client httputil.Doer, refreshToken string, scopes []string) (*RefreshResult, error) {
	if len(scopes) == 0 {
		scopes = auth.DefaultScopes()
	}
	reqBody := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     auth.ClaudeClientID,
		"scope":         strings.Join(scopes, " "),
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://platform.claude.com/v1/oauth/token",
		bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed: HTTP %d", resp.StatusCode)
	}

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("empty token in refresh response")
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = auth.DefaultExpiresInSec
	}
	return &RefreshResult{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, nil
}
