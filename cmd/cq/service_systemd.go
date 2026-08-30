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
	unitDirectory string
	executable    string
	run           func(context.Context, ...string) ([]byte, error)
	inspectProxy  func(context.Context, string) componentStatus
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

func (platform *systemdServicePlatform) InstallProxy(_ context.Context, executable string) error {
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		return err
	}
	if err := atomicWriteSystemdUnit(platform.unitPath(systemdProxyUnit), definitions[systemdProxyUnit]); err != nil {
		return fmt.Errorf("write %s: %w", systemdProxyUnit, err)
	}
	return nil
}

func (platform *systemdServicePlatform) InstallRefresh(ctx context.Context, executable string) error {
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
	if _, err := platform.run(ctx, "--user", "enable", "--now", systemdProxyUnit, systemdRefreshTimer); err != nil {
		return fmt.Errorf("enable CQ systemd units: %w", err)
	}
	if _, err := platform.run(ctx, "--user", "start", systemdRefreshService); err != nil {
		return fmt.Errorf("run initial credential refresh: %w", err)
	}
	return nil
}

func (platform *systemdServicePlatform) installProxyOnly(ctx context.Context, executable string) error {
	if err := platform.InstallProxy(ctx, executable); err != nil {
		return err
	}
	if _, err := platform.run(ctx, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("reload user systemd manager: %w", err)
	}
	if _, err := platform.run(ctx, "--user", "enable", "--now", systemdProxyUnit); err != nil {
		return fmt.Errorf("enable %s: %w", systemdProxyUnit, err)
	}
	return nil
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
	if _, err := platform.run(ctx, "--user", "restart", systemdProxyUnit); err != nil {
		return fmt.Errorf("restart %s: %w", systemdProxyUnit, err)
	}
	return nil
}

func (platform *systemdServicePlatform) RestartRefresh(ctx context.Context) error {
	if _, err := platform.run(ctx, "--user", "restart", systemdRefreshService); err != nil {
		return fmt.Errorf("restart %s: %w", systemdRefreshService, err)
	}
	return nil
}

func (platform *systemdServicePlatform) RemoveProxy(ctx context.Context) error {
	return platform.remove(ctx, []string{systemdProxyUnit}, []string{systemdProxyUnit})
}

func (platform *systemdServicePlatform) RemoveRefresh(ctx context.Context) error {
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
