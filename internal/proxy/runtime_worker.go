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
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type RuntimeRoleFiles struct {
	Listener  *os.File
	Lifecycle *os.File
	Control   *os.File
	Secret    *os.File
}

func (files *RuntimeRoleFiles) Close() error {
	if files == nil {
		return nil
	}
	return errors.Join(closeRuntimeFile(files.Listener), closeRuntimeFile(files.Lifecycle), closeRuntimeFile(files.Control), closeRuntimeFile(files.Secret))
}

func closeRuntimeFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func ValidateRuntimeRoleFiles(manifest RuntimeRoleManifestV1, files RuntimeRoleFiles) error {
	if files.Secret == nil || ValidateRuntimeRoleDescriptors(manifest, files) != nil {
		return ErrRuntimeRoleManifest
	}
	return nil
}

func ValidateRuntimeRoleDescriptors(manifest RuntimeRoleManifestV1, files RuntimeRoleFiles) error {
	if err := manifest.validate(); err != nil || files.Lifecycle == nil || files.Control == nil {
		return ErrRuntimeRoleManifest
	}
	if (manifest.Role == RuntimeRoleSupervisor && files.Listener == nil) || (manifest.Role == RuntimeRoleWorker && files.Listener != nil) {
		return ErrRuntimeRoleManifest
	}
	digest, err := RuntimeDescriptorIdentityDigest(files.Lifecycle)
	if err != nil || digest != manifest.LifecycleHolderIdentityDigest {
		return ErrRuntimeRoleManifest
	}
	return nil
}

// RunRuntimeWorkerRole validates its private-only inherited descriptors before
// reading channel material or accepting control messages.
func RunRuntimeWorkerRole(ctx context.Context, manifest RuntimeRoleManifestV1, files RuntimeRoleFiles) error {
	return RunRuntimeWorkerRoleWithHandler(ctx, manifest, files, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "runtime worker unavailable", http.StatusServiceUnavailable)
	}))
}

func RunRuntimeWorkerRoleWithHandler(ctx context.Context, manifest RuntimeRoleManifestV1, files RuntimeRoleFiles, handler http.Handler) error {
	defer files.Close()
	if manifest.Role != RuntimeRoleWorker || ValidateRuntimeRoleFiles(manifest, files) != nil || handler == nil {
		return ErrRuntimeRoleManifest
	}
	secret, err := ReadRuntimeSecret(files.Secret)
	files.Secret = nil
	if err != nil {
		return err
	}
	defer secret.Destroy()
	connection, err := net.FileConn(files.Control)
	if err != nil {
		return err
	}
	_ = files.Control.Close()
	files.Control = nil
	defer connection.Close()
	receiver := NewRuntimeControlReceiver(secret)
	defer receiver.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-done:
		}
	}()
	sequence := uint64(1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := ReadRuntimeControlMessage(connection, receiver)
		if err != nil {
			return err
		}
		kind := ""
		switch frame.Kind {
		case "hello":
			kind = "ready"
		case "begin_drain":
			kind = "draining"
		case "await_quiescence":
			kind = "quiescent"
		case "shutdown":
			kind = "stopped"
		case "http_request":
			var request RuntimeHTTPRequestV1
			if len(frame.Payload) == 0 || json.Unmarshal(frame.Payload, &request) != nil || request.Method == "" || request.RequestURI == "" || len(request.Body) > RuntimeHTTPBodyLimit {
				return ErrRuntimeControlFrame
			}
			httpRequest, requestErr := http.NewRequestWithContext(ctx, request.Method, request.RequestURI, bytes.NewReader(request.Body))
			if requestErr != nil {
				return ErrRuntimeControlFrame
			}
			httpRequest.Header = request.Header.Clone()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httpRequest)
			result := recorder.Result()
			body, readErr := io.ReadAll(io.LimitReader(result.Body, RuntimeHTTPBodyLimit+1))
			_ = result.Body.Close()
			if readErr != nil || len(body) > RuntimeHTTPBodyLimit {
				return ErrRuntimeControlBackpressure
			}
			payload, marshalErr := json.Marshal(RuntimeHTTPResponseV1{StatusCode: result.StatusCode, Header: result.Header.Clone(), Body: body})
			if marshalErr != nil {
				return marshalErr
			}
			if err := WriteRuntimeControlMessage(connection, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: "http_response", Payload: payload}); err != nil {
				return err
			}
			sequence++
			continue
		default:
			return ErrRuntimeControlFrame
		}
		if err := WriteRuntimeControlMessage(connection, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: kind, Payload: json.RawMessage(`{}`)}); err != nil {
			return err
		}
		sequence++
		if frame.Kind == "shutdown" {
			return nil
		}
	}
}

