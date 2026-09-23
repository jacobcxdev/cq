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
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const (
	windowsTaskFolderName       = "cq"
	windowsTaskFolderPath       = `\cq`
	windowsProxyTaskPath        = `\cq\Proxy`
	windowsRefreshTaskPath      = `\cq\Refresh`
	windowsProxySessionTaskPath = `\cq\ProxySession`
	windowsTaskXMLNS            = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	maxWindowsTaskXMLBytes      = 256 << 10
	windowsTaskNotFound         = uint32(0x80070002)
	windowsTaskAlreadyRunning   = uint32(0x8004131f)
	windowsTaskStopAttempts     = 100
	windowsTaskStopInterval     = 100 * time.Millisecond
)

type windowsTaskKind uint8

const (
	windowsProxyTask windowsTaskKind = iota + 1
	windowsRefreshTask
	windowsProxySessionTask
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
	Author             string `xml:"Author"`
	Description        string `xml:"Description"`
	URI                string `xml:"URI"`
	SecurityDescriptor string `xml:"SecurityDescriptor"`
}

type windowsTaskTriggers struct {
	LogonTrigger windowsTaskLogonTrigger `xml:"LogonTrigger"`
}

type windowsTaskLogonTrigger struct {
	Repetition *windowsTaskRepetition `xml:"Repetition,omitempty"`
	Enabled    *bool                  `xml:"Enabled,omitempty"`
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
	Enabled                    *bool                     `xml:"Enabled,omitempty"`
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
	Running            bool
	LastResult         uint32
	HasLastResult      bool
	EnginePIDs         []uint32
	SecurityDescriptor string
}

type windowsTaskFolderState struct {
	Exists             bool
	SecurityDescriptor string
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
	sid                  string
	executable           string
	temporaryRoot        string
	refreshInterval      int
	run                  func(context.Context, ...string) ([]byte, error)
	queryState           func(context.Context, string) (windowsTaskRuntimeState, error)
	queryFolder          func(context.Context) (windowsTaskFolderState, error)
	createFolder         func(context.Context, string) error
	removeFolder         func(context.Context) error
	inspectProxy         func(context.Context, string) componentStatus
	roots                userdirs.Roots
	validateExecutable   func(string) error
	completion           func(string, userdirs.Roots, time.Time) (serviceRefreshCompletion, error)
	forceRefresh         func(context.Context, string, userdirs.Roots) error
	inspectSelectedProxy func(context.Context, string, string, userdirs.Roots) componentStatus
	verifyProxyOwner     func(context.Context) error
}

func (platform *windowsTaskServicePlatform) Preflight(ctx context.Context, executable string) error {
	if err := validateWindowsSID(platform.sid); err != nil {
		return err
	}
	if err := validateAbsoluteWindowsExecutable(executable); err != nil {
		return err
	}
	if platform.run == nil || platform.queryState == nil || platform.queryFolder == nil || platform.createFolder == nil || platform.removeFolder == nil {
		return fmt.Errorf("Windows Task Scheduler is unavailable")
	}
	platform.executable = executable
	folder, err := platform.queryFolder(ctx)
	if err != nil {
		return fmt.Errorf("inspect Windows task folder security: %w", err)
	}
	if folder.Exists && !validWindowsTaskSecurityDescriptor(folder.SecurityDescriptor, platform.sid, true) {
		return fmt.Errorf("%w: Windows task folder security descriptor differs", installstate.ErrOwnershipConflict)
	}
	for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
		definition, exists, err := platform.queryDefinition(ctx, kind)
		if err != nil {
			return err
		}
		if exists {
			if err := validateWindowsTaskDefinition(definition, kind, platform.sid, executable, platform.interval()); err != nil {
				return err
			}
			taskPath, _, _, _ := windowsTaskValues(kind)
			state, err := platform.queryState(ctx, taskPath)
			if err != nil {
				return fmt.Errorf("inspect Windows task security: %w", err)
			}
			if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, false) {
				return fmt.Errorf("%w: Windows task security descriptor differs", installstate.ErrOwnershipConflict)
			}
		}
	}
	_, _, err = platform.proxySession(ctx)
	return err
}

func (platform *windowsTaskServicePlatform) PrepareRollback(ctx context.Context) (serviceRestore, error) {
	snapshot, err := platform.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return func(restoreCtx context.Context) error {
		return platform.Restore(restoreCtx, snapshot)
	}, nil
}

func (platform *windowsTaskServicePlatform) Snapshot(ctx context.Context) (servicePlatformSnapshot, error) {
	if err := platform.rejectProxySession(ctx); err != nil {
		return servicePlatformSnapshot{}, err
	}
	folder, err := platform.queryFolder(ctx)
	if err != nil {
		return servicePlatformSnapshot{}, fmt.Errorf("snapshot Windows task folder: %w", err)
	}
	snapshot := servicePlatformSnapshot{Manager: "task-scheduler", FolderExists: folder.Exists, Components: make([]serviceComponentSnapshot, 0, 2)}
	if folder.Exists {
		if !validWindowsTaskSecurityDescriptor(folder.SecurityDescriptor, platform.sid, true) {
			return servicePlatformSnapshot{}, fmt.Errorf("%w: Windows task folder security descriptor differs", installstate.ErrOwnershipConflict)
		}
		snapshot.FolderSecurityDescriptor = folder.SecurityDescriptor
	}
	for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
		definition, exists, err := platform.queryDefinitionBytes(ctx, kind)
		if err != nil {
			return servicePlatformSnapshot{}, err
		}
		taskPath, _, _, _ := windowsTaskValues(kind)
		running := false
		if exists {
			state, err := platform.queryState(ctx, taskPath)
			if err != nil {
				return servicePlatformSnapshot{}, fmt.Errorf("snapshot Windows task state: %w", err)
			}
			running = state.Running
		}
		snapshot.Components = append(snapshot.Components, serviceComponentSnapshot{ID: taskPath, Definition: append([]byte(nil), definition...), Exists: exists, Running: running})
	}
	return snapshot, nil
}

