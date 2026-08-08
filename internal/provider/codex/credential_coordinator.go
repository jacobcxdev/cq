package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

type RevisionSet map[CandidateID]Revision

type CredentialInventory interface {
	List(context.Context) (Inventory, error)
}

type SecretResolver interface {
	Resolve(context.Context, CandidateRef) (CredentialMaterial, error)
}

type CredentialAdmin interface {
	SaveLogin(context.Context, LoginCredential) (CandidateRef, Revision, error)
	Adopt(context.Context, SystemSnapshot) (CandidateRef, Revision, error)
	Activate(context.Context, CandidateRef, Revision) (ActivationResult, error)
	RemoveManaged(context.Context, AccountKey, RevisionSet, bool) (RemovalResult, error)
}

type CredentialRefreshBroker interface {
	Refresh(context.Context, CandidateRef, Revision) (RefreshResult, error)
}

type CredentialCoordinator struct {
	Store           *ManagedStore
	Activator       *FileSystemActivator
	Registry        Registry
	Journal         RemovalJournal
	RefreshExchange RefreshExchange
	Now             func() time.Time
	mu              sync.Mutex
	refreshMu       sync.Mutex
	refreshFlights  map[string]*refreshFlight
	refreshRetained map[CandidateID]retainedRefresh
}

func NewCredentialCoordinator(store *ManagedStore) (*CredentialCoordinator, error) {
	if store == nil || store.FS == nil {
		return nil, errors.New("managed store unavailable")
	}
	activator, err := NewFileSystemActivator(store.FS)
	if err != nil {
		return nil, err
	}
	coordinator := &CredentialCoordinator{
		Store: store, Activator: activator,
		Registry: Registry{FS: store.FS, Home: store.Home},
		Now:      time.Now,
	}
	activator.Replace = store.durableReplace
	activator.Remove = func(path string) error {
		if err := store.FS.Remove(path); err != nil {
			return err
		}
		return store.FS.SyncDir(filepath.Dir(path))
	}
	coordinator.Journal = RemovalJournal{FS: store.FS, Home: store.Home, Store: store}
	return coordinator, nil
}

func (c *CredentialCoordinator) List(context.Context) (Inventory, error) {
	return sanitiseCredentialInventory(DiscoverInventory(c.Store.FS)), nil
}

func sanitiseCredentialInventory(inventory Inventory) Inventory {
	for i := range inventory.Accounts {
		for j := range inventory.Accounts[i].Candidates {
			inventory.Accounts[i].Candidates[j].Credential = CodexAccount{}
		}
	}
	return inventory
}

func (c *CredentialCoordinator) SaveLogin(ctx context.Context, credential LoginCredential) (CandidateRef, Revision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.finishPendingRemovalLocked(ctx); err != nil {
		return CandidateRef{}, "", err
	}
	if credential.CreatedAt.IsZero() {
		credential.CreatedAt = time.Now().UTC()
	}
	var accountKey AccountKey
	records, err := c.managedRecords()
	if err != nil {
		return CandidateRef{}, "", err
	}
	for _, existing := range records {
		claims := auth.DecodeCodexClaims(existing.Credential.IDToken)
		if existing.Credential.AccountID != credential.Claims.AccountID || claims.UserID != credential.Claims.UserID {
			continue
		}
		accountKey = existing.Metadata.AccountKey
		if existing.Metadata.Provenance != ProvenanceCQOAuth || existing.Metadata.Version != 1 {
			continue
		}
		expected := existing.Metadata.Revision
		existing.Credential = CredentialMaterial{
			AccessToken: credential.Tokens.AccessToken, RefreshToken: credential.Tokens.RefreshToken,
			IDToken: credential.Tokens.IDToken, AccountID: credential.Claims.AccountID,
		}
		existing.Document["last_refresh"] = credential.CreatedAt.UTC().Format(time.RFC3339Nano)
		lineageID, err := c.Store.randomID("lineage")
		if err != nil {
			return CandidateRef{}, "", err
		}
		existing.Metadata.LineageID = LineageID(lineageID)
		existing.Metadata.RefreshOwnership = RefreshCQOwnedNeverExported
		existing.Metadata.OperationState = OperationReady
		if err := c.Store.Commit(&existing, expected); err != nil {
			return CandidateRef{}, "", err
		}
		if err := c.upsertLoginRegistry(existing.Metadata.AccountKey, credential); err != nil {
			return CandidateRef{}, "", err
		}
		return recordRef(existing), existing.Metadata.Revision, nil
	}
	record, err := c.Store.saveNewForAccount(credential, accountKey, ProvenanceCQOAuth, RefreshCQOwnedNeverExported)
	if err != nil {
		return CandidateRef{}, "", err
	}
	if err := c.upsertLoginRegistry(record.Metadata.AccountKey, credential); err != nil {
		return CandidateRef{}, "", err
	}
	return recordRef(record), record.Metadata.Revision, nil
}

