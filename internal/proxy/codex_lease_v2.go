package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/compat"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexLeaseJournalVersionV2 = 2
	codexLeaseJournalVersionV3 = 3
	codexLeaseHashVersion      = 1

	CodexLeaseCutoverComplete         = "complete"
	CodexLeaseCutoverLegacyQuarantine = "legacy_quarantine"
)

var (
	ErrCodexLeaseFreshInstallAuthorityRequired = errors.New("Codex lease fresh-install authority required")
	ErrCodexLeaseTrustLost                     = errors.New("Codex lease authority trust lost")
	ErrCodexLegacyQuarantine                   = errors.New("Codex legacy lease quarantine active")
	ErrCodexLeaseWriterUnavailable             = errors.New("Codex lease writer unavailable")
	ErrCodexLeaseStorePoisoned                 = errors.New("Codex lease store poisoned")
	ErrCodexLeaseStaleMutation                 = errors.New("stale Codex lease mutation")
	ErrCodexLeaseAuthorityMismatch             = errors.New("Codex lease authority mismatch")
)

type CodexLeaseCutover struct {
	SourceVersion           int       `json:"source_version"`
	CompatibilityEpoch      int       `json:"compatibility_epoch"`
	State                   string    `json:"state"`
	At                      time.Time `json:"at"`
	JournalGeneration       uint64    `json:"journal_generation"`
	AuthoritativeModeEpochs []uint64  `json:"authoritative_mode_epochs,omitempty"`
	ShadowModeEpochs        []uint64  `json:"shadow_mode_epochs,omitempty"`
	LegacyQuarantineUntil   time.Time `json:"legacy_quarantine_until,omitempty"`
	LegacyV1SHA256          string    `json:"legacy_v1_sha256,omitempty"`
	CompletedAt             time.Time `json:"completed_at,omitempty"`
	CompletionGeneration    uint64    `json:"completion_generation,omitempty"`
	NoLegacyAuthority       bool      `json:"no_legacy_authority"`
}

type CodexJournalLane struct {
	SessionHash                     string    `json:"session_hash"`
	ThreadHash                      string    `json:"thread_hash"`
	NamespaceHash                   string    `json:"namespace_hash"`
	Generation                      uint64    `json:"generation"`
	CurrentTurnHash                 string    `json:"current_turn_hash,omitempty"`
	CurrentModeEpoch                uint64    `json:"current_mode_epoch,omitempty"`
	CurrentAuthoritative            bool      `json:"current_authoritative,omitempty"`
	LastTurnHash                    string    `json:"last_turn_hash,omitempty"`
	LastModeEpoch                   uint64    `json:"last_mode_epoch,omitempty"`
	LastAuthoritative               bool      `json:"last_authoritative,omitempty"`
	LastAdmittedAccountHash         string    `json:"last_admitted_account_hash,omitempty"`
	LastAdmittedTurnHash            string    `json:"last_admitted_turn_hash,omitempty"`
	LastAdmittedModeEpoch           uint64    `json:"last_admitted_mode_epoch,omitempty"`
	LastAdmittedAuthoritative       bool      `json:"last_admitted_authoritative,omitempty"`
	LastAdmissionJournalGeneration  uint64    `json:"last_admission_journal_generation,omitempty"`
	LastAdmittedAt                  time.Time `json:"last_admitted_at,omitempty"`
	LastCacheAdmittedAt             time.Time `json:"last_cache_admitted_at,omitzero"`
	LastCacheEffectiveModel         string    `json:"last_cache_effective_model,omitempty"`
	RequestUnavailableAccountHashes []string  `json:"request_unavailable_account_hashes,omitempty"`
	QuotaExhaustedAccountHashes     []string  `json:"quota_exhausted_account_hashes,omitempty"`
	LastObservedAt                  time.Time `json:"last_observed_at"`
}

type CodexAttemptSlotKind string

const (
	CodexAttemptSlotDirect                 CodexAttemptSlotKind = "direct"
	CodexAttemptSlotEligibleManagedRefresh CodexAttemptSlotKind = "eligible_managed_refresh"
)

type CodexAttemptSlot struct {
	Index         uint32               `json:"index"`
	AccountHash   string               `json:"account_hash"`
	CandidateHash string               `json:"candidate_hash"`
	Kind          CodexAttemptSlotKind `json:"kind"`
}

type CodexAttemptEnvelope struct {
	PolicyVersion uint32             `json:"policy_version"`
	PlanDigest    string             `json:"plan_digest"`
	AttemptLimit  uint32             `json:"attempt_limit"`
	Slots         []CodexAttemptSlot `json:"slots"`
}

type CodexJournalAttempt struct {
	Generation     uint64            `json:"generation"`
	Revision       uint64            `json:"revision"`
	Slot           uint32            `json:"slot"`
	State          CodexAttemptState `json:"state"`
	CreatedAt      time.Time         `json:"created_at"`
	LastObservedAt time.Time         `json:"last_observed_at"`
}

// CodexCurrentRequest is the bounded, replaceable request authority within a
// stable logical turn record. Generation disambiguates callbacks after a
// completed request is atomically replaced by an explicit BeginRequest.
type CodexCurrentRequest struct {
	Generation               uint64                `json:"generation"`
	RequestKind              CodexRequestKind      `json:"request_kind,omitempty"`
	CompactionPhase          CodexCompactionPhase  `json:"compaction_phase,omitempty"`
	RequestedModelHash       string                `json:"requested_model_hash,omitempty"`
	DispatchPermitDigest     string                `json:"dispatch_permit_digest,omitempty"`
	QuotaExhaustionProbe     bool                  `json:"quota_exhaustion_probe,omitempty"`
	EffectiveModel           string                `json:"effective_model,omitempty"`
	RequiredBuckets          []CapacityBucket      `json:"required_buckets,omitempty"`
	AttemptEnvelope          CodexAttemptEnvelope  `json:"attempt_envelope"`
	CurrentAttemptGeneration uint64                `json:"current_attempt_generation,omitempty"`
	RoutingRefs              int                   `json:"routing_refs,omitempty"`
	AttemptRefs              int                   `json:"attempt_refs,omitempty"`
	ResponseObserverRefs     int                   `json:"response_observer_refs,omitempty"`
	Attempts                 []CodexJournalAttempt `json:"attempts,omitempty"`
}

type CodexJournalRecordV2 struct {
	SessionHash                      string     `json:"session_hash"`
	ThreadHash                       string     `json:"thread_hash"`
	NamespaceHash                    string     `json:"namespace_hash"`
	TurnHash                         string     `json:"turn_hash"`
	AccountHash                      string     `json:"account_hash,omitempty"`
	PredecessorTurnHash              string     `json:"predecessor_turn_hash,omitempty"`
	PredecessorModeEpoch             uint64     `json:"predecessor_mode_epoch,omitempty"`
	PredecessorAuthoritative         bool       `json:"predecessor_authoritative,omitempty"`
	CorrelationHash                  string     `json:"correlation_hash,omitempty"`
	TurnStateHash                    string     `json:"turn_state_hash,omitempty"`
	RecordGeneration                 uint64     `json:"record_generation"`
	LaneGeneration                   uint64     `json:"lane_generation"`
	PredecessorGeneration            uint64     `json:"predecessor_generation,omitempty"`
	LeaseGeneration                  uint64     `json:"lease_generation"`
	ModeEpoch                        uint64     `json:"mode_epoch"`
	DownstreamSocketGeneration       uint64     `json:"downstream_socket_generation,omitempty"`
	UpstreamSocketGeneration         uint64     `json:"upstream_socket_generation,omitempty"`
	State                            LeaseState `json:"state"`
	ProtocolSchema                   int        `json:"protocol_schema"`
	Authoritative                    bool       `json:"authoritative"`
	SocketLineageExtinct             bool       `json:"socket_lineage_extinct"`
	CodexCurrentRequest              `json:"current_request"`
	HasEncryptedState                bool                 `json:"has_encrypted_state,omitempty"`
	HasResponseAnchor                bool                 `json:"has_response_anchor,omitempty"`
	HasTurnState                     bool                 `json:"has_turn_state,omitempty"`
	NonMigratable                    bool                 `json:"non_migratable,omitempty"`
	AdoptedPrewarm                   bool                 `json:"adopted_prewarm,omitempty"`
	PrewarmAdoptionJournalGeneration uint64               `json:"prewarm_adoption_journal_generation,omitempty"`
	EverAdmitted                     bool                 `json:"ever_admitted,omitempty"`
	AdmissionJournalGeneration       uint64               `json:"admission_journal_generation,omitempty"`
	AdmissionRequestGeneration       uint64               `json:"admission_request_generation,omitempty"`
	AdmissionRequestKind             CodexRequestKind     `json:"admission_request_kind,omitempty"`
	AdmissionCompactionPhase         CodexCompactionPhase `json:"admission_compaction_phase,omitempty"`
	AdmittedAt                       time.Time            `json:"admitted_at,omitempty"`
	CreatedAt                        time.Time            `json:"created_at"`
	LastObservedAt                   time.Time            `json:"last_observed_at"`
}

type codexLeaseJournalEnvelopeV2 struct {
	Version                        int                    `json:"version"`
	HashVersion                    int                    `json:"hash_version"`
	Generation                     uint64                 `json:"generation"`
	AffinityInvalidationGeneration uint64                 `json:"affinity_invalidation_generation,omitempty"`
	Cutover                        CodexLeaseCutover      `json:"cutover"`
	Lanes                          []CodexJournalLane     `json:"lanes"`
	Records                        []CodexJournalRecordV2 `json:"records"`
	MAC                            string                 `json:"mac"`
}

type CodexLeaseWriterAuthority interface {
	AssertOwner() error
	BeginOwnerOperation() (*codex.CredentialOwnerOperation, error)
}

type CodexLeasePolicy struct {
	Retention time.Duration
	Now       func() time.Time
}

type CodexModeAuthoritySnapshot struct {
	RecognisedAuthoritativeEpochs []uint64
}

type CodexContinuityOpenOptions struct {
	FS          fsutil.DurableFileSystem
	KeyPath     string
	JournalPath string
	Policy      CodexLeasePolicy
	Modes       CodexModeAuthoritySnapshot
}