func (platform *windowsTaskServicePlatform) Restore(ctx context.Context, snapshot servicePlatformSnapshot) error {
	if err := platform.rejectProxySession(ctx); err != nil {
		return err
	}
	taskPaths := []string{windowsProxyTaskPath, windowsRefreshTaskPath}
	validFolder := snapshot.FolderExists && validWindowsTaskSecurityDescriptor(snapshot.FolderSecurityDescriptor, platform.sid, true)
	validAbsentFolder := !snapshot.FolderExists && snapshot.FolderSecurityDescriptor == ""
	if snapshot.Manager != "task-scheduler" || (!validFolder && !validAbsentFolder) || len(snapshot.Components) != len(taskPaths) {
		return fmt.Errorf("invalid Windows task service snapshot")
	}
	for index, component := range snapshot.Components {
		if component.ID != taskPaths[index] || component.Enabled || component.UnitFileState != "" || (!component.Exists && (component.Running || len(component.Definition) != 0)) || len(component.Definition) > maxWindowsTaskXMLBytes {
			return fmt.Errorf("invalid Windows task service snapshot component %q", component.ID)
		}
	}
	var result error
	if snapshot.FolderExists {
		folder, err := platform.queryFolder(ctx)
		if err != nil {
			return fmt.Errorf("inspect Windows task folder before restore: %w", err)
		}
		if !folder.Exists {
			if err := platform.createFolder(ctx, snapshot.FolderSecurityDescriptor); err != nil {
				return fmt.Errorf("restore Windows task folder: %w", err)
			}
			folder, err = platform.queryFolder(ctx)
			if err != nil {
				return fmt.Errorf("inspect restored Windows task folder: %w", err)
			}
		}
		if !folder.Exists || folder.SecurityDescriptor != snapshot.FolderSecurityDescriptor {
			return fmt.Errorf("restored Windows task folder security descriptor differs")
		}
	}
	for index := len(snapshot.Components) - 1; index >= 0; index-- {
		component := snapshot.Components[index]
		result = errors.Join(result, platform.restore(ctx, component.ID, component.Definition, component.Exists, component.Running))
	}
	if !snapshot.FolderExists {
		result = errors.Join(result, platform.removeFolder(ctx))
	}
	return result
}

func (platform *windowsTaskServicePlatform) InstallProxy(ctx context.Context, executable string) error {
	if selectedServiceContext(ctx) {
		if err := platform.removeProxySession(ctx); err != nil {
			return err
		}
	} else if err := platform.rejectProxySession(ctx); err != nil {
		return err
	}
	return platform.reconcile(ctx, windowsProxyTask, executable)
}

func (platform *windowsTaskServicePlatform) InstallRefresh(ctx context.Context, executable string) error {
	return platform.reconcile(ctx, windowsRefreshTask, executable)
}

func (platform *windowsTaskServicePlatform) RestartProxy(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		return platform.restartSelected(ctx, windowsProxyTask)
	}
	if err := platform.rejectProxySession(ctx); err != nil {
		return err
	}
	return platform.restartTask(ctx, windowsProxyTaskPath)
}

func (platform *windowsTaskServicePlatform) RestartRefresh(ctx context.Context) error {
	if selectedServiceContext(ctx) {
		return platform.restartSelected(ctx, windowsRefreshTask)
	}
	return platform.restartTask(ctx, windowsRefreshTaskPath)
}

func (platform *windowsTaskServicePlatform) RemoveProxy(ctx context.Context) error {
	if err := platform.removeProxySession(ctx); err != nil {
		return err
	}
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
	folderCreated, err := platform.ensureTaskFolder(ctx)
	if err != nil {
		return err
	}
	definition, err := renderWindowsTaskDefinition(kind, platform.sid, executable, platform.interval())
	if err != nil {
		return errors.Join(err, platform.removeCreatedTaskFolder(ctx, folderCreated))
	}
	if err := platform.createTask(ctx, taskPath, definition); err != nil {
		return errors.Join(fmt.Errorf("create Windows task %s: %w", taskPath, err), platform.restore(ctx, taskPath, oldDefinition, oldExists, oldState.Running), platform.removeCreatedTaskFolder(ctx, folderCreated))
	}
	if err := platform.runTask(ctx, taskPath); err != nil {
		return errors.Join(err, platform.restore(ctx, taskPath, oldDefinition, oldExists, oldState.Running), platform.removeCreatedTaskFolder(ctx, folderCreated))
	}
	return nil
}

func (platform *windowsTaskServicePlatform) ensureTaskFolder(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := platform.queryFolder(ctx)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, fmt.Errorf("inspect Windows task folder: %w", err)
	}
	if state.Exists {
		if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, true) {
			return false, fmt.Errorf("%w: Windows task folder security descriptor differs", installstate.ErrOwnershipConflict)
		}
		return false, nil
	}
	if err := platform.createFolder(ctx, windowsTaskSecurityDescriptor(platform.sid)); err != nil {
		return false, fmt.Errorf("create Windows task folder: %w", err)
	}
	state, err = platform.queryFolder(ctx)
	if err != nil {
		return true, fmt.Errorf("inspect created Windows task folder: %w", err)
	}
	if !state.Exists || !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, true) {
		return true, fmt.Errorf("%w: created Windows task folder security descriptor differs", installstate.ErrOwnershipConflict)
	}
	return true, nil
}

