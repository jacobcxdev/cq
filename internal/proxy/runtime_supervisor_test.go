package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type runtimeTestListener struct {
	mu      sync.Mutex
	closed  bool
	accepts int
}

func (l *runtimeTestListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.accepts++
	l.mu.Unlock()
	return nil, errors.New("test listener stopped")
}
func (l *runtimeTestListener) Addr() net.Addr { return runtimeTestAddr("127.0.0.1:19280") }
func (l *runtimeTestListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

type runtimeTestAddr string

func (a runtimeTestAddr) Network() string { return "tcp" }
func (a runtimeTestAddr) String() string  { return string(a) }

type runtimeTestWorker struct {
	holder   LifecycleHolderProof
	events   *[]string
	response RuntimeHTTPResponseV1
	bootAck  RuntimeBootAckV1
}

func (w *runtimeTestWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	*w.events = append(*w.events, "boot:"+w.holder.DescriptionID)
	ack := w.bootAck
	ack.SchemaVersion = 1
	ack.Kind = "runtime_boot_ack_v1"
	ack.Holder = w.holder
	return ack, nil
}
func (w *runtimeTestWorker) BeginDrain(context.Context, TrafficMode, uint64) error {
	*w.events = append(*w.events, "drain:"+w.holder.DescriptionID)
	return nil
}
func (w *runtimeTestWorker) AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error) {
	*w.events = append(*w.events, "quiesce:"+w.holder.DescriptionID)
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}
func (w *runtimeTestWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	*w.events = append(*w.events, "reap:"+w.holder.DescriptionID)
	return RuntimeWorkerReleaseV1{
		ProcessIdentityDigest:         "process-" + w.holder.DescriptionID,
		ProcessTreeAbsenceProofDigest: "absence-" + w.holder.DescriptionID,
		HolderReleaseProofDigest:      "release-" + w.holder.DescriptionID,
	}, nil
}
func (w *runtimeTestWorker) HolderProof() LifecycleHolderProof { return w.holder }
func (w *runtimeTestWorker) ExecuteHTTP(context.Context, RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	*w.events = append(*w.events, "execute:"+w.holder.DescriptionID)
	if w.response.StatusCode == 0 {
		w.response.StatusCode = http.StatusNoContent
	}
	return w.response, nil
}

type runtimeTestLauncher struct {
	events  *[]string
	workers []*runtimeTestWorker
}

func TestRuntimeSupervisorForwardsNormalHTTPOnlyToSelectedWorker(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events, response: RuntimeHTTPResponseV1{StatusCode: http.StatusAccepted, Header: http.Header{"X-Worker": {"selected"}}, Body: []byte("from worker")}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/normal?x=1", bytes.NewBufferString("body"))
	request.Header.Set("Authorization", "Bearer local-token")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("X-Worker") != "selected" || response.Body.String() != "from worker" {
		t.Fatalf("response = %#v", response)
	}
	if events[len(events)-1] != "execute:worker" {
		t.Fatalf("events = %#v", events)
	}
}

type runtimeFailingLauncher struct{ calls int }

func (launcher *runtimeFailingLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	launcher.calls++
	return nil, errors.New("successor failed")
}

func TestRuntimeSupervisorSealsPendingReleaseAfterSuccessorFailure(t *testing.T) {
	events := []string{}
	first := &runtimeTestWorker{holder: runtimeHolder("worker-1"), events: &events}
	launcher := &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{first}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), launcher, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	failing := &runtimeFailingLauncher{}
	supervisor.launcher = failing
	if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); err == nil {
		t.Fatal("successor failure succeeded")
	}
	if _, err := supervisor.Boot(context.Background(), manifest); !errors.Is(err, ErrRuntimeRecoveryPending) {
		t.Fatalf("Boot after successor failure = %v", err)
	}
	if _, err := supervisor.Boot(context.Background(), manifest); !errors.Is(err, ErrRuntimeRecoveryPending) {
		t.Fatalf("second Boot after successor failure = %v", err)
	}
	if failing.calls != 1 {
		t.Fatalf("launch attempts = %d, want 1", failing.calls)
	}
	if !supervisor.PendingRelease().valid() {
		t.Fatal("prior release proof was dropped")
	}
}

