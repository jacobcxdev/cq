//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var releaseSigningKeyReader io.Reader = os.Stdin

func buildOperationalRelease(manifest releaseBuildManifestV1, output io.Writer) (resultErr error) {
	repositoryRoot, err := filepath.EvalSymlinks(".")
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	repositoryRoot, err = filepath.Abs(repositoryRoot)
	if err != nil {
		return err
	}
	if err := verifyOperationalReleaseSource(repositoryRoot, manifest); err != nil {
		return err
	}
	authority, err := readOperationalReleaseAuthority(filepath.Join(repositoryRoot, manifest.ApprovedAuthorityPath))
	if err != nil {
		return err
	}
	pin := proxy.ReleaseAuthorityPinV1{Digest: manifest.ApprovedReleaseBuildAuthorityDigest, Ed25519PublicKey: manifest.ApprovedEd25519PublicKey}
	if err := proxy.VerifyMinimalReleaseBuildAuthority(authority, pin); err != nil {
		return fmt.Errorf("verify release authority: %w", err)
	}
	if authority.RepositoryIdentityDigest != manifest.RepositoryIdentityDigest {
		return fmt.Errorf("release repository identity mismatch")
	}
	privateKey, err := readOperationalReleaseSigningKey(releaseSigningKeyReader, authority.Ed25519PublicKey)
	if err != nil {
		return err
	}
	defer zeroReleaseBytes(privateKey)
	if err := verifyOperationalReleaseToolchain(repositoryRoot, authority.ToolchainIdentity); err != nil {
		return err
	}
	bundlePath := filepath.Join(repositoryRoot, manifest.BundlePath)
	if _, err := os.Lstat(bundlePath); err == nil {
		return fmt.Errorf("release bundle path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(bundlePath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return fmt.Errorf("release bundle parent is not an exact directory")
	}
	temporary, err := os.MkdirTemp(parent, ".cq-release-build-")
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			resultErr = errors.Join(resultErr, os.RemoveAll(temporary))
		}
	}()
	roles := []string{"launcher", "supervisor", "worker"}
	if manifest.Purpose == "floor" {
		roles = roles[1:]
	}
	binaryPath := filepath.Join(temporary, "cq")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/opt/homebrew/bin/go", "build", "-trimpath", "-o", binaryPath, "./cmd/cq")
	command.Dir = repositoryRoot
	command.Env = os.Environ()
	buildOutput, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build release executable: %w: %s", err, bytes.TrimSpace(buildOutput))
	}
	artifactDirectory := filepath.Join(temporary, "artifacts")
	if err := os.Mkdir(artifactDirectory, 0o700); err != nil {
		return err
	}
	roleEntries := make([]proxy.OperationalReleaseRoleV1, 0, len(roles))
	for _, role := range roles {
		target := filepath.Join(artifactDirectory, "cq-"+role)
		if err := copyReleaseArtifact(binaryPath, target); err != nil {
			return err
		}
		digest, size, err := hashReleaseArtifact(target)
		if err != nil {
			return err
		}
		roleEntries = append(roleEntries, proxy.OperationalReleaseRoleV1{Role: role, ArtifactDigest: digest, ByteCount: size})
	}
	if err := os.Remove(binaryPath); err != nil {
		return err
	}
	bundle, err := proxy.BuildOperationalReleaseBundleV1(authority, proxy.OperationalReleaseBuildInputV1{
		Purpose: manifest.Purpose, AuthorityDigest: manifest.ApprovedReleaseBuildAuthorityDigest,
		SourceCommit: manifest.SourceCommit, SourceTreeDigest: manifest.SourceTreeDigest,
		Roles: roleEntries, BuiltAt: time.Now().UTC().Truncate(time.Second),
	}, privateKey)
	if err != nil {
		return err
	}
	if err := proxy.VerifyOperationalReleaseBundleV1(authority, bundle, pin); err != nil {
		return err
	}
	canonical, err := proxy.CanonicalJSONV1(bundle)
	if err != nil {
		return err
	}
	bundleFile := filepath.Join(temporary, "bundle.json")
	if err := os.WriteFile(bundleFile, canonical, 0o600); err != nil {
		return err
	}
	if err := syncReleasePath(bundleFile); err != nil {
		return err
	}
	if err := syncReleasePath(artifactDirectory); err != nil {
		return err
	}
	if err := syncReleasePath(temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, bundlePath); err != nil {
		return err
	}
	removeTemporary = false
	if err := syncReleasePath(parent); err != nil {
		return err
	}
	_, err = output.Write(append(canonical, '\n'))
	return err
}

