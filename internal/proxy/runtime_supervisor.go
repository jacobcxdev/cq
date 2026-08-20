package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type RuntimeSupervisorRoleDependencies struct {
	Files            RuntimeRoleFiles
	SupervisorHolder LifecycleHolderProof
	Launcher         RuntimeWorkerLauncher
	Checkpoints      RuntimeHolderCheckpointStore
	WorkerManifest   WorkerManifestV1
	AdoptListener    func(*os.File) (net.Listener, error)
	Serve            func(context.Context, net.Listener, http.Handler) error
}

func RunRuntimeSupervisorRole(ctx context.Context, manifest RuntimeRoleManifestV1, dependencies RuntimeSupervisorRoleDependencies) error {
	files := dependencies.Files
	if manifest.Role != RuntimeRoleSupervisor || ValidateRuntimeRoleFiles(manifest, files) != nil || dependencies.Serve == nil {
		_ = files.Close()
		return ErrRuntimeRoleManifest
	}
	secret, err := ReadRuntimeSecret(files.Secret)
	files.Secret = nil
	if err != nil {
		_ = files.Close()
		return err
	}
	secret.Destroy()
	dependencies.Files = files
	return RunValidatedRuntimeSupervisorRole(ctx, manifest, dependencies)
}

func RunValidatedRuntimeSupervisorRole(ctx context.Context, manifest RuntimeRoleManifestV1, dependencies RuntimeSupervisorRoleDependencies) error {
	files := dependencies.Files
	defer files.Close()
	if manifest.Role != RuntimeRoleSupervisor || ValidateRuntimeRoleDescriptors(manifest, files) != nil || dependencies.Serve == nil {
		return ErrRuntimeRoleManifest
	}
	adopt := dependencies.AdoptListener
	if adopt == nil {
		adopt = net.FileListener
	}
	listener, err := adopt(files.Listener)
	if err != nil {
		return err
	}
	_ = files.Listener.Close()
	files.Listener = nil
	defer listener.Close()
	supervisor, err := NewRuntimeSupervisor(listener, dependencies.SupervisorHolder, dependencies.Launcher, dependencies.Checkpoints)
	if err != nil {
		return err
	}
	if _, err := supervisor.Boot(ctx, dependencies.WorkerManifest); err != nil {
		return err
	}
	return dependencies.Serve(ctx, listener, supervisor)
}

func RunAdoptedRuntimeSupervisor(ctx context.Context, listener net.Listener, holder LifecycleHolderProof, launcher RuntimeWorkerLauncher, checkpoints RuntimeHolderCheckpointStore, workerManifest WorkerManifestV1, serve func(context.Context, net.Listener, http.Handler) error) (returnErr error) {
	supervisor, err := NewRuntimeSupervisor(listener, holder, launcher, checkpoints)
	if err != nil {
		return err
	}
	if _, err := supervisor.Boot(ctx, workerManifest); err != nil {
		return err
	}
	defer func() {
		supervisor.mu.Lock()
		defer supervisor.mu.Unlock()
		supervisor.admissionReady = false
		if supervisor.worker != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, reapErr := supervisor.worker.StopAndReap(cleanupCtx)
			returnErr = errors.Join(returnErr, reapErr)
			supervisor.worker = nil
		}
	}()
	return serve(ctx, listener, supervisor)
}

type RuntimeHashCheckpointStore struct {
	mu      sync.Mutex
	current string
}

func (store *RuntimeHashCheckpointStore) Select(_ context.Context, checkpoint RuntimeHolderCheckpointV1) (string, error) {
	if store == nil {
		return "", ErrRuntimeCheckpointUnavailable
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if checkpoint.PreviousCheckpointDigest != store.current {
		return "", ErrRuntimeCheckpointUnavailable
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("cq/runtime-holder-checkpoint/v1\x00"), encoded...))
	store.current = hex.EncodeToString(digest[:])
	return store.current, nil
}

var (
	ErrRuntimeSupervisorUnavailable = errors.New("runtime supervisor unavailable")
	ErrRuntimeCrashLoop             = errors.New("runtime worker crash loop")
	ErrRuntimeGeneration            = errors.New("runtime generation mismatch")
	ErrRuntimeRecoveryPending       = errors.New("runtime worker recovery pending")
)

