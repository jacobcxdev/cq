//go:build windows

package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWindowsNativeSpawnChild(t *testing.T) {
	if os.Getenv("CQ_WINDOWS_NATIVE_SPAWN_FIXTURE") != "1" {
		return
	}
	var handles []string
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--remote-debugging-io-pipes=") {
			handles = strings.Split(strings.TrimPrefix(arg, "--remote-debugging-io-pipes="), ",")
		}
	}
	if len(handles) != 2 {
		os.Exit(90)
	}
	readHandle, readErr := strconv.ParseUint(handles[0], 10, 64)
	writeHandle, writeErr := strconv.ParseUint(handles[1], 10, 64)
	if readErr != nil || writeErr != nil {
		os.Exit(91)
	}
	input := os.NewFile(uintptr(readHandle), "cdp-input")
	output := os.NewFile(uintptr(writeHandle), "cdp-output")
	defer input.Close()
	defer output.Close()
	data, err := io.ReadAll(io.LimitReader(input, 4096))
	if err != nil {
		os.Exit(92)
	}
	report := map[string]any{"pid": os.Getpid(), "payload": string(data), "args": os.Args, "value": os.Getenv("CQ_WINDOWS_NATIVE_SPAWN_VALUE")}
	if json.NewEncoder(output).Encode(report) != nil {
		os.Exit(93)
	}
	os.Exit(23)
}

func TestWindowsNativeSpawnInheritedPipesAndArguments(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childRead, parentWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer childRead.Close()
	defer parentWrite.Close()
	parentRead, childWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentRead.Close()
	defer childWrite.Close()
	args := []string{"-test.run=^TestWindowsNativeSpawnChild$", "--", "literal space", "$(not-a-command)"}
	env := []string{"CQ_WINDOWS_NATIVE_SPAWN_FIXTURE=1", "CQ_WINDOWS_NATIVE_SPAWN_VALUE=literal value = yes"}
	app, err := startNativeApp(executable, args, env, childRead, childWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Kill(); _, _ = app.Wait() }()
	_ = childRead.Close()
	_ = childWrite.Close()
	payload := "private CDP payload\x00with literal data"
	if _, err := io.WriteString(parentWrite, payload); err != nil {
		t.Fatal(err)
	}
	_ = parentWrite.Close()
	var report struct {
		PID     int      `json:"pid"`
		Payload string   `json:"payload"`
		Args    []string `json:"args"`
		Value   string   `json:"value"`
	}
	read := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				read <- io.ErrUnexpectedEOF
			}
		}()
		read <- json.NewDecoder(io.LimitReader(parentRead, 8192)).Decode(&report)
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = app.Kill()
		t.Fatal("Windows native child did not reply")
	}
	if report.PID != app.Pid || report.Payload != payload || report.Value != "literal value = yes" || !strings.Contains(strings.Join(report.Args, "|"), "literal space|$(not-a-command)") {
		t.Fatalf("Windows native spawn changed process contract: %#v", report)
	}
	if !bytes.Contains([]byte(strings.Join(report.Args, "\n")), []byte("--remote-debugging-pipe")) || !nativeDebugFlagsMatch(report.Args[len(report.Args)-2:]) {
		t.Fatal("Windows launch omitted Electron's pipe gate or Chromium's handle switch")
	}
	state, err := app.Wait()
	if err != nil || state.ExitCode() != 23 {
		t.Fatalf("Windows native spawn exit: %v %v", state, err)
	}
}

func TestWindowsNativeSpawnRejectsDebugFlagAndMissingExecutable(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	for _, tc := range []struct {
		executable string
		args       []string
	}{
		{filepath.Join(t.TempDir(), "missing.exe"), nil},
		{os.Args[0], []string{"--remote-debugging-port=1"}},
		{os.Args[0], []string{"--remote-debugging-io-pipes=1,2"}},
	} {
		app, err := startNativeApp(tc.executable, tc.args, nil, input, output)
		if app != nil {
			_ = app.Kill()
			_, _ = app.Wait()
			t.Fatal("invalid Windows native spawn created a process")
		}
		if err == nil {
			t.Fatal("invalid Windows native spawn accepted")
		}
	}
}

// This opt-in test runs only on a fresh hosted runner. It does not read or
// modify a developer's Desktop profile, credentials or existing process.
func TestWindowsNativePipeInstalledApp(t *testing.T) {
	if os.Getenv("CQ_DOTS_WINDOWS_PIPE_ACCEPTANCE") != "1" {
		t.Skip("set CQ_DOTS_WINDOWS_PIPE_ACCEPTANCE=1 on a fresh hosted Windows runner")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_OS") != "Windows" {
		t.Fatal("installed Desktop pipe acceptance requires a fresh GitHub-hosted Windows runner")
	}
	root := os.Getenv("CQ_DOTS_WINDOWS_PACKAGE")
	if !filepath.IsAbs(root) {
		t.Fatal("CQ_DOTS_WINDOWS_PACKAGE must name the registered package root")
	}
	pin, executable, err := QualifyWindowsApp(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckWindowsApp(&pin); err != nil {
		t.Fatal(err)
	}
	pids, err := dotsProcessIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		_, args, err := dotsProcessArguments(context.Background(), pid)
		if err == nil && len(args) > 0 && strings.EqualFold(args[0], executable) {
			t.Fatal("official Desktop is already running in the runner session")
		}
	}
	isolated := t.TempDir()
	codexHome := filepath.Join(isolated, "codex-home")
	userData := filepath.Join(isolated, "electron-data")
	for _, path := range []string{codexHome, userData} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	childRead, parentWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer childRead.Close()
	defer parentWrite.Close()
	parentRead, childWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentRead.Close()
	defer childWrite.Close()
	env := setEnvironment(os.Environ(), "CODEX_HOME", codexHome)
	env = setEnvironment(env, "CODEX_ELECTRON_USER_DATA_PATH", userData)
	app, err := startNativeApp(executable, []string{"--user-data-dir=" + userData, "--cq-dots-runtime=" + isolated}, env, childRead, childWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Kill(); _, _ = app.Wait() }()
	_ = childRead.Close()
	_ = childWrite.Close()
	pipe := newNativePipe(parentRead, parentWrite)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := pipe.call(ctx, "Browser.getVersion", nil, ""); err != nil {
		t.Fatalf("official Windows Desktop CDP pipe did not answer: %v", err)
	}
	_, runningArgs, err := dotsProcessArguments(ctx, app.Pid)
	if err != nil || len(runningArgs) < 3 || !strings.EqualFold(runningArgs[0], executable) || !nativeDebugFlagsMatch(runningArgs[len(runningArgs)-2:]) {
		t.Fatalf("official Windows Desktop CDP process identity unavailable: %v", err)
	}
	for {
		result, err := pipe.call(ctx, "Target.getTargets", nil, "")
		if err != nil {
			t.Fatalf("official Windows Desktop renderer target unavailable: %v", err)
		}
		infos, _ := result["targetInfos"].([]any)
		for _, value := range infos {
			info, _ := value.(map[string]any)
			if info["type"] == "page" && info["url"] == "app://-/index.html" {
				t.Log("official Windows Desktop CDP pipe and renderer target verified")
				return
			}
		}
		if ctx.Err() != nil {
			t.Fatal("official Windows Desktop did not expose its renderer target")
		}
		select {
		case <-ctx.Done():
			t.Fatal("official Windows Desktop did not expose its renderer target")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