type CodexLeaseAuthorityPolicy struct {
	ModeEpoch                   uint64
	Authoritative               bool
	RetainedAuthoritativeEpochs []uint64
}

type CodexRestoredLaneClassification string

const (
	CodexRestoredLaneCurrent         CodexRestoredLaneClassification = "current"
	CodexRestoredLaneHistorical      CodexRestoredLaneClassification = "historical"
	CodexRestoredLaneUnseen          CodexRestoredLaneClassification = "unseen"
	CodexRestoredLaneShadowOnly      CodexRestoredLaneClassification = "shadow_only"
	CodexRestoredLaneRecoveryBlocked CodexRestoredLaneClassification = "recovery_blocked"
)

type CodexRestoredLane struct {
	Classification                 CodexRestoredLaneClassification
	RequestedIdentity              CodexJournalRecordIdentity
	RequestedRecord                CodexJournalRecordV2
	Lane                           CodexJournalLane
	Records                        []CodexJournalRecordV2
	ResolvedRecords                []CodexRestoredRecord
	Affinity                       *CodexLeaseAffinityHint
	AffinityInvalidationGeneration uint64
	Fence                          CodexLeaseGenerationFence
}

// CodexLeaseAffinityHint is signed, non-authoritative routing affinity from
// the latest successfully admitted authoritative record in a lane. An
// unresolved hint deliberately carries no account key or persisted HMAC.
type CodexLeaseAffinityHint struct {
	Resolved                   bool
	AccountKey                 codex.AccountKey
	Source                     CodexJournalRecordIdentity
	AdmissionJournalGeneration uint64
	AdmittedAt                 time.Time
	CacheAdmittedAt            time.Time
	CacheEffectiveModel        string
}

type CodexContinuityCoordinator struct {
	store    *CodexLeaseStore
	leases   *CodexTurnLeaseManager
	prewarms *CodexPrewarmManager
}

type codexLeaseStoreOperation struct {
	store *CodexLeaseStore
	owner *codex.CredentialOwnerOperation
	once  sync.Once
}

// Store returns the exact durable store opened by this coordinator. The
// coordinator retains lifecycle ownership: Close revokes every live-manager
// alias before closing this store, and retained store aliases then fail closed.
func (coordinator *CodexContinuityCoordinator) Store() *CodexLeaseStore {
	if coordinator == nil {
		return nil
	}
	return coordinator.store
}

func OpenCodexContinuityCoordinator(options CodexContinuityOpenOptions, owner CodexLeaseWriterAuthority) (*CodexContinuityCoordinator, error) {
	if options.FS == nil || options.KeyPath == "" || options.JournalPath == "" || filepath.Dir(options.KeyPath) != filepath.Dir(options.JournalPath) || filepath.Base(options.KeyPath) == filepath.Base(options.JournalPath) {
		return nil, errors.New("Codex lease journal and key require distinct names in one state directory")
	}
	if options.Policy.Retention <= 0 || options.Policy.Now == nil {
		return nil, errors.New("Codex lease policy requires positive retention and a clock")
	}
	modes := cloneCodexModeSnapshot(options.Modes)
	if !validCodexModeSnapshot(modes) {
		return nil, fmt.Errorf("%w: missing or non-canonical mode authority snapshot", ErrCodexLeaseTrustLost)
	}
	if compat.CurrentEpoch != 4 {
		return nil, fmt.Errorf("%w: unsupported compatibility floor %d", ErrCodexLeaseTrustLost, compat.CurrentEpoch)
	}
	operation, err := beginCodexLeaseOwnerOperation(owner)
	if err != nil {
		return nil, err
	}
	defer operation.Release()

	inspector, ok := options.FS.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := options.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(options.JournalPath)
	directoryExists, directoryErr := codexLeasePathExists(inspector, directoryPath)
	if directoryErr != nil {
		return nil, fmt.Errorf("%w: inspect Codex lease directory: %v", ErrCodexLeaseTrustLost, directoryErr)
	}
	if directoryExists {
		if err := fsutil.ValidateSecureDirectory(options.FS, directoryPath); err != nil {
			return nil, fmt.Errorf("%w: secure Codex lease directory: %v", ErrCodexLeaseTrustLost, err)
		}
	}
	keyExists, keyErr := codexLeasePathExists(inspector, options.KeyPath)
	journalExists, journalErr := codexLeasePathExists(inspector, options.JournalPath)
	if keyErr != nil {
		return nil, fmt.Errorf("inspect Codex lease key: %w", keyErr)
	}
	if journalErr != nil {
		return nil, fmt.Errorf("inspect Codex lease journal: %w", journalErr)
	}
	if !keyExists && !journalExists {
		return nil, ErrCodexLeaseFreshInstallAuthorityRequired
	}
	if keyExists != journalExists {
		return nil, fmt.Errorf("%w: Codex lease key/journal pair incomplete", ErrCodexLeaseTrustLost)
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: open Codex lease directory: %v", ErrCodexLeaseTrustLost, err)
	}
	keepDirectory := false
	defer func() {
		if !keepDirectory {
			_ = directory.Close()
		}
	}()
	directoryID, err := codexLeaseDirectoryIdentity(inspector, directory, directoryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: validate Codex lease directory: %v", ErrCodexLeaseTrustLost, err)
	}
	key, keyID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, filepath.Base(options.KeyPath), codexLeaseHMACKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read Codex lease key: %v", ErrCodexLeaseTrustLost, err)
	}
	if len(key) != codexLeaseHMACKeyBytes {
		clear(key)
		return nil, fmt.Errorf("%w: invalid Codex lease HMAC key length", ErrCodexLeaseTrustLost)
	}
	defer clear(key)
	journal, journalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, filepath.Base(options.JournalPath), codexLeaseJournalMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read Codex lease journal: %v", ErrCodexLeaseTrustLost, err)
	}
	store := &CodexLeaseStore{
		fs:            options.FS,
		path:          options.JournalPath,
		keyPath:       options.KeyPath,
		key:           append([]byte(nil), key...),
		owner:         owner,
		inspector:     inspector,
		directory:     directory,
		directoryPath: directoryPath,
		directoryID:   directoryID,
		journalName:   filepath.Base(options.JournalPath),
		keyName:       filepath.Base(options.KeyPath),
		keyID:         keyID,
		journalID:     journalID,
		journalBytes:  append([]byte(nil), journal...),
		policy:        options.Policy,
		modes:         modes,
	}
	if err := store.loadOrMigrateV2Locked(journal); err != nil {
		return nil, err
	}
	keepDirectory = true
	epoch := uint64(1)
	if len(store.modes.RecognisedAuthoritativeEpochs) != 0 {
		epoch = store.modes.RecognisedAuthoritativeEpochs[len(store.modes.RecognisedAuthoritativeEpochs)-1]
	}
	leases := NewCodexTurnLeaseManager(epoch, false, options.Policy.Now)
	return &CodexContinuityCoordinator{store: store, leases: leases, prewarms: NewCodexPrewarmManager(leases, options.Policy.Now)}, nil
}

func (coordinator *CodexContinuityCoordinator) Close() error {
	if coordinator == nil {
		return nil
	}
	if coordinator.leases != nil {
		coordinator.leases.revoke()
	}
	if coordinator.store == nil {
		return nil
	}
	return coordinator.store.close()
}

func (store *CodexLeaseStore) beginOperation() (*codexLeaseStoreOperation, error) {
	if store == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	store.lifecycle.RLock()
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		store.lifecycle.RUnlock()
		return nil, ErrCodexLeaseWriterUnavailable
	}
	owner := store.owner
	store.mu.Unlock()
	operation, err := beginCodexLeaseOwnerOperation(owner)
	if err != nil {
		store.lifecycle.RUnlock()
		return nil, err
	}
	return &codexLeaseStoreOperation{store: store, owner: operation}, nil
}

func (operation *codexLeaseStoreOperation) Release() {
	if operation == nil {
		return
	}
	operation.once.Do(func() {
		if operation.owner != nil {
			operation.owner.Release()
		}
		if operation.store != nil {
			operation.store.lifecycle.RUnlock()
		}
		operation.owner = nil
		operation.store = nil
	})
}

func (store *CodexLeaseStore) close() error {
	if store == nil {
		return nil
	}
	store.lifecycle.Lock()
	defer store.lifecycle.Unlock()
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return nil
	}
	store.closed = true
	clearCodexLeaseV2Envelope(store.v2)
	store.v2 = nil
	clear(store.records)
	store.records = nil
	clear(store.modes.RecognisedAuthoritativeEpochs)
	store.modes = CodexModeAuthoritySnapshot{}
	clear(store.key)
	clear(store.journalBytes)
	clear(store.legacyArchiveBytes)
	store.key = nil
	store.journalBytes = nil
	store.legacyArchiveBytes = nil
	directory := store.directory
	store.directory = nil
	store.fs = nil
	store.inspector = nil
	store.owner = nil
	store.policy = CodexLeasePolicy{}
	store.path = ""
	store.keyPath = ""
	store.directoryPath = ""
	store.journalName = ""
	store.keyName = ""
	store.legacyArchive = ""
	store.directoryID = fsutil.SecureFileIdentity{}
	store.keyID = fsutil.SecureFileIdentity{}
	store.journalID = fsutil.SecureFileIdentity{}
	store.legacyArchiveID = fsutil.SecureFileIdentity{}
	store.poisoned = nil
	store.mu.Unlock()
	if directory != nil {
		return directory.Close()
	}
	return nil
}

func clearCodexLeaseV2Envelope(envelope *codexLeaseJournalEnvelopeV2) {
	if envelope == nil {
		return
	}
	clear(envelope.Cutover.AuthoritativeModeEpochs)
	clear(envelope.Cutover.ShadowModeEpochs)
	for index := range envelope.Lanes {
		clear(envelope.Lanes[index].RequestUnavailableAccountHashes)
		clear(envelope.Lanes[index].QuotaExhaustedAccountHashes)
		envelope.Lanes[index] = CodexJournalLane{}
	}
	for index := range envelope.Records {
		record := &envelope.Records[index]
		clear(record.RequiredBuckets)
		clear(record.AttemptEnvelope.Slots)
		clear(record.Attempts)
		*record = CodexJournalRecordV2{}
	}
	clear(envelope.Lanes)
	clear(envelope.Records)
	*envelope = codexLeaseJournalEnvelopeV2{}
}

