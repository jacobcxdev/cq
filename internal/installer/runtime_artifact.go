package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const RuntimeUpgradeProtocolVersion uint32 = 1
const runtimeArtifactCheckTimeout = 30 * time.Second
const maxRuntimeArtifactBytes = 512 << 20

var ErrRuntimeArtifact = errors.New("invalid retained runtime artifact")

type RuntimeArtifact struct {
	Path            string `json:"path"`
	Version         string `json:"version"`
	SHA256          string `json:"sha256"`
	ProtocolVersion uint32 `json:"protocol_version"`
}

type RuntimeArtifactStore struct {
	FS    fsutil.FileSystem
	Roots userdirs.Roots
}

type RuntimeCheckV1 struct {
	SchemaVersion   int    `json:"schema_version"`
	Version         string `json:"version"`
	ProtocolVersion uint32 `json:"protocol_version"`
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
}

func (store RuntimeArtifactStore) artifactPath(digest string) string {
	return filepath.Join(store.Roots.State, "runtime-artifacts", digest, "cq")
}

func (artifact RuntimeArtifact) Validate() error {
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != artifact.SHA256 || artifact.Version == "" || artifact.ProtocolVersion != RuntimeUpgradeProtocolVersion || !filepath.IsAbs(artifact.Path) {
		return ErrRuntimeArtifact
	}
	return nil
}

func (store RuntimeArtifactStore) Stage(ctx context.Context, source string) (RuntimeArtifact, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeArtifact{}, err
	}
	if store.FS == nil || !filepath.IsAbs(store.Roots.State) {
		return RuntimeArtifact{}, ErrRuntimeArtifact
	}
	lock, err := store.Lock()
	if err != nil {
		return RuntimeArtifact{}, err
	}
	defer lock.Close()
	// Package directories are public. Bind the regular source inode before reading.
	before, err := os.Lstat(source)
	if err != nil {
		return RuntimeArtifact{}, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o022 != 0 {
		return RuntimeArtifact{}, ErrRuntimeArtifact
	}
	file, err := os.Open(source)
	if err != nil {
		return RuntimeArtifact{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return RuntimeArtifact{}, ErrRuntimeArtifact
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeArtifactBytes+1))
	if err != nil || len(data) > maxRuntimeArtifactBytes {
		return RuntimeArtifact{}, errors.Join(ErrRuntimeArtifact, err)
	}
	if err := validateRuntimeBuild(data); err != nil {
		return RuntimeArtifact{}, err
	}
	digest := sha256.Sum256(data)
	artifact := RuntimeArtifact{Path: store.artifactPath(hex.EncodeToString(digest[:])), SHA256: hex.EncodeToString(digest[:]), ProtocolVersion: RuntimeUpgradeProtocolVersion}
	dirPath := filepath.Dir(artifact.Path)
	if err := fsutil.EnsureSecureDirectory(store.FS, dirPath); err != nil {
		return RuntimeArtifact{}, err
	}
	opener, ok := store.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return RuntimeArtifact{}, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(dirPath)
	if err != nil {
		return RuntimeArtifact{}, err
	}
	defer directory.Close()
	if existing, err := directory.OpenNoFollow("cq"); err == nil {
		existing.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return RuntimeArtifact{}, err
	} else {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return RuntimeArtifact{}, err
		}
		name := ".stage-" + hex.EncodeToString(nonce[:])
		writer, err := directory.CreateExclusive(name, 0o500)
		if err != nil {
			return RuntimeArtifact{}, err
		}
		defer directory.Remove(name)
		_, writeErr := writer.Write(data)
		syncErr := writer.Sync()
		closeErr := writer.Close()
		if err := errors.Join(writeErr, syncErr, closeErr, ctx.Err()); err != nil {
			return RuntimeArtifact{}, err
		}
		if err := directory.RenameNoReplace(name, "cq"); err != nil {
			return RuntimeArtifact{}, err
		}
		if err := directory.Sync(); err != nil {
			return RuntimeArtifact{}, err
		}
	}
	artifact.Version = "staging"
	if err := store.verifyStored(ctx, artifact); err != nil {
		return RuntimeArtifact{}, err
	}
	check, err := checkRuntimeExecutable(ctx, artifact.Path)
	if err != nil {
		return RuntimeArtifact{}, err
	}
	artifact.Version = check.Version
	if err := store.Verify(ctx, artifact); err != nil {
		return RuntimeArtifact{}, err
	}
	return artifact, nil
}

func validateRuntimeBuild(data []byte) error {
	info, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil || info.Path != "github.com/jacobcxdev/cq/cmd/cq" {
		return ErrRuntimeArtifact
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("%w: architecture mismatch", ErrRuntimeArtifact)
	}
	return nil
}

func checkRuntimeExecutable(ctx context.Context, path string) (RuntimeCheckV1, error) {
	checkCtx, cancel := context.WithTimeout(ctx, runtimeArtifactCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "proxy", "runtime-check", "--json")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return RuntimeCheckV1{}, err
	}
	if err := cmd.Start(); err != nil {
		return RuntimeCheckV1{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, 4097))
	if len(data) > 4096 || readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil || len(data) > 4096 {
		return RuntimeCheckV1{}, fmt.Errorf("%w: runtime preflight failed", ErrRuntimeArtifact)
	}
	output := bytes.NewBuffer(data)
	var check RuntimeCheckV1
	decoder := json.NewDecoder(output)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&check); err != nil {
		return check, ErrRuntimeArtifact
	}
	if decoder.Decode(new(any)) != io.EOF || check.SchemaVersion != 1 || check.Version == "" || check.ProtocolVersion != RuntimeUpgradeProtocolVersion || check.GOOS != runtime.GOOS || check.GOARCH != runtime.GOARCH {
		return check, ErrRuntimeArtifact
	}
	return check, nil
}

func (store RuntimeArtifactStore) Verify(ctx context.Context, artifact RuntimeArtifact) error {
	if err := store.verifyStored(ctx, artifact); err != nil {
		return err
	}
	check, err := checkRuntimeExecutable(ctx, artifact.Path)
	if err != nil || check.Version != artifact.Version || check.ProtocolVersion != artifact.ProtocolVersion {
		return errors.Join(ErrRuntimeArtifact, err)
	}
	return nil
}

func (store RuntimeArtifactStore) verifyStored(ctx context.Context, artifact RuntimeArtifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	if artifact.Path != store.artifactPath(artifact.SHA256) {
		return ErrRuntimeArtifact
	}
	_, ok := store.FS.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	if err := fsutil.ValidateSecureDirectory(store.FS, filepath.Dir(artifact.Path)); err != nil {
		return err
	}
	opener, ok := store.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	dir, err := opener.OpenSecureDirectory(filepath.Dir(artifact.Path))
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.OpenNoFollow("cq")
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o500 {
		return ErrRuntimeArtifact
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeArtifactBytes+1))
	if err != nil || len(data) > maxRuntimeArtifactBytes {
		return ErrRuntimeArtifact
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return ErrRuntimeArtifact
	}
	return validateRuntimeBuild(data)
}
