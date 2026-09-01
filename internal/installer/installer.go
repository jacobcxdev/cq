package installer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
)

var (
	ErrForeignBinary         = errors.New("foreign CQ binary")
	ErrStagedVersionMismatch = errors.New("staged CQ version mismatch")
	ErrRollbackUnverified    = errors.New("installer rollback could not be verified")
)

const maxInstallerBinaryBytes int64 = 512 << 20

// Installation describes one complete CQ installation.
type Installation struct {
	Owner        installstate.Owner
	Version      string
	Executable   string
	BinaryDigest string
	Services     []string
}

// Lifecycle owns CQ proxy and refresh service mutations.
type Lifecycle interface {
	Stop(context.Context) error
	Install(context.Context, installstate.Owner) error
	Snapshot(context.Context, installstate.Owner, string) error
	Restore(context.Context, installstate.Owner, string) error
	Status(context.Context) error
	Uninstall(context.Context, installstate.Owner) error
}

// PlatformMetadata owns package-manager-visible installation metadata.
type PlatformMetadata interface {
	Install(context.Context, Installation) error
	Remove(context.Context, Installation) error
	Inspect(context.Context, Installation) error
}

// Downloader stages one verified release binary in a caller-owned directory.
type Downloader interface {
	Download(context.Context, string) (StagedBinary, error)
}

// VersionRunner executes staged CQ and reports its semantic version.
type VersionRunner interface {
	Version(context.Context, string) (string, error)
}

// BinaryOwnershipClassifier decides whether an unowned destination can be
// adopted by the Go installer.
type BinaryOwnershipClassifier interface {
	Classify(installstate.Owner, string) (BinaryOwnership, error)
}

type installationState interface {
	Load() (installstate.Record, error)
	Save(installstate.Record) error
	Remove() error
	CheckClaim(installstate.Owner, string) error
}

type temporaryDirectories interface {
	Create() (string, error)
	Remove(string) error
}

type installerFileSystem interface {
	fsutil.DurableFileSystem
	fsutil.ExclusiveFileCreator
	fsutil.NoFollowFileOpener
}

// Installer coordinates binary, service, metadata, and ownership transactions.
type Installer struct {
	FS           installerFileSystem
	Downloader   Downloader
	Runner       VersionRunner
	Classifier   BinaryOwnershipClassifier
	Lifecycle    Lifecycle
	Metadata     PlatformMetadata
	State        installationState
	Locker       InstallerLocker
	Temporary    temporaryDirectories
	Installation Installation
}

