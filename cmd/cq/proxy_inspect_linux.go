//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type linuxProxyInspectionDependencies struct {
	executable     func() (string, error)
	resolvePath    func(string) (string, error)
	loadConfig     func() (*proxy.Config, error)
	loadBootstrap  func() (*proxy.ProxyRescueBootstrapConfig, error)
	inspectService func(context.Context) (serviceStatus, error)
	inspectRuntime func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error)
	health         func(context.Context, string) bool
}

type linuxProxyInspectionFacts struct {
	inspector proxy.Fact[proxy.InspectorIdentity]
	desired   proxy.Fact[proxy.DesiredProxyState]
	service   proxy.Fact[proxy.ServiceState]
	listener  proxy.Fact[proxy.ListenerState]
	process   proxy.Fact[proxy.ProcessState]
	runtime   proxy.Fact[proxy.RuntimeIdentity]
	dataPlane proxy.Fact[proxy.DataPlaneProof]
}

func linuxProxyInspectionTarget() ProxyInspectionTarget {
	return linuxProxyInspectionTargetForRoot("")
}

func linuxProxyInspectionTargetForRoot(instanceRoot string) ProxyInspectionTarget {
	return linuxProxyInspectionTargetWithDependencies(instanceRoot, linuxProxyInspectionDependencies{})
}

func linuxProxyInspectionTargetWithDependencies(instanceRoot string, dependencies linuxProxyInspectionDependencies) ProxyInspectionTarget {
	dependencies = defaultLinuxProxyInspectionDependencies(dependencies)
	var once sync.Once
	var facts linuxProxyInspectionFacts
	collect := func(ctx context.Context) {
		once.Do(func() { facts = collectLinuxProxyInspectionFacts(ctx, instanceRoot, dependencies) })
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

func defaultLinuxProxyInspectionDependencies(dependencies linuxProxyInspectionDependencies) linuxProxyInspectionDependencies {
	if dependencies.executable == nil {
		dependencies.executable = os.Executable
	}
	if dependencies.loadConfig == nil {
		dependencies.loadConfig = proxy.LoadExistingConfig
	}
	if dependencies.resolvePath == nil {
		dependencies.resolvePath = filepath.EvalSymlinks
	}
	if dependencies.loadBootstrap == nil {
		dependencies.loadBootstrap = proxy.LoadProxyRescueBootstrapConfig
	}
	if dependencies.inspectService == nil {
		dependencies.inspectService = func(ctx context.Context) (serviceStatus, error) {
			platform, _, _, err := defaultLinuxSystemdPlatform()
			if err != nil {
				return serviceStatus{}, err
			}
			return platform.Inspect(ctx)
		}
	}
	if dependencies.inspectRuntime == nil {
		dependencies.inspectRuntime = proxy.InspectLinuxProxyRuntime
	}
	if dependencies.health == nil {
		dependencies.health = probeLinuxProxyHealth
	}
	return dependencies
}

func collectLinuxProxyInspectionFacts(ctx context.Context, instanceRoot string, dependencies linuxProxyInspectionDependencies) linuxProxyInspectionFacts {
	facts := unavailableLinuxProxyInspectionFacts()
	if ctx == nil || ctx.Err() != nil {
		return facts
	}
	dependencies = defaultLinuxProxyInspectionDependencies(dependencies)
	executable, err := dependencies.executable()
	if err == nil {
		executable, err = dependencies.resolvePath(executable)
	}
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return facts
	}
	facts.inspector = proxy.KnownFact(proxy.InspectorIdentity{Executable: executable, Version: version})

	port, configured := linuxProxyInspectionPort(instanceRoot, dependencies)
	if !configured {
		return facts
	}
	listenerAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	facts.desired = proxy.KnownFact(proxy.DesiredProxyState{Manager: "systemd-user", Configured: true, Listener: listenerAddress})

	manager, managerErr := dependencies.inspectService(ctx)
	configuredExecutable := executable
	if managerErr == nil && manager.Proxy.ConfiguredExecutable != "" {
		configuredExecutable = manager.Proxy.ConfiguredExecutable
	}
	runtimeIdentity, runtimeErr := dependencies.inspectRuntime(ctx, configuredExecutable, port)
	if runtimeErr == nil && runtimeIdentity.Valid() {
		facts.listener = proxy.KnownFact(proxy.ListenerState{
			State: "listening", Listener: runtimeIdentity.Listener.Address,
			PID: runtimeIdentity.Process.PID, Executable: runtimeIdentity.Process.Executable.Path,
		})
		facts.process = proxy.KnownFact(proxy.ProcessState{PID: runtimeIdentity.Process.PID, Executable: runtimeIdentity.Process.Executable.Path})
		if dependencies.health(ctx, runtimeIdentity.Listener.Address) {
			facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{
				Reachable: true, PID: runtimeIdentity.Process.PID,
				Executable: runtimeIdentity.Process.Executable.Path, Health: "healthy",
			})
		} else {
			facts.runtime = proxy.KnownFact(proxy.RuntimeIdentity{Health: "unhealthy"})
		}
		facts.dataPlane = proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"})
	}

	if managerErr != nil {
		return facts
	}
	component := manager.Proxy
	if !component.Registered {
		facts.service = proxy.AbsentFact[proxy.ServiceState]()
		return facts
	}
	if !component.Running {
		facts.service = proxy.KnownFact(proxy.ServiceState{Manager: "systemd-user", State: "stopped"})
		return facts
	}
	if component.ID != systemdProxyUnit || component.Manager != "systemd-user" || component.PID <= 1 ||
		component.ConfiguredExecutable == "" || component.Error != "" {
		return invalidLinuxProxyInspectionRuntimeFacts(facts)
	}
	if runtimeErr != nil || !runtimeIdentity.Valid() {
		facts.service = proxy.KnownFact(proxy.ServiceState{
			Manager: "systemd-user", State: "running", PID: component.PID, Executable: component.ConfiguredExecutable,
		})
		return facts
	}
	if component.PID != runtimeIdentity.Process.PID ||
		(component.LiveExecutable != "" && !sameServiceExecutable(component.LiveExecutable, runtimeIdentity.Process.Executable.Path)) ||
		(component.Listener != "" && component.Listener != runtimeIdentity.Listener.Address) ||
		!sameServiceExecutable(component.ConfiguredExecutable, runtimeIdentity.Process.Executable.Path) {
		return invalidLinuxProxyInspectionRuntimeFacts(facts)
	}
	facts.service = proxy.KnownFact(proxy.ServiceState{
		Manager: "systemd-user", State: "running", PID: component.PID, Executable: runtimeIdentity.Process.Executable.Path,
	})
	return facts
}

