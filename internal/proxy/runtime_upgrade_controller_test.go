package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
)

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
