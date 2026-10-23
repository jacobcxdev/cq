package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/userdirs"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
)

const (
	systemdProxyUnit      = "cq-proxy.service"
	systemdRefreshService = "cq-refresh.service"
	systemdRefreshTimer   = "cq-refresh.timer"
	maxSystemdUnitBytes   = 64 << 10
)

var systemdShowProperties = []string{
	"LoadState",
	"ActiveState",
	"SubState",
	"MainPID",
	"ExecStart",
	"FragmentPath",
	"UnitFileState",
	"Result",
	"NextElapseUSecRealtime",
}

type systemdServicePlatform struct {
	unitDirectory        string
	home                 string
	roots                userdirs.Roots
	inspectSelectedProxy func(context.Context, string, userdirs.Roots) componentStatus
	executable           string
	run                  func(context.Context, ...string) ([]byte, error)
	inspectProxy         func(context.Context, string) componentStatus
}

func renderSystemdServiceDefinitions(executable string) (map[string][]byte, error) {
	return renderSystemdServiceDefinitionsWithInterval(executable, 1800)
}

func renderSystemdServiceDefinitionsWithInterval(executable string, interval int) (map[string][]byte, error) {
	if executable == "" || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, fmt.Errorf("systemd executable must be a clean absolute path")
	}
	if interval <= 0 {
		interval = 1800
	}
	encodedExecutable, err := encodeSystemdArgument(executable)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		systemdProxyUnit: []byte(`[Unit]
Description=CQ local proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + encodedExecutable + ` proxy start
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`),
		systemdRefreshService: []byte(`[Unit]
Description=Refresh CQ credentials

[Service]
Type=oneshot
ExecStart=` + encodedExecutable + ` refresh
`),
		systemdRefreshTimer: []byte(`[Unit]
Description=Refresh CQ credentials every 30 minutes

[Timer]
OnStartupSec=0
OnUnitActiveSec=` + formatSystemdInterval(interval) + `
Persistent=true
Unit=cq-refresh.service

[Install]
WantedBy=timers.target
`),
	}, nil
}

func formatSystemdInterval(seconds int) string {
	if seconds == 1800 {
		return "30min"
	}
	return strconv.Itoa(seconds) + "s"
}

func encodeSystemdArgument(argument string) (string, error) {
	if argument == "" {
		return "", fmt.Errorf("systemd argument is empty")
	}
	if strings.ContainsAny(argument, "\x00\r\n") {
		return "", fmt.Errorf("systemd argument contains a line break or NUL")
	}
	argument = strings.ReplaceAll(argument, "%", "%%")
	safe := true
	for _, character := range argument {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			!strings.ContainsRune("/_+.,:=@-%", character) {
			safe = false
			break
		}
	}
	if safe {
		return argument, nil
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`)
	return `"` + replacer.Replace(argument) + `"`, nil
}

func (platform *systemdServicePlatform) Preflight(ctx context.Context, executable string) error {
	if err := validateSystemdExecutable(executable); err != nil {
		return err
	}
	if platform.run == nil {
		return fmt.Errorf("user systemd manager runner is unavailable")
	}
	if _, err := platform.run(ctx, "--user", "show-environment"); err != nil {
		return fmt.Errorf("user systemd manager is unavailable: %w", err)
	}
	platform.executable = executable
	expected, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		return err
	}
	for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
		properties, err := platform.show(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect loaded %s: %w", name, err)
		}
		data, err := readBoundedSystemdUnit(platform.unitPath(name))
		localExists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if localExists && !bytes.Equal(data, expected[name]) {
			return fmt.Errorf("%w: %s definition differs", installstate.ErrOwnershipConflict, name)
		}
		switch properties["LoadState"] {
		case "not-found":
		case "loaded":
			fragment := filepath.Clean(properties["FragmentPath"])
			if !localExists || properties["FragmentPath"] == "" || fragment != platform.unitPath(name) {
				return fmt.Errorf("%w: %s is loaded from %q", installstate.ErrOwnershipConflict, name, properties["FragmentPath"])
			}
		default:
			return fmt.Errorf("%w: %s has load state %q", installstate.ErrOwnershipConflict, name, properties["LoadState"])
		}
	}
	return nil
}

func (platform *systemdServicePlatform) PrepareRollback(ctx context.Context) (serviceRestore, error) {
	snapshot, err := platform.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return func(restoreCtx context.Context) error {
		return platform.Restore(restoreCtx, snapshot)
	}, nil
}

func (platform *systemdServicePlatform) Snapshot(ctx context.Context) (servicePlatformSnapshot, error) {
	snapshot := servicePlatformSnapshot{Manager: "systemd-user", Components: make([]serviceComponentSnapshot, 0, 3)}
	for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
		definition, exists, err := platform.definition(name)
		if err != nil {
			return servicePlatformSnapshot{}, err
		}
		properties, err := platform.show(ctx, name)
		if err != nil {
			return servicePlatformSnapshot{}, err
		}
		if properties["LoadState"] == "loaded" && !exists {
			return servicePlatformSnapshot{}, fmt.Errorf("%w: %s is loaded without managed definition", installstate.ErrOwnershipConflict, name)
		}
		unitFileState := ""
		if exists {
			unitFileState = properties["UnitFileState"]
			if !supportedSystemdUnitFileState(unitFileState) {
				return servicePlatformSnapshot{}, fmt.Errorf("%w: %s has unsupported unit file state %q", installstate.ErrOwnershipConflict, name, unitFileState)
			}
		}
		snapshot.Components = append(snapshot.Components, serviceComponentSnapshot{
			ID:            name,
			Definition:    append([]byte(nil), definition...),
			Exists:        exists,
			UnitFileState: unitFileState,
			Running:       properties["ActiveState"] == "active",
		})
	}
	return snapshot, nil
}

