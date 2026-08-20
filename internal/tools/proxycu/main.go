//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

const maxGoTestJSONBytes = 8 << 20

type testDependencies struct {
	VerifyBlueprintReview func(path, sibling string) error
	SelfTest              func() error
	Unit                  func(cuID string) error
}

type commandRunner interface {
	Run(directory string, environment []string, name string, args ...string) ([]byte, []byte, error)
}

func main() {
	repositoryRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-proxy-cu: determine repository root: %v\n", err)
		os.Exit(1)
	}
	dependencies := testDependencies{
		VerifyBlueprintReview: proxy.VerifyBlueprintReview,
		SelfTest:              selfTest,
		Unit: func(cuID string) error {
			return runCU0(repositoryRoot, cuID)
		},
	}
	if err := run(os.Args[1:], dependencies); err != nil {
		fmt.Fprintf(os.Stderr, "verify-proxy-cu: %v\n", err)
		os.Exit(1)
	}
}

func runCU0(repositoryRoot, cuID string) (resultErr error) {
	if cuID != "CU-0" && cuID != "CU-1" && cuID != "CU-2" {
		return fmt.Errorf("construction unit %q has no manifest", cuID)
	}
	if err := proxy.VerifyBlueprintReview(
		filepath.Join(repositoryRoot, "docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md"),
		filepath.Join(repositoryRoot, "docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json"),
	); err != nil {
		return err
	}
	manifestBytes, err := proxy.CanonicalCUManifestV1(cuID)
	if err != nil {
		return err
	}
	manifest, err := proxy.ParseCUManifestV1(bytes.NewReader(manifestBytes))
	if err != nil {
		return err
	}
	runner, cleanup, err := newOSCommandRunner(repositoryRoot)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	return verifyUnit(repositoryRoot, manifest, runner)
}

type osCommandRunner struct {
	environment []string
	goTool      string
	timeout     time.Duration
	waitDelay   time.Duration
	pipe        func() (*os.File, *os.File, error)
}

func newOSCommandRunner(_ string) (*osCommandRunner, func() error, error) {
	workspace, err := os.MkdirTemp("", "cq-cu0-")
	if err != nil {
		return nil, nil, fmt.Errorf("create isolated CU workspace: %w", err)
	}
	cleanup := func() error {
		walkErr := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return nil
		})
		return errors.Join(walkErr, os.RemoveAll(workspace))
	}
	goTool, err := filepath.EvalSymlinks("/opt/homebrew/bin/go")
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("resolve pinned Go tool: %w", err), cleanup())
	}
	if !filepath.IsAbs(goTool) {
		return nil, nil, errors.Join(fmt.Errorf("resolved Go tool is not absolute"), cleanup())
	}
	currentUser, err := user.Current()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("resolve bootstrap user: %w", err), cleanup())
	}
	if !filepath.IsAbs(currentUser.HomeDir) {
		return nil, nil, errors.Join(fmt.Errorf("bootstrap home is not absolute"), cleanup())
	}
	privateHome := filepath.Join(workspace, "home")
	privateBuildCache := filepath.Join(workspace, "gocache")
	privateGoPath := filepath.Join(workspace, "gopath")
	privateXDG := filepath.Join(workspace, "xdg")
	for _, directory := range []string{privateHome, privateBuildCache, privateGoPath, privateXDG} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("create private Go workspace: %w", err), cleanup())
		}
	}
	moduleCache := filepath.Join(currentUser.HomeDir, "go", "pkg", "mod")
	info, err := os.Stat(moduleCache)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("approved local module cache is unavailable: %w", err), cleanup())
	}
	if !info.IsDir() {
		return nil, nil, errors.Join(fmt.Errorf("approved local module cache is not a directory"), cleanup())
	}
	environment := closedGoEnvironment(workspace, privateHome, moduleCache, privateBuildCache, privateGoPath, privateXDG, "off")
	return &osCommandRunner{
		environment: environment,
		goTool:      goTool,
		timeout:     10 * time.Minute,
		waitDelay:   2 * time.Second,
	}, cleanup, nil
}

