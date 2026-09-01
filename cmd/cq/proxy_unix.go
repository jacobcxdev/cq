//go:build darwin || linux

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

func runUnixProxyOwnedRuntime(ctx context.Context, port int, serve func(context.Context, net.Listener, http.Handler) error) (handled bool, returnErr error) {
	if err := initialiseUnixRuntimeLifecycle(); err != nil {
		return true, fmt.Errorf("initialise runtime lifecycle: %w", err)
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return true, fmt.Errorf("listen for owned runtime: %w", err)
	}
	defer listener.Close()
	return true, wrapUnixProxyRuntimeError("serve owned runtime", runUnixProxyAdoptedRuntime(ctx, listener, serve))
}

func runUnixProxyAdoptedRuntime(ctx context.Context, listener net.Listener, serve func(context.Context, net.Listener, http.Handler) error) error {
	path, err := proxy.DefaultRuntimeLifecyclePath()
	if err != nil {
		return err
	}
	file, holder, err := openUnixRuntimeLifecycle(path, "supervisor")
	if err != nil {
		return fmt.Errorf("open supervisor runtime lifecycle: %w", err)
	}
	defer file.Close()
	holderDigest, err := proxy.RuntimeDescriptorIdentityDigest(file)
	if err != nil {
		return fmt.Errorf("digest supervisor runtime lifecycle: %w", err)
	}
	executable, err := currentUnixRuntimeExecutable()
	if err != nil {
		return fmt.Errorf("resolve runtime executable: %w", err)
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		return fmt.Errorf("read runtime executable: %w", err)
	}
	manifestDigest := sha256.Sum256(artifact)
	manifest := proxy.RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: proxy.RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: "primary", RuntimeInstanceID: hex.EncodeToString(manifestDigest[:16]),
		ListenerFD: proxy.RuntimeListenerFD, LifecycleFD: proxy.RuntimeLifecycleFD,
		ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD, WorkFD: proxy.RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := newUnixRuntimeLauncher(executable, manifest, holder, path)
	admissionPath, err := proxy.DefaultNormalCallerAdmissionPath()
	if err != nil {
		return err
	}
	admissions, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, admissionPath)
	if err != nil {
		return fmt.Errorf("open normal caller admissions: %w", err)
	}
	defer admissions.Close()
	workerManifest := proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])}
	bootstrap, err := proxy.LoadProxyRescueBootstrapConfig()
	if errors.Is(err, os.ErrNotExist) {
		return wrapUnixProxyRuntimeError("run normal supervisor", proxy.RunAdoptedRuntimeSupervisor(ctx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest, serve))
	}
	if err != nil {
		return fmt.Errorf("load rescue bootstrap: %w", err)
	}
	state, err := proxy.OpenProxyRescueState(ctx, proxy.ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: bootstrap.StateRoot, Random: rand.Reader, Now: time.Now,
	})
	if err != nil {
		return fmt.Errorf("open rescue state: %w", err)
	}
	defer state.Close()
	callerKey := make([]byte, sha256.Size)
	if _, err := rand.Read(callerKey); err != nil {
		return fmt.Errorf("create caller authority key: %w", err)
	}
	callerAuthority, err := proxy.NewNormalCallerAuthority(callerKey, 1, []proxy.NormalCallerCredentialV1{{
		Domain: proxy.NormalCallerLocal, Bearer: bootstrap.LocalToken, SubjectID: "local-control",
	}}, admissions, time.Now, rand.Reader)
	for index := range callerKey {
		callerKey[index] = 0
	}
	if err != nil {
		return fmt.Errorf("create caller authority: %w", err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		return fmt.Errorf("parse rescue upstream: %w", err)
	}
	return wrapUnixProxyRuntimeError("run configured supervisor", proxy.RunAdoptedRuntimeSupervisorConfigured(ctx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest, func(supervisor *proxy.RuntimeSupervisor) error {
		if err := supervisor.SetCallerAuthority(callerAuthority); err != nil {
			return err
		}
		relay := &proxy.RescueRelay{Transport: http.DefaultTransport, Origin: origin}
		return supervisor.ConfigureRescue(ctx, relay, state.RuntimeMode)
	}, serve))
}