func (l *runtimeTestLauncher) Launch(_ context.Context, _ WorkerManifestV1) (RuntimeWorkerProcess, error) {
	w := l.workers[0]
	l.workers = l.workers[1:]
	return w, nil
}

type runtimeTestCheckpointStore struct {
	events      *[]string
	checkpoints []RuntimeHolderCheckpointV1
}

func (s *runtimeTestCheckpointStore) Select(_ context.Context, checkpoint RuntimeHolderCheckpointV1) (string, error) {
	worker := checkpoint.LifecycleLockHolders[1].Holder
	*s.events = append(*s.events, "checkpoint:"+worker.DescriptionID)
	s.checkpoints = append(s.checkpoints, checkpoint)
	return "digest-" + worker.DescriptionID, nil
}

func TestRuntimeSupervisorCheckpointPrecedesAdmissionAndReplacementKeepsListener(t *testing.T) {
	events := []string{}
	listener := &runtimeTestListener{}
	launcher := &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{
		{holder: runtimeHolder("worker-1"), events: &events},
		{holder: runtimeHolder("worker-2"), events: &events},
	}}
	checkpoints := &runtimeTestCheckpointStore{events: &events}
	supervisor, err := NewRuntimeSupervisor(listener, runtimeHolder("supervisor"), launcher, checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	if supervisor.AdmissionReady() {
		t.Fatal("admission ready before checkpoint")
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact-1"}); err != nil {
		t.Fatal(err)
	}
	if !supervisor.AdmissionReady() {
		t.Fatal("admission not ready after checkpoint")
	}
	if got := supervisor.ListenerIdentity(); got != "tcp|127.0.0.1:19280" {
		t.Fatalf("listener identity = %q", got)
	}
	if _, err := supervisor.ReplaceWorker(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact-2"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"boot:worker-1", "checkpoint:worker-1", "drain:worker-1", "quiesce:worker-1", "reap:worker-1", "boot:worker-2", "checkpoint:worker-2"}
	if len(events) != len(want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %#v, want %#v", events, want)
		}
	}
	if len(checkpoints.checkpoints) != 2 || checkpoints.checkpoints[1].Sequence != 1 || checkpoints.checkpoints[1].PreviousCheckpointDigest != "digest-worker-1" {
		t.Fatalf("checkpoints = %#v", checkpoints.checkpoints)
	}
	second := checkpoints.checkpoints[1]
	if second.PriorWorkerProcessTreeAbsenceProofDigest != "absence-worker-1" || second.PriorWorkerHolderReleaseProofDigest != "release-worker-1" {
		t.Fatalf("replacement checkpoint did not bind prior release: %#v", second)
	}
	listener.mu.Lock()
	closed := listener.closed
	listener.mu.Unlock()
	if closed {
		t.Fatal("replacement closed inherited listener")
	}
}

