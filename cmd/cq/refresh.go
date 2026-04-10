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
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
)

// refreshMarginMs is how far ahead of expiry we proactively refresh (30 min).
const refreshMarginMs = 30 * 60 * 1000

func runRefresh() error {
	accounts := keyring.DiscoverClaudeAccounts()
	if len(accounts) == 0 {
		return nil
	}

	httpClient := httputil.NewClient(10*time.Second, version)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Sync fresh anonymous keychain tokens into stale identified entries.
	// After Claude Code rotates tokens, mergeAnonymousFresh can't match
	// them (token affinity fails). We resolve the anonymous entry's email
	// via the profile API and sync its tokens into the matching account.
	var claudeChanged bool
	accounts, claudeChanged = syncAnonymousToIdentifiedWithChange(ctx, httpClient, accounts, now)

	threshold := now + refreshMarginMs

	var needsReauth []keyring.ClaudeOAuth
	for _, acct := range accounts {
		if acct.RefreshToken == "" && acct.ExpiresAt > 0 && acct.ExpiresAt < now {
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

		keyring.PersistRefreshedToken(&acct)
		if acct.AccountUUID != "" {
			if err := keyring.StoreCQAccount(&acct); err != nil {
				fmt.Fprintf(os.Stderr, "cq: store %s: %v\n", label, err)
			}
		}

		claudeChanged = true
		fmt.Fprintf(os.Stderr, "cq: refreshed %s\n", label)
	}

	if claudeChanged {
		invalidateProviderCache(provider.Claude)
	}

	if len(needsReauth) == 0 {
		return nil
	}

	if !isStdinTerminal() {
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

// syncAnonymousToIdentified resolves anonymous keychain entries via the
// profile API and syncs their fresh tokens into matching identified entries.
// This handles the case where Claude Code has rotated both access and refresh
// tokens, breaking token affinity in mergeAnonymousFresh.
func syncAnonymousToIdentified(ctx context.Context, client httputil.Doer, accounts []keyring.ClaudeOAuth, nowMs int64) []keyring.ClaudeOAuth {
	result, _ := syncAnonymousToIdentifiedWithChange(ctx, client, accounts, nowMs)
	return result
}

// syncAnonymousToIdentifiedWithChange is the testable variant of
// syncAnonymousToIdentified that also returns whether any account was mutated.
func syncAnonymousToIdentifiedWithChange(ctx context.Context, client httputil.Doer, accounts []keyring.ClaudeOAuth, nowMs int64) ([]keyring.ClaudeOAuth, bool) {
	// Collect anonymous entries with fresh tokens.
	var anonIndices []int
	identified := make(map[string]int) // email → index
	for i, a := range accounts {
		if a.Email == "" && a.AccountUUID == "" && a.AccessToken != "" && a.ExpiresAt > nowMs {
			anonIndices = append(anonIndices, i)
		}
		if a.Email != "" {
			identified[a.Email] = i
		}
	}
	if len(anonIndices) == 0 || len(identified) == 0 {
		return accounts, false
	}

	merged := make(map[int]bool)
	for _, ai := range anonIndices {
		anon := accounts[ai]

		email := resolveProfileEmail(ctx, client, anon.AccessToken)
		if email == "" {
			continue
		}

		ti, ok := identified[email]
		if !ok {
			continue
		}
		target := accounts[ti]
		if anon.ExpiresAt <= target.ExpiresAt {
			continue // identified entry is already fresher
		}

		// Sync fresh tokens from the anonymous keychain entry.
		target.AccessToken = anon.AccessToken
		target.RefreshToken = anon.RefreshToken
		target.ExpiresAt = anon.ExpiresAt
		if len(anon.Scopes) > 0 {
			target.Scopes = anon.Scopes
		}
		accounts[ti] = target

		keyring.PersistRefreshedToken(&target)
		if target.AccountUUID != "" {
			if err := keyring.StoreCQAccount(&target); err != nil {
				fmt.Fprintf(os.Stderr, "cq: sync store %s: %v\n", email, err)
			}
		}

		fmt.Fprintf(os.Stderr, "cq: synced keychain tokens for %s\n", email)
		merged[ai] = true
	}

	if len(merged) == 0 {
		return accounts, false
	}
	result := make([]keyring.ClaudeOAuth, 0, len(accounts)-len(merged))
	for i, a := range accounts {
		if !merged[i] {
			result = append(result, a)
		}
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
