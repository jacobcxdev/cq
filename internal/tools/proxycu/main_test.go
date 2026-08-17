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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	runner := &osCommandRunner{
		environment: append(os.Environ(), "CQ_PROCESS_HELPER=parent", "CQ_PROCESS_PID_FILE="+pidFile),
		timeout:     100 * time.Millisecond,
		waitDelay:   100 * time.Millisecond,
	}
	started := time.Now()
	_, _, err = runner.Run(t.TempDir(), nil, executable, "-test.run=^TestProcessGroupHelper$")
	if err == nil {
		t.Fatal("timed-out process group returned success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
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

	runner, cleanup, err := newOSCommandRunner()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

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
}

func TestProcessGroupHelper(t *testing.T) {
	switch os.Getenv("CQ_PROCESS_HELPER") {
	case "":
		return
	case "grandchild":
		time.Sleep(3 * time.Second)
		return
	case "parent":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(91)
		}
		child := exec.Command(executable, "-test.run=^TestProcessGroupHelper$")
		child.Env = append(os.Environ(), "CQ_PROCESS_HELPER=grandchild")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		if err := os.WriteFile(os.Getenv("CQ_PROCESS_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(93)
		}
		time.Sleep(3 * time.Second)
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
		`{"Action":"run","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"run","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy","Test":"TestOne"}`,
		`{"Action":"pass","Package":"github.com/jacobcxdev/cq/internal/proxy"}`,
	}, "\n") + "\n"
	if err := verifyTestEvents(selection, strings.NewReader(events), true); err != nil {
		t.Fatal(err)
	}
}

func TestShellWrappersVerifyFrozenReviewAndSelfTest(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range [][]string{
		{"scripts/verify-blueprint-review"},
		{"scripts/verify-proxy-cu", "--self-test"},
	} {
		command := exec.Command(filepath.Join(repositoryRoot, script[0]), script[1:]...)
		command.Dir = repositoryRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", script, err, output)
		}
	}
}

func TestShellWrappersCloseAmbientGoConfiguration(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, relativePath := range []string{
		"scripts/verify-blueprint-review",
		"scripts/verify-proxy-cu",
	} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot, relativePath))
		if err != nil {
			t.Fatal(err)
		}
		contents := string(data)
		for _, required := range []string{
			"/opt/homebrew/bin/go", "/usr/bin/env -i", "GOENV=off", "GOFLAGS=",
			"GOTOOLCHAIN=local", "GOWORK=off", "PATH=/usr/bin:/bin",
		} {
			if !strings.Contains(contents, required) {
				t.Errorf("%s does not close %q", relativePath, required)
			}
		}
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
		{stdout: []byte("{\"Action\":\"run\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"github.com/jacobcxdev/cq/internal/proxy\"}\n")},
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
		"package substitution": strings.ReplaceAll(run+"\n"+pass+"\n"+terminal+"\n", "github.com/jacobcxdev/cq/internal/proxy", "example/proxy"),
		"missing terminal":     run + "\n" + pass + "\n",
		"blank line":           run + "\n\n" + pass + "\n" + terminal + "\n",
		"whitespace line":      run + "\n \n" + pass + "\n" + terminal + "\n",
		"padded object":        " " + run + "\n" + pass + "\n" + terminal + "\n",
		"concatenated object":  run + `{}` + "\n" + pass + "\n" + terminal + "\n",
		"duplicate terminal":   run + "\n" + pass + "\n" + terminal + "\n" + terminal + "\n",
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
	for _, args := range [][]string{{"unit"}, {"unit", "CU-1"}, {"unit", "CU-0", "extra"}} {
		if err := run(args, testDependencies{}); err == nil {
			t.Fatalf("run(%v) accepted invalid unit invocation", args)
		}
	}
}