func (platform *systemdServicePlatform) Restore(ctx context.Context, snapshot servicePlatformSnapshot) error {
	names := []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer}
	if snapshot.Manager != "systemd-user" || snapshot.FolderExists || snapshot.FolderSecurityDescriptor != "" || len(snapshot.Components) != len(names) {
		return fmt.Errorf("invalid systemd service snapshot")
	}
	for index, component := range snapshot.Components {
		invalidAbsent := !component.Exists && (component.Enabled || component.UnitFileState != "" || component.Running || len(component.Definition) != 0)
		if component.ID != names[index] || component.Enabled || invalidAbsent || (component.Exists && !supportedSystemdUnitFileState(component.UnitFileState)) || len(component.Definition) > maxSystemdUnitBytes {
			return fmt.Errorf("invalid systemd service snapshot component %q", component.ID)
		}
	}
	var result error
	result = errors.Join(result, platform.RemoveRefresh(ctx), platform.RemoveProxy(ctx))
	for _, component := range snapshot.Components {
		if !component.Exists {
			continue
		}
		if err := atomicWriteSystemdUnit(platform.unitPath(component.ID), component.Definition); err != nil {
			result = errors.Join(result, fmt.Errorf("restore %s definition: %w", component.ID, err))
		}
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		return errors.Join(result, fmt.Errorf("reload restored user systemd units: %w", err))
	}
	for _, component := range snapshot.Components {
		if !component.Exists {
			continue
		}
		switch component.UnitFileState {
		case "enabled":
			if _, err := platform.run(ctx, "--user", "enable", component.ID); err != nil {
				result = errors.Join(result, fmt.Errorf("restore enabled state for %s: %w", component.ID, err))
			}
		case "enabled-runtime":
			if _, err := platform.run(ctx, "--user", "enable", "--runtime", component.ID); err != nil {
				result = errors.Join(result, fmt.Errorf("restore runtime-enabled state for %s: %w", component.ID, err))
			}
		}
		if component.Running {
			if _, err := platform.run(ctx, "--user", "start", component.ID); err != nil {
				result = errors.Join(result, fmt.Errorf("restore running state for %s: %w", component.ID, err))
			}
		}
	}
	return errors.Join(result, syncSystemdDirectory(platform.unitDirectory))
}

func supportedSystemdUnitFileState(state string) bool {
	switch state {
	case "enabled", "enabled-runtime", "disabled", "static":
		return true
	default:
		return false
	}
}

func (platform *systemdServicePlatform) InstallProxy(ctx context.Context, executable string) error {
	if selectedServiceContext(ctx) {
		return platform.installSelected(ctx, executable, serviceProxy)
	}
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		return err
	}
	if err := atomicWriteSystemdUnit(platform.unitPath(systemdProxyUnit), definitions[systemdProxyUnit]); err != nil {
		return fmt.Errorf("write %s: %w", systemdProxyUnit, err)
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("reload user systemd manager: %w", err)
	}
	if _, err := platform.run(ctx, "--user", "enable", "--now", systemdProxyUnit); err != nil {
		return fmt.Errorf("enable %s: %w", systemdProxyUnit, err)
	}
	return nil
}

func (platform *systemdServicePlatform) InstallRefresh(ctx context.Context, executable string) error {
	if selectedServiceContext(ctx) {
		return platform.installSelected(ctx, executable, serviceRefresh)
	}
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		return err
	}
	for _, name := range []string{systemdRefreshService, systemdRefreshTimer} {
		if err := atomicWriteSystemdUnit(platform.unitPath(name), definitions[name]); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("reload user systemd manager: %w", err)
	}
	if _, err := platform.run(ctx, "--user", "enable", "--now", systemdRefreshTimer); err != nil {
		return fmt.Errorf("enable %s: %w", systemdRefreshTimer, err)
	}
	if _, err := platform.run(ctx, "--user", "start", systemdRefreshService); err != nil {
		return fmt.Errorf("run initial credential refresh: %w", err)
	}
	return nil
}

func (platform *systemdServicePlatform) installProxyOnly(ctx context.Context, executable string) error {
	return platform.InstallProxy(ctx, executable)
}

func (platform *systemdServicePlatform) installRefreshOnly(ctx context.Context, executable string, interval int) error {
	definitions, err := renderSystemdServiceDefinitionsWithInterval(executable, interval)
	if err != nil {
		return err
	}
	for _, name := range []string{systemdRefreshService, systemdRefreshTimer} {
		if err := atomicWriteSystemdUnit(platform.unitPath(name), definitions[name]); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("reload user systemd manager: %w", err)
	}
	if _, err := platform.run(ctx, "--user", "enable", "--now", systemdRefreshTimer); err != nil {
		return fmt.Errorf("enable %s: %w", systemdRefreshTimer, err)
	}
	if _, err := platform.run(ctx, "--user", "start", systemdRefreshService); err != nil {
		return fmt.Errorf("run initial credential refresh: %w", err)
	}
	return nil
}

func (platform *systemdServicePlatform) RestartProxy(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		_, err := platform.selectedRun(ctx, "restart", systemdProxyUnit)
		return err
	}
	if _, err := platform.run(ctx, "--user", "restart", systemdProxyUnit); err != nil {
		return fmt.Errorf("restart %s: %w", systemdProxyUnit, err)
	}
	return nil
}