func wrapUnixProxyRuntimeError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func openUnixRuntimeLifecycle(path, role string) (*os.File, proxy.LifecycleHolderProof, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, proxy.LifecycleHolderProof{}, err
	}
	file := os.NewFile(uintptr(fd), "runtime-"+role+"-lifecycle")
	if file == nil {
		_ = unix.Close(fd)
		return nil, proxy.LifecycleHolderProof{}, proxy.ErrRuntimeRoleManifest
	}
	if err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, proxy.LifecycleHolderProof{}, err
	}
	description := make([]byte, 16)
	if _, err := rand.Read(description); err != nil {
		_ = file.Close()
		return nil, proxy.LifecycleHolderProof{}, err
	}
	holder, err := proxy.RuntimeLifecycleHolder(file, hex.EncodeToString(description))
	if err != nil {
		_ = file.Close()
		return nil, proxy.LifecycleHolderProof{}, err
	}
	return file, holder, nil
}

func newUnixRuntimeLauncher(executable string, manifest proxy.RuntimeRoleManifestV1, supervisorHolder proxy.LifecycleHolderProof, path string) *proxy.RuntimeProcessWorkerLauncher {
	return &proxy.RuntimeProcessWorkerLauncher{
		Executable: executable, BaseManifest: manifest, SupervisorHolder: supervisorHolder,
		OpenLifecycle: func() (*os.File, proxy.LifecycleHolderProof, error) {
			return openUnixRuntimeLifecycle(path, "worker")
		},
	}
}

func newUnixProxyRuntimeWorkerLauncher(manifest proxy.RuntimeRoleManifestV1, supervisorHolder proxy.LifecycleHolderProof) (proxy.RuntimeWorkerLauncher, error) {
	executable, err := currentUnixRuntimeExecutable()
	if err != nil {
		return nil, err
	}
	lifecyclePath, err := filepath.EvalSymlinks(runtimeDescriptorPath(manifest.LifecycleFD))
	if err != nil {
		return nil, fmt.Errorf("resolve runtime lifecycle descriptor: %w", err)
	}
	if !filepath.IsAbs(lifecyclePath) {
		return nil, fmt.Errorf("resolve runtime lifecycle descriptor: non-absolute path")
	}
	return newUnixRuntimeLauncher(executable, manifest, supervisorHolder, lifecyclePath), nil
}

func currentUnixRuntimeExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolveUnixRuntimeExecutable(executable)
}

func resolveUnixRuntimeExecutable(executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("non-absolute path")
	}
	return filepath.Clean(resolved), nil
}

func runtimeDescriptorPath(fd int) string {
	return filepath.Join(runtimeDescriptorRoot(), strconv.Itoa(fd))
}

func adoptUnixProxyListener() (net.Listener, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(proxy.RuntimeListenerFD, &stat); err != nil {
		if errors.Is(err, unix.EBADF) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect runtime proxy listener: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, nil
	}
	file := os.NewFile(uintptr(proxy.RuntimeListenerFD), "runtime-PublicListener")
	if file == nil {
		return nil, fmt.Errorf("activate runtime proxy listener: invalid descriptor")
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return nil, errors.Join(fmt.Errorf("adopt runtime proxy listener: %w", err), closeErr)
	}
	tcpListener, ok := listener.(*net.TCPListener)
	address, addressOK := listener.Addr().(*net.TCPAddr)
	if !ok || !addressOK || address.IP == nil || !address.IP.IsLoopback() || address.IP.To4() == nil {
		_ = listener.Close()
		return nil, fmt.Errorf("adopt runtime proxy listener: expected IPv4 loopback TCP")
	}
	return tcpListener, nil
}

func initialiseUnixRuntimeLifecycle() error {
	path, err := proxy.DefaultRuntimeLifecyclePath()
	if err != nil {
		return err
	}
	return initialiseUnixRuntimeLifecycleAt(path)
}

func initialiseUnixRuntimeLifecycleAt(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return proxy.ErrRuntimeRoleManifest
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "runtime-lifecycle-initialise")
	if file == nil {
		_ = unix.Close(fd)
		return proxy.ErrRuntimeRoleManifest
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return proxy.ErrRuntimeRoleManifest
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		return proxy.ErrRuntimeRoleManifest
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
