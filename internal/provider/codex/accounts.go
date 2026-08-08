package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
)

// CodexAccount holds parsed credentials from a Codex auth.json file.
type CodexAccount struct {
	AccountKey   AccountKey
	CandidateID  CandidateID
	Revision     Revision
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
	inventory := DiscoverInventory(fs)
	accounts := make([]CodexAccount, 0, len(inventory.Accounts))
	for _, logical := range inventory.Accounts {
		candidates := ResolveCandidate(logical, "", time.Now())
		if len(candidates) == 0 {
			continue
		}
		account := candidates[0].Credential
		account.IsActive = logical.Active
		accounts = append(accounts, account)
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
	FS    fsutil.FileSystem
	Admin CredentialAdmin
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

// Switch requires the credential coordinator for all system mutations.
func (a *Accounts) Switch(ctx context.Context, identifier string) (provider.Account, error) {
	if a.Admin == nil {
		return provider.Account{}, errors.New("Codex credential coordinator required")
	}
	return a.switchThroughCoordinator(ctx, identifier)
}

func (a *Accounts) switchThroughCoordinator(ctx context.Context, identifier string) (provider.Account, error) {
	logical, err := matchingLogicalAccount(DiscoverInventory(a.FS), identifier)
	if err != nil {
		return provider.Account{}, err
	}
	if logical.Active {
		return providerAccount(logical, true), nil
	}
	for _, candidate := range logical.Candidates {
		if candidate.Source != SourceManaged {
			continue
		}
		result, err := a.Admin.Activate(ctx, candidate.Ref, candidate.Revision)
		if err != nil {
			return provider.Account{}, err
		}
		if result.ProjectionError != nil {
			fmt.Fprintf(os.Stderr, "cq: Codex active projection: %v\n", result.ProjectionError)
		}
		return providerAccount(logical, true), nil
	}
	return provider.Account{}, errors.New("Codex account has no managed activation candidate")
}

// Remove requires the credential coordinator for all managed/system mutations.
func (a *Accounts) Remove(ctx context.Context, identifier string) error {
	if a.Admin == nil {
		return errors.New("Codex credential coordinator required")
	}
	return a.removeThroughCoordinator(ctx, identifier)
}

func (a *Accounts) removeThroughCoordinator(ctx context.Context, identifier string) error {
	logical, err := matchingLogicalAccount(DiscoverInventory(a.FS), identifier)
	if err != nil {
		return err
	}
	revisions := make(RevisionSet)
	for _, candidate := range logical.Candidates {
		if candidate.Source == SourceManaged {
			revisions[candidate.Ref.CandidateID] = candidate.Revision
		}
	}
	result, err := a.Admin.RemoveManaged(ctx, logical.Key, revisions, false)
	if err != nil {
		return err
	}
	if result.ProjectionError != nil {
		fmt.Fprintf(os.Stderr, "cq: Codex inactive projection: %v\n", result.ProjectionError)
	}
	if result.PendingRecovery {
		return errors.New("Codex account removal requires recovery")
	}
	return nil
}

func matchingLogicalAccount(inventory Inventory, identifier string) (LogicalAccount, error) {
	var matches []LogicalAccount
	for _, logical := range inventory.Accounts {
		if logical.Identity.Email == identifier {
			matches = append(matches, logical)
		}
	}
	if len(matches) == 0 {
		return LogicalAccount{}, fmt.Errorf("no account found with email %q", identifier)
	}
	if len(matches) != 1 {
		return LogicalAccount{}, fmt.Errorf("email %q matches multiple Codex accounts", identifier)
	}
	return matches[0], nil
}

func providerAccount(logical LogicalAccount, active bool) provider.Account {
	return provider.Account{
		AccountID: logical.Identity.AccountID, Email: logical.Identity.Email,
		Label: logical.Identity.PlanType, Active: active, SwitchID: logical.Identity.Email,
	}
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
