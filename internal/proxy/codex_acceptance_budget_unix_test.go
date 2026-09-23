//go:build darwin || linux

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCodexInstalledWebSocketValidationOwnedProcessBudget(t *testing.T) {
	for _, earlyCancel := range []bool{false, true} {
		t.Run(strconv.FormatBool(earlyCancel), func(t *testing.T) {
			dir := t.TempDir()
			parentFile := filepath.Join(dir, "parent")
			descendantFile := filepath.Join(dir, "descendant")
			parent := context.Background()
			cleanup, stopCleanup := context.WithTimeout(parent, 240*time.Millisecond)
			defer stopCleanup()
			work, stopWork := context.WithTimeout(cleanup, 200*time.Millisecond)
			defer stopWork()
			if earlyCancel {
				stopWork()
				stopCleanup()
				cleanup, stopCleanup = context.WithTimeout(parent, 200*time.Millisecond)
				defer stopCleanup()
				work = cleanup
			}
			// The shell is the direct owned child. Its separate sleep process deliberately
			// holds the output pipe; the fixture owns and terminates that descendant below.
			t.Cleanup(func() {
				data, err := os.ReadFile(descendantFile)
				if err == nil {
					pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
					if e == nil && pid > 1 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			execution := codexAcceptanceExecution{executable: "/bin/sh", args: []string{"-c", `echo $$ > "$1"; sleep 10 & echo $! > "$2"; wait`, "cq-fixture", parentFile, descendantFile}, command: codexAcceptanceCommand{cleanupContext: cleanup, captureOutput: true, env: []string{"PATH=/usr/bin:/bin"}, dir: dir}}
			started := time.Now()
			_, err := runCodexAcceptanceExecution(work, execution)
			if err == nil || time.Since(started) > 700*time.Millisecond {
				t.Fatalf("held pipe exceeded original budget: %v, %s", err, time.Since(started))
			}
			data, err := os.ReadFile(parentFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("direct child not reaped: %v", err)
			}
		})
	}
}

func TestCodexInstalledWebSocketValidationNormalExitHeldPipe(t *testing.T) {
	dir := t.TempDir()
	childFile := filepath.Join(dir, "descendant")
	parentFile := filepath.Join(dir, "parent")
	t.Cleanup(func() {
		data, _ := os.ReadFile(childFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	cleanup, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runCodexAcceptanceExecution(cleanup, codexAcceptanceExecution{executable: "/bin/sh", args: []string{"-c", `echo $$ > "$2"; sleep 10 & echo $! > "$1"; sleep .3; exit 0`, "cq-fixture", childFile, parentFile}, command: codexAcceptanceCommand{cleanupContext: cleanup, captureOutput: true, env: []string{"PATH=/usr/bin:/bin"}, dir: dir}})
	if err == nil || time.Since(started) > 550*time.Millisecond {
		t.Fatalf("normal exit held pipe exceeded budget: %v, %s", err, time.Since(started))
	}
	data, readErr := os.ReadFile(parentFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("normal direct child not reaped: %v", err)
	}
}

func TestCodexInstalledWebSocketValidationOwnedProcessFailures(t *testing.T) {
	for _, test := range []struct {
		name, executable, script string
		capture                  bool
	}{
		{"start", "/nonexistent/cq-fixture", "", true},
		{"overflow", "/bin/sh", "head -c 1048576 /dev/zero", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := runCodexAcceptanceExecution(ctx, codexAcceptanceExecution{executable: test.executable, args: []string{"-c", test.script}, command: codexAcceptanceCommand{cleanupContext: ctx, captureOutput: test.capture, env: []string{"PATH=/usr/bin:/bin"}, dir: t.TempDir()}})
			if err == nil {
				t.Fatal("invalid execution succeeded")
			}
		})
	}
}
