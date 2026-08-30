//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

const (
	darwinRefreshInterval = 1800
	maxDarwinPlistBytes   = 64 << 10
)

type darwinLaunchAgentDefinition struct {
	Label             string
	ProgramArguments  []string
	RunAtLoad         bool
	KeepAlive         bool
	StartInterval     int
	StandardErrorPath string
}

type darwinServicePlatform struct {
	home            string
	roots           userdirs.Roots
	uid             int
	executable      string
	run             func(context.Context, ...string) ([]byte, error)
	inspectProxy    func(context.Context, string) componentStatus
	initialiseProxy func() error
}

func init() {
	serviceLifecycleFactory = defaultDarwinServiceLifecycle
}

func defaultDarwinServiceLifecycle(stableExecutable string) (*serviceLifecycle, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	executable, err := resolveServiceExecutable(stableExecutable)
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute executable: %w", err)
	}
	executable = filepath.Clean(executable)
	platform := &darwinServicePlatform{
		home:       home,
		roots:      roots,
		uid:        os.Getuid(),
		executable: executable,
		run: func(ctx context.Context, args ...string) ([]byte, error) {
			output, runErr := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
			if runErr != nil {
				message := strings.TrimSpace(string(output))
				if message != "" {
					return output, fmt.Errorf("launchctl %s: %w: %s", args[0], runErr, message)
				}
				return output, fmt.Errorf("launchctl %s: %w", args[0], runErr)
			}
			return output, nil
		},
		inspectProxy:    inspectDarwinProxyRuntime,
		initialiseProxy: initialiseDarwinRuntimeLifecycle,
	}
	store := &installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
	return &serviceLifecycle{
		Platform:       platform,
		Store:          store,
		Executable:     executable,
		Version:        version,
		StatusAttempts: 20,
		StatusInterval: time.Second,
		MutationLocker: installer.FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: roots.State},
	}, nil
}

func newDarwinCommandServicePlatform(home string, roots userdirs.Roots, executable string) *darwinServicePlatform {
	return &darwinServicePlatform{
		home:       home,
		roots:      roots,
		uid:        os.Getuid(),
		executable: executable,
		run: func(_ context.Context, args ...string) ([]byte, error) {
			return nil, runProxyLaunchctl(args...)
		},
		initialiseProxy: initialiseDarwinRuntimeLifecycle,
	}
}

func (platform *darwinServicePlatform) Preflight(ctx context.Context, executable string) error {
	if err := validateDarwinServiceExecutable(executable); err != nil {
		return err
	}
	platform.executable = executable
	loaded, _, err := platform.printJob(ctx, homebrewProxyAgentLabel)
	if err != nil {
		return err
	}
	if loaded {
		return fmt.Errorf("%w: legacy Homebrew job %q is loaded", installstate.ErrOwnershipConflict, homebrewProxyAgentLabel)
	}

	checks := []struct {
		label string
		args  []string
	}{
		{label: proxyAgentLabel, args: []string{executable, "proxy", "start"}},
		{label: agentLabel, args: []string{executable, "refresh"}},
	}
	for _, check := range checks {
		definition, exists, readErr := platform.readDefinition(check.label)
		if readErr != nil {
			return fmt.Errorf("inspect %s definition: %w", check.label, readErr)
		}
		if exists && (definition.Label != check.label || !equalStrings(definition.ProgramArguments, check.args)) {
			return fmt.Errorf("%w: %s uses different executable or arguments", installstate.ErrOwnershipConflict, check.label)
		}
		loaded, _, printErr := platform.printJob(ctx, check.label)
		if printErr != nil {
			return printErr
		}
		if loaded && !exists {
			return fmt.Errorf("%w: %s is loaded without managed definition", installstate.ErrOwnershipConflict, check.label)
		}
	}
	return nil
}

type darwinServiceSnapshot struct {
	label  string
	path   string
	data   []byte
	exists bool
	loaded bool
}

