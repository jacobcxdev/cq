package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
)

func TestRuntimeUpgradePreparationExcludesArtifactPruning(t *testing.T) {
	controller, _, _, request := upgradeControllerFixture(t)
	artifacts := installer.RuntimeArtifactStore{FS: controller.options.Store.FS, Roots: controller.options.Store.Roots}
	lock, err := artifacts.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Begin(context.Background(), request); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("preparation bypassed artifact lock: %v", err)
	}
	lock.Close()
	controller.options.Artifacts = upgradePruneCheckingArtifacts{store: artifacts}
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upgradeWaitTerminal(t, controller, request.TransactionID)
}

type upgradePruneCheckingArtifacts struct {
	store installer.RuntimeArtifactStore
}

func (artifacts upgradePruneCheckingArtifacts) Verify(ctx context.Context, _ installer.RuntimeArtifact) error {
	err := artifacts.store.Prune(ctx, func() ([]string, error) {
		return nil, errors.New("pruning entered during verification")
	})
	if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return fmt.Errorf("candidate verification did not exclude pruning: %w", err)
	}
	return nil
}

type upgradeTestWorker struct {
	mu      sync.Mutex
	gate    *RuntimeUpgradeAdmission
	owners  *upgradeTestOwners
	stopped int
}
type upgradeTestOwners struct {
	mu              sync.Mutex
	active, maximum int
}

func (owners *upgradeTestOwners) add(delta int) {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	owners.active += delta
	owners.maximum = max(owners.maximum, owners.active)
}
func (worker *upgradeTestWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.HolderProof()}, nil
}
func (worker *upgradeTestWorker) HolderProof() LifecycleHolderProof {
	return runtimeHolder("upgrade-worker")
}
func (worker *upgradeTestWorker) BeginDrain(context.Context, TrafficMode, uint64) error { return nil }
func (worker *upgradeTestWorker) AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error) {
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}
func (worker *upgradeTestWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	worker.stopped++
	worker.owners.add(-1)
	return RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}, nil
}
func (worker *upgradeTestWorker) ExecuteHTTP(context.Context, RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	return RuntimeHTTPResponseV1{StatusCode: 200}, nil
}
func (worker *upgradeTestWorker) PrepareUpgrade(ctx context.Context) error {
	return worker.gate.Pause(ctx)
}
func (worker *upgradeTestWorker) AwaitUpgradeQuiescence(ctx context.Context) error {
	return worker.gate.AwaitQuiescence(ctx)
}
func (worker *upgradeTestWorker) ResumeUpgrade(context.Context) error {
	worker.gate.Resume()
	return nil
}

type upgradeTestLauncher struct{ owners *upgradeTestOwners }

func (launcher upgradeTestLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	launcher.owners.add(1)
	return &upgradeTestWorker{gate: NewRuntimeUpgradeAdmission(), owners: launcher.owners}, nil
}

type upgradeTestArtifacts struct{}

func (upgradeTestArtifacts) Verify(context.Context, installer.RuntimeArtifact) error { return nil }

type upgradeTestOwnership struct{ record installstate.Record }

func (ownership upgradeTestOwnership) Load() (installstate.Record, error) {
	return ownership.record, nil
}

