package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexLeaseJournalVersion   = 1
	codexLeaseHMACKeyBytes     = 32
	codexLeaseJournalMaxBytes  = 16 << 20
	DefaultCodexLeaseRetention = 7 * 24 * time.Hour
)

type CodexJournalRecord struct {
	SessionHash       string     `json:"session_hash"`
	ThreadHash        string     `json:"thread_hash"`
	TurnHash          string     `json:"turn_hash"`
	NamespaceHash     string     `json:"namespace_hash"`
	AccountHash       string     `json:"account_hash"`
	CorrelationHash   string     `json:"correlation_hash,omitempty"`
	TurnStateHash     string     `json:"turn_state_hash,omitempty"`
	State             LeaseState `json:"state"`
	LeaseGeneration   uint64     `json:"lease_generation"`
	ModeEpoch         uint64     `json:"mode_epoch"`
	Authoritative     bool       `json:"authoritative"`
	ActiveRefs        int        `json:"active_refs,omitempty"`
	SocketGeneration  uint64     `json:"socket_generation,omitempty"`
	HasEncryptedState bool       `json:"has_encrypted_state,omitempty"`
	HasResponseAnchor bool       `json:"has_response_anchor,omitempty"`
	HasTurnState      bool       `json:"has_turn_state,omitempty"`
	NonMigratable     bool       `json:"non_migratable,omitempty"`
	LastSeen          time.Time  `json:"last_seen"`
}

type codexLeaseJournalEnvelope struct {
	Version    int                  `json:"version"`
	Generation uint64               `json:"generation"`
	Records    []CodexJournalRecord `json:"records"`
	MAC        string               `json:"mac"`
}

type CodexLeaseStore struct {
	lifecycle  sync.RWMutex
	mu         sync.Mutex
	fs         fsutil.DurableFileSystem
	path       string
	keyPath    string
	key        []byte
	generation uint64
	records    []CodexJournalRecord

	owner              CodexLeaseWriterAuthority
	inspector          fsutil.SecurePathInspector
	directory          fsutil.SecureDirectory
	directoryPath      string
	directoryID        fsutil.SecureFileIdentity
	journalName        string
	keyName            string
	keyID              fsutil.SecureFileIdentity
	journalID          fsutil.SecureFileIdentity
	journalBytes       []byte
	v2                 *codexLeaseJournalEnvelopeV2
	policy             CodexLeasePolicy
	modes              CodexModeAuthoritySnapshot
	legacyArchive      string
	legacyArchiveID    fsutil.SecureFileIdentity
	legacyArchiveBytes []byte
	poisoned           error
	closed             bool
}

