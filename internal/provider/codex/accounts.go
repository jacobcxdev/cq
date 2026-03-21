package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
)

// CodexAccount holds parsed credentials from a Codex auth.json file.
type CodexAccount struct {
	AccessToken string
	IDToken     string
	AccountID   string // from tokens.account_id
	UserID      string // from JWT chatgpt_user_id
	Email       string // from JWT id_token
	PlanType    string // from JWT id_token
	RecordKey   string // "{user_id}::{account_id}" — codex-auth compat
	FilePath    string // source file path
	IsActive    bool   // true if from ~/.codex/auth.json
}

// codexAuthFile is the on-disk format shared with Codex CLI and codex-auth.
type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// DiscoverAccounts finds all Codex accounts from:
// 1. ~/.codex/auth.json (active account)
// 2. ~/.codex/accounts/*.auth.json (additional accounts, codex-auth interop)
func DiscoverAccounts(fs fsutil.FileSystem) []CodexAccount {
	home, err := fs.UserHomeDir()
	if err != nil {
		return nil
	}

	var accounts []CodexAccount
	seen := make(map[string]int) // recordKey -> index in accounts

	// 1. Read active account from ~/.codex/auth.json
	activeFile := filepath.Join(home, ".codex", "auth.json")
	if acct, ok := parseAccountFile(fs, activeFile); ok {
		acct.IsActive = true
		if acct.RecordKey != "" {
			seen[acct.RecordKey] = len(accounts)
		}
		accounts = append(accounts, acct)
	}

	// 2. Read additional accounts from ~/.codex/accounts/*.auth.json
	accountsDir := filepath.Join(home, ".codex", "accounts")
	entries, err := fs.ReadDir(accountsDir)
	if err != nil {
		return accounts
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".auth.json") {
			continue
		}
		path := filepath.Join(accountsDir, name)
		acct, ok := parseAccountFile(fs, path)
		if !ok {
			continue
		}
		if acct.RecordKey != "" {
			if idx, exists := seen[acct.RecordKey]; exists {
				// Already discovered (from auth.json). Keep the active entry
				// but update its FilePath to the accounts/ copy for switch ops.
				if accounts[idx].IsActive {
					accounts[idx].FilePath = path
				}
				continue
			}
			seen[acct.RecordKey] = len(accounts)
		}
		accounts = append(accounts, acct)
	}

	return accounts
}

// parseAccountFile reads and parses a single Codex auth.json file.
func parseAccountFile(fs fsutil.FileSystem, path string) (CodexAccount, bool) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return CodexAccount{}, false
	}
	var af codexAuthFile
	if json.Unmarshal(data, &af) != nil {
		return CodexAccount{}, false
	}
	if af.Tokens.AccessToken == "" {
		return CodexAccount{}, false
	}

	claims := auth.DecodeCodexClaims(af.Tokens.IDToken)
	accountID := af.Tokens.AccountID
	if accountID == "" {
		accountID = claims.AccountID
	}

	return CodexAccount{
		AccessToken: af.Tokens.AccessToken,
		IDToken:     af.Tokens.IDToken,
		AccountID:   accountID,
		UserID:      claims.UserID,
		Email:       claims.Email,
		PlanType:    claims.PlanType,
		RecordKey:   claims.RecordKey(),
		FilePath:    path,
	}, true
}

// Accounts implements provider.AccountManager for Codex.
type Accounts struct {
	FS fsutil.FileSystem
}

func (a *Accounts) ProviderID() provider.ID { return provider.Codex }

// Discover returns all known Codex accounts.
func (a *Accounts) Discover(_ context.Context) ([]provider.Account, error) {
	accts := DiscoverAccounts(a.FS)
	out := make([]provider.Account, len(accts))
	for i, acct := range accts {
		out[i] = provider.Account{
			AccountID: acct.AccountID,
			Email:     acct.Email,
			Label:     acct.PlanType,
			Active:    acct.IsActive,
			SwitchID:  acct.Email,
		}
	}
	return out, nil
}

