package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

// refreshMarginMs is how far ahead of expiry we proactively refresh (30 min).
const refreshMarginMs = 30 * 60 * 1000

var (
	discoverClaudeAccountsFn   = keyring.DiscoverClaudeAccounts
	newHTTPClientFn           = func(timeout time.Duration, version string) httputil.Doer { return httputil.NewClient(timeout, version) }
	refreshCodexAccountsFn    = refreshCodexAccounts
	invalidateProviderCacheFn = invalidateProviderCache
	codexRefreshFSFactory     = func() fsutil.FileSystem { return fsutil.OSFileSystem{} }
	persistRefreshedTokenFn   = keyring.PersistRefreshedToken
	storeCQAccountFn          = keyring.StoreCQAccount
	activeClaudeEmailFn       = keyring.ActiveClaudeEmail
	isStdinTerminalFn         = isStdinTerminal
)

func runRefresh() error {
	accounts := discoverClaudeAccountsFn()
	httpClient := newHTTPClientFn(10*time.Second, version)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	var claudeChanged bool
	var needsReauth []keyring.ClaudeOAuth
	if len(accounts) > 0 {
		// Sync fresh anonymous keychain tokens into stale identified entries.
		// After Claude Code rotates tokens, mergeAnonymousFresh can't match
		// them (token affinity fails). We resolve the anonymous entry's email
		// via the profile API and sync its tokens into the matching account.
		accounts, claudeChanged = syncAnonymousToIdentifiedWithChange(ctx, httpClient, accounts, now)

		threshold := now + refreshMarginMs
		for _, acct := range accounts {
			if acct.RefreshToken == "" && acct.ExpiresAt > 0 && acct.ExpiresAt < now {
				if acct.Email == "" && acct.AccountUUID == "" {
					continue
				}
				needsReauth = append(needsReauth, acct)
				continue
			}
			if acct.RefreshToken == "" {
				continue
			}
			if acct.ExpiresAt == 0 || acct.ExpiresAt > threshold {
				continue // unknown expiry or token still fresh
			}

			label := acctLabel(acct)

			rr, err := claudeprov.RefreshToken(ctx, httpClient, acct.RefreshToken, acct.Scopes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cq: refresh %s: %v\n", label, err)
				needsReauth = append(needsReauth, acct)
				continue
			}

			acct.AccessToken = rr.AccessToken
			acct.ExpiresAt = now + rr.ExpiresIn*1000
			if rr.RefreshToken != "" {
				acct.RefreshToken = rr.RefreshToken
			}

			persistRefreshedTokenFn(&acct)
			if acct.AccountUUID != "" {
				if err := storeCQAccountFn(&acct); err != nil {
					fmt.Fprintf(os.Stderr, "cq: store %s: %v\n", label, err)
				}
			}

			claudeChanged = true
			fmt.Fprintf(os.Stderr, "cq: refreshed %s\n", label)
		}
	}

	if claudeChanged {
		invalidateProviderCacheFn(provider.Claude)
	}

	// Codex refresh pass.
	codexChanged := refreshCodexAccountsFn(ctx, httpClient, now)
	if codexChanged {
		invalidateProviderCacheFn(provider.Codex)
	}

	if len(accounts) == 0 {
		return nil
	}

	if len(needsReauth) == 0 {
		return nil
	}

	if !isStdinTerminalFn() {
		return fmt.Errorf("%d account(s) need interactive reauth (run `cq refresh` in a terminal)", len(needsReauth))
	}

	fmt.Fprintf(os.Stderr, "\n%d account(s) need to sign in again:\n", len(needsReauth))
	scanner := bufio.NewScanner(os.Stdin)
	var failed int
	for _, acct := range needsReauth {
		label := acctLabel(acct)
		fmt.Fprintf(os.Stderr, "\n  Sign in as: %s\n", label)
		fmt.Fprintf(os.Stderr, "  Press Enter to open browser (or 's' to skip): ")

		if !scanner.Scan() {
			break
		}
		if strings.TrimSpace(scanner.Text()) == "s" {
			fmt.Fprintf(os.Stderr, "  skipped\n")
			failed++
			continue
		}

		if err := app.RunLogin(ctx, httpClient, false); err != nil {
			fmt.Fprintf(os.Stderr, "  login failed: %v\n", err)
			failed++
			continue
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d account(s) still need reauth", failed)
	}
	return nil
}

// refreshCodexAccounts iterates over all locally discovered Codex accounts and
// proactively refreshes those whose tokens expire within refreshMarginMs.
// Returns true if any credential was updated (caller should invalidate cache).
func refreshCodexAccounts(ctx context.Context, client httputil.Doer, nowMs int64) bool {
	fs := codexRefreshFSFactory()
	accounts := codexprov.DiscoverAccounts(fs)
	if len(accounts) == 0 {
		return false
	}

	home, err := fs.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: codex refresh: home dir: %v\n", err)
		return false
	}

	threshold := nowMs + refreshMarginMs
	changed := false

	for _, acct := range accounts {
		if acct.RefreshToken == "" {
			continue
		}
		if acct.ExpiresAt == 0 || acct.ExpiresAt > threshold {
			continue // unknown expiry or still fresh
		}

		label := acct.Email
		if label == "" {
			label = acct.AccountID
		}

		tokens, err := auth.RefreshCodexToken(ctx, client, acct.RefreshToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: codex refresh %s: %v\n", label, err)
			continue
		}

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
			acct.ExpiresAt = nowMs + tokens.ExpiresIn*1000
		}

		if err := codexprov.PersistCodexAccount(fs, acct, home); err != nil {
			fmt.Fprintf(os.Stderr, "cq: codex persist %s: %v\n", label, err)
			continue
		}

		changed = true
		fmt.Fprintf(os.Stderr, "cq: refreshed codex %s\n", label)
	}

	return changed
}