func upgradeControllerFixture(t *testing.T) (*RuntimeUpgradeController, *RuntimeSupervisor, *upgradeTestWorker, RuntimeUpgradeRequestV1) {
	t.Helper()
	store, receipt := upgradeReceiptFixture(t)
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Close() })
	events := []string{}
	owners := &upgradeTestOwners{active: 1, maximum: 1}
	worker := &upgradeTestWorker{gate: NewRuntimeUpgradeAdmission(), owners: owners}
	supervisor, err := NewRuntimeSupervisor(tcp, runtimeHolder("supervisor"), upgradeTestLauncher{owners}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.worker = worker
	supervisor.workerManifest = WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: receipt.Previous.SHA256}
	supervisor.admissionReady = true
	if err := supervisor.SetUpgradeIngress(NewRuntimeUpgradeListener(tcp), NewRuntimeUpgradeConnections(), func(bool) {}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "owner"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})); err != nil {
		t.Fatal(err)
	}
	candidate := receipt.Candidate
	candidate.SHA256 = strings.Repeat("b", 64)
	candidate.Path = strings.Replace(candidate.Path, strings.Repeat("a", 64), candidate.SHA256, 1)
	candidate.Version = "0.34.1"
	controller, err := NewRuntimeUpgradeController(supervisor, RuntimeUpgradeControllerOptions{
		Store: store, Artifacts: upgradeTestArtifacts{}, Ownership: upgradeTestOwnership{installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerHomebrew, Version: receipt.Previous.Version, Executable: "/package/cq", BinaryDigest: receipt.Previous.SHA256, Services: []string{"proxy", "refresh"}}}, Previous: receipt.Previous,
		Handoff: func(ctx context.Context, next RuntimeUpgradeReceiptV1, release RuntimeWorkerReleaseV1) error {
			owners.add(1)
			if !release.valid() {
				return errors.New("missing owner release")
			}
			next.Phase = "verifying"
			if err := store.Save(next); err != nil {
				return err
			}
			next.Phase = "committed"
			return store.Save(next)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.quietTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		controller.Await(ctx)
	})
	return controller, supervisor, worker, RuntimeUpgradeRequestV1{SchemaVersion: 1, TransactionID: "upgrade-test", Candidate: candidate}
}
func upgradeWaitTerminal(t *testing.T, controller *RuntimeUpgradeController, id string) RuntimeUpgradeReceiptV1 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Await(ctx); err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.Status(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
func TestRuntimeUpgradeRejectsRemoteCallerAndStaleGeneration(t *testing.T) {
	controller, supervisor, _, request := upgradeControllerFixture(t)
	body, _ := json.Marshal(request)
	for _, remote := range []string{"203.0.113.10:1234", "127.0.0.1:1234"} {
		req := httptest.NewRequest(http.MethodPost, RuntimeUpgradePath, bytes.NewReader(body))
		req.RemoteAddr = remote
		if strings.HasPrefix(remote, "203") {
			req.Header.Set("Authorization", "Bearer local-token")
		}
		response := httptest.NewRecorder()
		supervisor.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorised caller accepted: %d %s", response.Code, response.Body.String())
		}
	}
	request.ExpectedGeneration = 99
	if _, err := controller.Begin(context.Background(), request); !errors.Is(err, ErrRuntimeUpgradeGeneration) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, err := controller.options.Store.Load(); err == nil {
		t.Fatal("rejection mutated receipt")
	}
}
func TestRuntimeUpgradeDefersWithoutStoppingOldWorker(t *testing.T) {
	controller, _, worker, request := upgradeControllerFixture(t)
	release, _ := worker.gate.BeginTurn()
	defer release()
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	receipt := upgradeWaitTerminal(t, controller, request.TransactionID)
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if receipt.Phase != "deferred" || worker.stopped != 0 || worker.owners.maximum > 1 {
		t.Fatalf("deferred upgrade changed runtime: %+v stopped=%d", receipt, worker.stopped)
	}
	if next, err := worker.gate.BeginTurn(); err != nil {
		t.Fatal("deferral did not restore admissions")
	} else {
		next()
	}
}

type crashingUpgradeWorker struct {
	*upgradeTestWorker
	exited     chan struct{}
	waiting    chan struct{}
	pausedCase bool
	awaits     int
}

func (worker *crashingUpgradeWorker) Exited() <-chan struct{} { return worker.exited }
func (worker *crashingUpgradeWorker) PrepareUpgrade(ctx context.Context) error {
	if err := worker.upgradeTestWorker.PrepareUpgrade(ctx); err != nil {
		return err
	}
	close(worker.waiting)
	return nil
}
func (worker *crashingUpgradeWorker) AwaitUpgradeQuiescence(ctx context.Context) error {
	worker.awaits++
	if worker.pausedCase && worker.awaits == 1 {
		return nil
	}
	if !worker.pausedCase {
		close(worker.waiting)
	}
	select {
	case <-worker.exited:
		return ErrRuntimeSupervisorUnavailable
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (worker *crashingUpgradeWorker) ResumeUpgrade(context.Context) error {
	return ErrRuntimeSupervisorUnavailable
}

func TestRuntimeUpgradeRecoversWorkerDeathDuringWaitingAndPausedDrain(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint("paused=", paused), func(t *testing.T) {
			controller, supervisor, worker, request := upgradeControllerFixture(t)
			controller.quietTimeout = time.Second
			crashed := &crashingUpgradeWorker{upgradeTestWorker: worker, exited: make(chan struct{}), waiting: make(chan struct{}), pausedCase: paused}
			supervisor.mu.Lock()
			supervisor.worker = crashed
			supervisor.monitorWorkerLocked(crashed, supervisor.workerManifest)
			supervisor.mu.Unlock()
			if _, err := controller.Begin(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			<-crashed.waiting
			close(crashed.exited)
			upgradeWaitTerminal(t, controller, request.TransactionID)
			deadline := time.Now().Add(time.Second)
			for {
				supervisor.mu.RLock()
				ready := supervisor.admissionReady && supervisor.worker != nil && supervisor.worker != crashed
				supervisor.mu.RUnlock()
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker crash recovery was lost during upgrade")
				}
				time.Sleep(time.Millisecond)
			}
			supervisor.upgradeListener.mu.Lock()
			stillPaused := supervisor.upgradeListener.paused
			supervisor.upgradeListener.mu.Unlock()
			if stillPaused {
				t.Fatal("recovered worker retained paused ingress")
			}
			if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
				t.Fatal(err)
			}
			fresh := httptest.NewRequest(http.MethodPost, "/normal", strings.NewReader("fresh"))
			fresh.Header.Set("Authorization", "Bearer local-token")
			response := httptest.NewRecorder()
			supervisor.ServeHTTP(response, fresh)
			if response.Code != http.StatusOK {
				t.Fatalf("recovered worker did not serve fresh traffic: %d", response.Code)
			}
			worker.owners.mu.Lock()
			defer worker.owners.mu.Unlock()
			if worker.owners.active != 1 || worker.owners.maximum != 1 {
				t.Fatalf("worker recovery overlapped owners: %+v", worker.owners)
			}
		})
	}
}