// Switch sets the active Codex account by copying the matching account's
// auth file to ~/.codex/auth.json and updating codex-auth's registry.
// Before overwriting, it adopts the current auth.json into ~/.codex/accounts/
// if it's not already stored there (preserves accounts created by codex login).
func (a *Accounts) Switch(_ context.Context, identifier string) (provider.Account, error) {
	accts := DiscoverAccounts(a.FS)

	home, err := a.FS.UserHomeDir()
	if err != nil {
		return provider.Account{}, fmt.Errorf("home dir: %w", err)
	}

	// Adopt the current active account into accounts/ before overwriting.
	adoptActiveAccount(a.FS, home, accts)

	for _, acct := range accts {
		if acct.Email != identifier {
			continue
		}
		if acct.FilePath == "" {
			return provider.Account{}, fmt.Errorf("no stored auth file for %q", identifier)
		}

		// Read the account file to copy it
		data, err := a.FS.ReadFile(acct.FilePath)
		if err != nil {
			return provider.Account{}, fmt.Errorf("read account file: %w", err)
		}

		// Atomic write to ~/.codex/auth.json
		dest := filepath.Join(home, ".codex", "auth.json")
		tmp := dest + ".tmp"
		if err := a.FS.WriteFile(tmp, data, 0o600); err != nil {
			return provider.Account{}, fmt.Errorf("write tmp: %w", err)
		}
		if err := a.FS.Rename(tmp, dest); err != nil {
			a.FS.Remove(tmp)
			return provider.Account{}, fmt.Errorf("rename: %w", err)
		}

		// Update codex-auth registry if it exists
		updateRegistryActiveKey(a.FS, home, acct.RecordKey)

		return provider.Account{
			AccountID: acct.AccountID,
			Email:     acct.Email,
			Label:     acct.PlanType,
			Active:    true,
			SwitchID:  acct.Email,
		}, nil
	}
	return provider.Account{}, fmt.Errorf("no account found with email %q", identifier)
}

// adoptActiveAccount saves the current ~/.codex/auth.json into
// ~/.codex/accounts/{record_key}.auth.json if it isn't already stored there.
// This preserves accounts originally created by `codex login` (Codex CLI)
// so they aren't lost when Switch overwrites auth.json.
func adoptActiveAccount(fs fsutil.FileSystem, home string, accts []CodexAccount) {
	authPath := filepath.Join(home, ".codex", "auth.json")
	accountsDir := filepath.Join(home, ".codex", "accounts")

	for _, acct := range accts {
		if !acct.IsActive {
			continue
		}
		if acct.RecordKey == "" {
			break // can't adopt without a record key
		}
		// If FilePath already points into accounts/, it's already stored.
		if acct.FilePath != authPath {
			break
		}
		// Active account only lives in auth.json — adopt it.
		data, err := fs.ReadFile(authPath)
		if err != nil {
			break
		}
		if err := fs.MkdirAll(accountsDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "cq: adopt account: mkdir: %v\n", err)
			break
		}
		dest := filepath.Join(accountsDir, acct.RecordKey+".auth.json")
		tmp := dest + ".tmp"
		if err := fs.WriteFile(tmp, data, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "cq: adopt account: write: %v\n", err)
			break
		}
		if err := fs.Rename(tmp, dest); err != nil {
			fs.Remove(tmp)
			fmt.Fprintf(os.Stderr, "cq: adopt account: rename: %v\n", err)
		}
		break
	}
}

// updateRegistryActiveKey updates active_account_key in codex-auth's registry.json.
// Best-effort: errors are logged to stderr and swallowed.
func updateRegistryActiveKey(fs fsutil.FileSystem, home, recordKey string) {
	if recordKey == "" {
		return
	}
	regPath := filepath.Join(home, ".codex", "accounts", "registry.json")
	data, err := fs.ReadFile(regPath)
	if err != nil {
		return // registry doesn't exist, nothing to update
	}

	var reg map[string]any
	if json.Unmarshal(data, &reg) != nil {
		return
	}

	reg["active_account_key"] = recordKey
	updated, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return
	}

	tmp := regPath + ".tmp"
	if err := fs.WriteFile(tmp, updated, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "cq: update registry: write: %v\n", err)
		return
	}
	if err := fs.Rename(tmp, regPath); err != nil {
		fs.Remove(tmp)
		fmt.Fprintf(os.Stderr, "cq: update registry: rename: %v\n", err)
	}
}
