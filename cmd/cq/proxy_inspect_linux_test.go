//go:build linux

package main

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestLinuxProxyInspectionComposesSystemdRuntimeFacts(t *testing.T) {
	runtime := testLinuxProxyInspectionRuntime(4242, 24567)
	target := linuxProxyInspectionTargetWithDependencies("", linuxProxyInspectionDependencies{
		executable:  func() (string, error) { return "/opt/cq/bin/cq", nil },
		resolvePath: func(path string) (string, error) { return path, nil },
		loadConfig:  func() (*proxy.Config, error) { return &proxy.Config{Port: 24567}, nil },
		inspectService: func(context.Context) (serviceStatus, error) {
			return serviceStatus{Proxy: componentStatus{
				ID: systemdProxyUnit, Manager: "systemd-user", Registered: true, Running: true, Healthy: true,
				ConfiguredExecutable: "/opt/cq/bin/cq", LiveExecutable: "/opt/cq/bin/cq", PID: 4242, Listener: "127.0.0.1:24567",
			}}, nil
		},
		inspectRuntime: func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) { return runtime, nil },
		health:         func(context.Context, string) bool { return true },
	})

	snapshot := InspectProxy(context.Background(), target)
	if snapshot.Desired.Value == nil || snapshot.Desired.Value.Manager != "systemd-user" || snapshot.Desired.Value.Listener != "127.0.0.1:24567" {
		t.Fatalf("desired = %+v", snapshot.Desired)
	}
	if snapshot.Service.Value == nil || snapshot.Service.Value.State != "running" || snapshot.Service.Value.PID != 4242 {
		t.Fatalf("service = %+v", snapshot.Service)
	}
	if snapshot.Listener.Value == nil || snapshot.Listener.Value.State != "listening" || snapshot.Listener.Value.PID != 4242 {
		t.Fatalf("listener = %+v", snapshot.Listener)
	}
	if snapshot.Process.Value == nil || snapshot.Process.Value.PID != 4242 || snapshot.Runtime.Value == nil || !snapshot.Runtime.Value.Reachable || snapshot.Runtime.Value.Health != "healthy" {
		t.Fatalf("runtime topology = process=%+v runtime=%+v", snapshot.Process, snapshot.Runtime)
	}
	if snapshot.DataPlane.Value == nil || snapshot.DataPlane.Value.Proven || snapshot.DataPlane.Value.Code != "unproven" {
		t.Fatalf("data plane = %+v", snapshot.DataPlane)
	}
	if snapshot.Verdict != proxy.ProxyVerdictDegraded {
		t.Fatalf("health-only verdict = %q, want degraded", snapshot.Verdict)
	}
}

func TestLinuxProxyInspectionKeepsRuntimeEvidenceWhenManagerUnavailable(t *testing.T) {
	runtime := testLinuxProxyInspectionRuntime(4242, 24567)
	target := linuxProxyInspectionTargetWithDependencies("", linuxProxyInspectionDependencies{
		executable:  func() (string, error) { return "/opt/cq/bin/cq", nil },
		resolvePath: func(path string) (string, error) { return path, nil },
		loadConfig:  func() (*proxy.Config, error) { return &proxy.Config{Port: 24567}, nil },
		inspectService: func(context.Context) (serviceStatus, error) {
			return serviceStatus{}, errors.New("manager unavailable")
		},
		inspectRuntime: func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) { return runtime, nil },
		health:         func(context.Context, string) bool { return true },
	})

	snapshot := InspectProxy(context.Background(), target)
	if snapshot.Service.Status != proxy.FactUnavailable || snapshot.Listener.Value == nil || snapshot.Process.Value == nil || snapshot.Runtime.Value == nil {
		t.Fatalf("independent facts = service=%+v listener=%+v process=%+v runtime=%+v", snapshot.Service, snapshot.Listener, snapshot.Process, snapshot.Runtime)
	}
	if snapshot.Verdict != proxy.ProxyVerdictIndeterminate {
		t.Fatalf("verdict = %q, want indeterminate", snapshot.Verdict)
	}
}

func TestLinuxProxyInspectionRejectsManagerRuntimePIDMismatch(t *testing.T) {
	runtime := testLinuxProxyInspectionRuntime(4242, 24567)
	facts := collectLinuxProxyInspectionFacts(context.Background(), "", linuxProxyInspectionDependencies{
		executable:  func() (string, error) { return "/opt/cq/bin/cq", nil },
		resolvePath: func(path string) (string, error) { return path, nil },
		loadConfig:  func() (*proxy.Config, error) { return &proxy.Config{Port: 24567}, nil },
		inspectService: func(context.Context) (serviceStatus, error) {
			return serviceStatus{Proxy: componentStatus{
				ID: systemdProxyUnit, Manager: "systemd-user", Registered: true, Running: true,
				ConfiguredExecutable: "/opt/cq/bin/cq", PID: 4343,
			}}, nil
		},
		inspectRuntime: func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) { return runtime, nil },
		health:         func(context.Context, string) bool { return true },
	})
	if facts.service.Status != proxy.FactInvalid || facts.listener.Status != proxy.FactInvalid || facts.process.Status != proxy.FactInvalid || facts.runtime.Status != proxy.FactInvalid {
		t.Fatalf("mismatch facts = service=%+v listener=%+v process=%+v runtime=%+v", facts.service, facts.listener, facts.process, facts.runtime)
	}
}

func testLinuxProxyInspectionRuntime(pid, port int) proxy.LinuxProxyRuntimeIdentity {
	process := proxy.LinuxProcessIdentity{
		PID: pid, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/opt/cq/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: proxy.LinuxExecutableIdentity{
			Path: "/opt/cq/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 0, Size: 100, Mode: 0o100755, SHA256: [32]byte{1},
		},
	}
	listener := proxy.LinuxListenerIdentity{Address: "127.0.0.1:" + strconv.Itoa(port), Inode: 3, Process: process}
	return proxy.LinuxProxyRuntimeIdentity{Process: process, Listener: listener}
}