func OpenCodexLeaseStore(fsys fsutil.DurableFileSystem, path, keyPath string) (*CodexLeaseStore, error) {
	if fsys == nil {
		return nil, errors.New("durable filesystem required for Codex lease journal")
	}
	if path == "" || keyPath == "" || filepath.Dir(path) != filepath.Dir(keyPath) {
		return nil, errors.New("Codex lease journal and key require one state directory")
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(path)
	if err := fsutil.EnsureSecureDirectory(fsys, directoryPath); err != nil {
		return nil, fmt.Errorf("secure Codex lease state directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, fmt.Errorf("open Codex lease state directory: %w", err)
	}
	defer directory.Close()
	journalName := filepath.Base(path)
	keyName := filepath.Base(keyPath)
	journalData, _, journalErr := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, journalName, codexLeaseJournalMaxBytes)
	journalExists := journalErr == nil
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read Codex lease journal: %w", journalErr)
	}
	store := &CodexLeaseStore{fs: fsys, path: path, keyPath: keyPath}
	key, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, keyName, codexLeaseHMACKeyBytes)
	if errors.Is(err, os.ErrNotExist) {
		if journalExists {
			return nil, errors.New("Codex lease HMAC key missing for existing journal")
		}
		key = make([]byte, codexLeaseHMACKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate Codex lease HMAC key: %w", err)
		}
		if err := fsutil.SecureAtomicWriteInDirectory(inspector, directory, keyName, key); err != nil {
			return nil, fmt.Errorf("persist Codex lease HMAC key: %w", err)
		}
		key, _, err = fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, keyName, codexLeaseHMACKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("verify Codex lease HMAC key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read Codex lease HMAC key: %w", err)
	}
	if len(key) != codexLeaseHMACKeyBytes {
		return nil, errors.New("invalid Codex lease HMAC key length")
	}
	store.key = append([]byte(nil), key...)
	if !journalExists {
		return store, nil
	}
	var envelope codexLeaseJournalEnvelope
	if err := json.Unmarshal(journalData, &envelope); err != nil {
		return nil, fmt.Errorf("decode Codex lease journal: %w", err)
	}
	if envelope.Version != codexLeaseJournalVersion {
		return nil, fmt.Errorf("unsupported Codex lease journal version %d", envelope.Version)
	}
	if !store.validEnvelopeMAC(envelope) {
		return nil, errors.New("Codex lease journal HMAC mismatch")
	}
	store.generation = envelope.Generation
	store.records = append([]CodexJournalRecord(nil), envelope.Records...)
	if store.orphanRestoredRecords() {
		if err := store.commitRecordsLockedWithWriter(store.records, store.generation, func(data []byte) error {
			return fsutil.SecureAtomicWriteInDirectory(inspector, directory, journalName, data)
		}); err != nil {
			return nil, fmt.Errorf("orphan restored Codex leases: %w", err)
		}
	}
	return store, nil
}

func OpenDefaultCodexLeaseStore(fsys fsutil.DurableFileSystem) (*CodexLeaseStore, error) {
	dir := configDir()
	return OpenCodexLeaseStore(fsys, filepath.Join(dir, "codex-turn-leases.json"), filepath.Join(dir, "codex-turn-leases.key"))
}

func OpenCodexRuntimeObserver(runtime *CodexRoutingRuntime, fsys fsutil.DurableFileSystem) (*CodexTurnObserver, error) {
	if runtime == nil {
		return nil, nil
	}
	hasRetainedAuthority := len(runtime.HTTP.RetainedAuthoritativeEpochs) != 0 || len(runtime.WebSocket.RetainedAuthoritativeEpochs) != 0
	if runtime.HTTP.Effective == CodexRoutingOff && runtime.WebSocket.Effective == CodexRoutingOff && !hasRetainedAuthority {
		return nil, nil
	}
	epoch := max(runtime.HTTP.ModeEpoch, runtime.WebSocket.ModeEpoch)
	store, err := OpenDefaultCodexLeaseStore(fsys)
	if err != nil {
		return nil, err
	}
	if err := store.Compact(time.Now(), DefaultCodexLeaseRetention); err != nil {
		return nil, fmt.Errorf("compact Codex lease journal: %w", err)
	}
	manager := NewCodexTurnLeaseManager(epoch, false, nil)
	return NewCodexTurnObserver(manager, store)
}

func fileExists(fsys fsutil.FileSystem, path string) bool {
	_, err := fsys.Stat(path)
	return err == nil
}

func (store *CodexLeaseStore) Generation() uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.generation
}

func (store *CodexLeaseStore) CommitLeases(leases []CodexTurnLease, expectedGeneration uint64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.commitLeasesLocked(leases, expectedGeneration)
}

// CommitCurrentLeases serialises an in-process read-modify-write against the
// current journal generation. Callers needing stale-writer detection use
// CommitLeases with an explicit generation instead.
func (store *CodexLeaseStore) CommitCurrentLeases(leases []CodexTurnLease) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.commitLeasesLocked(leases, store.generation)
}

