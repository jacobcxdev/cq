package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

func runCU0(repositoryRoot, cuID string) error {
	if cuID != "CU-0" {
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
	runner, cleanup, err := newOSCommandRunner()
	if err != nil {
		return err
	}
	defer cleanup()
	return verifyUnit(repositoryRoot, manifest, runner)
}

type osCommandRunner struct {
	environment []string
	goTool      string
	timeout     time.Duration
	waitDelay   time.Duration
}

func newOSCommandRunner() (*osCommandRunner, func(), error) {
	workspace, err := os.MkdirTemp("", "cq-cu0-")
	if err != nil {
		return nil, nil, fmt.Errorf("create isolated CU workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workspace) }
	goTool, err := filepath.EvalSymlinks("/opt/homebrew/bin/go")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("resolve pinned Go tool: %w", err)
	}
	if !filepath.IsAbs(goTool) {
		cleanup()
		return nil, nil, fmt.Errorf("resolved Go tool is not absolute")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("resolve Go cache home: %w", err)
	}
	goEnvironmentCommand := exec.Command(goTool, "env", "GOMODCACHE", "GOCACHE", "GOPATH")
	goEnvironmentCommand.Env = []string{
		"GOENV=off",
		"GOFLAGS=",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"HOME=" + home,
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
		"TZ=UTC",
	}
	goEnvironment, err := goEnvironmentCommand.Output()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("resolve Go cache paths: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(goEnvironment)), "\n")
	if len(parts) != 3 {
		cleanup()
		return nil, nil, fmt.Errorf("unexpected go env output")
	}
	environment := []string{
		"CGO_ENABLED=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GOCACHE=" + parts[1],
		"GOENV=off",
		"GOFLAGS=",
		"GOMODCACHE=" + parts[0],
		"GONOSUMDB=*",
		"GOPATH=" + parts[2],
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"HOME=" + filepath.Join(workspace, "home"),
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
		"TMPDIR=" + workspace,
		"TZ=UTC",
		"XDG_CONFIG_HOME=" + filepath.Join(workspace, "xdg"),
	}
	sort.Strings(environment)
	return &osCommandRunner{
		environment: environment,
		goTool:      goTool,
		timeout:     10 * time.Minute,
		waitDelay:   2 * time.Second,
	}, cleanup, nil
}

func (runner *osCommandRunner) Run(directory string, _ []string, name string, args ...string) ([]byte, []byte, error) {
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
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = runner.environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return killProcessGroup(command.Process) }
	command.WaitDelay = runner.waitDelay
	stdout := &boundedBuffer{limit: maxGoTestJSONBytes}
	stderr := &boundedBuffer{limit: maxGoTestJSONBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	_ = killProcessGroup(command.Process)
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("command timed out")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
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
	if len(args) == 1 && mode == "CU-0" {
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
			return fmt.Errorf("usage: unit CU-0")
		}
		if args[1] != "CU-0" {
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
	runs   int
	passes int
}

func verifyTestEvents(selection proxy.CUTestPackageV1, reader io.Reader, raceEnabled bool) error {
	if !raceEnabled {
		return fmt.Errorf("race detector was not enabled")
	}
	expectedPackage, err := expectedGoTestPackage(selection.Package)
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: reader, N: maxGoTestJSONBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	states := make(map[string]*testEventState, len(selection.FullTestIDs))
	for _, name := range selection.FullTestIDs {
		states[name] = &testEventState{}
	}
	packagePasses := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			return fmt.Errorf("blank go test event line")
		}
		if !bytes.Equal(line, bytes.TrimSpace(line)) {
			return fmt.Errorf("go test event line contains surrounding whitespace")
		}
		if packagePasses != 0 {
			return fmt.Errorf("go test event follows package terminal")
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
		if event.Action == "fail" && event.Test == "" {
			return fmt.Errorf("go test package failed")
		}
		if event.Test == "" {
			if event.Action == "pass" {
				packagePasses++
			}
			continue
		}
		state, selected := states[event.Test]
		if !selected {
			return fmt.Errorf("unexpected test event %q", event.Test)
		}
		switch event.Action {
		case "run":
			state.runs++
			if state.runs != 1 {
				return fmt.Errorf("test %q ran more than once", event.Test)
			}
		case "pass":
			state.passes++
			if state.runs != 1 || state.passes != 1 {
				return fmt.Errorf("test %q has invalid pass cardinality", event.Test)
			}
		case "skip", "fail":
			return fmt.Errorf("test %q ended with %s", event.Test, event.Action)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read go test events: %w", err)
	}
	if limited.N <= 0 {
		return fmt.Errorf("go test event stream exceeds %d bytes", maxGoTestJSONBytes)
	}
	if packagePasses != 1 {
		return fmt.Errorf("go test package is missing its exact pass terminal")
	}
	passed := 0
	for name, state := range states {
		if state.runs != 1 || state.passes != 1 {
			return fmt.Errorf("test %q missing exact run/pass events", name)
		}
		passed++
	}
	if passed < selection.MinimumPassCount {
		return fmt.Errorf("observed %d passed tests, want at least %d", passed, selection.MinimumPassCount)
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
	valid := "{\"Action\":\"run\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestSelf\"}\n" +
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
