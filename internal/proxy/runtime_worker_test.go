//go:build !windows

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
	"golang.org/x/sys/unix"
)

func TestRuntimeCallerCredentialStateRetainsKnownExpiryBeyondAdmissionLifetime(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, sha256.Size)
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	current := []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "account\x00candidate\x00revision-a", ValidUntil: expires,
	}}
	state, err := newRuntimeCallerCredentialState(key, func(context.Context) ([]NormalCallerCredentialV1, error) {
		return append([]NormalCallerCredentialV1(nil), current...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state.now = func() time.Time { return now }
	bound, _, err := state.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldSubject := bound[0].SubjectID
	current = []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "new-bearer", SubjectID: "account\x00candidate\x00revision-b", ValidUntil: expires,
	}}
	if _, _, err := state.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)

	var forwardedBearer string
	var forwardedIdentity string
	handler := normalWorkerHandlerWithSource(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwardedBearer = request.Header.Get("Authorization")
		forwardedIdentity, _ = runtimeCallerIdentity(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}), state.credentials)
	request := httptest.NewRequest(http.MethodPost, "http://cq.test/responses", nil)
	request = request.WithContext(withRuntimeCallerAuthority(request.Context(), RuntimeCallerAuthorityV1{
		SchemaVersion:     1,
		Kind:              "provider_branch_admission_consumed_v1",
		Domain:            NormalCallerCodex,
		SubjectID:         oldSubject,
		BearerFingerprint: "fingerprint",
		IndexEpoch:        1,
		AdmissionID:       strings.Repeat("a", 32),
		SingleUseNonce:    strings.Repeat("b", 32),
		RequestNonce:      strings.Repeat("c", 32),
		Method:            http.MethodPost,
		RequestURI:        "/responses",
		ValidUntil:        time.Now().Add(time.Minute),
		ConsumptionDigest: "consumption",
		MAC:               "mac",
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("superseded caller status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if forwardedBearer != "Bearer old-bearer" || forwardedIdentity != "account\x00candidate\x00revision-a" {
		t.Fatalf("superseded caller forwarding = bearer %q identity %q", forwardedBearer, forwardedIdentity)
	}
}

func TestRuntimeCallerCredentialStateBoundsOpaqueBearerRollover(t *testing.T) {
	key := bytes.Repeat([]byte{0x72}, sha256.Size)
	current := []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "opaque-bearer", SubjectID: "account\x00candidate\x00revision-a",
	}}
	state, err := newRuntimeCallerCredentialState(key, func(context.Context) ([]NormalCallerCredentialV1, error) {
		return append([]NormalCallerCredentialV1(nil), current...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, _, err := state.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldSubject := initial[0].SubjectID
	current = []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "opaque-bearer", SubjectID: "account\x00candidate\x00revision-b",
	}}

	forwardedIdentity := ""
	handler := normalWorkerHandlerWithSource(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwardedIdentity, _ = runtimeCallerIdentity(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}), state.credentials)
	caller := RuntimeCallerAuthorityV1{
		SchemaVersion:     1,
		Kind:              "provider_branch_admission_consumed_v1",
		Domain:            NormalCallerCodex,
		SubjectID:         oldSubject,
		BearerFingerprint: "fingerprint",
		IndexEpoch:        1,
		AdmissionID:       strings.Repeat("a", 32),
		SingleUseNonce:    strings.Repeat("b", 32),
		RequestNonce:      strings.Repeat("c", 32),
		Method:            http.MethodPost,
		RequestURI:        "/responses",
		ValidUntil:        time.Now().Add(time.Minute),
		ConsumptionDigest: "consumption",
		MAC:               "mac",
	}
	request := httptest.NewRequest(http.MethodPost, "http://cq.test/responses", nil)
	request = request.WithContext(withRuntimeCallerAuthority(request.Context(), caller))
	response := httptest.NewRecorder()
	rolloverStarted := time.Now()
	handler.ServeHTTP(response, request)
	rolloverFinished := time.Now()
	if response.Code != http.StatusNoContent || forwardedIdentity != "account\x00candidate\x00revision-a" {
		t.Fatalf("opaque rollover = status %d identity %q", response.Code, forwardedIdentity)
	}

	foundRetired := false
	retireAt := time.Time{}
	state.mu.Lock()
	for index := range state.bound {
		if state.bound[index].SubjectID == oldSubject {
			retireAt = state.bound[index].ValidUntil
			state.bound[index].ValidUntil = time.Now().Add(-time.Second)
			foundRetired = true
		}
	}
	state.mu.Unlock()
	if !foundRetired {
		t.Fatal("superseded opaque caller lacked bounded retirement")
	}
	if retireAt.Before(rolloverStarted.Add(5*time.Second)) || retireAt.After(rolloverFinished.Add(5*time.Second)) {
		t.Fatalf("opaque caller retirement = %v, want five seconds after rollover", retireAt)
	}

	request = httptest.NewRequest(http.MethodPost, "http://cq.test/responses", nil)
	request = request.WithContext(withRuntimeCallerAuthority(request.Context(), caller))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expired opaque rollover status = %d, want %d", response.Code, http.StatusForbidden)
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
