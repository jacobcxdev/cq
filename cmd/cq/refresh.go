package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// refreshMarginMs is how far ahead of expiry we proactively refresh (30 min).
const refreshMarginMs = 30 * 60 * 1000

var (
	discoverClaudeAccountsFn  = keyring.DiscoverClaudeAccounts
	newHTTPClientFn           = func(timeout time.Duration, version string) httputil.Doer { return httputil.NewClient(timeout, version) }
	refreshCodexAccountsFn    = refreshCodexAccounts
	invalidateProviderCacheFn = invalidateProviderCache
	codexRefreshFSFactory     = func() fsutil.DurableFileSystem { return fsutil.OSFileSystem{} }
	persistRefreshedTokenFn   = keyring.PersistRefreshedToken
	storeCQAccountFn          = keyring.StoreCQAccount
	activeClaudeEmailFn       = keyring.ActiveClaudeEmail
	isStdinTerminalFn         = isStdinTerminal
	resolveRefreshRootsFn     = func() (userdirs.Roots, error) { return userdirs.Default() }
)

func runRefreshCommand(args []string) error {
	if helpRequested(args) {
		return writeManualHelp(os.Stdout, []string{"refresh"})
	}
	if len(args) > 0 {
		return fmt.Errorf("refresh: unexpected arguments")
	}
	return runRefresh()
}

func runRefresh() error {
	if _, err := resolveRefreshRootsFn(); err != nil {
		return fmt.Errorf("resolve CQ directories: %w", err)
	}
	client := newHTTPClientFn(10*time.Second, version)
	ctx := context.Background()
	now := time.Now()
	work := refreshClaudeOutcomes(ctx, v2AuthDependencies{
		HTTP: client, DiscoverClaude: func(context.Context) []keyring.ClaudeOAuth { return discoverClaudeAccountsFn() },
		PersistClaude: func(ctx context.Context, a *keyring.ClaudeOAuth) (bool, error) {
			r := keyring.PersistRefreshedTokenResultContext(ctx, a)
			return r.Changed, r.Err
		},
		Login: func(ctx context.Context) (authReauthResult, error) {
			return refreshClaudeLogin(ctx, client, auth.Login, persistV2Claude, time.Now)
		},
	}, now, false, &cli.Session{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: isStdinTerminalFn()})
	finishAuthProvider(&work)
	if work.result.CredentialsChanged {
		invalidateProviderCacheFn(provider.Claude)
	}
	for _, row := range work.result.Accounts {
		if row.Status == "refreshed" {
			fmt.Fprintf(os.Stderr, "cq: refreshed %s\n", row.AccountLabel)
		}
	}
	changed, err := refreshCodexAccountsFn(ctx, client, now.UnixMilli())
	if changed {
		invalidateProviderCacheFn(provider.Codex)
	}
	if err != nil {
		return err
	}
	if work.result.ReauthRequired > 0 {
		return fmt.Errorf("%d account(s) need interactive reauth (run `cq refresh` in a terminal)", work.result.ReauthRequired)
	}
	if len(work.errors) > 0 {
		return errors.New(work.errors[0].Message)
	}
	return nil
}

func refreshCodexAccounts(ctx context.Context, client httputil.Doer, nowMs int64) (bool, error) {
	fs := codexRefreshFSFactory()
	control, err := codexprov.OpenDefaultCredentialRefreshControl(ctx, fs, client)
	if err != nil {
		return false, codexRefreshAuthorityError(err)
	}
	defer control.Close()
	return refreshManagedCodexAuthority(ctx, control, time.UnixMilli(nowMs))
}

type codexRefreshAuthority interface {
	codexprov.CredentialInventory
	codexprov.CredentialRefreshBroker
}

func refreshManagedCodexAuthority(ctx context.Context, authority codexRefreshAuthority, now time.Time) (bool, error) {
	work := refreshManagedCodexOutcomes(ctx, authority, now)
	finishAuthProvider(&work)
	return work.result.CredentialsChanged, work.err
}

func codexRefreshAuthorityError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, codexprov.ErrCredentialInventoryDegraded):
		return codexprov.ErrCredentialInventoryDegraded
	default:
		return codexprov.ErrCredentialAuthorityUnavailable
	}
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
	changed := false
	updated := syncAnonymousCredentials(ctx, client, accounts, nowMs, func(acct *keyring.ClaudeOAuth) {
		persistRefreshedTokenFn(acct)
		if acct.AccountUUID != "" {
			if err := storeCQAccountFn(acct); err != nil {
				fmt.Fprintf(os.Stderr, "cq: store %s: %v\n", acctLabel(*acct), err)
			}
		}
		changed = true
	})
	return updated, changed
}

func syncAnonymousCredentials(ctx context.Context, client httputil.Doer, accounts []keyring.ClaudeOAuth, nowMs int64, persist func(*keyring.ClaudeOAuth)) []keyring.ClaudeOAuth {
	if len(accounts) == 0 {
		return accounts
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
		if ctx.Err() != nil {
			break
		}
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
		persist(&updated[idx])
		remove[i] = struct{}{}
		changed = true
	}
	if !changed {
		return updated
	}

	result := make([]keyring.ClaudeOAuth, 0, len(updated)-len(remove))
	for i, acct := range updated {
		if _, dropped := remove[i]; dropped {
			continue
		}
		result = append(result, acct)
	}
	return result
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
