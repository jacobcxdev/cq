//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestLinuxServiceUsesXDGSystemdUserDirectory(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	t.Setenv("XDG_CONFIG_HOME", config)

	directory, err := linuxSystemdUserDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config, "systemd", "user"); directory != want {
		t.Fatalf("unit directory = %q, want %q", directory, want)
	}
}

func TestLinuxServiceLifecycleBindsSystemdContract(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "cq")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	unitDirectory := filepath.Join(root, "systemd", "user")
	lifecycle := newLinuxServiceLifecycle(
		executable,
		unitDirectory,
		userdirs.Roots{State: filepath.Join(root, "state")},
		func(context.Context, ...string) ([]byte, error) { return nil, nil },
		func(context.Context, string) componentStatus { return componentStatus{} },
	)
	platform, ok := lifecycle.Platform.(*systemdServicePlatform)
	if !ok {
		t.Fatalf("platform = %T", lifecycle.Platform)
	}
	if platform.unitPath(systemdProxyUnit) != filepath.Join(unitDirectory, "cq-proxy.service") || platform.unitPath(systemdRefreshService) != filepath.Join(unitDirectory, "cq-refresh.service") || platform.unitPath(systemdRefreshTimer) != filepath.Join(unitDirectory, "cq-refresh.timer") {
		t.Fatalf("unit paths do not match published contract")
	}
	if lifecycle.Executable != executable || lifecycle.Version != version {
		t.Fatalf("lifecycle = %#v", lifecycle)
	}
	store, ok := lifecycle.Store.(*installstate.Store)
	if !ok || store.Roots.State == "" {
		t.Fatalf("store = %#v", lifecycle.Store)
	}
}

func TestLinuxProxyRuntimeHookBindsExactKernelIdentity(t *testing.T) {
	previous := inspectLinuxProxyRuntimeFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previous
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	inspectLinuxProxyRuntimeFn = func(_ context.Context, executable string, port int) (proxy.LinuxProxyRuntimeIdentity, error) {
		if executable != "/home/test/bin/cq" || port != 24567 {
			t.Fatalf("inspection inputs = %q %d", executable, port)
		}
		process := proxy.LinuxProcessIdentity{
			PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
			Arguments:  []string{executable, "proxy", "start"},
			CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
			Executable: proxy.LinuxExecutableIdentity{
				Path: executable, Device: 1, Inode: 2, Links: 1, Owner: 501,
				Size: 4, Mode: 0o100755, SHA256: [32]byte{1},
			},
		}
		return proxy.LinuxProxyRuntimeIdentity{
			Process:  process,
			Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process},
		}, nil
	}

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if !status.Healthy || status.PID != 731 || status.LiveExecutable != "/home/test/bin/cq" || status.Listener != "127.0.0.1:19280" || status.Error != "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedWithoutConfiguredPort(t *testing.T) {
	previous := linuxProxyRuntimePortFn
	t.Cleanup(func() { linuxProxyRuntimePortFn = previous })
	linuxProxyRuntimePortFn = func() (int, error) { return 0, errors.New("missing") }
	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}
