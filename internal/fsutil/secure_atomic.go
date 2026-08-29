package fsutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrSecureCapabilityUnavailable = errors.New("secure filesystem capability unavailable")
	ErrUnsafeSecurePath            = errors.New("unsafe secure filesystem path")
	ErrExclusiveLockHeld           = errors.New("exclusive filesystem lock held")
	ErrExclusiveLockNotHeld        = errors.New("exclusive filesystem lock is not held")
	ErrSecureFileTooLarge          = errors.New("secure file exceeds read limit")
	ErrCommitNotCommitted          = errors.New("durable write not committed")
	ErrCommitIndeterminate         = errors.New("durable write commit indeterminate")
)

type CommitOutcome uint8

const (
	CommitNotCommitted CommitOutcome = iota
	CommitCommitted
	CommitIndeterminate
)

func (outcome CommitOutcome) String() string {
	switch outcome {
	case CommitNotCommitted:
		return "not_committed"
	case CommitCommitted:
		return "committed"
	case CommitIndeterminate:
		return "indeterminate"
	default:
		return "unknown"
	}
}

type CommitError struct {
	Outcome CommitOutcome
	Op      string
	Err     error
}

func (err *CommitError) Error() string {
	return fmt.Sprintf("%s: %s: %v", err.Op, err.Outcome, err.Err)
}

func (err *CommitError) Unwrap() error { return err.Err }

func (err *CommitError) Is(target error) bool {
	switch target {
	case ErrCommitNotCommitted:
		return err.Outcome == CommitNotCommitted
	case ErrCommitIndeterminate:
		return err.Outcome == CommitIndeterminate
	default:
		return errors.Is(err.Err, target)
	}
}

func AtomicWriteOutcome(err error) CommitOutcome {
	if err == nil {
		return CommitCommitted
	}
	var commitErr *CommitError
	if errors.As(err, &commitErr) {
		return commitErr.Outcome
	}
	return CommitIndeterminate
}

func SecureAtomicWrite(fsys FileSystem, path string, data []byte) error {
	if fsys == nil || path == "" || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return commitFailure("validate destination", fmt.Errorf("invalid durable path"))
	}
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	opener, ok := fsys.(SecureDirectoryOpener)
	if !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := EnsureSecureDirectory(fsys, dir); err != nil {
		return commitFailure("secure durable state directory", err)
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return commitFailure("open durable state directory", err)
	}
	defer directory.Close()
	return secureAtomicWriteInDirectory(inspector, directory, base, data, nil, func() error {
		return validateSecureDirectoryHandle(inspector, directory, dir)
	}, false)
}

// SecureAtomicWriteInDirectory commits data relative to one retained directory
// handle. Callers that already established directory authority use this form so
// no operation can drift into a replacement pathname namespace.
func SecureAtomicWriteInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, data []byte) error {
	return secureAtomicWriteInDirectory(inspector, directory, name, data, nil, nil, false)
}

// SecureAtomicWriteInDirectoryChecked is SecureAtomicWriteInDirectory with a
// caller precondition that runs after the temporary file is durable and as the
// final operation before the canonical rename. A failed precondition leaves the
// previous destination intact and returns a not-committed outcome.
func SecureAtomicWriteInDirectoryChecked(inspector SecurePathInspector, directory SecureDirectory, name string, data []byte, beforeReplace func() error) error {
	return secureAtomicWriteInDirectory(inspector, directory, name, data, beforeReplace, nil, false)
}

// SecureAtomicCreateInDirectory publishes data under a previously absent name.
// The final rename is an atomic no-replace operation, so a concurrent creator
// wins without its bytes being overwritten.
func SecureAtomicCreateInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, data []byte) error {
	return SecureAtomicCreateInDirectoryChecked(inspector, directory, name, data, nil)
}

// SecureAtomicCreateInDirectoryChecked runs beforePublish after the temporary
// file is durable and as the final operation before the no-replace rename.
func SecureAtomicCreateInDirectoryChecked(inspector SecurePathInspector, directory SecureDirectory, name string, data []byte, beforePublish func() error) error {
	return secureAtomicWriteInDirectory(inspector, directory, name, data, beforePublish, nil, true)
}

func secureAtomicWriteInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, data []byte, beforeReplace, fence func() error, noReplace bool) (result error) {
	if inspector == nil || directory == nil {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	renamer, ok := directory.(IdentityBoundRenamer)
	if !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	remover, ok := directory.(IdentityBoundRemover)
	if !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	if err := validateSecureEntryName(name); err != nil {
		return commitFailure("validate destination", err)
	}
	validateDirectory := func() error {
		return validateSecureDirectoryDescriptor(inspector, directory)
	}
	if fence != nil {
		validateDirectory = fence
	}
	if err := validateDirectory(); err != nil {
		return commitFailure("validate durable state directory", err)
	}
	if err := validateSecureRegularFileInDirectoryIfPresent(inspector, directory, name); err != nil {
		return commitFailure("validate durable destination", err)
	}

	temporaryName, file, err := createUniqueTemporary(directory, name)
	if err != nil {
		return commitFailure("create durable temporary file", err)
	}
	temporaryInspector, ok := file.(DurableFileInspector)
	if !ok {
		_ = file.Close()
		return commitFailure("inspect durable temporary file", ErrSecureCapabilityUnavailable)
	}
	temporaryInfo, err := temporaryInspector.Stat()
	if err != nil {
		_ = file.Close()
		return commitFailure("inspect durable temporary file", err)
	}
	if err := validateSecureRegularInfo(inspector, temporaryInfo); err != nil {
		_ = file.Close()
		return commitFailure("validate durable temporary descriptor", err)
	}
	temporaryIdentity, ok := inspector.FileIdentity(temporaryInfo)
	if !ok {
		_ = file.Close()
		return commitFailure("validate durable temporary descriptor", fmt.Errorf("%w: temporary file identity", ErrUnsafeSecurePath))
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			if cleanupErr := remover.RemoveChecked(temporaryName, temporaryIdentity); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				if errors.Is(cleanupErr, ErrCommitIndeterminate) {
					result = commitIndeterminate("clean durable temporary file", errors.Join(result, cleanupErr))
				} else {
					result = commitFailure("clean durable temporary file", errors.Join(result, cleanupErr))
				}
			}
		}
	}()

	if err := writeFull(file, data); err != nil {
		_ = file.Close()
		return commitFailure("write durable temporary file", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return commitFailure("sync durable temporary file", err)
	}
	temporaryInfo, err = temporaryInspector.Stat()
	if err != nil {
		_ = file.Close()
		return commitFailure("inspect durable temporary file", err)
	}
	if err := validateSecureRegularInfo(inspector, temporaryInfo); err != nil {
		_ = file.Close()
		return commitFailure("validate durable temporary descriptor", err)
	}
	temporaryIdentityAfterSync, ok := inspector.FileIdentity(temporaryInfo)
	if !ok || !SameSecureObject(temporaryIdentityAfterSync, temporaryIdentity) {
		_ = file.Close()
		return commitFailure("validate durable temporary descriptor", fmt.Errorf("%w: temporary file identity", ErrUnsafeSecurePath))
	}
	if err := file.Close(); err != nil {
		return commitFailure("close durable temporary file", err)
	}
	if err := validateDirectory(); err != nil {
		return commitFailure("validate durable state directory before replace", err)
	}
	temporaryPathInfo, err := secureRegularFileInfoInDirectory(inspector, directory, temporaryName)
	if err != nil {
		return commitFailure("revalidate durable temporary path", err)
	}
	temporaryPathIdentity, ok := inspector.FileIdentity(temporaryPathInfo)
	if !ok || !SameSecureObject(temporaryPathIdentity, temporaryIdentity) {
		return commitFailure("revalidate durable temporary path", fmt.Errorf("%w: temporary file path identity", ErrUnsafeSecurePath))
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return commitFailure("validate durable write precondition", err)
		}
	}
	if noReplace {
		err = renamer.RenameNoReplaceChecked(temporaryName, name, temporaryIdentity)
	} else {
		err = renamer.RenameChecked(temporaryName, name, temporaryIdentity)
	}
	if err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			return commitIndeterminate("publish durable file", err)
		}
		return commitFailure("publish durable file", err)
	}
	removeTemporary = false
	installedInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return commitIndeterminate("validate installed durable file", err)
	}
	installedIdentity, ok := inspector.FileIdentity(installedInfo)
	if !ok || !SameSecureObject(installedIdentity, temporaryIdentity) {
		return commitIndeterminate("validate installed durable file", fmt.Errorf("%w: installed file identity", ErrUnsafeSecurePath))
	}
	if err := validateDirectory(); err != nil {
		return commitIndeterminate("validate durable state directory after replace", err)
	}
	if err := directory.Sync(); err != nil {
		return commitIndeterminate("sync durable state directory", err)
	}
	installedAfterSyncInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return commitIndeterminate("revalidate installed durable file after sync", err)
	}
	installedAfterSyncIdentity, ok := inspector.FileIdentity(installedAfterSyncInfo)
	if !ok || !SameSecureObject(installedAfterSyncIdentity, temporaryIdentity) {
		return commitIndeterminate("revalidate installed durable file after sync", fmt.Errorf("%w: installed file identity", ErrUnsafeSecurePath))
	}
	if err := validateDirectory(); err != nil {
		return commitIndeterminate("validate durable state directory after sync", err)
	}
	return nil
}

// SecurePromoteNoReplaceInDirectory atomically promotes one already durable
// private file to a previously absent canonical name. The source content and
// identity must match the caller's retained proof before publication and after
// the directory entry is durable.
func SecurePromoteNoReplaceInDirectory(inspector SecurePathInspector, directory SecureDirectory, sourceName, destinationName string, expected []byte, expectedIdentity SecureFileIdentity) error {
	return SecurePromoteNoReplaceInDirectoryChecked(inspector, directory, sourceName, destinationName, expected, expectedIdentity, nil)
}