// Install installs or upgrades CQ and proves rollback on failure.
func (installer *Installer) Install(ctx context.Context) (resultErr error) {
	if err := installer.validate(); err != nil {
		return err
	}
	lock, err := installer.Locker.Acquire()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	ctx = ContextWithInstallLock(ctx, lock)

	previous, previousRecord, adopted, err := installer.currentInstallation(true)
	if err != nil {
		return err
	}
	destinationCreated, err := installer.ensureExecutableDirectory()
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && destinationCreated {
			if cleanupErr := installer.FS.Remove(filepath.Dir(installer.Installation.Executable)); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove new CQ executable directory: %w", cleanupErr))
			}
		}
	}()
	temporaryRoot, err := installer.Temporary.Create()
	if err != nil {
		return fmt.Errorf("create installer temporary root: %w", err)
	}
	defer func() {
		if cleanupErr := installer.Temporary.Remove(temporaryRoot); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove installer temporary root %s: %w", temporaryRoot, cleanupErr))
		}
	}()

	staged, err := installer.Downloader.Download(ctx, temporaryRoot)
	if err != nil {
		return fmt.Errorf("stage CQ release: %w", err)
	}
	stagedBody, stagedDigest, err := installer.validateStaged(ctx, temporaryRoot, staged)
	if err != nil {
		return err
	}
	candidate := installer.Installation
	candidate.BinaryDigest = stagedDigest
	if previous != nil && !adopted && previous.Version == candidate.Version && previous.BinaryDigest == candidate.BinaryDigest {
		if err := installer.Lifecycle.Status(ctx); err != nil {
			if installErr := installer.Lifecycle.Install(ctx, candidate.Owner); installErr != nil {
				return errors.Join(fmt.Errorf("validate installed CQ services: %w", err), fmt.Errorf("repair installed CQ services: %w", installErr))
			}
			if statusErr := installer.Lifecycle.Status(ctx); statusErr != nil {
				return fmt.Errorf("validate repaired CQ services: %w", statusErr)
			}
		}
		if err := installer.Metadata.Inspect(ctx, *previous); err != nil {
			return fmt.Errorf("validate installed CQ metadata: %w", err)
		}
		return nil
	}

	if err := installer.prepareReplacement(previous, stagedBody, stagedDigest); err != nil {
		return err
	}
	serviceSnapshotPath := filepath.Join(temporaryRoot, "services.json")
	serviceSnapshotPrepared := false
	mutated := false
	candidateServicesInstalled := false
	fail := func(cause error) error {
		if !mutated {
			return errors.Join(cause, installer.cleanupPrepared(previous != nil))
		}
		rollbackErr := installer.rollback(ctx, previous, previousRecord, candidate, adopted, candidateServicesInstalled, serviceSnapshotPath, serviceSnapshotPrepared)
		if rollbackErr != nil {
			evidence := installer.Installation.Executable
			if previous != nil {
				evidence = installer.rollbackPath()
			}
			return errors.Join(cause, fmt.Errorf("%w; recovery evidence: %s: %v", ErrRollbackUnverified, evidence, rollbackErr))
		}
		return cause
	}

	if previous != nil && !adopted {
		if err := installer.Lifecycle.Snapshot(ctx, previous.Owner, serviceSnapshotPath); err != nil {
			return fail(fmt.Errorf("snapshot existing CQ services: %w", err))
		}
		serviceSnapshotPrepared = true
		mutated = true
		if err := installer.Lifecycle.Stop(ctx); err != nil {
			return fail(fmt.Errorf("stop existing CQ services: %w", err))
		}
	}
	mutated = true
	if err := installer.FS.Rename(installer.candidatePath(), installer.Installation.Executable); err != nil {
		return fail(fmt.Errorf("replace CQ executable: %w", err))
	}
	if _, _, installedDigest, err := installer.readBinary(installer.Installation.Executable); err != nil {
		return fail(fmt.Errorf("validate replaced CQ executable: %w", err))
	} else if installedDigest != candidate.BinaryDigest {
		return fail(fmt.Errorf("validate replaced CQ executable: digest differs"))
	}
	if err := installer.syncExecutableDirectory(); err != nil {
		return fail(err)
	}
	if err := installer.Lifecycle.Install(ctx, candidate.Owner); err != nil {
		return fail(fmt.Errorf("install CQ services: %w", err))
	}
	candidateServicesInstalled = true
	if err := installer.Lifecycle.Status(ctx); err != nil {
		return fail(fmt.Errorf("validate installed CQ services: %w", err))
	}
	if err := installer.Metadata.Install(ctx, candidate); err != nil {
		return fail(fmt.Errorf("install CQ platform metadata: %w", err))
	}
	if err := installer.Metadata.Inspect(ctx, candidate); err != nil {
		return fail(fmt.Errorf("validate CQ platform metadata: %w", err))
	}
	if err := installer.State.Save(candidate.record()); err != nil {
		return fail(fmt.Errorf("save CQ installation ownership: %w", err))
	}
	if err := installer.syncExecutableDirectory(); err != nil {
		return fail(err)
	}
	if err := installer.removeIfPresent(installer.rollbackPath()); err != nil {
		return fmt.Errorf("remove CQ rollback executable: %w", err)
	}
	return installer.syncExecutableDirectory()
}

