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
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type RuntimeRoleFiles struct {
	Listener  *os.File
	Lifecycle *os.File
	Control   *os.File
	Secret    *os.File
	Work      *os.File
}

func (files *RuntimeRoleFiles) Close() error {
	if files == nil {
		return nil
	}
	return errors.Join(closeRuntimeFile(files.Listener), closeRuntimeFile(files.Lifecycle), closeRuntimeFile(files.Control), closeRuntimeFile(files.Secret), closeRuntimeFile(files.Work))
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
	if (manifest.Role == RuntimeRoleSupervisor && (files.Listener == nil || files.Work != nil)) ||
		(manifest.Role == RuntimeRoleWorker && (files.Listener != nil || files.Work == nil)) {
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
	return RunRuntimeWorkerRoleWithHandlerAndCallerCredentials(ctx, manifest, files, handler, nil)
}

func RunRuntimeWorkerRoleWithHandlerAndCallerCredentials(ctx context.Context, manifest RuntimeRoleManifestV1, files RuntimeRoleFiles, handler http.Handler, credentials []NormalCallerCredentialV1) error {
	return RunRuntimeWorkerRoleWithHandlerAndCallerCredentialSource(ctx, manifest, files, handler, func(context.Context) ([]NormalCallerCredentialV1, error) {
		return append([]NormalCallerCredentialV1(nil), credentials...), nil
	})
}

type NormalCallerCredentialSource func(context.Context) ([]NormalCallerCredentialV1, error)

type runtimeCallerCredentialState struct {
	mu     sync.Mutex
	key    []byte
	epoch  uint64
	source NormalCallerCredentialSource
	bound  []NormalCallerCredentialV1
	index  NormalCallerIndexV1
}

func newRuntimeCallerCredentialState(key []byte, source NormalCallerCredentialSource) (*runtimeCallerCredentialState, error) {
	if len(key) != sha256.Size || source == nil {
		return nil, ErrNormalCallerAuthUnavailable
	}
	return &runtimeCallerCredentialState{key: append([]byte(nil), key...), source: source}, nil
}

func (state *runtimeCallerCredentialState) snapshot(ctx context.Context) ([]NormalCallerCredentialV1, NormalCallerIndexV1, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	credentials, err := state.source(ctx)
	if err != nil {
		return nil, NormalCallerIndexV1{}, err
	}
	bound, err := bindRuntimeCallerCredentials(state.key, credentials)
	if err != nil {
		return nil, NormalCallerIndexV1{}, err
	}
	epoch := state.epoch
	if epoch == 0 {
		epoch = 1
	}
	index, err := BuildNormalCallerIndexV1(state.key, epoch, bound)
	if err != nil {
		return nil, NormalCallerIndexV1{}, err
	}
	if state.epoch != 0 && !slices.Equal(index.Entries, state.index.Entries) {
		epoch++
		index, err = BuildNormalCallerIndexV1(state.key, epoch, bound)
		if err != nil {
			return nil, NormalCallerIndexV1{}, err
		}
	}
	state.epoch = epoch
	state.bound = append(state.bound[:0], bound...)
	state.index = index
	return append([]NormalCallerCredentialV1(nil), state.bound...), state.index, nil
}

func (state *runtimeCallerCredentialState) credentials(ctx context.Context) ([]NormalCallerCredentialV1, error) {
	credentials, _, err := state.snapshot(ctx)
	return credentials, err
}

func RunRuntimeWorkerRoleWithHandlerAndCallerCredentialSource(ctx context.Context, manifest RuntimeRoleManifestV1, files RuntimeRoleFiles, handler http.Handler, source NormalCallerCredentialSource) error {
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
	callerKey, err := normalCallerKeyFromRuntimeSecret(secret)
	if err != nil {
		return err
	}
	defer zeroRuntimeBytes(callerKey)
	callerState, err := newRuntimeCallerCredentialState(callerKey, source)
	if err != nil {
		return err
	}
	_, callerIndex, err := callerState.snapshot(ctx)
	if err != nil {
		return err
	}
	handler = normalWorkerHandlerWithSource(handler, callerState.credentials)
	workListener, err := net.FileListener(files.Work)
	if err != nil {
		return err
	}
	_ = files.Work.Close()
	files.Work = nil
	defer workListener.Close()
	workServer := &http.Server{Handler: runtimeWorkerIngressHandler(callerKey, callerState, handler), ReadHeaderTimeout: 10 * time.Second}
	workResult := make(chan error, 1)
	go func() {
		serveErr := workServer.Serve(workListener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		workResult <- serveErr
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = workServer.Shutdown(shutdownCtx)
		<-workResult
	}()
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
		payload := json.RawMessage(`{}`)
		switch frame.Kind {
		case "hello":
			kind = "ready"
			_, callerIndex, err = callerState.snapshot(ctx)
			if err != nil {
				return err
			}
			payload, err = json.Marshal(callerIndex)
			if err != nil {
				return err
			}
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
			if normalCallerPolicy(httpRequest) != normalCallerRoutePublic && (!validRuntimeCallerAuthority(request.Caller, request.Method, request.RequestURI, time.Now()) || !validateRuntimeCallerAuthorityMAC(callerKey, request.Caller)) {
				return ErrRuntimeControlFrame
			}
			httpRequest.Header = request.Header.Clone()
			if request.Caller.SchemaVersion == 1 {
				httpRequest = httpRequest.WithContext(withRuntimeCallerAuthority(httpRequest.Context(), request.Caller))
			}
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
		if err := WriteRuntimeControlMessage(connection, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: kind, Payload: payload}); err != nil {
			return err
		}
		sequence++
		if frame.Kind == "shutdown" {
			return nil
		}
	}
}

const (
	runtimeCallerAuthorityHeader = "X-Cq-Runtime-Caller-V1"
	runtimeCallerIndexPath       = "/__cq/runtime/caller-index"
)

func runtimeWorkerIngressHandler(key []byte, callerState *runtimeCallerCredentialState, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.EscapedPath() == runtimeCallerIndexPath {
			if request.Method != http.MethodGet || callerState == nil {
				http.Error(writer, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			_, index, err := callerState.snapshot(request.Context())
			if err != nil {
				http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(index)
			return
		}
		encoded := request.Header.Get(runtimeCallerAuthorityHeader)
		request.Header.Del(runtimeCallerAuthorityHeader)
		if normalCallerPolicy(request) == normalCallerRoutePublic {
			handler.ServeHTTP(writer, request)
			return
		}
		var caller RuntimeCallerAuthorityV1
		body, err := hex.DecodeString(encoded)
		if err != nil || json.Unmarshal(body, &caller) != nil || !validRuntimeCallerAuthority(caller, request.Method, request.URL.RequestURI(), time.Now()) || !validateRuntimeCallerAuthorityMAC(key, caller) {
			http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		handler.ServeHTTP(writer, request.WithContext(withRuntimeCallerAuthority(request.Context(), caller)))
	})
}

func normalCallerKeyFromRuntimeSecret(secret *RuntimeSecret) ([]byte, error) {
	material, err := secret.key()
	if err != nil {
		return nil, err
	}
	defer zeroRuntimeBytes(material)
	return DeriveNormalCallerAuthorityKey(material)
}

func bindRuntimeCallerCredentials(key []byte, credentials []NormalCallerCredentialV1) ([]NormalCallerCredentialV1, error) {
	bound := make([]NormalCallerCredentialV1, 0, len(credentials))
	for _, credential := range credentials {
		subjectID, err := NormalCallerSubjectID(key, credential.Domain, credential.SubjectID)
		if err != nil {
			return nil, err
		}
		credential.SubjectID = subjectID
		bound = append(bound, credential)
	}
	return bound, nil
}

type RuntimeProcessWorkerLauncher struct {
	Executable       string
	BaseManifest     RuntimeRoleManifestV1
	SupervisorHolder LifecycleHolderProof
	OpenLifecycle    func() (*os.File, LifecycleHolderProof, error)
	Random           io.Reader
	Command          func(context.Context, string, ...string) *exec.Cmd
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
	if err != nil || reopenedStat != verifiedStat || reopenedDigest != verifiedDigest {
		return nil, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	workerRole := launcher.BaseManifest
	workerRole.Role = RuntimeRoleWorker
	workerRole.ListenerFD = RuntimeNoListenerFD
	workerRole.LifecycleFD = RuntimeLifecycleFD
	workerRole.ControlFD = RuntimeControlFD
	workerRole.SecretFD = RuntimeSecretFD
	workerRole.WorkFD = RuntimeWorkFD
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
	privateDir, err := os.MkdirTemp("", "cq-runtime-worker-")
	if err != nil {
		return nil, err
	}
	removePrivateDir := true
	defer func() {
		if removePrivateDir {
			_ = os.RemoveAll(privateDir)
		}
	}()
	socketPath := filepath.Join(privateDir, "normal.sock")
	workListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	workListener.SetUnlinkOnClose(false)
	defer workListener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return nil, err
	}
	workFile, err := workListener.File()
	if err != nil {
		return nil, err
	}
	defer workFile.Close()
	commandFactory := launcher.Command
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	command := commandFactory(ctx, launcher.Executable, append([]string{"proxy", "start"}, arguments...)...)
	command.ExtraFiles = []*os.File{placeholder, lifecycle, workerControlFile, secretReader, workFile}
	spawnDigest, spawnStat, err := runtimeExecutableDigest(launcher.Executable)
	if err != nil || spawnStat != verifiedStat || spawnDigest != verifiedDigest {
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
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
		},
	}
	worker := &runtimeProcessWorker{
		command: command, control: control, secret: secret, holder: holder, lifecycle: lifecycle,
		receiver: NewRuntimeControlReceiver(secret), next: 1, transport: transport, privateDir: privateDir,
		waitDone: make(chan struct{}),
	}
	go func() {
		err := command.Wait()
		worker.waitMu.Lock()
		worker.waitErr = err
		worker.waitMu.Unlock()
		close(worker.waitDone)
	}()
	removePrivateDir = false
	return worker, nil
}

type runtimeProcessWorker struct {
	mu         sync.Mutex
	command    *exec.Cmd
	control    net.Conn
	secret     *RuntimeSecret
	receiver   *RuntimeControlReceiver
	holder     LifecycleHolderProof
	lifecycle  *os.File
	next       uint64
	transport  *http.Transport
	privateDir string
	waitMu     sync.Mutex
	waitDone   chan struct{}
	waitErr    error
	cleanup    sync.Once
	release    RuntimeWorkerReleaseV1
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
	var index NormalCallerIndexV1
	if json.Unmarshal(frame.Payload, &index) != nil {
		return RuntimeBootAckV1{}, ErrRuntimeControlFrame
	}
	key, err := normalCallerKeyFromRuntimeSecret(worker.secret)
	if err != nil || !VerifyNormalCallerIndexV1(key, index) {
		zeroRuntimeBytes(key)
		return RuntimeBootAckV1{}, errors.Join(ErrRuntimeControlFrame, err)
	}
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder, CallerIndex: index, CallerAuthorityKey: key}, nil
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
func (worker *runtimeProcessWorker) Exited() <-chan struct{}           { return worker.waitDone }
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

func (worker *runtimeProcessWorker) ServeHTTP(writer http.ResponseWriter, request *http.Request, caller RuntimeCallerAuthorityV1) error {
	if worker == nil || writer == nil || request == nil || worker.transport == nil {
		return ErrRuntimeWorkerUnavailable
	}
	var transportErr error
	reverse := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL.Scheme = "http"
			out.Out.URL.Host = "runtime-worker"
			out.Out.Host = out.In.Host
			out.Out.Header.Del("Authorization")
			out.Out.Header.Del("Proxy-Authorization")
			out.Out.Header.Del(runtimeCallerAuthorityHeader)
			if caller.SchemaVersion == 1 {
				encoded, _ := json.Marshal(caller)
				out.Out.Header.Set(runtimeCallerAuthorityHeader, hex.EncodeToString(encoded))
			}
		},
		Transport:     worker.transport,
		FlushInterval: -1,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, err error) {
			transportErr = err
			http.Error(response, "runtime worker unavailable", http.StatusServiceUnavailable)
		},
	}
	reverse.ServeHTTP(writer, request)
	return transportErr
}

