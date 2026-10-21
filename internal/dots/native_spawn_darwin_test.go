package dots

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type nativeSpawnReport struct {
	PID        int
	Args       []string
	Value      string
	Payload    string
	StdinEmpty bool
	LeakedFD   bool
}

func TestNativeSpawnChild(t *testing.T) {
	if os.Getenv("CQ_NATIVE_SPAWN_FIXTURE") != "1" {
		return
	}
	input, output := os.NewFile(3, "cdp-input"), os.NewFile(4, "cdp-output")
	data, err := io.ReadAll(io.LimitReader(input, 4096))
	if err != nil {
		os.Exit(90)
	}
	var one [1]byte
	n, stdinErr := os.Stdin.Read(one[:])
	leakFD, _ := strconv.Atoi(os.Getenv("CQ_NATIVE_SPAWN_LEAK_FD"))
	_, fdErr := unix.FcntlInt(uintptr(leakFD), unix.F_GETFD, 0)
	report := nativeSpawnReport{PID: os.Getpid(), Args: os.Args, Value: os.Getenv("CQ_NATIVE_SPAWN_VALUE"), Payload: string(data), StdinEmpty: n == 0 && stdinErr == io.EOF, LeakedFD: !errors.Is(fdErr, unix.EBADF)}
	if json.NewEncoder(output).Encode(report) != nil {
		os.Exit(91)
	}
	os.Exit(23)
}

func TestNativeSpawnPreservedPipesArgumentsAndExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, toApp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer toApp.Close()
	fromApp, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer fromApp.Close()
	defer output.Close()
	if err := fromApp.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Deliberately inheritable: closing only Go-created CLOEXEC descriptors
	// would miss it. The app must inherit exactly stdio and its private pipes.
	leak, err := unix.FcntlInt(toApp.Fd(), unix.F_DUPFD, 200)
	if err != nil {
		t.Fatal(err)
	}
	leakOpen := true
	defer func() {
		if leakOpen {
			_ = unix.Close(leak)
		}
	}()
	args := []string{"-test.run=^TestNativeSpawnChild$", "--", "literal space", "$(not-a-command)", "--remote-debugging-pipe"}
	env := []string{"CQ_NATIVE_SPAWN_FIXTURE=1", "CQ_NATIVE_SPAWN_VALUE=literal value = yes", "CQ_NATIVE_SPAWN_LEAK_FD=" + strconv.Itoa(leak)}
	app, err := startNativeApp(executable, args, env, input, output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Kill(); _, _ = app.Wait() }()
	_ = unix.Close(leak)
	leakOpen = false
	_ = input.Close()
	_ = output.Close()
	payload := "private CDP payload\x00with literal data"
	if _, err = io.WriteString(toApp, payload); err != nil {
		t.Fatal(err)
	}
	_ = toApp.Close()
	var report nativeSpawnReport
	if err = json.NewDecoder(io.LimitReader(fromApp, 8192)).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.PID != app.Pid || !slices.Equal(report.Args, append([]string{executable}, args...)) || report.Value != "literal value = yes" || report.Payload != payload || !report.StdinEmpty || report.LeakedFD {
		t.Fatalf("native launch changed process contract: %#v", report)
	}
	state, err := app.Wait()
	if err != nil || state.ExitCode() != 23 {
		t.Fatalf("native exit: %v, %v", state, err)
	}
}

func TestNativeSpawnRejectedInvalidInputs(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	closed, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	for _, tc := range []struct {
		name, executable string
		args, env        []string
		input            *os.File
	}{
		{name: "missing executable", executable: filepath.Join(t.TempDir(), "missing"), input: input},
		{name: "NUL path", executable: "/bin/sh\x00", input: input},
		{name: "NUL argument", executable: "/bin/sh", args: []string{"\x00"}, input: input},
		{name: "NUL environment", executable: "/bin/sh", env: []string{"VALUE=\x00"}, input: input},
		{name: "nil pipe", executable: "/bin/sh"},
		{name: "closed pipe", executable: "/bin/sh", input: closed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, err := startNativeApp(tc.executable, tc.args, tc.env, tc.input, output)
			if app != nil {
				_ = app.Kill()
				_, _ = app.Wait()
				t.Fatal("invalid launch created a process")
			}
			if err == nil {
				t.Fatal("invalid launch accepted")
			}
		})
	}
}
