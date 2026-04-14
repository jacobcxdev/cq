package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
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
	accounts := discoverClaudeAccounts()
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

// DiscoverAccounts returns all locally known Claude accounts without making
// network calls. It implements provider.Discoverer so cached runs can keep
// expired accounts visible.
func (p *Provider) DiscoverAccounts(_ context.Context) ([]provider.Account, error) {
	accts := discoverClaudeAccounts()
	activeEmail := activeCredentialEmail()
	out := make([]provider.Account, len(accts))
	for i, acct := range accts {
		out[i] = provider.Account{
			AccountID:     acct.AccountUUID,
			Email:         acct.Email,
			Label:         acct.SubscriptionType,
			RateLimitTier: acct.RateLimitTier,
			SwitchID:      acct.Email,
			Active:        activeEmail != "" && acct.Email == activeEmail,
		}
	}
	return out, nil
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
			// Refresh exchange failed. The access token may still be valid
			// (server-side expiry differs from the locally-stored ExpiresAt).
			// Probe it with FetchProfile: if it succeeds the current token
			// is still good and we continue with it; if not, return auth_expired.
			if _, probeErr := p.client.FetchProfile(ctx, token); probeErr != nil {
				return errorWithIdentity("auth_expired", "auth expired", 0)
			}
			// Current access token works — fall through with token unchanged.
		} else {
			token = rr.AccessToken
			acct.AccessToken = rr.AccessToken
			acct.ExpiresAt = nowMs + rr.ExpiresIn*1000
			if rr.RefreshToken != "" {
				acct.RefreshToken = rr.RefreshToken
			}
			persistRefreshedToken(&acct)
		}
	}

	// Fetch profile (always) and usage (skip for free plan) in parallel.
	var prof profile
	var usageBody []byte
	var usageCode int
	var usageDiag string
	var usageErr error
	var wg sync.WaitGroup

	wg.Add(1)
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

	// Only fetch usage for paid plans. Free accounts have no usage endpoint.
	skipUsage := acct.SubscriptionType == "free"
	if !skipUsage {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Fprintf(os.Stderr, "cq: panic in claude usage fetch: %v\n%s\n", rv, debug.Stack())
					usageErr = fmt.Errorf("panic: %v", rv)
				}
			}()
			usageBody, usageCode, _, usageDiag, usageErr = p.client.FetchUsage(ctx, token)
			if usageErr != nil {
				fmt.Fprintf(os.Stderr, "cq: claude usage: %v\n", usageErr)
			}
		}()
	}
	wg.Wait()

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
	// discovery and deduplication — even if usage failed, so that
	// stale plan/tier metadata is corrected.
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

	// Build error results with refreshed metadata (not the stale closure values).
	errorWithProfile := func(code, msg string, httpCode int) quota.Result {
		r := quota.ErrorResult(code, msg, httpCode)
		r.Email = prof.Email
		if r.Email == "" {
			r.Email = acct.Email
		}
		r.AccountID = prof.AccountUUID
		if r.AccountID == "" {
			r.AccountID = acct.AccountUUID
		}
		r.Plan = plan
		r.RateLimitTier = rlt
		return r
	}

	// Free plan: no usage data available.
	if plan == "free" {
		return errorWithProfile("free_plan", "usage not available on free plan", 0)
	}

	if usageErr != nil {
		return errorWithProfile("fetch_error", fmt.Sprintf("usage: %v", usageErr), 0)
	}
	if usageCode != 200 {
		msg := "api error"
		if usageDiag != "" {
			msg = fmt.Sprintf("api error (%s)", usageDiag)
		}
		return errorWithProfile("api_error", msg, usageCode)
	}

	email := prof.Email
	if email == "" {
		email = acct.Email
	}
	accountID := prof.AccountUUID
	if accountID == "" {
		accountID = acct.AccountUUID
	}

	return parseUsage(usageBody, plan, rlt, email, accountID)
}

// FetchAccountUsage fetches only usage data for a single account (no profile fetch).
// It uses metadata already stored in the keyring entry for plan/tier.
// Returns the Retry-After duration from a 429 response alongside the result.
func (p *Provider) FetchAccountUsage(ctx context.Context, acct keyring.ClaudeOAuth, now time.Time) (quota.Result, time.Duration, error) {
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
		return errorWithIdentity("no_token", "no token", 0), 0, nil
	}

	if acct.SubscriptionType == "free" {
		return errorWithIdentity("free_plan", "usage not available on free plan", 0), 0, nil
	}

	nowMs := now.UnixMilli()
	if acct.ExpiresAt > 0 && acct.ExpiresAt < nowMs && acct.RefreshToken != "" {
		rr, err := RefreshToken(ctx, p.client.http, acct.RefreshToken, acct.Scopes)
		if err != nil {
			if _, probeErr := p.client.FetchProfile(ctx, token); probeErr != nil {
				return errorWithIdentity("auth_expired", "auth expired", 0), 0, nil
			}
		} else {
			token = rr.AccessToken
			acct.AccessToken = rr.AccessToken
			acct.ExpiresAt = nowMs + rr.ExpiresIn*1000
			if rr.RefreshToken != "" {
				acct.RefreshToken = rr.RefreshToken
			}
			persistRefreshedToken(&acct)
		}
	}

	body, statusCode, retryAfter, diagnostics, err := p.client.FetchUsage(ctx, token)
	if err != nil {
		return errorWithIdentity("fetch_error", fmt.Sprintf("usage: %v", err), 0), 0, nil
	}
	if statusCode != http.StatusOK {
		msg := "api error"
		if diagnostics != "" {
			msg = fmt.Sprintf("api error (%s)", diagnostics)
		}
		return errorWithIdentity("api_error", msg, statusCode), retryAfter, nil
	}

	return parseUsage(body, acct.SubscriptionType, acct.RateLimitTier, acct.Email, acct.AccountUUID), 0, nil
}
