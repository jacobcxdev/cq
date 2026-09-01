//go:build darwin

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestOSCommandRunnerContainsPipeHoldingGrandchild(t *testing.T) {
	testOSCommandRunnerContainsGrandchild(t, false)
}

func TestOSCommandRunnerContainsSetsidGrandchild(t *testing.T) {
	testOSCommandRunnerContainsGrandchild(t, true)
}

func TestOSCommandRunnerContainsDetachedClosedPipeChild(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	runner := &osCommandRunner{environment: append(os.Environ(), "CQ_PROCESS_HELPER=parent", "CQ_PROCESS_PID_FILE="+pidFile, "CQ_PROCESS_ESCAPE=1", "CQ_PROCESS_DETACH=1"), timeout: 2 * time.Second, waitDelay: 100 * time.Millisecond}
	if _, _, err := runner.Run(t.TempDir(), nil, executable, "-test.run=^TestProcessGroupHelper$"); err == nil {
		t.Fatal("accepted successful parent that left a detached child")
	}
	assertProcessGone(t, pidFile)
}

func TestOSCommandRunnerContainsDoubleForkedSetsidChild(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "double-fork.pid")
	runner := &osCommandRunner{environment: append(os.Environ(), "CQ_PROCESS_HELPER=double-parent", "CQ_PROCESS_PID_FILE="+pidFile), timeout: 2 * time.Second, waitDelay: 100 * time.Millisecond}
	if _, _, err := runner.Run(t.TempDir(), nil, executable, "-test.run=^TestProcessGroupHelper$"); err == nil {
		t.Fatal("accepted successful parent that left a double-forked child")
	}
	assertProcessGone(t, pidFile)
}

func TestDarwinProcessTrackerFailsClosedWithoutKillingReusedPID(t *testing.T) {
	var signals []syscall.Signal
	tracker := &darwinProcessTracker{
		identity: func(int) (string, error) { return "new-birth", nil },
		children: func(int) ([]int, error) { return nil, nil },
		signal: func(_ int, signal syscall.Signal) error {
			signals = append(signals, signal)
			return nil
		},
		tracked: map[int]string{424242: "old-birth"},
	}
	if err := tracker.Contain(1); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("PID reuse result = %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("signalled reused unrelated PID: %v", signals)
	}
}

func TestDarwinProcessTrackerFailsClosedOnEnumerationError(t *testing.T) {
	want := errors.New("enumeration unavailable")
	tracker := &darwinProcessTracker{
		identity: func(int) (string, error) { return "same-birth", nil },
		children: func(int) ([]int, error) { return nil, want },
		signal:   func(int, syscall.Signal) error { return nil },
		tracked:  map[int]string{424242: "same-birth"},
	}
	if err := tracker.Contain(1); err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("enumeration failure result = %v", err)
	}
}

func TestDarwinProcessTrackerCloseReturnsQueuedTrackingError(t *testing.T) {
	kqueue, err := syscall.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("late queue error")
	done := make(chan struct{})
	close(done)
	tracker := &darwinProcessTracker{kqueue: kqueue, stop: make(chan struct{}), done: done, trackingErr: want}
	if err := tracker.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() = %v, want queued error", err)
	}
}

func TestDarwinProcessTrackerFindsChildCreatedDuringContainment(t *testing.T) {
	stopped := false
	childEnumerated := false
	childSignals := make(map[syscall.Signal]bool)
	tracker := &darwinProcessTracker{
		identity: func(pid int) (string, error) { return strconv.Itoa(pid), nil },
		children: func(pid int) ([]int, error) {
			if pid == 1 && stopped {
				childEnumerated = true
				return []int{2}, nil
			}
			return nil, nil
		},
		signal: func(pid int, signal syscall.Signal) error {
			if pid == 1 && signal == syscall.SIGSTOP {
				stopped = true
			}
			if pid == 2 {
				childSignals[signal] = true
			}
			return nil
		},
		tracked: map[int]string{1: "1"},
	}
	_ = tracker.Contain(1)
	if !childEnumerated {
		t.Fatal("containment did not enumerate children after stopping parent")
	}
	if !childSignals[syscall.SIGSTOP] || !childSignals[syscall.SIGKILL] {
		t.Fatalf("child signals = %v, want stop then kill", childSignals)
	}
}

