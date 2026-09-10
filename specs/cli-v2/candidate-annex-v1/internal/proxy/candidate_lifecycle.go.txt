package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	candidateLifecycleKeyName   = "candidate.key"
	candidateLifecycleLockName  = "candidate.lock"
	candidateLifecycleStateName = "candidate.json"
	candidateClientRegistryName = "client-sender-registry.json"
)

var (
	ErrCandidateLifecycleInvalid    = errors.New("candidate lifecycle invalid")
	ErrCandidateLifecycleExists     = errors.New("candidate lifecycle already exists")
	ErrCandidateEffectIndeterminate = errors.New("candidate external effect outcome indeterminate")
)

type CandidateLifecyclePhase string

const (
	CandidatePhasePrepared  CandidateLifecyclePhase = "prepared"
	CandidatePhaseRunning   CandidateLifecyclePhase = "running"
	CandidatePhaseStopped   CandidateLifecyclePhase = "stopped"
	CandidatePhaseValidated CandidateLifecyclePhase = "validated"
	CandidatePhaseRemoved   CandidateLifecyclePhase = "removed"
)

type CandidateLifecycleAction string

const (
	CandidateActionStart           CandidateLifecycleAction = "start"
	CandidateActionStop            CandidateLifecycleAction = "stop"
	CandidateActionRefreshBarrier  CandidateLifecycleAction = "refresh_client_bearer_barrier"
	CandidateActionArtifactSwitch  CandidateLifecycleAction = "artifact_switch"
	CandidateActionValidateRelease CandidateLifecycleAction = "validate_release"
	CandidateActionRemove          CandidateLifecycleAction = "remove"
)

type CandidatePrepareInputV1 struct {
	Root                           string
	Port                           int
	SourceConfigDigest             string
	TargetReleaseBundleDigest      string
	TargetReleaseSetDigest         string
	ClientBuild                    string
	ClientExecutableDigest         string
	LocalTokenClientRegistryDigest string
	LocalTokenClientRegistry       []byte
	CredentialMode                 string
	CredentialManifestDigest       string
	PolicySnapshotDigest           string
	PayloadCapture                 bool
}

type CandidateLifecycleStateV1 struct {
	SchemaVersion                    int                      `json:"schema_version"`
	Kind                             string                   `json:"kind"`
	OperationID                      string                   `json:"operation_id"`
	ValidationRunID                  string                   `json:"validation_run_id"`
	ProxyInstanceID                  string                   `json:"proxy_instance_id"`
	Port                             int                      `json:"port"`
	SourceConfigDigest               string                   `json:"source_config_digest"`
	TargetReleaseBundleDigest        string                   `json:"target_release_bundle_digest"`
	TargetReleaseSetDigest           string                   `json:"target_release_set_digest"`
	ActiveReleaseSetDigest           string                   `json:"active_release_set_digest"`
	ClientBuild                      string                   `json:"client_build"`
	ClientExecutableDigest           string                   `json:"client_executable_digest"`
	LocalTokenClientRegistryDigest   string                   `json:"local_token_client_registry_digest"`
	CredentialMode                   string                   `json:"credential_mode"`
	CredentialManifestDigest         string                   `json:"credential_manifest_digest,omitempty"`
	PolicySnapshotDigest             string                   `json:"policy_snapshot_digest,omitempty"`
	PayloadCapture                   bool                     `json:"payload_capture"`
	Phase                            CandidateLifecyclePhase  `json:"phase"`
	Generation                       uint64                   `json:"generation"`
	PendingAction                    CandidateLifecycleAction `json:"pending_action,omitempty"`
	EffectStarted                    bool                     `json:"effect_started,omitempty"`
	EffectReceiptDigest              string                   `json:"effect_receipt_digest,omitempty"`
	ClientBearerBarrierReceiptDigest string                   `json:"client_bearer_barrier_receipt_digest,omitempty"`
	ValidationReceiptDigest          string                   `json:"validation_receipt_digest,omitempty"`
	PendingTargetDigest              string                   `json:"pending_target_digest,omitempty"`
	UpdatedAt                        time.Time                `json:"updated_at"`
	MAC                              string                   `json:"mac"`
}

