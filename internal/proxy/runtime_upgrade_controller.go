package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
)

const RuntimeUpgradePath = "/_cq/runtime/upgrade"
const RuntimeUpgradeStatusPath = "/_cq/runtime/upgrade/status"

var ErrRuntimeUpgradeGeneration = errors.New("runtime upgrade generation conflict")
var ErrRuntimeUpgradeBusy = errors.New("runtime upgrade already in progress")

type RuntimeUpgradeRequestV1 struct {
	SchemaVersion      int                       `json:"schema_version"`
	TransactionID      string                    `json:"transaction_id"`
	ExpectedGeneration uint64                    `json:"expected_generation"`
	Candidate          installer.RuntimeArtifact `json:"candidate"`
	WaitTimeoutSeconds int                       `json:"wait_timeout_seconds"`
}

type RuntimeUpgradeControllerOptions struct {
	Store     RuntimeUpgradeStore
	Artifacts interface {
		Verify(context.Context, installer.RuntimeArtifact) error
	}
	Ownership interface {
		Load() (installstate.Record, error)
	}
	Previous installer.RuntimeArtifact
	// A successful native handoff never returns. Test handoffs must persist a
	// verified terminal receipt before returning success.
	Handoff func(context.Context, RuntimeUpgradeReceiptV1, RuntimeWorkerReleaseV1) error
}

type RuntimeUpgradeController struct {
	mu           sync.Mutex
	supervisor   *RuntimeSupervisor
	options      RuntimeUpgradeControllerOptions
	quietTimeout time.Duration
	done         chan struct{}
	lastErr      error
}

func NewRuntimeUpgradeController(supervisor *RuntimeSupervisor, options RuntimeUpgradeControllerOptions) (*RuntimeUpgradeController, error) {
	if supervisor == nil || options.Artifacts == nil || options.Ownership == nil || options.Handoff == nil || options.Previous.Validate() != nil {
		return nil, ErrRuntimeUpgradeUnsupported
	}
	if err := options.Artifacts.Verify(context.Background(), options.Previous); err != nil {
		return nil, err
	}
	controller := &RuntimeUpgradeController{supervisor: supervisor, options: options, quietTimeout: 30 * time.Second, done: closedRuntimeWaitChannel()}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.upgradeController != nil {
		return nil, ErrRuntimeUpgradeBusy
	}
	supervisor.upgradeController = controller
	return controller, nil
}

