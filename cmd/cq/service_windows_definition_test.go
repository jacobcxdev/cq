package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const testWindowsSID = "S-1-5-21-111111111-222222222-333333333-1001"

func TestWindowsTaskDefinitionProxyMatchesGoldenFile(t *testing.T) {
	assertWindowsTaskGolden(t, windowsProxyTask, `C:\Users\Test & Co\cq.exe`, "proxy-task.xml")
}

func TestWindowsTaskDefinitionRefreshMatchesGoldenFile(t *testing.T) {
	assertWindowsTaskGolden(t, windowsRefreshTask, `C:\Users\Test & Co\cq.exe`, "refresh-task.xml")
}

func TestWindowsTaskDefinitionKeepsCommandAndArgumentsSeparate(t *testing.T) {
	executable := `C:\Users\Test & Co\cq.exe`
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, executable, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Actions.Exec.Command != executable || definition.Actions.Exec.Arguments != "proxy start" || definition.Actions.Exec.WorkingDirectory != `C:\Users\Test & Co` {
		t.Fatalf("action = %#v", definition.Actions.Exec)
	}
	if strings.Contains(definition.Actions.Exec.Command, "proxy") {
		t.Fatalf("command embeds arguments: %q", definition.Actions.Exec.Command)
	}
}

func TestWindowsTaskDefinitionRejectsWrongPrincipalAndAction(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	definition.Principals.Principal.UserID = "S-1-5-18"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("principal validation error = %v", err)
	}
	definition.Principals.Principal.UserID = testWindowsSID
	definition.Principals.Principal.ID = "OtherUser"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("principal ID validation error = %v", err)
	}
	definition.Principals.Principal.ID = "CQUser"
	definition.Actions.Context = "OtherUser"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("action context validation error = %v", err)
	}
	definition.Actions.Context = "CQUser"
	definition.Actions.Exec.Arguments = "proxy stop"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("action validation error = %v", err)
	}
}

func TestWindowsTaskDefinitionAcceptsSchedulerDefaultCanonicalisation(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	definition.Triggers.LogonTrigger.Enabled = nil
	definition.Principals.Principal.RunLevel = ""
	definition.Settings.Enabled = nil
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); err != nil {
		t.Fatal(err)
	}
	definition.Triggers.LogonTrigger.Enabled = windowsTaskBool(false)
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("disabled trigger error = %v", err)
	}
}

func TestWindowsTaskDefinitionRejectsWrongSchema(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	definition.Version = "1.1"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("schema validation error = %v", err)
	}
}

func TestWindowsTaskDefinitionRejectsDifferentSecurityDescriptor(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	definition.RegistrationInfo.SecurityDescriptor = "D:(A;;FA;;;WD)"
	if err := validateWindowsTaskDefinition(definition, windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("security descriptor validation error = %v", err)
	}
}

func TestWindowsTaskSecurityDescriptorAcceptsSchedulerCanonicalForms(t *testing.T) {
	taskDescriptor := "D:(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")(A;;FR;;;" + testWindowsSID + ")"
	folderDescriptor := "D:PAI(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")"
	if !validWindowsTaskSecurityDescriptor(taskDescriptor, testWindowsSID, false) {
		t.Fatal("scheduler-normalised task descriptor was rejected")
	}
	if !validWindowsTaskSecurityDescriptor(folderDescriptor, testWindowsSID, true) {
		t.Fatal("scheduler-normalised folder descriptor was rejected")
	}
}

func TestWindowsTaskSecurityDescriptorAcceptsAdministratorAlias(t *testing.T) {
	administratorSID := "S-1-5-21-111111111-222222222-333333333-500"
	taskDescriptor := "D:(A;;FA;;;SY)(A;;FA;;;LA)(A;;FR;;;LA)"
	folderDescriptor := "D:PAI(A;;FA;;;SY)(A;;FA;;;LA)"
	if !validWindowsTaskSecurityDescriptor(taskDescriptor, administratorSID, false) {
		t.Fatal("scheduler Administrator task descriptor was rejected")
	}
	if !validWindowsTaskSecurityDescriptor(folderDescriptor, administratorSID, true) {
		t.Fatal("scheduler Administrator folder descriptor was rejected")
	}
	if validWindowsTaskSecurityDescriptor(taskDescriptor, testWindowsSID, false) {
		t.Fatal("Administrator alias accepted for non-Administrator SID")
	}
}

func TestWindowsTaskSecurityDescriptorRejectsBroaderOrWeakerAccess(t *testing.T) {
	tests := []struct {
		name       string
		descriptor string
		folder     bool
	}{
		{name: "foreign trustee", descriptor: "D:P(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")(A;;FR;;;WD)"},
		{name: "deny", descriptor: "D:P(D;;FR;;;WD)(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")"},
		{name: "missing system full", descriptor: "D:P(A;;FR;;;SY)(A;;FA;;;" + testWindowsSID + ")"},
		{name: "missing user full", descriptor: "D:P(A;;FA;;;SY)(A;;FR;;;" + testWindowsSID + ")"},
		{name: "unprotected folder", descriptor: "D:(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")", folder: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if validWindowsTaskSecurityDescriptor(test.descriptor, testWindowsSID, test.folder) {
				t.Fatalf("accepted %q", test.descriptor)
			}
		})
	}
}

func TestWindowsTaskDefinitionSupportsCustomRefreshInterval(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsRefreshTask, testWindowsSID, `C:\cq\cq.exe`, 75)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Triggers.LogonTrigger.Repetition == nil || definition.Triggers.LogonTrigger.Repetition.Interval != "PT1M15S" {
		t.Fatalf("repetition = %#v", definition.Triggers.LogonTrigger.Repetition)
	}
}

func TestWindowsTaskDefinitionTemporaryFileUsesUTF16(t *testing.T) {
	definition, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := writeTemporaryWindowsTaskXML(t.TempDir(), definition)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	fileData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileData) < 2 || fileData[0] != 0xff || fileData[1] != 0xfe {
		t.Fatalf("temporary XML prefix = %x", fileData[:min(len(fileData), 4)])
	}
	parsed, err := parseWindowsTaskDefinition(fileData)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Actions.Exec.Command != `C:\cq\cq.exe` {
		t.Fatalf("round-trip command = %q", parsed.Actions.Exec.Command)
	}
}