func (store *CodexLeaseStore) loadOrMigrateV2Locked(journal []byte) error {
	version, err := decodeCodexLeaseVersion(journal)
	if err != nil {
		return fmt.Errorf("%w: decode Codex lease journal discriminator: %v", ErrCodexLeaseTrustLost, err)
	}
	switch version {
	case codexLeaseJournalVersion:
		return store.migrateV1Locked(journal)
	case codexLeaseJournalVersionV2:
		if err := store.migrateV2Locked(journal); err != nil {
			return err
		}
		return store.normaliseRestoredV2Locked()
	case codexLeaseJournalVersionV3:
		if err := store.loadV2Locked(journal); err != nil {
			return err
		}
		return store.normaliseRestoredV2Locked()
	default:
		return fmt.Errorf("%w: unsupported Codex lease journal version %d", ErrCodexLeaseTrustLost, version)
	}
}

func (store *CodexLeaseStore) migrateV1Locked(journal []byte) error {
	if compat.CurrentEpoch != 4 {
		return fmt.Errorf("%w: unsupported compatibility floor %d", ErrCodexLeaseTrustLost, compat.CurrentEpoch)
	}
	var legacy codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(journal, &legacy); err != nil {
		return fmt.Errorf("%w: decode legacy Codex lease journal: %v", ErrCodexLeaseTrustLost, err)
	}
	if legacy.Version != codexLeaseJournalVersion || !store.validEnvelopeMAC(legacy) {
		return fmt.Errorf("%w: legacy Codex lease journal MAC mismatch", ErrCodexLeaseTrustLost)
	}
	if legacy.Generation == math.MaxUint64 {
		return fmt.Errorf("%w: legacy Codex lease generation overflow", ErrCodexLeaseTrustLost)
	}
	digestBytes := sha256.Sum256(journal)
	digest := hex.EncodeToString(digestBytes[:])
	archiveName := store.journalName + ".v1-" + digest + ".archive"
	authoritative, shadow := codexLeaseV1Epochs(legacy.Records)
	now := store.policy.Now().UTC()
	nextGeneration := legacy.Generation + 1
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV3,
		HashVersion: codexLeaseHashVersion,
		Generation:  nextGeneration,
		Cutover: CodexLeaseCutover{
			SourceVersion:           1,
			CompatibilityEpoch:      compat.CurrentEpoch,
			State:                   CodexLeaseCutoverLegacyQuarantine,
			At:                      now,
			JournalGeneration:       nextGeneration,
			AuthoritativeModeEpochs: authoritative,
			ShadowModeEpochs:        shadow,
			LegacyQuarantineUntil:   now.Add(store.policy.Retention),
			LegacyV1SHA256:          digest,
		},
		Lanes:   []CodexJournalLane{},
		Records: []CodexJournalRecordV2{},
	}
	data, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return err
	}
	if _, err := store.decodeAndValidateV2Candidate(data); err != nil {
		return fmt.Errorf("validate Codex lease migration candidate: %w", err)
	}
	archive, archiveID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
	switch {
	case err == nil:
		if !bytes.Equal(archive, journal) || store.validateLegacyArchive(archive, legacy.Generation) != nil {
			return fmt.Errorf("%w: legacy Codex lease archive mismatch", ErrCodexLeaseTrustLost)
		}
	case errors.Is(err, os.ErrNotExist):
		beforeArchive := func() error {
			if err := store.revalidateV1SourceLocked(journal, legacy.Generation); err != nil {
				return err
			}
			file, openErr := store.directory.OpenNoFollow(archiveName)
			if openErr == nil {
				_ = file.Close()
				return fmt.Errorf("%w: legacy archive appeared before replace", ErrCodexLeaseTrustLost)
			}
			if !errors.Is(openErr, os.ErrNotExist) {
				return openErr
			}
			return nil
		}
		if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, archiveName, journal, beforeArchive); err != nil {
			return fmt.Errorf("archive legacy Codex lease journal: %w", err)
		}
		archive, archiveID, err = fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
		if err != nil || !bytes.Equal(archive, journal) {
			cause := err
			if cause == nil {
				cause = errors.New("installed archive bytes differ")
			}
			return &fsutil.CommitError{
				Outcome: fsutil.CommitIndeterminate,
				Op:      "verify legacy Codex lease archive",
				Err:     errors.Join(ErrCodexLeaseTrustLost, cause),
			}
		}
	default:
		return fmt.Errorf("%w: read legacy Codex lease archive: %v", ErrCodexLeaseTrustLost, err)
	}

	beforeJournal := func() error {
		if err := store.revalidateV1SourceLocked(journal, legacy.Generation); err != nil {
			return err
		}
		got, gotID, readErr := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
		if readErr != nil || gotID != archiveID || !bytes.Equal(got, journal) {
			return fmt.Errorf("%w: legacy archive changed before cutover", ErrCodexLeaseTrustLost)
		}
		return nil
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, data, beforeJournal); err != nil {
		return fmt.Errorf("install Codex lease journal v2: %w", err)
	}
	installed, installedID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || !bytes.Equal(installed, data) {
		cause := err
		if cause == nil {
			cause = errors.New("installed journal bytes differ")
		}
		return &fsutil.CommitError{
			Outcome: fsutil.CommitIndeterminate,
			Op:      "verify installed Codex lease journal v2",
			Err:     errors.Join(ErrCodexLeaseTrustLost, cause),
		}
	}
	installedEnvelope, err := store.decodeAndValidateV2Candidate(installed)
	if err != nil {
		return &fsutil.CommitError{
			Outcome: fsutil.CommitIndeterminate,
			Op:      "validate installed Codex lease journal v2",
			Err:     errors.Join(ErrCodexLeaseTrustLost, err),
		}
	}
	store.v2 = &installedEnvelope
	store.generation = installedEnvelope.Generation
	store.journalBytes = append([]byte(nil), installed...)
	store.journalID = installedID
	store.legacyArchive = archiveName
	store.legacyArchiveID = archiveID
	store.legacyArchiveBytes = append([]byte(nil), archive...)
	return nil
}

func (store *CodexLeaseStore) migrateV2Locked(journal []byte) error {
	if err := store.loadLegacyV2Locked(journal); err != nil {
		return err
	}
	if store.v2.Generation == math.MaxUint64 {
		return fmt.Errorf("%w: schema-v2 Codex lease generation overflow", ErrCodexLeaseTrustLost)
	}
	digest := codexLeaseSHA256(journal)
	archiveName := store.journalName + ".v2-" + digest + ".archive"
	archive, archiveID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
	switch {
	case err == nil:
		if !bytes.Equal(archive, journal) {
			return fmt.Errorf("%w: schema-v2 Codex lease archive mismatch", ErrCodexLeaseTrustLost)
		}
	case errors.Is(err, os.ErrNotExist):
		beforeArchive := func() error {
			if err := store.revalidateV2InstalledLocked(); err != nil {
				return err
			}
			file, openErr := store.directory.OpenNoFollow(archiveName)
			if openErr == nil {
				_ = file.Close()
				return fmt.Errorf("%w: schema-v2 archive appeared before replace", ErrCodexLeaseTrustLost)
			}
			if !errors.Is(openErr, os.ErrNotExist) {
				return openErr
			}
			return nil
		}
		if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, archiveName, journal, beforeArchive); err != nil {
			return fmt.Errorf("archive schema-v2 Codex lease journal: %w", err)
		}
		archive, archiveID, err = fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
		if err != nil || !bytes.Equal(archive, journal) {
			cause := err
			if cause == nil {
				cause = errors.New("installed schema-v2 archive bytes differ")
			}
			return &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "verify schema-v2 Codex lease archive", Err: errors.Join(ErrCodexLeaseTrustLost, cause)}
		}
	default:
		return fmt.Errorf("%w: read schema-v2 Codex lease archive: %v", ErrCodexLeaseTrustLost, err)
	}

	next := cloneCodexLeaseV2Envelope(*store.v2)
	next.Version = codexLeaseJournalVersionV3
	next.Cutover.CompatibilityEpoch = compat.CurrentEpoch
	for index := range next.Records {
		next.Records[index].ProtocolSchema = CurrentCodexLeaseSchema
	}
	beforeJournal := func() error {
		if err := store.revalidateV2InstalledLocked(); err != nil {
			return err
		}
		got, gotID, readErr := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
		if readErr != nil || gotID != archiveID || !bytes.Equal(got, journal) {
			return fmt.Errorf("%w: schema-v2 archive changed before cutover", ErrCodexLeaseTrustLost)
		}
		return nil
	}
	return store.installV3MigrationLocked(next, beforeJournal)
}

func (store *CodexLeaseStore) loadLegacyV2Locked(journal []byte) error {
	var envelope codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(journal, &envelope); err != nil {
		return fmt.Errorf("%w: decode legacy Codex lease journal v2: %v", ErrCodexLeaseTrustLost, err)
	}
	if err := store.validateLegacyV2Envelope(envelope); err != nil {
		return err
	}
	if err := store.validateCodexLeaseV2StateForSchema(envelope, codexLeaseJournalVersionV2); err != nil {
		return fmt.Errorf("%w: invalid legacy Codex lease journal v2: %v", ErrCodexLeaseTrustLost, err)
	}
	store.v2 = &envelope
	store.generation = envelope.Generation
	return store.loadCodexLeaseLegacyArchiveLocked(envelope)
}

func (store *CodexLeaseStore) loadV2Locked(journal []byte) error {
	var envelope codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(journal, &envelope); err != nil {
		return fmt.Errorf("%w: decode Codex lease journal v2: %v", ErrCodexLeaseTrustLost, err)
	}
	if err := store.validateV2Envelope(envelope); err != nil {
		return err
	}
	store.v2 = &envelope
	store.generation = envelope.Generation
	return store.loadCodexLeaseLegacyArchiveLocked(envelope)
}

