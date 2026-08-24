package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
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
	CallerAdmissions NormalCallerAdmissionConsumer
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
	supervisor.setLifetimeContext(ctx)
	if dependencies.CallerAdmissions != nil {
		if err := supervisor.SetCallerAdmissionConsumer(dependencies.CallerAdmissions); err != nil {
			return err
		}
	}
	if _, err := supervisor.Boot(ctx, dependencies.WorkerManifest); err != nil {
		return err
	}
	return dependencies.Serve(ctx, listener, supervisor)
}

func RunAdoptedRuntimeSupervisor(ctx context.Context, listener net.Listener, holder LifecycleHolderProof, launcher RuntimeWorkerLauncher, checkpoints RuntimeHolderCheckpointStore, admissions NormalCallerAdmissionConsumer, workerManifest WorkerManifestV1, serve func(context.Context, net.Listener, http.Handler) error) (returnErr error) {
	return RunAdoptedRuntimeSupervisorConfigured(ctx, listener, holder, launcher, checkpoints, admissions, workerManifest, nil, serve)
}

func RunAdoptedRuntimeSupervisorConfigured(ctx context.Context, listener net.Listener, holder LifecycleHolderProof, launcher RuntimeWorkerLauncher, checkpoints RuntimeHolderCheckpointStore, admissions NormalCallerAdmissionConsumer, workerManifest WorkerManifestV1, configure func(*RuntimeSupervisor) error, serve func(context.Context, net.Listener, http.Handler) error) (returnErr error) {
	supervisor, err := NewRuntimeSupervisor(listener, holder, launcher, checkpoints)
	if err != nil {
		return err
	}
	supervisor.setLifetimeContext(ctx)
	if admissions != nil {
		if err := supervisor.SetCallerAdmissionConsumer(admissions); err != nil {
			return err
		}
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
	supervisor.mu.Lock()
	supervisor.workerManifest = workerManifest
	supervisor.mu.Unlock()
	if configure != nil {
		if err := configure(supervisor); err != nil {
			return err
		}
	}
	var startupErr error
	switch supervisor.TrafficMode() {
	case TrafficModeNormal:
		if _, err := supervisor.Boot(ctx, workerManifest); err != nil {
			startupErr = err
		}
	case TrafficModeRescue:
		// Durable rescue mode intentionally starts without a normal worker.
	case TrafficModeRescueDraining, TrafficModeRescueExitDraining:
		if err := supervisor.ReconcileRescue(ctx, workerManifest); err != nil {
			return err
		}
	default:
		return ErrRuntimeSupervisorUnavailable
	}
	return errors.Join(serve(ctx, listener, supervisor), startupErr)
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
	runtimeCrashWindow      = 10 * time.Minute
	runtimeCrashLimit       = 3
	RuntimeRescueEnterPath  = "/_cq/control/rescue/enter"
	RuntimeRescueExitPath   = "/_cq/control/rescue/exit"
	RuntimeRescueStatusPath = "/_cq/control/rescue/status"
	runtimeRescueDrainLimit = 5 * time.Second
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
	workerManifest   WorkerManifestV1
	checkpointDigest string
	sequence         uint64
	admissionReady   bool
	now              func() time.Time
	crashStarts      []time.Time
	crashLoop        bool
	recoveryPending  bool
	pendingRelease   RuntimeWorkerReleaseV1
	callerAuthority  *NormalCallerAuthority
	callerClassifier NormalCallerBranchClassifier
	callerAdmissions NormalCallerAdmissionConsumer
	trafficMode      TrafficMode
	modeGeneration   uint64
	modeEvidence     RuntimeModeEvidenceStore
	rescueHandler    http.Handler
	normalAdmitted   int
	rescueAdmitted   int
	normalZero       chan struct{}
	rescueZero       chan struct{}
	lifetimeCtx      context.Context
	rescueEntryRun   bool
}

func (supervisor *RuntimeSupervisor) SetCallerAdmissionConsumer(consumer NormalCallerAdmissionConsumer) error {
	if supervisor == nil || consumer == nil {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.worker != nil || supervisor.admissionReady {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.callerAdmissions = consumer
	if supervisor.callerClassifier == nil {
		supervisor.callerClassifier = NewNormalCallerBranchClassifier(nil)
	}
	return nil
}

func (supervisor *RuntimeSupervisor) SetCallerClassifier(classifier NormalCallerBranchClassifier) error {
	if supervisor == nil || classifier == nil {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.callerClassifier = classifier
	return nil
}

// SetCallerAuthority installs the pre-body normal-route authentication gate.
// It must be called before the adopted listener begins serving.
func (supervisor *RuntimeSupervisor) SetCallerAuthority(authority *NormalCallerAuthority) error {
	if supervisor == nil || authority == nil {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.callerAuthority != nil {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.callerAuthority = authority
	return nil
}

func NewRuntimeSupervisor(listener net.Listener, supervisorHolder LifecycleHolderProof, launcher RuntimeWorkerLauncher, checkpoints RuntimeHolderCheckpointStore) (*RuntimeSupervisor, error) {
	if listener == nil || launcher == nil || checkpoints == nil || supervisorHolder.Mode != LifecycleShared ||
		supervisorHolder.DescriptionID == "" || supervisorHolder.LockIdentity.Links != 1 {
		return nil, ErrRuntimeSupervisorUnavailable
	}
	return &RuntimeSupervisor{
		listener: listener, listenerIdentity: listener.Addr().Network() + "|" + listener.Addr().String(),
		supervisorHolder: supervisorHolder, launcher: launcher, checkpoints: checkpoints, now: time.Now,
		trafficMode: TrafficModeNormal, normalZero: closedRuntimeWaitChannel(), rescueZero: closedRuntimeWaitChannel(),
		lifetimeCtx: context.Background(),
	}, nil
}

func (supervisor *RuntimeSupervisor) setLifetimeContext(ctx context.Context) {
	if supervisor == nil || ctx == nil {
		return
	}
	supervisor.mu.Lock()
	supervisor.lifetimeCtx = ctx
	supervisor.mu.Unlock()
}

func closedRuntimeWaitChannel() chan struct{} {
	channel := make(chan struct{})
	close(channel)
	return channel
}

func (supervisor *RuntimeSupervisor) TrafficMode() TrafficMode {
	if supervisor == nil {
		return ""
	}
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	return supervisor.trafficMode
}

func (supervisor *RuntimeSupervisor) ConfigureRescue(ctx context.Context, handler http.Handler, evidence RuntimeModeEvidenceStore) error {
	if supervisor == nil || ctx == nil || handler == nil || evidence == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	record, found, err := evidence.Load(ctx)
	if err != nil {
		return err
	}
	if found {
		if err := record.validate(); err != nil {
			return err
		}
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.modeEvidence != nil {
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.rescueHandler = handler
	supervisor.modeEvidence = evidence
	if found {
		supervisor.modeGeneration = record.Generation
		supervisor.trafficMode = record.EffectiveMode
		if record.EffectiveMode != TrafficModeNormal {
			supervisor.admissionReady = false
		}
	}
	return nil
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

func (supervisor *RuntimeSupervisor) DeniesNormalBearer(bearer []byte) bool {
	if supervisor == nil {
		return false
	}
	supervisor.mu.RLock()
	authority := supervisor.callerAuthority
	supervisor.mu.RUnlock()
	return authority.DeniesBearer(bearer)
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
	if request.URL != nil && (request.URL.EscapedPath() == RuntimeRescueEnterPath || request.URL.EscapedPath() == RuntimeRescueExitPath || request.URL.EscapedPath() == RuntimeRescueStatusPath) {
		supervisor.serveRescueControl(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL != nil && request.URL.EscapedPath() == "/health" {
		supervisor.mu.RLock()
		mode := supervisor.trafficMode
		supervisor.mu.RUnlock()
		if mode != TrafficModeNormal {
			writeRuntimeSupervisorHealth(writer, mode)
			return
		}
	}
	supervisor.mu.Lock()
	switch supervisor.trafficMode {
	case TrafficModeRescue, TrafficModeRescueDraining:
		handler := supervisor.rescueHandler
		if handler == nil {
			supervisor.mu.Unlock()
			http.Error(writer, "rescue unavailable", http.StatusServiceUnavailable)
			return
		}
		if supervisor.rescueAdmitted == 0 {
			supervisor.rescueZero = make(chan struct{})
		}
		supervisor.rescueAdmitted++
		supervisor.mu.Unlock()
		defer supervisor.releaseRescueAdmission()
		handler.ServeHTTP(writer, request)
		return
	case TrafficModeRescueExitDraining:
		supervisor.mu.Unlock()
		http.Error(writer, "mode changed", http.StatusServiceUnavailable)
		return
	case TrafficModeNormal:
		supervisor.mu.Unlock()
	default:
		supervisor.mu.Unlock()
		http.Error(writer, "runtime mode unavailable", http.StatusServiceUnavailable)
		return
	}
	policy := normalCallerPolicy(request)
	var caller RuntimeCallerAuthorityV1
	var authentication normalCallerAuthentication
	var authority *NormalCallerAuthority
	var err error
	if policy != normalCallerRoutePublic {
		supervisor.mu.RLock()
		authority = supervisor.callerAuthority
		supervisor.mu.RUnlock()
		if err := supervisor.refreshCallerAuthority(request.Context(), authority); err != nil {
			http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		authentication, err = authority.authenticate(request, policy)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrNormalCallerAuthRequired) {
				status = http.StatusUnauthorized
			} else if errors.Is(err, ErrNormalCallerAuthScope) {
				status = http.StatusForbidden
			}
			http.Error(writer, http.StatusText(status), status)
			return
		}
	}
	var body []byte
	if policy != normalCallerRoutePublic {
		if policy == normalCallerRouteClassified {
			body, err = io.ReadAll(io.LimitReader(request.Body, RuntimeHTTPBodyLimit+1))
			if err != nil || len(body) > RuntimeHTTPBodyLimit {
				http.Error(writer, "runtime request exceeds limit", http.StatusRequestEntityTooLarge)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			supervisor.mu.RLock()
			classifier := supervisor.callerClassifier
			supervisor.mu.RUnlock()
			if classifier == nil {
				http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			branch, classifyErr := classifier(request.Method, request.URL.RequestURI(), body)
			if classifyErr != nil || !normalCallerAllowsBranch(authentication.domain, branch) {
				http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}
		}
		caller, err = authority.consume(request.Context(), authentication, request)
		if err != nil {
			http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
	}
	supervisor.mu.Lock()
	if !supervisor.admissionReady || supervisor.worker == nil {
		supervisor.mu.Unlock()
		writeRuntimeWorkerUnavailable(writer, request)
		return
	}
	worker := supervisor.worker
	if supervisor.normalAdmitted == 0 {
		supervisor.normalZero = make(chan struct{})
	}
	supervisor.normalAdmitted++
	supervisor.mu.Unlock()
	defer supervisor.releaseNormalAdmission()
	header := request.Header.Clone()
	header.Del("Authorization")
	header.Del("Proxy-Authorization")
	request.Header = header
	if streaming, ok := worker.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request, RuntimeCallerAuthorityV1) error
	}); ok {
		_ = streaming.ServeHTTP(writer, request, caller)
		return
	}
	response, err := worker.ExecuteHTTP(request.Context(), RuntimeHTTPRequestV1{Method: request.Method, RequestURI: request.URL.RequestURI(), Header: header, Body: body, Caller: caller})
	if err != nil {
		writeRuntimeWorkerUnavailable(writer, request)
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

func writeRuntimeWorkerUnavailable(writer http.ResponseWriter, request *http.Request) {
	if request != nil && request.Method == http.MethodGet && request.URL != nil && request.URL.EscapedPath() == "/health" {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "{\"status\":\"degraded\",\"supervisor_alive\":true,\"data_plane_ready\":false}\n")
		return
	}
	http.Error(writer, "runtime worker unavailable", http.StatusServiceUnavailable)
}

func writeRuntimeSupervisorHealth(writer http.ResponseWriter, mode TrafficMode) {
	ready := mode == TrafficModeRescue || mode == TrafficModeRescueDraining
	status := "ok"
	statusCode := http.StatusOK
	if !ready {
		status = "degraded"
		statusCode = http.StatusServiceUnavailable
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	_ = json.NewEncoder(writer).Encode(struct {
		Status          string      `json:"status"`
		SupervisorAlive bool        `json:"supervisor_alive"`
		DataPlaneReady  bool        `json:"data_plane_ready"`
		Mode            TrafficMode `json:"mode"`
	}{Status: status, SupervisorAlive: true, DataPlaneReady: ready, Mode: mode})
}

func (supervisor *RuntimeSupervisor) refreshCallerAuthority(ctx context.Context, authority *NormalCallerAuthority) error {
	if supervisor == nil || authority == nil || ctx == nil {
		return ErrNormalCallerAuthUnavailable
	}
	supervisor.mu.RLock()
	worker := supervisor.worker
	provider, refreshes := worker.(interface {
		CallerIndex(context.Context) (NormalCallerIndexV1, error)
	})
	supervisor.mu.RUnlock()
	if !refreshes {
		return nil
	}
	index, err := provider.CallerIndex(ctx)
	if err != nil {
		return err
	}
	supervisor.mu.RLock()
	current := supervisor.worker == worker && supervisor.callerAuthority == authority
	supervisor.mu.RUnlock()
	if !current {
		return ErrNormalCallerAuthUnavailable
	}
	return authority.UpdateFromIndex(index)
}

func (supervisor *RuntimeSupervisor) serveRescueControl(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.EscapedPath()
	wantMethod := http.MethodPost
	if path == RuntimeRescueStatusPath {
		wantMethod = http.MethodGet
	}
	if request.Method != wantMethod {
		writer.Header().Set("Allow", wantMethod)
		http.Error(writer, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	supervisor.mu.RLock()
	authority := supervisor.callerAuthority
	supervisor.mu.RUnlock()
	authentication, err := authority.authenticate(request, normalCallerRouteLocal)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrNormalCallerAuthScope) {
			status = http.StatusForbidden
		} else if errors.Is(err, ErrNormalCallerAuthUnavailable) {
			status = http.StatusServiceUnavailable
		}
		http.Error(writer, http.StatusText(status), status)
		return
	}
	if request.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 1))
		if readErr != nil || len(body) != 0 {
			http.Error(writer, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
	}
	if _, err := authority.consume(request.Context(), authentication, request); err != nil {
		http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	switch path {
	case RuntimeRescueEnterPath:
		err = supervisor.EnterRescue(request.Context())
	case RuntimeRescueExitPath:
		supervisor.mu.RLock()
		manifest := supervisor.workerManifest
		supervisor.mu.RUnlock()
		err = supervisor.ExitRescue(request.Context(), manifest)
	case RuntimeRescueStatusPath:
		// Authenticated read only.
	default:
		err = ErrRuntimeSupervisorUnavailable
	}
	if err != nil {
		http.Error(writer, "rescue transition unavailable", http.StatusConflict)
		return
	}
	supervisor.mu.RLock()
	response := struct {
		Mode       TrafficMode `json:"mode"`
		Generation uint64      `json:"generation"`
	}{Mode: supervisor.trafficMode, Generation: supervisor.modeGeneration}
	supervisor.mu.RUnlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}

func (supervisor *RuntimeSupervisor) releaseNormalAdmission() {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.normalAdmitted--
	if supervisor.normalAdmitted == 0 {
		close(supervisor.normalZero)
	}
}

func (supervisor *RuntimeSupervisor) releaseRescueAdmission() {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.rescueAdmitted--
	if supervisor.rescueAdmitted == 0 {
		close(supervisor.rescueZero)
	}
}

func waitRuntimeAdmissions(ctx context.Context, zero <-chan struct{}) error {
	if ctx == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	select {
	case <-zero:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (supervisor *RuntimeSupervisor) EnterRescue(ctx context.Context) error {
	if supervisor == nil || ctx == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.Lock()
	if supervisor.modeEvidence == nil || supervisor.rescueHandler == nil {
		supervisor.mu.Unlock()
		return ErrRuntimeSupervisorUnavailable
	}
	if supervisor.trafficMode == TrafficModeRescue {
		supervisor.mu.Unlock()
		return nil
	}
	if supervisor.trafficMode == TrafficModeRescueDraining {
		if !supervisor.rescueEntryRun {
			supervisor.startRescueEntryLocked(supervisor.modeGeneration)
		}
		supervisor.mu.Unlock()
		return nil
	}
	if supervisor.trafficMode != TrafficModeNormal || supervisor.worker == nil {
		supervisor.mu.Unlock()
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.modeGeneration++
	generation := supervisor.modeGeneration
	intent := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: generation, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescueDraining, Phase: RuntimeModePhaseIntent}
	if err := supervisor.modeEvidence.Commit(ctx, intent); err != nil {
		supervisor.modeGeneration--
		supervisor.mu.Unlock()
		return err
	}
	supervisor.trafficMode = TrafficModeRescueDraining
	supervisor.admissionReady = false
	supervisor.startRescueEntryLocked(generation)
	supervisor.mu.Unlock()
	return nil
}

func (supervisor *RuntimeSupervisor) startRescueEntryLocked(generation uint64) {
	lifetime := supervisor.lifetimeCtx
	if lifetime == nil {
		lifetime = context.Background()
	}
	supervisor.rescueEntryRun = true
	go func() {
		_ = supervisor.completeRescueEntry(lifetime, generation)
		supervisor.mu.Lock()
		if supervisor.modeGeneration == generation {
			supervisor.rescueEntryRun = false
		}
		supervisor.mu.Unlock()
	}()
}

func (supervisor *RuntimeSupervisor) completeRescueEntry(ctx context.Context, generation uint64) error {
	supervisor.mu.Lock()
	if supervisor.trafficMode != TrafficModeRescueDraining || supervisor.modeGeneration != generation {
		supervisor.mu.Unlock()
		return ErrRuntimeGeneration
	}
	worker := supervisor.worker
	sequence := supervisor.sequence
	zero := supervisor.normalZero
	supervisor.mu.Unlock()
	drainCtx, cancelDrain := context.WithTimeout(ctx, runtimeRescueDrainLimit)
	defer cancelDrain()
	if worker != nil {
		if err := worker.BeginDrain(drainCtx, TrafficModeDrain, sequence); err == nil {
			_, _ = worker.AwaitQuiescence(drainCtx, sequence)
		}
	}
	_ = waitRuntimeAdmissions(drainCtx, zero)
	var release RuntimeWorkerReleaseV1
	if worker != nil {
		release, _ = worker.StopAndReap(drainCtx)
		if !release.valid() {
			return ErrRuntimeSupervisorUnavailable
		}
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.trafficMode != TrafficModeRescueDraining || supervisor.modeGeneration != generation || supervisor.worker != worker {
		return ErrRuntimeGeneration
	}
	if worker != nil {
		supervisor.worker = nil
		supervisor.pendingRelease = release
	}
	receipt := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: generation, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective}
	if err := supervisor.modeEvidence.Commit(ctx, receipt); err != nil {
		return err
	}
	supervisor.trafficMode = TrafficModeRescue
	return nil
}

func (supervisor *RuntimeSupervisor) ExitRescue(ctx context.Context, manifest WorkerManifestV1) error {
	if supervisor == nil || ctx == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.Lock()
	if supervisor.modeEvidence == nil || supervisor.trafficMode != TrafficModeRescue || supervisor.worker != nil {
		supervisor.mu.Unlock()
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.modeGeneration++
	generation := supervisor.modeGeneration
	intent := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: generation, DesiredMode: TrafficModeNormal, EffectiveMode: TrafficModeRescueExitDraining, Phase: RuntimeModePhaseIntent}
	if err := supervisor.modeEvidence.Commit(ctx, intent); err != nil {
		supervisor.modeGeneration--
		supervisor.mu.Unlock()
		return err
	}
	supervisor.trafficMode = TrafficModeRescueExitDraining
	zero := supervisor.rescueZero
	supervisor.mu.Unlock()
	if err := waitRuntimeAdmissions(ctx, zero); err != nil {
		return err
	}
	return supervisor.completeRescueExit(ctx, generation, manifest)
}

func (supervisor *RuntimeSupervisor) completeRescueExit(ctx context.Context, generation uint64, manifest WorkerManifestV1) error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.trafficMode != TrafficModeRescueExitDraining || supervisor.modeGeneration != generation {
		return ErrRuntimeGeneration
	}
	if supervisor.worker == nil {
		if _, err := supervisor.bootLocked(ctx, manifest, "rescue_exit", supervisor.pendingRelease); err != nil {
			return err
		}
	}
	receipt := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: generation, DesiredMode: TrafficModeNormal, EffectiveMode: TrafficModeNormal, Phase: RuntimeModePhaseEffective}
	if err := supervisor.modeEvidence.Commit(ctx, receipt); err != nil {
		return err
	}
	supervisor.trafficMode = TrafficModeNormal
	return nil
}

func (supervisor *RuntimeSupervisor) ReconcileRescue(ctx context.Context, manifest WorkerManifestV1) error {
	if supervisor == nil || ctx == nil {
		return ErrRuntimeSupervisorUnavailable
	}
	supervisor.mu.RLock()
	mode := supervisor.trafficMode
	generation := supervisor.modeGeneration
	zero := supervisor.rescueZero
	supervisor.mu.RUnlock()
	switch mode {
	case TrafficModeNormal, TrafficModeRescue:
		return nil
	case TrafficModeRescueDraining:
		return supervisor.completeRescueEntry(ctx, generation)
	case TrafficModeRescueExitDraining:
		if err := waitRuntimeAdmissions(ctx, zero); err != nil {
			return err
		}
		return supervisor.completeRescueExit(ctx, generation, manifest)
	default:
		return ErrRuntimeSupervisorUnavailable
	}
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
	lifetimeCtx := supervisor.lifetimeCtx
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	worker, err := supervisor.launcher.Launch(lifetimeCtx, manifest)
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
	if supervisor.callerAdmissions != nil {
		authority, authorityErr := NewNormalCallerAuthorityFromIndex(ack.CallerAuthorityKey, ack.CallerIndex, supervisor.callerAdmissions, time.Now, rand.Reader)
		zeroRuntimeBytes(ack.CallerAuthorityKey)
		ack.CallerAuthorityKey = nil
		if authorityErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = worker.StopAndReap(cleanupCtx)
			return RuntimeBootAckV1{}, authorityErr
		}
		supervisor.callerAuthority = authority
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
	supervisor.workerManifest = manifest
	supervisor.checkpointDigest = digest
	supervisor.admissionReady = true
	supervisor.monitorWorkerLocked(worker, manifest)
	return ack, nil
}

func (supervisor *RuntimeSupervisor) monitorWorkerLocked(worker RuntimeWorkerProcess, manifest WorkerManifestV1) {
	exited, ok := worker.(interface{ Exited() <-chan struct{} })
	if !ok || exited.Exited() == nil {
		return
	}
	lifetime := supervisor.lifetimeCtx
	go func() {
		select {
		case <-exited.Exited():
		case <-lifetime.Done():
			return
		}
		supervisor.mu.RLock()
		current := supervisor.worker == worker && supervisor.admissionReady && supervisor.trafficMode == TrafficModeNormal
		supervisor.mu.RUnlock()
		if !current {
			return
		}
		replaceCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = supervisor.ReplaceFailedWorker(replaceCtx, manifest)
	}()
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