func TestWindowsTaskDefinitionAcceptsUTF8SchedulerOutputWithUTF16Declaration(t *testing.T) {
	data, err := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, `C:\cq\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`encoding="UTF-8"`), []byte(`encoding="UTF-16"`), 1)
	parsed, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Actions.Exec.Command != `C:\cq\cq.exe` {
		t.Fatalf("command = %q", parsed.Actions.Exec.Command)
	}
}

func TestWindowsTaskDefinitionServiceReconcilesTasksInOrder(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.Preflight(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/Query", "/TN", windowsProxyTaskPath, "/XML", "/HResult"},
		{"/Query", "/TN", windowsRefreshTaskPath, "/XML", "/HResult"},
		{"/Query", "/TN", windowsProxySessionTaskPath, "/XML", "/HResult"},
		{"/Query", "/TN", windowsProxySessionTaskPath, "/XML", "/HResult"},
		{"/Query", "/TN", windowsProxyTaskPath, "/XML", "/HResult"},
		{"/Create", "/TN", windowsProxyTaskPath, "/XML", runner.xmlPaths[0], "/F"},
		{"/Run", "/TN", windowsProxyTaskPath},
		{"/Query", "/TN", windowsRefreshTaskPath, "/XML", "/HResult"},
		{"/Create", "/TN", windowsRefreshTaskPath, "/XML", runner.xmlPaths[1], "/F"},
		{"/Run", "/TN", windowsRefreshTaskPath},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("schtasks calls = %#v\nwant = %#v", runner.calls, want)
	}
	if len(runner.tasks) != 2 || !runner.running[windowsProxyTaskPath] || !runner.running[windowsRefreshTaskPath] {
		t.Fatalf("tasks/running = %#v/%#v", runner.tasks, runner.running)
	}
	if !runner.folderExists {
		t.Fatal("protected task folder was not created")
	}
	for _, path := range runner.xmlPaths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary task XML remains at %s: %v", path, err)
		}
	}
}

func TestWindowsTaskDefinitionServiceRestoresPreviousTaskOnRunFailure(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	oldExecutable := `C:\Old\cq.exe`
	oldDefinition, err := renderWindowsTaskDefinition(windowsProxyTask, platform.sid, oldExecutable, 1800)
	if err != nil {
		t.Fatal(err)
	}
	runner.tasks[windowsProxyTaskPath] = oldDefinition
	runner.running[windowsProxyTaskPath] = true
	runner.failOnce["/Run\x00/TN\x00"+windowsProxyTaskPath] = errors.New("run failed")

	err = platform.InstallProxy(context.Background(), platform.executable)
	if err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	if string(runner.tasks[windowsProxyTaskPath]) != string(oldDefinition) || !runner.running[windowsProxyTaskPath] {
		t.Fatalf("previous task was not restored")
	}
}

func TestWindowsTaskDefinitionServiceWaitsForCandidateExitBeforeRestore(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	oldDefinition, err := renderWindowsTaskDefinition(windowsProxyTask, platform.sid, `C:\Old\cq.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	runner.folderExists = true
	runner.folderSDDL = windowsTaskSecurityDescriptor(platform.sid)
	runner.tasks[windowsProxyTaskPath] = oldDefinition
	runner.running[windowsProxyTaskPath] = true
	runner.enginePIDs[windowsProxyTaskPath] = []uint32{901}
	runner.failOnce["/Run\x00/TN\x00"+windowsProxyTaskPath] = errors.New("run failed")

	stateQueries := 0
	baseQueryState := platform.queryState
	platform.queryState = func(ctx context.Context, taskPath string) (windowsTaskRuntimeState, error) {
		stateQueries++
		if stateQueries == 2 {
			return windowsTaskRuntimeState{EnginePIDs: []uint32{902}}, nil
		}
		return baseQueryState(ctx, taskPath)
	}

	err = platform.InstallProxy(context.Background(), platform.executable)
	if err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	if stateQueries != 3 {
		t.Fatalf("task state queries = %d, want 3", stateQueries)
	}
	if string(runner.tasks[windowsProxyTaskPath]) != string(oldDefinition) || !runner.running[windowsProxyTaskPath] {
		t.Fatal("previous task was not restored after candidate exit")
	}
}

func TestWindowsTaskDefinitionServiceRestartProxyReplacesInstance(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	baseRun := platform.run
	nextPID := uint32(901)
	platform.run = func(ctx context.Context, args ...string) ([]byte, error) {
		output, err := baseRun(ctx, args...)
		if err == nil && reflect.DeepEqual(args, []string{"/Run", "/TN", windowsProxyTaskPath}) {
			nextPID++
			runner.enginePIDs[windowsProxyTaskPath] = []uint32{nextPID}
		}
		return output, err
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	before := runner.enginePIDs[windowsProxyTaskPath][0]
	runner.calls = nil

	if err := platform.RestartProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := runner.enginePIDs[windowsProxyTaskPath][0]
	if before == after {
		t.Fatalf("proxy PID remained %d", after)
	}
	want := [][]string{
		{"/Query", "/TN", windowsProxySessionTaskPath, "/XML", "/HResult"},
		{"/End", "/TN", windowsProxyTaskPath},
		{"/Run", "/TN", windowsProxyTaskPath},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("restart calls = %#v, want %#v", runner.calls, want)
	}
}

func TestWindowsTaskDefinitionServiceWaitsForTaskInstanceExitBeforeRestart(t *testing.T) {
	platform, _ := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}

	polls := 0
	baseQueryState := platform.queryState
	platform.queryState = func(ctx context.Context, taskPath string) (windowsTaskRuntimeState, error) {
		polls++
		if polls == 1 {
			return windowsTaskRuntimeState{EnginePIDs: []uint32{902}}, nil
		}
		return baseQueryState(ctx, taskPath)
	}
	baseRun := platform.run
	platform.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"/Run", "/TN", windowsProxyTaskPath}) && polls < 2 {
			return nil, errors.New("task restarted before its process exited")
		}
		return baseRun(ctx, args...)
	}

	if err := platform.RestartProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("task stop polls = %d, want 2", polls)
	}
}

