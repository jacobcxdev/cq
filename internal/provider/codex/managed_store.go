package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/compat"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

type Provenance string

const (
	ProvenanceCQOAuth        Provenance = "cq_oauth"
	ProvenanceSystemBorrowed Provenance = "system_borrowed"
	ProvenanceLegacyUnknown  Provenance = "legacy_unknown"
)

type RefreshOwnership string

const (
	RefreshCQOwnedNeverExported RefreshOwnership = "cq_owned_never_exported"
	RefreshExportedToSystem     RefreshOwnership = "exported_to_system"
	RefreshOwnershipUnknown     RefreshOwnership = "unknown"
)

type OperationState string

const (
	OperationReady             OperationState = "ready"
	OperationRefreshing        OperationState = "refreshing"
	OperationRotationUncertain OperationState = "rotation_uncertain"
	OperationActivationPending OperationState = "activation_pending"
	OperationRemoving          OperationState = "removing"
)

type ManagedMetadata struct {
	Version          int              `json:"version"`
	AccountKey       AccountKey       `json:"account_key"`
	CandidateID      CandidateID      `json:"candidate_id"`
	LineageID        LineageID        `json:"lineage_id"`
	Generation       uint64           `json:"generation"`
	Revision         Revision         `json:"revision"`
	Provenance       Provenance       `json:"provenance"`
	RefreshOwnership RefreshOwnership `json:"refresh_ownership"`
	OperationState   OperationState   `json:"operation_state"`
	OperationID      string           `json:"operation_id,omitempty"`
}

type CredentialMaterial struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
}

type ManagedRecord struct {
	Path             string
	Metadata         ManagedMetadata
	Credential       CredentialMaterial
	Document         map[string]any
	RefreshSuspended bool
}

type CommitError struct {
	Step      string
	Committed bool
	Err       error
}

func (e *CommitError) Error() string { return fmt.Sprintf("managed credential %s: %v", e.Step, e.Err) }
func (e *CommitError) Unwrap() error { return e.Err }

var ErrStaleRevision = errors.New("managed credential revision changed")

type ManagedStore struct {
	FS          fsutil.DurableFileSystem
	Home        string
	Random      io.Reader
	EnsureEpoch func() error
}

func NewManagedStore(fs fsutil.DurableFileSystem) (*ManagedStore, error) {
	home, err := fs.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	store := &ManagedStore{FS: fs, Home: home, Random: rand.Reader}
	store.EnsureEpoch = func() error {
		path, err := compat.DefaultEpochPath(fs, os.Getenv)
		if err != nil {
			return err
		}
		return compat.EnsureEpoch(fs, path, compat.CurrentEpoch)
	}
	return store, nil
}

func (s *ManagedStore) SaveNew(credential LoginCredential) (ManagedRecord, error) {
	return s.saveNewForAccount(credential, "", ProvenanceCQOAuth, RefreshCQOwnedNeverExported)
}

func (s *ManagedStore) saveNew(credential LoginCredential, provenance Provenance, ownership RefreshOwnership) (ManagedRecord, error) {
	return s.saveNewForAccount(credential, "", provenance, ownership)
}