type RuntimeProcessWorkerLauncher struct {
	Executable       string
	BaseManifest     RuntimeRoleManifestV1
	SupervisorHolder LifecycleHolderProof
	OpenLifecycle    func() (*os.File, LifecycleHolderProof, error)
	Random           io.Reader
	Command          func(context.Context, string, ...string) *exec.Cmd
}

func runtimeExecutableDigest(path string) ([sha256.Size]byte, unix.Stat_t, error) {
	var empty [sha256.Size]byte
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return empty, unix.Stat_t{}, err
	}
	file := os.NewFile(uintptr(fd), "runtime-worker-executable")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink == 0 {
		return empty, unix.Stat_t{}, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return empty, unix.Stat_t{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, stat, nil
}

func (launcher *RuntimeProcessWorkerLauncher) Launch(ctx context.Context, workerManifest WorkerManifestV1) (RuntimeWorkerProcess, error) {
	if launcher == nil || launcher.Executable == "" || launcher.OpenLifecycle == nil || workerManifest.SchemaVersion != 1 {
		return nil, ErrRuntimeWorkerUnavailable
	}
	lifecycle, holder, err := launcher.OpenLifecycle()
	if err != nil {
		return nil, err
	}
	closeLifecycle := true
	defer func() {
		if closeLifecycle {
			_ = lifecycle.Close()
		}
	}()
	if err := ValidateDistinctLifecycleHolders(launcher.SupervisorHolder, holder); err != nil {
		return nil, err
	}
	holderDigest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		return nil, err
	}
	artifactDigest, err := hex.DecodeString(workerManifest.WorkerArtifactDigest)
	if err != nil || len(artifactDigest) != sha256.Size || workerManifest.WorkerArtifactDigest != strings.ToLower(workerManifest.WorkerArtifactDigest) {
		return nil, ErrRuntimeRoleManifest
	}
	verifiedDigest, verifiedStat, err := runtimeExecutableDigest(launcher.Executable)
	if err != nil || !bytes.Equal(verifiedDigest[:], artifactDigest) {
		return nil, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	reopenedDigest, reopenedStat, err := runtimeExecutableDigest(launcher.Executable)
	if err != nil || reopenedStat.Dev != verifiedStat.Dev || reopenedStat.Ino != verifiedStat.Ino || reopenedDigest != verifiedDigest {
		return nil, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	workerRole := launcher.BaseManifest
	workerRole.Role = RuntimeRoleWorker
	workerRole.ListenerFD = RuntimeNoListenerFD
	workerRole.LifecycleFD = RuntimeLifecycleFD
	workerRole.ControlFD = RuntimeControlFD
	workerRole.SecretFD = RuntimeSecretFD
	copy(workerRole.ManifestDigest[:], artifactDigest)
	workerRole.LifecycleHolderIdentityDigest = holderDigest
	arguments := RuntimeRoleArguments(workerRole)
	if arguments == nil {
		return nil, ErrRuntimeRoleManifest
	}
	supervisorControlFile, workerControlFile, err := newRuntimePrivateSocketFiles()
	if err != nil {
		return nil, err
	}
	defer func() { _ = supervisorControlFile.Close(); _ = workerControlFile.Close() }()
	secretReader, secretWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer secretReader.Close()
	defer secretWriter.Close()
	placeholder, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer placeholder.Close()
	commandFactory := launcher.Command
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	command := commandFactory(ctx, launcher.Executable, append([]string{"proxy", "start"}, arguments...)...)
	command.ExtraFiles = []*os.File{placeholder, lifecycle, workerControlFile, secretReader}
	spawnDigest, spawnStat, err := runtimeExecutableDigest(launcher.Executable)
	if err != nil || spawnStat.Dev != verifiedStat.Dev || spawnStat.Ino != verifiedStat.Ino || spawnDigest != verifiedDigest {
		return nil, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	material := make([]byte, RuntimeSecretSize)
	defer zeroRuntimeBytes(material)
	random := launcher.Random
	if random == nil {
		random = rand.Reader
	}
	if _, err := io.ReadFull(random, material); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, err
	}
	if _, err := secretWriter.Write(material); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, err
	}
	_ = secretWriter.Close()
	secret, err := NewRuntimeSecret(material)
	if err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, err
	}
	control, err := net.FileConn(supervisorControlFile)
	if err != nil {
		secret.Destroy()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return nil, err
	}
	closeLifecycle = false
	return &runtimeProcessWorker{command: command, control: control, secret: secret, holder: holder, lifecycle: lifecycle, receiver: NewRuntimeControlReceiver(secret), next: 1}, nil
}