// SecurePromoteNoReplaceInDirectoryChecked runs beforePromote after the final
// source proof and immediately before the no-replace rename.
func SecurePromoteNoReplaceInDirectoryChecked(inspector SecurePathInspector, directory SecureDirectory, sourceName, destinationName string, expected []byte, expectedIdentity SecureFileIdentity, beforePromote func() error) error {
	if inspector == nil || directory == nil {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	renamer, ok := directory.(IdentityBoundRenamer)
	if !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	if _, ok := directory.(IdentityBoundRemover); !ok {
		return commitFailure("validate filesystem", ErrSecureCapabilityUnavailable)
	}
	if err := validateSecureEntryName(sourceName); err != nil {
		return commitFailure("validate source", err)
	}
	if err := validateSecureEntryName(destinationName); err != nil {
		return commitFailure("validate destination", err)
	}
	if sourceName == destinationName {
		return commitFailure("validate promotion", fmt.Errorf("%w: identical source and destination", ErrUnsafeSecurePath))
	}
	validateDirectory := func() error {
		return validateSecureDirectoryDescriptor(inspector, directory)
	}
	if err := validateDirectory(); err != nil {
		return commitFailure("validate durable state directory", err)
	}
	if err := validateSecureRegularFileInDirectoryIfPresent(inspector, directory, destinationName); err != nil {
		return commitFailure("validate durable destination", err)
	}
	validateSource := func() error {
		data, identity, err := ReadSecureFileInDirectoryWithIdentity(inspector, directory, sourceName, secureExpectedReadLimit(expected))
		if err != nil {
			return err
		}
		if identity != expectedIdentity || !bytes.Equal(data, expected) {
			return fmt.Errorf("%w: promotion source proof", ErrUnsafeSecurePath)
		}
		return nil
	}
	if err := validateSource(); err != nil {
		return commitFailure("validate promotion source", err)
	}
	if err := validateDirectory(); err != nil {
		return commitFailure("validate durable state directory before promotion", err)
	}
	if err := validateSource(); err != nil {
		return commitFailure("revalidate promotion source", err)
	}
	if beforePromote != nil {
		if err := beforePromote(); err != nil {
			return commitFailure("validate promotion precondition", err)
		}
	}
	if err := renamer.RenameNoReplaceChecked(sourceName, destinationName, expectedIdentity); err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			return commitIndeterminate("promote durable file", err)
		}
		return commitFailure("promote durable file", err)
	}
	validateInstalled := func() error {
		data, identity, err := ReadSecureFileInDirectoryWithIdentity(inspector, directory, destinationName, secureExpectedReadLimit(expected))
		if err != nil {
			return err
		}
		if identity != expectedIdentity || !bytes.Equal(data, expected) {
			return fmt.Errorf("%w: installed promotion proof", ErrUnsafeSecurePath)
		}
		return nil
	}
	if err := validateInstalled(); err != nil {
		return commitIndeterminate("validate promoted durable file", err)
	}
	if err := validateDirectory(); err != nil {
		return commitIndeterminate("validate durable state directory after promotion", err)
	}
	if err := directory.Sync(); err != nil {
		return commitIndeterminate("sync durable state directory", err)
	}
	if err := validateInstalled(); err != nil {
		return commitIndeterminate("revalidate promoted durable file after sync", err)
	}
	if err := validateDirectory(); err != nil {
		return commitIndeterminate("validate durable state directory after sync", err)
	}
	return nil
}

func secureExpectedReadLimit(expected []byte) int64 {
	if len(expected) >= int(^uint(0)>>1) {
		return int64(len(expected))
	}
	return int64(len(expected)) + 1
}

