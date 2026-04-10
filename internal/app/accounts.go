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
	"github.com/jacobcxdev/cq/internal/fsutil"
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
	tokens, claims, err := auth.CodexLogin(ctx, client)
	if err != nil {
		return err
	}

	if claims.AccountID == "" || claims.UserID == "" {
		return fmt.Errorf("login succeeded but JWT missing account or user ID")
	}

	// Build the standard auth.json format
	authFile := map[string]any{
		"auth_mode":    "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      tokens.IDToken,
			"access_token":  tokens.AccessToken,
			"refresh_token": tokens.RefreshToken,
			"account_id":    claims.AccountID,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(authFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}

	// Ensure accounts directory exists
	accountsDir := filepath.Join(home, ".codex", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		return fmt.Errorf("create accounts dir: %w", err)
	}

	// Write to ~/.codex/accounts/{record_key}.auth.json
	recordKey := claims.RecordKey()
	accountPath := filepath.Join(accountsDir, recordKey+".auth.json")
	tmp := accountPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write account file: %w", err)
	}
	if err := os.Rename(tmp, accountPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename account file: %w", err)
	}

	// Update codex-auth registry for interop
	updateCodexRegistry(home, recordKey, claims)

	if activate {
		dest := filepath.Join(home, ".codex", "auth.json")
		tmp := dest + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return fmt.Errorf("write active auth: %w", err)
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("rename active auth: %w", err)
		}
	}

	if claims.Email != "" {
		fmt.Printf("Logged in as %s", claims.Email)
		if claims.PlanType != "" {
			fmt.Printf(" (%s)", claims.PlanType)
		}
		fmt.Println()
	} else {
		fmt.Println("Login successful.")
	}
	return nil
}

// updateCodexRegistry upserts an account record in codex-auth's registry.json.
// Best-effort: errors are logged to stderr and swallowed.
func updateCodexRegistry(home, recordKey string, claims *auth.CodexClaims) {
	regPath := filepath.Join(home, ".codex", "accounts", "registry.json")
	var reg map[string]any

	data, err := os.ReadFile(regPath)
	if err != nil {
		// No existing registry — create one
		reg = map[string]any{
			"schema_version": 3,
		}
	} else if json.Unmarshal(data, &reg) != nil {
		return
	}

	// Build account record
	record := map[string]any{
		"account_key":         recordKey,
		"chatgpt_account_id":  claims.AccountID,
		"chatgpt_user_id":     claims.UserID,
		"email":               claims.Email,
		"alias":               "",
		"plan":                claims.PlanType,
		"auth_mode":           "chatgpt",
		"created_at":          time.Now().Unix(),
	}

	// Upsert in accounts array
	accounts, _ := reg["accounts"].([]any)
	found := false
	for i, a := range accounts {
		if m, ok := a.(map[string]any); ok {
			if m["account_key"] == recordKey {
				accounts[i] = record
				found = true
				break
			}
		}
	}
	if !found {
		accounts = append(accounts, record)
	}
	reg["accounts"] = accounts
	reg["active_account_key"] = recordKey

	updated, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return
	}
	tmp := regPath + ".tmp"
	if err := os.WriteFile(tmp, updated, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "cq: update codex registry: %v\n", err)
		return
	}
	if err := os.Rename(tmp, regPath); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "cq: update codex registry: %v\n", err)
	}
}

// RunAccounts lists discovered accounts for the given provider.
func RunAccounts(id provider.ID) error {
	mgr := AccountManager(id, nil)
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
func RunSwitch(id provider.ID, email string, client httputil.Doer) error {
	mgr := AccountManager(id, client)
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

// RunRemove removes the matching account for the given provider.
func RunRemove(id provider.ID, email string, client httputil.Doer) error {
	mgr := AccountManager(id, client)
	if mgr == nil {
		return fmt.Errorf("account removal not supported for %s", id)
	}
	if err := mgr.Remove(context.Background(), email); err != nil {
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