func (platform *darwinServicePlatform) PrepareRollback(ctx context.Context) (serviceRestore, error) {
	snapshots := make([]darwinServiceSnapshot, 0, 2)
	for _, label := range []string{proxyAgentLabel, agentLabel} {
		path := platform.plistPath(label)
		data, err := os.ReadFile(path)
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("snapshot %s definition: %w", label, err)
		}
		if len(data) > maxDarwinPlistBytes {
			return nil, fmt.Errorf("snapshot %s definition exceeds size limit", label)
		}
		loaded, _, err := platform.printJob(ctx, label)
		if err != nil {
			return nil, err
		}
		if loaded && !exists {
			return nil, fmt.Errorf("%w: %s is loaded without managed definition", installstate.ErrOwnershipConflict, label)
		}
		snapshots = append(snapshots, darwinServiceSnapshot{label: label, path: path, data: append([]byte(nil), data...), exists: exists, loaded: loaded})
	}
	return func(restoreCtx context.Context) error {
		var result error
		for index := len(snapshots) - 1; index >= 0; index-- {
			snapshot := snapshots[index]
			result = errors.Join(result, platform.restore(restoreCtx, snapshot.label, snapshot.path, snapshot.data, snapshot.exists, snapshot.loaded))
		}
		return result
	}, nil
}

func (platform *darwinServicePlatform) InstallProxy(ctx context.Context, executable string) error {
	if platform.initialiseProxy != nil {
		if err := platform.initialiseProxy(); err != nil {
			return fmt.Errorf("initialise proxy runtime: %w", err)
		}
	}
	definition := darwinLaunchAgentDefinition{
		Label:             proxyAgentLabel,
		ProgramArguments:  []string{executable, "proxy", "start"},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardErrorPath: filepath.Join(platform.roots.Logs, "proxy.log"),
	}
	return platform.reconcile(ctx, definition)
}

func (platform *darwinServicePlatform) InstallRefresh(ctx context.Context, executable string) error {
	return platform.installRefresh(ctx, executable, darwinRefreshInterval)
}

func (platform *darwinServicePlatform) installRefresh(ctx context.Context, executable string, interval int) error {
	if interval <= 0 {
		interval = darwinRefreshInterval
	}
	definition := darwinLaunchAgentDefinition{
		Label:             agentLabel,
		ProgramArguments:  []string{executable, "refresh"},
		RunAtLoad:         true,
		StartInterval:     interval,
		StandardErrorPath: filepath.Join(platform.roots.Logs, "refresh.log"),
	}
	return platform.reconcile(ctx, definition)
}

func (platform *darwinServicePlatform) RestartProxy(ctx context.Context) error {
	return platform.kickstart(ctx, proxyAgentLabel)
}

func (platform *darwinServicePlatform) RestartRefresh(ctx context.Context) error {
	return platform.kickstart(ctx, agentLabel)
}

func (platform *darwinServicePlatform) RemoveProxy(ctx context.Context) error {
	return platform.remove(ctx, proxyAgentLabel)
}

func (platform *darwinServicePlatform) RemoveRefresh(ctx context.Context) error {
	return platform.remove(ctx, agentLabel)
}

func (platform *darwinServicePlatform) Inspect(ctx context.Context) (serviceStatus, error) {
	proxyStatus, err := platform.inspectProxyComponent(ctx)
	if err != nil {
		return serviceStatus{}, err
	}
	refreshStatus, err := platform.inspectRefreshComponent(ctx)
	if err != nil {
		return serviceStatus{}, err
	}
	return serviceStatus{Proxy: proxyStatus, Refresh: refreshStatus}, nil
}