func closedGoEnvironment(workspace, home, moduleCache, buildCache, goPath, xdg, proxy string) []string {
	environment := []string{
		"CGO_ENABLED=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GOCACHE=" + buildCache,
		"GOENV=off",
		"GOFLAGS=",
		"GOMODCACHE=" + moduleCache,
		"GONOSUMDB=*",
		"GOPATH=" + goPath,
		"GOPROXY=" + proxy,
		"GOSUMDB=off",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"HOME=" + home,
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
		"TMPDIR=" + workspace,
		"TZ=UTC",
		"XDG_CONFIG_HOME=" + xdg,
	}
	sort.Strings(environment)
	return environment
}

func (runner *osCommandRunner) Run(directory string, _ []string, name string, args ...string) (stdoutResult, stderrResult []byte, resultErr error) {
	if name == "go" {
		if runner.goTool == "" {
			return nil, nil, fmt.Errorf("absolute Go tool is unavailable")
		}
		name = runner.goTool
	} else if !filepath.IsAbs(name) {
		return nil, nil, fmt.Errorf("command tool must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), runner.timeout)
	defer cancel()
	tracker, err := newDarwinProcessTracker(processBirthIdentity)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, tracker.Close()) }()
	openPipe := runner.pipe
	if openPipe == nil {
		openPipe = os.Pipe
	}
	gateRead, gateWrite, err := openPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create command start gate: %w", err)
	}
	defer gateRead.Close()
	defer gateWrite.Close()
	commandArgs := append([]string{"-c", "IFS= read -r _ <&3 || exit 125; exec \"$@\"", "cq-cu-gate", name}, args...)
	command := exec.Command("/bin/sh", commandArgs...)
	command.Dir = directory
	command.Env = runner.environment
	command.ExtraFiles = []*os.File{gateRead}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = runner.waitDelay
	stdout := &boundedBuffer{limit: maxGoTestJSONBytes}
	stderr := &boundedBuffer{limit: maxGoTestJSONBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = gateRead.Close()
		return stdout.Bytes(), stderr.Bytes(), err
	}
	_ = gateRead.Close()
	if err := tracker.TrackRoot(command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("track command descendants: %w", err)
	}
	if _, err := gateWrite.Write([]byte("start\n")); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("release command start gate: %w", err)
	}
	if err := gateWrite.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("close command start gate: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		timedOut = true
		containErr := tracker.Contain(command.Process.Pid)
		select {
		case waitErr = <-waited:
		case <-time.After(runner.waitDelay):
			waitErr = fmt.Errorf("command wait exceeded containment delay")
		}
		if containErr != nil {
			waitErr = errors.Join(waitErr, containErr)
		}
	}
	containErr := tracker.Contain(command.Process.Pid)
	if timedOut {
		return stdout.Bytes(), stderr.Bytes(), errors.Join(fmt.Errorf("command timed out"), waitErr, containErr, tracker.Err())
	}
	if containErr != nil || tracker.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), errors.Join(waitErr, containErr, tracker.Err())
	}
	return stdout.Bytes(), stderr.Bytes(), waitErr
}

type processIdentityReader func(pid int) (string, error)

// darwinProcessTracker is a synchronised fixed-point containment mechanism for
// the verifier's fixed, approved source. NOTE_FORK plus descendant polling is
// not a kernel-enforced sandbox and must not be treated as containment of a
// deliberately hostile same-UID child.
type darwinProcessTracker struct {
	kqueue      int
	identity    processIdentityReader
	children    func(int) ([]int, error)
	signal      func(int, syscall.Signal) error
	mu          sync.Mutex
	rootPID     int
	tracked     map[int]string
	trackingErr error
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
}