func TestWindowsTaskDefinitionServiceRestartAndRemovalAreIdempotent(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	runner.failOnce["/Run\x00/TN\x00"+windowsProxyTaskPath] = windowsTaskCommandError{Code: windowsTaskAlreadyRunning, Output: "already running"}

	if err := platform.RestartRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RestartProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.tasks) != 0 {
		t.Fatalf("tasks remain = %#v", runner.tasks)
	}
	if runner.folderExists {
		t.Fatal("empty task folder remains")
	}
	if !hasWindowsTaskCall(runner.calls, []string{"/End", "/TN", windowsRefreshTaskPath}) || !hasWindowsTaskCall(runner.calls, []string{"/Delete", "/TN", windowsProxyTaskPath, "/F"}) {
		t.Fatalf("removal calls = %#v", runner.calls)
	}
}

func TestWindowsTaskDefinitionServiceWaitsForTaskInstanceExitBeforeDelete(t *testing.T) {
	platform, _ := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}

	polls := 0
	platform.queryState = func(context.Context, string) (windowsTaskRuntimeState, error) {
		polls++
		state := windowsTaskRuntimeState{HasLastResult: true}
		if polls == 1 {
			state.EnginePIDs = []uint32{902}
		}
		return state, nil
	}
	originalRun := platform.run
	platform.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.EqualFold(args[0], "/Delete") && polls < 2 {
			return nil, errors.New("task deleted before its process exited")
		}
		return originalRun(ctx, args...)
	}

	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("task stop polls = %d, want 2", polls)
	}
}