func TestRuntimeUpgradeRollbackBeforeWorkerLaunch(t *testing.T) {
	events := []string{}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events}, &RuntimeHashCheckpointStore{})
	if err != nil {
		t.Fatal(err)
	}
	resume := RuntimeUpgradeResumeV1{Release: RuntimeWorkerReleaseV1{ProcessIdentityDigest: "previous", ProcessTreeAbsenceProofDigest: "absent", HolderReleaseProofDigest: "released"}, WorkerSequence: 7, PreviousCheckpointDigest: strings.Repeat("a", 64)}
	if err := supervisor.ResumeRuntimeUpgradeOwnership(resume); err != nil {
		t.Fatal(err)
	}
	release, sequence, checkpoint, err := supervisor.RuntimeUpgradeRollbackSnapshot()
	if err != nil || release != resume.Release || sequence != 8 || checkpoint != resume.PreviousCheckpointDigest {
		t.Fatalf("pre-boot setup failure discarded proven release: %+v %d %q %v", release, sequence, checkpoint, err)
	}
}
func TestRuntimeUpgradeNeverOverlapsCoordinatorOwners(t *testing.T) {
	controller, _, worker, request := upgradeControllerFixture(t)
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if receipt := upgradeWaitTerminal(t, controller, request.TransactionID); receipt.Phase != "committed" {
		t.Fatalf("not committed: %+v", receipt)
	}
	worker.owners.mu.Lock()
	defer worker.owners.mu.Unlock()
	if worker.owners.maximum != 1 || worker.owners.active != 1 {
		t.Fatalf("owner overlap: %+v", worker.owners)
	}
}
func TestRuntimeUpgradeBootFailureRestoresPrevious(t *testing.T) {
	controller, supervisor, _, request := upgradeControllerFixture(t)
	controller.options.Handoff = func(context.Context, RuntimeUpgradeReceiptV1, RuntimeWorkerReleaseV1) error {
		return errors.New("candidate boot failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := controller.Begin(ctx, request); err != nil {
		t.Fatal(err)
	}
	cancel()
	receipt := upgradeWaitTerminal(t, controller, request.TransactionID)
	if receipt.Phase != "rolled_back" || receipt.Previous != controller.options.Previous {
		t.Fatalf("rollback identity: %+v", receipt)
	}
	supervisor.mu.RLock()
	ready := supervisor.admissionReady
	worker := supervisor.worker.(*upgradeTestWorker)
	supervisor.mu.RUnlock()
	if !ready {
		t.Fatal("previous worker was not restored")
	}
	worker.owners.mu.Lock()
	defer worker.owners.mu.Unlock()
	if worker.owners.maximum != 1 || worker.owners.active != 1 {
		t.Fatalf("rollback overlap: %+v", worker.owners)
	}
}
func TestRuntimeUpgradeRetryIsIdempotent(t *testing.T) {
	controller, _, worker, request := upgradeControllerFixture(t)
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	terminal := upgradeWaitTerminal(t, controller, request.TransactionID)
	retry, err := controller.Begin(context.Background(), request)
	if err != nil || retry != terminal {
		t.Fatalf("retry changed terminal receipt: %+v %v", retry, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.stopped != 1 {
		t.Fatalf("retry restarted transaction: %d", worker.stopped)
	}
	request.Candidate.Version = "different"
	if _, err := controller.Begin(context.Background(), request); err == nil {
		t.Fatal("same id accepted different candidate")
	}
}

func TestRuntimeUpgradeBlocksCompetingWorkerReplacement(t *testing.T) {
	controller, supervisor, worker, request := upgradeControllerFixture(t)
	release, _ := worker.gate.BeginTurn()
	defer release()
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.ReplaceWorker(context.Background(), supervisor.workerManifest); err == nil {
		t.Fatal("worker replacement overlapped pending upgrade")
	}
	if _, err := supervisor.ReplaceFailedWorker(context.Background(), supervisor.workerManifest); err == nil {
		t.Fatal("crash replacement overlapped pending upgrade")
	}
}

func TestRuntimeUpgradeControlRejectsMalformedBodies(t *testing.T) {
	_, supervisor, _, request := upgradeControllerFixture(t)
	body, _ := json.Marshal(request)
	for _, invalid := range []string{string(body) + " {}", strings.TrimSuffix(string(body), "}") + `,"unknown":true}`, strings.Repeat(" ", 64<<10) + string(body)} {
		req := httptest.NewRequest(http.MethodPost, RuntimeUpgradePath, strings.NewReader(invalid))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer local-token")
		response := httptest.NewRecorder()
		supervisor.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed body accepted: %d", response.Code)
		}
	}
}

func TestRuntimeUpgradeExecFailurePreservesGuardReceipt(t *testing.T) {
	controller, _, _, request := upgradeControllerFixture(t)
	controller.options.Handoff = func(_ context.Context, receipt RuntimeUpgradeReceiptV1, _ RuntimeWorkerReleaseV1) error {
		receipt.GuardPID = 12345
		if err := controller.options.Store.Save(receipt); err != nil {
			return err
		}
		return errors.New("exec failed after guard prepared")
	}
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	receipt := upgradeWaitTerminal(t, controller, request.TransactionID)
	if receipt.Phase != "rolled_back" || receipt.GuardPID != 12345 {
		t.Fatalf("exec rollback discarded durable guard binding: %+v", receipt)
	}
}

type upgradeUnreleasedBootWorker struct{ *runtimeTestWorker }

func (*upgradeUnreleasedBootWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	return RuntimeWorkerReleaseV1{}, errors.New("release not proven")
}

type upgradeUnreleasedBootLauncher struct{ worker RuntimeWorkerProcess }

func (launcher upgradeUnreleasedBootLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return launcher.worker, nil
}
func TestRuntimeUpgradeFailedBootRetainsUnreleasedOwner(t *testing.T) {
	events := []string{}
	worker := &upgradeUnreleasedBootWorker{&runtimeTestWorker{holder: runtimeHolder("unreleased"), events: &events}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), upgradeUnreleasedBootLauncher{worker}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	// Force checkpoint selection to fail after the worker has booted.
	supervisor.checkpoints = upgradeFailingCheckpoint{}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); !errors.Is(err, ErrRuntimeOwnerReleaseUnproven) {
		t.Fatalf("failed boot lost owner release failure: %v", err)
	}
	if supervisor.worker != worker || supervisor.admissionReady {
		t.Fatal("unreleased owner was discarded")
	}
}

type upgradeFailingCheckpoint struct{}

func (upgradeFailingCheckpoint) Select(context.Context, RuntimeHolderCheckpointV1) (string, error) {
	return "", errors.New("checkpoint failed")
}

func TestRuntimeUpgradeRollbackBeforePackageOwnershipCommit(t *testing.T) {
	controller, supervisor, _, request := upgradeControllerFixture(t)
	previous := controller.options.Previous
	committed := RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "package-before-refresh", Generation: 1, Previous: previous, Candidate: request.Candidate, ListenerIdentity: supervisor.listenerIdentity, SupervisorPID: os.Getpid()}
	for _, phase := range []string{"prepared", "waiting", "handoff", "verifying", "committed"} {
		committed.Phase = phase
		if err := controller.options.Store.Save(committed); err != nil {
			t.Fatal(err)
		}
	}
	controller.options.Previous = request.Candidate
	supervisor.workerManifest.WorkerArtifactDigest = request.Candidate.SHA256
	request.Candidate = previous
	request.TransactionID = "package-rollback"
	request.ExpectedGeneration = 1
	if _, err := controller.Begin(context.Background(), request); err != nil {
		t.Fatalf("rollback before package ownership commit rejected: %v", err)
	}
	receipt := upgradeWaitTerminal(t, controller, request.TransactionID)
	if receipt.Phase != "committed" || receipt.Candidate != previous {
		t.Fatalf("package rollback: %+v", receipt)
	}
}
