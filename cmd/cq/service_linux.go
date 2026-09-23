//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var inspectLinuxProxyRuntimeFn = proxy.InspectLinuxProxyRuntime
var captureLinuxRuntimeWorkerFn = proxy.CaptureLinuxRuntimeWorker
var probeLinuxProxyRuntimeHealthFn = probeLinuxProxyHealth
var linuxProxyRuntimePortFn = func() (int, error) {
	config, err := proxy.LoadExistingConfig()
	if err != nil || config == nil || config.Port < 1 || config.Port > 65_535 {
		return 0, fmt.Errorf("load Linux proxy runtime port")
	}
	return config.Port, nil
}

var linuxProxyRuntimeInspector = func(ctx context.Context, executable string) componentStatus {
	port, err := linuxProxyRuntimePortFn()
	if err != nil {
		return componentStatus{ID: systemdProxyUnit, Manager: "systemd-user", Error: "Linux proxy runtime inspection is unavailable"}
	}
	return inspectLinuxProxyAtPort(ctx, executable, port)
}

func inspectSelectedLinuxProxy(ctx context.Context, executable string, roots userdirs.Roots) componentStatus {
	config, err := proxy.LoadExistingConfigAt(proxy.PathsForRoots(roots))
	if err != nil || config == nil || config.Port < 1 || config.Port > 65535 {
		return componentStatus{Error: "Linux proxy runtime inspection is unavailable"}
	}
	return inspectLinuxProxyAtPortWithHealth(ctx, executable, config.Port, func(ctx context.Context, address string) bool {
		return probeSelectedLinuxProxyHealth(ctx, address, config.LocalToken)
	})
}

func inspectLinuxProxyAtPort(ctx context.Context, executable string, port int) componentStatus {
	return inspectLinuxProxyAtPortWithHealth(ctx, executable, port, probeLinuxProxyRuntimeHealthFn)
}

func inspectLinuxProxyAtPortWithHealth(ctx context.Context, executable string, port int, health func(context.Context, string) bool) componentStatus {
	status := componentStatus{ID: systemdProxyUnit, Manager: "systemd-user"}
	identity, err := inspectLinuxProxyRuntimeFn(ctx, executable, port)
	if err != nil || !identity.Valid() {
		status.Error = "Linux proxy runtime inspection is unavailable"
		return status
	}
	worker, err := captureLinuxRuntimeWorkerFn(ctx, identity.Process)
	if err != nil {
		status.Error = "Linux proxy runtime inspection is unavailable"
		return status
	}
	if !health(ctx, identity.Listener.Address) {
		status.Error = "Linux proxy runtime health is unavailable"
		return status
	}
	confirmed, err := inspectLinuxProxyRuntimeFn(ctx, executable, port)
	if err != nil || !confirmed.Valid() || !confirmed.Equal(identity) {
		status.Error = "Linux proxy runtime inspection is unavailable"
		return status
	}
	confirmedWorker, err := captureLinuxRuntimeWorkerFn(ctx, confirmed.Process)
	if err != nil || !confirmedWorker.Equal(worker) {
		status.Error = "Linux proxy runtime inspection is unavailable"
		return status
	}
	status.Running = true
	status.LiveExecutable = identity.Process.Executable.Path
	status.PID = identity.Process.PID
	status.Listener = identity.Listener.Address
	status.Healthy = true
	return status
}

func init() {
	serviceRefreshRunner = runLinuxServiceRefresh
	serviceLifecycleFactory = defaultLinuxServiceLifecycle
	selectedServiceLifecycleFactory = defaultSelectedLinuxServiceLifecycle
}

var linuxSelectedHome = func() (string, error) {
	current, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", errServiceUnavailable
	}
	return filepath.Clean(current.HomeDir), nil
}

var linuxSelectedSystemctl = runLinuxSystemctl

func defaultLinuxServiceLifecycle(stableExecutable string) (*serviceLifecycle, error) {
	platform, roots, executable, err := defaultLinuxSystemdPlatform(stableExecutable)
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
		unitDirectory:        unitDirectory,
		roots:                roots,
		inspectSelectedProxy: inspectSelectedLinuxProxy,
		executable:           executable,
		run:                  run,
		inspectProxy:         inspectProxy,
	}
	return &serviceLifecycle{
		Platform:       platform,
		Store:          &installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots},
		Executable:     executable,
		Version:        version,
		StatusAttempts: 20,
		StatusInterval: time.Second,
		MutationLocker: installer.FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: roots.State},
	}
}

