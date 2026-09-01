//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

const (
	linuxValidationCandidateProxyInstanceID = "installed-http-validation"
	linuxValidationBackendEnvironment       = "CQ_LINUX_VALIDATION_BACKEND"
	linuxValidationCredentialEnvironment    = "CQ_LINUX_VALIDATION_CREDENTIAL"
	linuxValidationCredentialName           = "caller.token"
	linuxValidationCredentialMaxBytes       = 64
)

type linuxValidationReadyListener struct {
	net.Listener
	once  sync.Once
	ready chan struct{}
}

func (listener *linuxValidationReadyListener) Accept() (net.Conn, error) {
	listener.once.Do(func() { close(listener.ready) })
	return listener.Listener.Accept()
}

type linuxValidationHealthResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (response *linuxValidationHealthResponse) Header() http.Header { return response.header }
func (response *linuxValidationHealthResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}
func (response *linuxValidationHealthResponse) Write(data []byte) (int, error) {
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.body.Write(data)
}

type linuxValidationServingProofHandler struct {
	next     http.Handler
	attestor *proxy.ServingAttestor
}

type linuxValidationRuntimeAuthority struct {
	supervisor proxy.LinuxProcessIdentity
	worker     proxy.LinuxProcessIdentity
	listener   proxy.LinuxListenerIdentity
}

func (handler linuxValidationServingProofHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodGet || request.URL == nil || request.URL.EscapedPath() != "/health" ||
		request.Header.Get(proxy.ServingProofChallengeHeader) == "" {
		handler.next.ServeHTTP(writer, request)
		return
	}
	recorded := &linuxValidationHealthResponse{header: make(http.Header)}
	handler.next.ServeHTTP(recorded, request)
	for name, values := range recorded.header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	if proof, ok := handler.attestor.ProveHealth(request, recorded.body.Bytes()); ok {
		writer.Header().Set(proxy.ServingProofResponseHeader, proof)
	}
	status := recorded.status
	if status == 0 {
		status = http.StatusOK
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(recorded.body.Bytes())
}

func runLinuxProxyValidationCandidate(ctx context.Context, opts proxyCommandOptions, build string) (bool, error) {
	if ctx == nil || opts.LinuxValidationCandidateFD != 3 || opts.Port < 1 || opts.Port > 65_535 || opts.Port == proxy.DefaultPort || build == "" {
		return true, errors.New("Linux validation candidate requires an isolated port")
	}
	if err := activateProxyValidationCandidateFn(opts.LinuxValidationCandidateFD, opts.Port); err != nil {
		return true, err
	}
	return true, runLinuxInstalledHTTPValidationCandidateRuntime(ctx, opts.Port, build)
}

func runLinuxProxyValidationCandidateWorker(ctx context.Context, manifest proxy.RuntimeRoleManifestV1, files proxy.RuntimeRoleFiles) (bool, error) {
	if manifest.ProxyInstanceID != linuxValidationCandidateProxyInstanceID {
		return false, nil
	}
	backend := os.Getenv(linuxValidationBackendEnvironment)
	credentialPath := os.Getenv(linuxValidationCredentialEnvironment)
	if !filepath.IsAbs(backend) || filepath.Clean(backend) != backend || filepath.Base(backend) != "validation.sock" ||
		!filepath.IsAbs(credentialPath) || filepath.Clean(credentialPath) != credentialPath || filepath.Base(credentialPath) != linuxValidationCredentialName ||
		filepath.Dir(credentialPath) != filepath.Dir(backend) {
		return true, errors.New("installed validation backend is unavailable")
	}
	localToken, err := readLinuxValidationCandidateCredential(credentialPath)
	if err != nil {
		return true, err
	}
	transport := &http.Transport{DisableCompression: true, DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dialCtx, "unix", backend)
	}}
	defer transport.CloseIdleConnections()
	reverse := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL.Scheme = "http"
			out.Out.URL.Host = "validation-runtime"
			out.Out.Host = out.In.Host
		},
		Transport: transport, FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "validation candidate unavailable", http.StatusServiceUnavailable)
		},
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if linuxValidationBackendReady(backend) {
			reverse.ServeHTTP(writer, request)
			return
		}
		if request.Method == http.MethodGet && request.URL != nil && request.URL.EscapedPath() == "/health" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte("{\"status\":\"ok\",\"supervisor_alive\":true,\"data_plane_ready\":true}\n"))
			return
		}
		http.Error(writer, "validation candidate unavailable", http.StatusServiceUnavailable)
	})
	return true, proxy.RunRuntimeWorkerRoleWithHandlerAndCallerCredentials(ctx, manifest, files, handler, []proxy.NormalCallerCredentialV1{{
		Domain: proxy.NormalCallerCodex, Bearer: localToken, SubjectID: linuxValidationCandidateProxyInstanceID,
	}})
}