func EnsureSecureDirectory(fsys FileSystem, path string) error {
	if fsys == nil || path == "" {
		return fmt.Errorf("%w: invalid directory", ErrUnsafeSecurePath)
	}
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(DurableDirectoryOpener)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	clean := filepath.Clean(path)
	if err := ValidateSecureDirectory(fsys, clean); err == nil {
		if err := sealExistingDirectoryEntry(inspector, opener, clean, true); err != nil {
			return fmt.Errorf("seal existing secure directory: %w", err)
		}
		return ValidateSecureDirectory(fsys, clean)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	missing, existing, err := missingDirectoryChain(inspector, clean)
	if err != nil {
		return err
	}
	var parent DurableDirectory
	if boundaryOpener, ok := fsys.(interface {
		openSecureDirectoryAncestor(string) (DurableDirectory, error)
	}); ok {
		parent, err = boundaryOpener.openSecureDirectoryAncestor(existing)
		if err != nil {
			return fmt.Errorf("open secure directory boundary ancestor: %w", err)
		}
		if err := validateRetainedDirectoryPath(inspector, parent, existing, false); err != nil {
			_ = parent.Close()
			return fmt.Errorf("validate secure directory boundary ancestor: %w", err)
		}
	} else if ancestorParentPath := filepath.Dir(existing); ancestorParentPath != existing {
		ancestorParent, err := opener.OpenDurableDirectory(ancestorParentPath)
		if err != nil {
			return fmt.Errorf("open existing directory ancestor parent: %w", err)
		}
		if err := validateRetainedDirectoryPath(inspector, ancestorParent, ancestorParentPath, false); err != nil {
			_ = ancestorParent.Close()
			return fmt.Errorf("validate existing directory ancestor parent: %w", err)
		}
		parent, err = ancestorParent.OpenDirectory(filepath.Base(existing))
		if err != nil {
			_ = ancestorParent.Close()
			return fmt.Errorf("open secure directory ancestor: %w", err)
		}
		if err := sealRetainedDirectory(inspector, parent, existing, false); err != nil {
			_ = parent.Close()
			_ = ancestorParent.Close()
			return fmt.Errorf("seal existing directory ancestor: %w", err)
		}
		if err := sealRetainedDirectory(inspector, ancestorParent, ancestorParentPath, false); err != nil {
			_ = parent.Close()
			_ = ancestorParent.Close()
			return fmt.Errorf("seal existing directory ancestor parent: %w", err)
		}
		if err := ancestorParent.Close(); err != nil {
			_ = parent.Close()
			return fmt.Errorf("close existing directory ancestor parent: %w", err)
		}
		if err := validateRetainedDirectoryPath(inspector, parent, existing, false); err != nil {
			_ = parent.Close()
			return fmt.Errorf("revalidate existing directory ancestor: %w", err)
		}
	} else {
		parent, err = opener.OpenDurableDirectory(existing)
		if err != nil {
			return fmt.Errorf("open secure directory ancestor: %w", err)
		}
		if err := validateRetainedDirectoryPath(inspector, parent, existing, false); err != nil {
			_ = parent.Close()
			return fmt.Errorf("validate secure directory ancestor: %w", err)
		}
	}
	defer func() { _ = parent.Close() }()

	for index := len(missing) - 1; index >= 0; index-- {
		created := missing[index]
		name := filepath.Base(created)
		if err := validateRetainedDirectoryPath(inspector, parent, filepath.Dir(created), false); err != nil {
			return fmt.Errorf("validate secure directory parent: %w", err)
		}
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create secure directory: %w", err)
		}
		child, err := parent.OpenDirectory(name)
		if err != nil {
			return fmt.Errorf("open created secure directory: %w", err)
		}
		if err := sealRetainedDirectory(inspector, child, created, true); err != nil {
			_ = child.Close()
			return fmt.Errorf("sync created secure directory: %w", err)
		}
		if err := sealRetainedDirectory(inspector, parent, filepath.Dir(created), false); err != nil {
			_ = child.Close()
			return fmt.Errorf("sync secure directory parent: %w", err)
		}
		if err := validateRetainedDirectoryPath(inspector, child, created, true); err != nil {
			_ = child.Close()
			return fmt.Errorf("revalidate created secure directory: %w", err)
		}
		if err := parent.Close(); err != nil {
			_ = child.Close()
			return fmt.Errorf("close secure directory parent: %w", err)
		}
		parent = child
	}
	return ValidateSecureDirectory(fsys, clean)
}

func missingDirectoryChain(inspector SecurePathInspector, path string) ([]string, string, error) {
	missing := make([]string, 0, 1)
	for current := path; ; current = filepath.Dir(current) {
		info, err := inspector.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, "", fmt.Errorf("%w: directory ancestor type", ErrUnsafeSecurePath)
			}
			return missing, current, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, "", fmt.Errorf("%w: no existing directory ancestor", ErrUnsafeSecurePath)
		}
	}
}

