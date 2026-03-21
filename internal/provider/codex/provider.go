package codex

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
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

// fetchAccount fetches quota for a single Codex account.
func (p *Provider) fetchAccount(ctx context.Context, acct CodexAccount) quota.Result {
	if acct.AccessToken == "" {
		return quota.ErrorResult("no_token", "no token", 0)
	}

	body, code, err := fetchUsage(ctx, p.client, acct.AccessToken, acct.AccountID)
	if err != nil {
		return quota.ErrorResult("transport_error", err.Error(), 0)
	}

	// Do not refresh — cq shares credentials with codex CLI and codex-auth,
	// and Auth0 refresh token rotation would invalidate their copies.
	if code == 401 || code == 403 {
		return quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex login", code)
	}

	if code != 200 {
		return quota.ErrorResult("api_error", "api error", code)
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
