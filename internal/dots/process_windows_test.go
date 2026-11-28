//go:build windows

package dots

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsRealtimePortAllowsMissingPrivateFile(t *testing.T) {
	root := filepath.Join(windowsDotsTestRoot(t), "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "realtime-port")
	if port, err := realtimePort(path, root, true); err != nil || port != 0 {
		t.Fatalf("fresh realtime port = %d, %v", port, err)
	}
	if _, err := realtimePort(path, root, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("required realtime port returned %v", err)
	}
}

func TestWindowsDesktopStagesBeforeMappedWatchdog(t *testing.T) {
	const childEnv = "CQ_TEST_WINDOWS_MAPPED_DOTS_WATCHDOG"
	if os.Getenv(childEnv) == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	anchor := windowsDotsTestRoot(t)
	s := &Settings{RuntimeRoot: filepath.Join(anchor, "runtime")}
	if err := PrivateDirectory(s.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(anchor, "cq.exe")
	if err := os.WriteFile(source, []byte("previous version"), 0o600); err != nil {
		t.Fatal(err)
	}
	windowsDotsTestACL(t, source, false)
	if _, err := StageExecutable(source, s.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, current, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := desktopDefaults().stage(s, source)
	if err != nil {
		t.Fatal(err)
	}
	watchdog := exec.Command(staged, "-test.run=^TestWindowsDesktopStagesBeforeMappedWatchdog$")
	watchdog.Env = append(os.Environ(), childEnv+"=1")
	if err := watchdog.Start(); err != nil {
		t.Fatalf("launch current staged watchdog: %v", err)
	}
	t.Cleanup(func() {
		_ = watchdog.Process.Kill()
		_ = watchdog.Wait()
	})
	if reused, err := StageExecutable(source, s.RuntimeRoot); err != nil || reused != staged {
		t.Fatalf("reuse staged executable with watchdog running: %q, %v", reused, err)
	}
}

func TestWindowsProcessIdentityReadsCurrentOwnedImage(t *testing.T) {
	handle, err := windowsOwnedProcess(os.Getpid(), windows.PROCESS_VM_READ)
	if err != nil {
		t.Fatal(err)
	}
	image, imageErr := windowsProcessImage(handle)
	command, commandErr := windowsProcessCommandLine(handle)
	_ = windows.CloseHandle(handle)
	if imageErr != nil || commandErr != nil || len(command) == 0 {
		t.Fatalf("Windows process queries: image=%v command=%v", imageErr, commandErr)
	}
	if !strings.EqualFold(filepath.Base(command[0]), filepath.Base(image)) {
		t.Fatal("Windows command line image differs from the process image")
	}
	uid, args, err := dotsProcessArguments(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if uid != os.Getuid() || !strings.EqualFold(args[0], executable) || len(args) < 2 {
		t.Fatalf("Windows process identity differs: uid=%d image=%q", uid, args[0])
	}
	parent, err := dotsParentPID(context.Background(), os.Getpid())
	if err != nil || parent <= 0 {
		t.Fatalf("Windows process parent unavailable: pid=%d err=%v", parent, err)
	}
	if err := dotsTerminate(os.Getpid()); err == nil {
		t.Fatal("non-helper process was eligible for termination")
	}
}

func TestWindowsProcessPredicatesRequireExactOwnedRuntime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	s := &Settings{RuntimeRoot: root, AppPath: filepath.Join(t.TempDir(), "OpenAI.Codex_26.1120.5083.0_x64__2p2nqsd0c76g0"), IntegrationMode: "native"}
	app := nativeAppExecutable(s)
	if !normalAppArguments(s, os.Getuid(), []string{app}) || normalAppArguments(s, os.Getuid(), []string{app, "--user-data-dir=other"}) {
		t.Fatal("normal app predicate did not distinguish the ordinary profile")
	}
	if !nativeDebugFlagsMatch([]string{"--remote-debugging-pipe", "--remote-debugging-io-pipes=101,202"}) || nativeDebugFlagsMatch([]string{"--remote-debugging-pipe"}) || nativeDebugFlagsMatch([]string{"--remote-debugging-pipe", "--remote-debugging-io-pipes=1,1"}) {
		t.Fatal("Windows CDP pipe predicate accepted the wrong transport")
	}
	helper := helperExecutable(s)
	args := []string{helper, "__dots", "relay", filepath.Dir(root)}
	if role := runtimeRole(s, os.Getpid(), os.Getuid(), args, "", nil); role != "relay" {
		t.Fatalf("exact Windows helper role = %q", role)
	}
	args[3] = filepath.Dir(t.TempDir())
	if role := runtimeRole(s, os.Getpid(), os.Getuid(), args, "", nil); role != "" {
		t.Fatalf("foreign Windows helper root accepted: %q", role)
	}
}

func TestWindowsNormalAppRunningIgnoresLingeringChromiumProcesses(t *testing.T) {
	s := &Settings{AppPath: t.TempDir(), IntegrationMode: "native"}
	app := nativeAppExecutable(s)
	processes := [][]string{
		{app, "--type=gpu-process"},
		{app, "--type=utility"},
		{app, "--type=renderer"},
	}
	ops := processOperations{
		list: func(context.Context) ([]int, error) {
			pids := make([]int, len(processes))
			for i := range pids {
				pids[i] = i + 1
			}
			return pids, nil
		},
		arguments: func(_ context.Context, pid int) (int, []string, error) {
			return os.Getuid(), processes[pid-1], nil
		},
	}
	if running, err := normalAppRunning(t.Context(), s, ops); err != nil || running {
		t.Fatalf("lingering Chromium children block a cold Dots launch: running=%t, err=%v", running, err)
	}
	processes = append(processes, []string{app})
	if running, err := normalAppRunning(t.Context(), s, ops); err != nil || !running {
		t.Fatalf("the active Desktop process did not protect its session: running=%t, err=%v", running, err)
	}
}

func TestWindowsNativeAppRoleRequiresBrokerStateAndBothPipeFlags(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	s := &Settings{RuntimeRoot: root, AppPath: filepath.Join(anchor, "OpenAI.Codex_26.1120.5083.0_x64__2p2nqsd0c76g0"), IntegrationMode: "native"}
	const pid = 42
	if err := SaveJSON(filepath.Join(root, nativeStateName), map[string]int{"app_pid": pid}); err != nil {
		t.Fatal(err)
	}
	args := []string{nativeAppExecutable(s), "--cq-dots-runtime=" + root, "--remote-debugging-pipe", "--remote-debugging-io-pipes=101,202"}
	if role := runtimeRole(s, pid, os.Getuid(), args, "", nil); role != "app" {
		t.Fatalf("exact Windows app role = %q", role)
	}
	if role := runtimeRole(s, pid+1, os.Getuid(), args, "", nil); role != "" {
		t.Fatalf("wrong Windows app PID accepted: %q", role)
	}
	if role := runtimeRole(s, pid, os.Getuid(), args[:3], "", nil); role != "" {
		t.Fatalf("Windows app without handle switch accepted: %q", role)
	}
	if role := windowsStoppableRole(s, pid, nativeAppExecutable(s), args); role != "app" {
		t.Fatalf("owned Windows Dots app cannot be stopped: %q", role)
	}
	if role := windowsStoppableRole(s, pid, nativeAppExecutable(s), args[:3]); role != "" {
		t.Fatalf("Windows app without handle switch can be stopped: %q", role)
	}
	if role := windowsStoppableRole(s, pid+1, nativeAppExecutable(s), args); role != "" {
		t.Fatalf("foreign Windows app PID can be stopped: %q", role)
	}
	broker := []string{helperExecutable(s), "__dots", "native-broker", filepath.Dir(root)}
	if !nativeBrokerMatches(s, os.Getuid(), broker, "") {
		t.Fatal("exact Windows broker identity rejected")
	}
}