func verifyOperationalReleaseSource(repositoryRoot string, manifest releaseBuildManifestV1) error {
	status := exec.Command("/usr/bin/git", "status", "--porcelain=v1", "--untracked-files=all")
	status.Dir = repositoryRoot
	statusOutput, err := status.Output()
	if err != nil || len(statusOutput) != 0 {
		return fmt.Errorf("release source worktree is not clean")
	}
	head := exec.Command("/usr/bin/git", "rev-parse", "HEAD")
	head.Dir = repositoryRoot
	headOutput, err := head.Output()
	if err != nil || strings.TrimSpace(string(headOutput)) != manifest.SourceCommit {
		return fmt.Errorf("release source commit mismatch")
	}
	tree := exec.Command("/usr/bin/git", "ls-tree", "-r", "--full-tree", "HEAD")
	tree.Dir = repositoryRoot
	treeOutput, err := tree.Output()
	if err != nil {
		return fmt.Errorf("read release source tree: %w", err)
	}
	digest := sha256.Sum256(treeOutput)
	if hex.EncodeToString(digest[:]) != manifest.SourceTreeDigest {
		return fmt.Errorf("release source tree digest mismatch")
	}
	return nil
}

func readOperationalReleaseAuthority(path string) (proxy.ReleaseBuildAuthorityV1, error) {
	var authority proxy.ReleaseBuildAuthorityV1
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return authority, fmt.Errorf("inspect release authority: %w", err)
	}
	opener, ok := any(fsutil.OSFileSystem{}).(fsutil.NoFollowFileOpener)
	if !ok {
		return authority, fsutil.ErrSecureCapabilityUnavailable
	}
	file, err := opener.OpenNoFollow(path)
	if err != nil {
		return authority, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReleaseBuildManifestBytes+1))
	if err != nil || len(data) > maxReleaseBuildManifestBytes {
		return authority, fmt.Errorf("read release authority: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil {
		return authority, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return authority, fmt.Errorf("release authority has trailing JSON")
	}
	canonical, err := proxy.CanonicalJSONV1(authority)
	if err != nil || !bytes.Equal(canonical, data) {
		return authority, fmt.Errorf("release authority is not canonical JCS")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return authority, fmt.Errorf("release authority changed during read")
	}
	return authority, nil
}

func readOperationalReleaseSigningKey(reader io.Reader, encodedPublicKey string) (ed25519.PrivateKey, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 66))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	if len(data) != 64 {
		return nil, fmt.Errorf("release signing seed must be exactly 64 lower-case hexadecimal characters")
	}
	seed, err := hex.DecodeString(string(data))
	if err != nil || hex.EncodeToString(seed) != string(data) || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("release signing seed is invalid")
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	zeroReleaseBytes(seed)
	if hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)) != encodedPublicKey {
		zeroReleaseBytes(privateKey)
		return nil, fmt.Errorf("release signing seed does not match authority")
	}
	return privateKey, nil
}

func verifyOperationalReleaseToolchain(repositoryRoot, want string) error {
	command := exec.Command("/opt/homebrew/bin/go", "version")
	command.Dir = repositoryRoot
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != "go version "+want {
		return fmt.Errorf("release toolchain mismatch")
	}
	return nil
}

func copyReleaseArtifact(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func hashReleaseArtifact(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func syncReleasePath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func zeroReleaseBytes(data []byte) {
	for index := range data {
		data[index] = 0
	}
}