func testOSCommandRunnerContainsGrandchild(t *testing.T, escape bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	environment := append(os.Environ(), "CQ_PROCESS_HELPER=parent", "CQ_PROCESS_PID_FILE="+pidFile)
	if escape {
		environment = append(environment, "CQ_PROCESS_ESCAPE=1")
	}
	runner := &osCommandRunner{
		environment: environment,
		timeout:     500 * time.Millisecond,
		waitDelay:   100 * time.Millisecond,
	}
	started := time.Now()
	_, _, err = runner.Run(t.TempDir(), nil, executable, "-test.run=^TestProcessGroupHelper$")
	if err == nil {
		t.Fatal("timed-out process group returned success")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("runner waited %s for a descendant-held pipe", elapsed)
	}
	assertProcessGone(t, pidFile)
}

func TestNewOSCommandRunnerClosesAmbientGoEnvironment(t *testing.T) {
	t.Setenv("GOENV", "/tmp/attacker-goenv")
	t.Setenv("GOFLAGS", "-toolexec=/tmp/attacker")
	t.Setenv("GOWORK", "/tmp/attacker.work")
	t.Setenv("GOPROXY", "https://attacker.invalid")
	t.Setenv("PATH", "/tmp/attacker-bin")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/attacker.gitconfig")
	t.Setenv("CQ_TEST_SECRET", "must-not-cross-boundary")

	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runner, cleanup, err := newOSCommandRunner(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("clean runner workspace: %v", err)
		}
	})

	if !filepath.IsAbs(runner.goTool) {
		t.Fatalf("Go tool = %q, want absolute path", runner.goTool)
	}
	resolved, err := filepath.EvalSymlinks(runner.goTool)
	if err != nil {
		t.Fatal(err)
	}
	if runner.goTool != resolved {
		t.Fatalf("Go tool = %q, want resolved path %q", runner.goTool, resolved)
	}
	environment := strings.Join(runner.environment, "\n")
	for _, expected := range []string{
		"GOENV=off", "GOFLAGS=", "GONOSUMDB=*", "GOPROXY=off",
		"GOSUMDB=off", "GOTELEMETRY=off", "GOTOOLCHAIN=local",
		"GOWORK=off", "LC_ALL=C", "PATH=/usr/bin:/bin", "TZ=UTC",
	} {
		if !strings.Contains("\n"+environment+"\n", "\n"+expected+"\n") {
			t.Errorf("closed environment missing %q:\n%s", expected, environment)
		}
	}
	for _, forbidden := range []string{
		"attacker", "CQ_TEST_SECRET", "GIT_CONFIG_GLOBAL", "must-not-cross-boundary",
	} {
		if strings.Contains(environment, forbidden) {
			t.Errorf("closed environment contains %q:\n%s", forbidden, environment)
		}
	}
	workspace := environmentValueForTest(t, runner.environment, "TMPDIR")
	for _, key := range []string{"GOCACHE", "GOPATH", "HOME", "XDG_CONFIG_HOME"} {
		value := environmentValueForTest(t, runner.environment, key)
		if value != workspace && !strings.HasPrefix(value, workspace+string(filepath.Separator)) {
			t.Errorf("%s = %q, want private workspace %q", key, value, workspace)
		}
	}
	moduleCache := environmentValueForTest(t, runner.environment, "GOMODCACHE")
	if moduleCache == workspace || strings.HasPrefix(moduleCache, workspace+string(filepath.Separator)) {
		t.Fatalf("module cache %q was cloned beneath private workspace %q", moduleCache, workspace)
	}
	if _, err := os.Stat(moduleCache); err != nil {
		t.Fatalf("approved local module cache is unavailable: %v", err)
	}
}

func environmentValueForTest(t *testing.T, environment []string, key string) string {
	t.Helper()
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	t.Fatalf("environment has no %s entry", key)
	return ""
}

func TestOSCommandRunnerClosesBothStartGateDescriptorsWhenStartFails(t *testing.T) {
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	runner := &osCommandRunner{
		environment: []string{"PATH=/usr/bin:/bin"},
		timeout:     time.Second,
		waitDelay:   time.Second,
		pipe: func() (*os.File, *os.File, error) {
			return gateRead, gateWrite, nil
		},
	}
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	if _, _, err := runner.Run(missingDirectory, nil, "/bin/true"); err == nil {
		t.Fatal("command with missing working directory started")
	}
	for name, file := range map[string]*os.File{"read": gateRead, "write": gateWrite} {
		if _, err := file.Stat(); err == nil {
			t.Errorf("start-gate %s descriptor remains open", name)
		}
	}
}