func TestRuntimeSupervisorRoleBootstrapsWorkerAndCheckpointBeforeServe(t *testing.T) {
	lifecyclePath := t.TempDir() + "/lifecycle.lock"
	if err := os.WriteFile(lifecyclePath, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := os.Open(lifecyclePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	holderDigest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	supervisorHolder, err := RuntimeLifecycleHolder(lifecycle, "supervisor-description")
	if err != nil {
		t.Fatal(err)
	}
	workerHolder := supervisorHolder
	workerHolder.DescriptionID = "worker-description"
	listenerFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	controlFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	secretReader, secretWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secretWriter.Write(bytes.Repeat([]byte{0x28}, RuntimeSecretSize)); err != nil {
		t.Fatal(err)
	}
	_ = secretWriter.Close()
	manifestDigest := sha256.Sum256([]byte("runtime-manifest"))
	manifest := RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: "proxy-a", RuntimeInstanceID: "runtime-a",
		ListenerFD: RuntimeListenerFD, LifecycleFD: RuntimeLifecycleFD,
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	events := []string{}
	launcher := &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{{holder: workerHolder, events: &events}}}
	checkpoint := &runtimeTestCheckpointStore{events: &events}
	err = RunRuntimeSupervisorRole(context.Background(), manifest, RuntimeSupervisorRoleDependencies{
		Files:            RuntimeRoleFiles{Listener: listenerFile, Lifecycle: lifecycle, Control: controlFile, Secret: secretReader},
		SupervisorHolder: supervisorHolder, Launcher: launcher, Checkpoints: checkpoint,
		WorkerManifest: WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"},
		AdoptListener:  func(*os.File) (net.Listener, error) { return &runtimeTestListener{}, nil },
		Serve: func(_ context.Context, _ net.Listener, handler http.Handler) error {
			if handler == nil {
				t.Fatal("missing selected worker handler")
			}
			events = append(events, "serve")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"boot:worker-description", "checkpoint:worker-description", "serve"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestRuntimeSupervisorCrashReapsBeforeReplacement(t *testing.T) {
	events := []string{}
	listener := &runtimeTestListener{}
	launcher := &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{
		{holder: runtimeHolder("worker-1"), events: &events},
		{holder: runtimeHolder("worker-2"), events: &events},
	}}
	supervisor, err := NewRuntimeSupervisor(listener, runtimeHolder("supervisor"), launcher, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact-1"}); err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	if _, err := supervisor.ReplaceFailedWorker(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact-2"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"reap:worker-1", "boot:worker-2", "checkpoint:worker-2"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if supervisor.ListenerIdentity() != "tcp|127.0.0.1:19280" || !supervisor.AdmissionReady() {
		t.Fatal("replacement lost listener continuity or admission checkpoint")
	}
}

func TestRuntimeSupervisorSuccessfulReplacementResetsCrashBudgetAndBootCannotBypassStop(t *testing.T) {
	events := []string{}
	workers := make([]*runtimeTestWorker, 8)
	for index := range workers {
		workers[index] = &runtimeTestWorker{holder: runtimeHolder(fmt.Sprintf("worker-%d", index)), events: &events}
	}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: workers}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	for range runtimeCrashLimit - 1 {
		if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := supervisor.ReplaceWorker(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	for range runtimeCrashLimit {
		if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); err != nil {
			t.Fatalf("budget not reset: %v", err)
		}
	}
	if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); !errors.Is(err, ErrRuntimeCrashLoop) {
		t.Fatalf("crash loop = %v", err)
	}
	if _, err := supervisor.Boot(context.Background(), manifest); !errors.Is(err, ErrRuntimeCrashLoop) {
		t.Fatalf("Boot bypass = %v", err)
	}
}

func TestRuntimeSupervisorControlGenerationFencesReplacement(t *testing.T) {
	// The selected generation is an exact fence; a stale caller cannot drain or
	// accept quiescence from a predecessor after replacement.
	events := []string{}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{{holder: runtimeHolder("worker-1"), events: &events}, {holder: runtimeHolder("worker-2"), events: &events}}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.ReplaceWorker(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.BeginDrain(context.Background(), TrafficModeDrain, 0); !errors.Is(err, ErrRuntimeGeneration) {
		t.Fatalf("stale drain = %v", err)
	}
	if _, err := supervisor.AwaitQuiescence(context.Background(), 0); !errors.Is(err, ErrRuntimeGeneration) {
		t.Fatalf("stale quiescence = %v", err)
	}
}

func TestRuntimeSupervisorCrashLoopStopsAfterThreeStarts(t *testing.T) {
	events := []string{}
	listener := &runtimeTestListener{}
	workers := make([]*runtimeTestWorker, 5)
	for index := range workers {
		workers[index] = &runtimeTestWorker{holder: runtimeHolder(fmt.Sprintf("worker-%d", index)), events: &events}
	}
	supervisor, err := NewRuntimeSupervisor(listener, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: workers}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < runtimeCrashLimit; attempt++ {
		if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); err != nil {
			t.Fatalf("replacement %d: %v", attempt+1, err)
		}
	}
	if _, err := supervisor.ReplaceFailedWorker(context.Background(), manifest); !errors.Is(err, ErrRuntimeCrashLoop) {
		t.Fatalf("fourth crash replacement error = %v", err)
	}
	if supervisor.AdmissionReady() {
		t.Fatal("crash-loop supervisor remained admitting")
	}
}
