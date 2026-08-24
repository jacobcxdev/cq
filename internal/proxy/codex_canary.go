package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CodexCanaryVersion = 2

const (
	codexCanaryIntegrityKeyBytes = 32
	codexCanaryStateMaxBytes     = 1 << 20
	codexCanaryOwnerLockName     = ".codex-routing-canary.owner.lock"
)

var (
	ErrCodexCanaryActive        = errors.New("Codex canary is already active")
	ErrCodexCanaryNotPromotable = errors.New("Codex canary is not promotable")
)

type CodexCanaryProtectionKind string

const (
	CodexCanarySystemAuth       CodexCanaryProtectionKind = "system_auth"
	CodexCanaryRegistry         CodexCanaryProtectionKind = "account_registry"
	CodexCanaryCQManagedAuth    CodexCanaryProtectionKind = "cq_managed_auth"
	CodexCanaryCodexBarManifest CodexCanaryProtectionKind = "codexbar_manifest"
	CodexCanaryCodexBarAuth     CodexCanaryProtectionKind = "codexbar_auth"
	CodexCanaryRoutingDefault   CodexCanaryProtectionKind = "routing_default"
)

var requiredCodexCanaryProtection = []CodexCanaryProtectionKind{
	CodexCanarySystemAuth,
	CodexCanaryRegistry,
	CodexCanaryCQManagedAuth,
	CodexCanaryCodexBarManifest,
	CodexCanaryCodexBarAuth,
	CodexCanaryRoutingDefault,
}

type CodexCanaryTuple struct {
	CQBuild              string `json:"cq_build"`
	ClientBuild          string `json:"client_build"`
	ParserSchema         int    `json:"parser_schema"`
	LeaseSchema          int    `json:"lease_schema"`
	SemanticsRevision    string `json:"semantics_revision"`
	RetryBudget          int    `json:"retry_budget"`
	FixtureHash          string `json:"fixture_hash"`
	ReadinessFingerprint string `json:"readiness_fingerprint"`
}

type CodexCanaryProtectedDigest struct {
	Kind   CodexCanaryProtectionKind `json:"kind"`
	Digest string                    `json:"digest"`
}

type codexCanaryFinalisation struct {
	StopRequestDigest    string `json:"stop_request_digest"`
	ProcessBindingDigest string `json:"process_binding_digest"`
	CountersDigest       string `json:"counters_digest"`
	ActiveSessions       uint64 `json:"active_sessions"`
}

type CodexCanaryState struct {
	Version                 int                          `json:"version"`
	RunID                   string                       `json:"run_id"`
	Active                  bool                         `json:"active"`
	StartedAt               time.Time                    `json:"started_at"`
	EndedAt                 time.Time                    `json:"ended_at,omitempty"`
	LastObservedAt          time.Time                    `json:"last_observed_at"`
	Tuple                   CodexCanaryTuple             `json:"tuple"`
	AdmittedTurns           uint64                       `json:"admitted_turns"`
	KeyedMismatches         uint64                       `json:"keyed_mismatches"`
	AutomaticHashChanges    uint64                       `json:"automatic_protected_state_changes"`
	SecretLeaks             uint64                       `json:"secret_leaks"`
	UnexplainedLifecycles   uint64                       `json:"unexplained_lifecycles"`
	LiveSessionRepairs      uint64                       `json:"live_session_repairs"`
	ProtectedStateFailures  uint64                       `json:"protected_state_failures"`
	ProtectedDigests        []CodexCanaryProtectedDigest `json:"protected_digests"`
	ConsecutiveCalendarDays int                          `json:"consecutive_calendar_days"`
	Finalisation            *codexCanaryFinalisation     `json:"finalisation,omitempty"`
}

type codexCanaryEnvelope struct {
	Version    int              `json:"version"`
	Generation uint64           `json:"generation"`
	State      CodexCanaryState `json:"state"`
	MAC        string           `json:"mac"`
}