func (platform *darwinServicePlatform) inspectProxyComponent(ctx context.Context) (componentStatus, error) {
	status := componentStatus{ID: proxyAgentLabel, Manager: "launchd"}
	definition, exists, err := platform.readDefinition(proxyAgentLabel)
	if err != nil {
		return status, fmt.Errorf("inspect proxy definition: %w", err)
	}
	status.Registered = exists
	if exists && len(definition.ProgramArguments) > 0 {
		status.ConfiguredExecutable = definition.ProgramArguments[0]
	}
	loaded, _, err := platform.printJob(ctx, proxyAgentLabel)
	if err != nil {
		return status, err
	}
	if !loaded || !exists {
		return status, nil
	}
	if platform.inspectProxy == nil {
		return status, nil
	}
	runtimeStatus := platform.inspectProxy(ctx, status.ConfiguredExecutable)
	runtimeStatus.ID = proxyAgentLabel
	runtimeStatus.Manager = "launchd"
	runtimeStatus.Registered = true
	runtimeStatus.ConfiguredExecutable = status.ConfiguredExecutable
	return runtimeStatus, nil
}

func (platform *darwinServicePlatform) inspectRefreshComponent(ctx context.Context) (componentStatus, error) {
	status := componentStatus{ID: agentLabel, Manager: "launchd"}
	definition, exists, err := platform.readDefinition(agentLabel)
	if err != nil {
		return status, fmt.Errorf("inspect refresh definition: %w", err)
	}
	status.Registered = exists
	if exists && len(definition.ProgramArguments) > 0 {
		status.ConfiguredExecutable = definition.ProgramArguments[0]
	}
	loaded, output, err := platform.printJob(ctx, agentLabel)
	if err != nil {
		return status, err
	}
	if !loaded || !exists {
		return status, nil
	}
	state, runs, lastExit, err := parseDarwinLaunchctlStatus(output)
	if err != nil {
		status.Error = err.Error()
		return status, nil
	}
	status.Running = state == "running"
	if runs > 0 && lastExit == 0 {
		status.Healthy = true
		status.LastResult = "success"
	} else if runs > 0 {
		status.LastResult = "failed"
	}
	return status, nil
}

func (platform *darwinServicePlatform) reconcile(ctx context.Context, definition darwinLaunchAgentDefinition) error {
	if err := os.MkdirAll(platform.roots.Logs, 0o700); err != nil {
		return fmt.Errorf("create service log directory: %w", err)
	}
	data, err := renderDarwinLaunchAgent(definition)
	if err != nil {
		return err
	}
	path := platform.plistPath(definition.Label)
	oldData, readErr := os.ReadFile(path)
	oldExists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read existing LaunchAgent: %w", readErr)
	}
	oldLoaded := false
	if _, err := platform.run(ctx, "bootout", platform.target(definition.Label)); err == nil {
		oldLoaded = true
	} else if !isDarwinLaunchctlNotLoaded(err) {
		return fmt.Errorf("boot out %s: %w", definition.Label, err)
	}

	if err := atomicWriteDarwinLaunchAgent(path, data); err != nil {
		return errors.Join(fmt.Errorf("write %s definition: %w", definition.Label, err), platform.restore(ctx, definition.Label, path, oldData, oldExists, oldLoaded))
	}
	if _, err := platform.run(ctx, "bootstrap", platform.domain(), path); err != nil {
		return errors.Join(fmt.Errorf("bootstrap %s: %w", definition.Label, err), platform.restore(ctx, definition.Label, path, oldData, oldExists, oldLoaded))
	}
	if err := platform.kickstart(ctx, definition.Label); err != nil {
		return errors.Join(err, platform.restore(ctx, definition.Label, path, oldData, oldExists, oldLoaded))
	}
	return nil
}

