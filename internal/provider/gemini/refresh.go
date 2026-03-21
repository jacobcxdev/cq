package gemini

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
)

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
// response body and HTTP status code.
func fetchQuota(ctx context.Context, client httputil.Doer, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader("{}"))
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
