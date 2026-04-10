package claude

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
)

// Accounts implements provider.AccountManager for Claude.
type Accounts struct {
	HTTP httputil.Doer
}

var (
	discoverClaudeAccounts                    = keyring.DiscoverClaudeAccounts
	removeCQClaudeAccountsByEmail            = keyring.RemoveCQClaudeAccountsByEmail
	removeActiveClaudeCredentialsByEmail     = keyring.RemoveActiveClaudeCredentialsByEmail
	removePlatformClaudeKeychainAccountsByEmail = keyring.RemovePlatformClaudeKeychainAccountsByEmail
)

func (a *Accounts) ProviderID() provider.ID { return provider.Claude }

// Discover returns all known Claude accounts from the credentials file,
// platform keychain, and cq-managed keyring.
func (a *Accounts) Discover(_ context.Context) ([]provider.Account, error) {
	accts := keyring.DiscoverClaudeAccounts()
	out := make([]provider.Account, len(accts))
	for i, acct := range accts {
		out[i] = provider.Account{
			AccountID:     acct.AccountUUID,
			Email:         acct.Email,
			Label:         acct.SubscriptionType,
			RateLimitTier: acct.RateLimitTier,
			SwitchID:      acct.Email,
		}
	}
	return out, nil
}

// Switch sets the active Claude account by email. It refreshes account
// metadata from the profile API (best-effort), writes the credentials to
// the credentials file, and updates Claude Code's keychain entry.
func (a *Accounts) Switch(ctx context.Context, identifier string) (provider.Account, error) {
	accts := discoverClaudeAccounts()
	for _, acct := range accts {
		if acct.Email != identifier {
			continue
		}
		acctCopy := acct

		// Refresh expired token before attempting profile fetch.
		if a.HTTP != nil && acctCopy.RefreshToken != "" && acctCopy.ExpiresAt > 0 && acctCopy.ExpiresAt < time.Now().UnixMilli() {
			rr, err := RefreshToken(ctx, a.HTTP, acctCopy.RefreshToken, acctCopy.Scopes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: token refresh failed: %v\n", err)
			} else {
				acctCopy.AccessToken = rr.AccessToken
				acctCopy.ExpiresAt = time.Now().UnixMilli() + rr.ExpiresIn*1000
				if rr.RefreshToken != "" {
					acctCopy.RefreshToken = rr.RefreshToken
				}
				persistRefreshedToken(&acctCopy)
			}
		}

		// Best-effort profile refresh to pick up plan/tier changes.
		if a.HTTP != nil {
			client := &Client{http: a.HTTP}
			if p, err := client.FetchProfile(ctx, acctCopy.AccessToken); err != nil {
				fmt.Fprintf(os.Stderr, "warning: profile refresh failed: %v\n", err)
			} else {
				if p.Plan != "" {
					acctCopy.SubscriptionType = p.Plan
				}
				if p.RateLimitTier != "" {
					acctCopy.RateLimitTier = p.RateLimitTier
				}
			}
		}

		creds := &keyring.ClaudeCredentials{ClaudeAiOauth: &acctCopy}
		if err := keyring.WriteCredentialsFile(creds); err != nil {
			return provider.Account{}, fmt.Errorf("write credentials: %w", err)
		}
		if err := keyring.UpdateKeychainEntry("Claude Code-credentials", creds); err != nil {
			fmt.Fprintf(os.Stderr, "warning: keychain update failed: %v\n", err)
		}
		// Persist refreshed metadata to the cq keyring.
		if acctCopy.AccountUUID != "" {
			if err := keyring.StoreCQAccount(&acctCopy); err != nil {
				fmt.Fprintf(os.Stderr, "warning: keyring store failed: %v\n", err)
			}
		}
		return provider.Account{
			AccountID:     acctCopy.AccountUUID,
			Email:         acctCopy.Email,
			Label:         acctCopy.SubscriptionType,
			RateLimitTier: acctCopy.RateLimitTier,
			Active:        true,
			SwitchID:      acctCopy.Email,
		}, nil
	}
	return provider.Account{}, fmt.Errorf("no account found with email %q", identifier)
}

func (a *Accounts) Remove(_ context.Context, identifier string) error {
	found := false
	for _, acct := range discoverClaudeAccounts() {
		if acct.Email == identifier {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no account found with email %q", identifier)
	}
	if err := removeCQClaudeAccountsByEmail(identifier); err != nil {
		return fmt.Errorf("remove cq-managed Claude accounts: %w", err)
	}
	if err := removeActiveClaudeCredentialsByEmail(identifier); err != nil {
		return fmt.Errorf("remove active Claude credentials: %w", err)
	}
	if err := removePlatformClaudeKeychainAccountsByEmail(identifier); err != nil {
		return fmt.Errorf("remove platform Claude keychain accounts: %w", err)
	}
	return nil
}
