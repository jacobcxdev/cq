package zai

import (
	"context"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/quota"
)

// ResponsesBaseURL is Z.ai's native OpenAI Responses protocol endpoint.
const ResponsesBaseURL = "https://api.z.ai/api/v1"

const quotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"
const accountID = "zai-coding-plan"

// Provider retrieves quota for one Z.ai Coding Plan subscription.
type Provider struct {
	client httputil.Doer
	apiKey func() (string, error)
}

// New creates a quota provider with an injected HTTP client and credential reader.
func New(client httputil.Doer, apiKey func() (string, error)) *Provider {
	return &Provider{client: client, apiKey: apiKey}
}

// Fetch retrieves subscription quota without modifying credentials.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) (results []quota.Result, err error) {
	defer func() {
		if recover() != nil {
			results = singleError("fetch_panic", "Z.ai provider failed", 0)
			err = nil
		}
	}()
	key, err := p.apiKey()
	if err != nil {
		return singleError("fetch_error", "read Z.ai credentials", 0), nil
	}
	if key == "" {
		return singleError("not_configured", "set ZAI_API_KEY or configure CQ's zai.json", 0), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return singleError("fetch_error", "create Z.ai quota request", 0), nil
	}
	// Z.ai's monitor API requires the full key without a Bearer prefix.
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	resp, err := p.client.Do(req)
	if err != nil {
		return singleError("fetch_error", "Z.ai quota request failed", 0), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return singleError("auth_expired", "Z.ai API key rejected", resp.StatusCode), nil
	}
	if resp.StatusCode != http.StatusOK {
		return singleError("api_error", "Z.ai quota API error", resp.StatusCode), nil
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return singleError("fetch_error", "read Z.ai quota response", 0), nil
	}
	result, code := parseQuota(body)
	if code != "" {
		return singleError(code, "invalid or unsuccessful Z.ai quota response", resp.StatusCode), nil
	}
	return []quota.Result{result}, nil
}

func singleError(code, message string, status int) []quota.Result {
	result := quota.ErrorResult(code, message, status)
	result.AccountID = accountID
	result.Active = true
	return []quota.Result{result}
}
