package codex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
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

// Fetch reads ~/.codex/auth.json, fetches quota, and returns the parsed result.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) ([]quota.Result, error) {
	home, err := p.fs.UserHomeDir()
	if err != nil {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}
	authFile := filepath.Join(home, ".codex", "auth.json")
	data, err := p.fs.ReadFile(authFile)
	if err != nil {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}

	var authData struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			AccountID    string `json:"account_id"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &authData) != nil {
		return []quota.Result{quota.ErrorResult("parse_error", "", 0)}, nil
	}

	token := authData.Tokens.AccessToken
	if token == "" {
		return []quota.Result{quota.ErrorResult("no_token", "no token", 0)}, nil
	}

	email := auth.DecodeEmail(authData.Tokens.IDToken)

	body, code, err := fetchUsage(ctx, p.client, token, authData.Tokens.AccountID)
	if err != nil {
		return []quota.Result{quota.ErrorResult("transport_error", err.Error(), 0)}, nil
	}

	// Do not refresh — cq shares ~/.codex/auth.json with codex CLI, and Auth0
	// refresh token rotation would invalidate codex's copy.
	if code == 401 || code == 403 {
		return []quota.Result{quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex", code)}, nil
	}

	if code != 200 {
		return []quota.Result{quota.ErrorResult("api_error", "api error", code)}, nil
	}

	return []quota.Result{parseUsage(body, email)}, nil
}