// CodexCanaryProtection keeps source paths and snapshot functions in memory.
// Persisted canary evidence contains only the named kind and SHA-256 digest.
type CodexCanaryProtection struct {
	Kind            CodexCanaryProtectionKind
	snapshot        func(fsutil.FileSystem) ([]byte, error)
	optionalAbsence bool
}

type CodexCanaryRecorder struct {
	mu             sync.Mutex
	fs             fsutil.DurableFileSystem
	path           string
	key            []byte
	generation     uint64
	protected      []CodexCanaryProtection
	state          CodexCanaryState
	ownerLock      fsutil.ExclusiveLock
	ownerDirectory fsutil.SecureDirectory
	ownerInspector fsutil.SecurePathInspector
}

func CodexCanaryPath(stateDir string) string {
	return filepath.Join(stateDir, "codex-routing-canary.json")
}

func DefaultCodexCanaryPath() (string, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return "", err
	}
	return CodexCanaryPath(paths.StateDir), nil
}

func CodexCanaryFileProtection(kind CodexCanaryProtectionKind, path string) CodexCanaryProtection {
	path = filepath.Clean(path)
	return CodexCanaryProtection{
		Kind: kind,
		snapshot: func(fsys fsutil.FileSystem) ([]byte, error) {
			return fsutil.ReadSecureFile(fsys, path, codexCanaryStateMaxBytes)
		},
	}
}

func CodexCanaryOptionalFileProtection(kind CodexCanaryProtectionKind, path string) CodexCanaryProtection {
	protected := CodexCanaryFileProtection(kind, path)
	protected.optionalAbsence = true
	return protected
}

// CodexCanaryOptionalSnapshotProtection keeps provider-owned secure reads at
// their source boundary. The callback may return os.ErrNotExist only when the
// exact optional source is absent; persisted state contains only its digest.
func CodexCanaryOptionalSnapshotProtection(kind CodexCanaryProtectionKind, snapshot func() ([]byte, error)) CodexCanaryProtection {
	if snapshot == nil {
		return CodexCanaryProtection{Kind: kind, optionalAbsence: true}
	}
	return CodexCanaryProtection{
		Kind:            kind,
		optionalAbsence: true,
		snapshot: func(fsutil.FileSystem) ([]byte, error) {
			return snapshot()
		},
	}
}

// CodexCanaryDirectoryProtection snapshots only direct files with the exact
// configured suffix. It does not recurse or inspect any other directory.
func CodexCanaryDirectoryProtection(kind CodexCanaryProtectionKind, directory, suffix string) CodexCanaryProtection {
	directory = filepath.Clean(directory)
	return CodexCanaryProtection{
		Kind: kind,
		snapshot: func(fsys fsutil.FileSystem) ([]byte, error) {
			inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
			opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
			if !inspectorOK || !openerOK {
				return nil, fsutil.ErrSecureCapabilityUnavailable
			}
			if err := fsutil.ValidateSecureDirectory(fsys, directory); err != nil {
				return nil, err
			}
			opened, err := opener.OpenSecureDirectory(directory)
			if err != nil {
				return nil, err
			}
			defer opened.Close()
			reader, ok := opened.(fsutil.SecureDirectoryReader)
			if !ok {
				return nil, fsutil.ErrSecureCapabilityUnavailable
			}
			openedInfo, err := opened.Stat()
			if err != nil {
				return nil, err
			}
			openedIdentity, ok := inspector.FileIdentity(openedInfo)
			if !ok {
				return nil, fsutil.ErrUnsafeSecurePath
			}
			entries, err := reader.ReadDir()
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), suffix) {
					continue
				}
				if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					return nil, errors.New("unsafe protected directory entry")
				}
				names = append(names, entry.Name())
			}
			sort.Strings(names)
			var snapshot bytes.Buffer
			for _, name := range names {
				data, err := fsutil.ReadSecureFileInDirectory(inspector, opened, name, codexCanaryStateMaxBytes)
				if err != nil {
					return nil, err
				}
				appendCanarySnapshotPart(&snapshot, []byte(name))
				appendCanarySnapshotPart(&snapshot, data)
			}
			if err := fsutil.ValidateSecureDirectory(fsys, directory); err != nil {
				return nil, err
			}
			afterInfo, err := opened.Stat()
			if err != nil {
				return nil, err
			}
			afterIdentity, ok := inspector.FileIdentity(afterInfo)
			if !ok || afterIdentity != openedIdentity {
				return nil, fsutil.ErrUnsafeSecurePath
			}
			currentInfo, err := inspector.Lstat(directory)
			if err != nil {
				return nil, err
			}
			currentIdentity, ok := inspector.FileIdentity(currentInfo)
			if !ok || currentIdentity != openedIdentity {
				return nil, fsutil.ErrUnsafeSecurePath
			}
			return snapshot.Bytes(), nil
		},
	}
}