// Begin durably prepares a transaction and returns before waiting. Waiting in
// the HTTP handler would count its own accepted connection as active ingress.
func (controller *RuntimeUpgradeController) Begin(ctx context.Context, request RuntimeUpgradeRequestV1) (RuntimeUpgradeReceiptV1, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RuntimeUpgradeReceiptV1{}, err
	}
	if request.SchemaVersion != 1 || !runtimeUpgradeTransactionPattern.MatchString(request.TransactionID) || request.WaitTimeoutSeconds < 0 || request.WaitTimeoutSeconds > 30 || request.Candidate.Validate() != nil {
		return RuntimeUpgradeReceiptV1{}, ErrRuntimeUpgradeReceipt
	}
	previous, err := controller.options.Store.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return previous, err
	}
	generation := uint64(0)
	if err == nil {
		generation = previous.Generation
		if previous.TransactionID == request.TransactionID {
			if previous.Candidate != request.Candidate || request.ExpectedGeneration != generation-1 {
				return previous, ErrRuntimeUpgradeGeneration
			}
			return previous, nil
		}
		if !previous.terminal() {
			return previous, ErrRuntimeUpgradeBusy
		}
	}
	if request.ExpectedGeneration != generation {
		return previous, ErrRuntimeUpgradeGeneration
	}
	if err := controller.options.Artifacts.Verify(ctx, request.Candidate); err != nil {
		return previous, err
	}
	if err := controller.options.Artifacts.Verify(ctx, controller.options.Previous); err != nil {
		return previous, err
	}
	ownership, err := controller.options.Ownership.Load()
	if err != nil {
		return previous, err
	}
    // Package ownership commits after refresh reconciliation. A failed refresh
    // may need to return to the recorded predecessor while the selected runtime
    // already matches the verified committed candidate.
    matchesSelected:=ownership.BinaryDigest==controller.options.Previous.SHA256
    pendingPackageCommit:=err==nil&&previous.Phase=="committed"&&previous.Candidate==controller.options.Previous&&ownership.BinaryDigest==previous.Previous.SHA256
    if ownership.Validate()!=nil||ownership.Owner!=installstate.OwnerHomebrew||(!matchesSelected&&!pendingPackageCommit){return previous,installstate.ErrOwnershipConflict}
	supervisor := controller.supervisor
	supervisor.mu.Lock()
	if supervisor.upgradeBusy || !supervisor.admissionReady || supervisor.trafficMode != TrafficModeNormal || supervisor.upgradeListener == nil || supervisor.upgradeConnections == nil || supervisor.workerManifest.WorkerArtifactDigest != controller.options.Previous.SHA256 {
		supervisor.mu.Unlock()
		return previous, ErrRuntimeUpgradeBusy
	}
	worker, compatible := supervisor.worker.(RuntimeUpgradeWorker)
	if !compatible {
		supervisor.mu.Unlock()
		return previous, ErrRuntimeUpgradeUnsupported
	}
	receipt := RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: request.TransactionID, Generation: generation + 1, Phase: "prepared", Previous: controller.options.Previous, Candidate: request.Candidate, ListenerIdentity: supervisor.listenerIdentity, SupervisorPID: os.Getpid()}
	if err := controller.options.Store.Save(receipt); err != nil {
		supervisor.mu.Unlock()
		return previous, err
	}
	supervisor.upgradeBusy = true
	selected := supervisor.worker
	supervisor.mu.Unlock()
	controller.done = make(chan struct{})
	controller.lastErr = nil
	wait := controller.quietTimeout
	if request.WaitTimeoutSeconds > 0 {
		wait = time.Duration(request.WaitTimeoutSeconds) * time.Second
	}
	go controller.run(context.WithoutCancel(ctx), receipt, selected, worker, wait)
	return receipt, nil
}

func (controller *RuntimeUpgradeController) Status(ctx context.Context, transactionID string) (RuntimeUpgradeReceiptV1, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeUpgradeReceiptV1{}, err
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.lastErr != nil {
		return RuntimeUpgradeReceiptV1{}, controller.lastErr
	}
	receipt, err := controller.options.Store.Load()
	if errors.Is(err, os.ErrNotExist) && transactionID == "" {
		return RuntimeUpgradeReceiptV1{SchemaVersion: 1, Phase: "idle", Previous: controller.options.Previous, SupervisorPID: os.Getpid(), ListenerIdentity: controller.supervisor.listenerIdentity}, nil
	}
	if err == nil && transactionID != "" && receipt.TransactionID != transactionID {
		return receipt, ErrRuntimeUpgradeGeneration
	}
	return receipt, err
}
func (controller *RuntimeUpgradeController) Await(ctx context.Context) error {
	controller.mu.Lock()
	done := controller.done
	controller.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.lastErr
}