func TestWindowsTaskDefinitionServiceStopWaitHonoursCancellation(t *testing.T) {
	platform, _ := newWindowsTaskServiceHarness(t)
	platform.queryState = func(context.Context, string) (windowsTaskRuntimeState, error) {
		return windowsTaskRuntimeState{Running: true, EnginePIDs: []uint32{902}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := platform.waitTaskStoppedWithPolicy(ctx, windowsProxyTaskPath, 3, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitTaskStoppedWithPolicy() error = %v", err)
	}
}

func TestWindowsTaskDefinitionServiceStopWaitRejectsInspectionError(t *testing.T) {
	platform, _ := newWindowsTaskServiceHarness(t)
	want := errors.New("query failed")
	platform.queryState = func(context.Context, string) (windowsTaskRuntimeState, error) {
		return windowsTaskRuntimeState{}, want
	}

	err := platform.waitTaskStoppedWithPolicy(context.Background(), windowsProxyTaskPath, 3, 0)
	if !errors.Is(err, want) {
		t.Fatalf("waitTaskStoppedWithPolicy() error = %v", err)
	}
}

func TestWindowsTaskDefinitionServiceStopWaitStopsAfterBoundedAttempts(t *testing.T) {
	platform, _ := newWindowsTaskServiceHarness(t)
	polls := 0
	platform.queryState = func(context.Context, string) (windowsTaskRuntimeState, error) {
		polls++
		return windowsTaskRuntimeState{Running: true, EnginePIDs: []uint32{902}}, nil
	}

	err := platform.waitTaskStoppedWithPolicy(context.Background(), windowsProxyTaskPath, 2, 0)
	if err == nil || !strings.Contains(err.Error(), "remained running") {
		t.Fatalf("waitTaskStoppedWithPolicy() error = %v", err)
	}
	if polls != 2 {
		t.Fatalf("task stop polls = %d, want 2", polls)
	}
}

func TestWindowsTaskDefinitionServiceInspectCombinesSchedulerAndRuntime(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.running[windowsRefreshTaskPath] = false
	runner.lastResult[windowsRefreshTaskPath] = 0

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Proxy.Registered || !status.Proxy.Running || !status.Proxy.Healthy || status.Proxy.PID != 902 || status.Proxy.ConfiguredExecutable != platform.executable || status.Proxy.LiveExecutable != platform.executable || status.Proxy.Listener != "127.0.0.1:19280" {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
	if !status.Refresh.Registered || status.Refresh.Running || !status.Refresh.Healthy || status.Refresh.LastResult != "success" || status.Refresh.ConfiguredExecutable != platform.executable {
		t.Fatalf("refresh status = %#v", status.Refresh)
	}
}

func TestWindowsTaskDefinitionServiceKeepsSchedulerPIDAuthoritative(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.enginePIDs[windowsProxyTaskPath] = []uint32{777}

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Proxy.PID != 777 || status.Proxy.Healthy || !strings.Contains(status.Proxy.Error, "PID") {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
}

func TestWindowsTaskDefinitionServiceReportsProxyExitCode(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.running[windowsProxyTaskPath] = false
	runner.lastResult[windowsProxyTaskPath] = 0x80041320

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Proxy.Running || status.Proxy.Healthy || status.Proxy.LastResult != "0x80041320" {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
}

func TestWindowsTaskDefinitionServiceRejectsForeignFolderSecurity(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	runner.folderExists = true
	runner.folderSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;WD)"

	err := platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("Preflight() error = %v", err)
	}
}

func TestWindowsTaskDefinitionServiceRollbackRemovesCreatedFolder(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	restore, err := platform.PrepareRollback(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if !runner.folderExists {
		t.Fatal("task folder was not created")
	}
	if err := restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.folderExists || len(runner.tasks) != 0 {
		t.Fatalf("rollback left folder/tasks = %t/%#v", runner.folderExists, runner.tasks)
	}
}

func TestWindowsTaskDefinitionServiceRestoreRecreatesExactFolderFirst(t *testing.T) {
	platform, runner := newWindowsTaskServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	priorFolderSDDL := "D:PAR(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")"
	runner.folderSDDL = priorFolderSDDL
	snapshot, err := platform.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runner.tasks = map[string][]byte{}
	runner.running = map[string]bool{}
	runner.folderExists = false
	runner.folderSDDL = ""

	if err := platform.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if !runner.folderExists || runner.folderSDDL != priorFolderSDDL {
		t.Fatalf("restored folder = %t, %q", runner.folderExists, runner.folderSDDL)
	}
}

func newWindowsTaskServiceHarness(t *testing.T) (*windowsTaskServicePlatform, *fakeWindowsTaskRunner) {
	t.Helper()
	runner := &fakeWindowsTaskRunner{
		tasks:      map[string][]byte{},
		running:    map[string]bool{},
		lastResult: map[string]uint32{},
		enginePIDs: map[string][]uint32{},
		failOnce:   map[string]error{},
	}
	platform := &windowsTaskServicePlatform{
		sid:                testWindowsSID,
		validateExecutable: func(string) error { return nil },
		verifyProxyOwner:   func(context.Context) error { return nil },
		roots:              userdirs.Roots{Config: `C:\test\config`, State: `C:\test\state`, Cache: `C:\test\cache`, Runtime: `C:\test\runtime`, Logs: `C:\test\logs`},
		executable:         `C:\Users\Test\cq.exe`,
		temporaryRoot:      t.TempDir(),
		refreshInterval:    1800,
		run:                runner.Run,
		queryState:         runner.State,
		queryFolder:        runner.FolderState,
		createFolder:       runner.CreateFolder,
		removeFolder:       runner.RemoveFolderIfEmpty,
		inspectProxy: func(_ context.Context, executable string) componentStatus {
			return componentStatus{ID: windowsProxyTaskPath, Manager: "task-scheduler", Running: true, LiveExecutable: executable, PID: 902, Listener: "127.0.0.1:19280", Healthy: true}
		},
	}
	return platform, runner
}

type fakeWindowsTaskRunner struct {
	calls        [][]string
	xmlPaths     []string
	tasks        map[string][]byte
	running      map[string]bool
	lastResult   map[string]uint32
	enginePIDs   map[string][]uint32
	failOnce     map[string]error
	folderExists bool
	folderSDDL   string
}

func (runner *fakeWindowsTaskRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), args...))
	key := strings.Join(args, "\x00")
	if err := runner.failOnce[key]; err != nil {
		delete(runner.failOnce, key)
		return nil, err
	}
	switch strings.ToLower(args[0]) {
	case "/query":
		path := args[2]
		definition := runner.tasks[path]
		if definition == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
		}
		return append([]byte(nil), definition...), nil
	case "/create":
		if !runner.folderExists {
			return nil, errors.New("Task Scheduler folder does not exist")
		}
		path, xmlPath := args[2], args[4]
		if path == windowsProxySessionTaskPath && runner.tasks[path] != nil && len(args) == 5 {
			return nil, errors.New("task already exists")
		}
		definition, err := os.ReadFile(xmlPath)
		if err != nil {
			return nil, err
		}
		definition, err = normaliseWindowsTaskXML(definition)
		if err != nil {
			return nil, err
		}
		runner.xmlPaths = append(runner.xmlPaths, xmlPath)
		runner.tasks[path] = definition
		return nil, nil
	case "/enable", "/disable":
		path := args[2]
		data := runner.tasks[path]
		if data == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound}
		}
		d, err := parseWindowsTaskDefinition(data)
		if err != nil {
			return nil, err
		}
		value := "false"
		if strings.EqualFold(args[0], "/Enable") {
			value = "true"
		}
		old := "false"
		if windowsTaskDefaultTrue(d.Settings.Enabled) {
			old = "true"
		}
		offset := bytes.Index(data, []byte("<Settings>"))
		if offset < 0 {
			return nil, fmt.Errorf("missing settings")
		}
		data = append(append([]byte{}, data[:offset]...), bytes.Replace(data[offset:], []byte("<Enabled>"+old+"</Enabled>"), []byte("<Enabled>"+value+"</Enabled>"), 1)...)
		runner.tasks[path] = data
		return nil, nil
	case "/run":
		path := args[2]
		d, err := parseWindowsTaskDefinition(runner.tasks[path])
		if err != nil {
			return nil, err
		}
		if !windowsTaskDefaultTrue(d.Settings.Enabled) {
			return nil, windowsTaskCommandError{Code: 0x80041326}
		}

		if runner.running[path] {
			return nil, windowsTaskCommandError{Code: windowsTaskAlreadyRunning, Output: "already running"}
		}
		runner.running[path] = true
		if (path == windowsProxyTaskPath || path == windowsProxySessionTaskPath) && len(runner.enginePIDs[path]) == 0 {
			runner.enginePIDs[path] = []uint32{902}
		}
		return nil, nil
	case "/end":
		path := args[2]
		if runner.tasks[path] == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
		}
		runner.running[path] = false
		delete(runner.enginePIDs, path)
		return nil, nil
	case "/delete":
		path := args[2]
		if runner.tasks[path] == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
		}
		delete(runner.tasks, path)
		delete(runner.running, path)
		delete(runner.enginePIDs, path)
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected schtasks command %q", args[0])
	}
}

func (runner *fakeWindowsTaskRunner) State(_ context.Context, path string) (windowsTaskRuntimeState, error) {
	if runner.tasks[path] == nil {
		return windowsTaskRuntimeState{}, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
	}
	return windowsTaskRuntimeState{
		Running:            runner.running[path],
		LastResult:         runner.lastResult[path],
		HasLastResult:      true,
		EnginePIDs:         append([]uint32(nil), runner.enginePIDs[path]...),
		SecurityDescriptor: "D:(A;;FA;;;SY)(A;;FA;;;" + testWindowsSID + ")(A;;FR;;;" + testWindowsSID + ")",
	}, nil
}

func (runner *fakeWindowsTaskRunner) FolderState(context.Context) (windowsTaskFolderState, error) {
	return windowsTaskFolderState{Exists: runner.folderExists, SecurityDescriptor: runner.folderSDDL}, nil
}

func (runner *fakeWindowsTaskRunner) CreateFolder(_ context.Context, securityDescriptor string) error {
	runner.folderExists = true
	runner.folderSDDL = securityDescriptor
	return nil
}