func (store *CodexLeaseStore) loadCodexLeaseLegacyArchiveLocked(envelope codexLeaseJournalEnvelopeV2) error {
	if envelope.Cutover.SourceVersion == 1 {
		archiveName := store.journalName + ".v1-" + envelope.Cutover.LegacyV1SHA256 + ".archive"
		archive, archiveID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, archiveName, codexLeaseJournalMaxBytes)
		if err != nil || codexLeaseSHA256(archive) != envelope.Cutover.LegacyV1SHA256 || store.validateLegacyArchive(archive, envelope.Cutover.JournalGeneration-1) != nil {
			return fmt.Errorf("%w: validate legacy Codex lease archive: %v", ErrCodexLeaseTrustLost, err)
		}
		var legacy codexLeaseJournalEnvelope
		if err := decodeCodexLeaseV1StrictJSON(archive, &legacy); err != nil {
			return fmt.Errorf("%w: decode legacy Codex lease archive: %v", ErrCodexLeaseTrustLost, err)
		}
		authoritative, shadow := codexLeaseV1Epochs(legacy.Records)
		if !equalCodexEpochs(authoritative, envelope.Cutover.AuthoritativeModeEpochs) || !equalCodexEpochs(shadow, envelope.Cutover.ShadowModeEpochs) {
			return fmt.Errorf("%w: legacy Codex lease archive mode epochs changed", ErrCodexLeaseTrustLost)
		}
		store.legacyArchive = archiveName
		store.legacyArchiveID = archiveID
		store.legacyArchiveBytes = append([]byte(nil), archive...)
	}
	return nil
}

func (store *CodexLeaseStore) revalidateV1SourceLocked(want []byte, generation uint64) error {
	if err := store.revalidateDirectoryLocked(); err != nil {
		return err
	}
	key, keyID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.keyName, codexLeaseHMACKeyBytes)
	defer clear(key)
	if err != nil || keyID != store.keyID || !bytes.Equal(key, store.key) {
		return fmt.Errorf("%w: Codex lease key replaced", ErrCodexLeaseTrustLost)
	}
	journal, journalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || journalID != store.journalID || !bytes.Equal(journal, want) {
		return fmt.Errorf("%w: Codex lease journal replaced", ErrCodexLeaseTrustLost)
	}
	var envelope codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(journal, &envelope); err != nil || envelope.Generation != generation || !store.validEnvelopeMAC(envelope) {
		return fmt.Errorf("%w: legacy Codex lease authority changed", ErrCodexLeaseTrustLost)
	}
	return nil
}

func (store *CodexLeaseStore) revalidateV2InstalledLocked() error {
	if err := store.revalidateDirectoryLocked(); err != nil {
		return err
	}
	key, keyID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.keyName, codexLeaseHMACKeyBytes)
	defer clear(key)
	if err != nil || keyID != store.keyID || !bytes.Equal(key, store.key) {
		return fmt.Errorf("%w: Codex lease key replaced", ErrCodexLeaseTrustLost)
	}
	journal, journalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || journalID != store.journalID || !bytes.Equal(journal, store.journalBytes) {
		return fmt.Errorf("%w: Codex lease journal replaced", ErrCodexLeaseTrustLost)
	}
	if store.legacyArchive != "" {
		archive, archiveID, archiveErr := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.legacyArchive, codexLeaseJournalMaxBytes)
		if archiveErr != nil || archiveID != store.legacyArchiveID || !bytes.Equal(archive, store.legacyArchiveBytes) || codexLeaseSHA256(archive) != store.v2.Cutover.LegacyV1SHA256 || store.validateLegacyArchive(archive, store.v2.Cutover.JournalGeneration-1) != nil {
			return fmt.Errorf("%w: legacy Codex lease archive replaced", ErrCodexLeaseTrustLost)
		}
	}
	return nil
}

func (store *CodexLeaseStore) commitV2Locked(expectedGeneration uint64, envelope codexLeaseJournalEnvelopeV2) error {
	return store.commitV2LockedWithPrecondition(expectedGeneration, envelope, store.revalidateV2InstalledLocked)
}

func (store *CodexLeaseStore) commitV2LockedWithPrecondition(expectedGeneration uint64, envelope codexLeaseJournalEnvelopeV2, beforeWrite func() error) error {
	if store.v2 == nil || store.closed {
		return ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if expectedGeneration != store.v2.Generation || expectedGeneration == math.MaxUint64 {
		return fmt.Errorf("%w: journal generation have %d expected %d", ErrCodexLeaseStaleMutation, store.v2.Generation, expectedGeneration)
	}
	envelope = cloneCodexLeaseV2Envelope(envelope)
	envelope.Version = codexLeaseJournalVersionV3
	envelope.HashVersion = codexLeaseHashVersion
	envelope.Generation = expectedGeneration + 1
	canonicaliseCodexLeaseV2(&envelope)
	data, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return err
	}
	if _, err := store.decodeAndValidateV2Candidate(data); err != nil {
		return fmt.Errorf("validate Codex lease journal candidate: %w", err)
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, data, beforeWrite); err != nil {
		if fsutil.AtomicWriteOutcome(err) == fsutil.CommitIndeterminate || errors.Is(err, ErrCodexLeaseTrustLost) {
			store.poisoned = err
		}
		return err
	}
	installed, installedID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || !bytes.Equal(installed, data) {
		uncertain := fmt.Errorf("%w: verify committed Codex lease journal: %v", fsutil.ErrCommitIndeterminate, err)
		store.poisoned = uncertain
		return uncertain
	}
	installedEnvelope, err := store.decodeAndValidateV2Candidate(installed)
	if err != nil {
		uncertain := fmt.Errorf("%w: validate committed Codex lease journal", fsutil.ErrCommitIndeterminate)
		store.poisoned = uncertain
		return uncertain
	}
	store.v2 = &installedEnvelope
	store.generation = installedEnvelope.Generation
	store.journalBytes = append([]byte(nil), installed...)
	store.journalID = installedID
	return nil
}

func (store *CodexLeaseStore) installV3MigrationLocked(envelope codexLeaseJournalEnvelopeV2, beforeWrite func() error) error {
	if store.v2 == nil || store.closed {
		return ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	envelope = cloneCodexLeaseV2Envelope(envelope)
	envelope.Version = codexLeaseJournalVersionV3
	envelope.HashVersion = codexLeaseHashVersion
	envelope.Generation = store.v2.Generation
	canonicaliseCodexLeaseV2(&envelope)
	data, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return err
	}
	if _, err := store.decodeAndValidateV2Candidate(data); err != nil {
		return fmt.Errorf("validate Codex lease schema migration candidate: %w", err)
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, data, beforeWrite); err != nil {
		return err
	}
	installed, installedID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || !bytes.Equal(installed, data) {
		return &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "verify migrated Codex lease journal", Err: err}
	}
	installedEnvelope, err := store.decodeAndValidateV2Candidate(installed)
	if err != nil {
		return &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "validate migrated Codex lease journal", Err: err}
	}
	store.v2 = &installedEnvelope
	store.generation = installedEnvelope.Generation
	store.journalBytes = append([]byte(nil), installed...)
	store.journalID = installedID
	return nil
}

func (store *CodexLeaseStore) decodeAndValidateV2Candidate(data []byte) (codexLeaseJournalEnvelopeV2, error) {
	var envelope codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(data, &envelope); err != nil {
		return codexLeaseJournalEnvelopeV2{}, err
	}
	if err := store.validateV2Envelope(envelope); err != nil {
		return codexLeaseJournalEnvelopeV2{}, err
	}
	if err := store.validateCodexLeaseV2State(envelope); err != nil {
		return codexLeaseJournalEnvelopeV2{}, err
	}
	return envelope, nil
}