func TestProcessGroupHelper(t *testing.T) {
	switch os.Getenv("CQ_PROCESS_HELPER") {
	case "":
		return
	case "grandchild":
		time.Sleep(3 * time.Second)
		return
	case "double-parent":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(95)
		}
		middle := exec.Command(executable, "-test.run=^TestProcessGroupHelper$")
		middle.Env = append(os.Environ(), "CQ_PROCESS_HELPER=middle")
		middle.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := middle.Run(); err != nil {
			os.Exit(96)
		}
		return
	case "middle":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(97)
		}
		child := exec.Command(executable, "-test.run=^TestProcessGroupHelper$")
		child.Env = append(os.Environ(), "CQ_PROCESS_HELPER=grandchild")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(98)
		}
		if err := os.WriteFile(os.Getenv("CQ_PROCESS_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(99)
		}
		return
	case "parent":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(91)
		}
		child := exec.Command(executable, "-test.run=^TestProcessGroupHelper$")
		child.Env = append(os.Environ(), "CQ_PROCESS_HELPER=grandchild")
		if os.Getenv("CQ_PROCESS_ESCAPE") == "1" {
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if os.Getenv("CQ_PROCESS_DETACH") != "1" {
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		}
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		if err := os.WriteFile(os.Getenv("CQ_PROCESS_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(93)
		}
		if os.Getenv("CQ_PROCESS_DETACH") != "1" {
			time.Sleep(3 * time.Second)
		}
		os.Exit(0)
	default:
		os.Exit(94)
	}
}

func assertProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d remained alive", pid)
}

func TestVerifyTestEventsAcceptsExactRunAndPass(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne", "TestOne/edge"},
		MinimumPassCount: 2,
	}
	events := strings.Join([]string{
		`{"Action":"start","Package":"github.com/jacobcxdev/cq/internal/proxy"}`,
		`{"Action":"run","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"output","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne","Output":"=== RUN   TestOne\n"}`,
		`{"Action":"pause","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"cont","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"run","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy"}`,
	}, "\n") + "\n"
	if err := verifyTestEvents(selection, strings.NewReader(events), true); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyTestEventsRejectsDuplicateObjectMembers(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}
	pkg := "github.com/jacobcxdev/cq/internal/proxy"
	run := `{"Action":"run","Package":"` + pkg + `","Test":"TestOne"}`
	pass := `{"Action":"pass","Package":"` + pkg + `","Test":"TestOne"}`
	terminal := `{"Action":"pass","Package":"` + pkg + `"}`
	for name, start := range map[string]string{
		"action":       `{"Action":"fail","Action":"start","Package":"` + pkg + `"}`,
		"package":      `{"Action":"start","Package":"other","Package":"` + pkg + `"}`,
		"test":         `{"Action":"start","Package":"` + pkg + `","Test":"ignored","Test":""}`,
		"known output": `{"Action":"start","Package":"` + pkg + `","Output":"first","Output":"second"}`,
		"unknown":      `{"Action":"start","Package":"` + pkg + `","Mystery":1,"Mystery":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			events := start + "\n" + run + "\n" + pass + "\n" + terminal + "\n"
			err := verifyTestEvents(selection, strings.NewReader(events), true)
			if err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("duplicate object member result = %v, want duplicate error", err)
			}
		})
	}
}

func TestVerifyTestEventsRejectsCaseAliasMembersBeforeTypedDecode(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}
	pkg := "github.com/jacobcxdev/cq/internal/proxy"
	for name, event := range map[string]string{
		"Action action":   `{"Action":"fail","action":"start","Package":"` + pkg + `"}`,
		"Package package": `{"Action":"start","Package":"other","package":"` + pkg + `"}`,
		"Test test":       `{"Action":"start","Package":"` + pkg + `","Test":"ignored","test":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyTestEvents(selection, strings.NewReader(event+"\n"), true)
			if err == nil || !strings.Contains(err.Error(), "member") {
				t.Fatalf("case-alias member result = %v, want token-member rejection", err)
			}
		})
	}
}