// Uninstall removes only an exact binary owned by the requested installer.
func (installer *Installer) Uninstall(ctx context.Context) (resultErr error) {
	if err := installer.validate(); err != nil {
		return err
	}
	lock, err := installer.Locker.Acquire()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	ctx = ContextWithInstallLock(ctx, lock)

	current, record, _, err := installer.currentInstallation(false)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	body, mode, digest, err := installer.readBinary(current.Executable)
	if err != nil || digest != current.BinaryDigest {
		return fmt.Errorf("%w: installed executable digest differs", ErrForeignBinary)
	}
	temporaryRoot, err := installer.Temporary.Create()
	if err != nil {
		return fmt.Errorf("create installer temporary root: %w", err)
	}
	defer func() {
		if cleanupErr := installer.Temporary.Remove(temporaryRoot); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove installer temporary root %s: %w", temporaryRoot, cleanupErr))
		}
	}()
	serviceSnapshotPath := filepath.Join(temporaryRoot, "services.json")
	if err := installer.Lifecycle.Snapshot(ctx, current.Owner, serviceSnapshotPath); err != nil {
		return fmt.Errorf("snapshot installed CQ services: %w", err)
	}
	restore := func(cause error, restoreMetadata bool) error {
		var rollbackErr error
		if _, statErr := installer.FS.Stat(current.Executable); errors.Is(statErr, os.ErrNotExist) {
			rollbackErr = errors.Join(rollbackErr, installer.writeExclusive(current.Executable, body, mode))
		}
		if restoreMetadata {
			rollbackErr = errors.Join(rollbackErr, installer.Metadata.Install(ctx, *current))
		}
		rollbackErr = errors.Join(rollbackErr, installer.Lifecycle.Restore(ctx, current.Owner, serviceSnapshotPath))
		rollbackErr = errors.Join(rollbackErr, installer.State.Save(record))
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("%w: %v", ErrRollbackUnverified, rollbackErr))
		}
		return cause
	}
	if err := installer.Lifecycle.Uninstall(ctx, current.Owner); err != nil {
		return restore(fmt.Errorf("uninstall CQ services: %w", err), false)
	}
	if err := installer.Metadata.Remove(ctx, *current); err != nil {
		return restore(fmt.Errorf("remove CQ platform metadata: %w", err), true)
	}
	if err := installer.FS.Remove(current.Executable); err != nil {
		return restore(fmt.Errorf("remove CQ executable: %w", err), true)
	}
	if err := installer.syncExecutableDirectory(); err != nil {
		return restore(err, true)
	}
	if err := installer.State.Remove(); err != nil {
		return restore(fmt.Errorf("remove CQ installation ownership: %w", err), true)
	}
	return nil
}

func (installer *Installer) currentInstallation(allowAdoption bool) (*Installation, installstate.Record, bool, error) {
	record, err := installer.State.Load()
	if errors.Is(err, installstate.ErrNotInstalled) {
		if _, statErr := installer.FS.Stat(installer.Installation.Executable); statErr == nil {
			if !allowAdoption || installer.Installation.Owner != installstate.OwnerGo || installer.Classifier == nil {
				return nil, installstate.Record{}, false, fmt.Errorf("%w: destination exists without CQ ownership", ErrForeignBinary)
			}
			classification, classifyErr := installer.Classifier.Classify(installer.Installation.Owner, installer.Installation.Executable)
			if classifyErr != nil {
				return nil, installstate.Record{}, false, classifyErr
			}
			if classification != BinaryAdoptable {
				return nil, installstate.Record{}, false, fmt.Errorf("%w: destination exists without CQ ownership", ErrForeignBinary)
			}
			_, _, digest, readErr := installer.readBinary(installer.Installation.Executable)
			if readErr != nil {
				return nil, installstate.Record{}, false, readErr
			}
			adopted := Installation{
				Owner:        installstate.OwnerGo,
				Version:      "adopted",
				Executable:   installer.Installation.Executable,
				BinaryDigest: digest,
				Services:     append([]string(nil), installer.Installation.Services...),
			}
			return &adopted, adopted.record(), true, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, installstate.Record{}, false, fmt.Errorf("inspect CQ destination: %w", statErr)
		}
		return nil, installstate.Record{}, false, nil
	}
	if err != nil {
		return nil, installstate.Record{}, false, err
	}
	if err := installer.State.CheckClaim(installer.Installation.Owner, installer.Installation.Executable); err != nil {
		return nil, installstate.Record{}, false, err
	}
	_, _, digest, err := installer.readBinary(record.Executable)
	if err != nil || digest != record.BinaryDigest {
		return nil, installstate.Record{}, false, fmt.Errorf("%w: owned executable digest differs", ErrForeignBinary)
	}
	return &Installation{
		Owner:        record.Owner,
		Version:      record.Version,
		Executable:   record.Executable,
		BinaryDigest: record.BinaryDigest,
		Services:     append([]string(nil), record.Services...),
	}, record, false, nil
}