func newDarwinProcessTracker(identity processIdentityReader) (*darwinProcessTracker, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("process-event tracking is unsupported outside Darwin")
	}
	kqueue, err := syscall.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create process-event queue: %w", err)
	}
	tracker := &darwinProcessTracker{kqueue: kqueue, identity: identity, children: processChildPIDs, signal: syscall.Kill, tracked: make(map[int]string), stop: make(chan struct{}), done: make(chan struct{})}
	go tracker.readEvents()
	return tracker, nil
}

func (tracker *darwinProcessTracker) TrackRoot(pid int) error {
	identity, err := tracker.identity(pid)
	if err != nil {
		return err
	}
	if err := tracker.trackProcess(pid, identity); err != nil {
		return err
	}
	tracker.mu.Lock()
	tracker.rootPID = pid
	tracker.mu.Unlock()
	return nil
}

func (tracker *darwinProcessTracker) trackProcess(pid int, identity string) error {
	change := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ENABLE, Fflags: syscall.NOTE_EXIT | syscall.NOTE_FORK}
	if _, err := syscall.Kevent(tracker.kqueue, []syscall.Kevent_t{change}, nil, nil); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	tracker.mu.Lock()
	tracker.tracked[pid] = identity
	tracker.mu.Unlock()
	return tracker.trackCurrentChildren(pid)
}

func (tracker *darwinProcessTracker) readEvents() {
	defer close(tracker.done)
	events := make([]syscall.Kevent_t, 32)
	for {
		select {
		case <-tracker.stop:
			return
		default:
		}
		timeout := syscall.NsecToTimespec((10 * time.Millisecond).Nanoseconds())
		count, err := syscall.Kevent(tracker.kqueue, nil, events, &timeout)
		if err != nil && err != syscall.EINTR {
			select {
			case <-tracker.stop:
				return
			default:
			}
			tracker.setError(fmt.Errorf("read process events: %w", err))
			return
		}
		if count < 0 {
			select {
			case <-tracker.stop:
				return
			default:
			}
			continue
		}
		for _, event := range events[:count] {
			pid := int(event.Ident)
			if event.Fflags&syscall.NOTE_FORK != 0 {
				if err := tracker.trackCurrentChildren(pid); err != nil {
					tracker.setError(fmt.Errorf("track children of process %d: %w", pid, err))
				}
			}
			if event.Fflags&syscall.NOTE_EXIT != 0 {
				tracker.mu.Lock()
				delete(tracker.tracked, pid)
				tracker.mu.Unlock()
			}
		}
	}
}

func (tracker *darwinProcessTracker) trackCurrentChildren(parentPID int) error {
	pids, err := tracker.children(parentPID)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		identity, err := tracker.identity(pid)
		if err != nil {
			if errors.Is(tracker.signal(pid, 0), syscall.ESRCH) {
				continue
			}
			return err
		}
		tracker.mu.Lock()
		known := tracker.tracked[pid] == identity
		tracker.mu.Unlock()
		if !known {
			if err := tracker.trackProcess(pid, identity); err != nil {
				return err
			}
		}
	}
	return nil
}

