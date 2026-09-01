//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const linuxValidationWorkerHelperEnvironment = "CQ_LINUX_VALIDATION_WORKER_HELPER"

func TestLinuxValidationCandidateRoutesAuthenticatedNonPublicTrafficThroughWorker(t *testing.T) {
	temporaryRoot := t.TempDir()
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	localToken, err := proxy.NewCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(temporaryRoot, linuxValidationCredentialName)
	if err := writeLinuxValidationCandidateCredential(credentialPath, localToken); err != nil {
		t.Fatal(err)
	}
	backendPath := filepath.Join(temporaryRoot, "validation.sock")
	backendListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: backendPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	backendListener.SetUnlinkOnClose(true)
	if err := os.Chmod(backendPath, 0o600); err != nil {
		backendListener.Close()
		t.Fatal(err)
	}
	requestSeen := make(chan struct{}, 1)
	backendServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.EscapedPath() != "/responses" || request.Header.Get("Authorization") != "Bearer "+localToken {
			http.Error(writer, "unexpected validation request", http.StatusForbidden)
			return
		}
		requestSeen <- struct{}{}
		writer.WriteHeader(http.StatusNoContent)
	})}
	backendResult := make(chan error, 1)
	go func() {
		serveErr := backendServer.Serve(backendListener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		backendResult <- serveErr
	}()
	t.Cleanup(func() {
		if err := shutdownLinuxValidationHTTPServer(backendServer, backendResult); err != nil {
			t.Errorf("shutdown validation backend: %v", err)
		}
	})

	lifecyclePath := filepath.Join(temporaryRoot, "runtime.lifecycle")
	if err := initialiseUnixRuntimeLifecycleAt(lifecyclePath); err != nil {
		t.Fatal(err)
	}
	holderFile, holder, err := openUnixRuntimeLifecycle(lifecyclePath, "validation-test-supervisor")
	if err != nil {
		t.Fatal(err)
	}
	defer holderFile.Close()
	holderDigest, err := proxy.RuntimeDescriptorIdentityDigest(holderFile)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	artifactDigest := sha256.Sum256(artifact)
	manifest := proxy.RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: proxy.RuntimeRoleSupervisor, ManifestDigest: artifactDigest,
		ProxyInstanceID: linuxValidationCandidateProxyInstanceID, RuntimeInstanceID: strings.Repeat("a", 32),
		ListenerFD: proxy.RuntimeListenerFD, LifecycleFD: proxy.RuntimeLifecycleFD,
		ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD, WorkFD: proxy.RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := newUnixRuntimeLauncher(executable, manifest, holder, lifecyclePath)
	launcher.Command = func(commandCtx context.Context, path string, arguments ...string) *exec.Cmd {
		command := exec.CommandContext(commandCtx, path, append([]string{"-test.run=^TestLinuxValidationCandidateWorkerHelper$", "--"}, arguments...)...)
		command.Env = append(os.Environ(),
			linuxValidationWorkerHelperEnvironment+"=1",
			linuxValidationBackendEnvironment+"="+backendPath,
			linuxValidationCredentialEnvironment+"="+credentialPath,
		)
		return command
	}
	admissions, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, filepath.Join(temporaryRoot, "admissions"))
	if err != nil {
		t.Fatal(err)
	}
	defer admissions.Close()
	workerManifest := proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(artifactDigest[:])}
	err = proxy.RunAdoptedRuntimeSupervisor(context.Background(), runtimeSupervisorStartupListener{}, holder, launcher,
		&proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest,
		func(_ context.Context, _ net.Listener, supervisor http.Handler) error {
			request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
			request.Header.Set("Authorization", "Bearer "+localToken)
			response := httptest.NewRecorder()
			supervisor.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				return fmt.Errorf("validation response status %d: %s", response.Code, response.Body.String())
			}
			select {
			case <-requestSeen:
				return nil
			case <-time.After(2 * time.Second):
				return errors.New("validation backend did not receive request")
			}
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLinuxValidationCandidateWorkerHelper(t *testing.T) {
	if os.Getenv(linuxValidationWorkerHelperEnvironment) != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || len(os.Args) < separator+4 || os.Args[separator+1] != "proxy" || os.Args[separator+2] != "start" {
		t.Fatal("validation worker arguments are unavailable")
	}
	options, err := parseProxyCommandOptions(os.Args[separator+3:])
	if err != nil || options.RuntimeRole == nil || options.RuntimeRole.Role != proxy.RuntimeRoleWorker {
		t.Fatalf("parse validation worker role: %#v, %v", options.RuntimeRole, err)
	}
	if reserved := os.NewFile(uintptr(proxy.RuntimeListenerFD), "runtime-reserved"); reserved != nil {
		_ = reserved.Close()
	}
	files := proxy.RuntimeRoleFiles{
		Lifecycle: os.NewFile(uintptr(options.RuntimeRole.LifecycleFD), "runtime-lifecycle"),
		Control:   os.NewFile(uintptr(options.RuntimeRole.ControlFD), "runtime-control"),
		Secret:    os.NewFile(uintptr(options.RuntimeRole.SecretFD), "runtime-secret"),
		Work:      os.NewFile(uintptr(options.RuntimeRole.WorkFD), "runtime-work"),
	}
	handled, err := runLinuxProxyValidationCandidateWorker(context.Background(), *options.RuntimeRole, files)
	if !handled || err != nil {
		t.Fatalf("run validation worker: handled=%t error=%v", handled, err)
	}
}

func TestShutdownLinuxValidationHTTPServerBoundsServeWait(t *testing.T) {
	serveResult := make(chan error)
	timedOut := make(chan time.Time, 1)
	timedOut <- time.Now()
	err := shutdownLinuxValidationHTTPServerWithTimer(
		&http.Server{},
		serveResult,
		func(time.Duration) <-chan time.Time { return timedOut },
	)
	if err == nil || !strings.Contains(err.Error(), "installed validation server cleanup timed out") {
		t.Fatalf("shutdown error = %v", err)
	}
}

func TestLinuxValidationListenerTransitionRestoresSingleDescriptor(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	replacement, err := duplicateLinuxValidationTCPListener(listener)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := proxy.CaptureLinuxListener(os.Getpid(), port); err == nil {
		listener.Close()
		t.Fatal("duplicate listener descriptors accepted")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	identity, err := proxy.CaptureLinuxListener(os.Getpid(), port)
	if err != nil || identity.Process.PID != os.Getpid() {
		t.Fatalf("replacement listener identity = (%#v, %v)", identity, err)
	}
}