func (platform *systemdServicePlatform) RestartRefresh(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		_, err := platform.selectedRun(ctx, "restart", systemdRefreshService)
		return err
	}
	if _, err := platform.run(ctx, "--user", "restart", systemdRefreshService); err != nil {
		return fmt.Errorf("restart %s: %w", systemdRefreshService, err)
	}
	return nil
}

func (platform *systemdServicePlatform) RemoveProxy(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		return platform.removeSelected(ctx, serviceProxy)
	}
	return platform.remove(ctx, []string{systemdProxyUnit}, []string{systemdProxyUnit})
}

func (platform *systemdServicePlatform) RemoveRefresh(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		return platform.removeSelected(ctx, serviceRefresh)
	}
	return platform.remove(ctx, []string{systemdRefreshTimer, systemdRefreshService}, []string{systemdRefreshTimer})
}

func (platform *systemdServicePlatform) remove(ctx context.Context, files, disable []string) error {
	changed := false
	for _, name := range files {
		if _, err := os.Lstat(platform.unitPath(name)); err == nil {
			changed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	if !changed {
		return nil
	}
	var result error
	args := append([]string{"--user", "disable", "--now"}, disable...)
	if _, err := platform.run(ctx, args...); err != nil && !isSystemdUnitAbsent(err) {
		result = errors.Join(result, fmt.Errorf("disable CQ systemd units: %w", err))
	}
	for _, name := range files {
		if err := os.Remove(platform.unitPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		result = errors.Join(result, fmt.Errorf("reload user systemd manager: %w", err))
	}
	if err := syncSystemdDirectory(platform.unitDirectory); err != nil {
		result = errors.Join(result, fmt.Errorf("sync systemd unit directory: %w", err))
	}
	return result
}

func (platform *systemdServicePlatform) Inspect(ctx context.Context) (serviceStatus, error) {
	proxyDefinition, proxyExists, err := platform.definition(systemdProxyUnit)
	if err != nil {
		return serviceStatus{}, err
	}
	refreshDefinition, refreshExists, err := platform.definition(systemdRefreshService)
	if err != nil {
		return serviceStatus{}, err
	}
	_, timerExists, err := platform.definition(systemdRefreshTimer)
	if err != nil {
		return serviceStatus{}, err
	}
	proxyProperties, err := platform.show(ctx, systemdProxyUnit)
	if err != nil {
		return serviceStatus{}, err
	}
	refreshProperties, err := platform.show(ctx, systemdRefreshService)
	if err != nil {
		return serviceStatus{}, err
	}
	timerProperties, err := platform.show(ctx, systemdRefreshTimer)
	if err != nil {
		return serviceStatus{}, err
	}

	proxyStatus := componentStatus{ID: systemdProxyUnit, Manager: "systemd-user", Registered: proxyExists && proxyProperties["LoadState"] == "loaded"}
	if proxyExists {
		proxyStatus.ConfiguredExecutable, _ = parseSystemdExecStartExecutable(proxyDefinition)
	}
	proxyStatus.Running = proxyStatus.Registered && proxyProperties["ActiveState"] == "active" && proxyProperties["SubState"] == "running"
	if proxyStatus.Running {
		mainPID, ok := proxyProperties["MainPID"]
		if !ok {
			return serviceStatus{}, fmt.Errorf("systemctl show omitted MainPID")
		}
		proxyStatus.PID, err = strconv.Atoi(mainPID)
		if err != nil || proxyStatus.PID <= 0 {
			return serviceStatus{}, fmt.Errorf("systemctl show returned invalid MainPID %q", mainPID)
		}
	} else {
		proxyStatus.PID, _ = strconv.Atoi(proxyProperties["MainPID"])
	}
	if proxyStatus.Running && platform.inspectProxy != nil {
		runtimeStatus := platform.inspectProxy(ctx, proxyStatus.ConfiguredExecutable)
		proxyStatus.LiveExecutable = runtimeStatus.LiveExecutable
		proxyStatus.Listener = runtimeStatus.Listener
		proxyStatus.Error = runtimeStatus.Error
		if runtimeStatus.PID != proxyStatus.PID {
			proxyStatus.Error = fmt.Sprintf("runtime PID %d differs from systemd MainPID %d", runtimeStatus.PID, proxyStatus.PID)
		} else {
			proxyStatus.Healthy = runtimeStatus.Running && runtimeStatus.Healthy && proxyStatus.PID > 0 && sameServiceExecutable(proxyStatus.LiveExecutable, proxyStatus.ConfiguredExecutable)
		}
	}

	refreshStatus := componentStatus{ID: systemdRefreshTimer, Manager: "systemd-user"}
	refreshStatus.Registered = refreshExists && timerExists && refreshProperties["LoadState"] == "loaded" && timerProperties["LoadState"] == "loaded"
	if refreshExists {
		refreshStatus.ConfiguredExecutable, _ = parseSystemdExecStartExecutable(refreshDefinition)
	}
	refreshStatus.Running = refreshStatus.Registered && refreshProperties["ActiveState"] == "active"
	switch refreshProperties["Result"] {
	case "success":
		refreshStatus.LastResult = "success"
	case "", "none":
	default:
		refreshStatus.LastResult = "failed"
	}
	refreshStatus.Healthy = refreshStatus.Registered && timerProperties["ActiveState"] == "active" && timerProperties["SubState"] == "waiting" && refreshProperties["Result"] == "success"
	return serviceStatus{Proxy: proxyStatus, Refresh: refreshStatus}, nil
}

func (platform *systemdServicePlatform) show(ctx context.Context, unit string) (map[string]string, error) {
	propertyArgument := "--property=" + strings.Join(systemdShowProperties, ",")
	output, err := platform.run(ctx, "--user", "show", unit, "--no-pager", propertyArgument)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", unit, err)
	}
	return parseSystemdShow(output)
}

func (platform *systemdServicePlatform) definition(name string) ([]byte, bool, error) {
	data, err := readBoundedSystemdUnit(platform.unitPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (platform *systemdServicePlatform) unitPath(name string) string {
	return filepath.Join(platform.unitDirectory, name)
}

func parseSystemdShow(output []byte) (map[string]string, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("invalid systemctl show output")
		}
		values[parts[0]] = parts[1]
	}
	for _, required := range []string{"LoadState", "ActiveState", "SubState", "Result"} {
		if _, ok := values[required]; !ok {
			return nil, fmt.Errorf("systemctl show omitted %s", required)
		}
	}
	return values, nil
}

func parseSystemdExecStartExecutable(unit []byte) (string, error) {
	for _, line := range strings.Split(string(unit), "\n") {
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		encoded := strings.TrimPrefix(line, "ExecStart=")
		if encoded == "" {
			return "", fmt.Errorf("empty ExecStart")
		}
		if encoded[0] == '"' {
			var value strings.Builder
			escaped := false
			for index := 1; index < len(encoded); index++ {
				character := encoded[index]
				if escaped {
					switch character {
					case '\\', '"':
						value.WriteByte(character)
					case 't':
						value.WriteByte('\t')
					default:
						return "", fmt.Errorf("unsupported ExecStart escape")
					}
					escaped = false
					continue
				}
				if character == '\\' {
					escaped = true
					continue
				}
				if character == '"' {
					return strings.ReplaceAll(value.String(), "%%", "%"), nil
				}
				value.WriteByte(character)
			}
			return "", fmt.Errorf("unterminated ExecStart quote")
		}
		value := encoded
		if index := strings.IndexByte(value, ' '); index >= 0 {
			value = value[:index]
		}
		return strings.ReplaceAll(value, "%%", "%"), nil
	}
	return "", fmt.Errorf("ExecStart is missing")
}

func readBoundedSystemdUnit(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSystemdUnitBytes {
		return nil, fmt.Errorf("systemd unit is not a bounded regular file")
	}
	buffer := make([]byte, info.Size())
	if _, err := io.ReadFull(file, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

func atomicWriteSystemdUnit(path string, data []byte) error {
	if len(data) > maxSystemdUnitBytes || path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("invalid systemd unit write")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	inspector := fsutil.OSFileSystem{}
	owner, ownerOK := inspector.FileOwnerUID(info)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ownerOK || owner != inspector.EffectiveUID() {
		return fmt.Errorf("systemd user directory is not private and user-owned")
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncSystemdDirectory(directory)
}

func syncSystemdDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateSystemdExecutable(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("service executable must be a clean absolute path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect service executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service executable is not an executable regular file")
	}
	return nil
}

func isSystemdUnitAbsent(err error) bool {
	var exitError interface{ ExitCode() int }
	return errors.As(err, &exitError) && exitError.ExitCode() == 5
}

func sameServiceExecutable(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && resolvedLeft == resolvedRight
}

var _ servicePlatform = (*systemdServicePlatform)(nil)

// Selected operations use native enablement and only the selected definitions.
func systemdSelectedUnits(selection serviceSelection) []string {
	var names []string
	for _, component := range selection.components() {
		if component == serviceProxy {
			names = append(names, systemdProxyUnit)
		} else {
			names = append(names, systemdRefreshService, systemdRefreshTimer)
		}
	}
	return names
}
func (platform *systemdServicePlatform) selectedRun(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if platform.run == nil {
		return nil, errServiceUnavailable
	}
	output, err := platform.run(ctx, append([]string{"--user"}, args...)...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.Join(errServiceUnavailable, err)
	}
	return output, nil
}
func (platform *systemdServicePlatform) selectedShow(ctx context.Context, name string) (map[string]string, error) {
	properties := append(append([]string(nil), systemdShowProperties...), "DropInPaths", "NeedDaemonReload", "ExecMainStartTimestamp", "ExecMainExitTimestamp", "ExecMainCode", "ExecMainStatus")
	output, err := platform.selectedRun(ctx, "show", name, "--no-pager", "--all", "--timestamp=us+utc", "--property="+strings.Join(properties, ","))
	if err != nil {
		return nil, err
	}
	values, err := parseSystemdShow(output)
	if err != nil {
		return nil, errors.Join(errServiceUnavailable, err)
	}
	return values, nil
}
func (platform *systemdServicePlatform) StartProxy(ctx context.Context) error {
	_, err := platform.selectedRun(ctx, "enable", "--now", systemdProxyUnit)
	return err
}
func (platform *systemdServicePlatform) StopProxy(ctx context.Context) error {
	return platform.disableSelected(ctx, systemdProxyUnit)
}
func (platform *systemdServicePlatform) StartRefresh(ctx context.Context) error {
	if _, err := platform.selectedRun(ctx, "enable", "--now", systemdRefreshTimer); err != nil {
		return err
	}
	_, err := platform.selectedRun(ctx, "start", systemdRefreshService)
	return err
}
func (platform *systemdServicePlatform) StopRefresh(ctx context.Context) error {
	if err := platform.disableSelected(ctx, systemdRefreshTimer); err != nil {
		return err
	}
	_, err := platform.selectedRun(ctx, "stop", systemdRefreshService)
	return err
}

// Persistent and runtime enablement use different native symlink directories.
// Clear persistent policy first, then any runtime policy revealed beneath it.
func (platform *systemdServicePlatform) disableSelected(ctx context.Context, name string) error {
	if _, err := platform.selectedRun(ctx, "disable", "--now", name); err != nil {
		return err
	}
	properties, err := platform.selectedShow(ctx, name)
	if err != nil {
		return err
	}
	if properties["UnitFileState"] == "enabled-runtime" {
		if _, err := platform.selectedRun(ctx, "disable", "--runtime", "--now", name); err != nil {
			return err
		}
		properties, err = platform.selectedShow(ctx, name)
		if err != nil {
			return err
		}
	}
	if properties["UnitFileState"] != "disabled" && properties["LoadState"] != "not-found" {
		return installstate.ErrOwnershipConflict
	}
	if properties["ActiveState"] != "inactive" && properties["ActiveState"] != "failed" {
		return ErrServiceUnhealthy
	}
	return nil
}
func validateSystemdOwnedExecutable(path string) error {
	if err := validateSystemdExecutable(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	fs := fsutil.OSFileSystem{}
	uid, ok := fs.FileOwnerUID(info)
	if !ok || (uid != 0 && uid != fs.EffectiveUID()) || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return installstate.ErrOwnershipConflict
	}
	return nil
}
func readOwnedSystemdUnit(path string) ([]byte, bool, error) {
	fs := fsutil.OSFileSystem{}
	directory, err := os.Lstat(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	owner, ownerOK := fs.FileOwnerUID(directory)
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 || directory.Mode().Perm()&0o022 != 0 || !ownerOK || owner != fs.EffectiveUID() {
		return nil, false, installstate.ErrOwnershipConflict
	}
	// OpenNoFollow also uses O_NONBLOCK on Unix, so a replaced FIFO cannot hang.
	file, err := fs.OpenNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.Join(installstate.ErrOwnershipConflict, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	uid, ok := fs.FileOwnerUID(info)
	identity, identityOK := fs.FileIdentity(info)
	if !info.Mode().IsRegular() || info.Size() > maxSystemdUnitBytes || info.Mode().Perm()&0o022 != 0 || !ok || uid != fs.EffectiveUID() || !identityOK || identity.Links != 1 {
		return nil, false, installstate.ErrOwnershipConflict
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSystemdUnitBytes+1))
	if len(data) > maxSystemdUnitBytes {
		return nil, false, installstate.ErrOwnershipConflict
	}
	return data, err == nil, err
}
func systemdDefinitionRoots(data []byte) (string, *userdirs.Roots, error) {
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		value, err := parseSystemdExecStartExecutable([]byte("ExecStart=" + strings.TrimPrefix(line, "Environment=")))
		if err != nil {
			return "", nil, installstate.ErrOwnershipConflict
		}
		pair := strings.SplitN(value, "=", 2)
		if len(pair) != 2 || env[pair[0]] != "" {
			return "", nil, installstate.ErrOwnershipConflict
		}
		env[pair[0]] = pair[1]
	}
	if len(env) == 0 {
		return "", nil, nil
	}
	if len(env) != 3 && (len(env) != 4 || env["CQ_SERVICE_REFRESH"] != "1") {
		return "", nil, installstate.ErrOwnershipConflict
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		value := env[key]
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\n\r") {
			return "", nil, installstate.ErrOwnershipConflict
		}
	}
	config := filepath.Join(env["XDG_CONFIG_HOME"], "cq")
	state := filepath.Join(config, "state")
	roots := &userdirs.Roots{Config: config, State: state, Runtime: state, Cache: filepath.Join(env["XDG_CACHE_HOME"], "cq"), Logs: filepath.Join(state, "logs")}
	return env["HOME"], roots, nil
}
func renderSelectedSystemdDefinitions(executable, home string, roots userdirs.Roots) (map[string][]byte, error) {
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		return nil, err
	}
	values := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Dir(roots.Config), "XDG_CACHE_HOME=" + filepath.Dir(roots.Cache)}
	var environment strings.Builder
	for _, value := range values {
		encoded, err := encodeSystemdArgument(value)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&environment, "Environment=%s\n", encoded)
	}
	_, resolved, err := systemdDefinitionRoots([]byte(environment.String()))
	if err != nil || resolved == nil || *resolved != roots {
		return nil, fmt.Errorf("invalid installed systemd roots")
	}
	for _, name := range []string{systemdProxyUnit, systemdRefreshService} {
		bindings := environment.String()
		if name == systemdRefreshService {
			bindings += "Environment=CQ_SERVICE_REFRESH=1\n"
		}
		definitions[name] = bytes.Replace(definitions[name], []byte("[Service]\n"), []byte("[Service]\n"+bindings), 1)
	}
	return definitions, nil
}
func validateOwnedSystemdDefinition(name string, data []byte) (string, *userdirs.Roots, error) {
	if name == systemdRefreshTimer {
		interval := 1800
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "OnUnitActiveSec=") {
				continue
			}
			value := strings.TrimPrefix(line, "OnUnitActiveSec=")
			if value != "30min" {
				if !strings.HasSuffix(value, "s") {
					return "", nil, installstate.ErrOwnershipConflict
				}
				parsed, err := strconv.Atoi(strings.TrimSuffix(value, "s"))
				if err != nil || parsed <= 0 {
					return "", nil, installstate.ErrOwnershipConflict
				}
				interval = parsed
			}
		}
		defs, _ := renderSystemdServiceDefinitionsWithInterval("/cq", interval)
		if !bytes.Equal(data, defs[name]) {
			return "", nil, installstate.ErrOwnershipConflict
		}
		return "", nil, nil
	}
	executable, err := parseSystemdExecStartExecutable(data)
	if err != nil || validateSystemdOwnedExecutable(executable) != nil {
		return "", nil, installstate.ErrOwnershipConflict
	}
	home, roots, err := systemdDefinitionRoots(data)
	if err != nil {
		return "", nil, err
	}
	var defs map[string][]byte
	if roots == nil {
		defs, err = renderSystemdServiceDefinitions(executable)
	} else {
		defs, err = renderSelectedSystemdDefinitions(executable, home, *roots)
	}
	if err != nil || !bytes.Equal(data, defs[name]) {
		return "", nil, installstate.ErrOwnershipConflict
	}
	return executable, roots, nil
}
func (platform *systemdServicePlatform) selectedDefinition(ctx context.Context, name string) ([]byte, bool, string, *userdirs.Roots, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, "", nil, err
	}
	data, exists, err := readOwnedSystemdUnit(platform.unitPath(name))
	if err != nil || !exists {
		return data, exists, "", nil, err
	}
	executable, roots, err := validateOwnedSystemdDefinition(name, data)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return data, exists, executable, roots, err
}
func (platform *systemdServicePlatform) validateSelectedFragment(name string, exists bool, properties map[string]string) error {
	if _, ok := properties["DropInPaths"]; !ok {
		return errServiceUnavailable
	}
	if properties["DropInPaths"] != "" || properties["NeedDaemonReload"] == "yes" {
		return installstate.ErrOwnershipConflict
	}
	switch properties["LoadState"] {
	case "not-found":
		return nil
	case "loaded":
		if exists && properties["FragmentPath"] == platform.unitPath(name) {
			return nil
		}
	}
	return installstate.ErrOwnershipConflict
}
func (platform *systemdServicePlatform) PreflightSelected(ctx context.Context, executable string, selection serviceSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSystemdOwnedExecutable(executable); err != nil {
		return err
	}
	// Validate every selected file before launching the manager, including fresh installs.
	for _, name := range systemdSelectedUnits(selection) {
		_, exists, configured, _, err := platform.selectedDefinition(ctx, name)
		if err != nil {
			return err
		}
		if exists && configured != "" && !sameServiceExecutable(configured, executable) {
			return installstate.ErrOwnershipConflict
		}
	}
	if _, err := platform.selectedRun(ctx, "show-environment"); err != nil {
		return err
	}
	for _, name := range systemdSelectedUnits(selection) {
		_, exists, _, _, err := platform.selectedDefinition(ctx, name)
		if err != nil {
			return err
		}
		properties, err := platform.selectedShow(ctx, name)
		if err != nil {
			return err
		}
		if err := platform.validateSelectedFragment(name, exists, properties); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (platform *systemdServicePlatform) InspectSelected(ctx context.Context, selection serviceSelection) (serviceStatus, error) {
	var status serviceStatus
	if err := ctx.Err(); err != nil {
		return status, err
	}
	if err := validateSystemdOwnedExecutable(platform.executable); err != nil {
		return status, err
	}
	for _, component := range selection.components() {
		names := systemdSelectedUnits(component)
		c := componentStatus{ID: names[len(names)-1], Manager: "systemd-user", Observed: &serviceObservation{Owner: "none"}}
		properties := map[string]map[string]string{}
		allExist := true
		anyExists := false
		for _, name := range names {
			_, exists, executable, roots, err := platform.selectedDefinition(ctx, name)
			if err != nil {
				return status, err
			}
			values, err := platform.selectedShow(ctx, name)
			if err != nil {
				return status, err
			}
			if err := platform.validateSelectedFragment(name, exists, values); err != nil {
				return status, err
			}
			allExist = allExist && exists
			anyExists = anyExists || exists
			if executable != "" {
				c.ConfiguredExecutable = executable
				c.Observed.Roots = roots
			}
			properties[name] = values
		}
		c.Registered = allExist
		if !allExist {
			// A partial timer/service pair is owned but cannot be treated as absent.
			if component == serviceRefresh {
				if anyExists {
					return status, installstate.ErrOwnershipConflict
				}
				for _, name := range names {
					if properties[name]["LoadState"] == "loaded" {
						return status, installstate.ErrOwnershipConflict
					}
				}
			}
			c.Observed.Enabled = serviceBool(false)
			c.Observed.Healthy = serviceBool(false)
			status.setComponent(component, c)
			continue
		}
		c.Observed.Owner = "cq"
		policy := properties[c.ID]["UnitFileState"]
		switch policy {
		case "enabled", "enabled-runtime":
			c.Observed.Enabled = serviceBool(true)
		case "disabled":
			c.Observed.Enabled = serviceBool(false)
		default:
			return status, errServiceUnavailable
		}
		job := properties[names[0]]
		c.Running = job["ActiveState"] == "active" || job["ActiveState"] == "activating" || job["ActiveState"] == "deactivating"
		if component == serviceProxy {
			if c.Running {
				var err error
				c.PID, err = strconv.Atoi(job["MainPID"])
				if err != nil || c.PID <= 0 {
					return status, errServiceUnavailable
				}
			}
			c.Observed.Healthy = serviceBool(false)
			if c.Running && c.Observed.Roots != nil && platform.inspectSelectedProxy != nil {
				runtime := platform.inspectSelectedProxy(ctx, c.ConfiguredExecutable, *c.Observed.Roots)
				if ctx.Err() != nil {
					return status, ctx.Err()
				}
				c.LiveExecutable, c.Listener, c.Error = runtime.LiveExecutable, runtime.Listener, runtime.Error
				c.Healthy = runtime.Running && runtime.Healthy && runtime.PID == c.PID && sameServiceExecutable(runtime.LiveExecutable, c.ConfiguredExecutable)
				c.Observed.Healthy = serviceBool(c.Healthy)
			}
		} else {
			if *c.Observed.Enabled && properties[systemdRefreshTimer]["ActiveState"] != "active" {
				c.Observed.ErrorCode = "service_timer_unhealthy"
			}
			completed, exit, err := systemdCompletion(job)
			if err != nil {
				c.Observed.ErrorCode = "service_completion_unavailable"
			}
			if c.Observed.Roots != nil {
				receipt, receiptErr := readServiceRefreshCompletion(c.ConfiguredExecutable, *c.Observed.Roots, time.Now())
				if receiptErr == nil {
					// Native exit evidence wins over an older retained receipt, including a
					// failed/signalled process which could not publish a new receipt.
					if completed == nil || receipt.CompletedAt.After(*completed) {
						completed, exit = &receipt.CompletedAt, &receipt.ExitCode
					}
				} else if !errors.Is(receiptErr, os.ErrNotExist) {
					c.Observed.ErrorCode = "service_completion_unavailable"
				}
			}
			c.Observed.LastRunAt, c.Observed.LastExitCode = completed, exit
		}
		status.setComponent(component, c)
	}
	return status, ctx.Err()
}
func systemdCompletion(properties map[string]string) (*time.Time, *int, error) {
	value := properties["ExecMainExitTimestamp"]
	if value == "" || value == "n/a" {
		if result := properties["Result"]; result != "" && result != "none" && result != "success" {
			return nil, nil, errServiceUnavailable
		}
		return nil, nil, nil
	}
	completed, err := time.Parse("Mon 2006-01-02 15:04:05.999999 MST", value)
	if err != nil || !strings.HasSuffix(value, " UTC") {
		return nil, nil, errServiceUnavailable
	}
	started, err := time.Parse("Mon 2006-01-02 15:04:05.999999 MST", properties["ExecMainStartTimestamp"])
	if err != nil || completed.After(time.Now()) {
		return nil, nil, errServiceUnavailable
	}
	if completed.Before(started) {
		if properties["ActiveState"] == "active" || properties["ActiveState"] == "activating" {
			return nil, nil, nil
		}
		return nil, nil, errServiceUnavailable
	}
	code, err := strconv.Atoi(properties["ExecMainCode"])
	if err != nil || code < 1 || code > 3 {
		return nil, nil, errServiceUnavailable
	}
	exit, err := strconv.Atoi(properties["ExecMainStatus"])
	if err != nil || exit < 0 {
		return nil, nil, errServiceUnavailable
	}
	if code != 1 {
		exit = 128 + exit
	}
	if properties["Result"] != "success" && exit == 0 {
		return nil, nil, errServiceUnavailable
	}
	return &completed, &exit, nil
}
func (platform *systemdServicePlatform) SnapshotSelected(ctx context.Context, selection serviceSelection) (servicePlatformSnapshot, error) {
	snapshot := servicePlatformSnapshot{Manager: "systemd-user"}
	for _, name := range systemdSelectedUnits(selection) {
		data, exists, _, _, err := platform.selectedDefinition(ctx, name)
		if err != nil {
			return snapshot, err
		}
		properties, err := platform.selectedShow(ctx, name)
		if err != nil {
			return snapshot, err
		}
		if err := platform.validateSelectedFragment(name, exists, properties); err != nil {
			return snapshot, err
		}
		c := serviceComponentSnapshot{ID: name, Exists: exists, Definition: data}
		if exists {
			c.UnitFileState = properties["UnitFileState"]
			if !supportedSystemdUnitFileState(c.UnitFileState) {
				return snapshot, installstate.ErrOwnershipConflict
			}
			switch properties["ActiveState"] {
			case "active", "activating":
				c.Running = true
			case "inactive", "failed":
			default:
				return snapshot, errServiceUnavailable
			}
		}
		snapshot.Components = append(snapshot.Components, c)
	}
	return snapshot, ctx.Err()
}
func (platform *systemdServicePlatform) installSelected(ctx context.Context, executable string, selection serviceSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSystemdOwnedExecutable(executable); err != nil {
		return err
	}
	definitions, err := renderSelectedSystemdDefinitions(executable, platform.home, platform.roots)
	if err != nil {
		return err
	}
	for _, name := range systemdSelectedUnits(selection) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := atomicWriteSystemdUnit(platform.unitPath(name), definitions[name]); err != nil {
			return err
		}
	}
	if _, err := platform.selectedRun(ctx, "daemon-reload"); err != nil {
		return err
	}
	if selection == serviceProxy {
		return platform.StartProxy(ctx)
	}
	return platform.StartRefresh(ctx)
}
func (platform *systemdServicePlatform) removeSelected(ctx context.Context, selection serviceSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if selection == serviceRefresh {
		if err := platform.StopRefresh(ctx); err != nil {
			return err
		}
	} else {
		if err := platform.StopProxy(ctx); err != nil {
			return err
		}
	}
	for _, name := range systemdSelectedUnits(selection) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(platform.unitPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := platform.selectedRun(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncSystemdDirectory(platform.unitDirectory)
}
func (platform *systemdServicePlatform) RestoreSelected(ctx context.Context, selection serviceSelection, snapshot servicePlatformSnapshot) error {
	names := systemdSelectedUnits(selection)
	if snapshot.Manager != "systemd-user" || snapshot.FolderExists || snapshot.FolderSecurityDescriptor != "" || len(snapshot.Components) != len(names) {
		return fmt.Errorf("invalid systemd service snapshot")
	}
	for i, c := range snapshot.Components {
		if c.ID != names[i] || c.Enabled || len(c.Definition) > maxSystemdUnitBytes || (!c.Exists && (len(c.Definition) != 0 || c.UnitFileState != "" || c.Running)) {
			return fmt.Errorf("invalid systemd service snapshot component")
		}
		if c.Exists {
			if !supportedSystemdUnitFileState(c.UnitFileState) {
				return installstate.ErrOwnershipConflict
			}
			if _, _, err := validateOwnedSystemdDefinition(c.ID, c.Definition); err != nil {
				return err
			}
		}
	}
	// Reverse native order stops the timer before its job. An install may have
	// written definitions before a failed reload; proven-unloaded units need no stop.
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		_, exists, _, _, err := platform.selectedDefinition(ctx, name)
		if err != nil {
			return err
		}
		properties, err := platform.selectedShow(ctx, name)
		if err != nil {
			return err
		}
		if properties["LoadState"] == "not-found" {
			continue
		}
		if _, ok := properties["DropInPaths"]; !ok {
			return errServiceUnavailable
		}
		// NeedDaemonReload is expected when restoring our own pending definition.
		if !exists || properties["LoadState"] != "loaded" || properties["FragmentPath"] != platform.unitPath(name) || properties["DropInPaths"] != "" {
			return installstate.ErrOwnershipConflict
		}
		if name == systemdRefreshService {
			if _, err := platform.selectedRun(ctx, "stop", name); err != nil {
				return err
			}
		} else if err := platform.disableSelected(ctx, name); err != nil {
			return err
		}
	}
	for _, c := range snapshot.Components {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.Exists {
			if err := atomicWriteSystemdUnit(platform.unitPath(c.ID), c.Definition); err != nil {
				return err
			}
		} else {
			if err := os.Remove(platform.unitPath(c.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if _, err := platform.selectedRun(ctx, "daemon-reload"); err != nil {
		return err
	}
	for _, c := range snapshot.Components {
		if !c.Exists {
			continue
		}
		switch c.UnitFileState {
		case "enabled":
			if _, err := platform.selectedRun(ctx, "enable", c.ID); err != nil {
				return err
			}
		case "enabled-runtime":
			if _, err := platform.selectedRun(ctx, "enable", "--runtime", c.ID); err != nil {
				return err
			}
		}
		if c.Running {
			args := []string{"start", c.ID}
			if c.ID == systemdRefreshService {
				args = []string{"start", "--no-block", c.ID}
			}
			if _, err := platform.selectedRun(ctx, args...); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(platform.unitDirectory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return syncSystemdDirectory(platform.unitDirectory)
}

// Discovery precedes selecting the ownership store and mutation lock roots.
func (platform *systemdServicePlatform) discoverSelected(ctx context.Context, selection serviceSelection) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateSystemdOwnedExecutable(platform.executable); err != nil {
		return false, err
	}
	var directory string
	var roots userdirs.Roots
	for _, name := range systemdSelectedUnits(selection) {
		properties, err := platform.selectedShow(ctx, name)
		if err != nil {
			return false, err
		}
		if properties["LoadState"] == "not-found" && properties["FragmentPath"] == "" {
			continue
		}
		fragment := properties["FragmentPath"]
		unitDir := filepath.Dir(fragment)
		if !filepath.IsAbs(fragment) || filepath.Clean(fragment) != fragment || filepath.Base(fragment) != name || filepath.Base(unitDir) != "user" || filepath.Base(filepath.Dir(unitDir)) != "systemd" {
			return false, installstate.ErrOwnershipConflict
		}
		candidate := *platform
		candidate.unitDirectory = unitDir
		_, exists, _, installedRoots, err := candidate.selectedDefinition(ctx, name)
		if err != nil {
			return false, err
		}
		if err := candidate.validateSelectedFragment(name, exists, properties); err != nil {
			return false, err
		}
		config := filepath.Join(filepath.Dir(filepath.Dir(unitDir)), "cq")
		state := filepath.Join(config, "state")
		if installedRoots != nil && (installedRoots.Config != config || installedRoots.State != state) {
			return false, installstate.ErrOwnershipConflict
		}
		if directory != "" && (directory != unitDir || roots.State != state) {
			return false, installstate.ErrOwnershipConflict
		}
		if installedRoots != nil {
			if roots.Config != "" && roots != *installedRoots {
				return false, installstate.ErrOwnershipConflict
			}
			roots = *installedRoots
		} else if roots.State == "" {
			// Frozen legacy layout proves only the ownership-state location.
			// It does not establish runtime/cache roots for status observation.
			roots.State = state
		}
		directory = unitDir
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if directory == "" {
		return false, nil
	}
	platform.unitDirectory, platform.roots = directory, roots
	return true, nil
}