func unavailableLinuxProxyInspectionFacts() linuxProxyInspectionFacts {
	return linuxProxyInspectionFacts{
		inspector: proxy.UnavailableFact[proxy.InspectorIdentity]("inspector_unavailable"),
		desired:   proxy.UnavailableFact[proxy.DesiredProxyState]("config_unavailable"),
		service:   proxy.UnavailableFact[proxy.ServiceState]("service_unavailable"),
		listener:  proxy.UnavailableFact[proxy.ListenerState]("listener_unavailable"),
		process:   proxy.UnavailableFact[proxy.ProcessState]("process_unavailable"),
		runtime:   proxy.UnavailableFact[proxy.RuntimeIdentity]("runtime_unavailable"),
		dataPlane: proxy.UnavailableFact[proxy.DataPlaneProof]("data_plane_unavailable"),
	}
}

func invalidLinuxProxyInspectionRuntimeFacts(facts linuxProxyInspectionFacts) linuxProxyInspectionFacts {
	facts.service = proxy.InvalidFact[proxy.ServiceState]("service_runtime_mismatch")
	facts.listener = proxy.InvalidFact[proxy.ListenerState]("service_runtime_mismatch")
	facts.process = proxy.InvalidFact[proxy.ProcessState]("service_runtime_mismatch")
	facts.runtime = proxy.InvalidFact[proxy.RuntimeIdentity]("service_runtime_mismatch")
	facts.dataPlane = proxy.InvalidFact[proxy.DataPlaneProof]("service_runtime_mismatch")
	return facts
}

func linuxProxyInspectionPort(instanceRoot string, dependencies linuxProxyInspectionDependencies) (int, bool) {
	if instanceRoot != "" {
		bootstrap, err := dependencies.loadBootstrap()
		if err != nil || bootstrap == nil || bootstrap.StateRoot != instanceRoot || bootstrap.Port < 1 || bootstrap.Port > 65_535 {
			return 0, false
		}
		return bootstrap.Port, true
	}
	config, err := dependencies.loadConfig()
	if err != nil || config == nil || config.Port < 1 || config.Port > 65_535 {
		return 0, false
	}
	return config.Port, true
}

func probeLinuxProxyHealth(ctx context.Context, address string) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+address+"/health", http.NoBody)
	if err != nil {
		return false
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy status redirect refused")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	return err == nil && len(body) > 0 && len(body) <= 64<<10 && response.StatusCode == http.StatusOK &&
		response.Header.Get("Content-Type") == "application/json" && response.Header.Get("Content-Encoding") == ""
}