func sealExistingDirectoryEntry(inspector SecurePathInspector, opener DurableDirectoryOpener, path string, secure bool) error {
	parentPath := filepath.Dir(path)
	if parentPath == path {
		directory, err := opener.OpenDurableDirectory(path)
		if err != nil {
			return err
		}
		defer directory.Close()
		return sealRetainedDirectory(inspector, directory, path, secure)
	}
	parentPathInfo, err := inspector.Lstat(parentPath)
	if err != nil {
		return err
	}
	if parentPathInfo.Mode()&os.ModeSymlink != 0 || !parentPathInfo.IsDir() {
		return fmt.Errorf("%w: directory parent type", ErrUnsafeSecurePath)
	}
	parent, err := opener.OpenDurableDirectory(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := validateRetainedDirectoryPath(inspector, parent, parentPath, false); err != nil {
		return err
	}
	directory, err := parent.OpenDirectory(filepath.Base(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := sealRetainedDirectory(inspector, directory, path, secure); err != nil {
		return err
	}
	if err := sealRetainedDirectory(inspector, parent, parentPath, false); err != nil {
		return err
	}
	return validateRetainedDirectoryPath(inspector, directory, path, secure)
}

func sealRetainedDirectory(inspector SecurePathInspector, directory DurableDirectory, path string, secure bool) error {
	if err := validateRetainedDirectoryPath(inspector, directory, path, secure); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	return validateRetainedDirectoryPath(inspector, directory, path, secure)
}

func validateRetainedDirectoryPath(inspector SecurePathInspector, directory DurableDirectory, path string, secure bool) error {
	heldInfo, err := directory.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	if secure {
		if err := validateSecureDirectoryInfo(inspector, heldInfo); err != nil {
			return err
		}
		if err := validateSecureDirectoryInfo(inspector, pathInfo); err != nil {
			return err
		}
	} else {
		if heldInfo.Mode()&os.ModeSymlink != 0 || !heldInfo.IsDir() {
			return fmt.Errorf("%w: retained directory type", ErrUnsafeSecurePath)
		}
		if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() {
			return fmt.Errorf("%w: directory path type", ErrUnsafeSecurePath)
		}
		if ancestors, ok := inspector.(SecureAncestorInspector); ok {
			if err := ancestors.ValidateRetainedAncestor(heldInfo); err != nil {
				return err
			}
			if err := ancestors.ValidateRetainedAncestor(pathInfo); err != nil {
				return err
			}
		}
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || !SameSecureObject(heldIdentity, pathIdentity) {
		return fmt.Errorf("%w: retained directory path identity", ErrUnsafeSecurePath)
	}
	return nil
}

func ValidateSecureDirectory(fsys FileSystem, path string) error {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	return validateSecureDirectoryInfo(inspector, info)
}

// ValidateOwnerControlledDirectory accepts private directories and the
// standard owner-writable, world-readable directory mode used by Codex for
// ~/.codex. Secret children still require their own strict file validation.
func ValidateOwnerControlledDirectory(fsys FileSystem, path string) error {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	return validateOwnerControlledDirectoryInfo(inspector, info)
}

func ValidateExternalCredentialDirectory(fsys FileSystem, path string) error {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	return validateExternalCredentialDirectoryInfo(inspector, info)
}

func validateExternalCredentialDirectoryInfo(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: external credential directory type", ErrUnsafeSecurePath)
	}
	if external, ok := inspector.(SecureExternalPathInspector); ok {
		return external.ValidateExternalCredentialDirectoryInfo(info)
	}
	permissions := info.Mode().Perm()
	if permissions != 0o700 && permissions != 0o755 {
		return fmt.Errorf("%w: directory mode %04o", ErrUnsafeSecurePath, permissions)
	}
	return ValidateSecureOwner(inspector, info)
}

// OpenOwnerControlledDirectory retains an owner-controlled 0700 or 0755
// directory without applying the stricter private-directory opener policy.
func OpenOwnerControlledDirectory(fsys FileSystem, path string) (SecureDirectory, error) {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(DurableDirectoryOpener)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	durable, err := opener.OpenDurableDirectory(path)
	if err != nil {
		return nil, err
	}
	directory, ok := durable.(SecureDirectory)
	if !ok {
		_ = durable.Close()
		return nil, ErrSecureCapabilityUnavailable
	}
	if err := ValidateOwnerControlledDirectoryHandle(inspector, directory, path); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func validateOwnerControlledDirectoryInfo(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: directory type", ErrUnsafeSecurePath)
	}
	permissions := info.Mode().Perm()
	if permissions != 0o700 && permissions != 0o755 {
		return fmt.Errorf("%w: directory mode %04o", ErrUnsafeSecurePath, permissions)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: directory special mode", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func validateSecureDirectoryInfo(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: directory type", ErrUnsafeSecurePath)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: directory mode %04o", ErrUnsafeSecurePath, info.Mode().Perm())
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: directory special mode", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func ValidateSecureRegularFile(fsys FileSystem, path string) error {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	return validateSecureRegularInfo(inspector, info)
}

func ReadSecureFile(fsys FileSystem, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("invalid secure file read limit")
	}
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(SecureDirectoryOpener)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	dir := filepath.Dir(path)
	if err := ValidateSecureDirectory(fsys, dir); err != nil {
		return nil, err
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	data, _, err := readSecureFileInDirectory(inspector, directory, filepath.Base(path), maxBytes, func() error {
		return validateSecureDirectoryHandle(inspector, directory, dir)
	})
	return data, err
}

// ReadSecureFileInDirectory reads one private regular file relative to a
// retained directory handle without reopening its pathname namespace.
func ReadSecureFileInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, maxBytes int64) ([]byte, error) {
	data, _, err := readSecureFileInDirectory(inspector, directory, name, maxBytes, nil)
	return data, err
}

// ReadSecureFileInDirectoryWithIdentity returns content and identity from the
// same no-follow descriptor so callers can reject identical-byte replacement.
func ReadSecureFileInDirectoryWithIdentity(inspector SecurePathInspector, directory SecureDirectory, name string, maxBytes int64) ([]byte, SecureFileIdentity, error) {
	return readSecureFileInDirectory(inspector, directory, name, maxBytes, nil)
}

// ReadOwnerControlledFileInDirectoryWithIdentity reads a strict 0600 file
// from a retained owner-controlled 0700 or 0755 directory.
func ReadOwnerControlledFileInDirectoryWithIdentity(inspector SecurePathInspector, directory SecureDirectory, directoryPath, name string, maxBytes int64) ([]byte, SecureFileIdentity, error) {
	return readSecureFileInDirectory(inspector, directory, name, maxBytes, func() error {
		return ValidateOwnerControlledDirectoryHandle(inspector, directory, directoryPath)
	})
}

func readSecureFileInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, maxBytes int64, fence func() error) ([]byte, SecureFileIdentity, error) {
	if inspector == nil || directory == nil {
		return nil, SecureFileIdentity{}, ErrSecureCapabilityUnavailable
	}
	if maxBytes <= 0 {
		return nil, SecureFileIdentity{}, fmt.Errorf("invalid secure file read limit")
	}
	if err := validateSecureEntryName(name); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	validateDirectory := func() error {
		return validateSecureDirectoryDescriptor(inspector, directory)
	}
	if fence != nil {
		validateDirectory = fence
	}
	if err := validateDirectory(); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	file, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, SecureFileIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, SecureFileIdentity{}, err
	}
	if err := validateSecureRegularInfo(inspector, info); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok {
		return nil, SecureFileIdentity{}, fmt.Errorf("%w: secure file identity", ErrUnsafeSecurePath)
	}
	data, err := readSecureFileContent(file, maxBytes)
	if err != nil {
		return nil, SecureFileIdentity{}, err
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, SecureFileIdentity{}, err
	}
	if err := validateSecureRegularInfo(inspector, afterInfo); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	afterIdentity, ok := inspector.FileIdentity(afterInfo)
	if !ok || afterIdentity != identity {
		return nil, SecureFileIdentity{}, fmt.Errorf("%w: secure file descriptor identity", ErrUnsafeSecurePath)
	}
	canonical, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, SecureFileIdentity{}, err
	}
	canonicalInfo, statErr := canonical.Stat()
	closeErr := canonical.Close()
	if statErr != nil {
		return nil, SecureFileIdentity{}, statErr
	}
	if closeErr != nil {
		return nil, SecureFileIdentity{}, closeErr
	}
	if err := validateSecureRegularInfo(inspector, canonicalInfo); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	canonicalIdentity, ok := inspector.FileIdentity(canonicalInfo)
	if !ok || canonicalIdentity != identity {
		return nil, SecureFileIdentity{}, fmt.Errorf("%w: secure file canonical entry identity", ErrUnsafeSecurePath)
	}
	if err := validateDirectory(); err != nil {
		return nil, SecureFileIdentity{}, err
	}
	return data, identity, nil
}

