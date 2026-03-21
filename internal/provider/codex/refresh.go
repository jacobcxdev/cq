package codex

import (
	"context"
	"fmt"
	"net/http"

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

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("codex usage request: %w", err)
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read codex usage body: %w", err)
	}
	return body, resp.StatusCode, nil
}
