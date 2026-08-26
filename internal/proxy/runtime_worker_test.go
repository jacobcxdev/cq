package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestRuntimeProcessWorkerStopAndReapAfterControlFailure(t *testing.T) {
	secret, err := NewRuntimeSecret(bytes.Repeat([]byte{0x52}, RuntimeSecretSize))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := os.OpenFile(t.TempDir()+"/lifecycle.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan struct{})
	close(waitDone)
	worker := &runtimeProcessWorker{
		secret: secret, receiver: NewRuntimeControlReceiver(secret), lifecycle: lifecycle,
		holder: LifecycleHolderProof{DescriptionID: "worker"}, waitDone: waitDone,
	}

	if release, err := worker.StopAndReap(context.Background()); err != nil || !release.valid() {
		t.Fatalf("StopAndReap() = (%#v, %v)", release, err)
	}
}

func TestRuntimeProcessWorkerStopAndReapToleratesPartialCleanup(t *testing.T) {
	waitDone := make(chan struct{})
	close(waitDone)
	worker := &runtimeProcessWorker{
		holder:   LifecycleHolderProof{DescriptionID: "partial-worker"},
		waitDone: waitDone,
	}

	if release, err := worker.StopAndReap(context.Background()); err != nil || !release.valid() {
		t.Fatalf("StopAndReap() = (%#v, %v)", release, err)
	}
}

func TestRuntimeProcessWorkerStopAndReapSynchronisesControlCleanup(t *testing.T) {
	control, peer := net.Pipe()
	defer peer.Close()
	waitDone := make(chan struct{})
	close(waitDone)
	worker := &runtimeProcessWorker{
		control:  control,
		holder:   LifecycleHolderProof{DescriptionID: "concurrent-worker"},
		waitDone: waitDone,
	}
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				worker.mu.Lock()
				worker.control = nil
				worker.mu.Unlock()
			}
		}
	}()
	<-started
	_, _ = worker.StopAndReap(context.Background())
	close(stop)
	<-done
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
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD, WorkFD: RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	var workerCommand *exec.Cmd
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
			workerCommand = exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestRuntimeWorkerRoleHelperProcess", "--"}, args...)...)
			return workerCommand
		},
	}
	process, err := launcher.Launch(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])})
	if err != nil {
		t.Fatal(err)
	}
	if workerCommand == nil || workerCommand.Stderr != os.Stderr {
		t.Fatal("worker stderr is not connected to supervisor stderr")
	}
	boot, err := process.Boot(context.Background(), WorkerManifestV1{})
	if err != nil {
		t.Fatal(err)
	}
	if len(boot.CallerAuthorityKey) != 32 || len(boot.CallerIndex.Entries) != 1 || boot.CallerIndex.Entries[0].Domain != NormalCallerCodex {
		t.Fatalf("worker caller index = %#v", boot.CallerIndex)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	authority, err := NewNormalCallerAuthorityFromIndex(boot.CallerAuthorityKey, boot.CallerIndex, consumer, time.Now, rand.Reader)
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
	streaming, ok := process.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request, RuntimeCallerAuthorityV1) error
	})
	if !ok {
		t.Fatal("process worker has no streaming HTTP transport")
	}
	newCaller := func(request *http.Request) RuntimeCallerAuthorityV1 {
		request.Header.Set("Authorization", "Bearer worker-only-bearer")
		authentication, authErr := authority.authenticate(request, normalCallerRouteCodex)
		if authErr != nil {
			t.Fatal(authErr)
		}
		result, consumeErr := authority.consume(context.Background(), authentication, request)
		if consumeErr != nil {
			t.Fatal(consumeErr)
		}
		return result
	}
	largeBody := bytes.Repeat([]byte("x"), 96<<10)
	largeRequest := httptest.NewRequest(http.MethodPost, "http://runtime/responses?mode=large", bytes.NewReader(largeBody))
	largeResponse := httptest.NewRecorder()
	if err := streaming.ServeHTTP(largeResponse, largeRequest, newCaller(largeRequest)); err != nil {
		t.Fatal(err)
	}
	if largeResponse.Code != http.StatusCreated || largeResponse.Body.String() != strconv.Itoa(len(largeBody)) {
		t.Fatalf("large response = %d %q", largeResponse.Code, largeResponse.Body.String())
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := streaming.ServeHTTP(writer, request, newCaller(request)); err != nil {
			http.Error(writer, err.Error(), http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	sseRequest, err := http.NewRequest(http.MethodPost, server.URL+"/responses?mode=sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	sseRequest.Header.Set("Authorization", "Bearer worker-only-bearer")
	sseResponse, err := server.Client().Do(sseRequest)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: one\n\n"))
	started := time.Now()
	if _, err := io.ReadFull(sseResponse.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "data: one\n\n" || time.Since(started) > 150*time.Millisecond {
		t.Fatalf("first SSE event = %q after %v", first, time.Since(started))
	}
	_ = sseResponse.Body.Close()

	wsHeader := http.Header{"Authorization": {"Bearer worker-only-bearer"}}
	wsConnection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/responses?mode=websocket", wsHeader)
	if err != nil {
		t.Fatal(err)
	}
	defer wsConnection.Close()
	if err := wsConnection.WriteMessage(websocket.TextMessage, []byte("round-trip")); err != nil {
		t.Fatal(err)
	}
	messageType, message, err := wsConnection.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || string(message) != "round-trip" {
		t.Fatalf("websocket response = %d %q, %v", messageType, message, err)
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
		Work:      os.NewFile(RuntimeWorkFD, "work"),
	}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("mode") {
		case "large":
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, readErr.Error(), http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, strconv.Itoa(len(body)))
			return
		case "sse":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: one\n\n")
			writer.(http.Flusher).Flush()
			time.Sleep(250 * time.Millisecond)
			_, _ = io.WriteString(writer, "data: two\n\n")
			return
		case "websocket":
			connection, upgradeErr := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
			if upgradeErr != nil {
				return
			}
			defer connection.Close()
			messageType, message, readErr := connection.ReadMessage()
			if readErr == nil {
				_ = connection.WriteMessage(messageType, message)
			}
			return
		}
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