func (installer *Installer) validateStaged(ctx context.Context, temporaryRoot string, staged StagedBinary) ([]byte, string, error) {
	if staged.Path == "" || !filepath.IsAbs(staged.Path) || filepath.Clean(staged.Path) != staged.Path || filepath.Dir(staged.Path) != temporaryRoot || filepath.Base(staged.Path) != filepath.Base(installer.Installation.Executable) {
		return nil, "", fmt.Errorf("staged CQ executable escaped temporary root")
	}
	body, _, digest, err := installer.readBinary(staged.Path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect staged CQ executable: %w", err)
	}
	if staged.Digest != digest {
		return nil, "", fmt.Errorf("staged CQ executable digest mismatch")
	}
	version, err := installer.Runner.Version(ctx, staged.Path)
	if err != nil {
		return nil, "", fmt.Errorf("execute staged CQ version: %w", err)
	}
	if version != installer.Installation.Version {
		return nil, "", fmt.Errorf("%w: got %q, want %q", ErrStagedVersionMismatch, version, installer.Installation.Version)
	}
	return body, digest, nil
}

func (installer *Installer) prepareReplacement(previous *Installation, stagedBody []byte, stagedDigest string) error {
	if err := installer.ensureAbsent(installer.candidatePath()); err != nil {
		return err
	}
	if err := installer.ensureAbsent(installer.rollbackPath()); err != nil {
		return err
	}
	if previous != nil {
		rollbackBody, rollbackMode, _, err := installer.readBinary(previous.Executable)
		if err != nil {
			return err
		}
		if err := installer.writeExclusive(installer.rollbackPath(), rollbackBody, rollbackMode); err != nil {
			return fmt.Errorf("write CQ rollback executable: %w", err)
		}
	}
	if err := installer.writeExclusive(installer.candidatePath(), stagedBody, 0o700); err != nil {
		_ = installer.removeIfPresent(installer.rollbackPath())
		return errors.Join(fmt.Errorf("write CQ replacement executable: %w", err), installer.syncExecutableDirectory())
	}
	if _, _, candidateDigest, err := installer.readBinary(installer.candidatePath()); err != nil {
		_ = installer.cleanupPrepared(previous != nil)
		return fmt.Errorf("validate CQ replacement executable: %w", err)
	} else if candidateDigest != stagedDigest {
		_ = installer.cleanupPrepared(previous != nil)
		return fmt.Errorf("validate CQ replacement executable: digest differs")
	}
	if err := installer.syncExecutableDirectory(); err != nil {
		_ = installer.cleanupPrepared(previous != nil)
		return err
	}
	return nil
}