func (worker *runtimeProcessWorker) CallerIndex(ctx context.Context) (NormalCallerIndexV1, error) {
	if worker == nil || ctx == nil || worker.transport == nil || worker.secret == nil {
		return NormalCallerIndexV1{}, ErrRuntimeWorkerUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime-worker"+runtimeCallerIndexPath, nil)
	if err != nil {
		return NormalCallerIndexV1{}, err
	}
	response, err := worker.transport.RoundTrip(request)
	if err != nil {
		return NormalCallerIndexV1{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, RuntimeControlFrameLimit+1))
	if err != nil || len(body) > RuntimeControlFrameLimit {
		return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
	}
	var index NormalCallerIndexV1
	key, keyErr := normalCallerKeyFromRuntimeSecret(worker.secret)
	if keyErr != nil || json.Unmarshal(body, &index) != nil || !VerifyNormalCallerIndexV1(key, index) {
		zeroRuntimeBytes(key)
		return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
	}
	zeroRuntimeBytes(key)
	return index, nil
}
func (worker *runtimeProcessWorker) StopAndReap(ctx context.Context) (RuntimeWorkerReleaseV1, error) {
	if worker == nil || ctx == nil {
		return RuntimeWorkerReleaseV1{}, ErrRuntimeWorkerUnavailable
	}
	select {
	case <-worker.waitDone:
	default:
		_, _ = worker.exchange(ctx, "shutdown", nil)
	}
	select {
	case <-worker.waitDone:
	case <-ctx.Done():
		_ = worker.command.Process.Kill()
		<-worker.waitDone
	}
	worker.cleanup.Do(func() {
		worker.receiver.Close()
		if worker.control != nil {
			_ = worker.control.Close()
		}
		_ = worker.lifecycle.Close()
		if worker.transport != nil {
			worker.transport.CloseIdleConnections()
		}
		if worker.privateDir != "" {
			_ = os.RemoveAll(worker.privateDir)
		}
		worker.secret.Destroy()
		identity := sha256.Sum256([]byte(worker.holder.DescriptionID))
		value := hex.EncodeToString(identity[:])
		worker.release = RuntimeWorkerReleaseV1{ProcessIdentityDigest: value, ProcessTreeAbsenceProofDigest: value, HolderReleaseProofDigest: value}
	})
	if err := ctx.Err(); err != nil {
		return worker.release, err
	}
	return worker.release, nil
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
