package codex

import (
	"context"
	"encoding/json"
	"errors"
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
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string // from tokens.account_id
	UserID       string // from JWT chatgpt_user_id
	Email        string // from JWT id_token
	PlanType     string // from JWT id_token
	RecordKey    string // "{user_id}::{account_id}" — codex-auth compat
	FilePath     string // source file path
	IsActive     bool   // true if from ~/.codex/auth.json
	ExpiresAt    int64  // Unix ms derived from JWT exp claim; 0 = unknown
}

// codexAuthFile is the on-disk format shared with Codex CLI and codex-auth.
type codexAuthFile struct {
	AuthMode    string `json:"auth_mode"`
	Tokens      struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh,omitempty"`
	CQExpiresAt int64  `json:"cq_expires_at,omitempty"`
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
				// Already discovered from auth.json. Prefer the stored accounts/
				// copy so future reads keep using the canonical persisted tokens.
				if accounts[idx].IsActive {
					acct.IsActive = true
					acct.FilePath = path
					accounts[idx] = acct
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

	expiresAtMs := claims.ExpiresAt * 1000
	if af.CQExpiresAt > expiresAtMs {
		expiresAtMs = af.CQExpiresAt
	}

	return CodexAccount{
		AccessToken:  af.Tokens.AccessToken,
		RefreshToken: af.Tokens.RefreshToken,
		IDToken:      af.Tokens.IDToken,
		AccountID:    accountID,
		UserID:       claims.UserID,
		Email:        claims.Email,
		PlanType:     claims.PlanType,
		RecordKey:    claims.RecordKey(),
		FilePath:     path,
		ExpiresAt:    expiresAtMs,
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

func (a *Accounts) Remove(_ context.Context, identifier string) error {
	accts := DiscoverAccounts(a.FS)
	home, err := a.FS.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}

	authPath := filepath.Join(home, ".codex", "auth.json")
	recordKeys := make(map[string]bool)
	found := false
	for _, acct := range accts {
		if acct.Email != identifier {
			continue
		}
		found = true
		if acct.RecordKey != "" {
			recordKeys[acct.RecordKey] = true
		}
		if acct.FilePath != "" && acct.FilePath != authPath {
			if err := a.FS.Remove(acct.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove stored auth file: %w", err)
			}
		}
		if acct.IsActive {
			if err := a.FS.Remove(authPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove active auth file: %w", err)
			}
		}
	}
	if !found {
		return fmt.Errorf("no account found with email %q", identifier)
	}
	removeRegistryAccounts(a.FS, home, recordKeys)
	return nil
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

// PersistCodexAccount atomically rewrites the account's on-disk auth.json file
// (and ~/.codex/auth.json when IsActive) with updated tokens. The write follows
// the same tmp+rename pattern used throughout this package.
func PersistCodexAccount(fs fsutil.FileSystem, acct CodexAccount, home string) error {
	data, err := fs.ReadFile(acct.FilePath)
	if err != nil {
		return fmt.Errorf("read account file: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse account file: %w", err)
	}
	if doc == nil {
		doc = make(map[string]any)
	}

	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = make(map[string]any)
	}
	tokens["access_token"] = acct.AccessToken
	if acct.RefreshToken != "" {
		tokens["refresh_token"] = acct.RefreshToken
	}
	if acct.IDToken != "" {
		tokens["id_token"] = acct.IDToken
	}
	if acct.AccountID != "" {
		tokens["account_id"] = acct.AccountID
	}
	doc["tokens"] = tokens
	if acct.ExpiresAt > 0 {
		doc["cq_expires_at"] = acct.ExpiresAt
	} else {
		delete(doc, "cq_expires_at")
	}

	updated, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal account file: %w", err)
	}

	if err := atomicWrite(fs, acct.FilePath, updated); err != nil {
		return fmt.Errorf("write account file: %w", err)
	}

	// If active, also update ~/.codex/auth.json.
	if acct.IsActive {
		activeFile := filepath.Join(home, ".codex", "auth.json")
		if err := atomicWrite(fs, activeFile, updated); err != nil {
			return fmt.Errorf("write active auth file: %w", err)
		}
	}
	return nil
}

// atomicWrite writes data to path using a tmp+rename pattern.
func atomicWrite(fs fsutil.FileSystem, path string, data []byte) error {
	tmp := path + ".tmp"
	if err := fs.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := fs.Rename(tmp, path); err != nil {
		fs.Remove(tmp)
		return err
	}
	return nil
}

func removeRegistryAccounts(fs fsutil.FileSystem, home string, recordKeys map[string]bool) {
	if len(recordKeys) == 0 {
		return
	}
	regPath := filepath.Join(home, ".codex", "accounts", "registry.json")
	data, err := fs.ReadFile(regPath)
	if err != nil {
		return
	}

	var reg map[string]any
	if json.Unmarshal(data, &reg) != nil {
		return
	}
	if active, ok := reg["active_account_key"].(string); ok && recordKeys[active] {
		reg["active_account_key"] = ""
	}
	if rawAccounts, ok := reg["accounts"].([]any); ok {
		filtered := make([]any, 0, len(rawAccounts))
		for _, raw := range rawAccounts {
			acctMap, ok := raw.(map[string]any)
			if !ok {
				filtered = append(filtered, raw)
				continue
			}
			key, _ := acctMap["account_key"].(string)
			if recordKeys[key] {
				continue
			}
			filtered = append(filtered, raw)
		}
		reg["accounts"] = filtered
	}
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
