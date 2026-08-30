//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
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

func TestLinuxProxyRuntimeHookFailsClosedUntilRuntimeBinds(t *testing.T) {
	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}