func processChildPIDs(parentPID int) ([]int, error) {
	command := exec.Command("/usr/bin/pgrep", "-P", fmt.Sprintf("%d", parentPID))
	command.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin", "TZ=UTC"}
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, field := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("invalid child PID %q", field)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func (tracker *darwinProcessTracker) Contain(rootPID int) error {
	var result error
	stopped := make(map[int]bool)
	for round := 0; round < 64; round++ {
		tracker.mu.Lock()
		parents := make([]int, 0, len(tracker.tracked))
		for pid := range tracker.tracked {
			parents = append(parents, pid)
		}
		tracker.mu.Unlock()
		for _, pid := range parents {
			if !stopped[pid] {
				expected := tracker.trackedIdentity(pid)
				identity, err := tracker.identity(pid)
				if err != nil {
					if errors.Is(tracker.signal(pid, 0), syscall.ESRCH) {
						continue
					}
					result = errors.Join(result, fmt.Errorf("re-identify tracked process %d: %w", pid, err))
					continue
				}
				if identity != expected {
					result = errors.Join(result, fmt.Errorf("tracked process %d identity changed before containment", pid))
					continue
				}
				if err := tracker.signal(pid, syscall.SIGSTOP); err != nil && err != syscall.ESRCH {
					result = errors.Join(result, fmt.Errorf("stop tracked process %d: %w", pid, err))
					continue
				}
				stopped[pid] = true
			}
			if err := tracker.discoverStoppedChildren(pid); err != nil {
				result = errors.Join(result, fmt.Errorf("enumerate tracked process %d children: %w", pid, err))
			}
		}
		tracker.mu.Lock()
		count := len(tracker.tracked)
		tracker.mu.Unlock()
		if count == len(stopped) {
			break
		}
		if round == 63 {
			result = errors.Join(result, fmt.Errorf("descendant containment did not reach a stable fixed point"))
		}
	}
	tracker.mu.Lock()
	tracked := make(map[int]string, len(tracker.tracked))
	for pid, identity := range tracker.tracked {
		tracked[pid] = identity
	}
	tracker.mu.Unlock()
	for pid, expectedIdentity := range tracked {
		identity, err := tracker.identity(pid)
		if err != nil {
			if errors.Is(tracker.signal(pid, 0), syscall.ESRCH) {
				continue
			}
			result = errors.Join(result, fmt.Errorf("re-identify tracked process %d: %w", pid, err))
			continue
		}
		if identity != expectedIdentity {
			result = errors.Join(result, fmt.Errorf("tracked process %d identity changed before containment", pid))
			continue
		}
		if pid != rootPID {
			result = errors.Join(result, fmt.Errorf("command left tracked descendant process %d", pid))
		}
	}
	for pid, expectedIdentity := range tracked {
		identity, err := tracker.identity(pid)
		if err == nil && identity == expectedIdentity {
			if err := tracker.signal(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				result = errors.Join(result, fmt.Errorf("kill tracked process %d: %w", pid, err))
			}
		}
	}
	return result
}

func (tracker *darwinProcessTracker) trackedIdentity(pid int) string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.tracked[pid]
}

func (tracker *darwinProcessTracker) discoverStoppedChildren(parentPID int) error {
	pids, err := tracker.children(parentPID)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		identity, err := tracker.identity(pid)
		if err != nil {
			if errors.Is(tracker.signal(pid, 0), syscall.ESRCH) {
				continue
			}
			return err
		}
		tracker.mu.Lock()
		known, exists := tracker.tracked[pid]
		if !exists {
			tracker.tracked[pid] = identity
		}
		tracker.mu.Unlock()
		if exists && known != identity {
			return fmt.Errorf("child process %d identity changed", pid)
		}
	}
	return nil
}

func (tracker *darwinProcessTracker) setError(err error) {
	tracker.mu.Lock()
	tracker.trackingErr = errors.Join(tracker.trackingErr, err)
	tracker.mu.Unlock()
}

func (tracker *darwinProcessTracker) Err() error {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.trackingErr
}

func (tracker *darwinProcessTracker) Close() error {
	var closeErr error
	tracker.closeOnce.Do(func() {
		close(tracker.stop)
		closeErr = syscall.Close(tracker.kqueue)
		<-tracker.done
	})
	if closeErr != nil {
		closeErr = fmt.Errorf("close process-event queue: %w", closeErr)
	}
	return errors.Join(closeErr, tracker.Err())
}

func processBirthIdentity(pid int) (string, error) {
	command := exec.Command("/bin/ps", "-p", fmt.Sprintf("%d", pid), "-o", "lstart=")
	command.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin", "TZ=UTC"}
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(output))
	if identity == "" {
		return "", fmt.Errorf("empty process birth identity")
	}
	return identity, nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if len(data) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(data[:remaining])
		}
		return remaining, fmt.Errorf("captured output exceeds %d bytes", buffer.limit)
	}
	return buffer.buffer.Write(data)
}

