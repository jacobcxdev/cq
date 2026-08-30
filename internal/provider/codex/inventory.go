package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type AccountKey string
type CandidateID string
type LineageID string
type Revision string

type SelectionExclusion struct {
	AccountKey  AccountKey
	CandidateID CandidateID
}

type CredentialSource uint8

const (
	SourceSystem CredentialSource = iota + 1
	SourceManaged
	SourceExternal
)

type AccountIdentity struct {
	AccountID string
	UserID    string
	Email     string
	PlanType  string
	RecordKey string
}

type CredentialCandidate struct {
	Ref             CandidateRef
	Revision        Revision
	Source          CredentialSource
	Credential      CodexAccount
	AccessExpiresAt time.Time
	CQAuthored      bool
	RefreshEligible bool
	Routable        bool
	DispatchBlocked bool
	externalRef     *ExternalCandidateRef
}

type LogicalAccount struct {
	Key        AccountKey
	Identity   AccountIdentity
	Candidates []CredentialCandidate
	Active     bool
	Routable   bool
	Unstable   bool
}

type InventoryIntentKind string

const (
	IntentAssociate InventoryIntentKind = "associate"
	IntentAdopt     InventoryIntentKind = "adopt"
)

type InventoryIntent struct {
	Kind       InventoryIntentKind
	AccountKey AccountKey
	Candidates []CandidateID
}

type Inventory struct {
	Accounts        []LogicalAccount
	Intents         []InventoryIntent
	ExternalSources []ExternalSourceStatus
}

type ExternalSourceStatus struct {
	Name            string
	CandidateCount  int
	ErrorCode       string
	OptionalAbsent  bool
	TopologyInvalid bool
}

type rawCandidate struct {
	account    CodexAccount
	data       []byte
	source     CredentialSource
	sourceName string
	cqAuthored bool
	metadata   *ManagedMetadata
}

type sourcedExternalCandidate struct {
	sourceName string
	candidate  ExternalCandidate
}

const maximumCoreCredentialSize = int64(1 << 20)

// DiscoverInventory builds one generation-local read model without writing.
func DiscoverInventory(fs fsutil.FileSystem) Inventory {
	return DiscoverInventoryWithSources(context.Background(), fs)
}

// DiscoverInventoryWithSources adds validated read-only candidates to the
// local system/managed inventory without copying their credential material.
func DiscoverInventoryWithSources(ctx context.Context, fs fsutil.FileSystem, sources ...ExternalCredentialSource) Inventory {
	inventory, _ := discoverInventoryWithSources(ctx, fs, false, sources...)
	return inventory
}

// discoverAuthoritativeInventoryWithSources fails closed when the core CQ or
// system credential inventory is unreadable, malformed, or unsafe. External
// sources remain optional and report their own typed degraded status.
func discoverAuthoritativeInventoryWithSources(ctx context.Context, fs fsutil.FileSystem, sources ...ExternalCredentialSource) (Inventory, error) {
	return discoverInventoryWithSources(ctx, fs, true, sources...)
}