func defaultLinuxSystemdPlatform(stableExecutable ...string) (*systemdServicePlatform, userdirs.Roots, string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	unitDirectory, err := linuxSystemdUserDirectory()
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	requested := ""
	if len(stableExecutable) > 0 {
		requested = stableExecutable[0]
	}
	executable, err := resolveServiceExecutable(requested)
	if err != nil {
		return nil, userdirs.Roots{}, "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, userdirs.Roots{}, "", fmt.Errorf("resolve absolute executable: %w", err)
	}
	executable = filepath.Clean(executable)
	platform := &systemdServicePlatform{
		unitDirectory:        unitDirectory,
		roots:                roots,
		inspectSelectedProxy: inspectSelectedLinuxProxy,
		executable:           executable,
		run:                  runLinuxSystemctl,
		inspectProxy:         linuxProxyRuntimeInspector,
	}
	return platform, roots, executable, nil
}

func linuxSystemdUserDirectory() (string, error) {
	roots, err := userdirs.Default(userdirs.ConfigRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(roots.Config), "systemd", "user"), nil
}

func resolveLinuxExecutable() (string, error) {
	return resolveServiceExecutable("")
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

// Only a canonical installed unit opts in to completion reporting. Ordinary
// refresh calls preserve the frozen behaviour and never write service receipts.
func runLinuxServiceRefresh(run func() error) error {
	if os.Getenv("CQ_SERVICE_REFRESH") != "1" {
		return run()
	}
	unitDirectory, err := linuxSystemdUserDirectory()
	if err != nil {
		return err
	}
	data, exists, err := readOwnedSystemdUnit(filepath.Join(unitDirectory, systemdRefreshService))
	if err != nil || !exists {
		return errServiceUnavailable
	}
	executable, roots, err := validateOwnedSystemdDefinition(systemdRefreshService, data)
	if err != nil || roots == nil {
		return errServiceUnavailable
	}
	home, _, err := systemdDefinitionRoots(data)
	if err != nil {
		return err
	}
	actual, err := os.Executable()
	if err != nil || !sameServiceExecutable(actual, executable) {
		return errServiceUnavailable
	}
	processRoots, err := userdirs.Default()
	if err != nil || processRoots != *roots {
		return errServiceUnavailable
	}
	if os.Getenv("HOME") != home || os.Getenv("XDG_CONFIG_HOME") != filepath.Dir(roots.Config) || os.Getenv("XDG_CACHE_HOME") != filepath.Dir(roots.Cache) {
		return errServiceUnavailable
	}
	if !strings.Contains(string(data), "Environment=CQ_SERVICE_REFRESH=1\n") {
		return errServiceUnavailable
	}
	return recordServiceRefresh(executable, *roots, run, time.Now)
}

func defaultSelectedLinuxServiceLifecycle(ctx context.Context, action serviceAction, selection serviceSelection) (*serviceLifecycle, error) {
	executable, err := resolveServiceExecutable("")
	if err != nil {
		return nil, err
	}
	home, err := linuxSelectedHome()
	if err != nil {
		return nil, err
	}
	p := &systemdServicePlatform{executable: executable, home: home, run: linuxSelectedSystemctl, inspectSelectedProxy: inspectSelectedLinuxProxy}
	found, err := p.discoverSelected(ctx, selection)
	if err != nil {
		return nil, err
	}
	if !found {
		fresh, _, _, err := defaultLinuxSystemdPlatform(executable)
		if err != nil {
			return nil, err
		}
		p.unitDirectory, p.roots = fresh.unitDirectory, fresh.roots
	}
	if found && p.roots.Config == "" {
		store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: p.roots}
		record, err := store.Load()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, installstate.ErrNotInstalled) {
			if action != serviceInspect {
				return nil, installstate.ErrOwnershipConflict
			}
		} else if err != nil {
			return nil, err
		} else {
			for _, component := range selection.components() {
				names := systemdSelectedUnits(component)
				_, exists, configured, _, err := p.selectedDefinition(ctx, names[0])
				if err != nil {
					return nil, err
				}
				if exists && (!record.HasService(names[len(names)-1]) || !sameServiceExecutable(record.Executable, configured)) {
					return nil, installstate.ErrOwnershipConflict
				}
			}
		}
	}
	if action == serviceInstall && p.roots.Config == "" {
		cache, err := (userdirs.Resolver{Getenv: os.Getenv, UserHomeDir: func() (string, error) { return home, nil }}).Resolve(userdirs.CacheRoot)
		if err != nil {
			return nil, err
		}
		config := filepath.Join(filepath.Dir(filepath.Dir(p.unitDirectory)), "cq")
		p.roots = userdirs.Roots{Config: config, State: filepath.Join(config, "state"), Runtime: filepath.Join(config, "state"), Logs: filepath.Join(config, "state", "logs"), Cache: cache.Cache}
	}
	lifecycle := newLinuxServiceLifecycle(executable, p.unitDirectory, p.roots, p.run, linuxProxyRuntimeInspector)
	lifecycle.Platform = p
	return lifecycle, ctx.Err()
}
