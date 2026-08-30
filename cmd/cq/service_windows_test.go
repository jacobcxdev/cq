//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