func (controller *RuntimeUpgradeController) run(ctx context.Context, receipt RuntimeUpgradeReceiptV1, selected RuntimeWorkerProcess, worker RuntimeUpgradeWorker, wait time.Duration) {
	var released RuntimeWorkerReleaseV1
	var pausedAt time.Time
	defer func() {
		if recover() != nil {
			if released.valid() {
				controller.rollback(ctx, &receipt, released, pausedAt)
			} else {
				controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "upgrade_panic")
			}
		}
		controller.mu.Lock()
		controller.supervisor.mu.Lock()
		controller.supervisor.upgradeBusy = false
		controller.supervisor.mu.Unlock()
		close(controller.done)
		controller.mu.Unlock()
	}()
	quiet, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	receipt.Phase = "waiting"
	if err := controller.save(receipt); err != nil {
		return
	}
	supervisor := controller.supervisor
	if err := supervisor.upgradeRequests.AwaitQuiescence(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "quiet_deadline")
		return
	}
	if err := worker.AwaitUpgradeQuiescence(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "worker_not_quiescent")
		return
	}
	if err := supervisor.upgradeConnections.Admission.AwaitQuiescence(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "ingress_not_quiescent")
		return
	}
	pausedAt = time.Now()
	if err := supervisor.upgradeListener.Pause(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "quiet_deadline")
		return
	}
	supervisor.upgradeKeepAlive(false)
	if err := supervisor.upgradeRequests.Pause(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "quiet_deadline")
		return
	}
	if err := worker.PrepareUpgrade(quiet); err != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "upgrade_unsupported")
		return
	}
	for _, await := range []func(context.Context) error{supervisor.upgradeConnections.Admission.AwaitQuiescence, supervisor.upgradeRequests.AwaitQuiescence, worker.AwaitUpgradeQuiescence} {
		if err := await(quiet); err != nil {
			controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "quiet_deadline")
			return
		}
	}
	supervisor.mu.RLock()
	normalZero := supervisor.normalZero
	same := supervisor.worker == selected
	supervisor.mu.RUnlock()
	select {
	case <-normalZero:
	case <-quiet.Done():
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "quiet_deadline")
		return
	}
	if !same || quiet.Err() != nil {
		controller.deferUpgrade(ctx, &receipt, worker, pausedAt, "runtime_changed")
		return
	}
	receipt.Phase = "handoff"
	if err := controller.save(receipt); err != nil {
		controller.resume(ctx, worker)
		return
	}
	reap, cancelReap := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	proof, err := selected.StopAndReap(reap)
	cancelReap()
	if err != nil || !proof.valid() {
		// Without proven release no successor may claim coordinator ownership.
		receipt.Phase = "rolling_back"
		receipt.ErrorCode = "release_unproven"
		controller.save(receipt)
		receipt.Phase = "failed"
		controller.save(receipt)
		return
	}
	released = proof
	receipt.AdmissionPauseNanos = time.Since(pausedAt).Nanoseconds()
	supervisor.mu.Lock()
	supervisor.worker = nil
	supervisor.admissionReady = false
	supervisor.mu.Unlock()
	boot, cancelBoot := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	err = controller.options.Handoff(boot, receipt, proof)
	cancelBoot()
	if err == nil {
		terminal, loadErr := controller.options.Store.Load()
		if loadErr == nil && terminal.TransactionID == receipt.TransactionID && terminal.Phase == "committed" {
			return
		}
	}
	controller.rollback(ctx, &receipt, proof, pausedAt)
}
func (controller *RuntimeUpgradeController) save(receipt RuntimeUpgradeReceiptV1) error {
	err := controller.options.Store.Save(receipt)
	if err != nil {
		controller.mu.Lock()
		controller.lastErr = err
		controller.mu.Unlock()
	}
	return err
}
func (controller *RuntimeUpgradeController) resume(ctx context.Context, worker RuntimeUpgradeWorker) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := worker.ResumeUpgrade(recovery)
	if err == nil {
		controller.supervisor.upgradeRequests.Resume()
		controller.supervisor.upgradeKeepAlive(true)
		controller.supervisor.upgradeListener.Resume()
	}
	return err
}
func (controller *RuntimeUpgradeController) deferUpgrade(ctx context.Context, receipt *RuntimeUpgradeReceiptV1, worker RuntimeUpgradeWorker, pausedAt time.Time, code string) {
	receipt.Phase = "deferred"
	receipt.ErrorCode = code
	if err := controller.resume(ctx, worker); err != nil {
		receipt.Phase = "failed"
		receipt.ErrorCode = "resume_failed"
	}
	if !pausedAt.IsZero() {
		receipt.AdmissionPauseNanos = time.Since(pausedAt).Nanoseconds()
	}
	controller.save(*receipt)
}
func (controller *RuntimeUpgradeController) rollback(ctx context.Context, receipt *RuntimeUpgradeReceiptV1, proof RuntimeWorkerReleaseV1, pausedAt time.Time) {
	if loaded, err := controller.options.Store.Load(); err == nil && loaded.TransactionID == receipt.TransactionID && loaded.Generation == receipt.Generation {
		*receipt = loaded
	}
	receipt.Phase = "rolling_back"
	receipt.ErrorCode = "candidate_boot_failed"
	if err := controller.save(*receipt); err != nil {
		return
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := controller.options.Artifacts.Verify(recovery, receipt.Previous); err != nil {
		receipt.Phase = "failed"
		receipt.ErrorCode = "previous_artifact_invalid"
		controller.save(*receipt)
		return
	}
	supervisor := controller.supervisor
	supervisor.mu.Lock()
	manifest := supervisor.workerManifest
	manifest.WorkerArtifactDigest = receipt.Previous.SHA256
	supervisor.sequence++
	_, err := supervisor.bootLocked(recovery, manifest, "worker_switch", proof)
	supervisor.mu.Unlock()
	if err != nil {
		receipt.Phase = "failed"
		receipt.ErrorCode = "recovery_boot_failed"
		controller.save(*receipt)
		return
	}
	supervisor.upgradeRequests.Resume()
	supervisor.upgradeKeepAlive(true)
	supervisor.upgradeListener.Resume()
	receipt.Phase = "rolled_back"
	if !pausedAt.IsZero() {
		receipt.AdmissionPauseNanos = time.Since(pausedAt).Nanoseconds()
	}
	controller.save(*receipt)
}

func (supervisor *RuntimeSupervisor) serveUpgradeControl(writer http.ResponseWriter, request *http.Request) {
	want := http.MethodPost
	if request.URL.EscapedPath() == RuntimeUpgradeStatusPath {
		want = http.MethodGet
	}
	if request.Method != want {
		writer.Header().Set("Allow", want)
		http.Error(writer, http.StatusText(405), 405)
		return
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		http.Error(writer, http.StatusText(403), 403)
		return
	}
	supervisor.mu.RLock()
	authority := supervisor.callerAuthority
	controller := supervisor.upgradeController
	supervisor.mu.RUnlock()
	authentication, err := authority.authenticate(request, normalCallerRouteLocal)
	if err != nil {
		status := 401
		if errors.Is(err, ErrNormalCallerAuthScope) {
			status = 403
		}
		if errors.Is(err, ErrNormalCallerAuthUnavailable) {
			status = 503
		}
		http.Error(writer, http.StatusText(status), status)
		return
	}
	if controller == nil {
		http.Error(writer, "runtime upgrade unsupported", http.StatusConflict)
		return
	}
	var upgrade RuntimeUpgradeRequestV1
	if want == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&upgrade) != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(writer, http.StatusText(400), 400)
			return
		}
	} else if request.Body != nil {
		data, readErr := io.ReadAll(io.LimitReader(request.Body, 1))
		if readErr != nil || len(data) != 0 {
			http.Error(writer, http.StatusText(400), 400)
			return
		}
	}
	if _, err := authority.consume(request.Context(), authentication, request); err != nil {
		http.Error(writer, http.StatusText(503), 503)
		return
	}
	var receipt RuntimeUpgradeReceiptV1
	if want == http.MethodPost {
		receipt, err = controller.Begin(request.Context(), upgrade)
	} else {
		receipt, err = controller.Status(request.Context(), request.URL.Query().Get("transaction_id"))
	}
	if err != nil {
		http.Error(writer, "runtime upgrade unavailable", http.StatusConflict)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if want == http.MethodPost && !receipt.terminal() {
		writer.WriteHeader(http.StatusAccepted)
	}
	_ = json.NewEncoder(writer).Encode(receipt)
}
