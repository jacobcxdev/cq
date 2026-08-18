package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	maxReleaseBuildManifestBytes = 64 << 10
	maxReleaseCaptureBytes       = 8 << 20
)

type releaseBuildManifestV1 struct {
	SchemaVersion                       int    `json:"schema_version"`
	Kind                                string `json:"kind"`
	Purpose                             string `json:"purpose"`
	ApprovedReleaseBuildAuthorityDigest string `json:"approved_release_build_authority_digest"`
	ApprovedEd25519PublicKey            string `json:"approved_ed25519_public_key"`
	ApprovedAuthorityPath               string `json:"approved_authority_path"`
	RepositoryIdentityDigest            string `json:"repository_identity_digest"`
	SourceCommit                        string `json:"source_commit"`
	SourceTreeDigest                    string `json:"source_tree_digest"`
	BundlePath                          string `json:"bundle_path"`
	RepositoryRoot                      string `json:"-"`
	WorkingDirectory                    string `json:"-"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "build-proxy-release: expected exactly one release-build manifest")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "build-proxy-release: %v\n", err)
		os.Exit(1)
	}
}

func run(manifestPath string, output io.Writer) error {
	manifest, err := readReleaseBuildManifestV1(manifestPath)
	if err != nil {
		return err
	}
	return releaseProductionFeatureInactive(manifest.Purpose)
}

func releaseProductionFeatureInactive(purpose string) error {
	if purpose == "floor" {
		return fmt.Errorf("feature inactive: floor release requires Task 13/CU-8 construction authority")
	}
	return fmt.Errorf("feature inactive: target release requires Task 14/CU-9 construction authority")
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
	if len(manifest.ApprovedReleaseBuildAuthorityDigest) != 64 || strings.Trim(manifest.ApprovedReleaseBuildAuthorityDigest, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("approved_release_build_authority_digest must be 64 lower-case hexadecimal characters")
	}
	if len(manifest.ApprovedEd25519PublicKey) != 64 || strings.Trim(manifest.ApprovedEd25519PublicKey, "0123456789abcdef") != "" {
		return manifest, fmt.Errorf("approved_ed25519_public_key must be a 32-byte lower-case hexadecimal key")
	}
	if manifest.ApprovedAuthorityPath == "" || filepath.IsAbs(manifest.ApprovedAuthorityPath) || filepath.Clean(manifest.ApprovedAuthorityPath) != manifest.ApprovedAuthorityPath || manifest.ApprovedAuthorityPath == ".." || strings.HasPrefix(manifest.ApprovedAuthorityPath, "../") || strings.Contains(manifest.ApprovedAuthorityPath, `\`) {
		return manifest, fmt.Errorf("approved_authority_path must be canonical and repository-relative")
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