func (platform *darwinServicePlatform) restore(ctx context.Context, label, path string, data []byte, exists, loaded bool) error {
	var restoreErr error
	if _, err := platform.run(ctx, "bootout", platform.target(label)); err != nil && !isDarwinLaunchctlNotLoaded(err) {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("boot out failed candidate: %w", err))
	}
	if exists {
		if err := atomicWriteDarwinLaunchAgent(path, data); err != nil {
			return errors.Join(restoreErr, fmt.Errorf("restore previous definition: %w", err))
		}
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(restoreErr, fmt.Errorf("remove failed definition: %w", err))
	}
	if loaded && exists {
		if _, err := platform.run(ctx, "bootstrap", platform.domain(), path); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore previous job: %w", err))
		} else if err := platform.kickstart(ctx, label); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restart previous job: %w", err))
		}
	}
	return restoreErr
}

func (platform *darwinServicePlatform) remove(ctx context.Context, label string) error {
	var result error
	if _, err := platform.run(ctx, "bootout", platform.target(label)); err != nil && !isDarwinLaunchctlNotLoaded(err) {
		result = errors.Join(result, fmt.Errorf("boot out %s: %w", label, err))
	}
	path := platform.plistPath(label)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, fmt.Errorf("remove %s definition: %w", label, err))
	} else if err == nil {
		if syncErr := syncDarwinDirectory(filepath.Dir(path)); syncErr != nil {
			result = errors.Join(result, fmt.Errorf("sync LaunchAgents directory: %w", syncErr))
		}
	}
	return result
}

func (platform *darwinServicePlatform) kickstart(ctx context.Context, label string) error {
	if _, err := platform.run(ctx, "kickstart", "-k", platform.target(label)); err != nil {
		return fmt.Errorf("kickstart %s: %w", label, err)
	}
	return nil
}

func (platform *darwinServicePlatform) printJob(ctx context.Context, label string) (bool, []byte, error) {
	output, err := platform.run(ctx, "print", platform.target(label))
	if err == nil {
		return true, output, nil
	}
	if isDarwinLaunchctlNotLoaded(err) {
		return false, nil, nil
	}
	return false, nil, fmt.Errorf("inspect launchd job %s: %w", label, err)
}

func (platform *darwinServicePlatform) readDefinition(label string) (darwinLaunchAgentDefinition, bool, error) {
	data, err := os.ReadFile(platform.plistPath(label))
	if errors.Is(err, os.ErrNotExist) {
		return darwinLaunchAgentDefinition{}, false, nil
	}
	if err != nil {
		return darwinLaunchAgentDefinition{}, false, err
	}
	definition, err := parseDarwinLaunchAgent(data)
	return definition, true, err
}

func (platform *darwinServicePlatform) plistPath(label string) string {
	return filepath.Join(platform.home, "Library", "LaunchAgents", label+".plist")
}

func (platform *darwinServicePlatform) domain() string {
	return fmt.Sprintf("gui/%d", platform.uid)
}

func (platform *darwinServicePlatform) target(label string) string {
	return platform.domain() + "/" + label
}

func renderDarwinLaunchAgent(definition darwinLaunchAgentDefinition) ([]byte, error) {
	if definition.Label == "" || len(definition.ProgramArguments) == 0 || definition.StandardErrorPath == "" {
		return nil, fmt.Errorf("invalid LaunchAgent definition")
	}
	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	output.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	output.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writeDarwinString(&output, "Label", definition.Label)
	output.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, argument := range definition.ProgramArguments {
		output.WriteString("\t\t<string>")
		if err := xml.EscapeText(&output, []byte(argument)); err != nil {
			return nil, err
		}
		output.WriteString("</string>\n")
	}
	output.WriteString("\t</array>\n")
	if definition.KeepAlive {
		writeDarwinBool(&output, "KeepAlive", true)
	}
	if definition.StartInterval > 0 {
		output.WriteString("\t<key>StartInterval</key>\n\t<integer>")
		output.WriteString(strconv.Itoa(definition.StartInterval))
		output.WriteString("</integer>\n")
	}
	writeDarwinBool(&output, "RunAtLoad", definition.RunAtLoad)
	writeDarwinString(&output, "ProcessType", "Background")
	writeDarwinString(&output, "StandardErrorPath", definition.StandardErrorPath)
	output.WriteString("</dict>\n</plist>\n")
	return output.Bytes(), nil
}