const (
	runtimeCrashWindow = 10 * time.Minute
	runtimeCrashLimit  = 3
)

type RuntimeWorkerProcess interface {
	Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error)
	BeginDrain(context.Context, TrafficMode, uint64) error
	AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error)
	StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error)
	ExecuteHTTP(context.Context, RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error)
	HolderProof() LifecycleHolderProof
}

type RuntimeWorkerReleaseV1 struct {
	ProcessIdentityDigest         string `json:"process_identity_digest"`
	ProcessTreeAbsenceProofDigest string `json:"process_tree_absence_proof_digest"`
	HolderReleaseProofDigest      string `json:"holder_release_proof_digest"`
}

func (release RuntimeWorkerReleaseV1) valid() bool {
	return release.ProcessIdentityDigest != "" && release.ProcessTreeAbsenceProofDigest != "" && release.HolderReleaseProofDigest != ""
}

type RuntimeWorkerLauncher interface {
	Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error)
}

type RuntimeHolderCheckpointStore interface {
	Select(context.Context, RuntimeHolderCheckpointV1) (string, error)
}

type RuntimeHolderCheckpointV1 struct {
	SchemaVersion                            int                        `json:"schema_version"`
	Kind                                     string                     `json:"kind"`
	CheckpointKind                           string                     `json:"checkpoint_kind"`
	Sequence                                 uint64                     `json:"sequence"`
	PreviousCheckpointDigest                 string                     `json:"previous_checkpoint_digest,omitempty"`
	LifecycleLockHolders                     []RuntimeLifecycleHolderV1 `json:"lifecycle_lock_holders"`
	WorkerArtifactDigest                     string                     `json:"worker_artifact_digest"`
	PriorWorkerProcessIdentityDigest         string                     `json:"prior_worker_process_identity_digest,omitempty"`
	PriorWorkerProcessTreeAbsenceProofDigest string                     `json:"prior_worker_process_tree_absence_proof_digest,omitempty"`
	PriorWorkerHolderReleaseProofDigest      string                     `json:"prior_worker_holder_release_proof_digest,omitempty"`
}

type RuntimeLifecycleHolderV1 struct {
	Role   RuntimeRole          `json:"role"`
	Holder LifecycleHolderProof `json:"holder"`
}

type RuntimeSupervisor struct {
	mu sync.RWMutex

	listener         net.Listener
	listenerIdentity string
	supervisorHolder LifecycleHolderProof
	launcher         RuntimeWorkerLauncher
	checkpoints      RuntimeHolderCheckpointStore
	worker           RuntimeWorkerProcess
	checkpointDigest string
	sequence         uint64
	admissionReady   bool
	now              func() time.Time
	crashStarts      []time.Time
	crashLoop        bool
	recoveryPending  bool
	pendingRelease   RuntimeWorkerReleaseV1
}

func NewRuntimeSupervisor(listener net.Listener, supervisorHolder LifecycleHolderProof, launcher RuntimeWorkerLauncher, checkpoints RuntimeHolderCheckpointStore) (*RuntimeSupervisor, error) {
	if listener == nil || launcher == nil || checkpoints == nil || supervisorHolder.Mode != LifecycleShared ||
		supervisorHolder.DescriptionID == "" || supervisorHolder.LockIdentity.Links != 1 {
		return nil, ErrRuntimeSupervisorUnavailable
	}
	return &RuntimeSupervisor{
		listener: listener, listenerIdentity: listener.Addr().Network() + "|" + listener.Addr().String(),
		supervisorHolder: supervisorHolder, launcher: launcher, checkpoints: checkpoints, now: time.Now,
	}, nil
}