type runtimeProcessWorker struct {
	mu        sync.Mutex
	command   *exec.Cmd
	control   net.Conn
	secret    *RuntimeSecret
	receiver  *RuntimeControlReceiver
	holder    LifecycleHolderProof
	lifecycle *os.File
	next      uint64
}

func (worker *runtimeProcessWorker) exchange(ctx context.Context, kind string, payload json.RawMessage) (RuntimeControlFrameV1, error) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.control == nil || ctx == nil {
		return RuntimeControlFrameV1{}, ErrRuntimeWorkerUnavailable
	}
	connection := worker.control
	if err := ctx.Err(); err != nil {
		return RuntimeControlFrameV1{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-done:
		}
	}()
	defer connection.SetDeadline(time.Time{})
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := WriteRuntimeControlMessage(connection, worker.secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: worker.next, Kind: kind, Payload: payload}); err != nil {
		_ = connection.Close()
		worker.control = nil
		return RuntimeControlFrameV1{}, runtimeControlIOError(ctx, err)
	}
	worker.next++
	frame, err := ReadRuntimeControlMessage(connection, worker.receiver)
	if err != nil {
		_ = connection.Close()
		worker.control = nil
	}
	if err != nil {
		return RuntimeControlFrameV1{}, runtimeControlIOError(ctx, err)
	}
	return frame, err
}