func writeDarwinString(output *bytes.Buffer, key, value string) {
	output.WriteString("\t<key>")
	_ = xml.EscapeText(output, []byte(key))
	output.WriteString("</key>\n\t<string>")
	_ = xml.EscapeText(output, []byte(value))
	output.WriteString("</string>\n")
}

func writeDarwinBool(output *bytes.Buffer, key string, value bool) {
	output.WriteString("\t<key>")
	_ = xml.EscapeText(output, []byte(key))
	output.WriteString("</key>\n\t<")
	if !value {
		output.WriteString("false")
	} else {
		output.WriteString("true")
	}
	output.WriteString("/>\n")
}

func parseDarwinLaunchAgent(data []byte) (darwinLaunchAgentDefinition, error) {
	if len(data) > maxDarwinPlistBytes {
		return darwinLaunchAgentDefinition{}, fmt.Errorf("LaunchAgent definition exceeds %d bytes", maxDarwinPlistBytes)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return darwinLaunchAgentDefinition{}, fmt.Errorf("decode plist: %w", err)
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "dict" {
			values, err := parseDarwinPlistDict(decoder)
			if err != nil {
				return darwinLaunchAgentDefinition{}, err
			}
			return darwinDefinitionFromValues(values)
		}
	}
}

func parseDarwinPlistDict(decoder *xml.Decoder) (map[string]any, error) {
	values := make(map[string]any)
	for {
		token, err := nextDarwinXMLToken(decoder)
		if err != nil {
			return nil, err
		}
		if end, ok := token.(xml.EndElement); ok && end.Name.Local == "dict" {
			return values, nil
		}
		keyStart, ok := token.(xml.StartElement)
		if !ok || keyStart.Name.Local != "key" {
			return nil, fmt.Errorf("expected plist key")
		}
		var key string
		if err := decoder.DecodeElement(&key, &keyStart); err != nil {
			return nil, fmt.Errorf("decode plist key: %w", err)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate plist key %q", key)
		}
		valueToken, err := nextDarwinXMLToken(decoder)
		if err != nil {
			return nil, err
		}
		valueStart, ok := valueToken.(xml.StartElement)
		if !ok {
			return nil, fmt.Errorf("expected plist value for %q", key)
		}
		value, err := parseDarwinPlistValue(decoder, valueStart)
		if err != nil {
			return nil, fmt.Errorf("decode plist key %q: %w", key, err)
		}
		values[key] = value
	}
}

func parseDarwinPlistValue(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string":
		var value string
		return value, decoder.DecodeElement(&value, &start)
	case "integer":
		var text string
		if err := decoder.DecodeElement(&text, &start); err != nil {
			return nil, err
		}
		return strconv.Atoi(strings.TrimSpace(text))
	case "true", "false":
		value := start.Name.Local == "true"
		var discard struct{}
		return value, decoder.DecodeElement(&discard, &start)
	case "array":
		var values []string
		for {
			token, err := nextDarwinXMLToken(decoder)
			if err != nil {
				return nil, err
			}
			if end, ok := token.(xml.EndElement); ok && end.Name.Local == "array" {
				return values, nil
			}
			item, ok := token.(xml.StartElement)
			if !ok || item.Name.Local != "string" {
				return nil, fmt.Errorf("array contains non-string value")
			}
			var value string
			if err := decoder.DecodeElement(&value, &item); err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	default:
		return nil, fmt.Errorf("unsupported plist value %q", start.Name.Local)
	}
}

func nextDarwinXMLToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if characters, ok := token.(xml.CharData); ok && strings.TrimSpace(string(characters)) == "" {
			continue
		}
		return token, nil
	}
}

