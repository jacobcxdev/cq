//go:build darwin

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
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
	proxyInspectionTargetForRoot = darwinProxyInspectionTargetForRoot
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
		ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD, WorkFD: proxy.RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	launcher := newDarwinRuntimeLauncher(executable, manifest, holder, path)
	admissions, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, proxy.DefaultNormalCallerAdmissionPath())
	if err != nil {
		return err
	}
	defer admissions.Close()
	workerManifest := proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(manifestDigest[:])}
	bootstrap, err := proxy.LoadProxyRescueBootstrapConfig()
	if errors.Is(err, os.ErrNotExist) {
		return proxy.RunAdoptedRuntimeSupervisor(ctx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest, serve)
	}
	if err != nil {
		return err
	}
	state, err := proxy.OpenProxyRescueState(ctx, proxy.ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: bootstrap.StateRoot, Random: rand.Reader, Now: time.Now,
	})
	if err != nil {
		return err
	}
	defer state.Close()
	callerKey := make([]byte, sha256.Size)
	if _, err := rand.Read(callerKey); err != nil {
		return err
	}
	callerAuthority, err := proxy.NewNormalCallerAuthority(callerKey, 1, []proxy.NormalCallerCredentialV1{{
		Domain: proxy.NormalCallerLocal, Bearer: bootstrap.LocalToken, SubjectID: "local-control",
	}}, admissions, time.Now, rand.Reader)
	for index := range callerKey {
		callerKey[index] = 0
	}
	if err != nil {
		return err
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		return err
	}
	transport := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("rescue redirect refused")
		},
	}
	return proxy.RunAdoptedRuntimeSupervisorConfigured(ctx, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, admissions, workerManifest, func(supervisor *proxy.RuntimeSupervisor) error {
		if err := supervisor.SetCallerAuthority(callerAuthority); err != nil {
			return err
		}
		relay := &proxy.RescueRelay{
			Transport: transport, DialWS: websocket.DefaultDialer, Origin: origin,
			LoopbackHost: listener.Addr().String(), ForwardingAcknowledged: true,
			DenyBearer: supervisor.DeniesNormalBearer, Budget: proxy.NewRescueBudget(time.Now, state.RescueFairnessKey()),
			Admission: func(*http.Request) proxy.RescueIngressKind { return proxy.RescueIngressUnverified },
		}
		return supervisor.ConfigureRescue(ctx, relay, state.RuntimeMode)
	}, serve)
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
	fdPath := fmt.Sprintf("/dev/fd/%d", manifest.LifecycleFD)
	lifecyclePath, err = filepath.EvalSymlinks(fdPath)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime lifecycle descriptor: %w", err)
	}
	if !filepath.IsAbs(lifecyclePath) {
		return nil, fmt.Errorf("resolve runtime lifecycle descriptor: non-absolute path")
	}
	return newDarwinRuntimeLauncher(executable, manifest, supervisorHolder, lifecyclePath), nil
}