func readSecureFileContent(file io.Reader, maxBytes int64) ([]byte, error) {
	data := make([]byte, 0, min(maxBytes, 32*1024))
	buffer := make([]byte, 32*1024)
	for {
		remaining := maxBytes - int64(len(data))
		if remaining < int64(len(buffer)) {
			buffer = buffer[:remaining+1]
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			data = append(data, buffer[:n]...)
			if int64(len(data)) > maxBytes {
				return nil, ErrSecureFileTooLarge
			}
		}
		if errors.Is(readErr, io.EOF) {
			return data, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func validateSecureDirectoryHandle(inspector SecurePathInspector, directory SecureDirectory, path string) error {
	if err := validateSecureDirectoryDescriptor(inspector, directory); err != nil {
		return err
	}
	heldInfo, err := directory.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateSecureDirectoryInfo(inspector, pathInfo); err != nil {
		return err
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || !SameSecureObject(heldIdentity, pathIdentity) {
		return fmt.Errorf("%w: directory path identity", ErrUnsafeSecurePath)
	}
	return nil
}

// ValidateSecureDirectoryHandle proves a retained secure directory descriptor
// still names the canonical no-follow path supplied by its caller.
func ValidateSecureDirectoryHandle(inspector SecurePathInspector, directory SecureDirectory, path string) error {
	return validateSecureDirectoryHandle(inspector, directory, path)
}

// ValidateOwnerControlledDirectoryHandle proves a retained 0700 or 0755
// owner-controlled directory still names its canonical no-follow path.
func ValidateOwnerControlledDirectoryHandle(inspector SecurePathInspector, directory SecureDirectory, path string) error {
	heldInfo, err := directory.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateOwnerControlledDirectoryInfo(inspector, heldInfo); err != nil {
		return err
	}
	if err := validateOwnerControlledDirectoryInfo(inspector, pathInfo); err != nil {
		return err
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || !SameSecureObject(heldIdentity, pathIdentity) {
		return fmt.Errorf("%w: directory path identity", ErrUnsafeSecurePath)
	}
	return nil
}

func validateSecureDirectoryDescriptor(inspector SecurePathInspector, directory SecureDirectory) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	return validateSecureDirectoryInfo(inspector, info)
}

func validateSecureRegularFileInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string) error {
	_, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	return err
}

func secureRegularFileInfoInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string) (os.FileInfo, error) {
	file, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validateSecureRegularInfo(inspector, info); err != nil {
		return nil, err
	}
	return info, nil
}

func validateSecureRegularFileInDirectoryIfPresent(inspector SecurePathInspector, directory SecureDirectory, name string) error {
	err := validateSecureRegularFileInDirectory(inspector, directory, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func SameSecureObject(first, second SecureFileIdentity) bool {
	return first == second
}

func validateSecureEntryName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("%w: secure directory entry name", ErrUnsafeSecurePath)
	}
	return nil
}

func validateSecureRegularInfo(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: regular file type", ErrUnsafeSecurePath)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: regular file mode %04o", ErrUnsafeSecurePath, info.Mode().Perm())
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: regular file special mode", ErrUnsafeSecurePath)
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 {
		return fmt.Errorf("%w: regular file identity", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func AcquireExclusiveLock(fsys FileSystem, path string) (ExclusiveLock, error) {
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(SecureDirectoryOpener)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	dir := filepath.Dir(path)
	if err := ValidateSecureDirectory(fsys, dir); err != nil {
		return nil, fmt.Errorf("validate lock directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("open lock directory: %w", err)
	}
	defer directory.Close()
	return acquireExclusiveLockInDirectory(inspector, directory, filepath.Base(path), func() error {
		return validateSecureDirectoryHandle(inspector, directory, dir)
	})
}

// AcquireExclusiveLockInDirectory acquires one lifetime descriptor lock in a
// retained directory namespace and verifies the opened file remains its named
// single-link entry.
func AcquireExclusiveLockInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string) (ExclusiveLock, error) {
	return acquireExclusiveLockInDirectory(inspector, directory, name, nil)
}

// AcquireNewExclusiveLockInDirectory creates one new lifetime lock relative to
// a retained directory. It never opens an existing entry. The descriptor,
// named path, file metadata, and parent-directory commit are verified before
// ownership is returned.
func AcquireNewExclusiveLockInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string) (ExclusiveLock, error) {
	if inspector == nil || directory == nil {
		return nil, ErrSecureCapabilityUnavailable
	}
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	if err := validateSecureDirectoryDescriptor(inspector, directory); err != nil {
		return nil, fmt.Errorf("validate opened lock directory: %w", err)
	}
	creator, ok := directory.(NewExclusiveLocker)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	lock, err := creator.OpenNewExclusiveLock(name, 0o600)
	if err != nil {
		return nil, err
	}
	closeWith := func(err error) (ExclusiveLock, error) {
		return nil, errors.Join(err, lock.Close())
	}
	heldInfo, err := lock.Stat()
	if err != nil {
		return closeWith(fmt.Errorf("inspect new lock descriptor: %w", err))
	}
	if err := validateSecureRegularInfo(inspector, heldInfo); err != nil {
		return closeWith(fmt.Errorf("validate new lock descriptor: %w", err))
	}
	pathInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return closeWith(fmt.Errorf("inspect new lock path: %w", err))
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || heldIdentity != pathIdentity {
		return closeWith(fmt.Errorf("%w: new lock path identity", ErrUnsafeSecurePath))
	}
	if err := directory.Sync(); err != nil {
		return closeWith(fmt.Errorf("sync new lock directory: %w", err))
	}
	pathInfo, err = secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return closeWith(fmt.Errorf("revalidate new lock path: %w", err))
	}
	pathIdentity, pathOK = inspector.FileIdentity(pathInfo)
	if !pathOK || pathIdentity != heldIdentity {
		return closeWith(fmt.Errorf("%w: synced new lock path identity", ErrUnsafeSecurePath))
	}
	if err := validateSecureDirectoryDescriptor(inspector, directory); err != nil {
		return closeWith(fmt.Errorf("revalidate new lock directory: %w", err))
	}
	return lock, nil
}

// ValidateExclusiveLockHeldInDirectory proves that the expected named lock is
// still the strict single-link file in a retained directory and that another
// descriptor currently holds its non-blocking exclusive lock. It never creates
// the lock or retains ownership of an unlocked file.
func ValidateExclusiveLockHeldInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, expected SecureFileIdentity) error {
	if inspector == nil || directory == nil {
		return ErrSecureCapabilityUnavailable
	}
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	if err := validateSecureDirectoryDescriptor(inspector, directory); err != nil {
		return fmt.Errorf("validate opened lock directory: %w", err)
	}
	beforeInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return fmt.Errorf("validate lock file: %w", err)
	}
	beforeIdentity, ok := inspector.FileIdentity(beforeInfo)
	if !ok || beforeIdentity != expected {
		return fmt.Errorf("%w: expected lock identity", ErrUnsafeSecurePath)
	}
	prober, ok := directory.(ExclusiveLockHeldProber)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	probedInfo, err := prober.ProbeExclusiveLockHeld(name, 0o600)
	if err != nil {
		return err
	}
	if err := validateSecureRegularInfo(inspector, probedInfo); err != nil {
		return fmt.Errorf("validate probed lock descriptor: %w", err)
	}
	probedIdentity, ok := inspector.FileIdentity(probedInfo)
	if !ok || probedIdentity != expected {
		return fmt.Errorf("%w: probed lock identity", ErrUnsafeSecurePath)
	}
	afterInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		return fmt.Errorf("revalidate lock file: %w", err)
	}
	afterIdentity, ok := inspector.FileIdentity(afterInfo)
	if !ok || afterIdentity != expected {
		return fmt.Errorf("%w: revalidated lock identity", ErrUnsafeSecurePath)
	}
	if err := validateSecureDirectoryDescriptor(inspector, directory); err != nil {
		return fmt.Errorf("revalidate opened lock directory: %w", err)
	}
	return nil
}