func (store *CodexLeaseStore) normaliseRestoredV2Locked() error {
	if store.v2 == nil || store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return nil
	}
	next := cloneCodexLeaseV2Envelope(*store.v2)
	now := store.policy.Now().UTC()
	changed := false
	for recordIndex := range next.Records {
		record := &next.Records[recordIndex]
		recordChanged := false
		leaseChanged := false
		attemptUncertain := false
		attemptAbandoned := false
		if !record.SocketLineageExtinct || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 {
			record.SocketLineageExtinct = true
			record.RoutingRefs = 0
			record.AttemptRefs = 0
			record.ResponseObserverRefs = 0
			leaseChanged = true
			recordChanged = true
		}
		for attemptIndex := range record.Attempts {
			attempt := &record.Attempts[attemptIndex]
			if attempt.Generation != record.CurrentAttemptGeneration {
				continue
			}
			switch attempt.State {
			case CodexAttemptPrepared:
				attempt.State = CodexAttemptAbandonedBeforeDispatch
				attemptAbandoned = true
			case CodexAttemptDispatched, CodexAttemptStreaming:
				attempt.State = CodexAttemptIndeterminate
				attemptUncertain = true
				if !record.NonMigratable {
					record.NonMigratable = true
					leaseChanged = true
				}
			default:
				continue
			}
			attempt.Revision++
			attempt.LastObservedAt = monotonicCodexLeaseTime(now, attempt.LastObservedAt)
			recordChanged = true
		}
		switch {
		case record.State == LeaseReserving:
			record.State = LeaseFailedUnadmitted
			leaseChanged = true
			recordChanged = true
			for laneIndex := range next.Lanes {
				lane := &next.Lanes[laneIndex]
				if !codexLeaseRecordInLane(*record, *lane) || !codexLeaseLaneCurrentMatchesRecord(*lane, *record) {
					continue
				}
				lane.CurrentTurnHash = ""
				lane.CurrentModeEpoch = 0
				lane.CurrentAuthoritative = false
				lane.Generation++
				lane.LastObservedAt = monotonicCodexLeaseTime(now, lane.LastObservedAt)
			}
		case attemptUncertain ||
			(attemptAbandoned && record.EverAdmitted) ||
			record.State == LeaseContinuationPending || record.State == LeaseBoundQuiescent:
			record.State = LeaseOrphaned
			leaseChanged = true
			recordChanged = true
		}
		if leaseChanged {
			record.LeaseGeneration++
		}
		if recordChanged {
			record.RecordGeneration++
			record.LastObservedAt = monotonicCodexLeaseTime(now, record.LastObservedAt)
			for laneIndex := range next.Lanes {
				lane := &next.Lanes[laneIndex]
				if codexLeaseRecordInLane(*record, *lane) {
					lane.LastObservedAt = monotonicCodexLeaseTime(record.LastObservedAt, lane.LastObservedAt)
				}
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := store.validateCodexLeaseV2CandidateLocked(next); err != nil {
		return fmt.Errorf("%w: invalid restored Codex lease normalisation: %v", ErrCodexLeaseTrustLost, err)
	}
	return store.commitV2Locked(store.v2.Generation, next)
}

func monotonicCodexLeaseTime(now, prior time.Time) time.Time {
	if now.Before(prior) {
		return prior.UTC()
	}
	return now.UTC()
}

func codexLeaseRecordInLane(record CodexJournalRecordV2, lane CodexJournalLane) bool {
	return record.SessionHash == lane.SessionHash && record.ThreadHash == lane.ThreadHash && record.NamespaceHash == lane.NamespaceHash
}

func codexLeaseLaneCurrentMatchesRecord(lane CodexJournalLane, record CodexJournalRecordV2) bool {
	return lane.CurrentTurnHash == record.TurnHash && lane.CurrentModeEpoch == record.ModeEpoch && lane.CurrentAuthoritative == record.Authoritative
}

func (store *CodexLeaseStore) revalidateDirectoryLocked() error {
	identity, err := codexLeaseDirectoryIdentity(store.inspector, store.directory, store.directoryPath)
	if err != nil || !sameCodexLeaseObject(identity, store.directoryID) {
		return fmt.Errorf("%w: Codex lease directory replaced", ErrCodexLeaseTrustLost)
	}
	return nil
}

func (store *CodexLeaseStore) marshalV2Envelope(envelope codexLeaseJournalEnvelopeV2) ([]byte, error) {
	envelope = cloneCodexLeaseV2Envelope(envelope)
	canonicaliseCodexLeaseV2(&envelope)
	mac, err := store.v2EnvelopeMAC(envelope)
	if err != nil {
		return nil, err
	}
	envelope.MAC = mac
	return json.MarshalIndent(envelope, "", "  ")
}

func (store *CodexLeaseStore) prepareV2Envelope(envelope codexLeaseJournalEnvelopeV2) (codexLeaseJournalEnvelopeV2, []byte, error) {
	data, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, nil, err
	}
	var signed codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(data, &signed); err != nil {
		return codexLeaseJournalEnvelopeV2{}, nil, err
	}
	if err := store.validateV2Envelope(signed); err != nil {
		return codexLeaseJournalEnvelopeV2{}, nil, err
	}
	if err := store.validateCodexLeaseV2State(signed); err != nil {
		return codexLeaseJournalEnvelopeV2{}, nil, err
	}
	return signed, data, nil
}

func (store *CodexLeaseStore) validateV2Envelope(envelope codexLeaseJournalEnvelopeV2) error {
	return store.validateCodexLeaseEnvelope(envelope, codexLeaseJournalVersionV3, compat.CurrentEpoch, CurrentCodexLeaseSchema, true)
}

func (store *CodexLeaseStore) validateLegacyV2Envelope(envelope codexLeaseJournalEnvelopeV2) error {
	return store.validateCodexLeaseEnvelope(envelope, codexLeaseJournalVersionV2, 3, codexLeaseJournalVersionV2, false)
}

func (store *CodexLeaseStore) validateCodexLeaseEnvelope(envelope codexLeaseJournalEnvelopeV2, journalVersion, compatibilityEpoch, protocolSchema int, cacheFieldsAllowed bool) error {
	if envelope.Version != journalVersion || envelope.HashVersion != codexLeaseHashVersion {
		return fmt.Errorf("%w: unsupported Codex lease schema/hash version", ErrCodexLeaseTrustLost)
	}
	if envelope.Cutover.CompatibilityEpoch != compatibilityEpoch || envelope.Generation < envelope.Cutover.JournalGeneration || envelope.Cutover.JournalGeneration == 0 || envelope.Cutover.At.IsZero() || !codexLeaseUTCTime(envelope.Cutover.At) {
		return fmt.Errorf("%w: invalid Codex lease cutover tuple", ErrCodexLeaseTrustLost)
	}
	if !store.validV2EnvelopeMAC(envelope) {
		return fmt.Errorf("%w: Codex lease journal v2 MAC mismatch", ErrCodexLeaseTrustLost)
	}
	if !validSortedCodexEpochs(envelope.Cutover.AuthoritativeModeEpochs) || !validSortedCodexEpochs(envelope.Cutover.ShadowModeEpochs) {
		return fmt.Errorf("%w: non-canonical Codex lease cutover epochs", ErrCodexLeaseTrustLost)
	}
	switch envelope.Cutover.SourceVersion {
	case 0:
		if envelope.Cutover.State != CodexLeaseCutoverComplete || !envelope.Cutover.NoLegacyAuthority || envelope.Cutover.CompletedAt != envelope.Cutover.At || !codexLeaseUTCTime(envelope.Cutover.CompletedAt) || envelope.Cutover.CompletionGeneration != envelope.Cutover.JournalGeneration || envelope.Cutover.LegacyV1SHA256 != "" || !envelope.Cutover.LegacyQuarantineUntil.IsZero() || len(envelope.Cutover.AuthoritativeModeEpochs) != 0 || len(envelope.Cutover.ShadowModeEpochs) != 0 {
			return fmt.Errorf("%w: invalid fresh-v2 cutover tuple", ErrCodexLeaseTrustLost)
		}
	case 1:
		if !validCodexLeaseSHA256(envelope.Cutover.LegacyV1SHA256) || !codexLeaseUTCTime(envelope.Cutover.LegacyQuarantineUntil) || !envelope.Cutover.LegacyQuarantineUntil.After(envelope.Cutover.At) {
			return fmt.Errorf("%w: invalid legacy cutover evidence", ErrCodexLeaseTrustLost)
		}
		if envelope.Cutover.State == CodexLeaseCutoverLegacyQuarantine {
			if envelope.Cutover.NoLegacyAuthority || !envelope.Cutover.CompletedAt.IsZero() || envelope.Cutover.CompletionGeneration != 0 || envelope.Generation != envelope.Cutover.JournalGeneration || len(envelope.Lanes) != 0 || len(envelope.Records) != 0 {
				return fmt.Errorf("%w: invalid legacy quarantine tuple", ErrCodexLeaseTrustLost)
			}
		} else if envelope.Cutover.State != CodexLeaseCutoverComplete || !envelope.Cutover.NoLegacyAuthority || envelope.Cutover.CompletedAt.IsZero() || !codexLeaseUTCTime(envelope.Cutover.CompletedAt) || envelope.Cutover.CompletedAt.Before(envelope.Cutover.LegacyQuarantineUntil) || envelope.Cutover.JournalGeneration == math.MaxUint64 || envelope.Cutover.CompletionGeneration != envelope.Cutover.JournalGeneration+1 || envelope.Cutover.CompletionGeneration > envelope.Generation {
			return fmt.Errorf("%w: invalid legacy completion tuple", ErrCodexLeaseTrustLost)
		}
	default:
		return fmt.Errorf("%w: unsupported Codex lease cutover source", ErrCodexLeaseTrustLost)
	}
	if !cacheFieldsAllowed {
		if envelope.AffinityInvalidationGeneration != 0 {
			return fmt.Errorf("%w: schema-v2 journal contains affinity invalidation", ErrCodexLeaseTrustLost)
		}
		for _, lane := range envelope.Lanes {
			if !lane.LastCacheAdmittedAt.IsZero() || lane.LastCacheEffectiveModel != "" || len(lane.RequestUnavailableAccountHashes) != 0 || len(lane.QuotaExhaustedAccountHashes) != 0 {
				return fmt.Errorf("%w: schema-v2 journal contains schema-v3 cache affinity", ErrCodexLeaseTrustLost)
			}
		}
		for _, record := range envelope.Records {
			if record.QuotaExhaustionProbe {
				return fmt.Errorf("%w: schema-v2 journal contains schema-v3 quota probe", ErrCodexLeaseTrustLost)
			}
		}
	}
	if envelope.AffinityInvalidationGeneration != 0 && (envelope.Cutover.State != CodexLeaseCutoverComplete || envelope.AffinityInvalidationGeneration <= envelope.Cutover.CompletionGeneration || envelope.AffinityInvalidationGeneration > envelope.Generation) {
		return fmt.Errorf("%w: invalid affinity invalidation generation", ErrCodexLeaseTrustLost)
	}
	if err := store.validateV2SemanticStateForSchema(envelope, protocolSchema); err != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseTrustLost, err)
	}
	return nil
}

func (store *CodexLeaseStore) validateV2SemanticState(envelope codexLeaseJournalEnvelopeV2) error {
	return store.validateV2SemanticStateForSchema(envelope, CurrentCodexLeaseSchema)
}

