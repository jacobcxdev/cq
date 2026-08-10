package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type codexReadinessMarkerDurableOps struct {
	fileSystem    fsutil.FileSystem
	wrapDirectory func(fsutil.SecureDirectory) fsutil.SecureDirectory
}

var codexReadinessMarkerCommitPoison = []byte("cq-codex-readiness-commit-in-progress-v1\n")

func defaultCodexReadinessMarkerDurableOps() codexReadinessMarkerDurableOps {
	return codexReadinessMarkerDurableOps{
		fileSystem: fsutil.OSFileSystem{},
		wrapDirectory: func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
			return directory
		},
	}
}

// saveCodexHTTPReadinessMarkerDurably persists the HTTP readiness marker
// synchronously. Callers therefore retain their serving and process leases
// until this function returns.
func saveCodexHTTPReadinessMarkerDurably(dir string, marker CodexReadinessMarker) error {
	return saveCodexHTTPReadinessMarkerDurablyWithOps(dir, marker, defaultCodexReadinessMarkerDurableOps())
}

// invalidateCodexHTTPReadinessMarkerDurably removes any prior HTTP marker
// before a one-shot installed validation attempt can fail. The marker remains
// absent unless the held listener/process authority later commits a new one.
func invalidateCodexHTTPReadinessMarkerDurably(dir string) error {
	return invalidateCodexHTTPReadinessMarkerDurablyWithOps(dir, defaultCodexReadinessMarkerDurableOps())
}

