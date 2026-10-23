package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
)

// Accounts implements provider.AccountManager for Claude.
type Accounts struct {
	HTTP      httputil.Doer
	Mutations *AccountMutationOperations
}

var (
	discoverClaudeAccounts                      = keyring.DiscoverClaudeAccounts
	removeCQClaudeAccountsByEmail               = keyring.RemoveCQClaudeAccountsByEmail
	removeActiveClaudeCredentialsByEmail        = keyring.RemoveActiveClaudeCredentialsByEmail
	removePlatformClaudeKeychainAccountsByEmail = keyring.RemovePlatformClaudeKeychainAccountsByEmail
	writeCredentialsFile                        = keyring.WriteCredentialsFile
	updateKeychainEntry                         = keyring.UpdateKeychainEntry
	storeCQAccount                              = keyring.StoreCQAccount
)

func (a *Accounts) ProviderID() provider.ID { return provider.Claude }

// Discover returns all known Claude accounts from the credentials file,
// platform keychain, and cq-managed keyring.
func (a *Accounts) Discover(_ context.Context) ([]provider.Account, error) {
	accts := discoverClaudeAccounts()
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
		refreshFailed := false
		if a.HTTP != nil && acctCopy.RefreshToken != "" && acctCopy.ExpiresAt > 0 && acctCopy.ExpiresAt < time.Now().UnixMilli() {
			rr, err := RefreshToken(ctx, a.HTTP, acctCopy.RefreshToken, acctCopy.Scopes)
			if err != nil {
				refreshFailed = true
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
				if refreshFailed {
					return provider.Account{}, fmt.Errorf("profile refresh failed after token refresh failure: %w", err)
				}
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
		if err := writeCredentialsFile(creds); err != nil {
			return provider.Account{}, fmt.Errorf("write credentials: %w", err)
		}
		if err := updateKeychainEntry("Claude Code-credentials", creds); err != nil {
			fmt.Fprintf(os.Stderr, "warning: keychain update failed: %v\n", err)
		}
		// Persist refreshed metadata to the cq keyring.
		if acctCopy.AccountUUID != "" {
			if err := storeCQAccount(&acctCopy); err != nil {
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
	if err := removePlatformClaudeKeychainAccountsByEmail(identifier); err != nil {
		return fmt.Errorf("remove platform Claude keychain accounts: %w", err)
	}
	if err := removeCQClaudeAccountsByEmail(identifier); err != nil {
		return fmt.Errorf("remove cq-managed Claude accounts: %w", err)
	}
	if err := removeActiveClaudeCredentialsByEmail(identifier); err != nil {
		return fmt.Errorf("remove active Claude credentials: %w", err)
	}
	return nil
}

// Inspect reads complete local metadata without the legacy discovery merging
// policy, refresh, persistence, or network access.
func (a *Accounts) Inspect(ctx context.Context) ([]keyring.ClaudeAccountInspection, error) {
	if a.Mutations != nil && a.Mutations.Inspect != nil {
		return a.Mutations.Inspect(ctx)
	}
	return keyring.InspectClaudeAccounts(ctx)
}

// AccountMutationOperations are the narrow native-store seams used by explicit
// account transactions. Nil uses the platform implementation.
type AccountMutationOperations struct {
	Inspect func(context.Context) ([]keyring.ClaudeAccountInspection, error)
	Store   func(context.Context, *keyring.ClaudeOAuth) error
	Write   func(context.Context, *keyring.ClaudeCredentials) error
	Update  func(context.Context, string, *keyring.ClaudeCredentials) error
	Remove  func(context.Context, keyring.ClaudeOAuth) (keyring.ClaudeRemovalResult, error)
}

func (a *Accounts) mutationOperations() AccountMutationOperations {
	if a.Mutations != nil {
		return *a.Mutations
	}
	return AccountMutationOperations{Inspect: keyring.InspectClaudeAccounts, Store: keyring.StoreCQAccountContext, Write: keyring.WriteCredentialsFileContext, Update: keyring.UpdateKeychainEntryContext, Remove: keyring.RemoveClaudeAccountContext}
}

type AccountReferenceError struct{ Code string }

func (e *AccountReferenceError) Error() string { return "Claude account reference " + e.Code }
func ResolveAccountReference(rows []keyring.ClaudeAccountInspection, reference string) (keyring.ClaudeAccountInspection, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return keyring.ClaudeAccountInspection{}, &AccountReferenceError{Code: "empty"}
	}
	var selected keyring.ClaudeAccountInspection
	count := 0
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.Account.Email), reference) {
			selected = row
			count++
		}
	}
	if count == 0 {
		return selected, &AccountReferenceError{Code: "missing"}
	}
	if count != 1 {
		return selected, &AccountReferenceError{Code: "ambiguous"}
	}
	return selected, nil
}
func (a *Accounts) revalidate(ctx context.Context, expected keyring.ClaudeOAuth) (keyring.ClaudeAccountInspection, error) {
	rows, err := a.mutationOperations().Inspect(ctx)
	if err != nil {
		return keyring.ClaudeAccountInspection{}, err
	}
	selected, err := ResolveAccountReference(rows, expected.Email)
	if err != nil {
		return selected, err
	}
	if !keyring.SameClaudeIdentity(selected.Account, expected) {
		return selected, keyring.ErrClaudeIdentityChanged
	}
	return selected, nil
}