func (s *ManagedStore) saveNewForAccount(credential LoginCredential, existingAccountKey AccountKey, provenance Provenance, ownership RefreshOwnership) (ManagedRecord, error) {
	if s == nil || s.FS == nil {
		return ManagedRecord{}, errors.New("durable managed store unavailable")
	}
	if credential.Claims.RecordKey() == "" {
		return ManagedRecord{}, errors.New("credential lacks stable identity")
	}
	if credential.CreatedAt.IsZero() {
		credential.CreatedAt = time.Now().UTC()
	}
	accountKey := string(existingAccountKey)
	if accountKey == "" {
		var err error
		accountKey, err = s.randomID("acct")
		if err != nil {
			return ManagedRecord{}, err
		}
	}
	candidateID, err := s.randomID("cand")
	if err != nil {
		return ManagedRecord{}, err
	}
	lineageID, err := s.randomID("lineage")
	if err != nil {
		return ManagedRecord{}, err
	}
	path := filepath.Join(s.Home, ".codex", "accounts", string(candidateID)+".auth.json")
	record := ManagedRecord{
		Path: path,
		Metadata: ManagedMetadata{
			Version: 1, AccountKey: AccountKey(accountKey), CandidateID: CandidateID(candidateID),
			LineageID: LineageID(lineageID), Generation: 1, Provenance: provenance,
			RefreshOwnership: ownership, OperationState: OperationReady,
		},
		Credential: CredentialMaterial{
			AccessToken: credential.Tokens.AccessToken, RefreshToken: credential.Tokens.RefreshToken,
			IDToken: credential.Tokens.IDToken, AccountID: credential.Claims.AccountID,
		},
		Document: map[string]any{
			"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
			"last_refresh": credential.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if err := s.Commit(&record, ""); err != nil {
		return ManagedRecord{}, err
	}
	return record, nil
}

func (s *ManagedStore) Load(path string) (ManagedRecord, error) {
	data, err := s.FS.ReadFile(path)
	if err != nil {
		return ManagedRecord{}, err
	}
	info, err := s.FS.Stat(path)
	if err != nil {
		return ManagedRecord{}, err
	}
	if info.Mode().Perm() != 0o600 {
		return ManagedRecord{}, fmt.Errorf("managed credential permissions %o, want 600", info.Mode().Perm())
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return ManagedRecord{}, errors.New("invalid managed credential JSON")
	}
	credential, err := credentialFromDocument(doc)
	if err != nil {
		return ManagedRecord{}, err
	}
	rawMetadata, exists := doc["_cq"]
	if !exists {
		return ManagedRecord{
			Path: path, Document: doc, Credential: credential, RefreshSuspended: true,
			Metadata: ManagedMetadata{
				Version: 0, AccountKey: AccountKey("legacy:" + shortHash(path)),
				CandidateID: CandidateID("legacy:" + shortHash(path)), Provenance: ProvenanceLegacyUnknown,
				RefreshOwnership: RefreshOwnershipUnknown, OperationState: OperationReady,
			},
		}, nil
	}
	metadataData, err := json.Marshal(rawMetadata)
	if err != nil {
		return ManagedRecord{}, errors.New("invalid _cq metadata")
	}
	var metadata ManagedMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil || metadata.Version != 1 || metadata.AccountKey == "" || metadata.CandidateID == "" || metadata.LineageID == "" || metadata.Generation == 0 {
		return ManagedRecord{}, errors.New("invalid _cq metadata")
	}
	record := ManagedRecord{Path: path, Metadata: metadata, Credential: credential, Document: doc}
	if expected := managedRecordRevision(doc, credential, metadata); metadata.Revision != expected {
		return ManagedRecord{}, errors.New("managed credential revision mismatch")
	}
	if !validProvenance(metadata.Provenance) || !validRefreshOwnership(metadata.RefreshOwnership) || !validOperationState(metadata.OperationState) {
		record.RefreshSuspended = true
		return record, nil
	}
	record.RefreshSuspended = metadata.Provenance != ProvenanceCQOAuth || metadata.RefreshOwnership != RefreshCQOwnedNeverExported || metadata.OperationState != OperationReady
	return record, nil
}

func (s *ManagedStore) Commit(record *ManagedRecord, expected Revision) error {
	if record == nil || record.Path == "" || record.Metadata.AccountKey == "" || record.Metadata.CandidateID == "" || record.Metadata.LineageID == "" {
		return errors.New("invalid managed record")
	}
	if s.EnsureEpoch == nil {
		return errors.New("compatibility epoch guard unavailable")
	}
	if err := s.EnsureEpoch(); err != nil {
		return fmt.Errorf("advance compatibility epoch: %w", err)
	}
	if expected != "" {
		current, err := s.Load(record.Path)
		if err != nil {
			return err
		}
		if current.Metadata.Revision != expected {
			return ErrStaleRevision
		}
		if record.Metadata.Generation <= current.Metadata.Generation {
			record.Metadata.Generation = current.Metadata.Generation + 1
		}
	}
	record.Metadata.Revision = managedRecordRevision(record.Document, record.Credential, record.Metadata)
	doc := managedRecordDocument(record.Document, record.Credential, record.Metadata)
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := s.durableReplace(record.Path, data); err != nil {
		return err
	}
	record.Document = doc
	return nil
}

func (s *ManagedStore) durableReplace(path string, data []byte) error {
	if err := s.FS.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return &CommitError{Step: "mkdir", Err: err}
	}
	suffix, err := s.randomID("tmp")
	if err != nil {
		return err
	}
	tmp := path + "." + suffix
	if err := s.FS.WriteFile(tmp, data, 0o600); err != nil {
		return &CommitError{Step: "write", Err: err}
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.FS.Remove(tmp)
		}
	}()
	if err := s.FS.Chmod(tmp, 0o600); err != nil {
		return &CommitError{Step: "chmod", Err: err}
	}
	if err := s.FS.SyncFile(tmp); err != nil {
		return &CommitError{Step: "file sync", Err: err}
	}
	if err := s.FS.Rename(tmp, path); err != nil {
		return &CommitError{Step: "rename", Err: err}
	}
	cleanup = false
	info, err := s.FS.Stat(path)
	if err != nil {
		return &CommitError{Step: "permission check", Committed: true, Err: err}
	}
	if info.Mode().Perm() != 0o600 {
		return &CommitError{Step: "permission check", Committed: true, Err: fmt.Errorf("mode %o", info.Mode().Perm())}
	}
	if err := s.FS.SyncDir(filepath.Dir(path)); err != nil {
		return &CommitError{Step: "directory sync", Committed: true, Err: err}
	}
	return nil
}

