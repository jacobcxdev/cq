package zai

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

// ResponsesBaseURL is Z.ai's native OpenAI Responses protocol endpoint.
const ResponsesBaseURL = "https://api.z.ai/api/v1"

const quotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"
const accountID = "zai-coding-plan"

// Provider retrieves quota for Z.ai Coding Plan subscriptions.
type Provider struct {
	client   httputil.Doer
	accounts func() ([]Account, error)
	tokens   *zcode.ProjectTokenClient
}

// New creates a quota provider with an injected HTTP client and credential reader.
func New(client httputil.Doer, apiKey func() (string, error)) *Provider {
	return NewAccounts(client, func() ([]Account, error) {
		key, err := apiKey()
		if err != nil || key == "" {
			return nil, err
		}
		return []Account{{ID: accountID, APIKey: key}}, nil
	})
}

// NewAccounts creates a provider with an ordered subscription credential reader.
func NewAccounts(client httputil.Doer, accounts func() ([]Account, error)) *Provider {
	return &Provider{client: client, accounts: accounts, tokens: &zcode.ProjectTokenClient{HTTP: client, Version: zcode.ClientVersion}}
}

// Fetch retrieves subscription quota without modifying credentials.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) (results []quota.Result, err error) {
	defer func() {
		if recover() != nil {
			results = singleError("fetch_panic", "Z.ai provider failed", 0)
			err = nil
		}
	}()
	accounts, err := p.accounts()
	if err != nil {
		return singleError("fetch_error", "read Z.ai credentials", 0), nil
	}
	if len(accounts) == 0 {
		return singleError("not_configured", "run cq zai login or set ZAI_API_KEY", 0), nil
	}
	results = make([]quota.Result, len(accounts))
	var group sync.WaitGroup
	for i, account := range accounts {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i] = p.fetchAccount(ctx, account, i == 0)
		}()
	}
	group.Wait()
	return results, nil
}

func (p *Provider) fetchAccount(ctx context.Context, account Account, active bool) (result quota.Result) {
	defer func() {
		if recover() != nil {
			result = quota.ErrorResult("fetch_panic", "Z.ai provider failed", 0)
		}
		result.AccountID = account.ID
		result.Email = account.Label
		if result.Email == "" {
			result.Email = account.ID
		}
		result.Active = active
	}()
	if account.Transport == "zcode-start" {
		if account.OAuth == nil {
			return quota.ErrorResult("auth_expired", "Start Plan requires ZCode browser login", 0)
		}
		balance, err := (zcode.Client{HTTP: p.client, Version: zcode.ClientVersion, DeviceMid: account.OAuth.DeviceMid}).Balance(ctx, account.OAuth.AsSession())
		if err != nil {
			return quota.ErrorResult("fetch_error", "read ZCode token-plan balance", 0)
		}
		parsed, err := startPlanResult(balance)
		if err != nil {
			return quota.ErrorResult("parse_error", "ZCode token-plan quota unavailable", 0)
		}
		return parsed
	}
	key, err := account.Credential(ctx, p.tokens)
	if err != nil || key == "" {
		return quota.ErrorResult("fetch_error", "invalid Z.ai credentials", 0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return quota.ErrorResult("fetch_error", "create Z.ai quota request", 0)
	}
	// Z.ai's monitor API requires the full key without a Bearer prefix.
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	resp, err := p.client.Do(req)
	if err != nil {
		return quota.ErrorResult("fetch_error", "Z.ai quota request failed", 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return quota.ErrorResult("auth_expired", "Z.ai API key rejected", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return quota.ErrorResult("api_error", "Z.ai quota API error", resp.StatusCode)
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return quota.ErrorResult("fetch_error", "read Z.ai quota response", 0)
	}
	result, code := parseQuota(body)
	if code != "" {
		return quota.ErrorResult(code, "invalid or unsuccessful Z.ai quota response", resp.StatusCode)
	}
	return result
}

func singleError(code, message string, status int) []quota.Result {
	result := quota.ErrorResult(code, message, status)
	result.AccountID = accountID
	result.Active = true
	return []quota.Result{result}
}