func (store *CodexLeaseStore) commitLeasesLocked(leases []CodexTurnLease, expectedGeneration uint64) error {
	if store.closed {
		return ErrCodexLeaseWriterUnavailable
	}
	if store.v2 != nil {
		return fmt.Errorf("%w: legacy snapshot writes cannot target schema v2", ErrCodexLeaseWriterUnavailable)
	}
	records := make([]CodexJournalRecord, 0, len(leases))
	type modeKey struct {
		epoch         uint64
		authoritative bool
	}
	modes := make(map[modeKey]bool)
	for _, lease := range leases {
		records = append(records, store.recordForLease(lease))
		modes[modeKey{lease.ModeEpoch, lease.Authoritative}] = true
	}
	if len(modes) != 0 {
		for _, existing := range store.records {
			if !modes[modeKey{existing.ModeEpoch, existing.Authoritative}] {
				records = append(records, existing)
			}
		}
	}
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.SessionHash != right.SessionHash {
			return left.SessionHash < right.SessionHash
		}
		if left.ThreadHash != right.ThreadHash {
			return left.ThreadHash < right.ThreadHash
		}
		return left.TurnHash < right.TurnHash
	})
	return store.commitRecordsLocked(records, expectedGeneration)
}

func (store *CodexLeaseStore) commitRecordsLocked(records []CodexJournalRecord, expectedGeneration uint64) error {
	return store.commitRecordsLockedWithWriter(records, expectedGeneration, func(data []byte) error {
		return durableAtomicWrite(store.fs, store.path, data)
	})
}

func (store *CodexLeaseStore) commitRecordsLockedWithWriter(records []CodexJournalRecord, expectedGeneration uint64, write func([]byte) error) error {
	if store.v2 != nil {
		return fmt.Errorf("%w: legacy snapshot writes cannot target schema v2", ErrCodexLeaseWriterUnavailable)
	}
	if expectedGeneration != store.generation {
		return fmt.Errorf("Codex lease journal generation changed: have %d, expected %d", store.generation, expectedGeneration)
	}
	if write == nil {
		return errors.New("Codex lease journal writer required")
	}
	envelope := codexLeaseJournalEnvelope{Version: codexLeaseJournalVersion, Generation: store.generation + 1, Records: records}
	mac, err := store.envelopeMAC(envelope)
	if err != nil {
		return err
	}
	envelope.MAC = mac
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex lease journal: %w", err)
	}
	if err := write(data); err != nil {
		return err
	}
	store.generation = envelope.Generation
	store.records = append([]CodexJournalRecord(nil), records...)
	return nil
}

func durableAtomicWrite(fsys fsutil.DurableFileSystem, path string, data []byte) error {
	return fsutil.SecureAtomicWrite(fsys, path, data)
}

func (store *CodexLeaseStore) recordForLease(lease CodexTurnLease) CodexJournalRecord {
	return CodexJournalRecord{
		SessionHash:       store.hash("session", lease.Key.Lane.Session),
		ThreadHash:        store.hash("thread", lease.Key.Lane.Thread),
		TurnHash:          store.hash("turn", lease.Key.Turn),
		NamespaceHash:     store.hash("namespace", lease.Key.Lane.Namespace),
		AccountHash:       store.hash("account", string(lease.AccountKey)),
		CorrelationHash:   store.hashOptional("correlation", lease.ResponseAnchor),
		TurnStateHash:     store.hashOptional("turn-state", lease.TurnState),
		State:             lease.State,
		LeaseGeneration:   lease.Generation,
		ModeEpoch:         lease.ModeEpoch,
		Authoritative:     lease.Authoritative,
		ActiveRefs:        lease.RoutingRefs + lease.ActiveAttempts,
		SocketGeneration:  lease.UpstreamSocketGeneration,
		HasEncryptedState: lease.HasEncryptedState,
		HasResponseAnchor: lease.ResponseAnchor != "",
		HasTurnState:      lease.TurnState != "",
		NonMigratable:     lease.NonMigratable,
		LastSeen:          lease.LastSeen.UTC(),
	}
}

func (store *CodexLeaseStore) Lookup(key LeaseKey, accounts []codex.AccountKey) (CodexJournalRecord, codex.AccountKey, bool) {
	return store.lookup(key, accounts, 0, false, false)
}