func darwinDefinitionFromValues(values map[string]any) (darwinLaunchAgentDefinition, error) {
	definition := darwinLaunchAgentDefinition{}
	var ok bool
	definition.Label, ok = values["Label"].(string)
	if !ok || definition.Label == "" {
		return definition, fmt.Errorf("plist Label is missing")
	}
	definition.ProgramArguments, ok = values["ProgramArguments"].([]string)
	if !ok || len(definition.ProgramArguments) == 0 {
		return definition, fmt.Errorf("plist ProgramArguments are missing")
	}
	definition.RunAtLoad, _ = values["RunAtLoad"].(bool)
	definition.KeepAlive, _ = values["KeepAlive"].(bool)
	definition.StartInterval, _ = values["StartInterval"].(int)
	definition.StandardErrorPath, _ = values["StandardErrorPath"].(string)
	return definition, nil
}

func atomicWriteDarwinLaunchAgent(path string, data []byte) (returnErr error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("LaunchAgent path must be a clean absolute path")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect LaunchAgents directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("LaunchAgents directory is not a private directory")
	}
	inspector := fsutil.OSFileSystem{}
	owner, ok := inspector.FileOwnerUID(info)
	if !ok || owner != inspector.EffectiveUID() {
		return fmt.Errorf("LaunchAgents directory is not owned by current user")
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary LaunchAgent: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("seal temporary LaunchAgent: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary LaunchAgent: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary LaunchAgent: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary LaunchAgent: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit LaunchAgent: %w", err)
	}
	if err := syncDarwinDirectory(directory); err != nil {
		return fmt.Errorf("sync LaunchAgents directory: %w", err)
	}
	return nil
}

func syncDarwinDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateDarwinServiceExecutable(path string) error {
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

func isDarwinLaunchctlNotLoaded(err error) bool {
	var exitError interface{ ExitCode() int }
	return errors.As(err, &exitError) && (exitError.ExitCode() == 3 || exitError.ExitCode() == 113)
}

func parseDarwinLaunchctlStatus(output []byte) (state string, runs int, lastExit int, returnErr error) {
	lastExit = -1
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), " = ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "state":
			state = parts[1]
		case "runs":
			runs, returnErr = strconv.Atoi(parts[1])
			if returnErr != nil {
				return "", 0, -1, fmt.Errorf("invalid launchd runs")
			}
		case "last exit code":
			lastExit, returnErr = strconv.Atoi(parts[1])
			if returnErr != nil {
				return "", 0, -1, fmt.Errorf("invalid launchd exit code")
			}
		}
	}
	if state == "" || lastExit < 0 {
		return "", 0, -1, fmt.Errorf("incomplete launchd status")
	}
	return state, runs, lastExit, nil
}

func inspectDarwinProxyRuntime(ctx context.Context, executable string) componentStatus {
	status := componentStatus{ID: proxyAgentLabel, Manager: "launchd", Registered: true, ConfiguredExecutable: executable}
	facts := collectDarwinProxyInspectionFacts(ctx, "")
	if facts.service.Status == proxy.FactKnown && facts.service.Value != nil {
		status.Running = facts.service.Value.State == "running"
		status.PID = facts.service.Value.PID
		status.LiveExecutable = facts.service.Value.Executable
	}
	if facts.listener.Status == proxy.FactKnown && facts.listener.Value != nil {
		status.Listener = facts.listener.Value.Listener
	}
	if facts.runtime.Status == proxy.FactKnown && facts.runtime.Value != nil && status.LiveExecutable == "" {
		status.LiveExecutable = facts.runtime.Value.Executable
	}
	status.Healthy = status.Running && status.Listener != "" && facts.runtime.Status == proxy.FactKnown && facts.runtime.Value != nil && facts.runtime.Value.Reachable && facts.runtime.Value.Health == "healthy" && sameDarwinExecutable(status.LiveExecutable, executable)
	return status
}

func sameDarwinExecutable(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && resolvedLeft == resolvedRight
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

var _ servicePlatform = (*darwinServicePlatform)(nil)
