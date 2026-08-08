package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexPrimerJournalVersion = 1
const codexPrimerMaxRejectedAttempts = 2

type PrimerState string

const (
	PrimerStateObserved         PrimerState = "observed"
	PrimerStateClaimed          PrimerState = "claimed"
	PrimerStateRejected         PrimerState = "rejected"
	PrimerStateAdmitted         PrimerState = "admitted"
	PrimerStateAmbiguous        PrimerState = "ambiguous"
	PrimerStateVerifying        PrimerState = "verifying"
	PrimerStateVerified         PrimerState = "verified"
	PrimerStatePrimedExternally PrimerState = "primed_externally"
	PrimerStateUnresolved       PrimerState = "unresolved"
	PrimerStateFailed           PrimerState = "failed"
)

type PrimerRecord struct {
	AccountHash       string      `json:"account_hash"`
	ScopeHash         string      `json:"scope_hash"`
	WindowHash        string      `json:"window_hash,omitempty"`
	ResetAt           time.Time   `json:"reset_at"`
	ModelID           string      `json:"model_id"`
	State             PrimerState `json:"state"`
	Generation        uint64      `json:"generation"`
	NextCheckAt       time.Time   `json:"next_check_at,omitempty"`
	ResultCode        string      `json:"result_code,omitempty"`
	Attempts          int         `json:"attempts,omitempty"`
	ExpectedScopeHash string      `json:"expected_scope_hash,omitempty"`
}

type codexPrimerEnvelope struct {
	Version    int            `json:"version"`
	Generation uint64         `json:"generation"`
	Records    []PrimerRecord `json:"records"`
	MAC        string         `json:"mac"`
}

type CodexPrimerStore struct {
	mu         sync.Mutex
	fs         fsutil.DurableFileSystem
	path       string
	keyPath    string
	key        []byte
	generation uint64
	records    []PrimerRecord
}

func OpenCodexPrimerStore(fsys fsutil.DurableFileSystem, path, keyPath string) (*CodexPrimerStore, error) {
	if fsys == nil || path == "" || keyPath == "" || filepath.Dir(path) != filepath.Dir(keyPath) {
		return nil, errors.New("Codex primer store requires one durable state directory")
	}
	store := &CodexPrimerStore{fs: fsys, path: path, keyPath: keyPath}
	journalExists := fileExists(fsys, path)
	key, err := fsys.ReadFile(keyPath)
	if os.IsNotExist(err) {
		if journalExists {
			return nil, errors.New("Codex primer key missing for existing journal")
		}
		key = make([]byte, codexLeaseHMACKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := durableAtomicWrite(fsys, keyPath, key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != codexLeaseHMACKeyBytes {
		return nil, errors.New("invalid Codex primer key")
	}
	store.key = append([]byte(nil), key...)
	if !journalExists {
		return store, nil
	}
	data, err := fsys.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var envelope codexPrimerEnvelope
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != codexPrimerJournalVersion || !store.validMAC(envelope) {
		return nil, errors.New("invalid Codex primer journal")
	}
	for _, record := range envelope.Records {
		if !record.State.valid() {
			return nil, errors.New("invalid Codex primer state")
		}
	}
	store.generation = envelope.Generation
	store.records = append([]PrimerRecord(nil), envelope.Records...)
	return store, nil
}

func OpenDefaultCodexPrimerStore(fsys fsutil.DurableFileSystem) (*CodexPrimerStore, error) {
	dir := configDir()
	return OpenCodexPrimerStore(fsys, filepath.Join(dir, "codex-window-primer.json"), filepath.Join(dir, "codex-window-primer.key"))
}

func (s *CodexPrimerStore) Observe(account codex.AccountKey, target CodexPrimerTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, target)
	windowHash := s.windowIdentity(target)
	if index := s.index(accountHash, scopeHash); index >= 0 {
		if s.records[index].ModelID == target.ModelID && s.records[index].WindowHash == windowHash {
			return nil
		}
		records := append([]PrimerRecord(nil), s.records...)
		records[index].ModelID = target.ModelID
		records[index].WindowHash = windowHash
		return s.commitLocked(records)
	}
	records := append([]PrimerRecord(nil), s.records...)
	records = append(records, PrimerRecord{
		AccountHash: accountHash, ScopeHash: scopeHash, WindowHash: windowHash, ResetAt: target.ResetAt.UTC(),
		ModelID: target.ModelID, State: PrimerStateObserved,
	})
	return s.commitLocked(records)
}

func (s *CodexPrimerStore) Claim(account codex.AccountKey, target CodexPrimerTarget) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, target)
	index := s.index(accountHash, scopeHash)
	if index < 0 || (s.records[index].State != PrimerStateObserved && s.records[index].State != PrimerStateRejected) || s.records[index].Attempts >= codexPrimerMaxRejectedAttempts {
		return false, nil
	}
	records := append([]PrimerRecord(nil), s.records...)
	records[index].State = PrimerStateClaimed
	records[index].Attempts++
	if err := s.commitLocked(records); err != nil {
		return false, err
	}
	return true, nil
}