// syncAnonymousToIdentified resolves anonymous keychain entries via the
// profile API and syncs their fresh tokens into matching identified entries.
// This handles the case where Claude Code has rotated both access and refresh
// tokens in the keychain but our stored identified entry is stale.
func syncAnonymousToIdentified(ctx context.Context, client httputil.Doer, accounts []keyring.ClaudeOAuth, nowMs int64) []keyring.ClaudeOAuth {
	updated, _ := syncAnonymousToIdentifiedWithChange(ctx, client, accounts, nowMs)
	return updated
}

// syncAnonymousToIdentifiedWithChange is the testable variant of
// syncAnonymousToIdentified that also reports whether any stored account was
// updated with fresher anonymous tokens.
func syncAnonymousToIdentifiedWithChange(ctx context.Context, client httputil.Doer, accounts []keyring.ClaudeOAuth, nowMs int64) ([]keyring.ClaudeOAuth, bool) {
	if len(accounts) == 0 {
		return accounts, false
	}

	updated := append([]keyring.ClaudeOAuth(nil), accounts...)
	identifiedByEmail := make(map[string]int)
	for i, acct := range updated {
		if acct.Email != "" {
			identifiedByEmail[strings.ToLower(acct.Email)] = i
		}
	}

	changed := false
	remove := make(map[int]struct{})
	for i, acct := range updated {
		if acct.Email != "" || acct.AccessToken == "" || acct.ExpiresAt <= nowMs {
			continue
		}
		email := resolveProfileEmail(ctx, client, acct.AccessToken)
		if email == "" {
			continue
		}
		idx, ok := identifiedByEmail[strings.ToLower(email)]
		if !ok {
			continue
		}
		if updated[idx].ExpiresAt > acct.ExpiresAt {
			continue
		}
		if updated[idx].AccessToken == acct.AccessToken && updated[idx].RefreshToken == acct.RefreshToken && updated[idx].ExpiresAt == acct.ExpiresAt {
			continue
		}

		repaired := updated[idx]
		repaired.AccessToken = acct.AccessToken
		repaired.RefreshToken = acct.RefreshToken
		repaired.ExpiresAt = acct.ExpiresAt
		if len(acct.Scopes) > 0 {
			repaired.Scopes = acct.Scopes
		}
		updated[idx] = repaired
		persistRefreshedTokenFn(&updated[idx])
		if updated[idx].AccountUUID != "" {
			if err := storeCQAccountFn(&updated[idx]); err != nil {
				fmt.Fprintf(os.Stderr, "cq: store %s: %v\n", acctLabel(updated[idx]), err)
			}
		}
		remove[i] = struct{}{}
		changed = true
	}
	if !changed {
		return updated, false
	}

	result := make([]keyring.ClaudeOAuth, 0, len(updated)-len(remove))
	for i, acct := range updated {
		if _, dropped := remove[i]; dropped {
			continue
		}
		result = append(result, acct)
	}
	return result, true
}


// resolveProfileEmail calls the Claude profile API to determine the email
// associated with an access token.
func resolveProfileEmail(ctx context.Context, client httputil.Doer, token string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return ""
	}
	var parsed struct {
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Account.Email
}

func acctLabel(acct keyring.ClaudeOAuth) string {
	if acct.Email != "" {
		return acct.Email
	}
	return "unknown"
}

func isStdinTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