func CodexCanaryJSONFieldProtection(kind CodexCanaryProtectionKind, path, field string) CodexCanaryProtection {
	path = filepath.Clean(path)
	return CodexCanaryProtection{
		Kind:            kind,
		optionalAbsence: true,
		snapshot: func(fsys fsutil.FileSystem) ([]byte, error) {
			data, err := fsutil.ReadSecureFile(fsys, path, codexCanaryStateMaxBytes)
			if err != nil {
				return nil, err
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(data, &object); err != nil {
				return nil, errors.New("invalid protected JSON")
			}
			value, ok := object[field]
			if !ok {
				return []byte("absent"), nil
			}
			var canonical bytes.Buffer
			if err := json.Compact(&canonical, value); err != nil {
				return nil, errors.New("invalid protected JSON field")
			}
			return canonical.Bytes(), nil
		},
	}
}

// CodexCanaryRoutingPolicyProtection snapshots the complete account-routing
// authority. Account order is canonical because allowlist order has no routing
// meaning.
func CodexCanaryRoutingPolicyProtection(kind CodexCanaryProtectionKind, path, defaultField, accountsField, pinnedField string) CodexCanaryProtection {
	path = filepath.Clean(path)
	return CodexCanaryProtection{
		Kind:            kind,
		optionalAbsence: true,
		snapshot: func(fsys fsutil.FileSystem) ([]byte, error) {
			data, err := fsutil.ReadSecureFile(fsys, path, codexCanaryStateMaxBytes)
			if err != nil {
				return nil, err
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(data, &object); err != nil {
				return nil, errors.New("invalid protected JSON")
			}
			type routingPolicySnapshot struct {
				DefaultPresent  bool     `json:"default_present"`
				Default         string   `json:"default,omitempty"`
				AccountsPresent bool     `json:"accounts_present"`
				Accounts        []string `json:"accounts,omitempty"`
				PinnedPresent   bool     `json:"pinned_present"`
				Pinned          string   `json:"pinned,omitempty"`
			}
			var snapshot routingPolicySnapshot
			if value, ok := object[defaultField]; ok {
				snapshot.DefaultPresent = true
				if err := json.Unmarshal(value, &snapshot.Default); err != nil {
					return nil, errors.New("invalid protected routing default")
				}
			}
			if value, ok := object[accountsField]; ok {
				snapshot.AccountsPresent = true
				if err := json.Unmarshal(value, &snapshot.Accounts); err != nil {
					return nil, errors.New("invalid protected routing accounts")
				}
				sort.Strings(snapshot.Accounts)
			}
			if value, ok := object[pinnedField]; ok {
				snapshot.PinnedPresent = true
				if err := json.Unmarshal(value, &snapshot.Pinned); err != nil {
					return nil, errors.New("invalid protected routing pin")
				}
			}
			return json.Marshal(snapshot)
		},
	}
}

func StartCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection, tuple CodexCanaryTuple, now time.Time) (*CodexCanaryRecorder, error) {
	protected, err := prepareCodexCanaryProtection(protected)
	if fsys == nil || path == "" || !completeCodexCanaryTuple(tuple) || err != nil {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	ownerDirectory, ownerLock, inspector, err := acquireCodexCanaryOwner(fsys, path)
	if err != nil {
		return nil, ErrCodexCanaryActive
	}
	releaseOwner := true
	defer func() {
		if releaseOwner {
			_ = ownerLock.Close()
			_ = ownerDirectory.Close()
		}
	}()
	existing, openErr := openCodexCanaryInDirectory(fsys, path, protected, inspector, ownerDirectory)
	if openErr == nil && existing.State().Active {
		return nil, ErrCodexCanaryActive
	}
	if openErr != nil && !errors.Is(openErr, os.ErrNotExist) {
		return nil, errors.New("open existing Codex canary")
	}
	var key []byte
	if existing != nil {
		key = append([]byte(nil), existing.key...)
	} else {
		key, err = loadOrCreateCodexCanaryIntegrityKeyInDirectory(inspector, ownerDirectory, filepath.Base(path)+".key")
		if err != nil {
			return nil, err
		}
	}
	recorder := &CodexCanaryRecorder{
		fs: fsys, path: path, key: key, protected: protected,
		ownerLock: ownerLock, ownerDirectory: ownerDirectory, ownerInspector: inspector,
	}
	if existing != nil {
		recorder.generation = existing.generation
	}
	runID, err := newCodexCanaryRandomID()
	if err != nil {
		return nil, err
	}
	recorder.state = CodexCanaryState{Version: CodexCanaryVersion, RunID: runID, Active: true, StartedAt: now.UTC(), Tuple: tuple}
	recorder.state.ProtectedDigests, err = recorder.digestsLocked()
	if err != nil {
		return nil, err
	}
	if err := recorder.persistLocked(); err != nil {
		return nil, err
	}
	releaseOwner = false
	return recorder, nil
}

// OpenServingCodexCanary opens the sole mutable recorder. Read-only status and
// promotion callers must use OpenCodexCanary and never acquire this lock.
func OpenServingCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection) (*CodexCanaryRecorder, error) {
	if fsys == nil || path == "" {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	protected, err := prepareCodexCanaryProtection(protected)
	if err != nil {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	ownerDirectory, ownerLock, inspector, err := acquireCodexCanaryOwner(fsys, path)
	if err != nil {
		return nil, errors.New("Codex canary serving owner unavailable")
	}
	recorder, err := openCodexCanaryInDirectory(fsys, path, protected, inspector, ownerDirectory)
	if err != nil {
		_ = ownerLock.Close()
		_ = ownerDirectory.Close()
		return nil, err
	}
	if !recorder.state.Active {
		_ = ownerLock.Close()
		_ = ownerDirectory.Close()
		return recorder, nil
	}
	recorder.ownerLock = ownerLock
	recorder.ownerDirectory = ownerDirectory
	recorder.ownerInspector = inspector
	return recorder, nil
}

func OpenCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection) (*CodexCanaryRecorder, error) {
	if fsys == nil || path == "" {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	protected, err := prepareCodexCanaryProtection(protected)
	if err != nil {
		return nil, errors.New("incomplete Codex canary configuration")
	}
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(path)
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		return nil, err
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := validateCodexCanaryRetainedDirectory(fsys, inspector, directory, directoryPath); err != nil {
		return nil, err
	}
	data, err := fsutil.ReadSecureFileInDirectory(inspector, directory, filepath.Base(path), codexCanaryStateMaxBytes)
	if err != nil {
		return nil, err
	}
	key, err := readCodexCanaryIntegrityKeyInDirectory(inspector, directory, filepath.Base(path)+".key")
	if err != nil {
		return nil, err
	}
	if err := validateCodexCanaryRetainedDirectory(fsys, inspector, directory, directoryPath); err != nil {
		return nil, err
	}
	return decodeCodexCanary(fsys, path, protected, data, key)
}

func validateCodexCanaryRetainedDirectory(fsys fsutil.FileSystem, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, path string) error {
	if err := fsutil.ValidateSecureDirectory(fsys, path); err != nil {
		return err
	}
	heldInfo, err := directory.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || heldIdentity != pathIdentity {
		return fsutil.ErrUnsafeSecurePath
	}
	return nil
}

func openCodexCanaryInDirectory(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory) (*CodexCanaryRecorder, error) {
	data, err := fsutil.ReadSecureFileInDirectory(inspector, directory, filepath.Base(path), codexCanaryStateMaxBytes)
	if err != nil {
		return nil, err
	}
	key, err := readCodexCanaryIntegrityKeyInDirectory(inspector, directory, filepath.Base(path)+".key")
	if err != nil {
		return nil, err
	}
	return decodeCodexCanary(fsys, path, protected, data, key)
}

func decodeCodexCanary(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection, data, key []byte) (*CodexCanaryRecorder, error) {
	var envelope codexCanaryEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("invalid Codex canary state")
	}
	canonical, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil || !bytes.Equal(data, canonical) {
		return nil, errors.New("non-canonical Codex canary state")
	}
	if envelope.Version != CodexCanaryVersion || envelope.State.Version != CodexCanaryVersion || envelope.Generation == 0 || !validCodexCanaryRandomID(envelope.State.RunID) || !validCodexCanaryFinalisation(envelope.State) {
		return nil, errors.New("unsupported Codex canary version")
	}
	if !validCodexCanaryEnvelopeMAC(key, envelope) {
		return nil, errors.New("Codex canary state integrity mismatch")
	}
	return &CodexCanaryRecorder{
		fs: fsys, path: path, key: key, generation: envelope.Generation,
		protected: protected, state: envelope.State,
	}, nil
}

func acquireCodexCanaryOwner(fsys fsutil.DurableFileSystem, path string) (fsutil.SecureDirectory, fsutil.ExclusiveLock, fsutil.SecurePathInspector, error) {
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return nil, nil, nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(path)
	if err := fsutil.EnsureSecureDirectory(fsys, directoryPath); err != nil {
		return nil, nil, nil, err
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, codexCanaryOwnerLockName)
	if err != nil {
		_ = directory.Close()
		return nil, nil, nil, err
	}
	return directory, lock, inspector, nil
}

func loadOrCreateCodexCanaryIntegrityKeyInDirectory(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string) ([]byte, error) {
	key, err := readCodexCanaryIntegrityKeyInDirectory(inspector, directory, name)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, codexCanaryIntegrityKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, errors.New("generate Codex canary integrity key")
	}
	if err := fsutil.SecureAtomicCreateInDirectory(inspector, directory, name, key); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readCodexCanaryIntegrityKeyInDirectory(inspector, directory, name)
		}
		return nil, errors.New("persist Codex canary integrity key")
	}
	return key, nil
}