func (buffer *boundedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

func run(args []string, dependencies testDependencies) error {
	if len(args) == 0 {
		return fmt.Errorf("missing mode")
	}
	mode := args[0]
	if len(args) == 1 && mode == "--self-test" {
		mode = "self-test"
	}
	if len(args) == 1 && (mode == "CU-0" || mode == "CU-1" || mode == "CU-2") {
		args = []string{"unit", mode}
		mode = "unit"
	}
	switch mode {
	case "blueprint-review":
		if len(args) != 3 {
			return fmt.Errorf("usage: blueprint-review BLUEPRINT SIBLING")
		}
		if dependencies.VerifyBlueprintReview == nil {
			return fmt.Errorf("blueprint-review verifier is unavailable")
		}
		return dependencies.VerifyBlueprintReview(args[1], args[2])
	case "self-test":
		if len(args) != 1 {
			return fmt.Errorf("self-test accepts no arguments")
		}
		if dependencies.SelfTest == nil {
			return fmt.Errorf("self-test is unavailable")
		}
		return dependencies.SelfTest()
	case "unit":
		if len(args) != 2 {
			return fmt.Errorf("usage: unit CU-0|CU-1|CU-2")
		}
		if args[1] != "CU-0" && args[1] != "CU-1" && args[1] != "CU-2" {
			return fmt.Errorf("construction unit %q has no manifest", args[1])
		}
		if dependencies.Unit == nil {
			return fmt.Errorf("unit verifier is unavailable")
		}
		return dependencies.Unit(args[1])
	default:
		return fmt.Errorf("unknown mode %q", args[0])
	}
}

type goTestEvent struct {
	Time        string  `json:"Time"`
	Action      string  `json:"Action"`
	Package     string  `json:"Package"`
	Test        string  `json:"Test"`
	Elapsed     float64 `json:"Elapsed"`
	Output      string  `json:"Output"`
	FailedBuild string  `json:"FailedBuild"`
}

type testEventState struct {
	phase testEventPhase
}

type testEventPhase uint8

const (
	testEventPending testEventPhase = iota
	testEventRunning
	testEventPaused
	testEventPassed
)

func verifyTestEvents(selection proxy.CUTestPackageV1, reader io.Reader, raceEnabled bool) error {
	if !raceEnabled {
		return fmt.Errorf("race detector was not enabled")
	}
	expectedPackage, err := expectedGoTestPackage(selection.Package)
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: reader, N: maxGoTestJSONBytes + 1}
	stream, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read go test events: %w", err)
	}
	if limited.N <= 0 {
		return fmt.Errorf("go test event stream exceeds %d bytes", maxGoTestJSONBytes)
	}
	if len(stream) == 0 || stream[len(stream)-1] != '\n' {
		return fmt.Errorf("go test event stream is missing final LF")
	}
	if bytes.IndexByte(stream, '\r') >= 0 {
		return fmt.Errorf("go test event stream contains CR framing")
	}
	states := make(map[string]*testEventState, len(selection.FullTestIDs))
	for _, name := range selection.FullTestIDs {
		states[name] = &testEventState{}
	}
	packageStarted := false
	packagePassed := false
	for lineIndex, line := range bytes.Split(stream[:len(stream)-1], []byte{'\n'}) {
		if len(line) == 0 {
			return fmt.Errorf("blank go test event line")
		}
		if !bytes.Equal(line, bytes.TrimSpace(line)) {
			return fmt.Errorf("go test event line contains surrounding whitespace")
		}
		if packagePassed {
			return fmt.Errorf("go test event follows package terminal")
		}
		if err := rejectDuplicateGoTestEventMembers(line); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var event goTestEvent
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode go test event: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("go test event line contains trailing JSON")
		}
		if event.Package != expectedPackage {
			return fmt.Errorf("go test event package %q, want %q", event.Package, expectedPackage)
		}
		if strings.Contains(event.Output, "no tests to run") {
			return fmt.Errorf("go test selected no tests")
		}
		if event.Action == "skip" {
			return fmt.Errorf("go test package or test was skipped")
		}
		if event.Action == "fail" && event.Test == "" {
			return fmt.Errorf("go test package failed")
		}
		if event.Test == "" {
			switch event.Action {
			case "start":
				if lineIndex != 0 || packageStarted {
					return fmt.Errorf("go test package start is not exactly first")
				}
				packageStarted = true
			case "output":
				if !packageStarted {
					return fmt.Errorf("go test package output precedes start")
				}
			case "pass":
				if !packageStarted {
					return fmt.Errorf("go test package pass precedes start")
				}
				for name, state := range states {
					if state.phase != testEventPassed {
						return fmt.Errorf("test %q missing exact run/pass events", name)
					}
				}
				packagePassed = true
			case "fail":
				return fmt.Errorf("go test package failed")
			default:
				return fmt.Errorf("unknown go test package action %q", event.Action)
			}
			continue
		}
		if !packageStarted {
			return fmt.Errorf("go test test event precedes package start")
		}
		state, selected := states[event.Test]
		if !selected {
			return fmt.Errorf("unexpected test event %q", event.Test)
		}
		switch event.Action {
		case "run":
			if state.phase != testEventPending {
				return fmt.Errorf("test %q ran more than once", event.Test)
			}
			state.phase = testEventRunning
		case "pass":
			if state.phase != testEventRunning {
				return fmt.Errorf("test %q has invalid pass cardinality", event.Test)
			}
			state.phase = testEventPassed
		case "output":
			if state.phase != testEventRunning && state.phase != testEventPaused {
				return fmt.Errorf("test %q output is out of order", event.Test)
			}
		case "pause":
			if state.phase != testEventRunning {
				return fmt.Errorf("test %q pause is out of order", event.Test)
			}
			state.phase = testEventPaused
		case "cont":
			if state.phase != testEventPaused {
				return fmt.Errorf("test %q continue is out of order", event.Test)
			}
			state.phase = testEventRunning
		case "skip", "fail":
			return fmt.Errorf("test %q ended with %s", event.Test, event.Action)
		default:
			return fmt.Errorf("unknown go test action %q for test %q", event.Action, event.Test)
		}
	}
	if !packagePassed {
		return fmt.Errorf("go test package is missing its exact pass terminal")
	}
	passed := 0
	for name, state := range states {
		if state.phase != testEventPassed {
			return fmt.Errorf("test %q missing exact run/pass events", name)
		}
		passed++
	}
	if passed < selection.MinimumPassCount {
		return fmt.Errorf("observed %d passed tests, want at least %d", passed, selection.MinimumPassCount)
	}
	return nil
}

