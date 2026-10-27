package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type ResetAutomationMode string
type ResetAutomationEntryState string

const (
	ResetAutomationFixed     ResetAutomationMode       = "fixed"
	ResetAutomationAdaptive  ResetAutomationMode       = "adaptive"
	ResetAutomationPending   ResetAutomationEntryState = "pending"
	ResetAutomationRunning   ResetAutomationEntryState = "running"
	ResetAutomationRetrying  ResetAutomationEntryState = "retrying"
	ResetAutomationBlocked   ResetAutomationEntryState = "blocked"
	ResetAutomationCompleted ResetAutomationEntryState = "completed"
	ResetAutomationSkipped   ResetAutomationEntryState = "skipped"
	ResetAutomationMissed    ResetAutomationEntryState = "missed"
	ResetAutomationCancelled ResetAutomationEntryState = "cancelled"
)

type ResetAutomationEntry struct {
	ID            string                    `json:"id"`
	AccountKey    AccountKey                `json:"account_key"`
	AccountID     string                    `json:"account_id,omitempty"`
	Email         string                    `json:"email,omitempty"`
	CreditID      string                    `json:"credit_id"`
	At            *time.Time                `json:"scheduled_at,omitempty"`
	ExpiresAt     *time.Time                `json:"expires_at,omitempty"`
	UseBy         *time.Time                `json:"use_by,omitempty"`
	Pinned        bool                      `json:"pinned"`
	Source        string                    `json:"source,omitempty"`
	Confidence    string                    `json:"confidence,omitempty"`
	State         ResetAutomationEntryState `json:"state"`
	Outcome       ConsumeResetOutcome       `json:"outcome,omitempty"`
	LastError     string                    `json:"last_error,omitempty"`
	Warnings      []string                  `json:"warnings,omitempty"`
	NextAttemptAt *time.Time                `json:"next_attempt_at,omitempty"`
	UpdatedAt     time.Time                 `json:"updated_at"`
}

func (e ResetAutomationEntry) Terminal() bool {
	return e.State == ResetAutomationCompleted || e.State == ResetAutomationSkipped || e.State == ResetAutomationMissed || e.State == ResetAutomationCancelled
}

type ResetAutomationState struct {
	Version      int                    `json:"version"`
	Revision     uint64                 `json:"revision"`
	Enabled      bool                   `json:"enabled"`
	Mode         ResetAutomationMode    `json:"mode"`
	Entries      []ResetAutomationEntry `json:"entries"`
	LastReplanAt *time.Time             `json:"last_replan_at,omitempty"`
	PlanValid    bool                   `json:"plan_valid"`
	PlanError    string                 `json:"plan_error,omitempty"`
	NextReplanAt *time.Time             `json:"next_replan_at,omitempty"`
}

func (s *ResetAutomationState) Entry(id string) *ResetAutomationEntry {
	for i := range s.Entries {
		if s.Entries[i].ID == id {
			return &s.Entries[i]
		}
	}
	return nil
}

type ResetAutomationStore struct {
	FS   fsutil.DurableFileSystem
	Path string
}

func NewResetAutomationStore(fs fsutil.DurableFileSystem, path string) *ResetAutomationStore {
	return &ResetAutomationStore{FS: fs, Path: path}
}

func (s *ResetAutomationStore) Load() (ResetAutomationState, error) {
	state := ResetAutomationState{Version: 1, Mode: ResetAutomationFixed, Entries: []ResetAutomationEntry{}}
	if s == nil || s.FS == nil || !filepath.IsAbs(s.Path) {
		return state, errors.New("reset schedule storage unavailable")
	}
	data, err := fsutil.ReadSecureFile(s.FS, s.Path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read reset schedule: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// Decode into a zero value: a missing version or mode is corrupt state,
	// never permission to silently activate defaults.
	state = ResetAutomationState{}
	if err := decoder.Decode(&state); err != nil {
		return state, errors.New("invalid reset schedule JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return state, errors.New("trailing reset schedule JSON")
	}
	if err := validateResetAutomation(state); err != nil {
		return state, err
	}
	return state, nil
}

func (s *ResetAutomationStore) Update(ctx context.Context, change func(*ResetAutomationState) error) (ResetAutomationState, error) {
	if s == nil || s.FS == nil || !filepath.IsAbs(s.Path) || change == nil {
		return ResetAutomationState{}, errors.New("reset schedule storage unavailable")
	}
	if err := ctx.Err(); err != nil {
		return ResetAutomationState{}, err
	}
	if err := fsutil.EnsureSecureDirectory(s.FS, filepath.Dir(s.Path)); err != nil {
		return ResetAutomationState{}, err
	}
	var lock fsutil.ExclusiveLock
	for {
		var err error
		lock, err = fsutil.AcquireExclusiveLock(s.FS, s.Path+".lock")
		if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			if err != nil {
				return ResetAutomationState{}, err
			}
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ResetAutomationState{}, ctx.Err()
		case <-timer.C:
		}
	}
	defer lock.Close()
	state, err := s.Load()
	if err != nil {
		return state, err
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if err := change(&state); err != nil {
		return state, err
	}
	state.Revision++
	if err := validateResetAutomation(state); err != nil {
		return state, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return state, err
	}
	if len(data) > 1<<20 {
		return state, errors.New("reset schedule exceeds size limit")
	}
	if err := fsutil.SecureAtomicWrite(s.FS, s.Path, data); err != nil {
		return state, err
	}
	return state, nil
}

func (s *ResetAutomationStore) AcquireExecutor() (fsutil.ExclusiveLock, error) {
	if s == nil || s.FS == nil || !filepath.IsAbs(s.Path) {
		return nil, errors.New("reset schedule storage unavailable")
	}
	if err := fsutil.ValidateSecureDirectory(s.FS, filepath.Dir(s.Path)); errors.Is(err, os.ErrNotExist) {
		if err := fsutil.EnsureSecureDirectory(s.FS, filepath.Dir(s.Path)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return fsutil.AcquireExclusiveLock(s.FS, s.Path+".executor.lock")
}

func validateResetAutomation(s ResetAutomationState) error {
	if s.Version != 1 || (s.Mode != ResetAutomationFixed && s.Mode != ResetAutomationAdaptive) || len(s.Entries) > 1000 {
		return errors.New("invalid reset schedule schema")
	}
	ids, active := map[string]bool{}, map[string]bool{}
	for _, e := range s.Entries {
		if e.ID == "" || e.AccountKey == "" || e.CreditID == "" || ids[e.ID] {
			return errors.New("invalid reset schedule entry identity")
		}
		ids[e.ID] = true
		switch e.State {
		case ResetAutomationPending, ResetAutomationRunning, ResetAutomationRetrying, ResetAutomationBlocked, ResetAutomationCompleted, ResetAutomationSkipped, ResetAutomationMissed, ResetAutomationCancelled:
		default:
			return errors.New("invalid reset schedule entry state")
		}
		if !e.Terminal() {
			pair := string(e.AccountKey) + "\x00" + e.CreditID
			if active[pair] {
				return errors.New("duplicate reset schedule credit")
			}
			active[pair] = true
		}
		if e.At != nil && e.At.IsZero() || e.ExpiresAt != nil && e.ExpiresAt.IsZero() {
			return errors.New("invalid reset schedule time")
		}
	}
	return nil
}