func runtimeControlIOError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var timeout net.Error
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline && !time.Now().Before(deadline) && errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}
func (worker *runtimeProcessWorker) Boot(ctx context.Context, _ WorkerManifestV1) (RuntimeBootAckV1, error) {
	frame, err := worker.exchange(ctx, "hello", nil)
	if err != nil || frame.Kind != "ready" {
		return RuntimeBootAckV1{}, errors.Join(ErrRuntimeWorkerUnavailable, err)
	}
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder}, nil
}
func (worker *runtimeProcessWorker) BeginDrain(ctx context.Context, _ TrafficMode, _ uint64) error {
	frame, err := worker.exchange(ctx, "begin_drain", nil)
	if err == nil && frame.Kind != "draining" {
		err = ErrRuntimeControlFrame
	}
	return err
}
func (worker *runtimeProcessWorker) AwaitQuiescence(ctx context.Context, _ uint64) (RuntimeQuiescenceAckV1, error) {
	frame, err := worker.exchange(ctx, "await_quiescence", nil)
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: err == nil && frame.Kind == "quiescent"}, err
}
func (worker *runtimeProcessWorker) HolderProof() LifecycleHolderProof { return worker.holder }
func (worker *runtimeProcessWorker) ExecuteHTTP(ctx context.Context, request RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	if request.Method == "" || request.RequestURI == "" || len(request.Body) > RuntimeHTTPBodyLimit {
		return RuntimeHTTPResponseV1{}, ErrRuntimeControlFrame
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return RuntimeHTTPResponseV1{}, err
	}
	frame, err := worker.exchange(ctx, "http_request", payload)
	if err != nil || frame.Kind != "http_response" {
		return RuntimeHTTPResponseV1{}, errors.Join(ErrRuntimeWorkerUnavailable, err)
	}
	var response RuntimeHTTPResponseV1
	if json.Unmarshal(frame.Payload, &response) != nil || response.StatusCode < 100 || response.StatusCode > 999 || len(response.Body) > RuntimeHTTPBodyLimit {
		return RuntimeHTTPResponseV1{}, ErrRuntimeControlFrame
	}
	return response, nil
}
func (worker *runtimeProcessWorker) StopAndReap(ctx context.Context) (RuntimeWorkerReleaseV1, error) {
	_, exchangeErr := worker.exchange(ctx, "shutdown", nil)
	waited := make(chan error, 1)
	go func() { waited <- worker.command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		_ = worker.command.Process.Kill()
		waitErr = errors.Join(ctx.Err(), <-waited)
	}
	worker.receiver.Close()
	_ = worker.control.Close()
	_ = worker.lifecycle.Close()
	worker.secret.Destroy()
	if exchangeErr != nil || waitErr != nil {
		return RuntimeWorkerReleaseV1{}, errors.Join(exchangeErr, waitErr)
	}
	identity := sha256.Sum256([]byte(worker.holder.DescriptionID))
	value := hex.EncodeToString(identity[:])
	return RuntimeWorkerReleaseV1{ProcessIdentityDigest: value, ProcessTreeAbsenceProofDigest: value, HolderReleaseProofDigest: value}, nil
}

var (
	ErrRuntimeCheckpointUnavailable = errors.New("runtime checkpoint unavailable")
	ErrRuntimeWorkerUnavailable     = errors.New("runtime worker unavailable")
	ErrRuntimeArtifactMismatch      = errors.New("runtime worker artifact mismatch")
)

type RuntimeWorkV1 struct {
	RequestID string `json:"request_id"`
	Body      []byte `json:"body,omitempty"`
}

type RuntimeWorkResultV1 struct {
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"body,omitempty"`
}

type RuntimeWorkHandler func(context.Context, RuntimeWorkV1) (RuntimeWorkResultV1, error)

// RuntimeWorker has no public listener. It admits private work only after its
// distinct shared lifecycle holder participates in a selected checkpoint.
type RuntimeWorker struct {
	mu               sync.RWMutex
	supervisorHolder LifecycleHolderProof
	workerHolder     LifecycleHolderProof
	handler          RuntimeWorkHandler
	checkpointDigest string
}

func NewRuntimeWorker(supervisorHolder, workerHolder LifecycleHolderProof, handler RuntimeWorkHandler) (*RuntimeWorker, error) {
	if err := ValidateDistinctLifecycleHolders(supervisorHolder, workerHolder); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, ErrRuntimeWorkerUnavailable
	}
	return &RuntimeWorker{supervisorHolder: supervisorHolder, workerHolder: workerHolder, handler: handler}, nil
}

func (worker *RuntimeWorker) SelectCheckpoint(digest string) error {
	if worker == nil || digest == "" {
		return ErrRuntimeCheckpointUnavailable
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.checkpointDigest != "" {
		return ErrRuntimeCheckpointUnavailable
	}
	worker.checkpointDigest = digest
	return nil
}

func (worker *RuntimeWorker) Execute(ctx context.Context, work RuntimeWorkV1) (RuntimeWorkResultV1, error) {
	if worker == nil || ctx == nil || work.RequestID == "" {
		return RuntimeWorkResultV1{}, ErrRuntimeWorkerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RuntimeWorkResultV1{}, err
	}
	worker.mu.RLock()
	digest := worker.checkpointDigest
	handler := worker.handler
	worker.mu.RUnlock()
	if digest == "" {
		return RuntimeWorkResultV1{}, ErrRuntimeCheckpointUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RuntimeWorkResultV1{}, err
	}
	return handler(ctx, work)
}
