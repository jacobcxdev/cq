package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/installstate"
)

const (
	windowsProxyTaskPath      = `\cq\Proxy`
	windowsRefreshTaskPath    = `\cq\Refresh`
	windowsTaskXMLNS          = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	maxWindowsTaskXMLBytes    = 256 << 10
	windowsTaskNotFound       = uint32(0x80070002)
	windowsTaskAlreadyRunning = uint32(0x8004131f)
)

type windowsTaskKind uint8

const (
	windowsProxyTask windowsTaskKind = iota + 1
	windowsRefreshTask
)

type windowsTaskDefinition struct {
	XMLName          xml.Name                    `xml:"Task"`
	Version          string                      `xml:"version,attr"`
	XMLNS            string                      `xml:"xmlns,attr"`
	RegistrationInfo windowsTaskRegistrationInfo `xml:"RegistrationInfo"`
	Triggers         windowsTaskTriggers         `xml:"Triggers"`
	Principals       windowsTaskPrincipals       `xml:"Principals"`
	Settings         windowsTaskSettings         `xml:"Settings"`
	Actions          windowsTaskActions          `xml:"Actions"`
}

type windowsTaskRegistrationInfo struct {
	Author      string `xml:"Author"`
	Description string `xml:"Description"`
	URI         string `xml:"URI"`
}

type windowsTaskTriggers struct {
	LogonTrigger windowsTaskLogonTrigger `xml:"LogonTrigger"`
}

type windowsTaskLogonTrigger struct {
	Repetition *windowsTaskRepetition `xml:"Repetition,omitempty"`
	Enabled    bool                   `xml:"Enabled"`
	UserID     string                 `xml:"UserId"`
}

type windowsTaskRepetition struct {
	Interval          string `xml:"Interval"`
	StopAtDurationEnd bool   `xml:"StopAtDurationEnd"`
}

type windowsTaskPrincipals struct {
	Principal windowsTaskPrincipal `xml:"Principal"`
}