func rejectDuplicateGoTestEventMembers(line []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode go test event: %w", err)
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return fmt.Errorf("go test event must be a JSON object")
	}
	seen := make(map[string]struct{})
	unsupported := ""
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode go test event member: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("go test event member name is not a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate go test event member %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "Time", "Action", "Package", "Test", "Elapsed", "Output", "FailedBuild":
		default:
			if unsupported == "" {
				unsupported = name
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode go test event member %q: %w", name, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode go test event: %w", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("go test event object is not closed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("go test event line contains trailing JSON")
	}
	if unsupported != "" {
		return fmt.Errorf("unsupported go test event member %q", unsupported)
	}
	return nil
}

func expectedGoTestPackage(packagePath string) (string, error) {
	if !strings.HasPrefix(packagePath, "./") {
		return "", fmt.Errorf("test package %q is not repository relative", packagePath)
	}
	relative := strings.TrimPrefix(packagePath, "./")
	if relative == "" || strings.Contains(relative, "..") || strings.ContainsAny(relative, "\\:") {
		return "", fmt.Errorf("test package %q is invalid", packagePath)
	}
	return "github.com/jacobcxdev/cq/" + relative, nil
}

func verifyUnit(repositoryRoot string, manifest proxy.CUManifestV1, runner commandRunner) error {
	for _, selection := range manifest.Packages {
		topLevelPattern := exactTestPattern(selection.TopLevelTests)
		listOutput, listStderr, err := runner.Run(
			repositoryRoot,
			nil,
			"go", "test", "-list", topLevelPattern, "-race", "-count=1", selection.Package,
		)
		if err != nil {
			return fmt.Errorf("list tests for %s: %w: %s", selection.Package, err, bytes.TrimSpace(listStderr))
		}
		if err := verifyListedTests(selection, listOutput); err != nil {
			return err
		}
		testOutput, testStderr, err := runner.Run(
			repositoryRoot,
			nil,
			"go", "test", "-json", "-race", "-count=1", "-run", topLevelPattern, selection.Package,
		)
		if err != nil {
			return fmt.Errorf("run tests for %s: %w: %s", selection.Package, err, bytes.TrimSpace(testStderr))
		}
		if err := verifyTestEvents(selection, bytes.NewReader(testOutput), manifest.RaceCount == 1); err != nil {
			return fmt.Errorf("verify tests for %s: %w", selection.Package, err)
		}
	}
	return nil
}

func exactTestPattern(tests []string) string {
	escaped := make([]string, len(tests))
	for index, name := range tests {
		escaped[index] = regexp.QuoteMeta(name)
	}
	return "^(?:" + strings.Join(escaped, "|") + ")$"
}

func verifyListedTests(selection proxy.CUTestPackageV1, output []byte) error {
	var listed []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Test") {
			listed = append(listed, line)
		}
	}
	sort.Strings(listed)
	if len(listed) != len(selection.TopLevelTests) {
		return fmt.Errorf("package %s listed %d tests, want %d", selection.Package, len(listed), len(selection.TopLevelTests))
	}
	for index := range listed {
		if listed[index] != selection.TopLevelTests[index] {
			return fmt.Errorf("package %s listed test %q, want %q", selection.Package, listed[index], selection.TopLevelTests[index])
		}
	}
	return nil
}