func (c *CredentialCoordinator) upsertLoginRegistry(accountKey AccountKey, credential LoginCredential) error {
	return c.Registry.UpsertAccount(RegistryAccount{
		AccountKey: string(accountKey), AccountID: credential.Claims.AccountID,
		UserID: credential.Claims.UserID, Email: credential.Claims.Email,
		Plan: credential.Claims.PlanType, AuthMode: "chatgpt", CreatedAt: credential.CreatedAt.Unix(),
	})
}

func (c *CredentialCoordinator) Resolve(_ context.Context, ref CandidateRef) (CredentialMaterial, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, logical := range DiscoverInventory(c.Store.FS).Accounts {
		for _, candidate := range logical.Candidates {
			if candidate.Ref.AccountKey != ref.AccountKey || candidate.Ref.CandidateID != ref.CandidateID {
				continue
			}
			if candidate.DispatchBlocked {
				return CredentialMaterial{}, errors.New("credential candidate dispatch blocked")
			}
			if candidate.Source == SourceSystem {
				return CredentialMaterial{
					AccessToken:  candidate.Credential.AccessToken,
					RefreshToken: candidate.Credential.RefreshToken,
					IDToken:      candidate.Credential.IDToken,
					AccountID:    candidate.Credential.AccountID,
				}, nil
			}
			break
		}
	}
	record, err := c.loadRef(ref)
	if err != nil {
		return CredentialMaterial{}, err
	}
	return record.Credential, nil
}

func (c *CredentialCoordinator) Adopt(ctx context.Context, snapshot SystemSnapshot) (CandidateRef, Revision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.finishPendingRemovalLocked(ctx); err != nil {
		return CandidateRef{}, "", err
	}
	return c.adoptLocked(snapshot)
}

func (c *CredentialCoordinator) adoptLocked(snapshot SystemSnapshot) (CandidateRef, Revision, error) {
	active, err := c.Activator.Active(context.Background())
	if err != nil {
		return CandidateRef{}, "", err
	}
	if !active.Present || active.Revision != snapshot.Revision {
		return CandidateRef{}, "", errors.New("system snapshot changed before adoption")
	}
	data, err := c.Store.FS.ReadFile(filepath.Join(c.Store.Home, ".codex", "auth.json"))
	if err != nil {
		return CandidateRef{}, "", err
	}
	account, ok := parseAccountData(data, "")
	if !ok || account.RecordKey == "" {
		return CandidateRef{}, "", errors.New("system credential lacks stable identity")
	}
	accountClaims := auth.DecodeCodexClaims(account.IDToken)
	records, err := c.managedRecords()
	if err != nil {
		return CandidateRef{}, "", err
	}
	for _, record := range records {
		recordClaims := auth.DecodeCodexClaims(record.Credential.IDToken)
		if record.Credential.AccountID != account.AccountID || recordClaims.UserID != accountClaims.UserID {
			continue
		}
		if record.Metadata.Provenance == ProvenanceSystemBorrowed {
			expected := record.Metadata.Revision
			record.Credential = CredentialMaterial{AccessToken: account.AccessToken, RefreshToken: account.RefreshToken, IDToken: account.IDToken, AccountID: account.AccountID}
			if err := c.Store.Commit(&record, expected); err != nil {
				return CandidateRef{}, "", err
			}
			return recordRef(record), record.Metadata.Revision, nil
		}
		// Never overwrite CQ-owned or legacy material.
		return recordRef(record), record.Metadata.Revision, nil
	}
	claims := auth.DecodeCodexClaims(account.IDToken)
	record, err := c.Store.saveNew(LoginCredential{
		Tokens: auth.CodexTokenResponse{AccessToken: account.AccessToken, RefreshToken: account.RefreshToken, IDToken: account.IDToken},
		Claims: claims, CreatedAt: time.Now().UTC(),
	}, ProvenanceSystemBorrowed, RefreshExportedToSystem)
	if err != nil {
		return CandidateRef{}, "", err
	}
	if err := c.Registry.UpsertAccount(RegistryAccount{
		AccountKey: string(record.Metadata.AccountKey), AccountID: claims.AccountID,
		UserID: claims.UserID, Email: claims.Email, Plan: claims.PlanType,
		AuthMode: "chatgpt", CreatedAt: time.Now().Unix(),
	}); err != nil {
		return CandidateRef{}, "", err
	}
	return recordRef(record), record.Metadata.Revision, nil
}