func (store *CodexLeaseStore) validateV2SemanticStateForSchema(envelope codexLeaseJournalEnvelopeV2, protocolSchema int) error {
	laneIndexes := make(map[string]int, len(envelope.Lanes))
	for index, lane := range envelope.Lanes {
		if !validCodexLeaseDigest(lane.SessionHash) || !validCodexLeaseDigest(lane.ThreadHash) || lane.NamespaceHash != store.hash("namespace", CodexResponsesNamespace) {
			return errors.New("invalid lane identity hash")
		}
		if lane.Generation == 0 || lane.Generation > envelope.Generation || lane.LastObservedAt.IsZero() || !codexLeaseUTCTime(lane.LastObservedAt) {
			return errors.New("invalid lane generation or timestamp")
		}
		for hashIndex, accountHash := range lane.RequestUnavailableAccountHashes {
			if !validCodexLeaseDigest(accountHash) || (hashIndex != 0 && lane.RequestUnavailableAccountHashes[hashIndex-1] >= accountHash) {
				return errors.New("invalid or non-canonical request-unavailable accounts")
			}
		}
		for hashIndex, accountHash := range lane.QuotaExhaustedAccountHashes {
			if !validCodexLeaseDigest(accountHash) || (hashIndex != 0 && lane.QuotaExhaustedAccountHashes[hashIndex-1] >= accountHash) {
				return errors.New("invalid or non-canonical quota-exhausted accounts")
			}
		}
		if index > 0 && !codexJournalLaneLess(envelope.Lanes[index-1], lane) {
			return errors.New("duplicate or non-canonical lane identity")
		}
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		if _, duplicate := laneIndexes[laneDigest]; duplicate {
			return errors.New("duplicate lane identity")
		}
		laneIndexes[laneDigest] = index
		if lane.CurrentTurnHash == "" {
			if lane.CurrentModeEpoch != 0 || lane.CurrentAuthoritative {
				return errors.New("empty current lane tuple has authority fields")
			}
		} else {
			if !validCodexLeaseDigest(lane.CurrentTurnHash) || lane.CurrentModeEpoch == 0 || lane.CurrentTurnHash != lane.LastTurnHash || lane.CurrentModeEpoch != lane.LastModeEpoch || lane.CurrentAuthoritative != lane.LastAuthoritative {
				return errors.New("current and last lane tuples disagree")
			}
		}
		if lane.LastTurnHash == "" {
			if lane.LastModeEpoch != 0 || lane.LastAuthoritative || lane.CurrentTurnHash != "" {
				return errors.New("empty last lane tuple has authority fields")
			}
		} else if !validCodexLeaseDigest(lane.LastTurnHash) || lane.LastModeEpoch == 0 {
			return errors.New("invalid last lane tuple")
		}
	}

	recordIndexes := make(map[CodexJournalRecordIdentity]int, len(envelope.Records))
	for index, record := range envelope.Records {
		if index > 0 && !codexJournalRecordLess(envelope.Records[index-1], record) {
			return errors.New("duplicate or non-canonical record identity")
		}
		identity := record.Identity()
		if _, duplicate := recordIndexes[identity]; duplicate {
			return errors.New("duplicate record identity")
		}
		recordIndexes[identity] = index
		laneIndex, ok := laneIndexes[identity.LaneDigest]
		if !ok {
			return errors.New("record references absent lane")
		}
		lane := envelope.Lanes[laneIndex]
		if err := store.validateV2Record(envelope, lane, record, protocolSchema); err != nil {
			return err
		}
	}

	for _, lane := range envelope.Lanes {
		if lane.LastTurnHash == "" {
			return errors.New("lane has no retained last record")
		}
		last := CodexJournalRecordIdentity{
			LaneDigest:    codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash),
			TurnDigest:    lane.LastTurnHash,
			ModeEpoch:     lane.LastModeEpoch,
			Authoritative: lane.LastAuthoritative,
		}
		lastIndex, ok := recordIndexes[last]
		if !ok || envelope.Records[lastIndex].LastObservedAt.After(lane.LastObservedAt) {
			return errors.New("lane last tuple has no matching lifecycle record")
		}
		if lane.CurrentTurnHash != "" {
			current := CodexJournalRecordIdentity{
				LaneDigest:    last.LaneDigest,
				TurnDigest:    lane.CurrentTurnHash,
				ModeEpoch:     lane.CurrentModeEpoch,
				Authoritative: lane.CurrentAuthoritative,
			}
			currentIndex, ok := recordIndexes[current]
			if !ok || envelope.Records[currentIndex].LastObservedAt.After(lane.LastObservedAt) {
				return errors.New("lane current tuple has no matching lifecycle record")
			}
			state := envelope.Records[currentIndex].State
			if state == LeaseFailedUnadmitted || state == LeaseSuperseded || state == LeaseExpired {
				return errors.New("terminal stale record is current")
			}
		}
	}

	for _, record := range envelope.Records {
		if record.PredecessorTurnHash == "" {
			continue
		}
		predecessor := CodexJournalRecordIdentity{
			LaneDigest:    record.Identity().LaneDigest,
			TurnDigest:    record.PredecessorTurnHash,
			ModeEpoch:     record.PredecessorModeEpoch,
			Authoritative: record.PredecessorAuthoritative,
		}
		predecessorIndex, ok := recordIndexes[predecessor]
		if predecessor == record.Identity() || (ok && envelope.Records[predecessorIndex].RecordGeneration != record.PredecessorGeneration) {
			return errors.New("invalid predecessor generation link")
		}
	}
	return store.validateCodexLeaseAdmissionEvidence(envelope)
}

func (store *CodexLeaseStore) validateV2Record(envelope codexLeaseJournalEnvelopeV2, lane CodexJournalLane, record CodexJournalRecordV2, protocolSchema int) error {
	if !validCodexLeaseDigest(record.SessionHash) || !validCodexLeaseDigest(record.ThreadHash) || record.NamespaceHash != store.hash("namespace", CodexResponsesNamespace) || !validCodexLeaseDigest(record.TurnHash) {
		return errors.New("invalid record identity hash")
	}
	for _, digest := range []string{record.AccountHash, record.PredecessorTurnHash, record.CorrelationHash, record.TurnStateHash, record.RequestedModelHash, record.DispatchPermitDigest} {
		if digest != "" && !validCodexLeaseDigest(digest) {
			return errors.New("invalid optional record hash")
		}
	}
	if record.HasResponseAnchor != (record.CorrelationHash != "") || record.HasTurnState != (record.TurnStateHash != "") {
		return errors.New("continuation hash presence flag mismatch")
	}
	if record.AdoptedPrewarm != (record.PrewarmAdoptionJournalGeneration != 0) {
		return errors.New("partial prewarm adoption marker")
	}
	if record.AdoptedPrewarm && (!record.Authoritative || !record.NonMigratable || record.PrewarmAdoptionJournalGeneration <= envelope.Cutover.CompletionGeneration || record.PrewarmAdoptionJournalGeneration > envelope.Generation) {
		return errors.New("invalid prewarm adoption marker")
	}
	if record.RecordGeneration == 0 || record.RecordGeneration > envelope.Generation || record.LaneGeneration == 0 || record.LaneGeneration > lane.Generation || record.LeaseGeneration == 0 || record.LeaseGeneration > record.RecordGeneration || record.ModeEpoch == 0 || record.ProtocolSchema != protocolSchema || record.RoutingRefs < 0 || record.AttemptRefs < 0 || record.ResponseObserverRefs < 0 {
		return errors.New("invalid record generation, schema, or reference count")
	}
	if record.SocketLineageExtinct && (record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0) {
		return errors.New("extinct socket lineage has live references")
	}
	if record.CreatedAt.IsZero() || record.LastObservedAt.IsZero() || !codexLeaseUTCTime(record.CreatedAt) || !codexLeaseUTCTime(record.LastObservedAt) || record.CreatedAt.Before(envelope.Cutover.CompletedAt) || record.LastObservedAt.Before(record.CreatedAt) || record.LastObservedAt.After(lane.LastObservedAt) {
		return errors.New("invalid record lifecycle timestamps")
	}
	if record.PredecessorTurnHash == "" {
		if record.PredecessorModeEpoch != 0 || record.PredecessorAuthoritative || record.PredecessorGeneration != 0 {
			return errors.New("predecessor fields present without predecessor")
		}
	} else if record.PredecessorModeEpoch == 0 || record.PredecessorGeneration == 0 {
		return errors.New("incomplete predecessor tuple")
	}
	if !validCodexLeaseState(record.State) || (record.Generation != 0 && !validCodexLeaseRequest(record.RequestKind, record.CompactionPhase)) {
		return fmt.Errorf("unsupported lease state, request kind, or compaction phase: state=%s request_generation=%d kind=%q phase=%q", record.State, record.Generation, record.RequestKind, record.CompactionPhase)
	}
	if err := store.validateV2RouteAndAttempts(record); err != nil {
		return err
	}
	return nil
}