func (runner *fakeWindowsTaskRunner) RemoveFolderIfEmpty(context.Context) error {
	if len(runner.tasks) == 0 {
		runner.folderExists = false
		runner.folderSDDL = ""
	}
	return nil
}

func hasWindowsTaskCall(calls [][]string, want []string) bool {
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			return true
		}
	}
	return false
}

func assertWindowsTaskGolden(t *testing.T, kind windowsTaskKind, executable, name string) {
	t.Helper()
	data, err := renderWindowsTaskDefinition(kind, testWindowsSID, executable, 1800)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "windows", name))
	if err != nil {
		t.Fatal(err)
	}
	data = normaliseWindowsTaskXMLNewlines(data)
	want = normaliseWindowsTaskXMLNewlines(want)
	if string(data) != string(want) {
		t.Fatalf("definition differs\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func normaliseWindowsTaskXMLNewlines(value []byte) []byte {
	return bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
}

func TestCLIV2WindowsServiceStop(t *testing.T) {
	runV2Case(t, v2Case{Name: "Windows selected task remains disabled", Scenario: "windows-stop", Args: []string{"service", "stop", "--component", "proxy", "--json"}, Exit: 0, Command: "service stop", WantJSON: `{"component":"proxy"}`, Forbid: []string{"refresh-manager"}, Calls: map[string]int{"proxy-disable": 1, "proxy-stop": 1}})
}
func init() {
	registerV2Fixture("windows-stop", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		p, r := newWindowsTaskServiceHarness(t)
		for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
			name, _, _, _ := windowsTaskValues(kind)
			r.tasks[name], _ = renderWindowsTaskDefinition(kind, testWindowsSID, p.executable, 1800)
			r.running[name] = true
		}
		r.folderExists = true
		r.folderSDDL = windowsTaskSecurityDescriptor(testWindowsSID)
		p.run = func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 2 && args[2] == windowsRefreshTaskPath {
				f.Call("refresh-manager")
			}
			if args[0] == "/Disable" {
				f.Call("proxy-disable")
			}
			if args[0] == "/End" {
				f.Call("proxy-stop")
			}
			return r.Run(ctx, args...)
		}
		l := &serviceLifecycle{Platform: p, Store: &selectedServiceStore{exists: true, record: installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerManual, Executable: p.executable, Version: "fixture", BinaryDigest: strings.Repeat("a", 64), Services: []string{windowsProxyTaskPath, windowsRefreshTaskPath}}}, Executable: p.executable, Version: "fixture", StatusAttempts: 1, MutationLocker: &selectedServiceLock{}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
			}, true
		}
		return f
	})
}
func TestWindowsTaskSelectedAcceptsDisabled(t *testing.T) {
	p, _ := newWindowsTaskServiceHarness(t)
	d, _ := renderWindowsTaskDefinition(windowsProxyTask, testWindowsSID, p.executable, 1800)
	parsed, _ := parseWindowsTaskDefinition(d)
	parsed.Settings.Enabled = windowsTaskBool(false)
	if err := validateWindowsTaskDefinition(parsed, windowsProxyTask, testWindowsSID, p.executable, 1800); err != nil {
		t.Fatal(err)
	}
}

