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
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
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
				// Keep the live system credential authoritative. A managed copy can
				// be stale after Codex or another account tool rotates system auth.
				_ = idx
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

// Switch is an explicit-only compatibility adapter around SystemActivator.
func (a *Accounts) Switch(ctx context.Context, identifier string) (provider.Account, error) {
	accts := DiscoverAccounts(a.FS)
	var matches []CodexAccount
	for _, acct := range accts {
		if acct.Email == identifier {
			matches = append(matches, acct)
		}
	}
	if len(matches) == 0 {
		return provider.Account{}, fmt.Errorf("no account found with email %q", identifier)
	}
	if len(matches) != 1 {
		return provider.Account{}, fmt.Errorf("email %q matches multiple Codex accounts", identifier)
	}
	acct := matches[0]
	ref, revision, err := candidateRefFromFS(a.FS, acct)
	if err != nil {
		return provider.Account{}, err
	}
	activator, err := NewFileSystemActivator(a.FS)
	if err != nil {
		return provider.Account{}, err
	}
	result, err := activator.Activate(ctx, ref, revision)
	if err != nil {
		return provider.Account{}, err
	}
	if result.ProjectionError != nil {
		fmt.Fprintf(os.Stderr, "cq: Codex active projection: %v\n", result.ProjectionError)
	}
	return provider.Account{
		AccountID: acct.AccountID, Email: acct.Email, Label: acct.PlanType,
		Active: true, SwitchID: acct.Email,
	}, nil
}

// Remove is an explicit-only compatibility adapter. Active system credentials
// are deactivated through SystemActivator before managed candidates are removed.
func (a *Accounts) Remove(ctx context.Context, identifier string) error {
	accts := DiscoverAccounts(a.FS)
	home, err := a.FS.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	var matches []CodexAccount
	for _, acct := range accts {
		if acct.Email == identifier {
			matches = append(matches, acct)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no account found with email %q", identifier)
	}
	if len(matches) != 1 {
		return fmt.Errorf("email %q matches multiple Codex accounts", identifier)
	}
	target := matches[0]
	authPath := filepath.Join(home, ".codex", "auth.json")
	recordKeys := make(map[string]bool)
	if target.RecordKey != "" {
		recordKeys[target.RecordKey] = true
	}
	if target.IsActive {
		activator, err := NewFileSystemActivator(a.FS)
		if err != nil {
			return err
		}
		active, err := activator.Active(ctx)
		if err != nil {
			return err
		}
		result, err := activator.Deactivate(ctx, active.AccountKey, active.Revision)
		if err != nil {
			return err
		}
		if result.ProjectionError != nil {
			fmt.Fprintf(os.Stderr, "cq: Codex inactive projection: %v\n", result.ProjectionError)
		}
	}
	if target.FilePath != "" && target.FilePath != authPath {
		if err := a.FS.Remove(target.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stored auth file: %w", err)
		}
	}
	// Discovery deliberately keeps a matching live system credential instead of
	// replacing it with a managed duplicate. Explicit removal still removes the
	// matching managed compatibility record by its validated record key.
	for recordKey := range recordKeys {
		path := filepath.Join(home, ".codex", "accounts", recordKey+".auth.json")
		if err := a.FS.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stored auth file: %w", err)
		}
	}
	if err := (Registry{FS: a.FS, Home: home}).RemoveAccounts(recordKeys); err != nil {
		return err
	}
	return nil
}

// PersistCodexAccount atomically rewrites one CQ-managed account file. Automatic
// code must never mirror a managed refresh into the shared system auth file.
func PersistCodexAccount(fs fsutil.FileSystem, acct CodexAccount, home string) error {
	systemAuthPath := filepath.Join(home, ".codex", "auth.json")
	if filepath.Clean(acct.FilePath) == filepath.Clean(systemAuthPath) {
		return errors.New("refusing to rewrite Codex system auth")
	}
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