func (store *CodexLeaseStore) validateV2RouteAndAttempts(record CodexJournalRecordV2) error {
	if record.Generation == 0 {
		if record.AccountHash != "" || !codexCurrentRequestIsZero(record.CodexCurrentRequest) || (record.State != LeaseReserving && record.State != LeaseFailedUnadmitted) || record.EverAdmitted {
			return errors.New("zero request generation carries request authority")
		}
		return nil
	}
	hasRoute := record.AccountHash != "" || record.RequestedModelHash != "" || record.EffectiveModel != "" || len(record.RequiredBuckets) != 0
	if hasRoute {
		if !validCodexLeaseDigest(record.AccountHash) || !validCodexLeaseDigest(record.RequestedModelHash) || record.EffectiveModel == "" || strings.TrimSpace(record.EffectiveModel) != record.EffectiveModel || !validCodexLeaseBuckets(record.RequiredBuckets, record.EffectiveModel) {
			return errors.New("invalid persisted route choice")
		}
	} else if record.AccountHash != "" || record.RequestedModelHash != "" || record.EffectiveModel != "" || len(record.RequiredBuckets) != 0 {
		return errors.New("partial persisted route choice")
	}

	envelope := record.AttemptEnvelope
	if len(envelope.Slots) == 0 {
		if envelope.PolicyVersion != 0 || envelope.PlanDigest != "" || envelope.AttemptLimit != 0 || record.QuotaExhaustionProbe || len(record.Attempts) != 0 || record.CurrentAttemptGeneration != 0 {
			return errors.New("attempt metadata exists without a frozen envelope")
		}
		if record.State != LeaseReserving && record.State != LeaseFailedUnadmitted {
			return errors.New("routable lease is missing an attempt envelope")
		}
		if record.State == LeaseReserving && hasRoute {
			return errors.New("reserving lease already has a route")
		}
		return nil
	}
	if !hasRoute || envelope.PolicyVersion != CodexLeaseAttemptPolicyVersion || envelope.AttemptLimit == 0 || int(envelope.AttemptLimit) != len(envelope.Slots) {
		return errors.New("invalid frozen attempt envelope")
	}
	for index, slot := range envelope.Slots {
		if slot.Index != uint32(index+1) || !validCodexLeaseDigest(slot.AccountHash) || !validCodexLeaseDigest(slot.CandidateHash) || (slot.Kind != CodexAttemptSlotDirect && slot.Kind != CodexAttemptSlotEligibleManagedRefresh) {
			return errors.New("invalid attempt slot")
		}
		if record.QuotaExhaustionProbe && !constantTimeCodexLeaseDigestEqual(slot.AccountHash, envelope.Slots[0].AccountHash) {
			return errors.New("quota exhaustion probe spans multiple accounts")
		}
	}
	if !validCodexLeaseDigest(envelope.PlanDigest) || !constantTimeCodexLeaseDigestEqual(envelope.PlanDigest, codexLeaseAttemptPlanDigest(store.key, envelope.Slots)) {
		return errors.New("attempt plan digest mismatch")
	}
	if len(record.Attempts) == 0 || len(record.Attempts) > len(envelope.Slots) || record.CurrentAttemptGeneration == 0 {
		return errors.New("attempt envelope has no current attempt")
	}
	usedSlots := make(map[uint32]struct{})
	for index, attempt := range record.Attempts {
		if attempt.Generation == 0 || attempt.Generation != uint64(index+1) || attempt.Revision == 0 || attempt.Revision > record.RecordGeneration || attempt.Slot == 0 || attempt.Slot > envelope.AttemptLimit || !validCodexLeaseAttemptState(attempt.State) {
			return errors.New("invalid attempt generation, revision, slot, or state")
		}
		if attempt.CreatedAt.IsZero() || attempt.LastObservedAt.IsZero() || !codexLeaseUTCTime(attempt.CreatedAt) || !codexLeaseUTCTime(attempt.LastObservedAt) || attempt.CreatedAt.Before(record.CreatedAt) || attempt.LastObservedAt.Before(attempt.CreatedAt) || attempt.LastObservedAt.After(record.LastObservedAt) {
			return errors.New("invalid attempt lifecycle timestamps")
		}
		if _, duplicate := usedSlots[attempt.Slot]; duplicate {
			return errors.New("attempt slot used more than once")
		}
		usedSlots[attempt.Slot] = struct{}{}
		if index < len(record.Attempts)-1 && attempt.State != CodexAttemptProviderFailed && attempt.State != CodexAttemptAccountUnavailable {
			return errors.New("historical attempt is not a failed pre-admission route")
		}
		if index > 0 && !constantTimeCodexLeaseDigestEqual(envelope.Slots[attempt.Slot-1].AccountHash, envelope.Slots[record.Attempts[index-1].Slot-1].AccountHash) && record.Attempts[index-1].State != CodexAttemptAccountUnavailable && ((record.EverAdmitted && record.Generation > record.AdmissionRequestGeneration) || record.Attempts[index-1].State != CodexAttemptProviderFailed) {
			return errors.New("attempt changed account without account unavailability")
		}
	}
	current := record.Attempts[len(record.Attempts)-1]
	pendingHardRebind := len(record.Attempts) > 1 && !record.NonMigratable &&
		record.Attempts[len(record.Attempts)-2].State == CodexAttemptAccountUnavailable &&
		(current.State == CodexAttemptPrepared || current.State == CodexAttemptDispatched || current.State == CodexAttemptAccountUnavailable)
	pendingFullCreateRebind := record.EverAdmitted && !record.NonMigratable &&
		(current.State == CodexAttemptPrepared || current.State == CodexAttemptDispatched || current.State == CodexAttemptAbandonedBeforeDispatch || current.State == CodexAttemptAccountUnavailable) &&
		!constantTimeCodexLeaseDigestEqual(envelope.Slots[current.Slot-1].AccountHash, record.AccountHash)
	pendingIndeterminateFullCreateRebind := record.EverAdmitted && record.NonMigratable && current.State == CodexAttemptIndeterminate &&
		!constantTimeCodexLeaseDigestEqual(envelope.Slots[current.Slot-1].AccountHash, record.AccountHash)
	if record.CurrentAttemptGeneration != current.Generation || (!constantTimeCodexLeaseDigestEqual(envelope.Slots[current.Slot-1].AccountHash, record.AccountHash) && !pendingHardRebind && !pendingFullCreateRebind && !pendingIndeterminateFullCreateRebind) {
		return errors.New("current attempt does not match latest persisted route")
	}
	if current.State == CodexAttemptIndeterminate && !record.NonMigratable {
		return errors.New("indeterminate request remains migratable")
	}
	switch record.State {
	case LeaseReserving:
		return errors.New("reserving lease has attempts")
	case LeaseProvisional:
		if current.State != CodexAttemptPrepared && current.State != CodexAttemptDispatched && current.State != CodexAttemptAbandonedBeforeDispatch {
			return errors.New("provisional lease has admitted or terminal current attempt")
		}
	case LeaseBoundActive:
		if current.State != CodexAttemptStreaming && (!record.EverAdmitted || (current.State != CodexAttemptPrepared && current.State != CodexAttemptDispatched)) {
			return errors.New("bound active lease has invalid current attempt")
		}
	case LeaseContinuationPending, LeaseBoundQuiescent:
		if current.State != CodexAttemptProviderCompleted && current.State != CodexAttemptProviderFailed && current.State != CodexAttemptAccountUnavailable {
			return errors.New("quiescent lease has non-terminal current attempt")
		}
	case LeaseOrphaned:
		if current.State != CodexAttemptIndeterminate && current.State != CodexAttemptProviderCompleted && current.State != CodexAttemptProviderFailed && current.State != CodexAttemptAbandonedBeforeDispatch && current.State != CodexAttemptAccountUnavailable {
			return errors.New("orphaned lease has replayable current attempt")
		}
		if record.Authoritative && current.State != CodexAttemptIndeterminate && !record.EverAdmitted {
			return errors.New("authoritative terminal orphan lacks admission evidence")
		}
	case LeaseFailedUnadmitted:
		if current.State == CodexAttemptPrepared || current.State == CodexAttemptDispatched || current.State == CodexAttemptStreaming {
			return errors.New("failed tombstone retains non-terminal attempt")
		}
	case LeaseSuperseded, LeaseExpired:
		if current.State == CodexAttemptPrepared || current.State == CodexAttemptDispatched || current.State == CodexAttemptStreaming {
			return errors.New("historical terminal lease retains live attempt")
		}
	}
	return nil
}

func codexLeaseUTCTime(value time.Time) bool {
	return !value.IsZero() && value == value.UTC()
}

func validSortedCodexEpochs(epochs []uint64) bool {
	for index, epoch := range epochs {
		if epoch == 0 || (index > 0 && epochs[index-1] >= epoch) {
			return false
		}
	}
	return true
}

func validCodexLeaseSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validCodexLeaseDigest(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func constantTimeCodexLeaseDigestEqual(left, right string) bool {
	leftBytes, leftErr := base64.RawURLEncoding.DecodeString(left)
	rightBytes, rightErr := base64.RawURLEncoding.DecodeString(right)
	return leftErr == nil && rightErr == nil && len(leftBytes) == sha256.Size && len(rightBytes) == sha256.Size && hmac.Equal(leftBytes, rightBytes)
}

func codexJournalLaneLess(left, right CodexJournalLane) bool {
	if left.SessionHash != right.SessionHash {
		return left.SessionHash < right.SessionHash
	}
	if left.ThreadHash != right.ThreadHash {
		return left.ThreadHash < right.ThreadHash
	}
	return left.NamespaceHash < right.NamespaceHash
}

func codexJournalRecordLess(left, right CodexJournalRecordV2) bool {
	if left.SessionHash != right.SessionHash {
		return left.SessionHash < right.SessionHash
	}
	if left.ThreadHash != right.ThreadHash {
		return left.ThreadHash < right.ThreadHash
	}
	if left.NamespaceHash != right.NamespaceHash {
		return left.NamespaceHash < right.NamespaceHash
	}
	if left.TurnHash != right.TurnHash {
		return left.TurnHash < right.TurnHash
	}
	if left.ModeEpoch != right.ModeEpoch {
		return left.ModeEpoch < right.ModeEpoch
	}
	return !left.Authoritative && right.Authoritative
}

func validCodexLeaseState(state LeaseState) bool {
	switch state {
	case LeaseReserving, LeaseProvisional, LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned, LeaseSuperseded, LeaseExpired, LeaseFailedUnadmitted:
		return true
	default:
		return false
	}
}

func validCodexLeaseAttemptState(state CodexAttemptState) bool {
	switch state {
	case CodexAttemptPrepared, CodexAttemptDispatched, CodexAttemptStreaming, CodexAttemptProviderCompleted, CodexAttemptProviderFailed, CodexAttemptIndeterminate, CodexAttemptAbandonedBeforeDispatch, CodexAttemptAccountUnavailable:
		return true
	default:
		return false
	}
}

func codexLeaseRestartableFailedHead(record CodexJournalRecordV2) bool {
	state := codexLeaseCurrentAttemptState(record)
	return record.Generation > 0 && record.State == LeaseFailedUnadmitted && !record.EverAdmitted &&
		record.RoutingRefs == 0 && record.AttemptRefs == 0 && record.ResponseObserverRefs == 0 && record.SocketLineageExtinct &&
		record.CurrentAttemptGeneration > 0 && (state == CodexAttemptProviderFailed || state == CodexAttemptAccountUnavailable)
}

func codexRestoredLaneRestartableFailedHead(restored CodexRestoredLane) (CodexRestoredRecord, bool) {
	if restored.Classification != CodexRestoredLaneHistorical || !restored.Fence.Current.IsZero() || restored.Fence.Last != restored.RequestedIdentity {
		return CodexRestoredRecord{}, false
	}
	for _, record := range restored.ResolvedRecords {
		if record.Identity == restored.RequestedIdentity && codexLeaseRestartableFailedHead(record.Record) {
			return record, true
		}
	}
	return CodexRestoredRecord{}, false
}

func validCodexLeaseRequest(kind CodexRequestKind, phase CodexCompactionPhase) bool {
	switch kind {
	case CodexRequestTurn, CodexRequestPrewarm:
		return phase == ""
	case CodexRequestCompaction:
		return phase == CodexCompactionStandalone || phase == CodexCompactionPreTurn || phase == CodexCompactionMidTurn
	default:
		return false
	}
}

func validCodexLeaseBuckets(buckets []CapacityBucket, effectiveModel string) bool {
	if len(buckets) == 0 || !canonicalCodexCapacityBuckets(buckets) {
		return false
	}
	required := CapacityBucketForModel(effectiveModel)
	for _, bucket := range buckets {
		if bucket == required {
			return true
		}
	}
	return false
}

func (store *CodexLeaseStore) v2EnvelopeMAC(envelope codexLeaseJournalEnvelopeV2) (string, error) {
	envelope = cloneCodexLeaseV2Envelope(envelope)
	canonicaliseCodexLeaseV2(&envelope)
	envelope.MAC = ""
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode Codex lease journal v2 MAC payload: %w", err)
	}
	mac := hmac.New(sha256.New, store.key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (store *CodexLeaseStore) validV2EnvelopeMAC(envelope codexLeaseJournalEnvelopeV2) bool {
	want, err := store.v2EnvelopeMAC(envelope)
	if err != nil {
		return false
	}
	wantBytes, wantOK := decodeCanonicalCodexLeaseMAC(want)
	gotBytes, gotOK := decodeCanonicalCodexLeaseMAC(envelope.MAC)
	return wantOK && gotOK && hmac.Equal(gotBytes, wantBytes)
}

func beginCodexLeaseOwnerOperation(owner CodexLeaseWriterAuthority) (*codex.CredentialOwnerOperation, error) {
	if owner == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if err := owner.AssertOwner(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCodexLeaseWriterUnavailable, err)
	}
	operation, err := owner.BeginOwnerOperation()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCodexLeaseWriterUnavailable, err)
	}
	if operation == nil {
		return nil, fmt.Errorf("%w: empty owner operation", ErrCodexLeaseWriterUnavailable)
	}
	return operation, nil
}