type CandidateLifecycleStore struct {
	mu        sync.Mutex
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	lock      fsutil.ExclusiveLock
	key       [sha256.Size]byte
	state     CandidateLifecycleStateV1
	now       func() time.Time
	hook      func(string) error
	closed    bool
}

func PrepareCandidateLifecycle(ctx context.Context, fsys fsutil.FileSystem, input CandidatePrepareInputV1, random io.Reader, now func() time.Time) (*CandidateLifecycleStore, CandidateLifecycleStateV1, error) {
	if err := validateCandidatePrepareInput(ctx, fsys, input, random, now); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	inspector := fsys.(fsutil.SecurePathInspector)
	if _, err := inspector.Lstat(input.Root); err == nil {
		return nil, CandidateLifecycleStateV1{}, ErrCandidateLifecycleExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := fsutil.EnsureSecureDirectory(fsys, input.Root); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, CandidateLifecycleStateV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(input.Root)
	if err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = directory.Close()
		}
	}()
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(random, key); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	defer zeroRuntimeBytes(key)
	if err := fsutil.SecureAtomicCreateInDirectory(inspector, directory, candidateLifecycleKeyName, key); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	lockFile, err := directory.CreateExclusive(candidateLifecycleLockName, 0o600)
	if err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := lockFile.Sync(); err != nil {
		_ = lockFile.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := lockFile.Close(); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := directory.Sync(); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, candidateLifecycleLockName)
	if err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	operationID, err := candidateRandomHex(random, 16)
	if err != nil {
		_ = lock.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	validationRunID, err := candidateRandomHex(random, 32)
	if err != nil {
		_ = lock.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	instanceID, err := candidateRandomHex(random, 16)
	if err != nil {
		_ = lock.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	state := CandidateLifecycleStateV1{
		SchemaVersion: 1, Kind: "candidate_lifecycle_v1", OperationID: operationID,
		ValidationRunID: validationRunID, ProxyInstanceID: instanceID, Port: input.Port,
		SourceConfigDigest: input.SourceConfigDigest, TargetReleaseBundleDigest: input.TargetReleaseBundleDigest,
		TargetReleaseSetDigest: input.TargetReleaseSetDigest, ActiveReleaseSetDigest: input.TargetReleaseSetDigest, ClientBuild: input.ClientBuild,
		ClientExecutableDigest:         input.ClientExecutableDigest,
		LocalTokenClientRegistryDigest: input.LocalTokenClientRegistryDigest,
		CredentialMode:                 input.CredentialMode, CredentialManifestDigest: input.CredentialManifestDigest,
		PolicySnapshotDigest: input.PolicySnapshotDigest, PayloadCapture: input.PayloadCapture,
		Phase:      CandidatePhasePrepared,
		Generation: 1, UpdatedAt: now().UTC(),
	}
	store := &CandidateLifecycleStore{ctx: ctx, inspector: inspector, directory: directory, lock: lock, now: now, state: state}
	copy(store.key[:], key)
	if err := fsutil.SecureAtomicCreateInDirectory(inspector, directory, candidateClientRegistryName, input.LocalTokenClientRegistry); err != nil {
		_ = lock.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := store.persist(); err != nil {
		_ = lock.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	cleanup = false
	return store, store.state, nil
}

func OpenCandidateLifecycle(ctx context.Context, fsys fsutil.FileSystem, root string) (*CandidateLifecycleStore, CandidateLifecycleStateV1, error) {
	if ctx == nil || fsys == nil || invalidInspectionRoot(root) {
		return nil, CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	if err := fsutil.ValidateSecureDirectory(fsys, root); err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return nil, CandidateLifecycleStateV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(root)
	if err != nil {
		return nil, CandidateLifecycleStateV1{}, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, candidateLifecycleLockName)
	if err != nil {
		_ = directory.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	key, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, candidateLifecycleKeyName, sha256.Size+1)
	if err != nil || len(key) != sha256.Size {
		_ = lock.Close()
		_ = directory.Close()
		if err != nil {
			return nil, CandidateLifecycleStateV1{}, err
		}
		return nil, CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	defer zeroRuntimeBytes(key)
	body, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, candidateLifecycleStateName, 64<<10)
	if err != nil {
		_ = lock.Close()
		_ = directory.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	state, err := decodeCandidateLifecycle(body, key)
	if err != nil {
		_ = lock.Close()
		_ = directory.Close()
		return nil, CandidateLifecycleStateV1{}, err
	}
	registry, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, candidateClientRegistryName, (64<<10)+1)
	if err != nil || len(registry) == 0 || len(registry) > 64<<10 {
		_ = lock.Close()
		_ = directory.Close()
		if err != nil {
			return nil, CandidateLifecycleStateV1{}, err
		}
		return nil, CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	registryDigest := sha256.Sum256(registry)
	if hex.EncodeToString(registryDigest[:]) != state.LocalTokenClientRegistryDigest {
		_ = lock.Close()
		_ = directory.Close()
		return nil, CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	store := &CandidateLifecycleStore{ctx: ctx, inspector: inspector, directory: directory, lock: lock, now: time.Now, state: state}
	copy(store.key[:], key)
	return store, state, nil
}

func (s *CandidateLifecycleStore) State() CandidateLifecycleStateV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *CandidateLifecycleStore) RuntimeControlToken() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(s.ctx); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte("cq/candidate-runtime-control/v1\x00"))
	_, _ = mac.Write([]byte(s.state.ProxyInstanceID))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(s.state.ValidationRunID))
	return mac.Sum(nil), nil
}

func (s *CandidateLifecycleStore) Apply(ctx context.Context, action CandidateLifecycleAction, effect func(CandidateLifecycleStateV1) (string, error)) (CandidateLifecycleStateV1, error) {
	return s.ApplyTarget(ctx, action, "", effect)
}

func (s *CandidateLifecycleStore) ApplyTarget(ctx context.Context, action CandidateLifecycleAction, targetDigest string, effect func(CandidateLifecycleStateV1) (string, error)) (CandidateLifecycleStateV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	if effect == nil || !candidateActionAllowed(s.state.Phase, action) || (action == CandidateActionValidateRelease && s.state.ClientBearerBarrierReceiptDigest == "") {
		return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	if action == CandidateActionArtifactSwitch {
		if !lowerHexBytes(targetDigest, sha256.Size) {
			return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
		}
	} else if targetDigest != "" {
		return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	if s.state.PendingAction != "" {
		if s.state.EffectStarted {
			return CandidateLifecycleStateV1{}, ErrCandidateEffectIndeterminate
		}
		if s.state.PendingAction != action || s.state.PendingTargetDigest != targetDigest {
			return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
		}
	} else {
		s.state.PendingAction = action
		s.state.EffectStarted = false
		s.state.EffectReceiptDigest = ""
		s.state.PendingTargetDigest = targetDigest
		s.bump()
		if err := s.persist(); err != nil {
			return CandidateLifecycleStateV1{}, err
		}
		if err := s.callHook("intent_durable"); err != nil {
			return CandidateLifecycleStateV1{}, err
		}
	}
	s.state.EffectStarted = true
	s.bump()
	if err := s.persist(); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	if err := s.callHook("effect_started"); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	receipt, err := effect(s.state)
	if err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	if !lowerHexBytes(receipt, sha256.Size) {
		return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	if err := s.callHook("effect_returned"); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	return s.complete(action, receipt)
}

func (s *CandidateLifecycleStore) Reconcile(ctx context.Context, action CandidateLifecycleAction, receipt string) (CandidateLifecycleStateV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	if s.state.PendingAction != action || !s.state.EffectStarted || !lowerHexBytes(receipt, sha256.Size) {
		return CandidateLifecycleStateV1{}, ErrCandidateLifecycleInvalid
	}
	return s.complete(action, receipt)
}

func (s *CandidateLifecycleStore) complete(action CandidateLifecycleAction, receipt string) (CandidateLifecycleStateV1, error) {
	s.state.Phase = candidateActionPhase(s.state.Phase, action)
	switch action {
	case CandidateActionRefreshBarrier:
		s.state.ClientBearerBarrierReceiptDigest = receipt
	case CandidateActionArtifactSwitch:
		s.state.ActiveReleaseSetDigest = s.state.PendingTargetDigest
	case CandidateActionValidateRelease:
		s.state.ValidationReceiptDigest = receipt
	}
	s.state.PendingAction = ""
	s.state.EffectStarted = false
	s.state.EffectReceiptDigest = receipt
	s.state.PendingTargetDigest = ""
	s.bump()
	if err := s.persist(); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	if err := s.callHook("receipt_durable"); err != nil {
		return CandidateLifecycleStateV1{}, err
	}
	return s.state, nil
}

func (s *CandidateLifecycleStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	zeroRuntimeBytes(s.key[:])
	return errors.Join(s.lock.Close(), s.directory.Close())
}

func (s *CandidateLifecycleStore) ready(ctx context.Context) error {
	if s == nil || s.closed || s.directory == nil || s.lock == nil || ctx == nil {
		return ErrCandidateLifecycleInvalid
	}
	return ctx.Err()
}

func (s *CandidateLifecycleStore) bump() {
	s.state.Generation++
	s.state.UpdatedAt = s.now().UTC()
}

func (s *CandidateLifecycleStore) persist() error {
	body, err := encodeCandidateLifecycle(s.state, s.key[:])
	if err != nil {
		return err
	}
	var persisted CandidateLifecycleStateV1
	if err := json.Unmarshal(body, &persisted); err != nil {
		return err
	}
	s.state.MAC = persisted.MAC
	if s.state.Generation == 1 {
		return fsutil.SecureAtomicCreateInDirectory(s.inspector, s.directory, candidateLifecycleStateName, body)
	}
	return fsutil.SecureAtomicWriteInDirectory(s.inspector, s.directory, candidateLifecycleStateName, body)
}

func (s *CandidateLifecycleStore) callHook(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

func validateCandidatePrepareInput(ctx context.Context, fsys fsutil.FileSystem, input CandidatePrepareInputV1, random io.Reader, now func() time.Time) error {
	if ctx == nil || fsys == nil || random == nil || now == nil || invalidInspectionRoot(input.Root) || input.Port < 1 || input.Port > 65535 || input.Port == DefaultPort || input.ClientBuild == "" || len(input.LocalTokenClientRegistry) == 0 || len(input.LocalTokenClientRegistry) > 64<<10 {
		return ErrCandidateLifecycleInvalid
	}
	if _, ok := fsys.(fsutil.SecurePathInspector); !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	for _, digest := range []string{input.SourceConfigDigest, input.TargetReleaseBundleDigest, input.TargetReleaseSetDigest, input.ClientExecutableDigest, input.LocalTokenClientRegistryDigest} {
		if !lowerHexBytes(digest, sha256.Size) {
			return ErrCandidateLifecycleInvalid
		}
	}
	registryDigest := sha256.Sum256(input.LocalTokenClientRegistry)
	if hex.EncodeToString(registryDigest[:]) != input.LocalTokenClientRegistryDigest {
		return ErrCandidateLifecycleInvalid
	}
	if input.PolicySnapshotDigest != "" && !lowerHexBytes(input.PolicySnapshotDigest, sha256.Size) {
		return ErrCandidateLifecycleInvalid
	}
	switch input.CredentialMode {
	case "none":
		if input.CredentialManifestDigest != "" {
			return ErrCandidateLifecycleInvalid
		}
	case "read-only":
		if !lowerHexBytes(input.CredentialManifestDigest, sha256.Size) {
			return ErrCandidateLifecycleInvalid
		}
	default:
		return ErrCandidateLifecycleInvalid
	}
	return ctx.Err()
}

func candidateRandomHex(random io.Reader, size int) (string, error) {
	body := make([]byte, size)
	if _, err := io.ReadFull(random, body); err != nil {
		return "", err
	}
	return hex.EncodeToString(body), nil
}

func encodeCandidateLifecycle(state CandidateLifecycleStateV1, key []byte) ([]byte, error) {
	state.MAC = ""
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/candidate-lifecycle/v1\x00"))
	_, _ = mac.Write(body)
	state.MAC = hex.EncodeToString(mac.Sum(nil))
	return json.Marshal(state)
}

func decodeCandidateLifecycle(body, key []byte) (CandidateLifecycleStateV1, error) {
	var state CandidateLifecycleStateV1
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return state, ErrCandidateLifecycleInvalid
	}
	want := state.MAC
	canonical, err := encodeCandidateLifecycle(state, key)
	if err != nil {
		return state, err
	}
	var authenticated CandidateLifecycleStateV1
	if err := json.Unmarshal(canonical, &authenticated); err != nil || !hmac.Equal([]byte(want), []byte(authenticated.MAC)) || !validCandidateLifecycleState(state) {
		return state, ErrCandidateLifecycleInvalid
	}
	state.MAC = want
	return state, nil
}

func validCandidateLifecycleState(state CandidateLifecycleStateV1) bool {
	if state.SchemaVersion != 1 || state.Kind != "candidate_lifecycle_v1" || !lowerHexBytes(state.OperationID, 16) || !lowerHexBytes(state.ValidationRunID, 32) || !lowerHexBytes(state.ProxyInstanceID, 16) || state.Port < 1 || state.Port > 65535 || state.Port == DefaultPort || state.ClientBuild == "" || state.Generation == 0 || state.UpdatedAt.IsZero() || !state.UpdatedAt.Equal(state.UpdatedAt.UTC()) {
		return false
	}
	for _, digest := range []string{state.SourceConfigDigest, state.TargetReleaseBundleDigest, state.TargetReleaseSetDigest, state.ActiveReleaseSetDigest, state.ClientExecutableDigest, state.LocalTokenClientRegistryDigest} {
		if !lowerHexBytes(digest, sha256.Size) {
			return false
		}
	}
	if state.PolicySnapshotDigest != "" && !lowerHexBytes(state.PolicySnapshotDigest, sha256.Size) {
		return false
	}
	for _, digest := range []string{state.ClientBearerBarrierReceiptDigest, state.ValidationReceiptDigest, state.PendingTargetDigest} {
		if digest != "" && !lowerHexBytes(digest, sha256.Size) {
			return false
		}
	}
	if state.CredentialMode == "none" {
		if state.CredentialManifestDigest != "" {
			return false
		}
	} else if state.CredentialMode != "read-only" || !lowerHexBytes(state.CredentialManifestDigest, sha256.Size) {
		return false
	}
	switch state.Phase {
	case CandidatePhasePrepared, CandidatePhaseRunning, CandidatePhaseStopped, CandidatePhaseValidated, CandidatePhaseRemoved:
	default:
		return false
	}
	if state.PendingAction == "" {
		return !state.EffectStarted && state.PendingTargetDigest == ""
	}
	if (state.PendingAction == CandidateActionArtifactSwitch) != (state.PendingTargetDigest != "") {
		return false
	}
	return candidateActionAllowed(state.Phase, state.PendingAction) && (state.EffectReceiptDigest == "" || lowerHexBytes(state.EffectReceiptDigest, sha256.Size))
}

func candidateActionAllowed(phase CandidateLifecyclePhase, action CandidateLifecycleAction) bool {
	switch action {
	case CandidateActionStart:
		return phase == CandidatePhasePrepared || phase == CandidatePhaseStopped || phase == CandidatePhaseValidated
	case CandidateActionStop:
		return phase == CandidatePhaseRunning
	case CandidateActionRefreshBarrier:
		return phase == CandidatePhasePrepared || phase == CandidatePhaseStopped
	case CandidateActionArtifactSwitch:
		return phase == CandidatePhaseRunning
	case CandidateActionValidateRelease:
		return phase == CandidatePhaseRunning
	case CandidateActionRemove:
		return phase == CandidatePhasePrepared || phase == CandidatePhaseStopped || phase == CandidatePhaseValidated
	default:
		return false
	}
}

func candidateActionPhase(current CandidateLifecyclePhase, action CandidateLifecycleAction) CandidateLifecyclePhase {
	switch action {
	case CandidateActionStart:
		return CandidatePhaseRunning
	case CandidateActionStop:
		return CandidatePhaseStopped
	case CandidateActionRefreshBarrier, CandidateActionArtifactSwitch:
		return current
	case CandidateActionValidateRelease:
		return CandidatePhaseValidated
	case CandidateActionRemove:
		return CandidatePhaseRemoved
	default:
		return ""
	}
}

func CandidateEffectReceiptDigest(action CandidateLifecycleAction, material []byte) string {
	digest := sha256.Sum256(append(append([]byte("cq/candidate-lifecycle-effect/v1\x00"), []byte(action)...), material...))
	return hex.EncodeToString(digest[:])
}