func newSelectedWindowsHarness(t *testing.T) (*serviceLifecycle, *windowsTaskServicePlatform, *fakeWindowsTaskRunner) {
	t.Helper()
	p, r := newWindowsTaskServiceHarness(t)
	r.folderExists = true
	r.folderSDDL = windowsTaskSecurityDescriptor(testWindowsSID)
	for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
		name, _, _, _ := windowsTaskValues(kind)
		r.tasks[name], _ = renderWindowsTaskDefinition(kind, testWindowsSID, p.executable, 1800)
	}
	r.running[windowsProxyTaskPath] = true
	r.enginePIDs[windowsProxyTaskPath] = []uint32{902}
	completed := time.Now().Add(-time.Minute)
	p.completion = func(exe string, roots userdirs.Roots, now time.Time) (serviceRefreshCompletion, error) {
		return serviceRefreshCompletion{CompletedAt: completed, ExitCode: 0}, nil
	}
	p.forceRefresh = func(ctx context.Context, exe string, roots userdirs.Roots) error {
		completed = time.Now()
		return ctx.Err()
	}
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := r.Run(ctx, args...)
		if err == nil && args[0] == "/Run" && args[2] == windowsRefreshTaskPath {
			r.running[windowsRefreshTaskPath] = false
			completed = time.Now()
		}
		return data, err
	}
	l := &serviceLifecycle{Platform: p, Store: &selectedServiceStore{exists: true, record: installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerManual, Executable: p.executable, Version: "fixture", BinaryDigest: strings.Repeat("a", 64), Services: []string{windowsProxyTaskPath, windowsRefreshTaskPath}}}, Executable: p.executable, Version: "fixture", StatusAttempts: 1, MutationLocker: &selectedServiceLock{}, DigestExecutable: func(string) (string, error) { return strings.Repeat("a", 64), nil }}
	l.Store = &windowsSelectedStore{selectedServiceStore: *l.Store.(*selectedServiceStore)}
	return l, p, r
}
func TestWindowsServiceSelectedLifecycleMatrix(t *testing.T) {
	for _, action := range []serviceAction{serviceInstall, serviceStart, serviceStop, serviceRestart, serviceInspect, serviceUninstall} {
		for _, selection := range []serviceSelection{serviceProxy, serviceRefresh, serviceAll} {
			t.Run(string(action)+"/"+string(selection), func(t *testing.T) {
				l, p, r := newSelectedWindowsHarness(t)
				before := map[string][]byte{}
				for k, v := range r.tasks {
					before[k] = append([]byte{}, v...)
				}
				if action == serviceStart {
					for _, kind := range windowsSelectedKinds(selection) {
						if err := p.stopSelected(context.Background(), kind); err != nil {
							t.Fatal(err)
						}
					}
					r.calls = nil
				}
				result, err := l.Selected(context.Background(), action, selection, false)
				if err != nil {
					t.Fatalf("%s: %v (%s)", action, err, result.Rollback)
				}
				for _, kind := range []windowsTaskKind{windowsProxyTask, windowsRefreshTask} {
					id := serviceProxy
					if kind == windowsRefreshTask {
						id = serviceRefresh
					}
					path, _, _, _ := windowsTaskValues(kind)
					if selection != serviceAll && id != selection {
						if !bytes.Equal(before[path], r.tasks[path]) {
							t.Fatal("unselected definition changed")
						}
						for _, call := range r.calls {
							if len(call) > 2 && call[2] == path {
								t.Fatalf("unselected access %v", call)
							}
						}
						continue
					}
					c := result.Status.component(id)
					if action == serviceUninstall {
						if c.Registered {
							t.Fatal("registration survived")
						}
						continue
					}
					if action == serviceStop {
						if c.Running || *c.Observed.Enabled {
							t.Fatal("stop did not persist disabled policy")
						}
					} else if !serviceSelectedHealthy(id, c) {
						t.Fatalf("not healthy: %+v", c)
					}
				}
			})
		}
	}
}
func TestWindowsServiceDisabledRestartPreservesPolicy(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh} {
		t.Run(string(selection), func(t *testing.T) {
			l, p, r := newSelectedWindowsHarness(t)
			kind := windowsProxyTask
			if selection == serviceRefresh {
				kind = windowsRefreshTask
			}
			if err := p.stopSelected(context.Background(), kind); err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			result, err := l.Selected(context.Background(), serviceRestart, selection, false)
			if err != nil {
				t.Fatalf("restart: %v", err)
			}
			c := result.Status.component(selection)
			if *c.Observed.Enabled {
				t.Fatal("enabled primary automatic triggers")
			}
			if selection == serviceRefresh {
				projected := projectV2ServiceComponent(selection, c, time.Now())
				if projected.Healthy == nil || *projected.Healthy {
					t.Fatal("disabled refresh must remain unhealthy")
				}
			}
			for _, call := range r.calls {
				if call[0] == "/Enable" {
					t.Fatal("transient enable")
				}
			}
			if selection == serviceProxy {
				raw := r.tasks[windowsProxySessionTaskPath]
				if len(raw) == 0 || bytes.Contains(raw, []byte("<Triggers>")) || bytes.Contains(raw, []byte("<RestartOnFailure>")) || bytes.Contains(raw, []byte("<StartWhenAvailable>true")) {
					t.Fatal("automatic auxiliary policy")
				}
				if _, err := p.Snapshot(context.Background()); !errors.Is(err, errServiceUnavailable) {
					t.Fatalf("legacy snapshot = %v", err)
				}
				if _, err := l.Selected(context.Background(), serviceStop, serviceProxy, false); err != nil {
					t.Fatal(err)
				}
				if r.tasks[windowsProxySessionTaskPath] != nil {
					t.Fatal("session orphaned")
				}
			}
		})
	}
}
func TestWindowsServiceSelectedRejectsConflictsBeforeMutation(t *testing.T) {
	for _, mode := range []string{"sid", "folder", "executable", "auxiliary", "extra-action", "extra-trigger"} {
		t.Run(mode, func(t *testing.T) {
			l, p, r := newSelectedWindowsHarness(t)
			switch mode {
			case "sid":
				r.tasks[windowsProxyTaskPath] = bytes.ReplaceAll(r.tasks[windowsProxyTaskPath], []byte(testWindowsSID), []byte("S-1-5-18"))
			case "folder":
				r.folderSDDL = "D:P(A;;FA;;;WD)"
			case "executable":
				p.validateExecutable = func(string) error { return installstate.ErrOwnershipConflict }
			case "auxiliary":
				r.tasks[windowsProxySessionTaskPath], _ = renderWindowsTaskDefinition(windowsProxySessionTask, testWindowsSID, p.executable, 1800)
				r.tasks[windowsProxySessionTaskPath] = bytes.ReplaceAll(r.tasks[windowsProxySessionTaskPath], []byte("proxy start"), []byte("proxy stop"))
			case "extra-action":
				r.tasks[windowsProxyTaskPath] = bytes.Replace(r.tasks[windowsProxyTaskPath], []byte("</Actions>"), []byte("<ComHandler></ComHandler></Actions>"), 1)
			case "extra-trigger":
				r.tasks[windowsProxyTaskPath] = bytes.Replace(r.tasks[windowsProxyTaskPath], []byte("</Triggers>"), []byte("<CalendarTrigger></CalendarTrigger></Triggers>"), 1)
			}
			_, err := l.Selected(context.Background(), serviceStop, serviceProxy, false)
			if !errors.Is(err, installstate.ErrOwnershipConflict) {
				t.Fatalf("conflict=%v", err)
			}
			for _, call := range r.calls {
				if call[0] != "/Query" {
					t.Fatalf("mutation before conflict: %v", call)
				}
			}
		})
	}
}
func TestWindowsServiceSelectedRollbackExactXML(t *testing.T) {
	l, p, r := newSelectedWindowsHarness(t)
	if err := p.StopProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Selected(context.Background(), serviceRestart, serviceProxy, false); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), selectedServiceContextKey{}, true)
	snapshot, err := p.SnapshotSelected(ctx, serviceProxy)
	if err != nil {
		t.Fatal(err)
	}
	refresh := append([]byte{}, r.tasks[windowsRefreshTaskPath]...)
	if err := p.StartProxy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreSelected(ctx, serviceProxy, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, c := range snapshot.Components {
		if !bytes.Equal(c.Definition, r.tasks[c.ID]) {
			t.Fatalf("restore changed XML %s", c.ID)
		}
	}
	if !r.running[windowsProxySessionTaskPath] || !bytes.Equal(refresh, r.tasks[windowsRefreshTaskPath]) {
		t.Fatal("restore changed scope or session")
	}
}
func TestWindowsServiceProcessBinding(t *testing.T) {
	action := windowsServiceProcessIdentity{PID: 42, ParentPID: 7, Created: 200, Executable: `C:\CQ\cq.exe`, SID: testWindowsSID}
	engine := windowsServiceProcessIdentity{PID: 7, Created: 100, Children: 1, OnlyChildPID: 42, SID: testWindowsSID, Executable: `C:\Windows\System32\taskeng.exe`}
	state := windowsTaskRuntimeState{Running: true, EnginePIDs: []uint32{7}, SecurityDescriptor: windowsTaskSecurityDescriptor(testWindowsSID)}
	if !windowsTaskOwnsProcess(state, action, engine, action.Executable, testWindowsSID) {
		t.Fatal("valid direct engine parent rejected")
	}
	for _, mode := range []string{"owner", "parent", "pid-reuse", "executable", "ambiguous", "shared-engine", "child-replaced", "security"} {
		t.Run(mode, func(t *testing.T) {
			a, e, s := action, engine, state
			switch mode {
			case "owner":
				a.SID = "S-1-5-18"
			case "parent":
				a.ParentPID = 9
			case "pid-reuse":
				e.Created = 300
			case "executable":
				a.Executable = `C:\Other\cq.exe`
			case "ambiguous":
				s.EnginePIDs = []uint32{7, 8}
			case "child-replaced":
				e.OnlyChildPID = 99
			case "shared-engine":
				e.Children = 2
			case "security":
				s.SecurityDescriptor = "D:P(A;;FA;;;WD)"
			}
			if windowsTaskOwnsProcess(s, a, e, action.Executable, testWindowsSID) {
				t.Fatal("foreign identity accepted")
			}
		})
	}
	state.EnginePIDs = []uint32{42}
	if !windowsTaskOwnsProcess(state, action, action, action.Executable, testWindowsSID) {
		t.Fatal("stable self engine rejected")
	}
}
func TestWindowsServiceSelectedCancellationNoMutation(t *testing.T) {
	l, p, r := newSelectedWindowsHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	original := p.queryFolder
	p.queryFolder = func(ctx context.Context) (windowsTaskFolderState, error) {
		s, e := original(ctx)
		cancel()
		return s, e
	}
	_, err := l.Selected(ctx, serviceStop, serviceProxy, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	for _, call := range r.calls {
		if call[0] != "/Query" {
			t.Fatalf("post-cancel mutation %v", call)
		}
	}
}
func TestWindowsServiceRefreshReceiptCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, close := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer close()
	for _, ctx := range []context.Context{cancelled, expired} {
		called := false
		err := recordServiceRefreshContext(ctx, "fixture", userdirs.Roots{State: filepath.Join(t.TempDir(), "must-not-exist")}, func() error { called = true; return nil }, time.Now)
		if !errors.Is(err, ctx.Err()) || called {
			t.Fatalf("error=%v called=%v", err, called)
		}
	}
}