func linuxValidationBackendReady(path string) bool {
	var stat unix.Stat_t
	return filepath.IsAbs(path) && filepath.Clean(path) == path && unix.Lstat(path, &stat) == nil &&
		stat.Mode&unix.S_IFMT == unix.S_IFSOCK && stat.Mode&0o777 == 0o600 && stat.Uid == uint32(os.Geteuid())
}

func runLinuxInstalledHTTPValidationCandidateRuntime(ctx context.Context, port int, build string) (returnErr error) {
	temporaryRoot, err := os.MkdirTemp("/tmp", "cq-linux-validation-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(temporaryRoot)) }()
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		return err
	}
	localToken, err := proxy.NewCodexInstalledHTTPValidationToken()
	if err != nil {
		return err
	}
	credentialPath := filepath.Join(temporaryRoot, linuxValidationCredentialName)
	if err := writeLinuxValidationCandidateCredential(credentialPath, localToken); err != nil {
		return err
	}
	admissions, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, filepath.Join(temporaryRoot, "admissions"))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, admissions.Close()) }()
	lifecyclePath := filepath.Join(temporaryRoot, "runtime.lifecycle")
	if err := initialiseUnixRuntimeLifecycleAt(lifecyclePath); err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return err
	}
	defer listener.Close()
	holderFile, holder, err := openUnixRuntimeLifecycle(lifecyclePath, "validation-supervisor")
	if err != nil {
		return err
	}
	defer holderFile.Close()
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return errors.New("resolve installed validation candidate executable")
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	manifestDigest := sha256.Sum256(artifact)
	holderDigest, err := proxy.RuntimeDescriptorIdentityDigest(holderFile)
	if err != nil {
		return err
	}
	var runtimeID [16]byte
	if _, err := rand.Read(runtimeID[:]); err != nil {
		return err
	}
	manifest := proxy.RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: proxy.RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: linuxValidationCandidateProxyInstanceID, RuntimeInstanceID: hex.EncodeToString(runtimeID[:]),
		ListenerFD: proxy.RuntimeListenerFD, LifecycleFD: proxy.RuntimeLifecycleFD,
		ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD, WorkFD: proxy.RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := newUnixRuntimeLauncher(executable, manifest, holder, lifecyclePath)
	backendPath := filepath.Join(temporaryRoot, "validation.sock")
	launcher.Command = func(commandCtx context.Context, path string, arguments ...string) *exec.Cmd {
		command := exec.CommandContext(commandCtx, path, arguments...)
		environment := make([]string, 0, len(os.Environ())+2)
		backendPrefix := linuxValidationBackendEnvironment + "="
		credentialPrefix := linuxValidationCredentialEnvironment + "="
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, backendPrefix) && !strings.HasPrefix(value, credentialPrefix) {
				environment = append(environment, value)
			}
		}
		command.Env = append(environment, backendPrefix+backendPath, credentialPrefix+credentialPath)
		return command
	}
	workerManifest := proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])}
	candidateCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	restart := make(chan os.Signal, 1)
	signal.Notify(restart, syscall.SIGUSR1)
	defer signal.Stop(restart)
	return proxy.RunAdoptedRuntimeSupervisor(candidateCtx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest,
		func(serveCtx context.Context, owned net.Listener, handler http.Handler) error {
			return serveLinuxInstalledHTTPValidationCandidate(serveCtx, owned, handler, restart, backendPath, port, build, localToken)
		})
}

