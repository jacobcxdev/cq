package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	resetAttemptVersion = 1
	resetAttemptDirName = "reset-attempts-v1"
	resetAttemptMaxSize = int64(16 * 1024)
)

type ResetAttempt struct {
	Version        int        `json:"version"`
	AccountKey     AccountKey `json:"account_key"`
	CreditID       string     `json:"credit_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	StartedAt      time.Time  `json:"started_at"`
}

type ResetAttemptStore struct {
	FS  fsutil.DurableFileSystem
	Dir string
	mu  sync.Mutex
}

type ResetCreditSelection struct {
	Credit  ResetCredit
	Attempt *ResetAttempt
	Resume  bool
}

type ResetCreditSelectionError struct {
	Code string
}

func (e *ResetCreditSelectionError) Error() string {
	return "reset credit selection failed: " + e.Code
}

func ResetIdempotencyKey(account AccountKey, creditID string) string {
	digest := resetAttemptDigest(account, creditID)
	return "cq-reset-v1-" + digest
}

func NewResetAttemptStore(fs fsutil.DurableFileSystem, cacheRoot string) (*ResetAttemptStore, error) {
	if fs == nil || strings.TrimSpace(cacheRoot) == "" {
		return nil, errors.New("reset attempt storage unavailable")
	}
	if _, ok := fs.(fsutil.ExclusiveFileCreator); !ok {
		return nil, errors.New("exclusive reset attempt creation unavailable")
	}
	dir := filepath.Join(cacheRoot, resetAttemptDirName)
	if _, inspectable := fs.(fsutil.SecurePathInspector); inspectable {
		if _, durable := fs.(fsutil.DurableDirectoryOpener); !durable {
			return nil, fsutil.ErrSecureCapabilityUnavailable
		}
		if err := fsutil.EnsureSecureDirectory(fs, dir); err != nil {
			return nil, fmt.Errorf("create reset attempt directory: %w", err)
		}
	} else {
		if err := fs.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create reset attempt directory: %w", err)
		}
		if err := fs.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("secure reset attempt directory: %w", err)
		}
		if err := fs.SyncDir(filepath.Dir(dir)); err != nil {
			return nil, fmt.Errorf("sync reset attempt directory parent: %w", err)
		}
	}
	return &ResetAttemptStore{FS: fs, Dir: dir}, nil
}

func (s *ResetAttemptStore) Pending(account AccountKey) ([]ResetAttempt, error) {
	if s == nil || s.FS == nil || account == "" {
		return nil, errors.New("reset attempt account is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateDirectory(); err != nil {
		return nil, err
	}
	entries, err := s.FS.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	attempts := make([]ResetAttempt, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("invalid reset attempt entry")
		}
		path := filepath.Join(s.Dir, entry.Name())
		attempt, err := s.read(path, "", "")
		if err != nil {
			return nil, err
		}
		if entry.Name() != resetAttemptFileName(attempt.AccountKey, attempt.CreditID) {
			return nil, errors.New("reset attempt filename mismatch")
		}
		if attempt.AccountKey == account {
			attempts = append(attempts, attempt)
		}
	}
	sort.Slice(attempts, func(i, j int) bool {
		if !attempts[i].StartedAt.Equal(attempts[j].StartedAt) {
			return attempts[i].StartedAt.Before(attempts[j].StartedAt)
		}
		return attempts[i].CreditID < attempts[j].CreditID
	})
	return attempts, nil
}

func (s *ResetAttemptStore) Ensure(account AccountKey, creditID string, startedAt time.Time) (ResetAttempt, error) {
	if s == nil || s.FS == nil || account == "" || strings.TrimSpace(creditID) == "" || startedAt.IsZero() {
		return ResetAttempt{}, errors.New("invalid reset attempt")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateDirectory(); err != nil {
		return ResetAttempt{}, err
	}
	attempt := ResetAttempt{
		Version: resetAttemptVersion, AccountKey: account, CreditID: creditID,
		IdempotencyKey: ResetIdempotencyKey(account, creditID), StartedAt: startedAt.UTC(),
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		return ResetAttempt{}, err
	}
	path := filepath.Join(s.Dir, resetAttemptFileName(account, creditID))
	creator := s.FS.(fsutil.ExclusiveFileCreator)
	file, err := creator.CreateExclusive(path, 0o600)
	if errors.Is(err, os.ErrExist) {
		return s.read(path, account, creditID)
	}
	if err != nil {
		return ResetAttempt{}, fmt.Errorf("create reset attempt: %w", err)
	}
	created := true
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if created {
			_ = s.FS.Remove(path)
		}
	}()
	if err := writeResetAttempt(file, data); err != nil {
		return ResetAttempt{}, fmt.Errorf("write reset attempt: %w", err)
	}
	if err := file.Sync(); err != nil {
		return ResetAttempt{}, fmt.Errorf("sync reset attempt: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return ResetAttempt{}, fmt.Errorf("close reset attempt: %w", err)
	}
	closed = true
	if err := s.FS.SyncDir(s.Dir); err != nil {
		return ResetAttempt{}, fmt.Errorf("sync reset attempt directory: %w", err)
	}
	created = false
	return s.read(path, account, creditID)
}

func (s *ResetAttemptStore) Remove(account AccountKey, creditID string) error {
	if s == nil || s.FS == nil || account == "" || strings.TrimSpace(creditID) == "" {
		return errors.New("invalid reset attempt")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateDirectory(); err != nil {
		return err
	}
	path := filepath.Join(s.Dir, resetAttemptFileName(account, creditID))
	if err := s.FS.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return s.FS.SyncDir(s.Dir)
}

func (s *ResetAttemptStore) validateDirectory() error {
	if _, ok := s.FS.(fsutil.SecurePathInspector); ok {
		return fsutil.ValidateSecureDirectory(s.FS, s.Dir)
	}
	info, err := s.FS.Stat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("unsafe reset attempt directory")
	}
	return nil
}

func (s *ResetAttemptStore) read(path string, expectedAccount AccountKey, expectedCredit string) (ResetAttempt, error) {
	data, err := readSecureResetAttempt(s.FS, path)
	if err != nil {
		return ResetAttempt{}, err
	}
	var attempt ResetAttempt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&attempt); err != nil {
		return ResetAttempt{}, errors.New("invalid reset attempt record")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ResetAttempt{}, errors.New("invalid reset attempt record")
	}
	if attempt.Version != resetAttemptVersion || attempt.AccountKey == "" || attempt.CreditID == "" || attempt.StartedAt.IsZero() ||
		attempt.IdempotencyKey != ResetIdempotencyKey(attempt.AccountKey, attempt.CreditID) {
		return ResetAttempt{}, errors.New("invalid reset attempt record")
	}
	if expectedAccount != "" && (attempt.AccountKey != expectedAccount || attempt.CreditID != expectedCredit) {
		return ResetAttempt{}, errors.New("reset attempt identity mismatch")
	}
	return attempt, nil
}

func readSecureResetAttempt(fs fsutil.DurableFileSystem, path string) ([]byte, error) {
	if _, ok := fs.(fsutil.SecurePathInspector); ok {
		if err := fsutil.ValidateSecureRegularFile(fs, path); err != nil {
			return nil, err
		}
		opener, ok := fs.(fsutil.NoFollowFileOpener)
		if !ok {
			return nil, fsutil.ErrSecureCapabilityUnavailable
		}
		file, err := opener.OpenNoFollow(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return readBoundedResetAttempt(file)
	}
	info, err := fs.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("unsafe reset attempt record")
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > resetAttemptMaxSize {
		return nil, errors.New("reset attempt record too large")
	}
	return data, nil
}

func readBoundedResetAttempt(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, resetAttemptMaxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > resetAttemptMaxSize {
		return nil, errors.New("reset attempt record too large")
	}
	return data, nil
}

func writeResetAttempt(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func resetAttemptDigest(account AccountKey, creditID string) string {
	digest := sha256.Sum256([]byte(string(account) + "\x00" + creditID))
	return hex.EncodeToString(digest[:])
}

func resetAttemptFileName(account AccountKey, creditID string) string {
	return resetAttemptDigest(account, creditID) + ".json"
}

func SelectResetCredit(now time.Time, credits []ResetCredit, pending []ResetAttempt, explicitID string) (ResetCreditSelection, error) {
	orderedPending := append([]ResetAttempt(nil), pending...)
	sort.Slice(orderedPending, func(i, j int) bool {
		if !orderedPending[i].StartedAt.Equal(orderedPending[j].StartedAt) {
			return orderedPending[i].StartedAt.Before(orderedPending[j].StartedAt)
		}
		return orderedPending[i].CreditID < orderedPending[j].CreditID
	})
	if len(orderedPending) > 0 {
		selected := orderedPending[0]
		if explicitID != "" {
			matched := false
			for _, attempt := range orderedPending {
				if attempt.CreditID == explicitID {
					selected = attempt
					matched = true
					break
				}
			}
			if !matched {
				return ResetCreditSelection{}, &ResetCreditSelectionError{Code: "pending_attempt_conflict"}
			}
		}
		credit := ResetCredit{ID: selected.CreditID}
		for _, candidate := range credits {
			if candidate.ID == selected.CreditID {
				credit = candidate
				break
			}
		}
		attempt := selected
		return ResetCreditSelection{Credit: credit, Attempt: &attempt, Resume: true}, nil
	}

	eligible := make([]ResetCredit, 0, len(credits))
	for _, credit := range credits {
		if credit.ID == "" || credit.ResetType != ResetTypeCodexRateLimits || credit.Status != ResetCreditAvailable ||
			(credit.ExpiresAt != nil && !credit.ExpiresAt.After(now)) {
			continue
		}
		eligible = append(eligible, credit)
	}
	sort.Slice(eligible, func(i, j int) bool {
		leftExpiry, rightExpiry := eligible[i].ExpiresAt, eligible[j].ExpiresAt
		if leftExpiry == nil && rightExpiry != nil {
			return false
		}
		if leftExpiry != nil && rightExpiry == nil {
			return true
		}
		if leftExpiry != nil && !leftExpiry.Equal(*rightExpiry) {
			return leftExpiry.Before(*rightExpiry)
		}
		if !eligible[i].GrantedAt.Equal(eligible[j].GrantedAt) {
			return eligible[i].GrantedAt.Before(eligible[j].GrantedAt)
		}
		return eligible[i].ID < eligible[j].ID
	})
	if explicitID != "" {
		for _, credit := range eligible {
			if credit.ID == explicitID {
				return ResetCreditSelection{Credit: credit}, nil
			}
		}
		return ResetCreditSelection{}, &ResetCreditSelectionError{Code: "credit_unavailable"}
	}
	if len(eligible) == 0 {
		return ResetCreditSelection{}, &ResetCreditSelectionError{Code: "no_available_credit"}
	}
	return ResetCreditSelection{Credit: eligible[0]}, nil
}
