//go:build windows

package installer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"golang.org/x/sys/windows"
)

const windowsExecutableRemovalHelper = "CQ_WINDOWS_EXECUTABLE_REMOVAL_HELPER"

func TestWindowsReplaceExecutableRemovesExistingDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "cq.exe.candidate")
	destination := filepath.Join(directory, "cq.exe")
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(context.Background(), fsutil.OSFileSystem{}, source, destination); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Fatalf("replacement body = %q", body)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("replacement source remains: %v", err)
	}
}

func TestWindowsReplaceExecutableAllowsMissingDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "cq.exe.candidate")
	destination := filepath.Join(directory, "cq.exe")
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(context.Background(), fsutil.OSFileSystem{}, source, destination); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Fatalf("replacement body = %q", body)
	}
}

func TestWindowsReplaceExecutableWaitsForExistingImageRelease(t *testing.T) {
	body, err := os.ReadFile(mustExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "cq.exe.candidate")
	destination := filepath.Join(directory, "cq.exe")
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, body, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(destination, "-test.run=^TestWindowsRemoveExecutableHelper$")
	command.Env = append(os.Environ(), windowsExecutableRemovalHelper+"="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	waitForWindowsExecutableRemovalHelper(t, marker)

	if err := replaceExecutable(context.Background(), fsutil.OSFileSystem{}, source, destination); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	replaced, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(replaced) != "new" {
		t.Fatalf("replacement body = %q", replaced)
	}
}

func TestWindowsRemoveExecutableWaitsForImageRelease(t *testing.T) {
	body, err := os.ReadFile(mustExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "cq.exe")
	if err := os.WriteFile(target, body, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(target, "-test.run=^TestWindowsRemoveExecutableHelper$")
	command.Env = append(os.Environ(), windowsExecutableRemovalHelper+"="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	waitForWindowsExecutableRemovalHelper(t, marker)

	if err := removeExecutable(context.Background(), fsutil.OSFileSystem{}, target); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("removed executable stat error = %v", err)
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func waitForWindowsExecutableRemovalHelper(t *testing.T, marker string) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for running executable")
}

func TestWindowsInstallerRollbackWaitsForCandidateImageRelease(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old-binary")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.statusErrors = []error{errors.New("candidate unhealthy"), nil}
	harness.fsys.failRemoveTarget = harness.installer.Installation.Executable
	harness.fsys.failRemoveCount = 2
	harness.fsys.failRemoveError = windows.ERROR_ACCESS_DENIED

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "candidate unhealthy") {
		t.Fatalf("Install() error = %v", err)
	}
	if errors.Is(err, ErrRollbackUnverified) {
		t.Fatalf("rollback was not verified: %v", err)
	}
	if harness.fsys.failRemoveCount != 0 {
		t.Fatalf("candidate removal retries remaining = %d", harness.fsys.failRemoveCount)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
}

func TestWindowsRemoveExecutableStopsOnNonRetryableError(t *testing.T) {
	harness := newInstallerHarness(t)
	target := harness.installer.Installation.Executable
	if err := harness.fsys.WriteFile(target, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := errors.New("remove failed")
	harness.fsys.failRemoveTarget = target
	harness.fsys.failRemoveCount = 2
	harness.fsys.failRemoveError = want

	err := removeExecutableWithRetry(context.Background(), harness.fsys, target, 3, time.Millisecond)
	if !errors.Is(err, want) {
		t.Fatalf("removeExecutableWithRetry() error = %v", err)
	}
	if harness.fsys.failRemoveCount != 1 {
		t.Fatalf("non-retryable removal attempts = %d, want 1", 2-harness.fsys.failRemoveCount)
	}
}

func TestWindowsRemoveExecutableHonoursCancellation(t *testing.T) {
	harness := newInstallerHarness(t)
	target := harness.installer.Installation.Executable
	harness.fsys.failRemoveTarget = target
	harness.fsys.failRemoveCount = 2
	harness.fsys.failRemoveError = windows.ERROR_ACCESS_DENIED
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := removeExecutableWithRetry(ctx, harness.fsys, target, 3, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removeExecutableWithRetry() error = %v", err)
	}
	if harness.fsys.failRemoveCount != 1 {
		t.Fatalf("cancelled removal attempts = %d, want 1", 2-harness.fsys.failRemoveCount)
	}
}

func TestWindowsRemoveExecutableStopsAfterBoundedAttempts(t *testing.T) {
	harness := newInstallerHarness(t)
	target := harness.installer.Installation.Executable
	harness.fsys.failRemoveTarget = target
	harness.fsys.failRemoveCount = 3
	harness.fsys.failRemoveError = windows.ERROR_SHARING_VIOLATION

	err := removeExecutableWithRetry(context.Background(), harness.fsys, target, 2, time.Millisecond)
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("removeExecutableWithRetry() error = %v", err)
	}
	if harness.fsys.failRemoveCount != 1 {
		t.Fatalf("removal attempts = %d, want 2", 3-harness.fsys.failRemoveCount)
	}
}

func TestWindowsRemoveExecutableHelper(t *testing.T) {
	marker := os.Getenv(windowsExecutableRemovalHelper)
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(300 * time.Millisecond)
	os.Exit(0)
}
