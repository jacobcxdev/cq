package claude

import (
	"context"
	"fmt"
	"os"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
)

// Accounts implements provider.AccountManager for Claude.
type Accounts struct{}

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

// Switch sets the active Claude account by email. It writes the matching
// account's credentials to the credentials file and updates Claude Code's
// keychain entry.
func (a *Accounts) Switch(_ context.Context, identifier string) (provider.Account, error) {
	accts := keyring.DiscoverClaudeAccounts()
	for _, acct := range accts {
		if acct.Email == identifier {
			acctCopy := acct
			creds := &keyring.ClaudeCredentials{ClaudeAiOauth: &acctCopy}
			if err := keyring.WriteCredentialsFile(creds); err != nil {
				return provider.Account{}, fmt.Errorf("write credentials: %w", err)
			}
			if err := keyring.UpdateKeychainEntry("Claude Code-credentials", creds); err != nil {
				fmt.Fprintf(os.Stderr, "warning: keychain update failed: %v\n", err)
			}
			return provider.Account{
				AccountID: acct.AccountUUID,
				Email:     acct.Email,
				Label:     acct.SubscriptionType,
				Active:    true,
				SwitchID:  acct.Email,
			}, nil
		}
	}
	return provider.Account{}, fmt.Errorf("no account found with email %q", identifier)
}