func (c *CredentialCoordinator) Activate(ctx context.Context, ref CandidateRef, revision Revision) (ActivationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.finishPendingRemovalLocked(ctx); err != nil {
		return ActivationResult{}, err
	}
	if active, err := c.Activator.Active(ctx); err != nil {
		return ActivationResult{}, err
	} else if active.Present {
		if _, _, err := c.adoptLocked(active); err != nil {
			return ActivationResult{}, err
		}
	}
	record, err := c.loadRef(ref)
	if err != nil {
		return ActivationResult{}, err
	}
	if record.Metadata.Revision != revision {
		return ActivationResult{}, ErrStaleRevision
	}
	originalRevision := record.Metadata.Revision
	if record.Metadata.Version == 1 {
		record.Metadata.OperationState = OperationActivationPending
		record.Metadata.RefreshOwnership = RefreshExportedToSystem
		if err := c.Store.Commit(&record, originalRevision); err != nil {
			return ActivationResult{}, err
		}
		revision = record.Metadata.Revision
	}
	account, ok := parseAccountDataFromRecord(record)
	if !ok {
		return ActivationResult{}, errors.New("activation record identity invalid")
	}
	activationRef := CandidateRef{AccountKey: AccountKey(account.RecordKey), CandidateID: ref.CandidateID, path: record.Path}
	candidateRevision, err := credentialRevisionForPath(c.Store.FS, record.Path)
	if err != nil {
		return ActivationResult{}, err
	}
	result, err := c.Activator.Activate(ctx, activationRef, candidateRevision)
	if err != nil {
		return ActivationResult{}, err
	}
	if record.Metadata.Version == 1 {
		record.Metadata.OperationState = OperationReady
		record.Metadata.RefreshOwnership = RefreshExportedToSystem
		if err := c.Store.Commit(&record, revision); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (c *CredentialCoordinator) RemoveManaged(ctx context.Context, accountKey AccountKey, revisions RevisionSet, force bool) (RemovalResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pending, ok, err := c.Journal.Load(); err != nil {
		return RemovalResult{}, err
	} else if ok {
		recovered, err := c.resumeRemoval(ctx, pending)
		if err != nil || pending.AccountKey == accountKey {
			return recovered, err
		}
	}
	operationID, err := c.Store.randomID("remove")
	if err != nil {
		return RemovalResult{}, err
	}
	plan := RemovalPlan{Version: 1, OperationID: operationID, AccountKey: accountKey, Force: force}
	ids := make([]string, 0, len(revisions))
	for candidateID := range revisions {
		ids = append(ids, string(candidateID))
	}
	sort.Strings(ids)
	for _, id := range ids {
		candidateID := CandidateID(id)
		revision := revisions[candidateID]
		plan.Candidates = append(plan.Candidates, RemovalCandidate{CandidateID: candidateID, Revision: revision})
	}
	targetActive := false
	targetFound := false
	expectedRevisions := make(RevisionSet)
	registryKeys := map[string]bool{string(accountKey): true}
	for _, logical := range DiscoverInventory(c.Store.FS).Accounts {
		if logical.Key == accountKey {
			targetFound = true
			targetActive = logical.Active
			for _, candidate := range logical.Candidates {
				if candidate.Source == SourceManaged {
					expectedRevisions[candidate.Ref.CandidateID] = candidate.Revision
				}
			}
			if logical.Identity.RecordKey != "" {
				registryKeys[logical.Identity.RecordKey] = true
			}
			break
		}
	}
	if !targetFound {
		return RemovalResult{}, errors.New("Codex account no longer exists")
	}
	if len(expectedRevisions) != len(revisions) {
		return RemovalResult{}, ErrStaleRevision
	}
	for candidateID, expected := range expectedRevisions {
		if revisions[candidateID] != expected {
			return RemovalResult{}, ErrStaleRevision
		}
	}
	for key := range registryKeys {
		plan.RegistryKeys = append(plan.RegistryKeys, key)
	}
	sort.Strings(plan.RegistryKeys)
	if targetActive {
		active, err := c.Activator.Active(ctx)
		if err != nil {
			return RemovalResult{}, err
		}
		if !active.Present {
			return RemovalResult{}, ErrStaleRevision
		}
		plan.ExpectedSystemRevision = active.Revision
	}
	if err := c.Journal.Save(plan); err != nil {
		return RemovalResult{}, err
	}
	return c.resumeRemoval(ctx, plan)
}

func (c *CredentialCoordinator) finishPendingRemovalLocked(ctx context.Context) error {
	plan, ok, err := c.Journal.Load()
	if err != nil || !ok {
		return err
	}
	_, err = c.resumeRemoval(ctx, plan)
	return err
}

func (c *CredentialCoordinator) RecoverRemoval(ctx context.Context) (RemovalResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	plan, ok, err := c.Journal.Load()
	if err != nil || !ok {
		return RemovalResult{}, err
	}
	return c.resumeRemoval(ctx, plan)
}

func (c *CredentialCoordinator) resumeRemoval(ctx context.Context, plan RemovalPlan) (RemovalResult, error) {
	result := RemovalResult{PendingRecovery: true}
	for _, candidate := range plan.Candidates {
		path := c.candidatePath(candidate.CandidateID)
		record, err := c.Store.Load(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, err
		}
		if record.Metadata.Version == 0 {
			currentRevision, err := credentialRevisionForPath(c.Store.FS, path)
			if err != nil {
				return result, err
			}
			if currentRevision != candidate.Revision {
				return result, ErrStaleRevision
			}
		} else if record.Metadata.AccountKey != plan.AccountKey || record.Metadata.Revision != candidate.Revision {
			return result, ErrStaleRevision
		}
		if err := c.Store.FS.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		if err := c.Store.FS.SyncDir(filepath.Dir(path)); err != nil {
			return result, err
		}
		result.ManagedDeleted++
	}
	if plan.ExpectedSystemRevision != "" {
		active, err := c.Activator.Active(ctx)
		if err != nil {
			return result, err
		}
		if active.Present && active.Revision != plan.ExpectedSystemRevision {
			return result, ErrStaleRevision
		}
		if active.Present {
			deactivated, err := c.Activator.Deactivate(ctx, active.AccountKey, active.Revision)
			if err != nil {
				return result, err
			}
			result.SystemDeactivated = deactivated.SystemRemoved
			result.ProjectionError = deactivated.ProjectionError
		} else {
			result.SystemDeactivated = true
		}
	}
	registryKeys := make(map[string]bool)
	for _, key := range plan.RegistryKeys {
		registryKeys[key] = true
	}
	if len(registryKeys) == 0 {
		registryKeys[string(plan.AccountKey)] = true
	}
	if err := c.Registry.RemoveAccounts(registryKeys); err != nil {
		return result, err
	}
	if err := c.Journal.Clear(); err != nil {
		return result, err
	}
	result.PendingRecovery = false
	return result, nil
}

func (c *CredentialCoordinator) loadRef(ref CandidateRef) (ManagedRecord, error) {
	path := ref.path
	if path == "" {
		path = c.candidatePath(ref.CandidateID)
	}
	record, err := c.Store.Load(path)
	if err != nil {
		return ManagedRecord{}, err
	}
	if record.Metadata.Version == 0 {
		record.Metadata.AccountKey = ref.AccountKey
		record.Metadata.CandidateID = ref.CandidateID
		record.Metadata.Revision, err = credentialRevisionForPath(c.Store.FS, path)
		if err != nil {
			return ManagedRecord{}, err
		}
		return record, nil
	}
	if record.Metadata.AccountKey != ref.AccountKey || record.Metadata.CandidateID != ref.CandidateID {
		return ManagedRecord{}, errors.New("credential reference mismatch")
	}
	return record, nil
}

func (c *CredentialCoordinator) candidatePath(candidateID CandidateID) string {
	for _, logical := range DiscoverInventory(c.Store.FS).Accounts {
		for _, candidate := range logical.Candidates {
			if candidate.Ref.CandidateID == candidateID && candidate.Source == SourceManaged {
				return candidate.Credential.FilePath
			}
		}
	}
	return filepath.Join(c.Store.Home, ".codex", "accounts", string(candidateID)+".auth.json")
}

func (c *CredentialCoordinator) managedRecords() ([]ManagedRecord, error) {
	dir := filepath.Join(c.Store.Home, ".codex", "accounts")
	entries, err := c.Store.FS.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []ManagedRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".auth.json") {
			continue
		}
		record, err := c.Store.Load(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}

func recordRef(record ManagedRecord) CandidateRef {
	return CandidateRef{AccountKey: record.Metadata.AccountKey, CandidateID: record.Metadata.CandidateID, path: record.Path}
}

func parseAccountDataFromRecord(record ManagedRecord) (CodexAccount, bool) {
	data, err := json.Marshal(record.Document)
	if err != nil {
		return CodexAccount{}, false
	}
	return parseAccountData(data, record.Path)
}

func credentialRevisionForPath(fs interface{ ReadFile(string) ([]byte, error) }, path string) (Revision, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return "", err
	}
	return credentialRevision(data), nil
}