func (platform *windowsTaskServicePlatform) removeCreatedTaskFolder(ctx context.Context, created bool) error {
	if !created {
		return nil
	}
	return platform.removeFolder(ctx)
}

func (platform *windowsTaskServicePlatform) restore(ctx context.Context, taskPath string, definition []byte, exists, running bool) error {
	var result error
	if err := platform.endAndWait(ctx, taskPath); err != nil {
		return fmt.Errorf("stop failed candidate task: %w", err)
	}
	if !exists {
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	path, cleanup, err := writeTemporaryWindowsTaskXML(platform.temporaryRoot, definition)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := ctx.Err(); err != nil {
		return err
	}
	args := []string{"/Create", "/TN", taskPath, "/XML", path}
	if taskPath != windowsProxySessionTaskPath {
		args = append(args, "/F")
	}
	if _, err := platform.run(ctx, args...); err != nil {
		return err
	}
	return nil
}

func (platform *windowsTaskServicePlatform) runTask(ctx context.Context, taskPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := platform.run(ctx, "/Run", "/TN", taskPath); err != nil && !isWindowsTaskAlreadyRunning(err) {
		return fmt.Errorf("run Windows task %s: %w", taskPath, err)
	}
	return ctx.Err()
}

func (platform *windowsTaskServicePlatform) restartTask(ctx context.Context, taskPath string) error {
	if err := platform.endAndWait(ctx, taskPath); err != nil {
		return fmt.Errorf("stop Windows task %s before restart: %w", taskPath, err)
	}
	return platform.runTask(ctx, taskPath)
}

func (platform *windowsTaskServicePlatform) remove(ctx context.Context, taskPath string) error {
	var result error
	if err := platform.endAndWait(ctx, taskPath); err != nil {
		return errors.Join(result, err, platform.removeFolder(ctx))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := platform.run(ctx, "/Delete", "/TN", taskPath, "/F"); err != nil && !isWindowsTaskNotFound(err) {
		result = errors.Join(result, fmt.Errorf("delete Windows task %s: %w", taskPath, err))
	}
	result = errors.Join(result, platform.removeFolder(ctx))
	return result
}

func (platform *windowsTaskServicePlatform) endAndWait(ctx context.Context, taskPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := platform.run(ctx, "/End", "/TN", taskPath); err != nil && !isWindowsTaskNotFound(err) && !isWindowsTaskNotRunning(err) {
		return fmt.Errorf("end Windows task %s: %w", taskPath, err)
	}
	return platform.waitTaskStopped(ctx, taskPath)
}

func (platform *windowsTaskServicePlatform) waitTaskStopped(ctx context.Context, taskPath string) error {
	return platform.waitTaskStoppedWithPolicy(ctx, taskPath, windowsTaskStopAttempts, windowsTaskStopInterval)
}

func (platform *windowsTaskServicePlatform) waitTaskStoppedWithPolicy(ctx context.Context, taskPath string, attempts int, interval time.Duration) error {
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := platform.queryState(ctx, taskPath)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isWindowsTaskNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect Windows task %s while stopping: %w", taskPath, err)
		}
		if !state.Running && len(state.EnginePIDs) == 0 {
			return nil
		}
		if attempt+1 < attempts {
			if err := waitForServicePoll(ctx, interval); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("Windows task %s remained running after stop", taskPath)
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
	if runtimeState.HasLastResult {
		status.LastResult = fmt.Sprintf("0x%08x", runtimeState.LastResult)
		if runtimeState.LastResult == 0 {
			status.LastResult = "success"
		}
	}
	if !validWindowsTaskSecurityDescriptor(runtimeState.SecurityDescriptor, platform.sid, false) {
		return status, fmt.Errorf("%w: Windows task security descriptor differs", installstate.ErrOwnershipConflict)
	}
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
		status.Listener = runtimeStatus.Listener
		status.Error = runtimeStatus.Error
		if len(runtimeState.EnginePIDs) != 1 || runtimeState.EnginePIDs[0] == 0 {
			status.Error = "Task Scheduler returned ambiguous proxy instance PIDs"
			return status, nil
		}
		status.PID = int(runtimeState.EnginePIDs[0])
		if runtimeStatus.PID != status.PID {
			status.Error = fmt.Sprintf("listener PID %d differs from Task Scheduler EnginePID %d", runtimeStatus.PID, status.PID)
			return status, nil
		}
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
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	taskPath, _, _, err := windowsTaskValues(kind)
	if err != nil {
		return nil, false, err
	}
	data, err := platform.run(ctx, "/Query", "/TN", taskPath, "/XML", "/HResult")
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err == nil {
		if len(data) > maxWindowsTaskXMLBytes {
			return nil, false, fmt.Errorf("Windows task XML exceeds size limit")
		}
		if selectedServiceContext(ctx) || kind == windowsProxySessionTask {
			if err := validateWindowsTaskShape(data, kind); err != nil {
				return nil, false, err
			}
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
	fileData, err := encodeWindowsTaskXMLFile(definition)
	if err != nil {
		return "", func() {}, err
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
	if _, err := file.Write(fileData); err != nil {
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
	if kind == windowsProxySessionTask {
		data, err := renderWindowsTaskDefinition(windowsProxyTask, sid, executable, refreshInterval)
		if err != nil {
			return nil, err
		}
		data = bytes.Replace(data, []byte(windowsProxyTaskPath), []byte(windowsProxySessionTaskPath), 1)
		start, end := bytes.Index(data, []byte("<Triggers>")), bytes.Index(data, []byte("</Triggers>"))
		data = append(append([]byte{}, data[:start]...), data[end+len("</Triggers>"):]...)
		start, end = bytes.Index(data, []byte("<RestartOnFailure>")), bytes.Index(data, []byte("</RestartOnFailure>"))
		data = append(append([]byte{}, data[:start]...), data[end+len("</RestartOnFailure>"):]...)
		return bytes.Replace(data, []byte("<StartWhenAvailable>true</StartWhenAvailable>"), []byte("<StartWhenAvailable>false</StartWhenAvailable>"), 1), nil
	}

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
			Author:             "CQ",
			Description:        description,
			URI:                taskPath,
			SecurityDescriptor: windowsTaskSecurityDescriptor(sid),
		},
		Triggers: windowsTaskTriggers{LogonTrigger: windowsTaskLogonTrigger{
			Enabled: windowsTaskBool(true),
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
			Enabled:                    windowsTaskBool(true),
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
		definition.Settings.RestartOnFailure = &windowsTaskRestartPolicy{Interval: "PT1M", Count: 999}
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
	data, err := normaliseWindowsTaskXML(data)
	if err != nil {
		return windowsTaskDefinition{}, err
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

func encodeWindowsTaskXMLFile(data []byte) ([]byte, error) {
	normalised, err := normaliseWindowsTaskXML(data)
	if err != nil {
		return nil, err
	}
	text := strings.Replace(string(normalised), `encoding="UTF-8"`, `encoding="UTF-16"`, 1)
	units := utf16.Encode([]rune(text))
	encoded := make([]byte, 2, 2+len(units)*2)
	encoded[0], encoded[1] = 0xff, 0xfe
	for _, unit := range units {
		encoded = append(encoded, byte(unit), byte(unit>>8))
	}
	return encoded, nil
}

func normaliseWindowsTaskXML(data []byte) ([]byte, error) {
	if len(data) >= 2 && ((data[0] == 0xff && data[1] == 0xfe) || (data[0] == 0xfe && data[1] == 0xff)) {
		littleEndian := data[0] == 0xff
		data = data[2:]
		if len(data)%2 != 0 {
			return nil, fmt.Errorf("Windows task XML has invalid UTF-16 length")
		}
		units := make([]uint16, len(data)/2)
		for index := range units {
			if littleEndian {
				units[index] = uint16(data[index*2]) | uint16(data[index*2+1])<<8
			} else {
				units[index] = uint16(data[index*2])<<8 | uint16(data[index*2+1])
			}
		}
		text := string(utf16.Decode(units))
		text = strings.Replace(text, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
		return []byte(text), nil
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("Windows task XML is not valid UTF-8 or UTF-16")
	}
	data = bytes.Replace(data, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
	return append([]byte(nil), data...), nil
}

func validateWindowsTaskDefinition(definition windowsTaskDefinition, kind windowsTaskKind, sid, executable string, refreshInterval int) error {
	if kind == windowsProxySessionTask {
		trigger := definition.Triggers.LogonTrigger
		if definition.RegistrationInfo.URI != windowsProxySessionTaskPath || trigger.Enabled != nil || trigger.UserID != "" || trigger.Repetition != nil || definition.Settings.StartWhenAvailable || definition.Settings.RestartOnFailure != nil || !windowsTaskDefaultTrue(definition.Settings.Enabled) {
			return installstate.ErrOwnershipConflict
		}
		definition.RegistrationInfo.URI = windowsProxyTaskPath
		definition.Triggers.LogonTrigger = windowsTaskLogonTrigger{Enabled: windowsTaskBool(true), UserID: sid}
		definition.Settings.StartWhenAvailable = true
		definition.Settings.RestartOnFailure = &windowsTaskRestartPolicy{Interval: "PT1M", Count: 999}
		kind = windowsProxyTask
	}

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
	if !validWindowsTaskSecurityDescriptor(definition.RegistrationInfo.SecurityDescriptor, sid, false) {
		return conflict("security descriptor differs")
	}
	trigger := definition.Triggers.LogonTrigger
	principal := definition.Principals.Principal
	if !windowsTaskDefaultTrue(trigger.Enabled) || !windowsTaskUserMatchesSID(trigger.UserID, sid) || principal.ID != "CQUser" || principal.UserID != sid || principal.LogonType != "InteractiveToken" || (principal.RunLevel != "" && principal.RunLevel != "LeastPrivilege") {
		return conflict("principal or logon trigger differs")
	}
	if definition.Settings.MultipleInstancesPolicy != "IgnoreNew" || !definition.Settings.StartWhenAvailable {
		return conflict("settings differ")
	}
	action := definition.Actions.Exec
	if definition.Actions.Context != "CQUser" || !equalWindowsPath(action.Command, executable) || action.Arguments != arguments || !equalWindowsPath(action.WorkingDirectory, windowsDirectory(executable)) {
		return conflict("action differs")
	}
	if kind == windowsProxyTask {
		if trigger.Repetition != nil || definition.Settings.ExecutionTimeLimit != "PT0S" || definition.Settings.RestartOnFailure == nil || definition.Settings.RestartOnFailure.Interval != "PT1M" || definition.Settings.RestartOnFailure.Count != 999 {
			return conflict("proxy lifetime policy differs")
		}
	} else if trigger.Repetition == nil || trigger.Repetition.Interval != formatWindowsDuration(refreshInterval) || definition.Settings.ExecutionTimeLimit != "PT5M" {
		return conflict("refresh repetition differs")
	}
	return nil
}

func windowsTaskBool(value bool) *bool {
	return &value
}

func windowsTaskDefaultTrue(value *bool) bool {
	return value == nil || *value
}

func windowsTaskSecurityDescriptor(sid string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;" + sid + ")"
}

func validWindowsTaskSecurityDescriptor(value, sid string, requireProtected bool) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	sid = strings.ToUpper(sid)
	if !strings.HasPrefix(value, "D:") {
		return false
	}
	body := strings.TrimPrefix(value, "D:")
	aceStart := strings.IndexByte(body, '(')
	if aceStart < 0 {
		return false
	}
	flags := body[:aceStart]
	if requireProtected && !strings.Contains(flags, "P") {
		return false
	}
	for flags != "" {
		switch {
		case strings.HasPrefix(flags, "AI"), strings.HasPrefix(flags, "AR"):
			flags = flags[2:]
		case strings.HasPrefix(flags, "P"):
			flags = flags[1:]
		default:
			return false
		}
	}
	foundSystemFull := false
	foundUserFull := false
	for body = body[aceStart:]; body != ""; {
		if body[0] != '(' {
			return false
		}
		aceEnd := strings.IndexByte(body, ')')
		if aceEnd < 0 {
			return false
		}
		fields := strings.Split(body[1:aceEnd], ";")
		if len(fields) != 6 || fields[0] != "A" || fields[1] != "" || fields[3] != "" || fields[4] != "" {
			return false
		}
		if fields[2] != "FA" && fields[2] != "FR" {
			return false
		}
		switch {
		case fields[5] == "SY":
			if fields[2] != "FA" {
				return false
			}
			foundSystemFull = true
		case fields[5] == sid || fields[5] == "LA" && windowsSIDHasRID(sid, 500):
			if fields[2] == "FA" {
				foundUserFull = true
			}
		default:
			return false
		}
		body = body[aceEnd+1:]
	}
	return foundSystemFull && foundUserFull
}

func windowsSIDHasRID(sid string, rid uint64) bool {
	if validateWindowsSID(sid) != nil {
		return false
	}
	separator := strings.LastIndexByte(sid, '-')
	if separator < 0 || separator == len(sid)-1 {
		return false
	}
	value, err := strconv.ParseUint(sid[separator+1:], 10, 32)
	return err == nil && value == rid
}

func windowsTaskValues(kind windowsTaskKind) (taskPath, arguments, description string, err error) {
	switch kind {
	case windowsProxyTask:
		return windowsProxyTaskPath, "proxy start", "Runs CQ local proxy", nil
	case windowsProxySessionTask:
		return windowsProxySessionTaskPath, "proxy start", "Runs CQ local proxy", nil
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

// Canonical controls use the task's Enabled property as durable policy. They
// never rewrite the XML to toggle it, preserving scheduler-owned XML details.
func (platform *windowsTaskServicePlatform) StartProxy(ctx context.Context) error {
	return platform.startSelected(ctx, windowsProxyTask)
}
func (platform *windowsTaskServicePlatform) StopProxy(ctx context.Context) error {
	return platform.stopSelected(ctx, windowsProxyTask)
}
func (platform *windowsTaskServicePlatform) StartRefresh(ctx context.Context) error {
	return platform.startSelected(ctx, windowsRefreshTask)
}
func (platform *windowsTaskServicePlatform) StopRefresh(ctx context.Context) error {
	return platform.stopSelected(ctx, windowsRefreshTask)
}

func windowsSelectedKinds(selection serviceSelection) []windowsTaskKind {
	var kinds []windowsTaskKind
	for _, id := range selection.components() {
		if id == serviceProxy {
			kinds = append(kinds, windowsProxyTask)
		} else {
			kinds = append(kinds, windowsRefreshTask)
		}
	}
	return kinds
}
func (platform *windowsTaskServicePlatform) selectedReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateWindowsSID(platform.sid); err != nil {
		return err
	}
	if platform.run == nil || platform.queryState == nil || platform.queryFolder == nil {
		return errServiceUnavailable
	}
	folder, err := platform.queryFolder(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if folder.Exists && !validWindowsTaskSecurityDescriptor(folder.SecurityDescriptor, platform.sid, true) {
		return installstate.ErrOwnershipConflict
	}
	return nil
}
func (platform *windowsTaskServicePlatform) validateSelectedExecutable(executable string) error {
	if err := validateAbsoluteWindowsExecutable(executable); err != nil {
		return err
	}
	if platform.validateExecutable == nil {
		return errServiceUnavailable
	}
	return platform.validateExecutable(executable)
}
func (platform *windowsTaskServicePlatform) PreflightSelected(ctx context.Context, executable string, selection serviceSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := platform.validateSelectedExecutable(executable); err != nil {
		return err
	}
	if err := platform.selectedReady(ctx); err != nil {
		return err
	}
	for _, kind := range windowsSelectedKinds(selection) {
		d, exists, err := platform.querySelectedDefinition(ctx, kind)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := validateWindowsTaskDefinition(d, kind, platform.sid, executable, platform.interval()); err != nil {
			return err
		}
		path, _, _, _ := windowsTaskValues(kind)
		state, err := platform.queryState(ctx, path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, false) {
			return installstate.ErrOwnershipConflict
		}
	}
	if selection == serviceProxy || selection == serviceAll {
		if _, _, err := platform.proxySession(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (platform *windowsTaskServicePlatform) InspectSelected(ctx context.Context, selection serviceSelection) (serviceStatus, error) {
	var result serviceStatus
	if err := platform.selectedReady(ctx); err != nil {
		return result, err
	}
	for _, kind := range windowsSelectedKinds(selection) {
		path, _, _, _ := windowsTaskValues(kind)
		c := componentStatus{ID: path, Manager: "task-scheduler", Observed: &serviceObservation{Owner: "none", Enabled: serviceBool(false), Healthy: serviceBool(false)}}
		d, exists, err := platform.querySelectedDefinition(ctx, kind)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			return result, err
		}
		if !exists && kind == windowsProxyTask {
			if _, _, err := platform.proxySession(ctx); err != nil {
				return result, err
			}
		}
		if exists {
			if err := validateWindowsTaskDefinition(d, kind, platform.sid, platform.executable, platform.interval()); err != nil {
				return result, err
			}
			if err := platform.validateSelectedExecutable(d.Actions.Exec.Command); err != nil {
				return result, err
			}

			state, stateErr := platform.queryState(ctx, path)
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if stateErr != nil {
				return result, stateErr
			}
			if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, false) {
				return result, installstate.ErrOwnershipConflict
			}
			c.Registered = true
			c.ConfiguredExecutable = d.Actions.Exec.Command
			c.Running = state.Running
			if kind == windowsProxyTask {
				_, sessionExists, sessionErr := platform.proxySession(ctx)
				if sessionErr != nil {
					return result, sessionErr
				}
				actualPath := windowsProxyTaskPath
				if sessionExists {
					if state.Running || windowsTaskDefaultTrue(d.Settings.Enabled) {
						return result, installstate.ErrOwnershipConflict
					}
					actualPath = windowsProxySessionTaskPath
					state, stateErr = platform.queryState(ctx, actualPath)
					if ctx.Err() != nil {
						return result, ctx.Err()
					}
					if stateErr != nil {
						return result, stateErr
					}
					c.Running = state.Running
				}
				if platform.inspectSelectedProxy != nil {
					c = platform.inspectSelectedProxy(ctx, d.Actions.Exec.Command, actualPath, platform.roots)
					c.ID = windowsProxyTaskPath
					c.Manager = "task-scheduler"
					c.Registered = true
					c.ConfiguredExecutable = d.Actions.Exec.Command
				} else if c.Running && platform.inspectProxy != nil {
					runtime := platform.inspectProxy(ctx, d.Actions.Exec.Command)
					c.PID = runtime.PID
					c.LiveExecutable = runtime.LiveExecutable
					c.Listener = runtime.Listener
					c.Healthy = runtime.Healthy && len(state.EnginePIDs) == 1 && int(state.EnginePIDs[0]) == runtime.PID && sameServiceExecutable(runtime.LiveExecutable, c.ConfiguredExecutable)
				}
			}
			if err := ctx.Err(); err != nil {
				return result, err
			}
			c.Observed = &serviceObservation{Owner: "cq", Enabled: serviceBool(windowsTaskDefaultTrue(d.Settings.Enabled)), Roots: &platform.roots, Healthy: serviceBool(c.Healthy)}
			if kind == windowsRefreshTask {
				c.Observed.Healthy = nil
				if platform.completion != nil {
					receipt, readErr := platform.completion(c.ConfiguredExecutable, platform.roots, time.Now())
					if readErr == nil {
						c.Observed.LastRunAt = &receipt.CompletedAt
						c.Observed.LastExitCode = &receipt.ExitCode
					}
					if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
						return result, readErr
					}
				}
			}
		}
		if kind == windowsProxyTask {
			result.Proxy = c
		} else {
			result.Refresh = c
		}
	}
	return result, ctx.Err()
}
func (platform *windowsTaskServicePlatform) setEnabled(ctx context.Context, kind windowsTaskKind, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, _, _, _ := windowsTaskValues(kind)
	action := "/Disable"
	if enabled {
		action = "/Enable"
	}
	if _, err := platform.run(ctx, action, "/TN", path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d, exists, err := platform.queryDefinition(ctx, kind)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if !exists || windowsTaskDefaultTrue(d.Settings.Enabled) != enabled {
		return ErrServiceUnhealthy
	}
	return nil
}
func (platform *windowsTaskServicePlatform) startSelected(ctx context.Context, kind windowsTaskKind) error {
	if kind == windowsProxyTask {
		if err := platform.removeProxySession(ctx); err != nil {
			return err
		}
	}
	if err := platform.setEnabled(ctx, kind, true); err != nil {
		return err
	}
	path, _, _, _ := windowsTaskValues(kind)
	return platform.runTask(ctx, path)
}
func (platform *windowsTaskServicePlatform) stopSelected(ctx context.Context, kind windowsTaskKind) error {
	if kind == windowsProxyTask {
		if err := platform.removeProxySession(ctx); err != nil {
			return err
		}
	}
	if err := platform.setEnabled(ctx, kind, false); err != nil {
		return err
	}
	path, _, _, _ := windowsTaskValues(kind)
	return platform.endAndWait(ctx, path)
}
func (platform *windowsTaskServicePlatform) SnapshotSelected(ctx context.Context, selection serviceSelection) (servicePlatformSnapshot, error) {
	snapshot := servicePlatformSnapshot{Manager: "task-scheduler"}
	if err := platform.selectedReady(ctx); err != nil {
		return snapshot, err
	}
	folder, err := platform.queryFolder(ctx)
	if ctx.Err() != nil {
		return snapshot, ctx.Err()
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.FolderExists = folder.Exists
	snapshot.FolderSecurityDescriptor = folder.SecurityDescriptor
	kinds := windowsSelectedKinds(selection)
	if selection == serviceProxy || selection == serviceAll {
		kinds = append(kinds, windowsProxySessionTask)
		if _, _, err := platform.proxySession(ctx); err != nil {
			return snapshot, err
		}
	}
	for _, kind := range kinds {
		path, _, _, _ := windowsTaskValues(kind)
		data, exists, err := platform.queryDefinitionBytes(ctx, kind)
		if ctx.Err() != nil {
			return snapshot, ctx.Err()
		}
		if err != nil {
			return snapshot, err
		}
		c := serviceComponentSnapshot{ID: path, Exists: exists, Definition: data}
		if exists {
			if err := validateWindowsTaskShape(data, kind); err != nil {
				return snapshot, err
			}
			d, err := parseWindowsTaskDefinition(data)
			if err != nil {
				return snapshot, err
			}
			if err := validateWindowsTaskDefinition(d, kind, platform.sid, platform.executable, platform.interval()); err != nil {
				return snapshot, err
			}
			state, err := platform.queryState(ctx, path)
			if ctx.Err() != nil {
				return snapshot, ctx.Err()
			}
			if err != nil {
				return snapshot, err
			}
			if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, false) {
				return snapshot, installstate.ErrOwnershipConflict
			}
			c.Running = state.Running
			c.Enabled = windowsTaskDefaultTrue(d.Settings.Enabled)
		}
		snapshot.Components = append(snapshot.Components, c)
	}
	return snapshot, ctx.Err()
}
func (platform *windowsTaskServicePlatform) RestoreSelected(ctx context.Context, selection serviceSelection, snapshot servicePlatformSnapshot) error {
	kinds := windowsSelectedKinds(selection)
	if selection == serviceProxy || selection == serviceAll {
		kinds = append(kinds, windowsProxySessionTask)
	}
	if snapshot.Manager != "task-scheduler" || len(kinds) != len(snapshot.Components) || (snapshot.FolderExists && !validWindowsTaskSecurityDescriptor(snapshot.FolderSecurityDescriptor, platform.sid, true)) || (!snapshot.FolderExists && snapshot.FolderSecurityDescriptor != "") {
		return errServiceUnavailable
	}
	if err := platform.PreflightSelected(ctx, platform.executable, selection); err != nil {
		return err
	}
	for i, kind := range kinds {
		c := snapshot.Components[i]
		path, _, _, _ := windowsTaskValues(kind)
		if c.ID != path || c.UnitFileState != "" || (!c.Exists && (c.Enabled || c.Running || len(c.Definition) > 0)) {
			return errServiceUnavailable
		}
		if c.Exists {
			if err := validateWindowsTaskShape(c.Definition, kind); err != nil {
				return err
			}
			if !snapshot.FolderExists {
				return errServiceUnavailable
			}
			d, err := parseWindowsTaskDefinition(c.Definition)
			if err != nil {
				return err
			}
			if err := validateWindowsTaskDefinition(d, kind, platform.sid, platform.executable, platform.interval()); err != nil {
				return err
			}
			if c.Enabled != windowsTaskDefaultTrue(d.Settings.Enabled) {
				return errServiceUnavailable
			}
		}
	}
	if selection == serviceProxy || selection == serviceAll {
		if err := platform.removeProxySession(ctx); err != nil {
			return err
		}
	}
	if snapshot.FolderExists {
		folder, err := platform.queryFolder(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !folder.Exists {
			if err := platform.createFolder(ctx, snapshot.FolderSecurityDescriptor); err != nil {
				return err
			}
		}
		folder, err = platform.queryFolder(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !folder.Exists || folder.SecurityDescriptor != snapshot.FolderSecurityDescriptor {
			return ErrServiceUnhealthy
		}
	}
	for i := 0; i < len(snapshot.Components); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := snapshot.Components[i]
		if c.ID == windowsProxySessionTaskPath && !c.Exists {
			continue
		}
		if err := platform.restore(ctx, c.ID, c.Definition, c.Exists, c.Running); err != nil {
			return err
		}
		restored, exists, err := platform.queryDefinitionBytes(ctx, kinds[i])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if exists != c.Exists || (exists && !bytes.Equal(restored, c.Definition)) {
			return ErrServiceUnhealthy
		}
	}
	if !snapshot.FolderExists {
		if err := ctx.Err(); err != nil {
			return err
		}
		return platform.removeFolder(ctx)
	}
	return ctx.Err()
}

func (platform *windowsTaskServicePlatform) proxySession(ctx context.Context) ([]byte, bool, error) {
	data, exists, err := platform.queryDefinitionBytes(ctx, windowsProxySessionTask)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err != nil || !exists {
		return data, exists, err
	}
	if platform.verifyProxyOwner == nil {
		return nil, false, errServiceUnavailable
	}
	if err := platform.verifyProxyOwner(ctx); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	primary, found, err := platform.querySelectedDefinition(ctx, windowsProxyTask)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, installstate.ErrOwnershipConflict
	}
	if err := validateWindowsTaskDefinition(primary, windowsProxyTask, platform.sid, platform.executable, platform.interval()); err != nil {
		return nil, false, err
	}
	if err := platform.validateSelectedExecutable(primary.Actions.Exec.Command); err != nil {
		return nil, false, err
	}
	primaryState, err := platform.queryState(ctx, windowsProxyTaskPath)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err != nil {
		return nil, false, err
	}
	if !validWindowsTaskSecurityDescriptor(primaryState.SecurityDescriptor, platform.sid, false) {
		return nil, false, installstate.ErrOwnershipConflict
	}
	d, err := parseWindowsTaskDefinition(data)
	if err != nil {
		return nil, false, err
	}
	if err := validateWindowsTaskDefinition(d, windowsProxySessionTask, platform.sid, platform.executable, platform.interval()); err != nil {
		return nil, false, err
	}
	state, err := platform.queryState(ctx, windowsProxySessionTaskPath)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if err != nil {
		return nil, false, err
	}
	if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, platform.sid, false) {
		return nil, false, installstate.ErrOwnershipConflict
	}
	return data, true, nil
}
func (platform *windowsTaskServicePlatform) rejectProxySession(ctx context.Context) error {
	_, exists, err := platform.proxySession(ctx)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: stop the proxy session before package snapshot or upgrade", errServiceUnavailable)
	}
	return nil
}
func (platform *windowsTaskServicePlatform) removeProxySession(ctx context.Context) error {
	_, exists, err := platform.proxySession(ctx)
	if err != nil || !exists {
		return err
	}
	if err := platform.endAndWait(ctx, windowsProxySessionTaskPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = platform.run(ctx, "/Delete", "/TN", windowsProxySessionTaskPath, "/F")
	if err != nil {
		return err
	}
	_, exists, err = platform.queryDefinitionBytes(ctx, windowsProxySessionTask)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if exists {
		return ErrServiceUnhealthy
	}
	return nil
}
func (platform *windowsTaskServicePlatform) restartSelected(ctx context.Context, kind windowsTaskKind) error {
	d, exists, err := platform.querySelectedDefinition(ctx, kind)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if !exists {
		return installstate.ErrNotInstalled
	}
	if err := validateWindowsTaskDefinition(d, kind, platform.sid, platform.executable, platform.interval()); err != nil {
		return err
	}
	if err := platform.validateSelectedExecutable(d.Actions.Exec.Command); err != nil {
		return err
	}
	if kind == windowsProxyTask {
		if err := platform.removeProxySession(ctx); err != nil {
			return err
		}
	}
	path, _, _, _ := windowsTaskValues(kind)
	if err := platform.endAndWait(ctx, path); err != nil {
		return err
	}
	if windowsTaskDefaultTrue(d.Settings.Enabled) {
		return platform.runTask(ctx, path)
	}
	if kind == windowsRefreshTask {
		if platform.forceRefresh == nil {
			return errServiceUnavailable
		}
		return platform.forceRefresh(ctx, d.Actions.Exec.Command, platform.roots)
	}
	data, err := renderWindowsTaskDefinition(windowsProxySessionTask, platform.sid, d.Actions.Exec.Command, platform.interval())
	if err != nil {
		return err
	}
	if err := platform.createTask(ctx, windowsProxySessionTaskPath, data); err != nil {
		return err
	}
	if err := platform.runTask(ctx, windowsProxySessionTaskPath); err != nil {
		return errors.Join(err, platform.removeProxySession(ctx))
	}
	return ctx.Err()
}

// A task engine PID may be the action itself or its immediate parent. Match
// stable native identities, not a PID number or HTTP liveness alone.
type windowsServiceProcessIdentity struct {
	PID, ParentPID  uint32
	Executable, SID string
	Created         uint64
	Children        uint32
	OnlyChildPID    uint32
}

func windowsTaskOwnsProcess(state windowsTaskRuntimeState, action, engine windowsServiceProcessIdentity, executable, sid string) bool {
	if !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, sid, false) || !state.Running || len(state.EnginePIDs) != 1 || state.EnginePIDs[0] == 0 || action.PID == 0 || action.Created == 0 || engine.Created == 0 || action.SID != sid || engine.SID != sid || !equalWindowsPath(action.Executable, executable) || state.EnginePIDs[0] != engine.PID {
		return false
	}
	if action.PID == engine.PID {
		return action == engine
	}
	return action.ParentPID == engine.PID && action.Created >= engine.Created && engine.Children == 1 && engine.OnlyChildPID == action.PID
}

func validateWindowsTaskShape(data []byte, kind windowsTaskKind) error {
	data, err := normaliseWindowsTaskXML(data)
	if err != nil {
		return err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var stack []string
	actions, triggers := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case xml.StartElement:
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if parent == "Actions" {
				actions++
				if token.Name.Local != "Exec" {
					return installstate.ErrOwnershipConflict
				}
			}
			if parent == "Triggers" {
				triggers++
				if token.Name.Local != "LogonTrigger" {
					return installstate.ErrOwnershipConflict
				}
			}
			stack = append(stack, token.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	wantedTriggers := 1
	if kind == windowsProxySessionTask {
		wantedTriggers = 0
	}
	if actions != 1 || triggers != wantedTriggers {
		return installstate.ErrOwnershipConflict
	}
	return nil
}

func validWindowsServicePath(path string) bool {
	if len(path) < 3 || !((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) || path[1] != ':' || path[2] != '\\' || strings.ContainsAny(path[3:], ":/\x00") {
		return false
	}
	if len(path) == 3 {
		return true
	}
	for _, part := range strings.Split(path[3:], `\`) {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (platform *windowsTaskServicePlatform) querySelectedDefinition(ctx context.Context, kind windowsTaskKind) (windowsTaskDefinition, bool, error) {
	data, exists, err := platform.queryDefinitionBytes(ctx, kind)
	if err != nil || !exists {
		return windowsTaskDefinition{}, exists, err
	}
	if err := validateWindowsTaskShape(data, kind); err != nil {
		return windowsTaskDefinition{}, false, err
	}
	definition, err := parseWindowsTaskDefinition(data)
	return definition, true, err
}