type AccountActivationResult struct {
	Account         keyring.ClaudeOAuth
	Changed, Active bool
}

func (a *Accounts) ActivateSelected(ctx context.Context, expected keyring.ClaudeAccountInspection) (AccountActivationResult, error) {
	result := AccountActivationResult{Account: expected.Account}
	selected, err := a.revalidate(ctx, expected.Account)
	if err != nil {
		return result, err
	}
	if expected.Active {
		if !selected.Active {
			return result, keyring.ErrClaudeIdentityChanged
		}
		result.Active = true
		return result, nil
	}
	managed := false
	for _, source := range selected.Sources {
		managed = managed || source == "cq_managed"
	}
	if !managed {
		return result, &AccountReferenceError{Code: "not_activatable"}
	}
	account := selected.Account
	ops := a.mutationOperations()
	ctx = keyring.WithDiagnostics(ctx, nil)
	refreshFailed := false
	if a.HTTP != nil && account.RefreshToken != "" && account.ExpiresAt > 0 && account.ExpiresAt < time.Now().UnixMilli() {
		refreshed, err := RefreshToken(ctx, a.HTTP, account.RefreshToken, account.Scopes)
		if err != nil {
			refreshFailed = true
		} else {
			account.AccessToken = refreshed.AccessToken
			account.ExpiresAt = time.Now().UnixMilli() + refreshed.ExpiresIn*1000
			if refreshed.RefreshToken != "" {
				account.RefreshToken = refreshed.RefreshToken
			}
			if err := ops.Store(ctx, &account); err != nil {
				return result, err
			}
		}
	}
	if a.HTTP != nil {
		profile, err := (&Client{http: a.HTTP}).FetchProfile(ctx, account.AccessToken)
		if err != nil && refreshFailed {
			return result, err
		}
		if err == nil {
			if profile.Plan != "" {
				account.SubscriptionType = profile.Plan
			}
			if profile.RateLimitTier != "" {
				account.RateLimitTier = profile.RateLimitTier
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Revalidate after network work; a same-email replacement is not the selected identity.
	if _, err := a.revalidate(ctx, expected.Account); err != nil {
		return result, err
	}
	credentials := &keyring.ClaudeCredentials{ClaudeAiOauth: &account}
	if err := ops.Write(ctx, credentials); err != nil {
		return result, err
	}
	result.Account, result.Changed, result.Active = account, true, true
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := ops.Update(ctx, "Claude Code-credentials", credentials); err != nil {
		return result, err
	}
	if account.AccountUUID != "" {
		if err := ops.Store(ctx, &account); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (a *Accounts) RemoveSelected(ctx context.Context, expected keyring.ClaudeOAuth) (keyring.ClaudeRemovalResult, error) {
	if _, err := a.revalidate(ctx, expected); err != nil {
		return keyring.ClaudeRemovalResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return keyring.ClaudeRemovalResult{}, err
	}
	return a.mutationOperations().Remove(keyring.WithDiagnostics(ctx, nil), expected)
}

// SaveLogin stores an authenticated identity without changing the native default.
func (a *Accounts) SaveLogin(ctx context.Context, account keyring.ClaudeOAuth) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if account.AccountUUID == "" || account.AccessToken == "" {
		return false, errors.New("authenticated account identity missing")
	}
	err := a.mutationOperations().Store(keyring.WithDiagnostics(ctx, nil), &account)
	var committed *keyring.ClaudeStoreError
	return err == nil || errors.As(err, &committed), err
}
