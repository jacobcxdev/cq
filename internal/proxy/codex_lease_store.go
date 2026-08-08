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
	mu         sync.Mutex
	fs         fsutil.DurableFileSystem
	path       string
	keyPath    string
	key        []byte
	generation uint64
	records    []CodexJournalRecord
}

func OpenCodexLeaseStore(fsys fsutil.DurableFileSystem, path, keyPath string) (*CodexLeaseStore, error) {
	if fsys == nil {
		return nil, errors.New("durable filesystem required for Codex lease journal")
	}
	if path == "" || keyPath == "" || filepath.Dir(path) != filepath.Dir(keyPath) {
		return nil, errors.New("Codex lease journal and key require one state directory")
	}
	store := &CodexLeaseStore{fs: fsys, path: path, keyPath: keyPath}
	journalExists := fileExists(fsys, path)
	key, err := fsys.ReadFile(keyPath)
	if os.IsNotExist(err) {
		if journalExists {
			return nil, errors.New("Codex lease HMAC key missing for existing journal")
		}
		key = make([]byte, codexLeaseHMACKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate Codex lease HMAC key: %w", err)
		}
		if err := durableAtomicWrite(fsys, keyPath, key); err != nil {
			return nil, fmt.Errorf("persist Codex lease HMAC key: %w", err)
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
	data, err := fsys.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Codex lease journal: %w", err)
	}
	var envelope codexLeaseJournalEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
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
		if err := store.commitRecordsLocked(store.records, store.generation); err != nil {
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
	if expectedGeneration != store.generation {
		return fmt.Errorf("Codex lease journal generation changed: have %d, expected %d", store.generation, expectedGeneration)
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
	if err := durableAtomicWrite(store.fs, store.path, data); err != nil {
		return err
	}
	store.generation = envelope.Generation
	store.records = append([]CodexJournalRecord(nil), records...)
	return nil
}

func durableAtomicWrite(fsys fsutil.DurableFileSystem, path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create durable state directory: %w", err)
	}
	if err := fsys.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure durable state directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write durable temporary file: %w", err)
	}
	cleanup := func() { _ = fsys.Remove(tmp) }
	if err := fsys.Chmod(tmp, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure durable temporary file: %w", err)
	}
	if err := fsys.SyncFile(tmp); err != nil {
		cleanup()
		return fmt.Errorf("sync durable temporary file: %w", err)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("replace durable file: %w", err)
	}
	if err := fsys.SyncDir(dir); err != nil {
		return fmt.Errorf("sync durable state directory: %w", err)
	}
	return nil
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
	if retention <= 0 {
		retention = DefaultCodexLeaseRetention
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	records := make([]CodexJournalRecord, 0, len(store.records))
	for _, record := range store.records {
		if record.ActiveRefs > 0 || now.Sub(record.LastSeen) <= retention {
			records = append(records, record)
			continue
		}
		switch record.State {
		case LeaseSuperseded, LeaseExpired, LeaseFailedUnadmitted:
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
	wantBytes, err := base64.RawURLEncoding.DecodeString(want)
	if err != nil {
		return false
	}
	gotBytes, err := base64.RawURLEncoding.DecodeString(envelope.MAC)
	if err != nil {
		return false
	}
	return hmac.Equal(gotBytes, wantBytes)
}