func (s *ManagedStore) randomID(prefix string) (string, error) {
	reader := s.Random
	if reader == nil {
		reader = rand.Reader
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(buf), nil
}

func credentialFromDocument(doc map[string]any) (CredentialMaterial, error) {
	tokens, ok := doc["tokens"].(map[string]any)
	if !ok {
		return CredentialMaterial{}, errors.New("managed credential tokens missing")
	}
	material := CredentialMaterial{
		AccessToken: stringValue(tokens["access_token"]), RefreshToken: stringValue(tokens["refresh_token"]),
		IDToken: stringValue(tokens["id_token"]), AccountID: stringValue(tokens["account_id"]),
	}
	if material.AccessToken == "" {
		return CredentialMaterial{}, errors.New("managed credential access token missing")
	}
	return material, nil
}

func managedRecordRevision(document map[string]any, material CredentialMaterial, metadata ManagedMetadata) Revision {
	metadata.Revision = ""
	data, _ := json.Marshal(managedRecordDocument(document, material, metadata))
	sum := sha256.Sum256(data)
	return Revision(hex.EncodeToString(sum[:]))
}

func managedRecordDocument(document map[string]any, material CredentialMaterial, metadata ManagedMetadata) map[string]any {
	doc := cloneDocument(document)
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = make(map[string]any)
	}
	tokens["access_token"] = material.AccessToken
	tokens["refresh_token"] = material.RefreshToken
	tokens["id_token"] = material.IDToken
	tokens["account_id"] = material.AccountID
	doc["tokens"] = tokens

	metadataDocument := make(map[string]any)
	if existing, ok := doc["_cq"].(map[string]any); ok {
		for key, value := range existing {
			metadataDocument[key] = value
		}
	}
	// Optional known fields must disappear when cleared; preserving the old
	// value would keep a completed operation permanently pending.
	delete(metadataDocument, "operation_id")
	knownData, _ := json.Marshal(metadata)
	var known map[string]any
	_ = json.Unmarshal(knownData, &known)
	for key, value := range known {
		metadataDocument[key] = value
	}
	doc["_cq"] = metadataDocument
	return doc
}

func stringValue(value any) string { valueString, _ := value.(string); return valueString }

func cloneDocument(document map[string]any) map[string]any {
	data, _ := json.Marshal(document)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	if clone == nil {
		clone = make(map[string]any)
	}
	return clone
}

func validProvenance(value Provenance) bool {
	return value == ProvenanceCQOAuth || value == ProvenanceSystemBorrowed || value == ProvenanceLegacyUnknown
}

func validRefreshOwnership(value RefreshOwnership) bool {
	return value == RefreshCQOwnedNeverExported || value == RefreshExportedToSystem || value == RefreshOwnershipUnknown
}

func validOperationState(value OperationState) bool {
	return value == OperationReady || value == OperationRefreshing || value == OperationRotationUncertain || value == OperationActivationPending || value == OperationRemoving
}
