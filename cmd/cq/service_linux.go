//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var linuxProxyRuntimeInspector = func(context.Context, string) componentStatus {
	return componentStatus{
		ID:      systemdProxyUnit,
		Manager: "systemd-user",
		Error:   "Linux proxy runtime inspection is unavailable",
	}
}

func init() {
	serviceLifecycleFactory = defaultLinuxServiceLifecycle
}

func defaultLinuxServiceLifecycle() (*serviceLifecycle, error) {
	platform, roots, executable, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return nil, err
	}
	return newLinuxServiceLifecycle(executable, platform.unitDirectory, roots, platform.run, linuxProxyRuntimeInspector), nil
}

func newLinuxServiceLifecycle(
	executable string,
	unitDirectory string,
	roots userdirs.Roots,
	run func(context.Context, ...string) ([]byte, error),
	inspectProxy func(context.Context, string) componentStatus,
) *serviceLifecycle {
	platform := &systemdServicePlatform{
		unitDirectory: unitDirectory,
		executable:    executable,
		run:           run,
		inspectProxy:  inspectProxy,
	}
	return &serviceLifecycle{
		Platform:       platform,
		Store:          &installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots},
		Executable:     executable,
		Version:        version,
		StatusAttempts: 20,
		StatusInterval: time.Second,
	}
}

func defaultLinuxSystemdPlatform() (*systemdServicePlatform, userdirs.Roots, string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	unitDirectory, err := linuxSystemdUserDirectory()
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	executable, err := resolveLinuxExecutable()
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, userdirs.Roots{}, "", fmt.Errorf("resolve absolute executable: %w", err)
	}
	executable = filepath.Clean(executable)
	platform := &systemdServicePlatform{
		unitDirectory: unitDirectory,
		executable:    executable,
		run:           runLinuxSystemctl,
		inspectProxy:  linuxProxyRuntimeInspector,
	}
	return platform, roots, executable, nil
}

func linuxSystemdUserDirectory() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func resolveLinuxExecutable() (string, error) {
	if executable, err := exec.LookPath("cq"); err == nil {
		return executable, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	return executable, nil
}

func runLinuxSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	message := strings.TrimSpace(string(output))
	if message != "" {
		return output, fmt.Errorf("systemctl %s: %w: %s", args[1], err, message)
	}
	return output, fmt.Errorf("systemctl %s: %w", args[1], err)
}

func ensureAgent() {}

func installAgent(interval int) error {
	platform, _, executable, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := platform.Preflight(ctx, executable); err != nil {
		return err
	}
	if err := platform.installRefreshOnly(ctx, executable, interval); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cq: installed systemd user refresh timer (every %ds)\n", normaliseRefreshInterval(interval))
	fmt.Fprintf(os.Stderr, "cq: units: %s\n", platform.unitDirectory)
	return nil
}

func uninstallAgent() error {
	platform, _, _, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return err
	}
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cq: uninstalled systemd user refresh timer\n")
	return nil
}

func installProxyAgent() error {
	platform, _, executable, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := platform.Preflight(ctx, executable); err != nil {
		return err
	}
	if err := platform.installProxyOnly(ctx, executable); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cq: installed systemd user proxy service\n")
	fmt.Fprintf(os.Stderr, "cq: unit: %s\n", platform.unitPath(systemdProxyUnit))
	return nil
}

func uninstallProxyAgent() error {
	platform, _, _, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return err
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cq: uninstalled systemd user proxy service\n")
	return nil
}

func restartProxyAgent() error {
	platform, _, _, err := defaultLinuxSystemdPlatform()
	if err != nil {
		return err
	}
	return platform.RestartProxy(context.Background())
}

func normaliseRefreshInterval(interval int) int {
	if interval <= 0 {
		return 1800
	}
	return interval
}