func readCodexCanaryIntegrityKeyInDirectory(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string) ([]byte, error) {
	key, err := fsutil.ReadSecureFileInDirectory(inspector, directory, name, codexCanaryIntegrityKeyBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("read Codex canary integrity key")
	}
	if len(key) != codexCanaryIntegrityKeyBytes {
		return nil, errors.New("invalid Codex canary integrity key")
	}
	return key, nil
}

func newCodexCanaryRandomID() (string, error) {
	value := make([]byte, sha256.Size)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate Codex canary random identifier")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validCodexCanaryRandomID(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func prepareCodexCanaryProtection(protected []CodexCanaryProtection) ([]CodexCanaryProtection, error) {
	result := append([]CodexCanaryProtection(nil), protected...)
	seen := make(map[CodexCanaryProtectionKind]bool, len(result))
	for _, source := range result {
		if source.Kind == "" || source.snapshot == nil || seen[source.Kind] {
			return nil, errors.New("invalid Codex canary protection")
		}
		seen[source.Kind] = true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result, nil
}

func (recorder *CodexCanaryRecorder) State() CodexCanaryState {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	state := recorder.state
	state.ProtectedDigests = append([]CodexCanaryProtectedDigest(nil), recorder.state.ProtectedDigests...)
	if recorder.state.Finalisation != nil {
		finalisation := *recorder.state.Finalisation
		state.Finalisation = &finalisation
	}
	return state
}

func (recorder *CodexCanaryRecorder) Close() error {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	lock := recorder.ownerLock
	directory := recorder.ownerDirectory
	recorder.ownerLock = nil
	recorder.ownerDirectory = nil
	recorder.ownerInspector = nil
	recorder.mu.Unlock()
	var err error
	if lock != nil {
		err = lock.Close()
	}
	if directory != nil {
		err = errors.Join(err, directory.Close())
	}
	return err
}

func (recorder *CodexCanaryRecorder) requireOwnerLocked() error {
	if recorder.ownerLock == nil || recorder.ownerDirectory == nil || recorder.ownerInspector == nil {
		return errors.New("Codex canary recorder is read-only")
	}
	heldInfo, err := recorder.ownerLock.Stat()
	if err != nil || !heldInfo.Mode().IsRegular() || heldInfo.Mode().Perm() != 0o600 {
		return errors.New("Codex canary serving owner unavailable")
	}
	heldOwner, heldOwnerOK := recorder.ownerInspector.FileOwnerUID(heldInfo)
	heldIdentity, heldIdentityOK := recorder.ownerInspector.FileIdentity(heldInfo)
	pathFile, err := recorder.ownerDirectory.OpenNoFollow(codexCanaryOwnerLockName)
	if err != nil {
		return errors.New("Codex canary serving owner unavailable")
	}
	defer pathFile.Close()
	pathInfo, err := pathFile.Stat()
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 {
		return errors.New("Codex canary serving owner unavailable")
	}
	pathOwner, pathOwnerOK := recorder.ownerInspector.FileOwnerUID(pathInfo)
	pathIdentity, pathIdentityOK := recorder.ownerInspector.FileIdentity(pathInfo)
	if !heldOwnerOK || !pathOwnerOK || heldOwner != recorder.ownerInspector.EffectiveUID() || pathOwner != heldOwner ||
		!heldIdentityOK || !pathIdentityOK || heldIdentity != pathIdentity || heldIdentity.Links != 1 {
		return errors.New("Codex canary serving owner unavailable")
	}
	return nil
}

// ValidateTuple rejects attaching a persisted run to a different binary,
// client, parser, lease, retry, fixture, semantics, or readiness marker.
func (recorder *CodexCanaryRecorder) ValidateTuple(tuple CodexCanaryTuple) error {
	if recorder == nil || !completeCodexCanaryTuple(tuple) {
		return errors.New("incomplete Codex canary tuple")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.state.Tuple != tuple {
		return errors.New("Codex canary runtime tuple mismatch")
	}
	return nil
}

func (recorder *CodexCanaryRecorder) RecordAdmitted(now time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.requireOwnerLocked(); err != nil {
		return err
	}
	if !recorder.state.Active {
		return errors.New("Codex canary is not active")
	}
	recorder.state.AdmittedTurns++
	recorder.observeDayLocked(now)
	recorder.checkProtectedLocked()
	return recorder.persistLocked()
}

// RecordServiceHeartbeat records an observation emitted by the running proxy
// service. Read-only CLI operations must never call it.
func (recorder *CodexCanaryRecorder) RecordServiceHeartbeat(now time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.requireOwnerLocked(); err != nil {
		return err
	}
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

func (recorder *CodexCanaryRecorder) RecordLiveSessionRepair() error {
	return recorder.increment(func(state *CodexCanaryState) { state.LiveSessionRepairs++ })
}

func (recorder *CodexCanaryRecorder) increment(update func(*CodexCanaryState)) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.requireOwnerLocked(); err != nil {
		return err
	}
	if !recorder.state.Active {
		return errors.New("Codex canary is not active")
	}
	update(&recorder.state)
	return recorder.persistLocked()
}

func (recorder *CodexCanaryRecorder) observeDayLocked(now time.Time) {
	day := canaryCalendarDay(now)
	previous := canaryCalendarDay(recorder.state.LastObservedAt)
	if previous.IsZero() {
		recorder.state.ConsecutiveCalendarDays = 1
		recorder.state.LastObservedAt = day
		return
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
	current, err := recorder.digestsLocked()
	if err != nil {
		recorder.state.ProtectedStateFailures++
		return
	}
	if !equalCodexCanaryDigests(current, recorder.state.ProtectedDigests) {
		recorder.state.AutomaticHashChanges++
	}
	recorder.state.ProtectedDigests = current
}

func equalCodexCanaryDigests(first, second []CodexCanaryProtectedDigest) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func (recorder *CodexCanaryRecorder) digestsLocked() ([]CodexCanaryProtectedDigest, error) {
	result := make([]CodexCanaryProtectedDigest, 0, len(recorder.protected))
	for _, protected := range recorder.protected {
		data, err := snapshotCodexCanaryProtection(protected, recorder.fs)
		status := byte('p')
		if errors.Is(err, os.ErrNotExist) && protected.optionalAbsence {
			status = 'a'
			data = nil
		} else if err != nil {
			return nil, errors.New("Codex canary protected state unavailable")
		}
		hash := sha256.New()
		_, _ = hash.Write([]byte("cq-codex-canary-protection-v2\x00"))
		_, _ = hash.Write([]byte(protected.Kind))
		_, _ = hash.Write([]byte{0, status, 0})
		_, _ = hash.Write(data)
		result = append(result, CodexCanaryProtectedDigest{Kind: protected.Kind, Digest: hex.EncodeToString(hash.Sum(nil))})
	}
	return result, nil
}

func snapshotCodexCanaryProtection(protected CodexCanaryProtection, fsys fsutil.FileSystem) (data []byte, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("Codex canary protected state unavailable")
		}
	}()
	return protected.snapshot(fsys)
}

func appendCanarySnapshotPart(destination *bytes.Buffer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination.Write(length[:])
	destination.Write(value)
}

func completeCodexCanaryTuple(tuple CodexCanaryTuple) bool {
	decodedFingerprint, err := hex.DecodeString(tuple.ReadinessFingerprint)
	return tuple.CQBuild != "" && tuple.ClientBuild != "" && tuple.ParserSchema > 0 && tuple.LeaseSchema > 0 &&
		tuple.SemanticsRevision != "" && tuple.RetryBudget >= 0 && tuple.FixtureHash != "" && err == nil && len(decodedFingerprint) == sha256.Size
}

// BuildCodexCanaryTuple binds a run to the current, fully validated HTTP
// readiness marker. The marker fingerprint excludes only its validation time.
func BuildCodexCanaryTuple(required CodexTransportRequirements, marker CodexReadinessMarker) (CodexCanaryTuple, error) {
	if required.Transport != CodexRoutingHTTP {
		return CodexCanaryTuple{}, errors.New("Codex canary requires HTTP readiness")
	}
	if err := ValidateCodexReadinessMarker(marker, required); err != nil {
		return CodexCanaryTuple{}, fmt.Errorf("validate Codex canary readiness: %w", err)
	}
	return CodexCanaryTuple{
		CQBuild:              required.CQBuild,
		ClientBuild:          required.ClientBuild,
		ParserSchema:         required.ParserSchema,
		LeaseSchema:          required.LeaseSchema,
		SemanticsRevision:    required.SemanticsRevision,
		RetryBudget:          required.RetryBudget,
		FixtureHash:          required.FixtureHash,
		ReadinessFingerprint: markerFingerprint(marker),
	}, nil
}

// BuildCurrentCodexCanaryTuple binds canary start to the exact currently
// installed CQ, client, and loaded service artifacts. It returns no process or
// path evidence to the caller.
func BuildCurrentCodexCanaryTuple(cqBuild, clientBuild string, marker CodexReadinessMarker) (CodexCanaryTuple, error) {
	required, _ := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	return buildCurrentCodexCanaryTupleWithArtifactCapture(required, marker, captureCurrentCodexInstalledArtifacts)
}

func buildCurrentCodexCanaryTupleWithArtifactCapture(
	required CodexTransportRequirements,
	marker CodexReadinessMarker,
	capture codexInstalledArtifactCapture,
) (CodexCanaryTuple, error) {
	artifacts, err := captureCodexInstalledArtifactsSafely(capture, required.ClientBuild)
	if err != nil || !artifacts.valid() {
		return CodexCanaryTuple{}, errors.New("current installed Codex artifacts unavailable")
	}
	required.installedArtifacts = artifacts
	return BuildCodexCanaryTuple(required, marker)
}

func (recorder *CodexCanaryRecorder) persistLocked() error {
	_, err := recorder.persistEnvelopeLocked()
	return err
}

type codexCanaryPersistedEnvelope struct {
	generation uint64
	data       []byte
}

func (recorder *CodexCanaryRecorder) persistEnvelopeLocked() (codexCanaryPersistedEnvelope, error) {
	if len(recorder.key) != codexCanaryIntegrityKeyBytes {
		return codexCanaryPersistedEnvelope{}, errors.New("invalid Codex canary integrity key")
	}
	envelope := codexCanaryEnvelope{
		Version:    CodexCanaryVersion,
		Generation: recorder.generation + 1,
		State:      recorder.state,
	}
	mac, err := codexCanaryEnvelopeMAC(recorder.key, envelope)
	if err != nil {
		return codexCanaryPersistedEnvelope{}, err
	}
	envelope.MAC = mac
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return codexCanaryPersistedEnvelope{}, err
	}
	if recorder.ownerDirectory != nil {
		if err := recorder.requireOwnerLocked(); err != nil {
			return codexCanaryPersistedEnvelope{}, err
		}
		if err := fsutil.SecureAtomicWriteInDirectoryChecked(recorder.ownerInspector, recorder.ownerDirectory, filepath.Base(recorder.path), data, recorder.requireOwnerLocked); err != nil {
			return codexCanaryPersistedEnvelope{}, err
		}
	} else if err := durableAtomicWrite(recorder.fs, recorder.path, data); err != nil {
		return codexCanaryPersistedEnvelope{}, err
	}
	recorder.generation = envelope.Generation
	return codexCanaryPersistedEnvelope{generation: envelope.Generation, data: append([]byte(nil), data...)}, nil
}

func codexCanaryEnvelopeMAC(key []byte, envelope codexCanaryEnvelope) (string, error) {
	envelope.MAC = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("encode Codex canary integrity payload")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq-codex-canary-state-v2\x00"))
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func validCodexCanaryEnvelopeMAC(key []byte, envelope codexCanaryEnvelope) bool {
	want, err := codexCanaryEnvelopeMAC(key, envelope)
	if err != nil {
		return false
	}
	wantBytes, wantErr := base64.RawURLEncoding.DecodeString(want)
	gotBytes, gotErr := base64.RawURLEncoding.DecodeString(envelope.MAC)
	return wantErr == nil && gotErr == nil && len(gotBytes) == sha256.Size &&
		base64.RawURLEncoding.EncodeToString(gotBytes) == envelope.MAC && hmac.Equal(gotBytes, wantBytes)
}