type windowsTaskPrincipal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type windowsTaskSettings struct {
	MultipleInstancesPolicy    string                    `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool                      `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool                      `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         bool                      `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool                      `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool                      `xml:"RunOnlyIfNetworkAvailable"`
	WakeToRun                  bool                      `xml:"WakeToRun"`
	Enabled                    bool                      `xml:"Enabled"`
	Hidden                     bool                      `xml:"Hidden"`
	ExecutionTimeLimit         string                    `xml:"ExecutionTimeLimit"`
	Priority                   int                       `xml:"Priority"`
	RestartOnFailure           *windowsTaskRestartPolicy `xml:"RestartOnFailure,omitempty"`
}

type windowsTaskRestartPolicy struct {
	Interval string `xml:"Interval"`
	Count    int    `xml:"Count"`
}

type windowsTaskActions struct {
	Context string          `xml:"Context,attr"`
	Exec    windowsTaskExec `xml:"Exec"`
}

type windowsTaskExec struct {
	Command          string `xml:"Command"`
	Arguments        string `xml:"Arguments"`
	WorkingDirectory string `xml:"WorkingDirectory"`
}

type windowsTaskRuntimeState struct {
	Running       bool
	LastResult    uint32
	HasLastResult bool
}

type windowsTaskCommandError struct {
	Code   uint32
	Output string
}

func (err windowsTaskCommandError) Error() string {
	if err.Output == "" {
		return fmt.Sprintf("schtasks failed with code 0x%08x", err.Code)
	}
	return fmt.Sprintf("schtasks failed with code 0x%08x: %s", err.Code, err.Output)
}

type windowsTaskServicePlatform struct {
	sid             string
	executable      string
	temporaryRoot   string
	refreshInterval int
	run             func(context.Context, ...string) ([]byte, error)
	queryState      func(context.Context, string) (windowsTaskRuntimeState, error)
	inspectProxy    func(context.Context, string) componentStatus
}

func (platform *windowsTaskServicePlatform) Preflight(ctx context.Context, executable string) error {
	if err := validateWindowsSID(platform.sid); err != nil {
		return err
	}
	if err := validateAbsoluteWindowsExecutable(executable); err != nil {
		return err
	}
	if platform.run == nil || platform.queryState == nil {
		return fmt.Errorf("Windows Task Scheduler is unavailable")
	}
	platform.executable = executable
	for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
		definition, exists, err := platform.queryDefinition(ctx, kind)
		if err != nil {
			return err
		}
		if exists {
			if err := validateWindowsTaskDefinition(definition, kind, platform.sid, executable, platform.interval()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (platform *windowsTaskServicePlatform) InstallProxy(ctx context.Context, executable string) error {
	return platform.reconcile(ctx, windowsProxyTask, executable)
}

func (platform *windowsTaskServicePlatform) InstallRefresh(ctx context.Context, executable string) error {
	return platform.reconcile(ctx, windowsRefreshTask, executable)
}

func (platform *windowsTaskServicePlatform) RestartProxy(ctx context.Context) error {
	return platform.runTask(ctx, windowsProxyTaskPath)
}

func (platform *windowsTaskServicePlatform) RestartRefresh(ctx context.Context) error {
	return platform.runTask(ctx, windowsRefreshTaskPath)
}

func (platform *windowsTaskServicePlatform) RemoveProxy(ctx context.Context) error {
	return platform.remove(ctx, windowsProxyTaskPath)
}

func (platform *windowsTaskServicePlatform) RemoveRefresh(ctx context.Context) error {
	return platform.remove(ctx, windowsRefreshTaskPath)
}

func (platform *windowsTaskServicePlatform) Inspect(ctx context.Context) (serviceStatus, error) {
	proxyStatus, err := platform.inspect(ctx, windowsProxyTask)
	if err != nil {
		return serviceStatus{}, err
	}
	refreshStatus, err := platform.inspect(ctx, windowsRefreshTask)
	if err != nil {
		return serviceStatus{}, err
	}
	return serviceStatus{Proxy: proxyStatus, Refresh: refreshStatus}, nil
}

func (platform *windowsTaskServicePlatform) reconcile(ctx context.Context, kind windowsTaskKind, executable string) error {
	oldDefinition, oldExists, err := platform.queryDefinitionBytes(ctx, kind)
	if err != nil {
		return err
	}
	taskPath, _, _, _ := windowsTaskValues(kind)
	oldState := windowsTaskRuntimeState{}
	if oldExists {
		oldState, err = platform.queryState(ctx, taskPath)
		if err != nil {
			return fmt.Errorf("inspect existing Windows task state: %w", err)
		}
	}
	definition, err := renderWindowsTaskDefinition(kind, platform.sid, executable, platform.interval())
	if err != nil {
		return err
	}
	if err := platform.createTask(ctx, taskPath, definition); err != nil {
		return errors.Join(fmt.Errorf("create Windows task %s: %w", taskPath, err), platform.restore(ctx, taskPath, oldDefinition, oldExists, oldState.Running))
	}
	if err := platform.runTask(ctx, taskPath); err != nil {
		return errors.Join(err, platform.restore(ctx, taskPath, oldDefinition, oldExists, oldState.Running))
	}
	return nil
}

func (platform *windowsTaskServicePlatform) restore(ctx context.Context, taskPath string, definition []byte, exists, running bool) error {
	var result error
	if _, err := platform.run(ctx, "/End", "/TN", taskPath); err != nil && !isWindowsTaskNotFound(err) {
		result = errors.Join(result, fmt.Errorf("end failed candidate task: %w", err))
	}
	if !exists {
		if _, err := platform.run(ctx, "/Delete", "/TN", taskPath, "/F"); err != nil && !isWindowsTaskNotFound(err) {
			result = errors.Join(result, fmt.Errorf("delete failed candidate task: %w", err))
		}
		return result
	}
	if err := platform.createTask(ctx, taskPath, definition); err != nil {
		return errors.Join(result, fmt.Errorf("restore previous Windows task: %w", err))
	}
	if running {
		if err := platform.runTask(ctx, taskPath); err != nil {
			result = errors.Join(result, fmt.Errorf("restart previous Windows task: %w", err))
		}
	}
	return result
}

func (platform *windowsTaskServicePlatform) createTask(ctx context.Context, taskPath string, definition []byte) error {
	path, cleanup, err := writeTemporaryWindowsTaskXML(platform.temporaryRoot, definition)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := platform.run(ctx, "/Create", "/TN", taskPath, "/XML", path, "/F"); err != nil {
		return err
	}
	return nil
}

func (platform *windowsTaskServicePlatform) runTask(ctx context.Context, taskPath string) error {
	if _, err := platform.run(ctx, "/Run", "/TN", taskPath); err != nil && !isWindowsTaskAlreadyRunning(err) {
		return fmt.Errorf("run Windows task %s: %w", taskPath, err)
	}
	return nil
}

func (platform *windowsTaskServicePlatform) remove(ctx context.Context, taskPath string) error {
	var result error
	if _, err := platform.run(ctx, "/End", "/TN", taskPath); err != nil && !isWindowsTaskNotFound(err) && !isWindowsTaskNotRunning(err) {
		result = errors.Join(result, fmt.Errorf("end Windows task %s: %w", taskPath, err))
	}
	if _, err := platform.run(ctx, "/Delete", "/TN", taskPath, "/F"); err != nil && !isWindowsTaskNotFound(err) {
		result = errors.Join(result, fmt.Errorf("delete Windows task %s: %w", taskPath, err))
	}
	return result
}

func (platform *windowsTaskServicePlatform) inspect(ctx context.Context, kind windowsTaskKind) (componentStatus, error) {
	taskPath, _, _, _ := windowsTaskValues(kind)
	status := componentStatus{ID: taskPath, Manager: "task-scheduler"}
	definition, exists, err := platform.queryDefinition(ctx, kind)
	if err != nil {
		return status, err
	}
	if !exists {
		return status, nil
	}
	if err := validateWindowsTaskDefinition(definition, kind, platform.sid, platform.executable, platform.interval()); err != nil {
		return status, err
	}
	status.Registered = true
	status.ConfiguredExecutable = definition.Actions.Exec.Command
	runtimeState, err := platform.queryState(ctx, taskPath)
	if err != nil {
		return status, fmt.Errorf("inspect Windows task state %s: %w", taskPath, err)
	}
	status.Running = runtimeState.Running
	if kind == windowsRefreshTask {
		if runtimeState.HasLastResult && runtimeState.LastResult == 0 {
			status.Healthy = true
			status.LastResult = "success"
		} else if runtimeState.HasLastResult {
			status.LastResult = "failed"
		}
		return status, nil
	}
	if status.Running && platform.inspectProxy != nil {
		runtimeStatus := platform.inspectProxy(ctx, status.ConfiguredExecutable)
		status.LiveExecutable = runtimeStatus.LiveExecutable
		status.PID = runtimeStatus.PID
		status.Listener = runtimeStatus.Listener
		status.Error = runtimeStatus.Error
		status.Healthy = runtimeStatus.Healthy && sameServiceExecutable(status.LiveExecutable, status.ConfiguredExecutable)
	}
	return status, nil
}

func (platform *windowsTaskServicePlatform) queryDefinition(ctx context.Context, kind windowsTaskKind) (windowsTaskDefinition, bool, error) {
	data, exists, err := platform.queryDefinitionBytes(ctx, kind)
	if err != nil || !exists {
		return windowsTaskDefinition{}, exists, err
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		return windowsTaskDefinition{}, false, fmt.Errorf("parse Windows task definition: %w", err)
	}
	return definition, true, nil
}

func (platform *windowsTaskServicePlatform) queryDefinitionBytes(ctx context.Context, kind windowsTaskKind) ([]byte, bool, error) {
	taskPath, _, _, err := windowsTaskValues(kind)
	if err != nil {
		return nil, false, err
	}
	data, err := platform.run(ctx, "/Query", "/TN", taskPath, "/XML")
	if err == nil {
		if len(data) > maxWindowsTaskXMLBytes {
			return nil, false, fmt.Errorf("Windows task XML exceeds size limit")
		}
		return data, true, nil
	}
	if isWindowsTaskNotFound(err) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("query Windows task %s: %w", taskPath, err)
}

func (platform *windowsTaskServicePlatform) interval() int {
	if platform.refreshInterval <= 0 {
		return 1800
	}
	return platform.refreshInterval
}

func writeTemporaryWindowsTaskXML(root string, definition []byte) (string, func(), error) {
	if len(definition) == 0 || len(definition) > maxWindowsTaskXMLBytes || root == "" {
		return "", func() {}, fmt.Errorf("invalid temporary Windows task XML")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create temporary task directory: %w", err)
	}
	file, err := os.CreateTemp(root, "cq-task-*.xml")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temporary task XML: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("seal temporary task XML: %w", err)
	}
	if _, err := file.Write(definition); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write temporary task XML: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("sync temporary task XML: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close temporary task XML: %w", err)
	}
	return path, cleanup, nil
}

func isWindowsTaskNotFound(err error) bool {
	var commandError windowsTaskCommandError
	return errors.As(err, &commandError) && commandError.Code == windowsTaskNotFound
}

func isWindowsTaskAlreadyRunning(err error) bool {
	var commandError windowsTaskCommandError
	return errors.As(err, &commandError) && commandError.Code == windowsTaskAlreadyRunning
}

func isWindowsTaskNotRunning(err error) bool {
	var commandError windowsTaskCommandError
	return errors.As(err, &commandError) && commandError.Code == uint32(0x8004130b)
}

var _ servicePlatform = (*windowsTaskServicePlatform)(nil)

func renderWindowsTaskDefinition(kind windowsTaskKind, sid, executable string, refreshInterval int) ([]byte, error) {
	if err := validateWindowsSID(sid); err != nil {
		return nil, err
	}
	if err := validateAbsoluteWindowsExecutable(executable); err != nil {
		return nil, err
	}
	if refreshInterval <= 0 {
		refreshInterval = 1800
	}
	taskPath, arguments, description, err := windowsTaskValues(kind)
	if err != nil {
		return nil, err
	}
	definition := windowsTaskDefinition{
		Version: "1.4",
		XMLNS:   windowsTaskXMLNS,
		RegistrationInfo: windowsTaskRegistrationInfo{
			Author:      "CQ",
			Description: description,
			URI:         taskPath,
		},
		Triggers: windowsTaskTriggers{LogonTrigger: windowsTaskLogonTrigger{
			Enabled: true,
			UserID:  sid,
		}},
		Principals: windowsTaskPrincipals{Principal: windowsTaskPrincipal{
			ID:        "CQUser",
			UserID:    sid,
			LogonType: "InteractiveToken",
			RunLevel:  "LeastPrivilege",
		}},
		Settings: windowsTaskSettings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			AllowHardTerminate:         true,
			StartWhenAvailable:         true,
			RunOnlyIfNetworkAvailable:  false,
			WakeToRun:                  false,
			Enabled:                    true,
			Hidden:                     false,
			ExecutionTimeLimit:         "PT5M",
			Priority:                   7,
		},
		Actions: windowsTaskActions{
			Context: "CQUser",
			Exec: windowsTaskExec{
				Command:          executable,
				Arguments:        arguments,
				WorkingDirectory: windowsDirectory(executable),
			},
		},
	}
	if kind == windowsProxyTask {
		definition.Settings.ExecutionTimeLimit = "PT0S"
		definition.Settings.RestartOnFailure = &windowsTaskRestartPolicy{Interval: "PT5S", Count: 999}
	} else {
		definition.Triggers.LogonTrigger.Repetition = &windowsTaskRepetition{
			Interval:          formatWindowsDuration(refreshInterval),
			StopAtDurationEnd: false,
		}
	}
	encoded, err := xml.MarshalIndent(definition, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Windows task XML: %w", err)
	}
	result := append([]byte(xml.Header), encoded...)
	result = append(result, '\n')
	return result, nil
}

func parseWindowsTaskDefinition(data []byte) (windowsTaskDefinition, error) {
	if len(data) == 0 || len(data) > maxWindowsTaskXMLBytes {
		return windowsTaskDefinition{}, fmt.Errorf("Windows task XML has invalid size")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var definition windowsTaskDefinition
	if err := decoder.Decode(&definition); err != nil {
		return windowsTaskDefinition{}, fmt.Errorf("decode Windows task XML: %w", err)
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return windowsTaskDefinition{}, fmt.Errorf("decode trailing Windows task XML: %w", err)
		}
		if characters, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(characters)) != "" {
			return windowsTaskDefinition{}, fmt.Errorf("Windows task XML has trailing content")
		}
	}
	return definition, nil
}

func validateWindowsTaskDefinition(definition windowsTaskDefinition, kind windowsTaskKind, sid, executable string, refreshInterval int) error {
	if refreshInterval <= 0 {
		refreshInterval = 1800
	}
	taskPath, arguments, _, err := windowsTaskValues(kind)
	if err != nil {
		return err
	}
	conflict := func(reason string) error {
		return fmt.Errorf("%w: Windows task %s", installstate.ErrOwnershipConflict, reason)
	}
	if definition.XMLName.Local != "Task" || definition.XMLName.Space != windowsTaskXMLNS || definition.Version != "1.4" {
		return conflict("schema differs")
	}
	if definition.RegistrationInfo.URI != taskPath {
		return conflict("URI differs")
	}
	trigger := definition.Triggers.LogonTrigger
	principal := definition.Principals.Principal
	if !trigger.Enabled || trigger.UserID != sid || principal.ID != "CQUser" || principal.UserID != sid || principal.LogonType != "InteractiveToken" || principal.RunLevel != "LeastPrivilege" {
		return conflict("principal or logon trigger differs")
	}
	if !definition.Settings.Enabled || definition.Settings.MultipleInstancesPolicy != "IgnoreNew" || !definition.Settings.StartWhenAvailable {
		return conflict("settings differ")
	}
	action := definition.Actions.Exec
	if definition.Actions.Context != "CQUser" || !equalWindowsPath(action.Command, executable) || action.Arguments != arguments || !equalWindowsPath(action.WorkingDirectory, windowsDirectory(executable)) {
		return conflict("action differs")
	}
	if kind == windowsProxyTask {
		if trigger.Repetition != nil || definition.Settings.ExecutionTimeLimit != "PT0S" || definition.Settings.RestartOnFailure == nil || definition.Settings.RestartOnFailure.Interval != "PT5S" || definition.Settings.RestartOnFailure.Count != 999 {
			return conflict("proxy lifetime policy differs")
		}
	} else if trigger.Repetition == nil || trigger.Repetition.Interval != formatWindowsDuration(refreshInterval) || definition.Settings.ExecutionTimeLimit != "PT5M" {
		return conflict("refresh repetition differs")
	}
	return nil
}

func windowsTaskValues(kind windowsTaskKind) (taskPath, arguments, description string, err error) {
	switch kind {
	case windowsProxyTask:
		return windowsProxyTaskPath, "proxy start", "Runs CQ local proxy", nil
	case windowsRefreshTask:
		return windowsRefreshTaskPath, "refresh", "Refreshes CQ credentials", nil
	default:
		return "", "", "", fmt.Errorf("unknown Windows task kind %d", kind)
	}
}

func formatWindowsDuration(seconds int) string {
	if seconds <= 0 {
		seconds = 1800
	}
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60
	var duration strings.Builder
	duration.WriteString("PT")
	if hours > 0 {
		duration.WriteString(strconv.Itoa(hours))
		duration.WriteByte('H')
	}
	if minutes > 0 {
		duration.WriteString(strconv.Itoa(minutes))
		duration.WriteByte('M')
	}
	if seconds > 0 || hours == 0 && minutes == 0 {
		duration.WriteString(strconv.Itoa(seconds))
		duration.WriteByte('S')
	}
	return duration.String()
}

func validateWindowsSID(sid string) error {
	if !strings.HasPrefix(sid, "S-") || len(sid) > 184 {
		return fmt.Errorf("invalid Windows user SID")
	}
	for _, character := range sid[2:] {
		if character != '-' && !unicode.IsDigit(character) {
			return fmt.Errorf("invalid Windows user SID")
		}
	}
	return nil
}

func validateAbsoluteWindowsExecutable(path string) error {
	normalised := strings.ReplaceAll(path, "/", `\`)
	if len(normalised) < 4 || !unicode.IsLetter(rune(normalised[0])) || normalised[1] != ':' || normalised[2] != '\\' {
		return fmt.Errorf("Windows task executable must be an absolute drive path")
	}
	parts := strings.Split(normalised[3:], `\`)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("Windows task executable is not a clean absolute path")
		}
	}
	if !strings.EqualFold(filepath.Ext(normalised), ".exe") {
		return fmt.Errorf("Windows task executable must end in .exe")
	}
	return nil
}

func windowsDirectory(path string) string {
	normalised := strings.ReplaceAll(path, "/", `\`)
	if index := strings.LastIndex(normalised, `\`); index > 2 {
		return normalised[:index]
	}
	return normalised[:3]
}

func equalWindowsPath(left, right string) bool {
	return strings.EqualFold(strings.ReplaceAll(left, "/", `\`), strings.ReplaceAll(right, "/", `\`))
}
