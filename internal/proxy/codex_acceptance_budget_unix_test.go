//go:build darwin || linux

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestCodexInstalledWebSocketValidationLegacyWrapperHeldPipe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("actual installed runner fixture uses Darwin confinement")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then\n /bin/sleep 4 &\n echo 'codex-cli 0.1.0'\n exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(executable, []byte(script), 0500); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	// The descendant has a finite lifetime even on the broken Background path.
	// Wait through that fixture lifetime after an early legacy return; no claim
	// is made that the production runner reaps arbitrary descendants.
	t.Cleanup(func() {
		if remaining := time.Until(started.Add(4500 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
	})
	_, err := RunCodexInstalledWebSocketValidation(context.Background(), "test", "0.1.0", executable, filepath.Join(dir, "state"))
	elapsed := time.Since(started)
	if !errors.Is(err, ErrCodexValidationClientUnavailable) || elapsed > 3*time.Second {
		t.Fatalf("legacy wrapper lost bounded pipe drain: error=%v elapsed=%s", err, elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("fixture did not exercise legacy pipe drain: %s", elapsed)
	}
}