func discoverInventoryWithSources(ctx context.Context, fs fsutil.FileSystem, authoritative bool, sources ...ExternalCredentialSource) (Inventory, error) {
	if authoritative {
		if _, ok := fs.(fsutil.SecurePathInspector); !ok {
			return Inventory{}, fsutil.ErrSecureCapabilityUnavailable
		}
		if _, ok := fs.(fsutil.NoFollowFileOpener); !ok {
			return Inventory{}, fsutil.ErrSecureCapabilityUnavailable
		}
	}
	home, err := fs.UserHomeDir()
	if err != nil {
		if authoritative {
			return Inventory{}, err
		}
		return Inventory{}, nil
	}
	coreDir := filepath.Join(home, ".codex")
	if authoritative {
		inspector := fs.(fsutil.SecurePathInspector)
		if _, err := inspector.Lstat(coreDir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Inventory{}, err
			}
		} else if err := fsutil.ValidateOwnerControlledDirectory(fs, coreDir); err != nil {
			return Inventory{}, err
		}
	}
	var raw []rawCandidate
	systemPath := filepath.Join(coreDir, "auth.json")
	if candidate, ok, err := readRawCandidate(fs, systemPath, SourceSystem, "system", authoritative); err != nil {
		return Inventory{}, err
	} else if ok {
		candidate.account.IsActive = true
		raw = append(raw, candidate)
	}
	accountsDir := filepath.Join(coreDir, "accounts")
	var entries []os.DirEntry
	if authoritative {
		inspector := fs.(fsutil.SecurePathInspector)
		if _, err := inspector.Lstat(accountsDir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Inventory{}, err
			}
		} else {
			if err := fsutil.ValidateOwnerControlledDirectory(fs, filepath.Dir(accountsDir)); err != nil {
				return Inventory{}, err
			}
			if err := fsutil.ValidateSecureDirectory(fs, accountsDir); err != nil {
				return Inventory{}, err
			}
			entries, err = fs.ReadDir(accountsDir)
			if err != nil {
				return Inventory{}, err
			}
		}
	} else {
		entries, err = fs.ReadDir(accountsDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			entries = nil
		}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".auth.json") {
			continue
		}
		if entry.IsDir() {
			if authoritative {
				return Inventory{}, fsutil.ErrUnsafeSecurePath
			}
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(accountsDir, name)
		if candidate, ok, err := readRawCandidate(fs, path, SourceManaged, name, authoritative); err != nil {
			return Inventory{}, err
		} else if ok {
			raw = append(raw, candidate)
		}
	}

	var inventory Inventory
	for _, candidate := range raw {
		matches := compatibleLogicalAccounts(inventory.Accounts, candidate.account)
		index := -1
		if candidate.metadata != nil {
			for i := range inventory.Accounts {
				if inventory.Accounts[i].Key == candidate.metadata.AccountKey {
					index = i
					break
				}
			}
		}
		if len(matches) == 1 {
			if index < 0 {
				index = matches[0]
			}
		}
		if index < 0 {
			identity := identityFromAccount(candidate.account)
			key := generationAccountKey(identity, candidate.source, candidate.sourceName)
			unstable := true
			if candidate.metadata != nil {
				key = candidate.metadata.AccountKey
				unstable = false
			}
			inventory.Accounts = append(inventory.Accounts, LogicalAccount{
				Key: key, Identity: identity, Unstable: unstable,
			})
			index = len(inventory.Accounts) - 1
		}
		logical := &inventory.Accounts[index]
		if candidate.metadata != nil && logical.Unstable {
			logical.Key = candidate.metadata.AccountKey
			logical.Unstable = false
			for i := range logical.Candidates {
				logical.Candidates[i].Ref.AccountKey = logical.Key
				logical.Candidates[i].Credential.AccountKey = logical.Key
			}
		}
		enrichIdentity(&logical.Identity, candidate.account)
		candidateID := generationCandidateID(candidate.source, candidate.account, candidate.sourceName)
		revision := credentialRevision(candidate.data)
		if candidate.metadata != nil {
			candidateID = candidate.metadata.CandidateID
			revision = candidate.metadata.Revision
		}
		ref := CandidateRef{AccountKey: logical.Key, CandidateID: candidateID, path: candidate.account.FilePath}
		credential := candidate.account
		credential.AccountKey = logical.Key
		credential.CandidateID = candidateID
		credential.Revision = revision
		routable := credential.AccountID != "" && credential.UserID != "" && credential.AccessToken != ""
		logical.Candidates = append(logical.Candidates, CredentialCandidate{
			Ref: ref, Revision: credential.Revision, Source: candidate.source,
			Credential: credential, AccessExpiresAt: unixMilliTime(credential.ExpiresAt),
			CQAuthored:      candidate.cqAuthored,
			RefreshEligible: resetCandidateRefreshEligible(candidate),
			Routable:        routable,
			DispatchBlocked: candidate.metadata != nil && candidate.metadata.OperationState != OperationReady,
		})
		logical.Active = logical.Active || candidate.source == SourceSystem
		logical.Routable = logical.Routable || routable
	}
	var externalCandidates []sourcedExternalCandidate
	sourceNames, sourceNameCounts := snapshotExternalSourceNames(sources)
	for index, source := range sources {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		if source == nil {
			continue
		}
		sourceName := sourceNames[index]
		status := ExternalSourceStatus{Name: sourceName}
		if sourceName == "" || sourceNameCounts[sourceName] != 1 {
			status.ErrorCode = externalSourceErrorCode(ErrExternalInvalid)
			status.TopologyInvalid = true
			inventory.ExternalSources = append(inventory.ExternalSources, status)
			continue
		}
		candidates, err := safeExternalSourceList(ctx, source)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Inventory{}, ctxErr
		}
		if err != nil {
			status.ErrorCode = externalSourceErrorCode(err)
			status.OptionalAbsent = errors.Is(err, errExternalNotConfigured)
			inventory.ExternalSources = append(inventory.ExternalSources, status)
			continue
		}
		validSource := true
		records := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			_, duplicate := records[candidate.Ref.RecordID]
			if candidate.Ref.Source != sourceName || candidate.Ref.RecordID == "" || candidate.Ref.Revision == "" || duplicate {
				validSource = false
				break
			}
			records[candidate.Ref.RecordID] = struct{}{}
		}
		if !validSource {
			status.ErrorCode = externalSourceErrorCode(ErrExternalInvalid)
			status.TopologyInvalid = true
			inventory.ExternalSources = append(inventory.ExternalSources, status)
			continue
		}
		status.CandidateCount = len(candidates)
		inventory.ExternalSources = append(inventory.ExternalSources, status)
		for _, candidate := range candidates {
			externalCandidates = append(externalCandidates, sourcedExternalCandidate{
				sourceName: sourceName, candidate: candidate,
			})
		}
	}
	sort.Slice(inventory.ExternalSources, func(i, j int) bool {
		a, b := inventory.ExternalSources[i], inventory.ExternalSources[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.ErrorCode != b.ErrorCode {
			return a.ErrorCode < b.ErrorCode
		}
		return a.CandidateCount < b.CandidateCount
	})
	sort.Slice(externalCandidates, func(i, j int) bool {
		a, b := externalCandidates[i], externalCandidates[j]
		if a.sourceName != b.sourceName {
			return a.sourceName < b.sourceName
		}
		if a.candidate.Ref.RecordID != b.candidate.Ref.RecordID {
			return a.candidate.Ref.RecordID < b.candidate.Ref.RecordID
		}
		return a.candidate.Ref.Revision < b.candidate.Ref.Revision
	})
	for _, candidate := range externalCandidates {
		appendExternalCandidate(&inventory, candidate.sourceName, candidate.candidate)
	}

	for i := range inventory.Accounts {
		logical := &inventory.Accounts[i]
		sortCandidates(logical.Candidates, "", time.Now())
		ids := make([]CandidateID, len(logical.Candidates))
		hasSystem, hasManaged := false, false
		for j, candidate := range logical.Candidates {
			ids[j] = candidate.Ref.CandidateID
			hasSystem = hasSystem || candidate.Source == SourceSystem
			hasManaged = hasManaged || candidate.Source == SourceManaged
		}
		if len(ids) > 1 {
			inventory.Intents = append(inventory.Intents, InventoryIntent{Kind: IntentAssociate, AccountKey: logical.Key, Candidates: ids})
		}
		if hasSystem && !hasManaged && logical.Routable {
			inventory.Intents = append(inventory.Intents, InventoryIntent{Kind: IntentAdopt, AccountKey: logical.Key, Candidates: ids})
		}
	}
	sort.SliceStable(inventory.Accounts, func(i, j int) bool {
		if inventory.Accounts[i].Active != inventory.Accounts[j].Active {
			return inventory.Accounts[i].Active
		}
		return inventory.Accounts[i].Key < inventory.Accounts[j].Key
	})
	return inventory, nil
}

