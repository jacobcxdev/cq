package claude

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Client handles http communication with Claude APIs.
type Client struct {
	http httputil.Doer
}

// FetchUsage calls the Claude usage API and returns the raw response body and
// http status code.
func (c *Client) FetchUsage(ctx context.Context, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read usage body: %w", err)
	}
	return body, resp.StatusCode, nil
}

// FetchProfile calls the Claude profile API and returns the parsed profile.
func (c *Client) FetchProfile(ctx context.Context, token string) (profile, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		return profile{}, fmt.Errorf("create profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := c.http.Do(req)
	if err != nil {
		return profile{}, fmt.Errorf("profile request: %w", err)
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return profile{}, fmt.Errorf("read profile body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return profile{}, fmt.Errorf("profile request: HTTP %d", resp.StatusCode)
	}
	return parseProfile(body), nil
}
