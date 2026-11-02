package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

// ZAIResetScheduleEntry binds one accepted date to one account and exact card.
// It never contains credentials, and terminal entries remain inspectable.
type ZAIResetScheduleEntry struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	CreditID      string     `json:"credit_id"`
	Window        string     `json:"window"`
	At            time.Time  `json:"scheduled_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	State         string     `json:"state"`
	LastError     string     `json:"last_error,omitempty"`
}

type ZAIResetScheduleState struct {
	SchemaVersion int                     `json:"schema_version"`
	Enabled       bool                    `json:"enabled"`
	Entries       []ZAIResetScheduleEntry `json:"entries"`
}

type ZAIResetScheduleStore struct {
	FS   fsutil.FileSystem
	Path string
}

func (s *ZAIResetScheduleStore) Load() (ZAIResetScheduleState, error) {
	state := ZAIResetScheduleState{SchemaVersion: 1, Entries: []ZAIResetScheduleEntry{}}
	if s == nil || s.FS == nil || s.Path == "" {
		return state, errors.New("Z.ai reset schedule unavailable")
	}
	data, err := s.FS.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, errors.New("read Z.ai reset schedule")
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &state) != nil || state.SchemaVersion != 1 || state.Entries == nil {
		return ZAIResetScheduleState{}, errors.New("invalid Z.ai reset schedule")
	}
	seen := make(map[string]bool)
	cards := make(map[string]bool)
	for _, entry := range state.Entries {
		key := entry.AccountID + "\x00" + entry.CreditID
		if !validZAIResetScheduleEntry(entry) || seen[entry.ID] || cards[key] {
			return ZAIResetScheduleState{}, errors.New("invalid Z.ai reset schedule")
		}
		seen[entry.ID], cards[key] = true, true
	}
	return state, nil
}

func validZAIResetScheduleEntry(entry ZAIResetScheduleEntry) bool {
	if !zaiPrimerAccountID(entry.AccountID) || !safeZAIResetScheduleText(entry.ID, 80) || !safeZAIResetScheduleText(entry.CreditID, 300) || entry.At.IsZero() || (entry.Window != "5h" && entry.Window != "7d") || !strings.HasPrefix(entry.CreditID, entry.Window+":") {
		return false
	}
	if entry.ExpiresAt != nil && !entry.At.Before(*entry.ExpiresAt) {
		return false
	}
	switch entry.State {
	case "pending", "running", "accepted", "rejected", "ambiguous", "unavailable", "cancelled":
		return len(entry.LastError) <= 160
	}
	return false
}

func safeZAIResetScheduleText(value string, limit int) bool {
	if value == "" || len(value) > limit || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *ZAIResetScheduleStore) save(state ZAIResetScheduleState) error {
	data, err := json.Marshal(state)
	if err != nil || fsutil.SecureAtomicWrite(s.FS, s.Path, append(data, '\n')) != nil {
		return errors.New("save Z.ai reset schedule")
	}
	return nil
}

func (s *ZAIResetScheduleStore) change(change func(*ZAIResetScheduleState) error) (ZAIResetScheduleState, error) {
	if err := fsutil.EnsureSecureDirectory(s.FS, filepath.Dir(s.Path)); err != nil {
		return ZAIResetScheduleState{}, errors.New("prepare Z.ai reset schedule directory")
	}
	lock, err := fsutil.AcquireExclusiveLock(s.FS, s.Path+".lock")
	if err != nil {
		return ZAIResetScheduleState{}, errors.New("Z.ai reset schedule busy or storage unavailable")
	}
	defer lock.Close()
	state, err := s.Load()
	if err != nil {
		return state, err
	}
	if err := change(&state); err != nil {
		return state, err
	}
	return state, s.save(state)
}

func (s *ZAIResetScheduleStore) Add(accountID string, credit zaiprov.ResetCredit, at, now time.Time, acceptedEnabled bool) (ZAIResetScheduleState, error) {
	entry := ZAIResetScheduleEntry{ID: rand.Text(), AccountID: accountID, CreditID: credit.ID, Window: credit.Window, At: at.UTC(), ExpiresAt: credit.ExpiresAt, State: "pending"}
	if !credit.Available || !at.After(now) || !validZAIResetScheduleEntry(entry) {
		return ZAIResetScheduleState{}, errors.New("Z.ai reset schedule needs an available card and a future date before expiry")
	}
	return s.change(func(state *ZAIResetScheduleState) error {
		if state.Enabled && !acceptedEnabled {
			return errors.New("Z.ai reset schedule is enabled; confirm the new entry")
		}
		for _, existing := range state.Entries {
			if existing.AccountID == accountID && existing.CreditID == credit.ID {
				return errors.New("Z.ai reset card already has a schedule entry")
			}
		}
		state.Entries = append(state.Entries, entry)
		return nil
	})
}

func (s *ZAIResetScheduleStore) SetEnabled(enabled bool) (ZAIResetScheduleState, error) {
	return s.change(func(state *ZAIResetScheduleState) error { state.Enabled = enabled; return nil })
}

// EnableReviewed binds confirmation to the exact displayed entries. A card
// added while a user considers the prompt must not become enabled unseen.
func (s *ZAIResetScheduleStore) EnableReviewed(reviewed ZAIResetScheduleState) (ZAIResetScheduleState, error) {
	return s.change(func(state *ZAIResetScheduleState) error {
		if !reflect.DeepEqual(*state, reviewed) {
			return errors.New("Z.ai reset schedule changed during review; inspect it and enable again")
		}
		state.Enabled = true
		return nil
	})
}

func (s *ZAIResetScheduleStore) Remove(id string) (ZAIResetScheduleState, error) {
	return s.change(func(state *ZAIResetScheduleState) error {
		for i := range state.Entries {
			if state.Entries[i].ID != id {
				continue
			}
			if state.Entries[i].State != "pending" {
				return errors.New("only a pending Z.ai reset entry can be cancelled")
			}
			state.Entries[i].State = "cancelled"
			return nil
		}
		return errors.New("Z.ai reset schedule entry not found")
	})
}

func (s *ZAIResetScheduleStore) Cancel() (ZAIResetScheduleState, error) {
	return s.change(func(state *ZAIResetScheduleState) error {
		state.Enabled = false
		for i := range state.Entries {
			if state.Entries[i].State == "pending" {
				state.Entries[i].State = "cancelled"
			}
		}
		return nil
	})
}

type ZAIResetScheduler struct {
	Store      *ZAIResetScheduleStore
	Accounts   func() ([]zaiprov.Account, error)
	Executor   zaiprov.ResetExecutor
	OnError    func(error)
	Invalidate func()

	runMu     sync.Mutex
	mu        sync.Mutex
	lastError string
}

func NewZAIResetScheduler(fsys fsutil.FileSystem, stateDir string, accounts func() ([]zaiprov.Account, error), executor zaiprov.ResetExecutor) (*ZAIResetScheduler, error) {
	if fsys == nil || stateDir == "" || accounts == nil {
		return nil, errors.New("Z.ai reset scheduler dependencies unavailable")
	}
	scheduler := &ZAIResetScheduler{Store: &ZAIResetScheduleStore{FS: fsys, Path: filepath.Join(stateDir, "zai-reset-schedule.json")}, Accounts: accounts, Executor: executor}
	if _, err := scheduler.Store.Load(); err != nil {
		return nil, err
	}
	return scheduler, nil
}

type ZAIResetSchedulerHealth struct {
	State     ZAIResetScheduleState `json:"schedule"`
	LastError string                `json:"last_error,omitempty"`
}

func (s *ZAIResetScheduler) Health() ZAIResetSchedulerHealth {
	if s == nil || s.Store == nil {
		return ZAIResetSchedulerHealth{LastError: "unavailable"}
	}
	state, err := s.Store.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	health := ZAIResetSchedulerHealth{State: state, LastError: s.lastError}
	if err != nil {
		health.LastError = "read_schedule_failed"
	}
	return health
}

// Run reloads the disabled-by-default schedule. Controls apply without restart.
func (s *ZAIResetScheduler) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.RunOnce(ctx, time.Now()); err != nil && ctx.Err() == nil && s.OnError != nil {
			s.OnError(err)
		}
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce serialises workers and persists the dispatch claim before execution.
// A stale running claim is ambiguous and is never sent automatically again.
func (s *ZAIResetScheduler) RunOnce(ctx context.Context, now time.Time) (runErr error) {
	if s == nil || s.Store == nil || s.Accounts == nil {
		return errors.New("Z.ai reset scheduler unavailable")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	defer func() {
		if recover() != nil {
			runErr = errors.New("Z.ai reset scheduler failed")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastError = ""
		if runErr != nil {
			s.lastError = "execution_failed"
		}
	}()
	state, err := s.Store.Load()
	if err != nil || !state.Enabled {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLock(s.Store.FS, s.Store.Path+".lock")
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return nil
	}
	if err != nil {
		return errors.New("lock Z.ai reset schedule")
	}
	defer lock.Close()
	state, err = s.Store.Load()
	if err != nil || !state.Enabled {
		return err
	}
	accounts, err := s.Accounts()
	if err != nil {
		return errors.New("read Z.ai scheduled reset accounts")
	}
	byID := make(map[string]zaiprov.Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	var failures []error
	for i := range state.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := &state.Entries[i]
		if entry.State == "running" {
			entry.State, entry.LastError = "ambiguous", "previous_execution_unresolved"
			if err := s.Store.save(state); err != nil {
				return err
			}
			continue
		}
		if entry.State != "pending" || entry.At.After(now) || entry.NextAttemptAt != nil && entry.NextAttemptAt.After(now) {
			continue
		}
		account, exists := byID[entry.AccountID]
		if !exists || entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
			entry.State, entry.LastError = "unavailable", "account_or_card_unavailable"
			if err := s.Store.save(state); err != nil {
				return err
			}
			continue
		}
		entry.State, entry.LastError = "running", ""
		if err := s.Store.save(state); err != nil {
			return err
		}
		result, err := s.execute(ctx, account, entry.Window, entry.CreditID)
		entry.NextAttemptAt = nil
		if err == nil && result.Accepted {
			entry.State = "accepted"
			if s.Invalidate != nil {
				s.Invalidate()
			}
		} else if err != nil && !result.Dispatched {
			// The shared executor proves that no reset POST began. Transient
			// inventory reads or manual-operation locks may recover safely.
			entry.State, entry.LastError = "pending", "pre_dispatch_failed"
			next := now.Add(5 * time.Minute)
			entry.NextAttemptAt = &next
			failures = append(failures, errors.New("Z.ai scheduled reset deferred before dispatch"))
		} else {
			entry.State, entry.LastError = "ambiguous", "reset_outcome_unresolved"
			var resetErr *zaiprov.ResetError
			if errors.As(err, &resetErr) && !resetErr.Ambiguous {
				entry.State, entry.LastError = "rejected", "reset_rejected"
			}
			failures = append(failures, errors.New("Z.ai scheduled reset failed; inspect schedule status"))
		}
		if err := s.Store.save(state); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (s *ZAIResetScheduler) execute(ctx context.Context, account zaiprov.Account, window, creditID string) (result zaiprov.ResetUseResult, err error) {
	defer func() {
		if recover() != nil {
			result.Dispatched = true
			err = errors.New("Z.ai scheduled reset failed")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return s.Executor.UseScheduled(ctx, account, window, creditID)
}