func (s *CodexPrimerStore) ClaimDormant(account codex.AccountKey, target CodexPrimerTarget, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, target)
	windowHash := s.windowIdentity(target)
	index := s.index(accountHash, scopeHash)
	if index < 0 || (s.records[index].State != PrimerStateObserved && s.records[index].State != PrimerStateRejected) {
		return false, nil
	}
	priorAttempts := 0
	for i := range s.records {
		record := s.records[i]
		if record.AccountHash != accountHash || record.WindowHash != windowHash || !record.ResetAt.After(now) || !strings.HasPrefix(record.ResultCode, "dormant_") {
			continue
		}
		switch record.State {
		case PrimerStateClaimed, PrimerStateAdmitted, PrimerStateAmbiguous, PrimerStateVerifying, PrimerStateVerified:
			return false, nil
		case PrimerStateRejected:
			priorAttempts = max(priorAttempts, record.Attempts)
		}
	}
	if priorAttempts >= codexPrimerMaxRejectedAttempts {
		return false, nil
	}
	records := append([]PrimerRecord(nil), s.records...)
	records[index].State = PrimerStateClaimed
	records[index].ResultCode = "dormant_claimed"
	records[index].Attempts = priorAttempts + 1
	if err := s.commitLocked(records); err != nil {
		return false, err
	}
	return true, nil
}

func (s *CodexPrimerStore) Mark(account codex.AccountKey, target CodexPrimerTarget, state PrimerState, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, target)
	index := s.index(accountHash, scopeHash)
	if index < 0 {
		return errors.New("Codex primer generation not observed")
	}
	current := s.records[index].State
	if (current == PrimerStateAdmitted || current == PrimerStateAmbiguous || current == PrimerStateVerifying) && (state == PrimerStateObserved || state == PrimerStateClaimed || state == PrimerStateRejected) {
		return errors.New("Codex primer admitted generation cannot become dispatchable")
	}
	records := append([]PrimerRecord(nil), s.records...)
	records[index].State = state
	records[index].ResultCode = code
	return s.commitLocked(records)
}

func (s *CodexPrimerStore) Lookup(account codex.AccountKey, target CodexPrimerTarget) (PrimerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, target)
	index := s.index(accountHash, scopeHash)
	if index < 0 {
		return PrimerRecord{}, false
	}
	return s.records[index], true
}

func (s *CodexPrimerStore) ReconcileAdvanced(account codex.AccountKey, resetEpochs []time.Time, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash := s.hash("account", string(account))
	currentEpochs := make(map[int64]struct{}, len(resetEpochs))
	for _, resetAt := range resetEpochs {
		currentEpochs[resetAt.UTC().UnixNano()] = struct{}{}
	}
	records := append([]PrimerRecord(nil), s.records...)
	changed := false
	for i := range records {
		if records[i].AccountHash != accountHash {
			continue
		}
		if _, current := currentEpochs[records[i].ResetAt.UTC().UnixNano()]; current {
			continue
		}
		switch records[i].State {
		case PrimerStateObserved, PrimerStateRejected:
			if records[i].ResetAt.After(now) {
				continue
			}
			records[i].State = PrimerStatePrimedExternally
			records[i].ResultCode = "reset_advanced"
			changed = true
		case PrimerStateClaimed, PrimerStateAdmitted, PrimerStateAmbiguous, PrimerStateVerifying:
			if strings.HasPrefix(records[i].ResultCode, "dormant_") {
				continue
			}
			records[i].State = PrimerStateVerified
			records[i].ResultCode = "reset_advanced"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.commitLocked(records)
}

func (s *CodexPrimerStore) MarkDormantVerifying(account codex.AccountKey, dispatched, expected CodexPrimerTarget, nextCheck time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash, scopeHash := s.identity(account, dispatched)
	index := s.index(accountHash, scopeHash)
	if index < 0 {
		return errors.New("Codex primer generation not observed")
	}
	records := append([]PrimerRecord(nil), s.records...)
	records[index].State = PrimerStateVerifying
	records[index].ResultCode = "dormant_epoch_stability"
	records[index].WindowHash = s.windowIdentity(expected)
	records[index].ExpectedScopeHash = s.scopeIdentity(expected)
	records[index].NextCheckAt = nextCheck.UTC()
	return s.commitLocked(records)
}

func (s *CodexPrimerStore) ReconcileDormant(account codex.AccountKey, targets []CodexPrimerTarget, now, nextCheck time.Time) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountHash := s.hash("account", string(account))
	type currentTarget struct{ scopeHash string }
	current := make(map[string]currentTarget, len(targets))
	for _, target := range targets {
		current[s.windowIdentity(target)] = currentTarget{scopeHash: s.scopeIdentity(target)}
	}
	records := append([]PrimerRecord(nil), s.records...)
	changed := false
	var earliest time.Time
	for i := range records {
		if records[i].AccountHash != accountHash || records[i].WindowHash == "" {
			continue
		}
		observed, ok := current[records[i].WindowHash]
		if !ok {
			continue
		}
		if (records[i].State == PrimerStateAdmitted || records[i].State == PrimerStateAmbiguous) && strings.HasPrefix(records[i].ResultCode, "dormant_") {
			records[i].State = PrimerStateVerifying
			records[i].ResultCode = "dormant_epoch_stability"
			records[i].ExpectedScopeHash = observed.scopeHash
			records[i].NextCheckAt = nextCheck.UTC()
			changed = true
		}
		if records[i].State != PrimerStateVerifying || records[i].ResultCode != "dormant_epoch_stability" {
			continue
		}
		if records[i].ExpectedScopeHash != observed.scopeHash {
			records[i].ExpectedScopeHash = observed.scopeHash
			records[i].NextCheckAt = nextCheck.UTC()
			changed = true
		}
		if !records[i].NextCheckAt.After(now) && records[i].ExpectedScopeHash == observed.scopeHash {
			records[i].State = PrimerStateVerified
			records[i].ResultCode = "dormant_epoch_stable"
			records[i].ExpectedScopeHash = ""
			records[i].NextCheckAt = time.Time{}
			changed = true
			continue
		}
		if earliest.IsZero() || records[i].NextCheckAt.Before(earliest) {
			earliest = records[i].NextCheckAt
		}
	}
	if !changed {
		return earliest, nil
	}
	return earliest, s.commitLocked(records)
}

func (s *CodexPrimerStore) Records() []PrimerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PrimerRecord(nil), s.records...)
}

