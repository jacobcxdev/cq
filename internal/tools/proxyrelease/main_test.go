package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown(t *testing.T) {
	valid := `{"cu_ids":["CU-0"],"kind":"proxy_release_build_manifest_v1","repository_root":"/repo","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","working_directory":"."}`
	manifest, err := parseReleaseBuildManifestV1(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.CUIDs) != 1 || manifest.CUIDs[0] != "CU-0" {
		t.Fatalf("manifest = %#v", manifest)
	}
	unknown := strings.Replace(valid, `"cu_ids"`, `"extra":true,"cu_ids"`, 1)
	if _, err := parseReleaseBuildManifestV1(strings.NewReader(unknown)); err == nil {
		t.Fatal("accepted unknown release-build manifest member")
	}
}

func TestReadReleaseBuildManifestRejectsSymlink(t *testing.T) {
	valid := []byte(`{"cu_ids":["CU-0"],"kind":"proxy_release_build_manifest_v1","repository_root":"/repo","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","working_directory":"."}`)
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
	result     releaseCommandResult
	panicValue any
	returned   bool
}

func (runner *fakeReleaseRunner) Run(repositoryRoot string, argv []string) releaseCommandResult {
	if runner.panicValue != nil {
		panic(runner.panicValue)
	}
	runner.returned = true
	return runner.result
}
