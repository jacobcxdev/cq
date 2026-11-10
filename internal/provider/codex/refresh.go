package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// fetchUsage calls the Codex usage API and returns the response body and HTTP status code.
func fetchUsage(ctx context.Context, client httputil.Doer, token, accountID string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create codex usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}

	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("codex usage request: %w", err)
		}
		body, err := httputil.ReadBody(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("read codex usage body: %w", err)
		}
		if attempt != 0 || resp.StatusCode != http.StatusNotFound || resp.Request == nil ||
			resp.Request.URL == nil || resp.Request.URL.String() != req.URL.String() {
			return body, resp.StatusCode, nil
		}

		// Upstream can return 404 while setting its routing affinity. Retry the
		// same read once, retaining only that infrastructure cookie for this fetch.
		jar, _ := cookiejar.New(nil)
		for _, cookie := range resp.Cookies() {
			if cookie.Name == "__oailb" {
				jar.SetCookies(req.URL, []*http.Cookie{cookie})
			}
		}
		retry := req.Clone(ctx)
		for _, cookie := range jar.Cookies(req.URL) {
			if cookie.Value != "" {
				retry.AddCookie(cookie)
			}
		}
		if retry.Header.Get("Cookie") == "" {
			return body, resp.StatusCode, nil
		}
		req = retry
	}
}
