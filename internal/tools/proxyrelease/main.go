package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	maxReleaseBuildManifestBytes = 64 << 10
	maxReleaseCaptureBytes       = 8 << 20
)

type releaseBuildManifestV1 struct {
	SchemaVersion            int    `json:"schema_version"`
	Kind                     string `json:"kind"`
	Purpose                  string `json:"purpose"`
	RepositoryIdentityDigest string `json:"repository_identity_digest"`
	SourceCommit             string `json:"source_commit"`
	SourceTreeDigest         string `json:"source_tree_digest"`
	BundlePath               string `json:"bundle_path"`
	RepositoryRoot           string `json:"-"`
	WorkingDirectory         string `json:"-"`
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
	repositoryRoot, err := filepath.EvalSymlinks(".")
	if err != nil {
		return fmt.Errorf("resolve wrapper repository root: %w", err)
	}
	repositoryRoot, err = filepath.Abs(repositoryRoot)
	if err != nil {
		return err
	}
	manifest.RepositoryRoot = repositoryRoot
	manifest.WorkingDirectory = "."
	if err := verifyPinnedReleaseToolchain(); err != nil {
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
	_ = runner
	bundleRoot := filepath.Join(repositoryRoot, filepath.FromSlash(manifest.BundlePath))
	graph, err := readReleaseGraphV1(bundleRoot)
	if err != nil {
		return err
	}
	if graph.Bundle.Purpose != manifest.Purpose || graph.Authority.RepositoryIdentityDigest != manifest.RepositoryIdentityDigest || graph.ArtifactSet.SourceCommit != manifest.SourceCommit || graph.ArtifactSet.SourceTreeDigest != manifest.SourceTreeDigest {
		return fmt.Errorf("release-build manifest does not equal the complete release graph")
	}
	if err := proxy.VerifyReleaseBundleDirectoryV1(bundleRoot, graph); err != nil {
		return err
	}
	if err := verifyExactCleanSource(manifest); err != nil {
		return fmt.Errorf("post-verification source identity: %w", err)
	}
	encoded, err := proxy.CanonicalJSONV1(graph.Bundle)
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
	if manifest.Purpose != "floor" && manifest.Purpose != "target" {
		return manifest, fmt.Errorf("purpose must be floor or target")
	}
	if len(manifest.RepositoryIdentityDigest) != 64 || strings.Trim(manifest.RepositoryIdentityDigest, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("repository_identity_digest must be 64 lower-case hexadecimal characters")
	}
	if len(manifest.SourceCommit) != 40 || strings.Trim(manifest.SourceCommit, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("source_commit must be 40 lower-case hexadecimal characters")
	}
	if len(manifest.SourceTreeDigest) != 64 || strings.Trim(manifest.SourceTreeDigest, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("source_tree_digest must be 64 lower-case hexadecimal characters")
	}
	if manifest.BundlePath == "" || filepath.IsAbs(manifest.BundlePath) || filepath.Clean(manifest.BundlePath) != manifest.BundlePath || manifest.BundlePath == ".." || strings.HasPrefix(manifest.BundlePath, "../") || strings.Contains(manifest.BundlePath, `\`) {
		return manifest, fmt.Errorf("bundle_path must be canonical and repository-relative")
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
	startedAt := clock().UTC().Truncate(time.Second)
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
	endedAt := clock().UTC().Truncate(time.Second)
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
	tree, err := sourceTreeDigestV1(manifest.RepositoryRoot, manifest.SourceCommit)
	if err != nil {
		return err
	}
	if tree != manifest.SourceTreeDigest {
		return fmt.Errorf("source tree does not match HEAD tree")
	}
	remote, err := gitOutput(manifest.RepositoryRoot, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	repositoryDigest := sha256.Sum256(append([]byte("cq/repository-identity/v1\x00"), bytes.TrimSpace(remote)...))
	if hex.EncodeToString(repositoryDigest[:]) != manifest.RepositoryIdentityDigest {
		return fmt.Errorf("repository identity does not match release manifest")
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

func sourceTreeDigestV1(repositoryRoot, commit string) (string, error) {
	listing, err := gitOutput(repositoryRoot, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(listing)
	return hex.EncodeToString(digest[:]), nil
}

func gitOutput(directory string, args ...string) ([]byte, error) {
	command := exec.Command("/usr/bin/git", args...)
	command.Dir = directory
	command.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "HOME=/var/empty", "LC_ALL=C", "PATH=/usr/bin:/bin", "TZ=UTC"}
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func verifyPinnedReleaseToolchain() error {
	const goPath = "/opt/homebrew/bin/go"
	resolved, err := filepath.EvalSymlinks(goPath)
	if err != nil || !filepath.IsAbs(resolved) {
		return fmt.Errorf("resolve pinned Go tool: %w", err)
	}
	command := exec.Command(resolved, "version")
	command.Env = []string{"GOENV=off", "GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off", "HOME=/var/empty", "LC_ALL=C", "PATH=/usr/bin:/bin", "TZ=UTC"}
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("verify pinned Go tool: %w", err)
	}
	if strings.TrimSpace(string(output)) != "go version go1.26.1 darwin/arm64" {
		return fmt.Errorf("release build requires go version go1.26.1 darwin/arm64")
	}
	return nil
}

func readReleaseGraphV1(root string) (proxy.ReleaseGraphV1, error) {
	var graph proxy.ReleaseGraphV1
	if err := readCanonicalReleaseObject(filepath.Join(root, "bundle.json"), &graph.Bundle); err != nil {
		return graph, err
	}
	if err := readCanonicalReleaseObject(filepath.Join(root, "release-build-authority.json"), &graph.Authority); err != nil {
		return graph, err
	}
	if err := readCanonicalReleaseObject(filepath.Join(root, "release-artifact-set.json"), &graph.ArtifactSet); err != nil {
		return graph, err
	}
	for path, destination := range map[string]any{
		"reports/build.json":              &graph.BuildReport,
		"reports/vet.json":                &graph.VetReport,
		"reports/race.json":               &graph.RaceReport,
		"reports/construction-units.json": &graph.CUReportSet,
	} {
		if err := readCanonicalReleaseObject(filepath.Join(root, filepath.FromSlash(path)), destination); err != nil {
			return graph, err
		}
	}
	roles := []string{"supervisor", "worker"}
	if graph.Bundle.Purpose == "target" {
		roles = []string{"launcher", "supervisor", "worker"}
		graph.Ancestry = &proxy.SourceAncestryReceiptV1{}
		if err := readCanonicalReleaseObject(filepath.Join(root, "source-ancestry.json"), graph.Ancestry); err != nil {
			return graph, err
		}
	}
	for _, role := range roles {
		var manifest proxy.ReleaseArtifactManifestV1
		if err := readCanonicalReleaseObject(filepath.Join(root, "manifests", role+".json"), &manifest); err != nil {
			return graph, err
		}
		graph.ArtifactManifests = append(graph.ArtifactManifests, manifest)
	}
	return graph, nil
}

func readCanonicalReleaseObject(path string, destination any) error {
	data, err := readNoFollowReleaseBytes(path, maxReleaseBuildManifestBytes)
	if err != nil {
		return fmt.Errorf("read release object %s: %w", filepath.Base(path), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode release object %s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("release object %s has trailing JSON", filepath.Base(path))
	}
	canonical, err := proxy.CanonicalJSONV1(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("release object %s is not canonical JCS", filepath.Base(path))
	}
	return nil
}

func readNoFollowReleaseBytes(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular non-symlink file")
	}
	opener, ok := any(fsutil.OSFileSystem{}).(fsutil.NoFollowFileOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	file, err := opener.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(before, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("path changed before read")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("read bounded object: %v %v", readErr, closeErr)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, after) {
		return nil, fmt.Errorf("path changed during read")
	}
	return data, nil
}

type osReleaseRunner struct {
	environment []string
	timeout     time.Duration
	waitDelay   time.Duration
}

func (runner osReleaseRunner) Run(repositoryRoot string, argv []string) releaseCommandResult {
	timeout := runner.timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	waitDelay := runner.waitDelay
	if waitDelay == 0 {
		waitDelay = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = repositoryRoot
	command.Env = runner.environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return killReleaseProcessGroup(command.Process) }
	command.WaitDelay = waitDelay
	stdout := &releaseBoundedBuffer{limit: maxReleaseCaptureBytes}
	stderr := &releaseBoundedBuffer{limit: maxReleaseCaptureBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	_ = killReleaseProcessGroup(command.Process)
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

func killReleaseProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
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