func codexLeasePathExists(inspector fsutil.SecurePathInspector, path string) (bool, error) {
	_, err := inspector.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func codexLeaseDirectoryIdentity(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, path string) (fsutil.SecureFileIdentity, error) {
	held, err := directory.Stat()
	if err != nil {
		return fsutil.SecureFileIdentity{}, err
	}
	heldID, ok := inspector.FileIdentity(held)
	if !ok {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return fsutil.SecureFileIdentity{}, err
	}
	pathID, ok := inspector.FileIdentity(pathInfo)
	if !ok || !sameCodexLeaseObject(pathID, heldID) {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return stableDirectoryObjectIdentity(heldID), nil
}

func codexLeaseV1Epochs(records []CodexJournalRecord) ([]uint64, []uint64) {
	authoritativeSet := make(map[uint64]struct{})
	shadowSet := make(map[uint64]struct{})
	for _, record := range records {
		if record.Authoritative {
			authoritativeSet[record.ModeEpoch] = struct{}{}
		} else {
			shadowSet[record.ModeEpoch] = struct{}{}
		}
	}
	return sortedCodexEpochSet(authoritativeSet), sortedCodexEpochSet(shadowSet)
}

func sortedCodexEpochSet(set map[uint64]struct{}) []uint64 {
	result := make([]uint64, 0, len(set))
	for epoch := range set {
		result = append(result, epoch)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func validCodexModeSnapshot(snapshot CodexModeAuthoritySnapshot) bool {
	return snapshot.RecognisedAuthoritativeEpochs != nil && validSortedCodexEpochs(snapshot.RecognisedAuthoritativeEpochs)
}

func cloneCodexModeSnapshot(snapshot CodexModeAuthoritySnapshot) CodexModeAuthoritySnapshot {
	if snapshot.RecognisedAuthoritativeEpochs != nil {
		epochs := make([]uint64, len(snapshot.RecognisedAuthoritativeEpochs))
		copy(epochs, snapshot.RecognisedAuthoritativeEpochs)
		snapshot.RecognisedAuthoritativeEpochs = epochs
	}
	return snapshot
}

func canonicaliseCodexLeaseV2(envelope *codexLeaseJournalEnvelopeV2) {
	if envelope == nil {
		return
	}
	if envelope.Lanes == nil {
		envelope.Lanes = []CodexJournalLane{}
	}
	if envelope.Records == nil {
		envelope.Records = []CodexJournalRecordV2{}
	}
	sort.Slice(envelope.Cutover.AuthoritativeModeEpochs, func(i, j int) bool {
		return envelope.Cutover.AuthoritativeModeEpochs[i] < envelope.Cutover.AuthoritativeModeEpochs[j]
	})
	sort.Slice(envelope.Cutover.ShadowModeEpochs, func(i, j int) bool {
		return envelope.Cutover.ShadowModeEpochs[i] < envelope.Cutover.ShadowModeEpochs[j]
	})
	sort.Slice(envelope.Lanes, func(i, j int) bool {
		left, right := envelope.Lanes[i], envelope.Lanes[j]
		if left.SessionHash != right.SessionHash {
			return left.SessionHash < right.SessionHash
		}
		if left.ThreadHash != right.ThreadHash {
			return left.ThreadHash < right.ThreadHash
		}
		return left.NamespaceHash < right.NamespaceHash
	})
	for index := range envelope.Lanes {
		if envelope.Lanes[index].RequestUnavailableAccountHashes == nil {
			envelope.Lanes[index].RequestUnavailableAccountHashes = []string{}
		}
		sort.Strings(envelope.Lanes[index].RequestUnavailableAccountHashes)
		if envelope.Lanes[index].QuotaExhaustedAccountHashes == nil {
			envelope.Lanes[index].QuotaExhaustedAccountHashes = []string{}
		}
		sort.Strings(envelope.Lanes[index].QuotaExhaustedAccountHashes)
	}
	sort.Slice(envelope.Records, func(i, j int) bool {
		left, right := envelope.Records[i], envelope.Records[j]
		if left.SessionHash != right.SessionHash {
			return left.SessionHash < right.SessionHash
		}
		if left.ThreadHash != right.ThreadHash {
			return left.ThreadHash < right.ThreadHash
		}
		if left.NamespaceHash != right.NamespaceHash {
			return left.NamespaceHash < right.NamespaceHash
		}
		if left.TurnHash != right.TurnHash {
			return left.TurnHash < right.TurnHash
		}
		if left.ModeEpoch != right.ModeEpoch {
			return left.ModeEpoch < right.ModeEpoch
		}
		return !left.Authoritative && right.Authoritative
	})
	for index := range envelope.Records {
		record := &envelope.Records[index]
		if record.AttemptEnvelope.Slots == nil {
			record.AttemptEnvelope.Slots = []CodexAttemptSlot{}
		}
		sort.Slice(record.AttemptEnvelope.Slots, func(i, j int) bool {
			return record.AttemptEnvelope.Slots[i].Index < record.AttemptEnvelope.Slots[j].Index
		})
		sort.Slice(record.Attempts, func(i, j int) bool {
			return record.Attempts[i].Generation < record.Attempts[j].Generation
		})
		sort.Slice(record.RequiredBuckets, func(i, j int) bool {
			return record.RequiredBuckets[i] < record.RequiredBuckets[j]
		})
	}
}

func cloneCodexLeaseV2Envelope(envelope codexLeaseJournalEnvelopeV2) codexLeaseJournalEnvelopeV2 {
	clone := envelope
	clone.Cutover.AuthoritativeModeEpochs = cloneCodexLeaseSlice(envelope.Cutover.AuthoritativeModeEpochs)
	clone.Cutover.ShadowModeEpochs = cloneCodexLeaseSlice(envelope.Cutover.ShadowModeEpochs)
	clone.Lanes = cloneCodexLeaseSlice(envelope.Lanes)
	for index := range envelope.Lanes {
		clone.Lanes[index].RequestUnavailableAccountHashes = cloneCodexLeaseSlice(envelope.Lanes[index].RequestUnavailableAccountHashes)
		clone.Lanes[index].QuotaExhaustedAccountHashes = cloneCodexLeaseSlice(envelope.Lanes[index].QuotaExhaustedAccountHashes)
	}
	clone.Records = make([]CodexJournalRecordV2, len(envelope.Records))
	for index, record := range envelope.Records {
		clone.Records[index] = record
		clone.Records[index].AttemptEnvelope.Slots = cloneCodexLeaseSlice(record.AttemptEnvelope.Slots)
		clone.Records[index].RequiredBuckets = cloneCodexLeaseSlice(record.RequiredBuckets)
		clone.Records[index].Attempts = cloneCodexLeaseSlice(record.Attempts)
	}
	return clone
}

func cloneCodexLeaseSlice[T any](source []T) []T {
	if source == nil {
		return nil
	}
	clone := make([]T, len(source))
	copy(clone, source)
	return clone
}

func sameCodexLeaseObject(left, right fsutil.SecureFileIdentity) bool {
	return fsutil.SameSecureObject(left, right)
}

func equalCodexEpochs(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func decodeCodexLeaseVersion(data []byte) (int, error) {
	if err := rejectCodexLeaseDuplicateJSONFields(data); err != nil {
		return 0, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return 0, err
	}
	versionData, ok := fields["version"]
	if !ok {
		return 0, errors.New("missing version")
	}
	var version int
	if err := json.Unmarshal(versionData, &version); err != nil {
		return 0, err
	}
	return version, nil
}

func decodeCodexLeaseStrictJSON(data []byte, destination any) error {
	if err := rejectCodexLeaseDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeCodexLeaseV2StrictJSON(data []byte, destination *codexLeaseJournalEnvelopeV2) error {
	if destination == nil {
		return errors.New("nil Codex lease v2 destination")
	}
	if err := decodeCodexLeaseStrictJSON(data, destination); err != nil {
		return err
	}
	original, err := decodeCodexLeaseJSONShape(data)
	if err != nil {
		return err
	}
	if codexLeaseJSONContainsNull(original) {
		return errors.New("Codex lease v2 JSON contains null")
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return err
	}
	if !bytes.Equal(compact.Bytes(), canonical) {
		return errors.New("Codex lease v2 JSON has non-canonical member order or encoding")
	}
	want, err := decodeCodexLeaseJSONShape(canonical)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(original, want) {
		return errors.New("Codex lease v2 JSON has non-canonical member presence")
	}
	return nil
}

func decodeCodexLeaseJSONShape(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func codexLeaseJSONContainsNull(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if codexLeaseJSONContainsNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if codexLeaseJSONContainsNull(item) {
				return true
			}
		}
	}
	return false
}

func rejectCodexLeaseDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanCodexLeaseJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanCodexLeaseJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make([]string, 0)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			for _, previous := range seen {
				if strings.EqualFold(previous, key) {
					return fmt.Errorf("duplicate field %q", key)
				}
			}
			for _, character := range key {
				if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
					return fmt.Errorf("non-canonical field %q", key)
				}
			}
			seen = append(seen, key)
			if err := scanCodexLeaseJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanCodexLeaseJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

func (store *CodexLeaseStore) validateLegacyArchive(data []byte, generation uint64) error {
	var envelope codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(data, &envelope); err != nil {
		return err
	}
	if envelope.Version != codexLeaseJournalVersion || envelope.Generation != generation || !store.validEnvelopeMAC(envelope) {
		return ErrCodexLeaseTrustLost
	}
	return nil
}

func codexLeaseSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
