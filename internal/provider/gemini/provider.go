package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/quota"
)

// Provider implements provider.Provider for Gemini (Google Cloud Code Assist).
type Provider struct {
	client httputil.Doer
	fs     fsutil.FileSystem
}

// New creates a Provider that uses the given HTTP client for API calls.
func New(client httputil.Doer) *Provider {
	return &Provider{client: client, fs: fsutil.OSFileSystem{}}
}

// Fetch reads ~/.gemini/oauth_creds.json, fetches tier and quota in parallel,
// and returns the parsed result.
func (p *Provider) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	home, err := p.fs.UserHomeDir()
	if err != nil {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}
	credsFile := filepath.Join(home, ".gemini", "oauth_creds.json")
	data, err := p.fs.ReadFile(credsFile)
	if err != nil {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}

	var creds struct {
		AccessToken  string  `json:"access_token"`
		ExpiryDate   float64 `json:"expiry_date"`
		RefreshToken string  `json:"refresh_token"`
		IDToken      string  `json:"id_token"`
	}
	if json.Unmarshal(data, &creds) != nil {
		return []quota.Result{quota.ErrorResult("parse_error", "", 0)}, nil
	}

	if creds.AccessToken == "" {
		return []quota.Result{quota.ErrorResult("no_token", "no token", 0)}, nil
	}

	// Refresh the access token if expired.
	nowMs := float64(now.UnixMilli())
	if creds.ExpiryDate > 0 && creds.ExpiryDate < nowMs {
		if creds.RefreshToken == "" {
			return []quota.Result{quota.ErrorResult("auth_expired", "auth expired — re-authenticate via gemini", 0)}, nil
		}
		tok, err := refreshAccessToken(ctx, p.client, creds.RefreshToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: gemini token refresh failed: %v\n", err)
			return []quota.Result{quota.ErrorResult("auth_expired", "auth expired — re-authenticate via gemini", 0)}, nil
		}
		creds.AccessToken = tok.AccessToken
		creds.ExpiryDate = float64(now.Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli())
		if tok.IDToken != "" {
			creds.IDToken = tok.IDToken
		}

		// Persist refreshed credentials atomically, preserving unknown fields.
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err == nil {
			raw["access_token"] = creds.AccessToken
			raw["expiry_date"] = creds.ExpiryDate
			if tok.IDToken != "" {
				raw["id_token"] = tok.IDToken
			}
			if updated, err := json.Marshal(raw); err == nil {
				tmp := credsFile + ".tmp"
				if err := p.fs.WriteFile(tmp, updated, 0o600); err != nil {
					fmt.Fprintf(os.Stderr, "cq: gemini write refreshed creds: %v\n", err)
				} else if err := p.fs.Rename(tmp, credsFile); err != nil {
					fmt.Fprintf(os.Stderr, "cq: gemini rename refreshed creds: %v\n", err)
					p.fs.Remove(tmp)
				}
			}
		}
	}

	token := creds.AccessToken
	email := auth.DecodeEmail(creds.IDToken)

	// Fetch tier and quota in parallel.
	var tierRaw []byte
	var quotaBody []byte
	var quotaErr error
	quotaCode := 0
	quotaPanic := false
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if rv := recover(); rv != nil {
				fmt.Fprintf(os.Stderr, "cq: panic in gemini tier fetch: %v\n%s\n", rv, debug.Stack())
			}
		}()
		var err error
		tierRaw, err = fetchTier(ctx, p.client, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: gemini tier: %v\n", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if rv := recover(); rv != nil {
				fmt.Fprintf(os.Stderr, "cq: panic in gemini quota fetch: %v\n%s\n", rv, debug.Stack())
				quotaPanic = true
			}
		}()
		quotaBody, quotaCode, quotaErr = fetchQuota(ctx, p.client, token)
		if quotaErr != nil {
			fmt.Fprintf(os.Stderr, "cq: gemini quota: %v\n", quotaErr)
		}
	}()
	wg.Wait()

	tier := parseTier(tierRaw)

	if quotaPanic {
		return []quota.Result{quota.ErrorResult("fetch_panic", "quota fetch failed (panic)", 0)}, nil
	}
	if quotaErr != nil {
		return []quota.Result{quota.ErrorResult("fetch_error", fmt.Sprintf("quota: %v", quotaErr), 0)}, nil
	}
	if quotaCode != 200 {
		return []quota.Result{quota.ErrorResult("api_error", "api error", quotaCode)}, nil
	}

	result := parseQuota(quotaBody, tier, email)
	result.Active = true
	return []quota.Result{result}, nil
}