func resetCandidateRefreshEligible(candidate rawCandidate) bool {
	metadata := candidate.metadata
	return candidate.source == SourceManaged && candidate.cqAuthored && metadata != nil &&
		metadata.Version == 1 && metadata.Provenance == ProvenanceCQOAuth &&
		metadata.RefreshOwnership == RefreshCQOwnedNeverExported &&
		metadata.OperationState == OperationReady && metadata.OperationID == "" &&
		candidate.account.RefreshToken != ""
}

func readRawCandidate(fs fsutil.FileSystem, path string, source CredentialSource, sourceName string, authoritative bool) (rawCandidate, bool, error) {
	var data []byte
	if authoritative {
		var present bool
		var err error
		data, present, err = readAuthoritativeCredentialFile(fs, path, source == SourceSystem)
		if err != nil || !present {
			return rawCandidate{}, false, err
		}
	} else {
		var err error
		data, err = fs.ReadFile(path)
		if err != nil {
			return rawCandidate{}, false, nil
		}
	}
	account, ok := parseAccountData(data, path)
	if !ok {
		if authoritative {
			return rawCandidate{}, false, errors.New("malformed credential file")
		}
		return rawCandidate{}, false, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		if authoritative {
			return rawCandidate{}, false, errors.New("malformed credential file")
		}
		return rawCandidate{}, false, nil
	}
	var metadata *ManagedMetadata
	if rawMetadata, ok := doc["_cq"]; ok && source == SourceManaged {
		var parsed ManagedMetadata
		metadataData, marshalErr := json.Marshal(rawMetadata)
		valid := marshalErr == nil && json.Unmarshal(metadataData, &parsed) == nil && parsed.Version == 1 && parsed.AccountKey != "" && parsed.CandidateID != "" && parsed.LineageID != "" && parsed.Generation > 0
		if !valid {
			if authoritative {
				return rawCandidate{}, false, errors.New("malformed managed credential metadata")
			}
			return rawCandidate{}, false, nil
		}
		if valid {
			material := CredentialMaterial{
				AccessToken: account.AccessToken, RefreshToken: account.RefreshToken,
				IDToken: account.IDToken, AccountID: account.AccountID,
			}
			if parsed.Revision != managedRecordRevision(doc, material, parsed) {
				if authoritative {
					return rawCandidate{}, false, errors.New("managed credential revision mismatch")
				}
				return rawCandidate{}, false, nil
			}
			if !authoritative {
				if info, err := fs.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
					return rawCandidate{}, false, nil
				}
			}
			metadata = &parsed
		}
	}
	return rawCandidate{
		account: account, data: data, source: source, sourceName: sourceName,
		cqAuthored: metadata != nil && metadata.Provenance == ProvenanceCQOAuth,
		metadata:   metadata,
	}, true, nil
}

