package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	"github.com/jacobcxdev/cq/internal/quota"
)

var (
	acctBoldStyle   = lipgloss.NewStyle().Bold(true)
	acctDimStyle    = lipgloss.NewStyle().Faint(true)
	acctLabelStyle  = lipgloss.NewStyle().Bold(true).Faint(true).Italic(true)
	acctActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
)

// RunLogin performs the Claude OAuth PKCE login flow.
// The caller is responsible for creating and passing the HTTP client.
func RunLogin(ctx context.Context, client httputil.Doer, activate bool) error {
	tokens, profile, err := auth.Login(ctx, client)
	if err != nil {
		return err
	}

	nowMs := time.Now().UnixMilli()
	expiresIn := tokens.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = auth.DefaultExpiresInSec
	}
	expiresAt := nowMs + expiresIn*1000
	scopes := strings.Fields(tokens.Scope)

	acct := &keyring.ClaudeOAuth{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    expiresAt,
		Scopes:       scopes,
	}

	if profile != nil {
		acct.Email = profile.Email
		acct.AccountUUID = profile.AccountUUID
		acct.SubscriptionType = profile.Plan
		acct.RateLimitTier = profile.RateLimitTier
		acct.Profile = profile.RawJSON
		acct.TokenAccount = &keyring.TokenAccount{
			UUID:             profile.AccountUUID,
			EmailAddress:     profile.Email,
			OrganizationUUID: profile.OrgUUID,
		}
	}

	if acct.AccountUUID != "" {
		if err := keyring.StoreCQAccount(acct); err != nil {
			fmt.Fprintf(os.Stderr, "warning: keyring store failed: %v\n", err)
		}
	}

	if activate {
		creds := &keyring.ClaudeCredentials{ClaudeAiOauth: acct}
		if err := keyring.WriteCredentialsFile(creds); err != nil {
			return fmt.Errorf("write credentials: %w", err)
		}
		if err := keyring.UpdateKeychainEntry("Claude Code-credentials", creds); err != nil {
			fmt.Fprintf(os.Stderr, "warning: keychain update failed: %v\n", err)
		}
	}

	if acct.Email != "" {
		fmt.Printf("Logged in as %s\n", acct.Email)
	} else {
		fmt.Println("Login successful.")
	}
	return nil
}

// RunAccounts lists discovered accounts for the given provider.
func RunAccounts(id provider.ID) error {
	mgr := AccountManager(id)
	if mgr == nil {
		return fmt.Errorf("account management not supported for %s", id)
	}

	accounts, err := mgr.Discover(context.Background())
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Printf("No %s accounts found.\n", id)
		return nil
	}

	activeToken, activeEmail := GetActiveCredentials()
	PrintAccounts(id, accounts, activeToken, activeEmail)
	return nil
}

// RunSwitch switches the active account for the given provider.
func RunSwitch(id provider.ID, email string) error {
	mgr := AccountManager(id)
	if mgr == nil {
		return fmt.Errorf("account switching not supported for %s", id)
	}

	acct, err := mgr.Switch(context.Background(), email)
	if err != nil {
		return err
	}
	fmt.Printf("Switched to %s\n", acct.Email)
	return nil
}

// AccountManager returns the AccountManager for a provider, or nil if unsupported.
func AccountManager(id provider.ID) provider.AccountManager {
	switch id {
	case provider.Claude:
		return &claudeprov.Accounts{}
	default:
		return nil
	}
}

// PrintAccounts renders a list of accounts with active-account highlighting.
func PrintAccounts(id provider.ID, accounts []provider.Account, activeToken, activeEmail string) {
	if id == provider.Claude {
		PrintClaudeAccounts(accounts, activeEmail)
		return
	}
	for _, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(no email stored)"
		}
		fmt.Printf("  %s\n", email)
	}
}

// PrintClaudeAccounts renders Claude accounts with plan, multiplier, and active status.
func PrintClaudeAccounts(accounts []provider.Account, activeEmail string) {
	for _, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(no email stored)"
		}
		plan := a.Label
		if plan == "" {
			plan = "unknown"
		}
		multiplier := ""
		if a.RateLimitTier != "" {
			if m := quota.ExtractMultiplier(a.RateLimitTier); m > 1 {
				multiplier = fmt.Sprintf(" %dx", m)
			}
		}

		labelStr := acctLabelStyle.Render(plan + multiplier)
		if activeEmail != "" && a.Email == activeEmail {
			fmt.Printf("  %s %s  %s\n",
				acctBoldStyle.Render(fmt.Sprintf("%-30s", email)),
				labelStr,
				acctActiveStyle.Render("(active)"),
			)
		} else {
			fmt.Printf("  %s %s\n",
				acctDimStyle.Render(fmt.Sprintf("%-30s", email)),
				labelStr,
			)
		}
	}
}

// GetActiveCredentials reads the active Claude access token and email from the
// credentials file. Returns empty strings on any error.
func GetActiveCredentials() (token, email string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", ""
	}
	var creds keyring.ClaudeCredentials
	if json.Unmarshal(data, &creds) != nil || creds.ClaudeAiOauth == nil {
		return "", ""
	}
	return creds.ClaudeAiOauth.AccessToken, creds.ClaudeAiOauth.Email
}
