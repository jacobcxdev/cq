package claude

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Client handles http communication with Claude APIs.
type Client struct {
	http httputil.Doer
}

// FetchUsage calls the Claude usage API and returns the raw response body,
// http status code, retry hint, and rate-limit diagnostic details.
func (c *Client) FetchUsage(ctx context.Context, token string) ([]byte, int, time.Duration, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("create usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("read usage body: %w", err)
	}
	retryAfter := retryAfterDuration(resp.Header.Get("Retry-After"))
	return body, resp.StatusCode, retryAfter, formatRateLimitDiagnostics(resp.Header, retryAfter), nil
}

func formatRateLimitDiagnostics(h http.Header, retryAfter time.Duration) string {
	parts := make([]string, 0, 12)
	if retryAfter > 0 {
		parts = append(parts, fmt.Sprintf("retry_after=%s", retryAfter.Round(time.Second)))
	}
	for _, key := range []string{
		"anthropic-ratelimit-requests-limit",
		"anthropic-ratelimit-requests-remaining",
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-limit",
		"anthropic-ratelimit-tokens-remaining",
		"anthropic-ratelimit-tokens-reset",
		"x-ratelimit-limit-requests",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-tokens",
	} {
		if value := strings.TrimSpace(h.Get(key)); value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return strings.Join(parts, "; ")
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

func retryAfterDuration(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		return max(0, time.Duration(seconds)*time.Second)
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	return max(0, time.Until(when))
}