func (installer *Installer) rollback(ctx context.Context, previous *Installation, previousRecord installstate.Record, candidate Installation, adopted, candidateServicesInstalled bool, serviceSnapshotPath string, serviceSnapshotPrepared bool) error {
	var result error
	if candidateServicesInstalled {
		result = errors.Join(result, installer.Lifecycle.Uninstall(ctx, candidate.Owner))
	}
	result = errors.Join(result, installer.Metadata.Remove(ctx, candidate))
	result = errors.Join(result, installer.removeIfPresent(installer.Installation.Executable))
	result = errors.Join(result, installer.removeIfPresent(installer.candidatePath()))
	if previous == nil {
		result = errors.Join(result, installer.State.Remove())
		result = errors.Join(result, installer.syncExecutableDirectory())
		if _, err := installer.FS.Stat(installer.Installation.Executable); err == nil || !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("candidate CQ executable remains"))
		}
		return result
	}
	rollbackBody, rollbackMode, rollbackDigest, err := installer.readBinary(installer.rollbackPath())
	if err != nil {
		return errors.Join(result, fmt.Errorf("read rollback executable: %w", err))
	}
	if rollbackDigest != previous.BinaryDigest {
		return errors.Join(result, fmt.Errorf("rollback executable digest differs"))
	}
	if err := installer.writeExclusive(installer.candidatePath(), rollbackBody, rollbackMode); err != nil {
		return errors.Join(result, err)
	}
	if err := installer.FS.Rename(installer.candidatePath(), previous.Executable); err != nil {
		return errors.Join(result, err)
	}
	result = errors.Join(result, installer.syncExecutableDirectory())
	if adopted {
		result = errors.Join(result, installer.State.Remove())
		_, _, restoredDigest, restoreErr := installer.readBinary(previous.Executable)
		if restoreErr != nil || restoredDigest != previous.BinaryDigest {
			result = errors.Join(result, fmt.Errorf("restored adopted CQ executable could not be verified: %w", restoreErr))
		}
		if result != nil {
			return result
		}
		if err := installer.removeIfPresent(installer.rollbackPath()); err != nil {
			return err
		}
		return installer.syncExecutableDirectory()
	}
	if !serviceSnapshotPrepared {
		result = errors.Join(result, fmt.Errorf("previous service snapshot is unavailable"))
	} else {
		result = errors.Join(result, installer.Lifecycle.Restore(ctx, previous.Owner, serviceSnapshotPath))
	}
	result = errors.Join(result, installer.Metadata.Install(ctx, *previous))
	result = errors.Join(result, installer.Metadata.Inspect(ctx, *previous))
	result = errors.Join(result, installer.State.Save(previousRecord))
	_, _, restoredDigest, err := installer.readBinary(previous.Executable)
	if err != nil || restoredDigest != previous.BinaryDigest {
		result = errors.Join(result, fmt.Errorf("restored CQ executable could not be verified: %w", err))
	}
	if result != nil {
		return result
	}
	if err := installer.removeIfPresent(installer.rollbackPath()); err != nil {
		return err
	}
	return installer.syncExecutableDirectory()
}

func (installer *Installer) cleanupPrepared(hasRollback bool) error {
	var result error
	result = errors.Join(result, installer.removeIfPresent(installer.candidatePath()))
	if hasRollback {
		result = errors.Join(result, installer.removeIfPresent(installer.rollbackPath()))
	}
	return errors.Join(result, installer.syncExecutableDirectory())
}

