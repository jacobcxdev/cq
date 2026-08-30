//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const (
	proxyAgentLabel         = "dev.jacobcx.cq.proxy"
	homebrewProxyAgentLabel = "homebrew.mxcl.cq"
)

func init() {
	defaultProxyInspectionTarget = darwinProxyInspectionTarget
	proxyInspectionTargetForRoot = darwinProxyInspectionTargetForRoot
	adoptProxyListenerFn = adoptUnixProxyListener
	newProxyRuntimeWorkerLauncherFn = newUnixProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runUnixProxyAdoptedRuntime
	runProxyOwnedRuntimeFn = runUnixProxyOwnedRuntime
}

func runtimeDescriptorRoot() string { return "/dev/fd" }

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

func proxyAgentLogPath(logsDir string) string {
	return filepath.Join(logsDir, "proxy.log")
}

var runProxyLaunchctl = func(args ...string) error {
	return exec.Command("launchctl", args...).Run()
}

var currentExecutable = os.Executable

func installProxyAgent() error {
	if err := rejectHomebrewProxyServiceMutation("start"); err != nil {
		return err
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if _, err := proxy.LoadConfig(); err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve absolute executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	exe = filepath.Clean(exe)
	platform := newDarwinCommandServicePlatform(home, roots, exe)
	if err := platform.InstallProxy(context.Background(), exe); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cq: installed proxy LaunchAgent (KeepAlive)\n")
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", platform.plistPath(proxyAgentLabel))
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", proxyAgentLogPath(roots.Logs))
	return nil
}

func initialiseDarwinRuntimeLifecycle() error {
	return initialiseUnixRuntimeLifecycle()
}

func restartProxyAgent() error {
	uid := os.Getuid()
	if executable, executableErr := currentExecutable(); executableErr == nil && isHomebrewFormulaExecutable(executable) {
		if err := runProxyLaunchctl("kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, homebrewProxyAgentLabel)); err != nil {
			return fmt.Errorf("launchctl kickstart Homebrew service: %w", err)
		}
		return nil
	}
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
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	platform := newDarwinCommandServicePlatform(home, roots, "")
	if err := platform.RemoveProxy(context.Background()); err != nil {
		return err
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
