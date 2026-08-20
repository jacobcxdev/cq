package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func TestRuntimeProcessWorkerBootCancellationInterruptsPrivateSocket(t *testing.T) {
	supervisor, child := net.Pipe()
	defer child.Close()
	secret, err := NewRuntimeSecret(bytes.Repeat([]byte{0x51}, RuntimeSecretSize))
	if err != nil {
		t.Fatal(err)
	}
	worker := &runtimeProcessWorker{control: supervisor, secret: secret, receiver: NewRuntimeControlReceiver(secret), next: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := worker.Boot(ctx, WorkerManifestV1{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Boot cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Boot cancellation took %v", elapsed)
	}
	if worker.control != nil {
		t.Fatal("cancelled private socket remained reusable")
	}
}

func runtimeHolder(description string) LifecycleHolderProof {
	return LifecycleHolderProof{
		LockIdentity:  fsutil.SecureFileIdentity{Device: 7, Inode: 11, Links: 1},
		DescriptionID: description,
		Mode:          LifecycleShared,
	}
}

func TestRuntimeWorkerProcessUsesPrivateTransportWithoutPublicListenerFD(t *testing.T) {
	path := t.TempDir() + "/lifecycle.lock"
	if err := os.WriteFile(path, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervisorFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisorFile.Close()
	if err := unix.Flock(int(supervisorFile.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	supervisorProof, err := RuntimeLifecycleHolder(supervisorFile, "supervisor-description")
	if err != nil {
		t.Fatal(err)
	}
	holderDigest, err := RuntimeDescriptorIdentityDigest(supervisorFile)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	manifestHasher := sha256.New()
	if _, err := io.Copy(manifestHasher, executable); err != nil {
		t.Fatal(err)
	}
	_ = executable.Close()
	var manifestDigest [sha256.Size]byte
	copy(manifestDigest[:], manifestHasher.Sum(nil))
	base := RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: "proxy-a", RuntimeInstanceID: "runtime-a",
		ListenerFD: RuntimeListenerFD, LifecycleFD: RuntimeLifecycleFD,
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := &RuntimeProcessWorkerLauncher{
		Executable: os.Args[0], BaseManifest: base, SupervisorHolder: supervisorProof,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x6e}, RuntimeSecretSize)),
		OpenLifecycle: func() (*os.File, LifecycleHolderProof, error) {
			file, err := os.Open(path)
			if err != nil {
				return nil, LifecycleHolderProof{}, err
			}
			if err := unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
				_ = file.Close()
				return nil, LifecycleHolderProof{}, err
			}
			proof, err := RuntimeLifecycleHolder(file, "worker-description")
			return file, proof, err
		},
		Command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestRuntimeWorkerRoleHelperProcess", "--"}, args...)...)
		},
	}
	process, err := launcher.Launch(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])})
	if err != nil {
		t.Fatal(err)
	}
	boot, err := process.Boot(context.Background(), WorkerManifestV1{})
	if err != nil {
		t.Fatal(err)
	}
	if len(boot.CallerAuthorityKey) != 32 || len(boot.CallerIndex.Entries) != 1 || boot.CallerIndex.Entries[0].Domain != NormalCallerCodex {
		t.Fatalf("worker caller index = %#v", boot.CallerIndex)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	authority, err := NewNormalCallerAuthorityFromIndex(boot.CallerAuthorityKey, boot.CallerIndex, consumer, time.Now, bytes.NewReader(bytes.Repeat([]byte{0x71}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	authorityRequest, _ := http.NewRequest(http.MethodPost, "/responses", nil)
	authorityRequest.Header.Set("Authorization", "Bearer worker-only-bearer")
	authentication, err := authority.authenticate(authorityRequest, normalCallerRouteCodex)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := authority.consume(context.Background(), authentication, authorityRequest)
	if err != nil {
		t.Fatal(err)
	}
	response, err := process.ExecuteHTTP(context.Background(), RuntimeHTTPRequestV1{
		Method: http.MethodPost, RequestURI: "/responses",
		Header: http.Header{"X-Request": {"private"}}, Body: []byte("payload"), Caller: caller,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Worker") != "child" || string(response.Body) != "Bearer worker-only-bearer" {
		t.Fatalf("worker response = %#v", response)
	}
	if err := process.BeginDrain(context.Background(), TrafficModeDrain, 0); err != nil {
		t.Fatal(err)
	}
	ack, err := process.AwaitQuiescence(context.Background(), 0)
	if err != nil || !ack.Quiescent {
		t.Fatalf("quiescence = %#v, %v", ack, err)
	}
	if _, err := process.StopAndReap(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeWorkerRoleHelperProcess(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		return
	}
	arguments := os.Args[separator+1:]
	if len(arguments) < 2 || arguments[0] != "proxy" || arguments[1] != "start" {
		os.Exit(91)
	}
	manifest, err := ParseRuntimeRoleArguments(arguments[2:])
	if err != nil || manifest.Role != RuntimeRoleWorker || slices.Contains(arguments, "--listener-fd") {
		os.Exit(92)
	}
	var reserved unix.Stat_t
	if err := unix.Fstat(RuntimeListenerFD, &reserved); err != nil || reserved.Mode&unix.S_IFMT == unix.S_IFSOCK {
		os.Exit(93)
	}
	_ = unix.Close(RuntimeListenerFD)
	err = RunRuntimeWorkerRoleWithHandlerAndCallerCredentials(context.Background(), manifest, RuntimeRoleFiles{
		Lifecycle: os.NewFile(RuntimeLifecycleFD, "lifecycle"),
		Control:   os.NewFile(RuntimeControlFD, "control"),
		Secret:    os.NewFile(RuntimeSecretFD, "secret"),
	}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Worker", "child")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(request.Header.Get("Authorization")))
	}), []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "worker-only-bearer", SubjectID: "codex-worker"}})
	if err != nil {
		os.Exit(94)
	}
	os.Exit(0)
}

func TestRuntimeWorkerLauncherRejectsArtifactMismatchBeforeSpawn(t *testing.T) {
	path := t.TempDir() + "/lifecycle.lock"
	if err := os.WriteFile(path, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervisorFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisorFile.Close()
	supervisorProof, err := RuntimeLifecycleHolder(supervisorFile, "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	spawned := false
	launcher := &RuntimeProcessWorkerLauncher{
		Executable: os.Args[0], BaseManifest: RuntimeRoleManifestV1{SchemaVersion: 1, ProxyInstanceID: "proxy", RuntimeInstanceID: "runtime"}, SupervisorHolder: supervisorProof,
		OpenLifecycle: func() (*os.File, LifecycleHolderProof, error) {
			file, openErr := os.Open(path)
			if openErr != nil {
				return nil, LifecycleHolderProof{}, openErr
			}
			proof, proofErr := RuntimeLifecycleHolder(file, "worker")
			return file, proof, proofErr
		},
		Command: func(context.Context, string, ...string) *exec.Cmd { spawned = true; return exec.Command(os.Args[0]) },
	}
	if _, err := launcher.Launch(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: strings.Repeat("0", 64)}); !errors.Is(err, ErrRuntimeArtifactMismatch) {
		t.Fatalf("Launch mismatch error = %v", err)
	}
	if spawned {
		t.Fatal("artifact mismatch reached spawn")
	}
}

func TestRuntimeWorkerRequiresDistinctHolderAndSelectedCheckpoint(t *testing.T) {
	calls := 0
	worker, err := NewRuntimeWorker(runtimeHolder("supervisor"), runtimeHolder("worker"), func(context.Context, RuntimeWorkV1) (RuntimeWorkResultV1, error) {
		calls++
		return RuntimeWorkResultV1{StatusCode: 204}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Execute(context.Background(), RuntimeWorkV1{RequestID: "r-1"}); !errors.Is(err, ErrRuntimeCheckpointUnavailable) {
		t.Fatalf("pre-checkpoint Execute error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("handler calls = %d before checkpoint", calls)
	}
	if err := worker.SelectCheckpoint("checkpoint-digest"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Execute(context.Background(), RuntimeWorkV1{RequestID: "r-1"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if _, err := NewRuntimeWorker(runtimeHolder("same"), runtimeHolder("same"), nil); !errors.Is(err, ErrLifecycleHolderConflict) {
		t.Fatalf("duplicate holder error = %v", err)
	}
}

func TestRuntimeWorkerCancellationDoesNotInvokeHandler(t *testing.T) {
	worker, err := NewRuntimeWorker(runtimeHolder("supervisor"), runtimeHolder("worker"), func(context.Context, RuntimeWorkV1) (RuntimeWorkResultV1, error) {
		t.Fatal("handler invoked after cancellation")
		return RuntimeWorkResultV1{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SelectCheckpoint("checkpoint-digest"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.Execute(ctx, RuntimeWorkV1{RequestID: "r-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v", err)
	}
}