func serveLinuxInstalledHTTPValidationCandidate(
	ctx context.Context,
	listener net.Listener,
	handler http.Handler,
	restart <-chan os.Signal,
	backendPath string,
	port int,
	build string,
	localToken string,
) error {
	tcpListener, ok := listener.(*net.TCPListener)
	if ctx == nil || !ok || handler == nil || restart == nil || !filepath.IsAbs(backendPath) {
		return proxy.ErrRuntimeSupervisorUnavailable
	}
	normalCtx, cancelNormal := context.WithCancel(ctx)
	normalResult := make(chan error, 1)
	go func() {
		var serveErr error
		defer func() {
			if recover() != nil {
				serveErr = errors.New("validation candidate supervisor panicked")
			}
			normalResult <- serveErr
		}()
		serveErr = serveRuntimeSupervisor(normalCtx, tcpListener, handler)
	}()
	var installedListener *net.TCPListener
	var transitionErr error
	select {
	case err := <-normalResult:
		cancelNormal()
		return err
	case <-ctx.Done():
		cancelNormal()
		<-normalResult
		return ctx.Err()
	case <-restart:
		installedListener, transitionErr = duplicateLinuxValidationTCPListener(tcpListener)
	}
	cancelNormal()
	if normalErr := <-normalResult; normalErr != nil && !errors.Is(normalErr, context.Canceled) {
		return errors.Join(transitionErr, normalErr)
	}
	if transitionErr != nil {
		return transitionErr
	}
	defer installedListener.Close()
	intent, err := claimInstalledHTTPValidationStartupRequest(build, consumeInstalledHTTPValidationStartupRequestFn, invalidateInstalledHTTPValidationMarkerFn)
	if err != nil {
		return err
	}
	if intent == nil {
		return errors.New("installed validation startup request is unavailable")
	}
	cfg, err := loadProxyStartConfigFn()
	if err != nil {
		return err
	}
	cfg.Port = port
	return runProxyInstalledHTTPValidationStartupRuntime(ctx, cfg, build, intent, installedListener, handler, backendPath, localToken)
}

func duplicateLinuxValidationTCPListener(listener *net.TCPListener) (*net.TCPListener, error) {
	if listener == nil {
		return nil, errors.New("installed validation listener is unavailable")
	}
	file, err := listener.File()
	if err != nil {
		return nil, err
	}
	duplicate, duplicateErr := net.FileListener(file)
	closeErr := file.Close()
	if duplicateErr != nil || closeErr != nil {
		if duplicate != nil {
			_ = duplicate.Close()
		}
		return nil, errors.Join(duplicateErr, closeErr)
	}
	tcp, ok := duplicate.(*net.TCPListener)
	if !ok {
		_ = duplicate.Close()
		return nil, errors.New("installed validation listener is unavailable")
	}
	return tcp, nil
}

func runProxyInstalledHTTPValidationStartupRuntime(
	ctx context.Context,
	cfg *proxy.Config,
	build string,
	guard proxy.CodexInstalledHTTPValidationGuard,
	listener *net.TCPListener,
	supervisor http.Handler,
	backendPath string,
	localToken string,
) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errors.New("installed Codex client build is unavailable")
		}
	}()
	clientBuild := installedHTTPValidationClientBuildFn()
	if clientBuild == "" {
		return errors.New("installed Codex client build is unavailable")
	}
	if err := proxy.RunCodexInstalledHTTPValidationRuntime(ctx, cfg, build, clientBuild, guard, localToken, func(validationCtx context.Context, runtime proxy.CodexInstalledHTTPValidationRuntime) error {
		return serveLinuxInstalledHTTPValidationDataPlane(validationCtx, listener, supervisor, backendPath, runtime)
	}); err != nil {
		return fmt.Errorf("installed HTTP validation: %w", err)
	}
	return nil
}

