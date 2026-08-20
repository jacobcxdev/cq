//go:build darwin

package main

/*
#include <launch.h>
#include <stdlib.h>
#include <fcntl.h>
#include <sys/param.h>
static int cq_runtime_fd_path(int fd, char *path) { return fcntl(fd, F_GETPATH, path); }
*/
import "C"

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

const (
	proxyAgentLabel         = "dev.jacobcx.cq.proxy"
	homebrewProxyAgentLabel = "homebrew.mxcl.cq"
)

var proxyPlistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{ .Label }}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{ .Binary }}</string>
		<string>proxy</string>
		<string>start</string>
	</array>
	<key>Sockets</key>
	<dict>
		<key>PublicListener</key>
		<dict>
			<key>SockFamily</key>
			<string>IPv4</string>
			<key>SockNodeName</key>
			<string>127.0.0.1</string>
			<key>SockServiceName</key>
			<string>{{ .Port }}</string>
			<key>SockType</key>
			<string>stream</string>
		</dict>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardErrorPath</key>
	<string>{{ .LogPath }}</string>
</dict>
</plist>
`))

type proxyPlistData struct {
	Label   string
	Binary  string
	LogPath string
	Port    int
}

func init() {
	defaultProxyInspectionTarget = darwinProxyInspectionTarget
	adoptProxyListenerFn = adoptDarwinProxyListener
	newProxyRuntimeWorkerLauncherFn = newDarwinProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runDarwinProxyAdoptedRuntime
}

func runDarwinProxyAdoptedRuntime(ctx context.Context, listener net.Listener, serve func(context.Context, net.Listener, http.Handler) error) error {
	path := proxy.DefaultRuntimeLifecyclePath()
	file, holder, err := openDarwinRuntimeLifecycle(path, "supervisor")
	if err != nil {
		return err
	}
	defer file.Close()
	holderDigest, err := proxy.RuntimeDescriptorIdentityDigest(file)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	manifestDigest := sha256.Sum256(artifact)
	manifest := proxy.RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: proxy.RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: "primary", RuntimeInstanceID: hex.EncodeToString(manifestDigest[:16]),
		ListenerFD: proxy.RuntimeListenerFD, LifecycleFD: proxy.RuntimeLifecycleFD,
		ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := newDarwinRuntimeLauncher(executable, manifest, holder, path)
	admissions, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, proxy.DefaultNormalCallerAdmissionPath())
	if err != nil {
		return err
	}
	defer admissions.Close()
	return proxy.RunAdoptedRuntimeSupervisor(ctx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])}, serve)
}

func openDarwinRuntimeLifecycle(path, role string) (*os.File, proxy.LifecycleHolderProof, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, proxy.LifecycleHolderProof{}, err
	}
	file := os.NewFile(uintptr(fd), "runtime-"+role+"-lifecycle")
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

func newDarwinRuntimeLauncher(executable string, manifest proxy.RuntimeRoleManifestV1, supervisorHolder proxy.LifecycleHolderProof, path string) *proxy.RuntimeProcessWorkerLauncher {
	return &proxy.RuntimeProcessWorkerLauncher{
		Executable: executable, BaseManifest: manifest, SupervisorHolder: supervisorHolder,
		OpenLifecycle: func() (*os.File, proxy.LifecycleHolderProof, error) {
			return openDarwinRuntimeLifecycle(path, "worker")
		},
	}
}

func newDarwinProxyRuntimeWorkerLauncher(manifest proxy.RuntimeRoleManifestV1, supervisorHolder proxy.LifecycleHolderProof) (proxy.RuntimeWorkerLauncher, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var lifecyclePath string
	{
		var path [C.MAXPATHLEN]C.char
		if C.cq_runtime_fd_path(C.int(manifest.LifecycleFD), &path[0]) != 0 {
			return nil, fmt.Errorf("resolve runtime lifecycle descriptor")
		}
		lifecyclePath = C.GoString(&path[0])
	}
	return newDarwinRuntimeLauncher(executable, manifest, supervisorHolder, lifecyclePath), nil
}

func adoptDarwinProxyListener() (net.Listener, error) {
	name := C.CString("PublicListener")
	defer C.free(unsafe.Pointer(name))
	var descriptors *C.int
	var count C.size_t
	result := syscall.Errno(C.launch_activate_socket(name, &descriptors, &count))
	if result != 0 {
		if errors.Is(result, syscall.ENOENT) || errors.Is(result, syscall.ESRCH) {
			return nil, nil
		}
		return nil, fmt.Errorf("activate launchd proxy listener: %w", result)
	}
	if descriptors == nil || count != 1 {
		if descriptors != nil {
			C.free(unsafe.Pointer(descriptors))
		}
		return nil, fmt.Errorf("activate launchd proxy listener: expected one descriptor")
	}
	descriptor := *descriptors
	C.free(unsafe.Pointer(descriptors))
	file := os.NewFile(uintptr(descriptor), "launchd-PublicListener")
	if file == nil {
		return nil, fmt.Errorf("activate launchd proxy listener: invalid descriptor")
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return nil, errors.Join(fmt.Errorf("adopt launchd proxy listener: %w", err), closeErr)
	}
	tcpListener, ok := listener.(*net.TCPListener)
	address, addressOK := listener.Addr().(*net.TCPAddr)
	if !ok || !addressOK || address.IP == nil || !address.IP.IsLoopback() || address.IP.To4() == nil {
		_ = listener.Close()
		return nil, fmt.Errorf("adopt launchd proxy listener: expected IPv4 loopback TCP")
	}
	return tcpListener, nil
}

// darwinProxyInspectionTarget is the CU-1 read-only platform boundary. It
// deliberately exposes no live collectors until the CU that owns effects.
func darwinProxyInspectionTarget() ProxyInspectionTarget { return ProxyInspectionTarget{} }

func proxyAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", proxyAgentLabel+".plist"), nil
}

func proxyAgentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "cq", "proxy.log"), nil
}

var runProxyLaunchctl = func(args ...string) error {
	return exec.Command("launchctl", args...).Run()
}

func installProxyAgent() error {
	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		return err
	}
	logPath, err := proxyAgentLogPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := initialiseDarwinRuntimeLifecycle(); err != nil {
		return fmt.Errorf("initialise runtime lifecycle: %w", err)
	}

	_ = runProxyLaunchctl("unload", plistPath)

	f, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	data := proxyPlistData{
		Label:   proxyAgentLabel,
		Binary:  exe,
		LogPath: logPath,
		Port:    proxy.DefaultPort,
	}
	if err := proxyPlistTemplate.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("write plist: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close plist: %w", err)
	}

	if err := runProxyLaunchctl("load", plistPath); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: installed proxy LaunchAgent (KeepAlive)\n")
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", plistPath)
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", logPath)
	return nil
}

func initialiseDarwinRuntimeLifecycle() error {
	path := proxy.DefaultRuntimeLifecyclePath()
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

func restartProxyAgent() error {
	uid := os.Getuid()
	err := runProxyLaunchctl("kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, proxyAgentLabel))
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(interface{ ExitCode() int }); !ok || exitErr.ExitCode() != 113 {
		return fmt.Errorf("launchctl kickstart: %w", err)
	}
	if err := runProxyLaunchctl("kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, homebrewProxyAgentLabel)); err != nil {
		return fmt.Errorf("launchctl kickstart Homebrew service: %w", err)
	}
	return nil
}

func uninstallProxyAgent() error {
	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "cq: no proxy LaunchAgent installed\n")
		return nil
	}

	if err := runProxyLaunchctl("unload", plistPath); err != nil {
		fmt.Fprintf(os.Stderr, "cq: launchctl unload: %v\n", err)
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: uninstalled proxy LaunchAgent\n")
	return nil
}