func TestVerifyTestEventsRejectsEveryInvalidStateTransition(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}
	pkg := "github.com/jacobcxdev/cq/internal/proxy"
	start := `{"Action":"start","Package":"` + pkg + `"}`
	run := `{"Action":"run","Package":"` + pkg + `","Test":"TestOne"}`
	pass := `{"Action":"pass","Package":"` + pkg + `","Test":"TestOne"}`
	terminal := `{"Action":"pass","Package":"` + pkg + `"}`
	valid := start + "\n" + run + "\n" + pass + "\n" + terminal + "\n"
	fixtures := map[string]string{
		"missing package start":       run + "\n" + pass + "\n" + terminal + "\n",
		"duplicate package start":     start + "\n" + start + "\n" + run + "\n" + pass + "\n" + terminal + "\n",
		"package output before start": `{"Action":"output","Package":"` + pkg + `"}` + "\n" + valid,
		"test output before run":      start + "\n" + `{"Action":"output","Package":"` + pkg + `","Test":"TestOne"}` + "\n" + run + "\n" + pass + "\n" + terminal + "\n",
		"test pause before run":       start + "\n" + `{"Action":"pause","Package":"` + pkg + `","Test":"TestOne"}` + "\n" + run + "\n" + pass + "\n" + terminal + "\n",
		"test cont before pause":      start + "\n" + run + "\n" + `{"Action":"cont","Package":"` + pkg + `","Test":"TestOne"}` + "\n" + pass + "\n" + terminal + "\n",
		"test pass while paused":      start + "\n" + run + "\n" + `{"Action":"pause","Package":"` + pkg + `","Test":"TestOne"}` + "\n" + pass + "\n" + terminal + "\n",
		"test output after pass":      start + "\n" + run + "\n" + pass + "\n" + `{"Action":"output","Package":"` + pkg + `","Test":"TestOne"}` + "\n" + terminal + "\n",
		"test run after pass":         start + "\n" + run + "\n" + pass + "\n" + run + "\n" + terminal + "\n",
		"package pass before tests":   start + "\n" + terminal + "\n",
		"package output after pass":   valid + `{"Action":"output","Package":"` + pkg + `"}` + "\n",
		"package fail":                start + "\n" + run + "\n" + pass + "\n" + `{"Action":"fail","Package":"` + pkg + `"}` + "\n",
		"test fail":                   start + "\n" + run + "\n" + `{"Action":"fail","Package":"` + pkg + `","Test":"TestOne"}` + "\n",
		"unselected output":           start + "\n" + `{"Action":"output","Package":"` + pkg + `","Test":"TestOther"}` + "\n",
	}
	for name, events := range fixtures {
		t.Run(name, func(t *testing.T) {
			if err := verifyTestEvents(selection, strings.NewReader(events), true); err == nil {
				t.Fatal("accepted invalid go test event transition")
			}
		})
	}
}

func TestShellWrappersVerifyFrozenReviewAndSelfTest(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(bin, "hostile-dirname-ran")
	if err := os.WriteFile(filepath.Join(bin, "dirname"), []byte("#!/bin/sh\n/usr/bin/touch \""+marker+"\"\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, script := range [][]string{
		{"scripts/verify-blueprint-review"},
		{"scripts/verify-proxy-cu", "--self-test"},
	} {
		command := exec.Command(filepath.Join(repositoryRoot, script[0]), script[1:]...)
		command.Dir = repositoryRoot
		hostileRoot := t.TempDir()
		hostilePaths := []string{
			filepath.Join(hostileRoot, "home"), filepath.Join(hostileRoot, "gocache"),
			filepath.Join(hostileRoot, "gomodcache"), filepath.Join(hostileRoot, "gopath"),
		}
		command.Env = append(os.Environ(),
			"PATH="+bin, "HOME="+hostilePaths[0], "GOCACHE="+hostilePaths[1],
			"GOMODCACHE="+hostilePaths[2], "GOPATH="+hostilePaths[3],
			"GOFLAGS=-definitely-invalid", "GOENV=/definitely/missing/goenv",
			"GOWORK=/definitely/missing/go.work", "GOTOOLCHAIN=definitely-invalid",
			"GOPROXY=http://127.0.0.1:1",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", script, err, output)
		}
		for _, path := range hostilePaths {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("valid wrapper used hostile path %q: %v", path, err)
			}
		}
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile dirname ran: %v", err)
	}
}

