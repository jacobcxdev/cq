//go:build windows

package main

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestWindowsServiceCurrentSIDUsesProcessToken(t *testing.T) {
	sid, err := currentWindowsServiceSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "S-") {
		t.Fatalf("SID = %q", sid)
	}
}

func TestWindowsServiceLifecycleBindsTaskContract(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "cq.exe")
	if err := os.WriteFile(executable, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	roots := userdirs.Roots{State: filepath.Join(root, "state")}
	lifecycle := newWindowsServiceLifecycle(
		executable,
		testWindowsSID,
		roots,
		func(context.Context, ...string) ([]byte, error) { return nil, nil },
		func(context.Context, string) (windowsTaskRuntimeState, error) { return windowsTaskRuntimeState{}, nil },
		func(context.Context, string) componentStatus { return componentStatus{} },
	)
	platform, ok := lifecycle.Platform.(*windowsTaskServicePlatform)
	if !ok {
		t.Fatalf("platform = %T", lifecycle.Platform)
	}
	if platform.sid != testWindowsSID || platform.executable != executable || platform.temporaryRoot != filepath.Join(roots.State, "task-xml") {
		t.Fatalf("platform = %#v", platform)
	}
}

func TestWindowsTaskFolderSecurityLifecycleNative(t *testing.T) {
	if os.Getenv("CQ_NATIVE_WINDOWS_SCHEDULER_TEST") != "1" {
		t.Skip("set CQ_NATIVE_WINDOWS_SCHEDULER_TEST=1 on an isolated Windows host")
	}
	ctx := context.Background()
	before, err := queryWindowsTaskFolderState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Exists {
		t.Fatal("refusing to replace existing CQ task folder")
	}
	sid, err := currentWindowsServiceSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := createWindowsTaskFolder(ctx, windowsTaskSecurityDescriptor(sid)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeWindowsTaskFolderIfEmpty(context.Background()); err != nil {
			t.Errorf("cleanup task folder: %v", err)
		}
	})
	created, err := queryWindowsTaskFolderState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Exists || !validWindowsTaskSecurityDescriptor(created.SecurityDescriptor, sid, true) {
		t.Fatalf("created folder state = %#v", created)
	}
	if err := removeWindowsTaskFolderIfEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := queryWindowsTaskFolderState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Exists {
		t.Fatalf("task folder remains = %#v", after)
	}
}

func TestWindowsTaskRuntimeStateNative(t *testing.T) {
	if os.Getenv("CQ_NATIVE_WINDOWS_SCHEDULER_TEST") != "1" {
		t.Skip("set CQ_NATIVE_WINDOWS_SCHEDULER_TEST=1 on an isolated Windows host")
	}
	ctx := context.Background()
	before, err := queryWindowsTaskFolderState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Exists {
		t.Fatal("refusing to replace existing CQ task folder")
	}
	sid, err := currentWindowsServiceSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := createWindowsTaskFolder(ctx, windowsTaskSecurityDescriptor(sid)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = runWindowsSchtasks(context.Background(), "/End", "/TN", windowsProxyTaskPath)
		_, _ = runWindowsSchtasks(context.Background(), "/Delete", "/TN", windowsProxyTaskPath, "/F")
		if err := removeWindowsTaskFolderIfEmpty(context.Background()); err != nil {
			t.Errorf("cleanup task folder: %v", err)
		}
	})
	data, err := renderWindowsTaskDefinition(windowsProxyTask, sid, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, 1800)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsTaskDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	definition.Actions.Exec.Arguments = "-NoProfile -NonInteractive -Command Start-Sleep -Seconds 60"
	definition.Actions.Exec.WorkingDirectory = `C:\Windows\System32\WindowsPowerShell\v1.0`
	data, err = xml.MarshalIndent(definition, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(xml.Header), append(data, '\n')...)
	path, cleanup, err := writeTemporaryWindowsTaskXML(t.TempDir(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := runWindowsSchtasks(ctx, "/Create", "/TN", windowsProxyTaskPath, "/XML", path, "/F"); err != nil {
		t.Fatal(err)
	}
	if _, err := runWindowsSchtasks(ctx, "/Run", "/TN", windowsProxyTaskPath); err != nil {
		t.Fatal(err)
	}
	var state windowsTaskRuntimeState
	for attempt := 0; attempt < 20; attempt++ {
		state, err = queryWindowsTaskState(ctx, windowsProxyTaskPath)
		if err == nil && state.Running && len(state.EnginePIDs) == 1 && state.EnginePIDs[0] != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || len(state.EnginePIDs) != 1 || state.EnginePIDs[0] == 0 || !validWindowsTaskSecurityDescriptor(state.SecurityDescriptor, sid, false) {
		t.Fatalf("runtime state = %#v", state)
	}
	executable, err := queryWindowsProcessExecutable(state.EnginePIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !equalWindowsPath(executable, definition.Actions.Exec.Command) {
		t.Fatalf("EnginePID executable = %q, want %q", executable, definition.Actions.Exec.Command)
	}
}

func TestWindowsServiceRuntimeInspectionRequiresMatchingListenerProcessAndHealth(t *testing.T) {
	oldPort := loadWindowsProxyPort
	oldNetstat := runWindowsNetstat
	oldExecutable := windowsProcessExecutable
	oldHealth := checkWindowsProxyHealth
	t.Cleanup(func() {
		loadWindowsProxyPort = oldPort
		runWindowsNetstat = oldNetstat
		windowsProcessExecutable = oldExecutable
		checkWindowsProxyHealth = oldHealth
	})
	loadWindowsProxyPort = func() (int, error) { return 19280, nil }
	runWindowsNetstat = func(context.Context) ([]byte, error) {
		return []byte("  TCP    127.0.0.1:19280    0.0.0.0:0    LISTENING    4120\r\n"), nil
	}
	windowsProcessExecutable = func(pid uint32) (string, error) {
		if pid != 4120 {
			t.Fatalf("PID = %d", pid)
		}
		return `C:\Program Files\cq\cq.exe`, nil
	}
	checkWindowsProxyHealth = func(_ context.Context, address string) error {
		if address != "127.0.0.1:19280" {
			t.Fatalf("address = %q", address)
		}
		return nil
	}

	status := inspectWindowsProxyRuntime(context.Background(), `C:\Program Files\cq\cq.exe`)
	if !status.Running || !status.Healthy || status.PID != 4120 || status.Listener != "127.0.0.1:19280" || status.LiveExecutable != `C:\Program Files\cq\cq.exe` {
		t.Fatalf("status = %#v", status)
	}
	windowsProcessExecutable = func(uint32) (string, error) { return `C:\Other\cq.exe`, nil }
	status = inspectWindowsProxyRuntime(context.Background(), `C:\Program Files\cq\cq.exe`)
	if status.Healthy || status.Error == "" {
		t.Fatalf("mismatched status = %#v", status)
	}
	windowsProcessExecutable = func(uint32) (string, error) { return `C:\Program Files\cq\cq.exe`, nil }
	checkWindowsProxyHealth = func(context.Context, string) error { return errors.New("down") }
	status = inspectWindowsProxyRuntime(context.Background(), `C:\Program Files\cq\cq.exe`)
	if status.Healthy || !strings.Contains(status.Error, "health") {
		t.Fatalf("unhealthy status = %#v", status)
	}
}
