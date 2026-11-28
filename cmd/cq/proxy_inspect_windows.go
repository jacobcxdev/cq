//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type windowsProxyInspectionDependencies struct {
	executable func() (string, error)
	port       func() (int, error)
	task       func(context.Context, string) (componentStatus, windowsTaskRuntimeState, error)
	listener   func(context.Context, int) (uint32, string, error)
	health     func(context.Context, string) error
}

func init() {
	defaultProxyInspectionTarget = func() ProxyInspectionTarget { return windowsProxyInspectionTarget("") }
	proxyInspectionTargetForRoot = windowsProxyInspectionTarget
}

func windowsProxyInspectionTarget(instanceRoot string) ProxyInspectionTarget {
	return windowsProxyInspectionTargetWithDependencies(instanceRoot, windowsProxyInspectionDependencies{
		executable: os.Executable,
		port:       loadWindowsProxyPort,
		task:       inspectRegisteredWindowsProxy,
		listener:   observeWindowsProxyListener,
		health:     checkWindowsProxyHTTPHealth,
	})
}

func windowsProxyInspectionTargetWithDependencies(instanceRoot string, dependencies windowsProxyInspectionDependencies) ProxyInspectionTarget {
	var once sync.Once
	var facts proxy.ProxySnapshot
	snapshot := func(ctx context.Context) proxy.ProxySnapshot {
		once.Do(func() { facts = collectWindowsProxyInspectionFacts(ctx, instanceRoot, dependencies) })
		return facts
	}
	return ProxyInspectionTarget{
		Inspector: func(ctx context.Context) proxy.Fact[proxy.InspectorIdentity] { return snapshot(ctx).Inspector },
		Desired:   func(ctx context.Context) proxy.Fact[proxy.DesiredProxyState] { return snapshot(ctx).Desired },
		Service:   func(ctx context.Context) proxy.Fact[proxy.ServiceState] { return snapshot(ctx).Service },
		Listener:  func(ctx context.Context) proxy.Fact[proxy.ListenerState] { return snapshot(ctx).Listener },
		Process:   func(ctx context.Context) proxy.Fact[proxy.ProcessState] { return snapshot(ctx).Process },
		Runtime:   func(ctx context.Context) proxy.Fact[proxy.RuntimeIdentity] { return snapshot(ctx).Runtime },
		DataPlane: func(ctx context.Context) proxy.Fact[proxy.DataPlaneProof] { return snapshot(ctx).DataPlane },
	}
}

