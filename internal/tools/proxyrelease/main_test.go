package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func TestOSReleaseRunnerContainsPipeHoldingGrandchild(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	runner := osReleaseRunner{
		environment: append(os.Environ(), "CQ_RELEASE_PROCESS_HELPER=parent", "CQ_RELEASE_PROCESS_PID_FILE="+pidFile),
		timeout:     100 * time.Millisecond,
		waitDelay:   100 * time.Millisecond,
	}
	started := time.Now()
	result := runner.Run(t.TempDir(), []string{executable, "-test.run=^TestReleaseProcessGroupHelper$"})
	if result.TerminationReason != "timeout" {
		t.Fatalf("termination reason = %q, want timeout", result.TerminationReason)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runner waited %s for a descendant-held pipe", elapsed)
	}
	assertReleaseProcessGone(t, pidFile)
}

func TestReleaseProcessGroupHelper(t *testing.T) {
	switch os.Getenv("CQ_RELEASE_PROCESS_HELPER") {
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
		child := exec.Command(executable, "-test.run=^TestReleaseProcessGroupHelper$")
		child.Env = append(os.Environ(), "CQ_RELEASE_PROCESS_HELPER=grandchild")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		if err := os.WriteFile(os.Getenv("CQ_RELEASE_PROCESS_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(93)
		}
		time.Sleep(3 * time.Second)
		os.Exit(0)
	default:
		os.Exit(94)
	}
}

func assertReleaseProcessGone(t *testing.T, pidFile string) {
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

func TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown(t *testing.T) {
	valid := `{"bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"floor","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`
	manifest, err := parseReleaseBuildManifestV1(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Purpose != "floor" || manifest.BundlePath != "release-bundle" {
		t.Fatalf("manifest = %#v", manifest)
	}
	unknown := strings.Replace(valid, `"bundle_path"`, `"extra":true,"bundle_path"`, 1)
	if _, err := parseReleaseBuildManifestV1(strings.NewReader(unknown)); err == nil {
		t.Fatal("accepted unknown release-build manifest member")
	}
}

func TestReadReleaseBuildManifestRejectsSymlink(t *testing.T) {
	valid := []byte(`{"bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"floor","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`)
	directory := t.TempDir()
	target := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(target, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "manifest-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readReleaseBuildManifestV1(link); err == nil {
		t.Fatal("accepted symlink release-build manifest")
	}
}

func TestSourceTreeDigestV1HashesExactGitListing(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	listing, err := gitOutput(repositoryRoot, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(listing)
	got, err := sourceTreeDigestV1(repositoryRoot, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("source tree digest = %s, want %x", got, want)
	}
}

func TestCaptureConstructionUnitBuildsReportOnlyFromCompletedCapture(t *testing.T) {
	manifest := releaseBuildManifestV1{RepositoryRoot: "/repo", WorkingDirectory: "."}
	runner := &fakeReleaseRunner{result: releaseCommandResult{
		Stdout:            []byte(`{"kind":"construction_unit_report_v1"}`),
		ExitCode:          0,
		TerminationReason: "exited",
	}}
	times := []time.Time{
		time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 14, 17, 0, 1, 0, time.UTC),
	}
	clock := func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	report, err := captureConstructionUnit(manifest, "CU-0", runner, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !runner.returned || report.Kind != "construction_unit_report_v1" || report.ExecutionResultDigest == "" {
		t.Fatalf("capture report = %#v, runner returned = %t", report, runner.returned)
	}
}

func TestCaptureConstructionUnitBindsArgvStdoutStderrAndReportBytes(t *testing.T) {
	manifest := releaseBuildManifestV1{RepositoryRoot: "/repo", WorkingDirectory: "."}
	clock := func() time.Time { return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC) }
	capture := func(stdout, stderr []byte) (proxy.CUReportV1, *fakeReleaseRunner) {
		runner := &fakeReleaseRunner{result: releaseCommandResult{Stdout: stdout, Stderr: stderr, ExitCode: 0, TerminationReason: "exited"}}
		report, err := captureConstructionUnit(manifest, "CU-0", runner, clock)
		if err != nil {
			t.Fatal(err)
		}
		return report, runner
	}
	base, runner := capture([]byte("stdout"), []byte("stderr"))
	if runner.repositoryRoot != "/repo" || strings.Join(runner.argv, "\x00") != "./scripts/verify-proxy-cu\x00CU-0" {
		t.Fatalf("captured invocation = root %q argv %q", runner.repositoryRoot, runner.argv)
	}
	wantInvocation, err := proxy.CommandDigestV1("verify-CU-0", ".", []string{"./scripts/verify-proxy-cu", "CU-0"})
	if err != nil {
		t.Fatal(err)
	}
	if base.InvocationDigest != wantInvocation {
		t.Fatalf("invocation digest = %s, want %s", base.InvocationDigest, wantInvocation)
	}
	for name, streams := range map[string]struct{ stdout, stderr []byte }{
		"stdout":              {stdout: []byte("changed"), stderr: []byte("stderr")},
		"stderr":              {stdout: []byte("stdout"), stderr: []byte("changed")},
		"report-shaped bytes": {stdout: []byte(`{"kind":"construction_unit_report_v1"}`), stderr: []byte("stderr")},
	} {
		t.Run(name, func(t *testing.T) {
			changed, _ := capture(streams.stdout, streams.stderr)
			if changed.ExecutionResultDigest == base.ExecutionResultDigest {
				t.Fatal("capture substitution preserved execution-result digest")
			}
		})
	}
}

func TestCaptureConstructionUnitRecoversRunnerPanic(t *testing.T) {
	runner := &fakeReleaseRunner{panicValue: errors.New("runner crash")}
	clock := func() time.Time { return time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC) }
	if _, err := captureConstructionUnit(releaseBuildManifestV1{RepositoryRoot: "/repo", WorkingDirectory: "."}, "CU-0", runner, clock); err == nil {
		t.Fatal("accepted panicking runner")
	}
}

func TestBuildProxyReleaseShellEntryRejectsMissingManifest(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "build-proxy-release"))
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("build-proxy-release accepted missing manifest")
	}
	if !bytes.Contains(output, []byte("exactly one release-build manifest")) {
		t.Fatalf("unexpected error output: %s", output)
	}
}

type fakeReleaseRunner struct {
	result         releaseCommandResult
	panicValue     any
	returned       bool
	repositoryRoot string
	argv           []string
}

func (runner *fakeReleaseRunner) Run(repositoryRoot string, argv []string) releaseCommandResult {
	if runner.panicValue != nil {
		panic(runner.panicValue)
	}
	runner.returned = true
	runner.repositoryRoot = repositoryRoot
	runner.argv = append([]string(nil), argv...)
	return runner.result
}