func writeLinuxValidationCandidateCredential(path, token string) (returnErr error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != linuxValidationCredentialName || token == "" {
		return errors.New("installed validation credential is unavailable")
	}
	descriptor, err := unix.Open(path, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(descriptor), linuxValidationCredentialName)
	if file == nil {
		_ = unix.Close(descriptor)
		return errors.New("installed validation credential is unavailable")
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	data := []byte(token)
	defer clear(data)
	if len(data) > linuxValidationCredentialMaxBytes {
		return errors.New("installed validation credential is unavailable")
	}
	if count, err := file.Write(data); err != nil || count != len(data) {
		return errors.Join(err, errors.New("write installed validation credential"))
	}
	if err := file.Sync(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readLinuxValidationCandidateCredential(path string) (returnErr string, returnError error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != linuxValidationCredentialName {
		return "", errors.New("installed validation credential is unavailable")
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(descriptor), linuxValidationCredentialName)
	if file == nil {
		_ = unix.Close(descriptor)
		return "", errors.New("installed validation credential is unavailable")
	}
	defer func() { returnError = errors.Join(returnError, file.Close()) }()
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0o777 != 0o600 ||
		before.Nlink != 1 || before.Uid != uint32(os.Geteuid()) || before.Size < 1 || before.Size > linuxValidationCredentialMaxBytes {
		return "", errors.New("installed validation credential is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, linuxValidationCredentialMaxBytes+1))
	if err != nil || int64(len(data)) != before.Size {
		clear(data)
		return "", errors.New("installed validation credential changed")
	}
	defer clear(data)
	var after, pathStat unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil || err == nil && (after.Dev != before.Dev || after.Ino != before.Ino || after.Size != before.Size || after.Mtim != before.Mtim || after.Ctim != before.Ctim) {
		return "", errors.New("installed validation credential changed")
	}
	if err := unix.Lstat(path, &pathStat); err != nil || pathStat.Dev != before.Dev || pathStat.Ino != before.Ino || pathStat.Mode != before.Mode || pathStat.Nlink != before.Nlink || pathStat.Uid != before.Uid || pathStat.Size != before.Size {
		return "", errors.New("installed validation credential path changed")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(string(data))
	valid := err == nil && len(decoded) == 32
	clear(decoded)
	if !valid {
		return "", errors.New("installed validation credential is unavailable")
	}
	return string(data), nil
}

func serveLinuxInstalledHTTPValidationDataPlane(
	ctx context.Context,
	listener *net.TCPListener,
	supervisor http.Handler,
	backendPath string,
	runtime proxy.CodexInstalledHTTPValidationRuntime,
) (returnErr error) {
	if ctx == nil || listener == nil || supervisor == nil || runtime.Handler == nil || runtime.ServingAttestor == nil ||
		runtime.StartupValidation == nil || runtime.AbortUnexpected == nil {
		return errors.New("installed validation runtime is unavailable")
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address == nil {
		return errors.New("installed validation runtime address is unavailable")
	}
	port := address.Port
	authorityBefore, err := captureLinuxValidationRuntimeAuthority(port)
	if err != nil {
		return err
	}
	backendListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: backendPath, Net: "unix"})
	if err != nil {
		return err
	}
	backendListener.SetUnlinkOnClose(true)
	if err := os.Chmod(backendPath, 0o600); err != nil {
		_ = backendListener.Close()
		return err
	}
	backendServer := &http.Server{Handler: runtime.Handler, ReadHeaderTimeout: 10 * time.Second}
	backendResult := make(chan error, 1)
	go func() {
		serveErr := backendServer.Serve(backendListener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		backendResult <- serveErr
	}()
	defer func() {
		returnErr = errors.Join(returnErr, shutdownLinuxValidationHTTPServer(backendServer, backendResult))
	}()
	attested, err := runtime.ServingAttestor.ActivateListener(listener)
	if err != nil {
		return err
	}
	ready := &linuxValidationReadyListener{Listener: attested, ready: make(chan struct{})}
	publicServer := &http.Server{
		Handler:           linuxValidationServingProofHandler{next: supervisor, attestor: runtime.ServingAttestor},
		ReadHeaderTimeout: 10 * time.Second,
	}
	publicResult := make(chan error, 1)
	go func() {
		serveErr := publicServer.Serve(ready)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		publicResult <- serveErr
	}()
	defer func() {
		var closeDone <-chan struct{}
		if returnErr == nil {
			closeDone = runtime.ServingAttestor.BeginClose()
		} else {
			closeDone = runtime.AbortUnexpected()
		}
		returnErr = errors.Join(returnErr, shutdownLinuxValidationHTTPServer(publicServer, publicResult))
		<-closeDone
	}()
	select {
	case <-ready.ready:
	case err := <-publicResult:
		publicResult <- err
		return errors.Join(errors.New("installed validation listener unavailable"), err)
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := runtime.StartupValidation(ctx, proxy.CodexHTTPStartupValidationRuntime{
		ListenerAddress: listener.Addr().String(), ServingAttestor: runtime.ServingAttestor,
	}, func() error {
		authorityAfter, err := captureLinuxValidationRuntimeAuthority(port)
		if err != nil || !authorityAfter.equal(authorityBefore) {
			return errors.Join(err, errors.New("installed validation runtime changed after traffic"))
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func captureLinuxValidationRuntimeAuthority(port int) (linuxValidationRuntimeAuthority, error) {
	if port < 1 || port > 65_535 || port == proxy.DefaultPort {
		return linuxValidationRuntimeAuthority{}, errors.New("installed validation runtime port is unavailable")
	}
	supervisor, err := proxy.CaptureLinuxProcess(os.Getpid())
	if err != nil {
		return linuxValidationRuntimeAuthority{}, err
	}
	binding := installedHTTPValidationServiceBinding{executableSHA256: hex.EncodeToString(supervisor.Executable.SHA256[:])}
	if !validLinuxInstalledHTTPValidationCandidateProcess(supervisor, os.Getpid(), port, binding) {
		return linuxValidationRuntimeAuthority{}, errors.New("installed validation supervisor is unavailable")
	}
	worker, err := proxy.CaptureLinuxRuntimeWorker(context.Background(), supervisor)
	if err != nil || !validLinuxInstalledHTTPValidationCandidateWorker(worker, supervisor, binding) {
		return linuxValidationRuntimeAuthority{}, errors.Join(err, errors.New("installed validation worker is unavailable"))
	}
	listener, err := proxy.CaptureLinuxListener(supervisor.PID, port)
	if err != nil || !listener.Valid() || listener.Address != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) || !listener.Process.Equal(supervisor) {
		return linuxValidationRuntimeAuthority{}, errors.Join(err, errors.New("installed validation listener is unavailable"))
	}
	return linuxValidationRuntimeAuthority{supervisor: supervisor, worker: worker, listener: listener}, nil
}

func (authority linuxValidationRuntimeAuthority) equal(other linuxValidationRuntimeAuthority) bool {
	return authority.supervisor.Equal(other.supervisor) && authority.worker.Equal(other.worker) &&
		authority.listener.Valid() && other.listener.Valid() && authority.listener.Address == other.listener.Address &&
		authority.listener.Inode == other.listener.Inode && authority.listener.Process.Equal(other.listener.Process)
}

func shutdownLinuxValidationHTTPServer(server *http.Server, result <-chan error) error {
	return shutdownLinuxValidationHTTPServerWithTimer(server, result, time.After)
}

func shutdownLinuxValidationHTTPServerWithTimer(server *http.Server, result <-chan error, timer func(time.Duration) <-chan time.Time) error {
	if server == nil || result == nil {
		return errors.New("installed validation server is unavailable")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	select {
	case serveErr := <-result:
		return errors.Join(shutdownErr, serveErr)
	case <-timer(5 * time.Second):
		return errors.Join(shutdownErr, errors.New("installed validation server cleanup timed out"))
	}
}