func (supervisor *RuntimeSupervisor) ListenerIdentity() string {
	if supervisor == nil {
		return ""
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	return supervisor.listenerIdentity
}

func (supervisor *RuntimeSupervisor) AdmissionReady() bool {
	if supervisor == nil {
		return false
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	return supervisor.admissionReady
}

func (supervisor *RuntimeSupervisor) PendingRelease() RuntimeWorkerReleaseV1 {
	if supervisor == nil {
		return RuntimeWorkerReleaseV1{}
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	return supervisor.pendingRelease
}

func (supervisor *RuntimeSupervisor) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if supervisor == nil || request == nil {
		http.Error(writer, "runtime worker unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, RuntimeHTTPBodyLimit+1))
	if err != nil || len(body) > RuntimeHTTPBodyLimit {
		http.Error(writer, "runtime request exceeds private transport limit", http.StatusRequestEntityTooLarge)
		return
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	if !supervisor.admissionReady || supervisor.worker == nil {
		http.Error(writer, "runtime worker unavailable", http.StatusServiceUnavailable)
		return
	}
	response, err := supervisor.worker.ExecuteHTTP(request.Context(), RuntimeHTTPRequestV1{Method: request.Method, RequestURI: request.URL.RequestURI(), Header: request.Header.Clone(), Body: body})
	if err != nil {
		http.Error(writer, "runtime worker unavailable", http.StatusServiceUnavailable)
		return
	}
	for name, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(response.Body)
}

func (supervisor *RuntimeSupervisor) Boot(ctx context.Context, manifest WorkerManifestV1) (RuntimeBootAckV1, error) {
	if supervisor == nil {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.recoveryPending {
		return RuntimeBootAckV1{}, ErrRuntimeRecoveryPending
	}
	if supervisor.crashLoop {
		return RuntimeBootAckV1{}, ErrRuntimeCrashLoop
	}
	if supervisor.worker != nil || supervisor.admissionReady {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	return supervisor.bootLocked(ctx, manifest, "boot", RuntimeWorkerReleaseV1{})
}

func (supervisor *RuntimeSupervisor) bootLocked(ctx context.Context, manifest WorkerManifestV1, checkpointKind string, prior RuntimeWorkerReleaseV1) (RuntimeBootAckV1, error) {
	if ctx == nil || manifest.SchemaVersion != 1 || manifest.WorkerArtifactDigest == "" {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RuntimeBootAckV1{}, err
	}
	worker, err := supervisor.launcher.Launch(ctx, manifest)
	if err != nil || worker == nil {
		return RuntimeBootAckV1{}, errors.Join(ErrRuntimeSupervisorUnavailable, err)
	}
	ack, err := worker.Boot(ctx, manifest)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = worker.StopAndReap(cleanupCtx)
		return RuntimeBootAckV1{}, err
	}
	workerHolder := worker.HolderProof()
	if ack.SchemaVersion != 1 || ack.Kind != "runtime_boot_ack_v1" || ack.Holder != workerHolder ||
		ValidateDistinctLifecycleHolders(supervisor.supervisorHolder, workerHolder) != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = worker.StopAndReap(cleanupCtx)
		return RuntimeBootAckV1{}, ErrLifecycleHolderConflict
	}
	checkpoint := RuntimeHolderCheckpointV1{
		SchemaVersion:            1,
		Kind:                     "runtime_holder_checkpoint_v1",
		CheckpointKind:           checkpointKind,
		Sequence:                 supervisor.sequence,
		PreviousCheckpointDigest: supervisor.checkpointDigest,
		LifecycleLockHolders: []RuntimeLifecycleHolderV1{
			{Role: RuntimeRoleSupervisor, Holder: supervisor.supervisorHolder},
			{Role: RuntimeRoleWorker, Holder: workerHolder},
		},
		WorkerArtifactDigest:                     manifest.WorkerArtifactDigest,
		PriorWorkerProcessIdentityDigest:         prior.ProcessIdentityDigest,
		PriorWorkerProcessTreeAbsenceProofDigest: prior.ProcessTreeAbsenceProofDigest,
		PriorWorkerHolderReleaseProofDigest:      prior.HolderReleaseProofDigest,
	}
	digest, err := supervisor.checkpoints.Select(ctx, checkpoint)
	if err != nil || digest == "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = worker.StopAndReap(cleanupCtx)
		return RuntimeBootAckV1{}, errors.Join(ErrRuntimeCheckpointUnavailable, err)
	}
	supervisor.worker = worker
	supervisor.checkpointDigest = digest
	supervisor.admissionReady = true
	return ack, nil
}

func (supervisor *RuntimeSupervisor) ReplaceWorker(ctx context.Context, manifest WorkerManifestV1) (RuntimeBootAckV1, error) {
	if supervisor == nil {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if ctx == nil || supervisor.worker == nil || !supervisor.admissionReady {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	previous := supervisor.worker
	supervisor.admissionReady = false
	if err := previous.BeginDrain(ctx, TrafficModeDrain, supervisor.sequence); err != nil {
		return RuntimeBootAckV1{}, err
	}
	quiescence, err := previous.AwaitQuiescence(ctx, supervisor.sequence)
	if err != nil || quiescence.SchemaVersion != 1 || !quiescence.Quiescent {
		return RuntimeBootAckV1{}, errors.Join(ErrRuntimeSupervisorUnavailable, err)
	}
	release, err := previous.StopAndReap(ctx)
	if err != nil || !release.valid() {
		if err == nil {
			err = ErrRuntimeSupervisorUnavailable
		}
		return RuntimeBootAckV1{}, err
	}
	supervisor.worker = nil
	supervisor.sequence++
	ack, err := supervisor.bootLocked(ctx, manifest, "worker_switch", release)
	if err == nil {
		supervisor.crashStarts = nil
		supervisor.crashLoop = false
		supervisor.pendingRelease = RuntimeWorkerReleaseV1{}
	} else {
		supervisor.recoveryPending = true
		supervisor.pendingRelease = release
	}
	return ack, err
}

func (supervisor *RuntimeSupervisor) allowCrashStartLocked() bool {
	now := supervisor.now()
	cutoff := now.Add(-runtimeCrashWindow)
	kept := supervisor.crashStarts[:0]
	for _, started := range supervisor.crashStarts {
		if started.After(cutoff) {
			kept = append(kept, started)
		}
	}
	supervisor.crashStarts = kept
	if len(supervisor.crashStarts) >= runtimeCrashLimit {
		return false
	}
	supervisor.crashStarts = append(supervisor.crashStarts, now)
	return true
}

// ReplaceFailedWorker reaps a failed worker before launching its successor.
// It never closes or replaces the inherited public listener.
func (supervisor *RuntimeSupervisor) ReplaceFailedWorker(ctx context.Context, manifest WorkerManifestV1) (RuntimeBootAckV1, error) {
	if supervisor == nil {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if ctx == nil || supervisor.worker == nil {
		return RuntimeBootAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	supervisor.admissionReady = false
	release, err := supervisor.worker.StopAndReap(ctx)
	if err != nil || !release.valid() {
		if err == nil {
			err = ErrRuntimeSupervisorUnavailable
		}
		return RuntimeBootAckV1{}, err
	}
	supervisor.worker = nil
	supervisor.sequence++
	if !supervisor.allowCrashStartLocked() {
		supervisor.crashLoop = true
		return RuntimeBootAckV1{}, ErrRuntimeCrashLoop
	}
	ack, err := supervisor.bootLocked(ctx, manifest, "worker_switch", release)
	if err != nil {
		supervisor.recoveryPending = true
		supervisor.pendingRelease = release
	}
	return ack, err
}

func (supervisor *RuntimeSupervisor) BeginDrain(ctx context.Context, mode TrafficMode, generation uint64) error {
	if supervisor == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	if generation != supervisor.sequence {
		return ErrRuntimeGeneration
	}
	if supervisor.worker == nil || (mode != TrafficModeNormal && mode != TrafficModeDrain) {
		return ErrRuntimeSupervisorUnavailable
	}
	return supervisor.worker.BeginDrain(ctx, mode, generation)
}

func (supervisor *RuntimeSupervisor) AwaitQuiescence(ctx context.Context, generation uint64) (RuntimeQuiescenceAckV1, error) {
	if supervisor == nil {
		return RuntimeQuiescenceAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	if generation != supervisor.sequence {
		return RuntimeQuiescenceAckV1{}, ErrRuntimeGeneration
	}
	if supervisor.worker == nil {
		return RuntimeQuiescenceAckV1{}, ErrRuntimeSupervisorUnavailable
	}
	return supervisor.worker.AwaitQuiescence(ctx, generation)
}