func readAuthoritativeCredentialFile(fs fsutil.FileSystem, path string, standardCodexCoreDirectory bool) ([]byte, bool, error) {
	inspector, ok := fs.(fsutil.SecurePathInspector)
	if !ok {
		return nil, false, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := fs.(fsutil.NoFollowFileOpener)
	if !ok {
		return nil, false, fsutil.ErrSecureCapabilityUnavailable
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	directoryValidator := fsutil.ValidateSecureDirectory
	if standardCodexCoreDirectory {
		directoryValidator = fsutil.ValidateOwnerControlledDirectory
	}
	if err := directoryValidator(fs, filepath.Dir(path)); err != nil {
		return nil, false, err
	}
	if err := fsutil.ValidateSecureRegularFile(fs, path); err != nil {
		return nil, false, err
	}
	pathIdentity, ok := inspector.FileIdentity(pathInfo)
	if !ok {
		return nil, false, fsutil.ErrUnsafeSecurePath
	}
	file, err := opener.OpenNoFollow(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	openedIdentity, err := validateAuthoritativeCredentialInfo(inspector, openedInfo)
	if err != nil || openedIdentity.Device != pathIdentity.Device || openedIdentity.Inode != pathIdentity.Inode {
		if err != nil {
			return nil, false, err
		}
		return nil, false, fsutil.ErrUnsafeSecurePath
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumCoreCredentialSize+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maximumCoreCredentialSize {
		return nil, false, fsutil.ErrSecureFileTooLarge
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	afterIdentity, err := validateAuthoritativeCredentialInfo(inspector, afterInfo)
	if err != nil || afterIdentity != openedIdentity {
		if err != nil {
			return nil, false, err
		}
		return nil, false, fsutil.ErrUnsafeSecurePath
	}
	return data, true, nil
}

func validateAuthoritativeCredentialInfo(inspector fsutil.SecurePathInspector, info os.FileInfo) (fsutil.SecureFileIdentity, error) {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	owner, ok := inspector.FileOwnerUID(info)
	if !ok || owner != inspector.EffectiveUID() {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return identity, nil
}

func identityFromAccount(account CodexAccount) AccountIdentity {
	return AccountIdentity{
		AccountID: account.AccountID, UserID: account.UserID, Email: account.Email,
		PlanType: account.PlanType, RecordKey: account.RecordKey,
	}
}

func compatibleLogicalAccounts(accounts []LogicalAccount, candidate CodexAccount) []int {
	var matches []int
	for i, logical := range accounts {
		identity := logical.Identity
		if candidate.AccountID == "" || identity.AccountID == "" || candidate.AccountID != identity.AccountID {
			continue
		}
		if candidate.UserID != "" && identity.UserID != "" && candidate.UserID != identity.UserID {
			continue
		}
		matches = append(matches, i)
	}
	return matches
}

func enrichIdentity(identity *AccountIdentity, candidate CodexAccount) {
	if identity.AccountID == "" {
		identity.AccountID = candidate.AccountID
	}
	if identity.UserID == "" {
		identity.UserID = candidate.UserID
	}
	if identity.Email == "" {
		identity.Email = candidate.Email
	}
	if identity.PlanType == "" {
		identity.PlanType = candidate.PlanType
	}
	if identity.RecordKey == "" {
		identity.RecordKey = candidate.RecordKey
	}
}

func generationAccountKey(identity AccountIdentity, source CredentialSource, sourceName string) AccountKey {
	seed := ""
	if identity.AccountID != "" {
		seed = "account:" + identity.AccountID
		if identity.UserID != "" {
			seed += ":user:" + identity.UserID
		}
	} else {
		seed = "isolated:" + source.String() + ":" + sourceName
	}
	return AccountKey("gen:" + shortHash(seed))
}

func generationCandidateID(source CredentialSource, account CodexAccount, sourceName string) CandidateID {
	identity := account.RecordKey
	if identity == "" {
		identity = account.AccountID
	}
	if identity == "" {
		identity = sourceName
	}
	return CandidateID(source.String() + ":" + shortHash(identity+":"+sourceName))
}

func (s CredentialSource) String() string {
	switch s {
	case SourceSystem:
		return "system"
	case SourceManaged:
		return "managed"
	case SourceExternal:
		return "external"
	default:
		return "unknown"
	}
}

func appendExternalCandidate(inventory *Inventory, sourceName string, candidate ExternalCandidate) {
	account := CodexAccount{
		AccountID: candidate.Identity.AccountID, UserID: candidate.Identity.UserID,
		Email: candidate.Identity.Email, PlanType: candidate.Identity.PlanType,
		RecordKey: candidate.Identity.RecordKey,
	}
	matches := compatibleLogicalAccounts(inventory.Accounts, account)
	index := -1
	if len(matches) == 1 {
		index = matches[0]
	}
	if index < 0 {
		key := generationAccountKey(candidate.Identity, SourceExternal, sourceName+":"+candidate.Ref.RecordID)
		inventory.Accounts = append(inventory.Accounts, LogicalAccount{
			Key: key, Identity: candidate.Identity, Unstable: true,
		})
		index = len(inventory.Accounts) - 1
	}
	logical := &inventory.Accounts[index]
	enrichIdentity(&logical.Identity, account)
	candidateID := CandidateID(SourceExternal.String() + ":" + shortHash(sourceName+":"+candidate.Ref.RecordID))
	ref := CandidateRef{AccountKey: logical.Key, CandidateID: candidateID}
	externalRef := candidate.Ref
	routable := candidate.Routable && completeStrongIdentity(candidate.Identity)
	logical.Candidates = append(logical.Candidates, CredentialCandidate{
		Ref: ref, Revision: candidate.Ref.Revision, Source: SourceExternal,
		AccessExpiresAt: candidate.AccessExpiresAt, Routable: routable, externalRef: &externalRef,
	})
	logical.Routable = logical.Routable || routable
}

func completeStrongIdentity(identity AccountIdentity) bool {
	return identity.AccountID != "" && identity.UserID != ""
}

func snapshotExternalSourceNames(sources []ExternalCredentialSource) ([]string, map[string]int) {
	names := make([]string, len(sources))
	counts := make(map[string]int, len(sources))
	for index, source := range sources {
		if source == nil {
			continue
		}
		names[index] = safeExternalSourceName(source)
		counts[names[index]]++
	}
	return names, counts
}

func safeExternalSourceName(source ExternalCredentialSource) (name string) {
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	return source.Name()
}

func safeExternalSourceList(ctx context.Context, source ExternalCredentialSource) (candidates []ExternalCandidate, err error) {
	defer func() {
		if recover() != nil {
			candidates = nil
			err = ErrExternalInvalid
		}
	}()
	candidates, err = source.List(ctx)
	if err == nil {
		return candidates, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	sanitised := sanitiseExternalSourceError(err)
	if sanitised == ErrExternalInvalid && !errors.Is(err, ErrExternalInvalid) {
		return nil, errors.New("external credential source fetch failed")
	}
	return nil, sanitised
}

func safeExternalSourceResolve(ctx context.Context, source ExternalCredentialSource, ref ExternalCandidateRef) (material CredentialMaterial, err error) {
	defer func() {
		if recover() != nil {
			material = CredentialMaterial{}
			err = ErrExternalInvalid
		}
	}()
	material, err = source.Resolve(ctx, ref)
	if err == nil {
		return material, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CredentialMaterial{}, ctxErr
	}
	return CredentialMaterial{}, sanitiseExternalSourceError(err)
}

func sanitiseExternalSourceError(err error) error {
	switch {
	case errors.Is(err, errExternalNotConfigured):
		return errors.Join(ErrExternalUnavailable, errExternalNotConfigured)
	case errors.Is(err, ErrExternalUnavailable):
		return ErrExternalUnavailable
	case errors.Is(err, ErrExternalUnsafePath):
		return ErrExternalUnsafePath
	case errors.Is(err, ErrStaleRevision):
		return ErrStaleRevision
	case errors.Is(err, ErrExternalIdentityMismatch):
		return ErrExternalIdentityMismatch
	case errors.Is(err, ErrExternalFingerprintMismatch):
		return ErrExternalFingerprintMismatch
	default:
		return ErrExternalInvalid
	}
}

func externalSourceErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrExternalUnavailable):
		return "unavailable"
	case errors.Is(err, ErrExternalUnsafePath):
		return "unsafe_path"
	case errors.Is(err, ErrStaleRevision):
		return "stale_revision"
	case errors.Is(err, ErrExternalIdentityMismatch):
		return "identity_mismatch"
	case errors.Is(err, ErrExternalFingerprintMismatch):
		return "fingerprint_mismatch"
	case errors.Is(err, ErrExternalInvalid):
		return "invalid"
	default:
		return "fetch_error"
	}
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func unixMilliTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

// ResolveCandidate returns deterministic candidates in retry order.
func ResolveCandidate(logical LogicalAccount, accepted Revision, now time.Time) []CredentialCandidate {
	candidates := make([]CredentialCandidate, 0, len(logical.Candidates))
	for _, candidate := range logical.Candidates {
		if !candidate.DispatchBlocked {
			candidates = append(candidates, candidate)
		}
	}
	sortCandidates(candidates, accepted, now)
	return candidates
}

func sortCandidates(candidates []CredentialCandidate, accepted Revision, now time.Time) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Revision == accepted) != (b.Revision == accepted) {
			return a.Revision == accepted
		}
		classA, classB := expiryClass(a.AccessExpiresAt, now), expiryClass(b.AccessExpiresAt, now)
		if classA != classB {
			return classA < classB
		}
		if a.CQAuthored != b.CQAuthored {
			return a.CQAuthored
		}
		if !a.AccessExpiresAt.Equal(b.AccessExpiresAt) {
			return a.AccessExpiresAt.After(b.AccessExpiresAt)
		}
		if a.Source != b.Source {
			return sourcePriority(a.Source) < sourcePriority(b.Source)
		}
		return a.Ref.CandidateID < b.Ref.CandidateID
	})
}

func sourcePriority(source CredentialSource) int {
	switch source {
	case SourceSystem:
		return 0
	case SourceManaged:
		return 1
	case SourceExternal:
		return 2
	default:
		return 3
	}
}

func expiryClass(expires time.Time, now time.Time) int {
	if expires.IsZero() {
		return 1
	}
	if expires.After(now) {
		return 0
	}
	return 2
}