func (installer *Installer) readBinary(path string) ([]byte, os.FileMode, string, error) {
	file, err := installer.FS.OpenNoFollow(path)
	if err != nil {
		return nil, 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxInstallerBinaryBytes {
		return nil, 0, "", fmt.Errorf("CQ executable is not a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxInstallerBinaryBytes+1))
	if err != nil {
		return nil, 0, "", err
	}
	if int64(len(body)) != info.Size() || int64(len(body)) > maxInstallerBinaryBytes {
		return nil, 0, "", fmt.Errorf("CQ executable changed while reading")
	}
	return body, info.Mode().Perm(), digestBytes(body), nil
}

func (installer *Installer) writeExclusive(path string, body []byte, mode os.FileMode) (resultErr error) {
	directory, err := fsutil.OpenOwnerControlledDirectory(installer.FS, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	file, err := directory.CreateExclusive(filepath.Base(path), mode.Perm())
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, installer.removeIfPresent(path))
		}
	}()
	written, err := file.Write(body)
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := installer.FS.Chmod(path, mode.Perm()); err != nil {
		return err
	}
	return nil
}

func (installer *Installer) ensureAbsent(path string) error {
	if _, err := installer.FS.Stat(path); err == nil {
		return fmt.Errorf("installer recovery path already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (installer *Installer) removeIfPresent(path string) error {
	if err := installer.FS.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (installer *Installer) syncExecutableDirectory() error {
	if err := installer.FS.SyncDir(filepath.Dir(installer.Installation.Executable)); err != nil {
		return fmt.Errorf("sync CQ executable directory: %w", err)
	}
	return nil
}

func (installer *Installer) ensureExecutableDirectory() (bool, error) {
	directory := filepath.Dir(installer.Installation.Executable)
	info, err := installer.FS.Stat(directory)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("CQ executable parent is not a directory")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect CQ executable directory: %w", err)
	}
	if err := installer.FS.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("create CQ executable directory: %w", err)
	}
	return true, nil
}

func (installer *Installer) candidatePath() string {
	return installer.Installation.Executable + ".candidate"
}

func (installer *Installer) rollbackPath() string {
	return installer.Installation.Executable + ".rollback"
}

func (installer *Installer) validate() error {
	if installer == nil || installer.FS == nil || installer.Downloader == nil || installer.Runner == nil || installer.Lifecycle == nil || installer.Metadata == nil || installer.State == nil || installer.Locker == nil || installer.Temporary == nil {
		return fmt.Errorf("invalid CQ installer dependencies")
	}
	installation := installer.Installation
	claim := installation
	claim.BinaryDigest = strings.Repeat("0", sha256.Size*2)
	if releaseVersionPattern.FindStringSubmatch(installation.Version) == nil || strings.HasPrefix(installation.Version, "v") || claim.record().Validate() != nil {
		return fmt.Errorf("invalid CQ installation target")
	}
	return nil
}

func (installation Installation) record() installstate.Record {
	return installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         installation.Owner,
		Version:       installation.Version,
		Executable:    installation.Executable,
		BinaryDigest:  installation.BinaryDigest,
		Services:      append([]string(nil), installation.Services...),
	}
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// OSTemporaryDirectories creates installer staging under one validated root.
type OSTemporaryDirectories struct {
	Root string
}

func (temporary OSTemporaryDirectories) Create() (directory string, resultErr error) {
	if temporary.Root == "" || !filepath.IsAbs(temporary.Root) || filepath.Clean(temporary.Root) != temporary.Root {
		return "", fmt.Errorf("invalid installer temporary root")
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.EnsureSecureDirectory(fsys, temporary.Root); err != nil {
		return "", err
	}
	parent, err := fsys.OpenDurableDirectory(temporary.Root)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			if directory != "" {
				cleanupErr := os.Remove(directory)
				directory = ""
				resultErr = errors.Join(resultErr, closeErr, cleanupErr)
				return
			}
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	for range 128 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := "cq-install-" + hex.EncodeToString(random[:])
		if err := parent.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		directory = filepath.Join(temporary.Root, name)
		if err := parent.Sync(); err != nil {
			return "", errors.Join(err, os.Remove(directory))
		}
		if err := fsutil.ValidateSecureDirectory(fsys, directory); err != nil {
			return "", errors.Join(err, os.Remove(directory))
		}
		return directory, nil
	}
	return "", fmt.Errorf("create unique installer temporary root")
}

func (temporary OSTemporaryDirectories) Remove(directory string) error {
	relative, err := filepath.Rel(temporary.Root, directory)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) || !strings.HasPrefix(filepath.Base(directory), "cq-install-") {
		return fmt.Errorf("refused installer temporary cleanup %q", directory)
	}
	return os.RemoveAll(directory)
}
