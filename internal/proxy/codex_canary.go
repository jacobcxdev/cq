package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CodexCanaryVersion = 1

var ErrCodexCanaryActive = errors.New("Codex canary is already active")

type CodexCanaryTuple struct {
	CQBuild      string `json:"cq_build"`
	ClientBuild  string `json:"client_build"`
	ParserSchema int    `json:"parser_schema"`
	LeaseSchema  int    `json:"lease_schema"`
	FixtureHash  string `json:"fixture_hash"`
}

type CodexCanaryState struct {
	Version                 int              `json:"version"`
	Active                  bool             `json:"active"`
	StartedAt               time.Time        `json:"started_at"`
	EndedAt                 time.Time        `json:"ended_at,omitempty"`
	LastObservedAt          time.Time        `json:"last_observed_at"`
	Tuple                   CodexCanaryTuple `json:"tuple"`
	AdmittedTurns           uint64           `json:"admitted_turns"`
	KeyedMismatches         uint64           `json:"keyed_mismatches"`
	AutomaticHashChanges    uint64           `json:"automatic_auth_registry_hash_changes"`
	SecretLeaks             uint64           `json:"secret_leaks"`
	UnexplainedLifecycles   uint64           `json:"unexplained_lifecycles"`
	ProtectedDigests        []string         `json:"protected_digests"`
	ConsecutiveCalendarDays int              `json:"consecutive_calendar_days"`
}

type CodexCanaryRecorder struct {
	mu        sync.Mutex
	fs        fsutil.DurableFileSystem
	path      string
	protected []string
	state     CodexCanaryState
}

func DefaultCodexCanaryPath() string {
	return filepath.Join(configDir(), "codex-routing-canary.json")
}

func StartCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []string, tuple CodexCanaryTuple, now time.Time) (*CodexCanaryRecorder, error) {
	if fsys == nil || path == "" || tuple.CQBuild == "" || tuple.ClientBuild == "" || tuple.ParserSchema <= 0 || tuple.LeaseSchema <= 0 || tuple.FixtureHash == "" {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	if _, err := fsys.ReadFile(path); err == nil {
		existing, err := OpenCodexCanary(fsys, path, protected)
		if err != nil {
			return nil, fmt.Errorf("open existing Codex canary: %w", err)
		}
		if existing.State().Active {
			return nil, ErrCodexCanaryActive
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing Codex canary: %w", err)
	}
	recorder := &CodexCanaryRecorder{fs: fsys, path: path, protected: append([]string(nil), protected...)}
	observed := canaryCalendarDay(now)
	recorder.state = CodexCanaryState{Version: CodexCanaryVersion, Active: true, StartedAt: now.UTC(), LastObservedAt: observed, Tuple: tuple, ConsecutiveCalendarDays: 1}
	recorder.state.ProtectedDigests = recorder.digestsLocked()
	if err := recorder.persistLocked(); err != nil {
		return nil, err
	}
	return recorder, nil
}

func OpenCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []string) (*CodexCanaryRecorder, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state CodexCanaryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Version != CodexCanaryVersion {
		return nil, errors.New("unsupported Codex canary version")
	}
	if state.LastObservedAt.IsZero() {
		state.LastObservedAt = canaryCalendarDay(state.StartedAt)
		state.ConsecutiveCalendarDays = max(state.ConsecutiveCalendarDays, 1)
	}
	return &CodexCanaryRecorder{fs: fsys, path: path, protected: append([]string(nil), protected...), state: state}, nil
}

func (recorder *CodexCanaryRecorder) State() CodexCanaryState {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.state
}

func (recorder *CodexCanaryRecorder) RecordAdmitted(now time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if !recorder.state.Active {
		return errors.New("Codex canary is not active")
	}
	recorder.state.AdmittedTurns++
	recorder.observeDayLocked(now)
	recorder.checkProtectedLocked()
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) RecordHeartbeat(now time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if !recorder.state.Active {
		return errors.New("Codex canary is not active")
	}
	recorder.observeDayLocked(now)
	recorder.checkProtectedLocked()
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) RecordKeyedMismatch() error {
	return recorder.increment(func(state *CodexCanaryState) { state.KeyedMismatches++ })
}

func (recorder *CodexCanaryRecorder) RecordUnexplainedLifecycle() error {
	return recorder.increment(func(state *CodexCanaryState) { state.UnexplainedLifecycles++ })
}

func (recorder *CodexCanaryRecorder) RecordSecretLeak() error {
	return recorder.increment(func(state *CodexCanaryState) { state.SecretLeaks++ })
}

func (recorder *CodexCanaryRecorder) AcknowledgeExplicitSwitch() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.state.ProtectedDigests = recorder.digestsLocked()
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) Stop(now time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.observeDayLocked(now)
	recorder.state.Active = false
	recorder.state.EndedAt = now.UTC()
	recorder.checkProtectedLocked()
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) increment(update func(*CodexCanaryState)) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	update(&recorder.state)
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) observeDayLocked(now time.Time) {
	day := canaryCalendarDay(now)
	previous := canaryCalendarDay(recorder.state.LastObservedAt)
	if previous.IsZero() {
		previous = canaryCalendarDay(recorder.state.StartedAt)
	}
	delta := int(day.Sub(previous) / (24 * time.Hour))
	switch {
	case delta == 1:
		recorder.state.ConsecutiveCalendarDays++
	case delta > 1:
		recorder.state.ConsecutiveCalendarDays = 1
	case delta < 0:
		return
	}
	recorder.state.LastObservedAt = day
}

func canaryCalendarDay(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	year, month, day := value.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func (recorder *CodexCanaryRecorder) checkProtectedLocked() {
	current := recorder.digestsLocked()
	if len(current) != len(recorder.state.ProtectedDigests) {
		recorder.state.AutomaticHashChanges++
	} else {
		for index := range current {
			if current[index] != recorder.state.ProtectedDigests[index] {
				recorder.state.AutomaticHashChanges++
				break
			}
		}
	}
	recorder.state.ProtectedDigests = current
}

func (recorder *CodexCanaryRecorder) digestsLocked() []string {
	result := make([]string, len(recorder.protected))
	for index, path := range recorder.protected {
		data, err := recorder.fs.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				result[index] = "missing"
			} else {
				result[index] = "unreadable"
			}
			continue
		}
		sum := sha256.Sum256(data)
		result[index] = hex.EncodeToString(sum[:])
	}
	return result
}

func (recorder *CodexCanaryRecorder) persistLocked() error {
	data, err := json.MarshalIndent(recorder.state, "", "  ")
	if err != nil {
		return err
	}
	return durableAtomicWrite(recorder.fs, recorder.path, data)
}