func acquireExclusiveLockInDirectory(inspector SecurePathInspector, directory SecureDirectory, name string, fence func() error) (ExclusiveLock, error) {
	if inspector == nil || directory == nil {
		return nil, ErrSecureCapabilityUnavailable
	}
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	validateDirectory := func() error {
		return validateSecureDirectoryDescriptor(inspector, directory)
	}
	if fence != nil {
		validateDirectory = fence
	}
	if err := validateDirectory(); err != nil {
		return nil, fmt.Errorf("validate opened lock directory: %w", err)
	}
	if err := validateSecureRegularFileInDirectoryIfPresent(inspector, directory, name); err != nil {
		return nil, fmt.Errorf("validate lock file: %w", err)
	}
	lock, err := directory.OpenExclusiveLock(name, 0o600)
	if err != nil {
		return nil, err
	}
	heldInfo, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("inspect acquired lock file: %w", err)
	}
	if err := validateSecureRegularInfo(inspector, heldInfo); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("validate acquired lock descriptor: %w", err)
	}
	pathInfo, err := secureRegularFileInfoInDirectory(inspector, directory, name)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("inspect acquired lock path: %w", err)
	}
	heldIdentity, heldOK := inspector.FileIdentity(heldInfo)
	pathIdentity, pathOK := inspector.FileIdentity(pathInfo)
	if !heldOK || !pathOK || heldIdentity != pathIdentity {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: acquired lock path identity", ErrUnsafeSecurePath)
	}
	if err := validateDirectory(); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("validate acquired lock directory: %w", err)
	}
	return lock, nil
}

