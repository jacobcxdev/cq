package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

// Provider implements provider.Provider for Claude.
type Provider struct {
	client *Client
}

// New creates a Provider that uses the given HTTP client for API calls.
func New(httpClient httputil.Doer) *Provider {
	return &Provider{client: &Client{http: httpClient}}
}

// Fetch discovers all Claude accounts and fetches quota for each in parallel.
func (p *Provider) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	accounts := keyring.DiscoverClaudeAccounts()
	if len(accounts) == 0 {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}

	results := make([]quota.Result, len(accounts))
	var wg sync.WaitGroup
	for i, acct := range accounts {
		wg.Add(1)
		go func(i int, acct keyring.ClaudeOAuth) {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Fprintf(os.Stderr, "cq: panic in claude provider: %v\n%s\n", rv, debug.Stack())
					results[i] = quota.ErrorResult("panic", fmt.Sprintf("%v", rv), 0)
				}
			}()
			results[i] = p.fetchAccount(ctx, acct, now)
		}(i, acct)
	}
	wg.Wait()

	deduped := dedup(results)

	// Mark the active account (the one from the credentials file).
	activeEmail := activeCredentialEmail()
	for i := range deduped {
		if activeEmail != "" && deduped[i].Email == activeEmail {
			deduped[i].Active = true
		}
	}

	return deduped, nil
}

// activeCredentialEmail reads the active Claude account's email from the
// credentials file. Returns empty string on any error.
func activeCredentialEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	var creds keyring.ClaudeCredentials
	if json.Unmarshal(data, &creds) != nil || creds.ClaudeAiOauth == nil {
		return ""
	}
	return creds.ClaudeAiOauth.Email
}

// fetchAccount fetches quota for a single Claude account. It handles token
// refresh, parallel profile+usage fetch, profile backfill, and result parsing.
func (p *Provider) fetchAccount(ctx context.Context, acct keyring.ClaudeOAuth, now time.Time) quota.Result {
	// errorWithIdentity wraps an error result with the account's known identity
	// so dedup can associate it with the correct account.
	errorWithIdentity := func(code, msg string, httpCode int) quota.Result {
		r := quota.ErrorResult(code, msg, httpCode)
		r.Email = acct.Email
		r.AccountID = acct.AccountUUID
		r.Plan = acct.SubscriptionType
		r.RateLimitTier = acct.RateLimitTier
		return r
	}

	token := acct.AccessToken
	if token == "" {
		return errorWithIdentity("no_token", "no token", 0)
	}

	// Check expiry and refresh if needed.
	nowMs := now.UnixMilli()
	if acct.ExpiresAt > 0 && acct.ExpiresAt < nowMs && acct.RefreshToken != "" {
		rr, err := RefreshToken(ctx, p.client.http, acct.RefreshToken, acct.Scopes)
		if err != nil {
			return errorWithIdentity("auth_expired", "auth expired", 0)
		}
		token = rr.AccessToken
		acct.AccessToken = rr.AccessToken
		acct.ExpiresAt = nowMs + rr.ExpiresIn*1000
		if rr.RefreshToken != "" {
			acct.RefreshToken = rr.RefreshToken
		}
		persistRefreshedToken(&acct)
	}

	// Fetch profile and usage in parallel.
	var prof profile
	var usageBody []byte
	var usageCode int
	var wg sync.WaitGroup

	var usageErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if rv := recover(); rv != nil {
				fmt.Fprintf(os.Stderr, "cq: panic in claude profile fetch: %v\n%s\n", rv, debug.Stack())
			}
		}()
		var err error
		prof, err = p.client.FetchProfile(ctx, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: claude profile: %v\n", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if rv := recover(); rv != nil {
				fmt.Fprintf(os.Stderr, "cq: panic in claude usage fetch: %v\n%s\n", rv, debug.Stack())
				usageErr = fmt.Errorf("panic: %v", rv)
			}
		}()
		usageBody, usageCode, usageErr = p.client.FetchUsage(ctx, token)
		if usageErr != nil {
			fmt.Fprintf(os.Stderr, "cq: claude usage: %v\n", usageErr)
		}
	}()
	wg.Wait()

	if usageErr != nil {
		return errorWithIdentity("fetch_error", fmt.Sprintf("usage: %v", usageErr), 0)
	}
	if usageCode != 200 {
		return errorWithIdentity("api_error", "api error", usageCode)
	}

	// Prefer profile API data over stored keychain fields.
	plan := prof.Plan
	if plan == "" {
		plan = acct.SubscriptionType
	}
	rlt := prof.RateLimitTier
	if rlt == "" {
		rlt = acct.RateLimitTier
	}

	// Backfill all credential stores with profile data for future
	// discovery and deduplication.
	if prof.Email != "" || prof.AccountUUID != "" {
		updated := acct
		if prof.Email != "" {
			updated.Email = prof.Email
		}
		if prof.AccountUUID != "" {
			updated.AccountUUID = prof.AccountUUID
		}
		if plan != "" {
			updated.SubscriptionType = plan
		}
		if rlt != "" {
			updated.RateLimitTier = rlt
		}
		if err := backfillCredentialsFile(&updated); err != nil {
			fmt.Fprintf(os.Stderr, "cq: backfill credentials: %v\n", err)
		}
	}

	return parseUsage(usageBody, plan, rlt, prof.Email, prof.AccountUUID)
}