func (store *CodexLeaseStore) LookupMode(key LeaseKey, accounts []codex.AccountKey, modeEpoch uint64, authoritative bool) (CodexJournalRecord, codex.AccountKey, bool) {
	return store.lookup(key, accounts, modeEpoch, authoritative, true)
}

func (store *CodexLeaseStore) lookup(key LeaseKey, accounts []codex.AccountKey, modeEpoch uint64, authoritative, exactMode bool) (CodexJournalRecord, codex.AccountKey, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return CodexJournalRecord{}, "", false
	}
	sessionHash := store.hash("session", key.Lane.Session)
	threadHash := store.hash("thread", key.Lane.Thread)
	turnHash := store.hash("turn", key.Turn)
	namespaceHash := store.hash("namespace", key.Lane.Namespace)
	for _, record := range store.records {
		if record.SessionHash != sessionHash || record.ThreadHash != threadHash || record.TurnHash != turnHash || record.NamespaceHash != namespaceHash {
			continue
		}
		if exactMode && (record.ModeEpoch != modeEpoch || record.Authoritative != authoritative) {
			continue
		}
		for _, account := range accounts {
			if record.AccountHash == store.hash("account", string(account)) {
				return record, account, true
			}
		}
		return record, "", true
	}
	return CodexJournalRecord{}, "", false
}

func (store *CodexLeaseStore) Compact(now time.Time, retention time.Duration) error {
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return ErrCodexLeaseWriterUnavailable
	}
	if store.v2 != nil {
		store.mu.Unlock()
		return store.compactV2()
	}
	defer store.mu.Unlock()
	if retention <= 0 {
		retention = DefaultCodexLeaseRetention
	}
	records := make([]CodexJournalRecord, 0, len(store.records))
	for _, record := range store.records {
		if record.ActiveRefs > 0 || now.Sub(record.LastSeen) <= retention {
			records = append(records, record)
			continue
		}
		switch record.State {
		case LeaseBoundQuiescent, LeaseOrphaned, LeaseSuperseded, LeaseExpired, LeaseFailedUnadmitted:
			continue
		default:
			record.State = LeaseExpired
			record.LeaseGeneration++
			record.LastSeen = now.UTC()
			record.SocketGeneration = 0
			record.ActiveRefs = 0
			records = append(records, record)
		}
	}
	return store.commitRecordsLocked(records, store.generation)
}

func (store *CodexLeaseStore) orphanRestoredRecords() bool {
	changed := false
	for index := range store.records {
		record := &store.records[index]
		switch record.State {
		case LeaseReserving, LeaseProvisional, LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent:
			record.State = LeaseOrphaned
			record.LeaseGeneration++
			changed = true
		}
		if record.ActiveRefs != 0 || record.SocketGeneration != 0 {
			record.ActiveRefs = 0
			record.SocketGeneration = 0
			changed = true
		}
	}
	return changed
}

func (store *CodexLeaseStore) hash(domain, value string) string {
	mac := hmac.New(sha256.New, store.key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (store *CodexLeaseStore) hashOptional(domain, value string) string {
	if value == "" {
		return ""
	}
	return store.hash(domain, value)
}

func (store *CodexLeaseStore) envelopeMAC(envelope codexLeaseJournalEnvelope) (string, error) {
	envelope.MAC = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode Codex lease journal MAC payload: %w", err)
	}
	mac := hmac.New(sha256.New, store.key)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (store *CodexLeaseStore) validEnvelopeMAC(envelope codexLeaseJournalEnvelope) bool {
	want, err := store.envelopeMAC(envelope)
	if err != nil {
		return false
	}
	wantBytes, wantOK := decodeCanonicalCodexLeaseMAC(want)
	gotBytes, gotOK := decodeCanonicalCodexLeaseMAC(envelope.MAC)
	return wantOK && gotOK && hmac.Equal(gotBytes, wantBytes)
}

func decodeCanonicalCodexLeaseMAC(value string) ([]byte, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return decoded, err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}