func selfTest() error {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestSelf"},
		FullTestIDs:      []string{"TestSelf"},
		MinimumPassCount: 1,
	}
	valid := "{\"Action\":\"start\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\"}\n" +
		"{\"Action\":\"run\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\"}\n" +
		"{\"Action\":\"output\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\",\"Output\":\"=== RUN   TestSelf\\n\"}\n" +
		"{\"Action\":\"pause\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\"}\n" +
		"{\"Action\":\"cont\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\"}\n" +
		"{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\"}\n" +
		"{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\"}\n"
	if err := verifyTestEvents(selection, strings.NewReader(valid), true); err != nil {
		return fmt.Errorf("positive self-test: %w", err)
	}
	for name, fixture := range map[string]struct {
		events      string
		raceEnabled bool
	}{
		"absent":        {events: "{\"Action\":\"pass\",\"Package\":\"self\"}\n", raceEnabled: true},
		"missing pass":  {events: "{\"Action\":\"run\",\"Package\":\"self\",\"Test\":\"TestSelf\"}\n", raceEnabled: true},
		"extra":         {events: "{\"Action\":\"run\",\"Package\":\"self\",\"Test\":\"TestExtra\"}\n", raceEnabled: true},
		"duplicate":     {events: strings.Repeat("{\"Action\":\"run\",\"Package\":\"self\",\"Test\":\"TestSelf\"}\n", 2), raceEnabled: true},
		"skipped":       {events: "{\"Action\":\"skip\",\"Package\":\"self\",\"Test\":\"TestSelf\"}\n", raceEnabled: true},
		"malformed":     {events: "{\n", raceEnabled: true},
		"race disabled": {events: valid, raceEnabled: false},
		"no tests":      {events: "{\"Action\":\"output\",\"Package\":\"self\",\"Output\":\"[no tests to run]\\n\"}\n", raceEnabled: true},
	} {
		if err := verifyTestEvents(selection, strings.NewReader(fixture.events), fixture.raceEnabled); err == nil {
			return fmt.Errorf("negative self-test %q was accepted", name)
		}
	}
	return nil
}