func TestShellWrappersRejectArityBeforeGoOrTemporaryWork(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		path string
		args []string
		want string
	}{
		{path: "scripts/build-proxy-release", want: "expected exactly one release-build manifest"},
		{path: "scripts/verify-blueprint-review", args: []string{"extra"}, want: "expected no arguments"},
		{path: "scripts/verify-proxy-cu", want: "expected exactly one CU-0, CU-1, CU-2, or --self-test argument"},
		{path: "scripts/verify-proxy-cu", args: []string{"CU-0", "extra"}, want: "expected exactly one CU-0, CU-1, CU-2, or --self-test argument"},
	}
	for _, fixture := range fixtures {
		t.Run(strings.ReplaceAll(fixture.path+strings.Join(fixture.args, "_"), "/", "_"), func(t *testing.T) {
			root := t.TempDir()
			paths := []string{filepath.Join(root, "home"), filepath.Join(root, "gocache"), filepath.Join(root, "gomodcache"), filepath.Join(root, "gopath"), filepath.Join(root, "tmp")}
			command := exec.Command(filepath.Join(repositoryRoot, fixture.path), fixture.args...)
			command.Dir = repositoryRoot
			command.Env = append(os.Environ(), "HOME="+paths[0], "GOCACHE="+paths[1], "GOMODCACHE="+paths[2], "GOPATH="+paths[3], "TMPDIR="+paths[4], "GOFLAGS=-toolexec=/definitely/missing")
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), fixture.want) {
				t.Fatalf("invalid arity = %v, %q; want %q", err, output, fixture.want)
			}
			for _, path := range paths {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid arity created %q: %v", path, err)
				}
			}
		})
	}
}

func TestVerifyProxyCUWrapperRunsCU0(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "verify-proxy-cu"), "CU-0")
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify-proxy-cu CU-0: %v\n%s", err, output)
	}
}

func TestVerifyProxyCUWrapperRunsCU1(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "verify-proxy-cu"), "CU-1")
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify-proxy-cu CU-1: %v\n%s", err, output)
	}
}

func TestVerifyUnitRunsListThenExactRaceSelection(t *testing.T) {
	manifest := proxy.CUManifestV1{
		SchemaVersion:                    1,
		Kind:                             "construction_unit_verification_manifest_v1",
		BlueprintSHA256:                  "bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38",
		ReviewAttestationAggregateSHA256: "3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be",
		ReviewAuthorityBaselineCommit:    "9fe30df8d4101f69084d6487740ed324a5d0b59d",
		Unit:                             "CU-0",
		RaceCount:                        1,
		Packages: []proxy.CUTestPackageV1{{
			Package:          "./internal/proxy",
			TopLevelTests:    []string{"TestOne"},
			FullTestIDs:      []string{"TestOne"},
			MinimumPassCount: 1,
		}},
	}
	runner := &fakeCommandRunner{results: []commandResult{
		{stdout: []byte("TestOne\nok\tgithub.com/jacobcxdev/cq/internal/proxy\t0.001s\n")},
		{stdout: []byte("{\"Action\":\"start\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\"}\n{\"Action\":\"run\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\"}\n")},
	}}
	if err := verifyUnit("/repo", manifest, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("command calls = %d, want 2", len(runner.calls))
	}
	if got := strings.Join(runner.calls[0].args, " "); !strings.Contains(got, "test -list") || !strings.Contains(got, "-race -count=1") {
		t.Fatalf("list command = %q", got)
	}
	if got := strings.Join(runner.calls[1].args, " "); !strings.Contains(got, "test -json -race -count=1") {
		t.Fatalf("race command = %q", got)
	}
}