func collectWindowsProxyInspectionFacts(ctx context.Context, instanceRoot string, dependencies windowsProxyInspectionDependencies) proxy.ProxySnapshot {
	facts := proxy.ProxySnapshot{
		Inspector: proxy.UnavailableFact[proxy.InspectorIdentity]("inspector_unavailable"),
		Desired:   proxy.UnavailableFact[proxy.DesiredProxyState]("config_unavailable"),
		Service:   proxy.UnavailableFact[proxy.ServiceState]("task_unavailable"),
		Listener:  proxy.UnavailableFact[proxy.ListenerState]("listener_unavailable"),
		Process:   proxy.UnavailableFact[proxy.ProcessState]("process_unavailable"),
		Runtime:   proxy.UnavailableFact[proxy.RuntimeIdentity]("runtime_unavailable"),
		DataPlane: proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"}),
	}
	executable, err := dependencies.executable()
	if err != nil {
		return facts
	}
	facts.Inspector = proxy.KnownFact(proxy.InspectorIdentity{Executable: executable, Version: version})
	if instanceRoot != "" {
		facts.Desired = proxy.UnavailableFact[proxy.DesiredProxyState]("instance_root_unsupported")
		return facts
	}
	port, err := dependencies.port()
	if err != nil || port <= 0 || port > 65535 {
		return facts
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	const manager = "task-scheduler"
	facts.Desired = proxy.KnownFact(proxy.DesiredProxyState{Manager: manager, Configured: true, Listener: address})
	task, state, err := dependencies.task(ctx, executable)
	switch {
	case errors.Is(err, installstate.ErrOwnershipConflict), errors.Is(err, installstate.ErrInvalidRecord):
		facts.Service = proxy.InvalidFact[proxy.ServiceState]("task_ownership_conflict")
	case errors.Is(err, os.ErrPermission):
		facts.Service = proxy.PermissionDeniedFact[proxy.ServiceState]("task_access_denied")
	case err != nil:
	case !task.Registered:
		facts.Service = proxy.AbsentFact[proxy.ServiceState]()
	case !task.Running:
		facts.Service = proxy.KnownFact(proxy.ServiceState{Manager: manager, State: "stopped"})
	case len(state.EnginePIDs) != 1 || state.EnginePIDs[0] == 0 || task.ConfiguredExecutable == "":
		facts.Service = proxy.InvalidFact[proxy.ServiceState]("task_instance_ambiguous")
	default:
		facts.Service = proxy.KnownFact(proxy.ServiceState{
			Manager: manager, State: "running", PID: int(state.EnginePIDs[0]), Executable: task.ConfiguredExecutable,
		})
	}

	pid, image, err := dependencies.listener(ctx, port)
	switch {
	case errors.Is(err, errWindowsProxyListenerAbsent):
		facts.Listener = proxy.AbsentFact[proxy.ListenerState]()
		facts.Process = proxy.AbsentFact[proxy.ProcessState]()
		return facts
	case errors.Is(err, errWindowsProxyListenerPID), errors.Is(err, errWindowsProxyListenerOwners):
		facts.Listener = proxy.InvalidFact[proxy.ListenerState]("listener_owner_ambiguous")
		return facts
	case err != nil:
		return facts
	}
	facts.Process = proxy.KnownFact(proxy.ProcessState{PID: int(pid), Executable: image})
	listener := proxy.ListenerState{State: "listening", Listener: address, PID: int(pid), Executable: image}
	service := facts.Service.Value
	if service == nil || service.State != "running" || service.PID != int(pid) || !equalWindowsPath(service.Executable, image) {
		listener.State = "foreign"
		facts.Listener = proxy.KnownFact(listener)
		return facts
	}
	service.Executable = image
	facts.Listener = proxy.KnownFact(listener)
	if err := dependencies.health(ctx, address); err != nil {
		facts.Runtime = proxy.KnownFact(proxy.RuntimeIdentity{Health: "unhealthy"})
		return facts
	}
	currentPID, currentImage, err := dependencies.listener(ctx, port)
	if err != nil || currentPID != pid || !equalWindowsPath(currentImage, image) {
		facts.Runtime = proxy.UnavailableFact[proxy.RuntimeIdentity]("listener_changed_during_probe")
		return facts
	}
	facts.Runtime = proxy.KnownFact(proxy.RuntimeIdentity{
		Reachable: true, PID: int(pid), Executable: image, Health: "healthy",
	})
	return facts
}

func inspectRegisteredWindowsProxy(ctx context.Context, executable string) (componentStatus, windowsTaskRuntimeState, error) {
	lifecycle, err := windowsServiceLifecycleForExecutable(executable)
	if err != nil {
		return componentStatus{}, windowsTaskRuntimeState{}, err
	}
	platform := lifecycle.Platform.(*windowsTaskServicePlatform)
	record, err := lifecycle.Store.Load()
	if err == nil {
		platform.executable = record.Executable
	} else if !errors.Is(err, installstate.ErrNotInstalled) {
		return componentStatus{}, windowsTaskRuntimeState{}, err
	}
	return platform.inspectTask(ctx, windowsProxyTask)
}

func observeWindowsProxyListener(ctx context.Context, port int) (uint32, string, error) {
	output, err := runWindowsNetstat(ctx)
	if err != nil {
		return 0, "", err
	}
	pid, err := parseWindowsListeningPID(output, port)
	if err != nil {
		return 0, "", err
	}
	image, err := windowsProcessExecutable(pid)
	return pid, image, err
}