type windowsSelectedStore struct{ selectedServiceStore }

func (s *windowsSelectedStore) Save(record installstate.Record) error {
	if err := validateAbsoluteWindowsExecutable(record.Executable); err != nil {
		return err
	}
	check := record
	check.Executable = filepath.Join(os.TempDir(), "fixture.exe")
	if err := check.Validate(); err != nil {
		return err
	}
	s.record = record
	s.exists = true
	return nil
}
func TestWindowsServiceProjectionPaths(t *testing.T) {
	for _, value := range []string{`C:\`, `C:\Users\Test & Co\cq.exe`} {
		if !validWindowsServicePath(value) {
			t.Fatalf("valid path %q", value)
		}
	}
	for _, value := range []string{`C:relative`, `\\server\share`, `C:\foo\..\bar`, `C:\foo\`, `C:\foo:bar`, "C:\x00foo", `/tmp/cq`} {
		if validWindowsServicePath(value) {
			t.Fatalf("unsafe path %q", value)
		}
	}
	l, p, _ := newSelectedWindowsHarness(t)
	status, err := p.InspectSelected(context.Background(), serviceProxy)
	if err != nil {
		t.Fatal(err)
	}
	result := projectV2ServiceComponent(serviceProxy, status.Proxy, time.Now())
	if result.Executable == nil || *result.Executable != l.Executable || result.ConfigDir == nil {
		t.Fatalf("Windows identity lost: %+v", result)
	}
	status.Proxy.Manager = "launchd"
	result = projectV2ServiceComponent(serviceProxy, status.Proxy, time.Now())
	if runtime.GOOS != "windows" && result.Executable != nil {
		t.Fatal("Windows path widened other managers")
	}
}

func TestWindowsServiceSelectedPartialFailureRollback(t *testing.T) {
	for _, mode := range []string{"disable", "end", "run", "aux-run", "restore-fails", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			l, p, r := newSelectedWindowsHarness(t)
			action := serviceStop
			ctx := context.Background()
			if mode == "aux-run" {
				if err := p.StopProxy(ctx); err != nil {
					t.Fatal(err)
				}
				action = serviceRestart
			}
			before := append([]byte{}, r.tasks[windowsProxyTaskPath]...)
			fail := func(args ...string) { r.failOnce[strings.Join(args, "\x00")] = errors.New("fixture mutation failed") }
			switch mode {
			case "disable":
				fail("/Disable", "/TN", windowsProxyTaskPath)
			case "end", "restore-fails":
				fail("/End", "/TN", windowsProxyTaskPath)
			case "run":
				action = serviceRestart
				fail("/Run", "/TN", windowsProxyTaskPath)
			case "aux-run":
				fail("/Run", "/TN", windowsProxySessionTaskPath)
			case "timeout":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				base := p.run
				p.run = func(ctx context.Context, args ...string) ([]byte, error) {
					out, err := base(ctx, args...)
					if args[0] == "/Disable" {
						cancel()
					}
					return out, err
				}
			}
			if mode == "restore-fails" {
				base := p.run
				p.run = func(ctx context.Context, args ...string) ([]byte, error) {
					if args[0] == "/Create" {
						return nil, errors.New("restore denied")
					}
					return base(ctx, args...)
				}
			}
			result, err := l.Selected(ctx, action, serviceProxy, false)
			if err == nil {
				t.Fatal("partial mutation reported success")
			}
			if mode == "timeout" || mode == "restore-fails" {
				if result.Rollback != "failed" {
					t.Fatalf("rollback=%s", result.Rollback)
				}
			} else {
				if result.Rollback != "restored" || !bytes.Equal(before, r.tasks[windowsProxyTaskPath]) {
					t.Fatalf("rollback=%s error=%v", result.Rollback, err)
				}
			}
			if mode != "timeout" && r.tasks[windowsProxySessionTaskPath] != nil {
				t.Fatal("failed launch orphaned session")
			}
		})
	}
}
func TestWindowsServiceRefreshCompletionHealth(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "failed", "missing", "running-previous", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			_, p, r := newSelectedWindowsHarness(t)
			then := time.Now().Add(-time.Minute)
			exit := 0
			switch mode {
			case "stale":
				then = time.Now().Add(-36 * time.Minute)
			case "failed":
				exit = 1
			case "running-previous":
				r.running[windowsRefreshTaskPath] = true
			case "disabled":
				if err := p.StopRefresh(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			p.completion = func(string, userdirs.Roots, time.Time) (serviceRefreshCompletion, error) {
				if mode == "missing" {
					return serviceRefreshCompletion{}, os.ErrNotExist
				}
				return serviceRefreshCompletion{CompletedAt: then, ExitCode: exit}, nil
			}
			status, err := p.InspectSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			got := projectV2ServiceComponent(serviceRefresh, status.Refresh, time.Now())
			if mode == "missing" {
				if got.Healthy != nil || got.State != "indeterminate" {
					t.Fatalf("missing evidence=%+v", got)
				}
				return
			}
			want := mode == "fresh" || mode == "running-previous"
			if got.Healthy == nil || *got.Healthy != want {
				t.Fatalf("health=%+v", got)
			}
			if got.LastRunAt == nil || *got.LastRunAt != then.UTC().Format(time.RFC3339) {
				t.Fatal("completion time lost")
			}
		})
	}
}
func TestWindowsServiceRefreshReceiptRunCancelled(t *testing.T) {
	for _, stage := range []string{"during-run", "before-publication"} {
		t.Run(stage, func(t *testing.T) {
			root := "/state"
			fs := fsutil.NewMemFS()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr := errors.New("refresh failed")
			clockCalls := 0
			now := func() time.Time {
				clockCalls++
				if stage == "before-publication" && clockCalls == 2 {
					cancel()
				}
				return time.Now()
			}
			err := recordServiceRefreshContextWithFS(ctx, fs, "fixture", userdirs.Roots{State: root}, func() error {
				if stage == "during-run" {
					cancel()
				}
				return runErr
			}, now)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, runErr) {
				t.Fatalf("lost cancellation/run failure %v", err)
			}
			if _, err := fs.Stat(filepath.Join(root, serviceRefreshCompletionName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("post-cancel receipt: %v", err)
			}
		})
	}
}

func TestWindowsServiceLegacyUninstallCleansSession(t *testing.T) {
	l, p, r := newSelectedWindowsHarness(t)
	if err := p.StopProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Selected(context.Background(), serviceRestart, serviceProxy, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Preflight(context.Background(), p.executable); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.tasks[windowsProxySessionTaskPath] != nil || r.tasks[windowsProxyTaskPath] != nil || r.tasks[windowsRefreshTaskPath] == nil {
		t.Fatal("legacy cleanup scope failed")
	}
}

func TestWindowsServiceAuxiliaryRequiresRecord(t *testing.T) {
	for _, mode := range []string{"missing-callback", "record-error", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			l, p, r := newSelectedWindowsHarness(t)
			if err := p.StopProxy(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Selected(context.Background(), serviceRestart, serviceProxy, false); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-callback":
				p.verifyProxyOwner = nil
			case "record-error":
				p.verifyProxyOwner = func(context.Context) error { return installstate.ErrOwnershipConflict }
			case "orphan":
				delete(r.tasks, windowsProxyTaskPath)
			}
			r.calls = nil
			if err := p.RemoveProxy(context.Background()); err == nil {
				t.Fatal("unowned auxiliary removed")
			}
			for _, call := range r.calls {
				if call[0] != "/Query" {
					t.Fatalf("mutated without record authority %v", call)
				}
			}
		})
	}
}
func TestWindowsServiceRefreshReceiptExactlyOnce(t *testing.T) {
	root := "/state"
	fs := fsutil.NewMemFS()
	roots := userdirs.Roots{State: root}
	calls := 0
	if err := recordServiceRefreshContextWithFS(context.Background(), fs, "fixture", roots, func() error { calls++; return nil }, time.Now); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(filepath.Join(root, serviceRefreshCompletionName))
	if err != nil {
		t.Fatal(err)
	}
	var got serviceRefreshCompletion
	err = json.Unmarshal(data, &got)
	if err != nil || calls != 1 || got.ExitCode != 0 {
		t.Fatalf("calls=%d receipt=%+v err=%v", calls, got, err)
	}
}

func TestWindowsServiceAbsentAndRepeatedStop(t *testing.T) {
	for _, action := range []serviceAction{serviceInspect, serviceStop, serviceUninstall, serviceStart, serviceRestart} {
		t.Run("absent-"+string(action), func(t *testing.T) {
			l, _, r := newSelectedWindowsHarness(t)
			delete(r.tasks, windowsProxyTaskPath)
			delete(r.running, windowsProxyTaskPath)
			result, err := l.Selected(context.Background(), action, serviceProxy, false)
			if action == serviceStart || action == serviceRestart {
				if !errors.Is(err, installstate.ErrNotInstalled) {
					t.Fatalf("missing registration=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Status.Proxy.Observed == nil || result.Status.Proxy.Registered {
				t.Fatal("absence not observed")
			}
			for _, call := range r.calls {
				if call[0] != "/Query" {
					t.Fatalf("mutated absent registration %v", call)
				}
			}
		})
	}
	l, _, r := newSelectedWindowsHarness(t)
	for i := 0; i < 2; i++ {
		if _, err := l.Selected(context.Background(), serviceStop, serviceProxy, false); err != nil {
			t.Fatal(err)
		}
	}
	before := append([]byte{}, r.tasks[windowsProxyTaskPath]...)
	if _, err := l.Selected(context.Background(), serviceInspect, serviceProxy, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, r.tasks[windowsProxyTaskPath]) {
		t.Fatal("status changed disabled policy")
	}
}
