package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
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

	// Always update credentials if the logged-in account is already active,
	// so that stale tokens in the credentials file are replaced by fresh ones.
	if !activate && acct.Email != "" {
		_, activeEmail := GetActiveCredentials()
		if activeEmail == acct.Email {
			activate = true
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

// RunCodexLogin performs the Codex OAuth PKCE login flow via Auth0.
// After login, it stores the account to ~/.codex/accounts/ for codex-auth interop.
func RunCodexLogin(ctx context.Context, client httputil.Doer, activate bool) error {
	fs := fsutil.OSFileSystem{}
	control, err := codexprov.OpenDefaultCredentialControl(ctx, fs)
	if err != nil {
		return err
	}
	defer control.Close()
	return runCodexLoginWithAdmin(ctx, client, activate, auth.CodexLogin, time.Now, os.Stdout, control)
}

type codexLoginFunc func(context.Context, httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error)

func runCodexLogin(ctx context.Context, client httputil.Doer, activate bool, fs fsutil.FileSystem, stateDir string, login codexLoginFunc, now func() time.Time, stdout io.Writer) error {
	durable, ok := fs.(fsutil.DurableFileSystem)
	if !ok {
		return errors.New("durable credential storage unavailable")
	}
	store, err := codexprov.NewManagedStore(durable)
	if err != nil {
		return err
	}
	coordinator, err := codexprov.NewCredentialCoordinator(store, stateDir)
	if err != nil {
		return err
	}
	return runCodexLoginWithAdmin(ctx, client, activate, login, now, stdout, coordinator)
}

func runCodexLoginWithAdmin(ctx context.Context, client httputil.Doer, activate bool, login codexLoginFunc, now func() time.Time, stdout io.Writer, admin codexprov.CredentialAdmin) error {
	tokens, claims, err := login(ctx, client)
	if err != nil {
		return err
	}
	if claims.AccountID == "" || claims.UserID == "" {
		return fmt.Errorf("login succeeded but JWT missing account or user ID")
	}
	ref, revision, err := admin.SaveLogin(ctx, codexprov.LoginCredential{
		Tokens: *tokens, Claims: *claims, CreatedAt: now().UTC(),
	})
	if err != nil {
		return err
	}
	if activate {
		result, err := admin.Activate(ctx, ref, revision)
		if err != nil {
			return err
		}
		if result.ProjectionError != nil {
			return fmt.Errorf("system auth activated; registry projection failed: %w", result.ProjectionError)
		}
	}
	if claims.Email != "" {
		fmt.Fprintf(stdout, "Logged in as %s", claims.Email)
		if claims.PlanType != "" {
			fmt.Fprintf(stdout, " (%s)", claims.PlanType)
		}
		fmt.Fprintln(stdout)
	} else {
		fmt.Fprintln(stdout, "Login successful.")
	}
	return nil
}

// RunAccounts lists discovered accounts for the given provider.
func RunAccounts(id provider.ID, jsonOutput bool) error {
	ctx := context.Background()
	var inventory codexprov.CredentialInventory
	var control *codexprov.CredentialControl
	if id == provider.Codex {
		var err error
		control, err = codexprov.OpenDefaultCredentialControl(ctx, fsutil.OSFileSystem{})
		if err != nil {
			return err
		}
		defer control.Close()
		inventory = control
	}
	mgr := accountListManager(id, nil, inventory)
	if mgr == nil {
		return fmt.Errorf("account management not supported for %s", id)
	}

	accounts, err := mgr.Discover(ctx)
	if err != nil {
		return err
	}
	if jsonOutput {
		if accounts == nil {
			accounts = []provider.Account{}
		}
		return json.NewEncoder(os.Stdout).Encode(accounts)
	}
	if len(accounts) == 0 {
		fmt.Printf("No %s accounts found.\n", id)
		return nil
	}

	activeToken, activeEmail := GetActiveCredentials()
	PrintAccounts(id, accounts, activeToken, activeEmail)
	return nil
}

func accountListManager(id provider.ID, client httputil.Doer, inventory codexprov.CredentialInventory) provider.AccountManager {
	if id == provider.Codex {
		return &codexprov.Accounts{Inventory: inventory}
	}
	return AccountManager(id, client)
}

// RunSwitch switches the active account for the given provider.
func RunSwitch(id provider.ID, email string, client httputil.Doer) error {
	ctx := context.Background()
	mgr := AccountManager(id, client)
	var control *codexprov.CredentialControl
	if id == provider.Codex {
		var err error
		control, err = codexprov.OpenDefaultCredentialControl(ctx, fsutil.OSFileSystem{})
		if err != nil {
			return err
		}
		defer control.Close()
		mgr = &codexprov.Accounts{FS: fsutil.OSFileSystem{}, Admin: control}
	}
	if mgr == nil {
		return fmt.Errorf("account switching not supported for %s", id)
	}

	acct, err := mgr.Switch(ctx, email)
	if err != nil {
		return err
	}
	fmt.Printf("Switched to %s\n", acct.Email)
	return nil
}

// RunRemove removes the matching account for the given provider.
func RunRemove(id provider.ID, email string, client httputil.Doer) error {
	ctx := context.Background()
	mgr := AccountManager(id, client)
	var control *codexprov.CredentialControl
	if id == provider.Codex {
		var err error
		control, err = codexprov.OpenDefaultCredentialControl(ctx, fsutil.OSFileSystem{})
		if err != nil {
			return err
		}
		defer control.Close()
		mgr = &codexprov.Accounts{FS: fsutil.OSFileSystem{}, Admin: control}
	}
	if mgr == nil {
		return fmt.Errorf("account removal not supported for %s", id)
	}
	if err := mgr.Remove(ctx, email); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", email)
	return nil
}

// AccountManager returns the AccountManager for a provider, or nil if unsupported.
// The client is used for providers that refresh metadata on switch (e.g. Claude).
func AccountManager(id provider.ID, client httputil.Doer) provider.AccountManager {
	switch id {
	case provider.Claude:
		return &claudeprov.Accounts{HTTP: client}
	case provider.Codex:
		return &codexprov.Accounts{FS: fsutil.OSFileSystem{}}
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
	if id == provider.Codex {
		PrintCodexAccounts(accounts)
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

// PrintCodexAccounts renders Codex accounts with plan and active status.
func PrintCodexAccounts(accounts []provider.Account) {
	for _, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(no email stored)"
		}
		plan := a.Label
		if plan == "" {
			plan = "unknown"
		}

		labelStr := acctLabelStyle.Render(plan)
		if a.Active {
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
