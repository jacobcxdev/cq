//go:build windows

package installer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const windowsExecutableRemovalHelper = "CQ_WINDOWS_EXECUTABLE_REMOVAL_HELPER"

func TestWindowsRemoveExecutableWaitsForImageRelease(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
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
	for attempt := 0; attempt < 100; attempt++ {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if attempt == 99 {
			t.Fatal("timed out waiting for running executable")
		}
		time.Sleep(10 * time.Millisecond)
	}

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