func TestVerifyTestEventsRejectsFramingAndPackageSubstitution(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}
	run := `{"Action":"run","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`
	pass := `{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`
	terminal := `{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy"}`
	for name, events := range map[string]string{
		"package substitution":   strings.ReplaceAll(run+"\n"+pass+"\n"+terminal+"\n", "github.com/jacobcxdev/cq/internal/proxy", "example/proxy"),
		"CRLF framing":           run + "\r\n" + pass + "\r\n" + terminal + "\r\n",
		"missing final LF":       run + "\n" + pass + "\n" + terminal,
		"missing terminal":       run + "\n" + pass + "\n",
		"blank line":             run + "\n\n" + pass + "\n" + terminal + "\n",
		"whitespace line":        run + "\n \n" + pass + "\n" + terminal + "\n",
		"padded object":          " " + run + "\n" + pass + "\n" + terminal + "\n",
		"concatenated object":    run + `{}` + "\n" + pass + "\n" + terminal + "\n",
		"duplicate terminal":     run + "\n" + pass + "\n" + terminal + "\n" + terminal + "\n",
		"unknown package action": `{"Action":"mystery","Package":"github.com/jacobcxdev/cq/internal/proxy"}` + "\n" + run + "\n" + pass + "\n" + terminal + "\n",
		"unknown test action":    run + "\n" + `{"Action":"mystery","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}` + "\n" + pass + "\n" + terminal + "\n",
		"package skip":           run + "\n" + pass + "\n" + `{"Action":"skip","Package":"github.com/jacobcxdev/cq/internal/proxy"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyTestEvents(selection, strings.NewReader(events), true); err == nil {
				t.Fatal("accepted substituted or incorrectly framed test evidence")
			}
		})
	}
}

func TestVerifyUnitRejectsChildFailure(t *testing.T) {
	manifest := proxy.CUManifestV1{Unit: "CU-0", Packages: []proxy.CUTestPackageV1{{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}}}
	runner := &fakeCommandRunner{results: []commandResult{{err: errors.New("child failed")}}}
	if err := verifyUnit("/repo", manifest, runner); err == nil {
		t.Fatal("accepted failed child command")
	}
}

type commandCall struct {
	directory string
	args      []string
}

type commandResult struct {
	stdout []byte
	stderr []byte
	err    error
}

type fakeCommandRunner struct {
	calls   []commandCall
	results []commandResult
}

func (runner *fakeCommandRunner) Run(directory string, environment []string, name string, args ...string) ([]byte, []byte, error) {
	runner.calls = append(runner.calls, commandCall{directory: directory, args: append([]string{name}, args...)})
	if len(runner.results) == 0 {
		return nil, nil, io.EOF
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result.stdout, result.stderr, result.err
}

func TestVerifyTestEventsRejectsCorruptEvidence(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne"},
		MinimumPassCount: 1,
	}
	for name, fixture := range map[string]struct {
		events      string
		raceEnabled bool
	}{
		"absent":        {events: `{"Action":"pass","Package":"example/proxy"}` + "\n", raceEnabled: true},
		"extra":         {events: `{"Action":"run","Package":"example/proxy","Test":"TestTwo"}` + "\n", raceEnabled: true},
		"duplicate":     {events: `{"Action":"run","Package":"example/proxy","Test":"TestOne"}` + "\n" + `{"Action":"run","Package":"example/proxy","Test":"TestOne"}` + "\n", raceEnabled: true},
		"skipped":       {events: `{"Action":"skip","Package":"example/proxy","Test":"TestOne"}` + "\n", raceEnabled: true},
		"malformed":     {events: "{\n", raceEnabled: true},
		"race disabled": {events: `{"Action":"run","Package":"example/proxy","Test":"TestOne"}` + "\n" + `{"Action":"pass","Package":"example/proxy","Test":"TestOne"}` + "\n", raceEnabled: false},
		"no tests":      {events: `{"Action":"output","Package":"example/proxy","Output":"testing: warning: no tests to run\n"}` + "\n", raceEnabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyTestEvents(selection, strings.NewReader(fixture.events), fixture.raceEnabled); err == nil {
				t.Fatal("accepted corrupt test evidence")
			}
		})
	}
}

func TestRunRejectsAbsentAndUnmanifestedCU(t *testing.T) {
	for _, args := range [][]string{{"unit"}, {"unit", "CU-3"}, {"unit", "CU-0", "extra"}} {
		if err := run(args, testDependencies{}); err == nil {
			t.Fatalf("run(%v) accepted invalid unit invocation", args)
		}
	}
}

func TestRunAcceptsManifestedCU2(t *testing.T) {
	var got string
	err := run([]string{"CU-2"}, testDependencies{Unit: func(cuID string) error {
		got = cuID
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "CU-2" {
		t.Fatalf("unit = %q, want CU-2", got)
	}
}

func TestRunAcceptsManifestedCU1(t *testing.T) {
	var got string
	err := run([]string{"CU-1"}, testDependencies{Unit: func(cuID string) error {
		got = cuID
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "CU-1" {
		t.Fatalf("unit = %q, want CU-1", got)
	}
}