func ValidateSecureOwner(inspector SecurePathInspector, info os.FileInfo) error {
	if principals, ok := inspector.(SecurePrincipalInspector); ok {
		effective, effectiveOK := principals.EffectivePrincipal()
		owner, ownerOK := principals.FileOwnerPrincipal(info)
		if !effectiveOK || !ownerOK || effective != owner {
			return fmt.Errorf("%w: owner", ErrUnsafeSecurePath)
		}
		return nil
	}
	owner, ok := inspector.FileOwnerUID(info)
	if !ok || owner != inspector.EffectiveUID() {
		return fmt.Errorf("%w: owner", ErrUnsafeSecurePath)
	}
	return nil
}

func validateSecureOwner(inspector SecurePathInspector, info os.FileInfo) error {
	return ValidateSecureOwner(inspector, info)
}

func ValidateExternalCredentialFile(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: external file type", ErrUnsafeSecurePath)
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 {
		return fmt.Errorf("%w: external credential identity", ErrUnsafeSecurePath)
	}
	if external, ok := inspector.(SecureExternalPathInspector); ok {
		return external.ValidateExternalCredential(info)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: external credential mode", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func ValidateExternalCacheFile(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: external cache file type", ErrUnsafeSecurePath)
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 {
		return fmt.Errorf("%w: external cache identity", ErrUnsafeSecurePath)
	}
	if external, ok := inspector.(SecureExternalPathInspector); ok {
		return external.ValidateExternalCache(info)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: external cache file is writable by another principal", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func ValidateRetainedExternalImportFile(inspector SecurePathInspector, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: external import file type", ErrUnsafeSecurePath)
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 {
		return fmt.Errorf("%w: external import identity", ErrUnsafeSecurePath)
	}
	if external, ok := inspector.(SecureExternalPathInspector); ok {
		return external.ValidateRetainedExternalImportFileInfo(info)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: external import file is writable by another principal", ErrUnsafeSecurePath)
	}
	return ValidateSecureOwner(inspector, info)
}

func createUniqueTemporary(creator ExclusiveFileCreator, base string) (string, DurableFile, error) {
	for range 32 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := "." + base + "." + hex.EncodeToString(random[:]) + ".tmp"
		file, err := creator.CreateExclusive(name, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("allocate unique durable temporary file: %w", os.ErrExist)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func commitFailure(op string, err error) error {
	return &CommitError{Outcome: CommitNotCommitted, Op: op, Err: err}
}

func commitIndeterminate(op string, err error) error {
	return &CommitError{Outcome: CommitIndeterminate, Op: op, Err: err}
}
