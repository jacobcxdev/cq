package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CodexCanaryVersion = 1

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
	recorder := &CodexCanaryRecorder{fs: fsys, path: path, protected: append([]string(nil), protected...)}
	recorder.state = CodexCanaryState{Version: CodexCanaryVersion, Active: true, StartedAt: now.UTC(), Tuple: tuple, ConsecutiveCalendarDays: 1}
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
	recorder.updateDaysLocked(now)
	recorder.checkProtectedLocked()
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) RecordKeyedMismatch() error {
	return recorder.increment(func(state *CodexCanaryState) { state.KeyedMismatches++ })
}

func (recorder *CodexCanaryRecorder) RecordUnexplainedLifecycle() error {
	return recorder.increment(func(state *CodexCanaryState) { state.UnexplainedLifecycles++ })
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

func (recorder *CodexCanaryRecorder) updateDaysLocked(now time.Time) {
	days := int(now.UTC().Truncate(24*time.Hour).Sub(recorder.state.StartedAt.UTC().Truncate(24*time.Hour))/(24*time.Hour)) + 1
	if days > recorder.state.ConsecutiveCalendarDays {
		recorder.state.ConsecutiveCalendarDays = days
	}
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
