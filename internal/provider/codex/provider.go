package codex

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

// Provider implements provider.Provider for Codex (OpenAI).
type Provider struct {
	client httputil.Doer
	fs     fsutil.FileSystem
}

// New creates a Provider that uses the given HTTP client for API calls.
func New(client httputil.Doer) *Provider {
	return &Provider{client: client, fs: fsutil.OSFileSystem{}}
}

// Fetch discovers all Codex accounts and fetches quota for each in parallel.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) ([]quota.Result, error) {
	accounts := DiscoverAccounts(p.fs)
	if len(accounts) == 0 {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}

	results := make([]quota.Result, len(accounts))
	var wg sync.WaitGroup
	for i, acct := range accounts {
		wg.Add(1)
		go func(i int, acct CodexAccount) {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Fprintf(os.Stderr, "cq: panic in codex provider: %v\n%s\n", rv, debug.Stack())
					results[i] = quota.ErrorResult("panic", fmt.Sprintf("%v", rv), 0)
				}
			}()
			results[i] = p.fetchAccount(ctx, acct)
			results[i].Active = acct.IsActive
		}(i, acct)
	}
	wg.Wait()

	return dedup(results), nil
}

// DiscoverAccounts returns all locally known Codex accounts without making
// network calls. It implements provider.Discoverer so the runner can
// synthesise auth_expired rows for accounts absent from the cache.
func (p *Provider) DiscoverAccounts(_ context.Context) ([]provider.Account, error) {
	accts := DiscoverAccounts(p.fs)
	out := make([]provider.Account, len(accts))
	for i, a := range accts {
		out[i] = provider.Account{
			AccountID: a.AccountID,
			Email:     a.Email,
			Label:     a.PlanType,
			Active:    a.IsActive,
		}
	}
	return out, nil
}

// fetchAccount fetches quota for a single Codex account, attempting token
// refresh on 401/403 before giving up. On final failure, identity fields
// (Email, AccountID) are preserved in the error result.
func (p *Provider) fetchAccount(ctx context.Context, acct CodexAccount) quota.Result {
	if acct.AccessToken == "" {
		r := quota.ErrorResult("no_token", "no token", 0)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	body, code, err := fetchUsage(ctx, p.client, acct.AccessToken, acct.AccountID)
	if err != nil {
		r := quota.ErrorResult("transport_error", err.Error(), 0)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	if code == 401 || code == 403 {
		// Attempt refresh if we have a refresh token. Always retry usage once
		// after the attempt, whether or not refresh succeeded.
		if acct.RefreshToken != "" {
			tokens, refreshErr := auth.RefreshCodexToken(ctx, p.client, acct.RefreshToken)
			if refreshErr == nil {
				acct.AccessToken = tokens.AccessToken
				if tokens.RefreshToken != "" {
					acct.RefreshToken = tokens.RefreshToken
				}
				if tokens.IDToken != "" {
					acct.IDToken = tokens.IDToken
				}
				claims := auth.DecodeCodexClaims(tokens.IDToken)
				if claims.ExpiresAt > 0 {
					acct.ExpiresAt = claims.ExpiresAt * 1000
				} else {
					acct.ExpiresAt = time.Now().UnixMilli() + tokens.ExpiresIn*1000
				}
				if home, homeErr := p.fs.UserHomeDir(); homeErr == nil {
					if err := PersistCodexAccount(p.fs, acct, home); err != nil {
						fmt.Fprintf(os.Stderr, "cq: persist codex tokens: %v\n", err)
					}
				}
			}

			// Retry usage regardless of whether refresh succeeded.
			body2, code2, err2 := fetchUsage(ctx, p.client, acct.AccessToken, acct.AccountID)
			if err2 != nil {
				r := quota.ErrorResult("transport_error", err2.Error(), 0)
				r.Email = acct.Email
				r.AccountID = acct.AccountID
				return r
			}
			if code2 == 200 {
				return parseUsage(body2, acct.Email, acct.AccountID)
			}
			if code2 == 401 || code2 == 403 {
				r := quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex login", code2)
				r.Email = acct.Email
				r.AccountID = acct.AccountID
				return r
			}
			r := quota.ErrorResult("api_error", "api error", code2)
			r.Email = acct.Email
			r.AccountID = acct.AccountID
			return r
		}
		// No refresh token — return auth_expired with identity.
		r := quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex login", code)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	if code != 200 {
		r := quota.ErrorResult("api_error", "api error", code)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	return parseUsage(body, acct.Email, acct.AccountID)
}

// dedup removes duplicate results by AccountID, preferring usable results
// over errors when both exist for the same account.
func dedup(results []quota.Result) []quota.Result {
	if len(results) <= 1 {
		return results
	}
	seen := make(map[string]int) // key -> index in out
	var out []quota.Result
	for _, r := range results {
		key := r.AccountID
		if key == "" {
			key = r.Email
		}
		if key == "" {
			out = append(out, r)
			continue
		}
		if idx, exists := seen[key]; exists {
			if r.IsUsable() && !out[idx].IsUsable() {
				out[idx] = r
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, r)
	}
	return out
}
