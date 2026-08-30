package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
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
		{"/Query", "/TN", windowsProxyTaskPath, "/XML"},
		{"/Query", "/TN", windowsRefreshTaskPath, "/XML"},
		{"/Query", "/TN", windowsProxyTaskPath, "/XML"},
		{"/Create", "/TN", windowsProxyTaskPath, "/XML", runner.xmlPaths[0], "/F"},
		{"/Run", "/TN", windowsProxyTaskPath},
		{"/Query", "/TN", windowsRefreshTaskPath, "/XML"},
		{"/Create", "/TN", windowsRefreshTaskPath, "/XML", runner.xmlPaths[1], "/F"},
		{"/Run", "/TN", windowsRefreshTaskPath},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("schtasks calls = %#v\nwant = %#v", runner.calls, want)
	}
	if len(runner.tasks) != 2 || !runner.running[windowsProxyTaskPath] || !runner.running[windowsRefreshTaskPath] {
		t.Fatalf("tasks/running = %#v/%#v", runner.tasks, runner.running)
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
	if !hasWindowsTaskCall(runner.calls, []string{"/End", "/TN", windowsRefreshTaskPath}) || !hasWindowsTaskCall(runner.calls, []string{"/Delete", "/TN", windowsProxyTaskPath, "/F"}) {
		t.Fatalf("removal calls = %#v", runner.calls)
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

func newWindowsTaskServiceHarness(t *testing.T) (*windowsTaskServicePlatform, *fakeWindowsTaskRunner) {
	t.Helper()
	runner := &fakeWindowsTaskRunner{
		tasks:      map[string][]byte{},
		running:    map[string]bool{},
		lastResult: map[string]uint32{},
		failOnce:   map[string]error{},
	}
	platform := &windowsTaskServicePlatform{
		sid:             testWindowsSID,
		executable:      `C:\Users\Test\cq.exe`,
		temporaryRoot:   t.TempDir(),
		refreshInterval: 1800,
		run:             runner.Run,
		queryState:      runner.State,
		inspectProxy: func(_ context.Context, executable string) componentStatus {
			return componentStatus{ID: windowsProxyTaskPath, Manager: "task-scheduler", Running: true, LiveExecutable: executable, PID: 902, Listener: "127.0.0.1:19280", Healthy: true}
		},
	}
	return platform, runner
}

type fakeWindowsTaskRunner struct {
	calls      [][]string
	xmlPaths   []string
	tasks      map[string][]byte
	running    map[string]bool
	lastResult map[string]uint32
	failOnce   map[string]error
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
		path, xmlPath := args[2], args[4]
		definition, err := os.ReadFile(xmlPath)
		if err != nil {
			return nil, err
		}
		runner.xmlPaths = append(runner.xmlPaths, xmlPath)
		runner.tasks[path] = definition
		return nil, nil
	case "/run":
		path := args[2]
		if runner.running[path] {
			return nil, windowsTaskCommandError{Code: windowsTaskAlreadyRunning, Output: "already running"}
		}
		runner.running[path] = true
		return nil, nil
	case "/end":
		path := args[2]
		if runner.tasks[path] == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
		}
		runner.running[path] = false
		return nil, nil
	case "/delete":
		path := args[2]
		if runner.tasks[path] == nil {
			return nil, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
		}
		delete(runner.tasks, path)
		delete(runner.running, path)
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected schtasks command %q", args[0])
	}
}

func (runner *fakeWindowsTaskRunner) State(_ context.Context, path string) (windowsTaskRuntimeState, error) {
	if runner.tasks[path] == nil {
		return windowsTaskRuntimeState{}, windowsTaskCommandError{Code: windowsTaskNotFound, Output: "cannot find the file"}
	}
	return windowsTaskRuntimeState{Running: runner.running[path], LastResult: runner.lastResult[path], HasLastResult: true}, nil
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
	if string(data) != string(want) {
		t.Fatalf("definition differs\ngot:\n%s\nwant:\n%s", data, want)
	}
}