func invalidateCodexHTTPReadinessMarkerDurablyWithOps(dir string, ops codexReadinessMarkerDurableOps) error {
	if ops.fileSystem == nil {
		return fmt.Errorf("durable readiness marker filesystem is unavailable")
	}
	inspector, ok := ops.fileSystem.(fsutil.SecurePathInspector)
	if !ok {
		return fmt.Errorf("durable readiness marker filesystem: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	opener, ok := ops.fileSystem.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fmt.Errorf("durable readiness marker filesystem: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	if err := fsutil.EnsureSecureDirectory(ops.fileSystem, dir); err != nil {
		return fmt.Errorf("secure readiness marker directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return fmt.Errorf("open readiness marker directory: %w", err)
	}
	if ops.wrapDirectory != nil {
		directory = ops.wrapDirectory(directory)
	}
	if directory == nil {
		return fmt.Errorf("open readiness marker directory: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	defer directory.Close()
	if err := validateCodexReadinessMarkerDirectoryAuthority(inspector, directory, dir); err != nil {
		return err
	}
	name := filepath.Base(codexReadinessPath(dir, CodexRoutingHTTP))
	poisonName := codexReadinessPoisonName(CodexRoutingHTTP)
	if err := prepareCodexReadinessMarkerCommit(inspector, directory, name, poisonName, func() error {
		return validateCodexReadinessMarkerDirectoryAuthority(inspector, directory, dir)
	}); err != nil {
		return err
	}
	return validateCodexReadinessMarkerDirectoryAuthority(inspector, directory, dir)
}

func saveCodexHTTPReadinessMarkerDurablyWithOps(
	dir string,
	marker CodexReadinessMarker,
	ops codexReadinessMarkerDurableOps,
) error {
	if marker.Transport != CodexRoutingHTTP {
		return fmt.Errorf("durable readiness marker requires HTTP transport")
	}
	if marker.Version != CodexReadinessMarkerVersion || marker.CQBuild == "" || marker.ParserSchema <= 0 || marker.LeaseSchema <= 0 ||
		marker.SemanticsRevision == "" || marker.ClientBuild == "" || marker.RetryBudget < 0 || marker.FixtureHash == "" ||
		marker.InstalledResult != "passed" || len(marker.CompletedGates) == 0 || marker.ValidatedAt.IsZero() {
		return fmt.Errorf("readiness marker is incomplete")
	}
	if err := validateCodexReadinessMarkerArtifactBinding(marker); err != nil {
		return err
	}
	data, err := canonicalCodexReadinessMarkerJSON(marker)
	if err != nil {
		return err
	}
	if ops.fileSystem == nil {
		return fmt.Errorf("durable readiness marker filesystem is unavailable")
	}
	inspector, ok := ops.fileSystem.(fsutil.SecurePathInspector)
	if !ok {
		return fmt.Errorf("durable readiness marker filesystem: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	opener, ok := ops.fileSystem.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fmt.Errorf("durable readiness marker filesystem: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	if err := fsutil.EnsureSecureDirectory(ops.fileSystem, dir); err != nil {
		return fmt.Errorf("secure readiness marker directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return fmt.Errorf("open readiness marker directory: %w", err)
	}
	if ops.wrapDirectory != nil {
		directory = ops.wrapDirectory(directory)
	}
	if directory == nil {
		return fmt.Errorf("open readiness marker directory: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	defer directory.Close()

	fence := func() error {
		return validateCodexReadinessMarkerDirectoryAuthority(inspector, directory, dir)
	}
	if err := fence(); err != nil {
		return err
	}
	name := filepath.Base(codexReadinessPath(dir, CodexRoutingHTTP))
	poisonName := codexReadinessPoisonName(CodexRoutingHTTP)
	if err := prepareCodexReadinessMarkerCommit(inspector, directory, name, poisonName, fence); err != nil {
		return err
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(inspector, directory, name, data, fence); err != nil {
		return errors.Join(err, removeCodexReadinessMarkerInDirectory(directory, name))
	}
	if err := fence(); err != nil {
		return errors.Join(err, removeCodexReadinessMarkerInDirectory(directory, name))
	}
	return finishCodexReadinessMarkerCommit(directory, poisonName, fence)
}

func prepareCodexReadinessMarkerCommit(
	inspector fsutil.SecurePathInspector,
	directory fsutil.SecureDirectory,
	markerName, poisonName string,
	fence func() error,
) error {
	if err := invalidateCodexReadinessMarkerInDirectory(inspector, directory, markerName); err != nil {
		return err
	}
	if err := fence(); err != nil {
		return err
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(inspector, directory, poisonName, codexReadinessMarkerCommitPoison, fence); err != nil {
		return err
	}
	return fence()
}

// The successful poison unlink is the commit point. A following directory
// sync failure is safe to report as committed: this process sees the durable
// marker without poison, while a restart can only recover that state or the
// earlier poison-and-marker state, which the reader rejects.
func finishCodexReadinessMarkerCommit(directory fsutil.SecureDirectory, poisonName string, fence func() error) error {
	removeErr := directory.Remove(poisonName)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove readiness marker commit poison: %w", removeErr)
	}
	syncErr := directory.Sync()
	if err := fence(); err != nil {
		return errors.Join(err, syncErr)
	}
	if err := requireCodexReadinessPoisonAbsent(directory, poisonName); err != nil {
		return errors.Join(err, syncErr)
	}
	return nil
}

func requireCodexReadinessPoisonAbsent(directory fsutil.SecureDirectory, poisonName string) error {
	file, err := directory.OpenNoFollow(poisonName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect readiness marker commit poison: %w", err)
	}
	return errors.Join(errors.New("readiness marker commit is incomplete"), file.Close())
}

func validateCodexReadinessMarkerDirectoryAuthority(
	inspector fsutil.SecurePathInspector,
	directory fsutil.SecureDirectory,
	path string,
) error {
	heldInfo, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("stat held readiness marker directory: %w", err)
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat readiness marker directory: %w", err)
	}
	if !heldInfo.IsDir() || !pathInfo.IsDir() || heldInfo.Mode().Perm() != 0o700 || pathInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("readiness marker directory: %w", fsutil.ErrUnsafeSecurePath)
	}
	heldOwner, heldOwnerOK := inspector.FileOwnerUID(heldInfo)
	pathOwner, pathOwnerOK := inspector.FileOwnerUID(pathInfo)
	if !heldOwnerOK || !pathOwnerOK || heldOwner != inspector.EffectiveUID() || pathOwner != inspector.EffectiveUID() {
		return fmt.Errorf("readiness marker directory owner: %w", fsutil.ErrUnsafeSecurePath)
	}
	heldIdentity, heldIdentityOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathIdentityOK := inspector.FileIdentity(pathInfo)
	if !heldIdentityOK || !pathIdentityOK || heldIdentity != pathIdentity {
		return fmt.Errorf("readiness marker directory identity: %w", fsutil.ErrUnsafeSecurePath)
	}
	return nil
}

func invalidateCodexReadinessMarkerInDirectory(
	inspector fsutil.SecurePathInspector,
	directory fsutil.SecureDirectory,
	name string,
) error {
	file, err := directory.OpenNoFollow(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(
			fmt.Errorf("open prior readiness marker without following links: %w", err),
			removeCodexReadinessMarkerInDirectory(directory, name),
		)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	var validationErr error
	if statErr != nil {
		validationErr = fmt.Errorf("stat prior readiness marker: %w", statErr)
	} else {
		owner, ownerOK := inspector.FileOwnerUID(info)
		identity, identityOK := inspector.FileIdentity(info)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownerOK || owner != inspector.EffectiveUID() ||
			!identityOK || identity.Links != 1 {
			validationErr = fmt.Errorf("prior readiness marker: %w", fsutil.ErrUnsafeSecurePath)
		}
	}
	if closeErr != nil {
		validationErr = errors.Join(validationErr, closeErr)
	}
	removeErr := removeCodexReadinessMarkerInDirectory(directory, name)
	if validationErr != nil {
		return errors.Join(validationErr, removeErr)
	}
	return removeErr
}

func removeCodexReadinessMarkerInDirectory(directory fsutil.SecureDirectory, name string) error {
	removeErr := directory.Remove(name)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	syncErr := directory.Sync()
	return errors.Join(removeErr, syncErr)
}