func adoptDarwinProxyListener() (net.Listener, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(proxy.RuntimeListenerFD, &stat); err != nil {
		if errors.Is(err, unix.EBADF) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect launchd proxy listener: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, nil
	}
	file := os.NewFile(uintptr(proxy.RuntimeListenerFD), "launchd-PublicListener")
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

type darwinProxyInspectionFacts struct {
	inspector proxy.Fact[proxy.InspectorIdentity]
	desired   proxy.Fact[proxy.DesiredProxyState]
	service   proxy.Fact[proxy.ServiceState]
	listener  proxy.Fact[proxy.ListenerState]
	process   proxy.Fact[proxy.ProcessState]
	runtime   proxy.Fact[proxy.RuntimeIdentity]
	dataPlane proxy.Fact[proxy.DataPlaneProof]
}

func darwinProxyInspectionTarget() ProxyInspectionTarget {
	return darwinProxyInspectionTargetForRoot("")
}

func darwinProxyInspectionTargetForRoot(instanceRoot string) ProxyInspectionTarget {
	var once sync.Once
	var facts darwinProxyInspectionFacts
	collect := func(ctx context.Context) {
		once.Do(func() { facts = collectDarwinProxyInspectionFacts(ctx, instanceRoot) })
	}
	return ProxyInspectionTarget{
		Inspector: func(ctx context.Context) proxy.Fact[proxy.InspectorIdentity] { collect(ctx); return facts.inspector },
		Desired:   func(ctx context.Context) proxy.Fact[proxy.DesiredProxyState] { collect(ctx); return facts.desired },
		Service:   func(ctx context.Context) proxy.Fact[proxy.ServiceState] { collect(ctx); return facts.service },
		Listener:  func(ctx context.Context) proxy.Fact[proxy.ListenerState] { collect(ctx); return facts.listener },
		Process:   func(ctx context.Context) proxy.Fact[proxy.ProcessState] { collect(ctx); return facts.process },
		Runtime:   func(ctx context.Context) proxy.Fact[proxy.RuntimeIdentity] { collect(ctx); return facts.runtime },
		DataPlane: func(ctx context.Context) proxy.Fact[proxy.DataPlaneProof] { collect(ctx); return facts.dataPlane },
	}
}

func collectDarwinProxyInspectionFacts(ctx context.Context, instanceRoot string) darwinProxyInspectionFacts {
	facts := darwinProxyInspectionFacts{
		inspector: proxy.UnavailableFact[proxy.InspectorIdentity]("inspector_unavailable"),
		desired:   proxy.UnavailableFact[proxy.DesiredProxyState]("config_unavailable"),
		service:   proxy.UnavailableFact[proxy.ServiceState]("service_unavailable"),
		listener:  proxy.UnavailableFact[proxy.ListenerState]("listener_unavailable"),
		process:   proxy.UnavailableFact[proxy.ProcessState]("process_unavailable"),
		runtime:   proxy.UnavailableFact[proxy.RuntimeIdentity]("runtime_unavailable"),
		dataPlane: proxy.UnavailableFact[proxy.DataPlaneProof]("data_plane_unavailable"),
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil || ctx.Err() != nil {
		return facts
	}
	facts.inspector = proxy.KnownFact(proxy.InspectorIdentity{Executable: executable, Version: version})
	bootstrap, err := proxy.LoadProxyRescueBootstrapConfig()
	port := 0
	if err == nil {
		if instanceRoot != "" && bootstrap.StateRoot != instanceRoot {
			return facts
		}
		port = bootstrap.Port
	} else {
		if instanceRoot != "" {
			return facts
		}
		cfg, configErr := proxy.LoadExistingConfig()
		if configErr != nil {
			return facts
		}
		port = cfg.Port
	}
	var binding installedHTTPValidationServiceBinding
	for _, label := range []string{proxyAgentLabel, homebrewProxyAgentLabel} {
		candidate, candidateErr := resolveInstalledHTTPValidationService(label)
		if candidateErr == nil {
			if binding.label != "" {
				facts.service = proxy.InvalidFact[proxy.ServiceState]("service_ambiguous")
				return facts
			}
			binding = candidate
		}
	}
	if binding.label == "" || ctx.Err() != nil {
		facts.desired = proxy.KnownFact(proxy.DesiredProxyState{Configured: true, Listener: fmt.Sprintf("127.0.0.1:%d", port)})
		return facts
	}
	manager := "launchagent"
	if binding.label == homebrewProxyAgentLabel {
		manager = "homebrew"
	}
	listenerPort := darwinProxyInspectionListenerPort(port, binding.port)
	listenerAddress := fmt.Sprintf("127.0.0.1:%d", listenerPort)
	facts.desired = proxy.KnownFact(proxy.DesiredProxyState{Manager: manager, Configured: true, Listener: listenerAddress})
	target, err := installedHTTPValidationLaunchctlTarget(binding.label, os.Geteuid)
	if err != nil {
		return facts
	}
	launchctlOutput, err := exec.CommandContext(ctx, "launchctl", "print", target).Output()
	if err != nil {
		facts.service = proxy.KnownFact(proxy.ServiceState{Manager: manager, State: "stopped"})
		facts.listener = proxy.AbsentFact[proxy.ListenerState]()
		facts.process = proxy.AbsentFact[proxy.ProcessState]()
		facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{Health: "unhealthy"})
		facts.dataPlane = proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"})
		return facts
	}
	pid, err := parseInstalledHTTPValidationLaunchctlPID(launchctlOutput, target)
	if err != nil {
		facts.service = proxy.InvalidFact[proxy.ServiceState]("service_invalid")
		return facts
	}
	lsofOutput, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", fmt.Sprintf("-iTCP:%d", listenerPort), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil || requireInstalledHTTPValidationListenerPID(lsofOutput, pid) != nil {
		facts.service = proxy.KnownFact(proxy.ServiceState{Manager: manager, State: "running", PID: pid, Executable: executable})
		facts.listener = proxy.InvalidFact[proxy.ListenerState]("listener_mismatch")
		return facts
	}
	facts.service = proxy.KnownFact(proxy.ServiceState{Manager: manager, State: "running", PID: pid, Executable: executable})
	facts.listener = proxy.KnownFact(proxy.ListenerState{State: "listening", Listener: listenerAddress, PID: pid, Executable: executable})
	facts.process = proxy.KnownFact(proxy.ProcessState{PID: pid, Executable: executable})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listenerAddress+"/health", http.NoBody)
	if err != nil {
		return facts
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("proxy status redirect refused") }}
	response, err := client.Do(request)
	if err != nil {
		facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{Health: "unhealthy"})
		facts.dataPlane = proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"})
		return facts
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: pid, Executable: executable, Health: "healthy"})
	} else {
		facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: pid, Executable: executable, Health: "unhealthy"})
	}
	facts.dataPlane = proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"})
	return facts
}

func darwinProxyInspectionListenerPort(configured, service int) int {
	if service > 0 {
		return service
	}
	return configured
}

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

var currentExecutable = os.Executable

func installProxyAgent() error {
	if err := rejectHomebrewProxyServiceMutation("start"); err != nil {
		return err
	}
	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
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
		Port:    cfg.Port,
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
	if err := rejectHomebrewProxyServiceMutation("stop"); err != nil {
		return err
	}
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

func rejectHomebrewProxyServiceMutation(action string) error {
	executable, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if !isHomebrewFormulaExecutable(executable) {
		return nil
	}
	return fmt.Errorf("cq is managed by Homebrew; use brew services %s cq", action)
}

func isHomebrewFormulaExecutable(executable string) bool {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	parts := strings.Split(filepath.Clean(executable), string(os.PathSeparator))
	for index := 0; index+4 < len(parts); index++ {
		if parts[index] == "Cellar" && parts[index+1] == "cq" && parts[index+2] != "" && parts[index+3] == "bin" && parts[index+4] == "cq" {
			return true
		}
	}
	return false
}