func (s *CodexPrimerStore) identity(account codex.AccountKey, target CodexPrimerTarget) (string, string) {
	return s.hash("account", string(account)), s.scopeIdentity(target)
}

func (s *CodexPrimerStore) scopeIdentity(target CodexPrimerTarget) string {
	return s.hash("scope", target.ResetAt.UTC().Format(time.RFC3339Nano)+"|"+s.windowIdentityMaterial(target))
}

func (s *CodexPrimerStore) windowIdentity(target CodexPrimerTarget) string {
	return s.hash("window", s.windowIdentityMaterial(target))
}

func (s *CodexPrimerStore) windowIdentityMaterial(target CodexPrimerTarget) string {
	windowIDs := make([]string, 0, len(target.Windows))
	for _, window := range target.Windows {
		windowIDs = append(windowIDs, window.RawLimitName+"|"+string(window.WindowName)+"|"+window.Period.String())
	}
	sort.Strings(windowIDs)
	return strings.Join(windowIDs, ",")
}

func (s *CodexPrimerStore) index(accountHash, scopeHash string) int {
	for i := range s.records {
		if s.records[i].AccountHash == accountHash && s.records[i].ScopeHash == scopeHash {
			return i
		}
	}
	return -1
}

func (s *CodexPrimerStore) commitLocked(records []PrimerRecord) error {
	next := s.generation + 1
	for i := range records {
		records[i].Generation = next
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].AccountHash != records[j].AccountHash {
			return records[i].AccountHash < records[j].AccountHash
		}
		return records[i].ScopeHash < records[j].ScopeHash
	})
	envelope := codexPrimerEnvelope{Version: codexPrimerJournalVersion, Generation: next, Records: records}
	mac, err := s.envelopeMAC(envelope)
	if err != nil {
		return err
	}
	envelope.MAC = mac
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	if err := durableAtomicWrite(s.fs, s.path, data); err != nil {
		return err
	}
	s.generation = next
	s.records = records
	return nil
}

func (s *CodexPrimerStore) hash(domain, value string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(domain + "\x00" + value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *CodexPrimerStore) envelopeMAC(envelope codexPrimerEnvelope) (string, error) {
	envelope.MAC = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *CodexPrimerStore) validMAC(envelope codexPrimerEnvelope) bool {
	want, err := s.envelopeMAC(envelope)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(want), []byte(envelope.MAC))
}

func (s PrimerState) valid() bool {
	switch s {
	case PrimerStateObserved, PrimerStateClaimed, PrimerStateRejected, PrimerStateAdmitted,
		PrimerStateAmbiguous, PrimerStateVerifying, PrimerStateVerified,
		PrimerStatePrimedExternally, PrimerStateUnresolved, PrimerStateFailed:
		return true
	default:
		return false
	}
}
