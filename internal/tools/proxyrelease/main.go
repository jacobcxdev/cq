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
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	maxReleaseBuildManifestBytes = 64 << 10
	maxReleaseCaptureBytes       = 8 << 20
)

type releaseBuildManifestV1 struct {
	SchemaVersion    int      `json:"schema_version"`
	Kind             string   `json:"kind"`
	RepositoryRoot   string   `json:"repository_root"`
	SourceCommit     string   `json:"source_commit"`
	WorkingDirectory string   `json:"working_directory"`
	CUIDs            []string `json:"cu_ids"`
}

type releaseCommandResult struct {
	Stdout            []byte
	Stderr            []byte
	ExitCode          int32
	TerminationReason string
}

type releaseRunner interface {
	Run(repositoryRoot string, argv []string) releaseCommandResult
}

type releaseCaptureV1 struct {
	SchemaVersion int                `json:"schema_version"`
	Kind          string             `json:"kind"`
	SourceCommit  string             `json:"source_commit"`
	Reports       []proxy.CUReportV1 `json:"reports"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "build-proxy-release: expected exactly one release-build manifest")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdout, osReleaseRunner{}); err != nil {
		fmt.Fprintf(os.Stderr, "build-proxy-release: %v\n", err)
		os.Exit(1)
	}
}

func run(manifestPath string, output io.Writer, runner releaseRunner) error {
	manifest, err := readReleaseBuildManifestV1(manifestPath)
	if err != nil {
		return err
	}
	if err := verifyExactCleanSource(manifest); err != nil {
		return err
	}
	if err := proxy.VerifyBlueprintReview(
		filepath.Join(manifest.RepositoryRoot, "docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md"),
		filepath.Join(manifest.RepositoryRoot, "docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json"),
	); err != nil {
		return err
	}
	reports := make([]proxy.CUReportV1, 0, len(manifest.CUIDs))
	for _, cuID := range manifest.CUIDs {
		report, err := captureConstructionUnit(manifest, cuID, runner, time.Now)
		if err != nil {
			return err
		}
		reports = append(reports, report)
	}
	encoded, err := proxy.CanonicalJSONV1(releaseCaptureV1{
		SchemaVersion: 1,
		Kind:          "proxy_release_capture_v1",
		SourceCommit:  manifest.SourceCommit,
		Reports:       reports,
	})
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = output.Write(encoded)
	return err
}

func readReleaseBuildManifestV1(path string) (releaseBuildManifestV1, error) {
	var manifest releaseBuildManifestV1
	before, err := os.Lstat(path)
	if err != nil {
		return manifest, fmt.Errorf("inspect release-build manifest: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return manifest, fmt.Errorf("release-build manifest must be a regular non-symlink file")
	}
	opener, ok := any(fsutil.OSFileSystem{}).(fsutil.NoFollowFileOpener)
	if !ok {
		return manifest, fsutil.ErrSecureCapabilityUnavailable
	}
	file, err := opener.OpenNoFollow(path)
	if err != nil {
		return manifest, fmt.Errorf("open release-build manifest: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(before, openedInfo) {
		_ = file.Close()
		if statErr != nil {
			return manifest, statErr
		}
		return manifest, fmt.Errorf("release-build manifest changed before read")
	}
	manifest, parseErr := parseReleaseBuildManifestV1(file)
	closeErr := file.Close()
	if parseErr != nil {
		return manifest, parseErr
	}
	if closeErr != nil {
		return manifest, fmt.Errorf("close release-build manifest: %w", closeErr)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, after) {
		return manifest, fmt.Errorf("release-build manifest changed during read")
	}
	return manifest, nil
}

func parseReleaseBuildManifestV1(reader io.Reader) (releaseBuildManifestV1, error) {
	var manifest releaseBuildManifestV1
	data, err := io.ReadAll(io.LimitReader(reader, maxReleaseBuildManifestBytes+1))
	if err != nil {
		return manifest, fmt.Errorf("read release-build manifest: %w", err)
	}
	if len(data) > maxReleaseBuildManifestBytes {
		return manifest, fmt.Errorf("release-build manifest exceeds %d bytes", maxReleaseBuildManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode release-build manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, fmt.Errorf("release-build manifest has trailing JSON")
	}
	canonical, err := proxy.CanonicalJSONV1(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(canonical, data) {
		return manifest, fmt.Errorf("release-build manifest is not canonical JCS")
	}
	if manifest.SchemaVersion != 1 || manifest.Kind != "proxy_release_build_manifest_v1" {
		return manifest, fmt.Errorf("invalid release-build manifest schema or kind")
	}
	if !filepath.IsAbs(manifest.RepositoryRoot) || filepath.Clean(manifest.RepositoryRoot) != manifest.RepositoryRoot {
		return manifest, fmt.Errorf("repository_root must be an absolute canonical path")
	}
	if manifest.WorkingDirectory != "." {
		return manifest, fmt.Errorf("working_directory must be repository-relative dot")
	}
	if len(manifest.SourceCommit) != 40 || strings.Trim(manifest.SourceCommit, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("source_commit must be 40 lower-case hexadecimal characters")
	}
	if len(manifest.CUIDs) != 1 || manifest.CUIDs[0] != "CU-0" {
		return manifest, fmt.Errorf("current release-build manifest requires exactly CU-0")
	}
	return manifest, nil
}

func captureConstructionUnit(manifest releaseBuildManifestV1, cuID string, runner releaseRunner, clock func() time.Time) (report proxy.CUReportV1, err error) {
	manifestDigest, err := proxy.VerificationManifestDigestV1(cuID)
	if err != nil {
		return report, err
	}
	argv := []string{"./scripts/verify-proxy-cu", cuID}
	invocationDigest, err := proxy.CommandDigestV1("verify-"+cuID, manifest.WorkingDirectory, argv)
	if err != nil {
		return report, err
	}
	startedAt := clock()
	var result releaseCommandResult
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("construction-unit runner panic: %v", recovered)
			}
		}()
		result = runner.Run(manifest.RepositoryRoot, argv)
	}()
	if err != nil {
		return report, err
	}
	endedAt := clock()
	return proxy.NewCUReportV1(proxy.CUReportCaptureV1{
		CUID:                       cuID,
		VerificationManifestDigest: manifestDigest,
		InvocationDigest:           invocationDigest,
		ExitCode:                   result.ExitCode,
		TerminationReason:          result.TerminationReason,
		RaceEnabled:                true,
		Stdout:                     result.Stdout,
		Stderr:                     result.Stderr,
		StartedAt:                  startedAt,
		EndedAt:                    endedAt,
	})
}

func verifyExactCleanSource(manifest releaseBuildManifestV1) error {
	head, err := gitOutput(manifest.RepositoryRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != manifest.SourceCommit {
		return fmt.Errorf("source commit does not match HEAD")
	}
	status, err := gitOutput(manifest.RepositoryRoot, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return fmt.Errorf("release source is not clean")
	}
	return nil
}

func gitOutput(directory string, args ...string) ([]byte, error) {
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

type osReleaseRunner struct{}

func (osReleaseRunner) Run(repositoryRoot string, argv []string) releaseCommandResult {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = repositoryRoot
	stdout := &releaseBoundedBuffer{limit: maxReleaseCaptureBytes}
	stderr := &releaseBoundedBuffer{limit: maxReleaseCaptureBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := releaseCommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), TerminationReason: "exited"}
	if ctx.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		result.TerminationReason = "timeout"
		return result
	}
	if err == nil {
		return result
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		result.ExitCode = -1
		result.TerminationReason = "signalled"
		return result
	}
	result.ExitCode = int32(exitError.ExitCode())
	if exitError.ProcessState != nil && !exitError.ProcessState.Exited() {
		result.TerminationReason = "signalled"
	}
	return result
}

type releaseBoundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *releaseBoundedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if len(data) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(data[:remaining])
		}
		return remaining, fmt.Errorf("capture exceeds %d bytes", buffer.limit)
	}
	return buffer.buffer.Write(data)
}

func (buffer *releaseBoundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }
