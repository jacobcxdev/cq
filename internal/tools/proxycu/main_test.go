package main

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestVerifyTestEventsAcceptsExactRunAndPass(t *testing.T) {
	selection := proxy.CUTestPackageV1{
		Package:          "./internal/proxy",
		TopLevelTests:    []string{"TestOne"},
		FullTestIDs:      []string{"TestOne", "TestOne/edge"},
		MinimumPassCount: 2,
	}
	events := strings.Join([]string{
		`{"Action":"run","Package":"example/proxy","Test":"TestOne"}`,
		`{"Action":"run","Package":"example/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"example/proxy","Test":"TestOne/edge"}`,
		`{"Action":"pass","Package":"example/proxy","Test":"TestOne"}`,
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
		{stdout: []byte("TestOne\nok\texample/proxy\t0.001s\n")},
		{stdout: []byte("{\"Action\":\"run\",\"Package\":\"example/proxy\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"example/proxy\",\"Test\":\"TestOne\"}\n")},
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
