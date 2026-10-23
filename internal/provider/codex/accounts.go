package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	ExpiresAt    int64  // Unix ms derived from access-token exp or CQ metadata; 0 = unknown
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

func accountAccessExpiresAt(af codexAuthFile) int64 {
	expiresAt := auth.DecodeCodexClaims(af.Tokens.AccessToken).ExpiresAt * 1000
	if expiresAt > 0 {
		return expiresAt
	}
	if af.CQExpiresAt > 0 {
		return af.CQExpiresAt
	}
	return 0
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
		ExpiresAt:    accountAccessExpiresAt(af),
	}, true
}

// Accounts implements provider.AccountManager for Codex.
type Accounts struct {
	FS              fsutil.FileSystem
	Admin           CredentialAdmin
	Inventory       CredentialInventory
	Now             func() time.Time
	ExternalSources []ExternalCredentialSource
	StateDir        string // Resolved CQ state authority for read-only inspection.
}

func (a *Accounts) ProviderID() provider.ID { return provider.Codex }

// Discover returns all known Codex accounts.
func (a *Accounts) Discover(ctx context.Context) ([]provider.Account, error) {
	if a.Inventory != nil {
		inventory, err := a.Inventory.List(ctx)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if a.Now != nil {
			now = a.Now()
		}
		return projectVisibleProviderAccounts(ProjectVisibleAccounts(inventory, now)), nil
	}
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

func projectVisibleProviderAccounts(accounts []ResetAccount) []provider.Account {
	out := make([]provider.Account, len(accounts))
	for i, account := range accounts {
		out[i] = provider.Account{
			AccountID: account.AccountID,
			Email:     account.Email,
			Label:     account.PlanType,
			Active:    account.Active,
			SwitchID:  account.Email,
		}
	}
	return out
}

// Switch requires the credential coordinator for all system mutations.
func (a *Accounts) Switch(ctx context.Context, identifier string) (provider.Account, error) {
	if a.Admin == nil {
		return provider.Account{}, errors.New("Codex credential coordinator required")
	}
	return a.switchThroughCoordinator(ctx, identifier)
}

func (a *Accounts) switchThroughCoordinator(ctx context.Context, identifier string) (provider.Account, error) {
	inventory := DiscoverInventory(a.FS)
	if a.Inventory != nil {
		var err error
		inventory, err = a.Inventory.List(ctx)
		if err != nil {
			return provider.Account{}, err
		}
	}
	logical, err := matchingLogicalAccount(inventory, identifier)
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

// Inspect preserves the complete read-only identity/source inventory. It never
// opens a coordinator, recovers pending work, adopts credentials, or bootstraps
// storage. Callers supply the resolved StateDir unless Inventory is injected.
// Nil ExternalSources selects the declared default CodexBar source.
func (a *Accounts) Inspect(ctx context.Context) (Inventory, error) {
	if err := ctx.Err(); err != nil {
		return Inventory{}, err
	}
	var inventory Inventory
	var err error
	if a.Inventory != nil {
		inventory, err = a.Inventory.List(ctx)
	} else {
		if err := a.inspectRemovalJournal(ctx); err != nil {
			return Inventory{}, err
		}
		home, homeErr := a.FS.UserHomeDir()
		if homeErr != nil {
			return Inventory{}, homeErr
		}
		sources := a.ExternalSources
		if sources == nil {
			sources = []ExternalCredentialSource{NewCodexBarSource(DefaultCodexBarRoot(home))}
		}
		inventory, err = discoverAuthoritativeInventoryWithSources(ctx, a.FS, sources...)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Inventory{}, ctxErr
	}
	if err != nil {
		return Inventory{}, ErrCredentialAuthorityUnavailable
	}
	for _, source := range inventory.ExternalSources {
		if source.ErrorCode != "" && !source.OptionalAbsent {
			return Inventory{}, ErrCredentialAuthorityUnavailable
		}
	}
	for _, account := range inventory.Accounts {
		for _, candidate := range account.Candidates {
			if candidate.DispatchBlocked {
				return Inventory{}, ErrCredentialAuthorityUnavailable
			}
		}
	}
	// Sanitisation must not mutate an injected authority's retained snapshot.
	inventory.Accounts = append([]LogicalAccount(nil), inventory.Accounts...)
	for i := range inventory.Accounts {
		inventory.Accounts[i].Candidates = append([]CredentialCandidate(nil), inventory.Accounts[i].Candidates...)
	}
	return sanitiseCredentialInventory(inventory), nil
}

// InspectAliases reads metadata only; it creates no registry or directories.
func (a *Accounts) InspectAliases(ctx context.Context) (AccountAliasIndex, error) {
	if err := ctx.Err(); err != nil {
		return AccountAliasIndex{}, err
	}
	home, err := a.FS.UserHomeDir()
	if err != nil {
		return AccountAliasIndex{}, err
	}
	aliases, err := (Registry{FS: a.FS, Home: home}).AccountAliasIndex()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return AccountAliasIndex{}, ctxErr
	}
	return aliases, err
}

// inspectRemovalJournal observes pending removal without opening a credential
// owner or recovering anything. Inspection remains a non-transactional snapshot.
func (a *Accounts) inspectRemovalJournal(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.StateDir == "" {
		return ErrCredentialAuthorityUnavailable
	}
	fs, ok := a.FS.(fsutil.DurableFileSystem)
	if !ok {
		return ErrCredentialAuthorityUnavailable
	}
	_, pending, err := (RemovalJournal{FS: fs, StateDir: a.StateDir}).Load()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil || pending {
		return ErrCredentialAuthorityUnavailable
	}
	return nil
}

// InspectRemoval preserves general inspection's read-only contract while making
// an interrupted removal selectable by its exact persisted opaque key.
func (a *Accounts) InspectRemoval(ctx context.Context) (Inventory, *RemovalPlan, error) {
	if err := ctx.Err(); err != nil {
		return Inventory{}, nil, err
	}
	fs, ok := a.FS.(fsutil.DurableFileSystem)
	if !ok || a.StateDir == "" {
		return Inventory{}, nil, ErrCredentialAuthorityUnavailable
	}
	plan, pending, err := (RemovalJournal{FS: fs, StateDir: a.StateDir}).Load()
	if err != nil {
		return Inventory{}, nil, err
	}
	if !pending {
		inventory, err := a.Inspect(ctx)
		return inventory, nil, err
	}
	home, err := a.FS.UserHomeDir()
	if err != nil {
		return Inventory{}, nil, err
	}
	sources := a.ExternalSources
	if sources == nil {
		sources = []ExternalCredentialSource{NewCodexBarSource(DefaultCodexBarRoot(home))}
	}
	inventory, err := discoverAuthoritativeInventoryWithSources(ctx, a.FS, sources...)
	if err != nil {
		return Inventory{}, nil, err
	}
	found := false
	for _, row := range inventory.Accounts {
		found = found || row.Key == plan.AccountKey
	}
	if !found {
		inventory.Accounts = append(inventory.Accounts, LogicalAccount{Key: plan.AccountKey})
	}
	if err := ctx.Err(); err != nil {
		return Inventory{}, nil, err
	}
	return sanitiseCredentialInventory(inventory), &plan, nil
}
