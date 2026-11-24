//go:build windows

package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// SealOwnedExternalCacheFileInDirectory repairs a user-owned, mutation-safe
// file to the private ACL after its exact bytes and retained identity are proved.
func SealOwnedExternalCacheFileInDirectory(fsys OSFileSystem, directory SecureDirectory, dirPath, name string, expectedIdentity SecureFileIdentity, expected []byte) (resultErr error) {
	retained, ok := directory.(*windowsSecureDirectory)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, dirPath); err != nil {
		return err
	}
	opened, err := openWindowsRelative(
		windows.Handle(retained.file.Fd()), name,
		windows.FILE_GENERIC_READ|windows.WRITE_DAC|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil,
	)
	if err != nil {
		return err
	}
	file := opened.file
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if opened.information != windowsFileOpened {
		return fmt.Errorf("%w: unexpected Windows credential open result", ErrUnsafeSecurePath)
	}
	info, err := inspectWindowsHandle(windows.Handle(file.Fd()), name)
	if err != nil {
		return err
	}
	if err := ValidateExternalCacheFile(fsys, info); err != nil {
		return err
	}
	identity, ok := fsys.FileIdentity(info)
	if !ok || !SameSecureObject(identity, expectedIdentity) {
		return fmt.Errorf("%w: managed credential identity", ErrUnsafeSecurePath)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil || !bytes.Equal(data, expected) {
		return errors.Join(fmt.Errorf("%w: managed credential bytes changed", ErrUnsafeSecurePath), err)
	}
	path := filepath.Join(dirPath, name)
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, dirPath); err != nil {
		return err
	}
	pathInfo, err := fsys.Lstat(path)
	if err != nil {
		return err
	}
	pathIdentity, ok := fsys.FileIdentity(pathInfo)
	if !ok || !SameSecureObject(pathIdentity, identity) {
		return fmt.Errorf("%w: managed credential path identity", ErrUnsafeSecurePath)
	}
	descriptor, err := windowsPrivateSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: reread repaired credential: %v", ErrCommitIndeterminate, err)
	}
	data, err = io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil || !bytes.Equal(data, expected) {
		return errors.Join(fmt.Errorf("%w: repaired credential bytes changed", ErrCommitIndeterminate), err)
	}
	info, err = inspectWindowsHandle(windows.Handle(file.Fd()), name)
	if err != nil {
		return fmt.Errorf("%w: inspect repaired credential: %v", ErrCommitIndeterminate, err)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: repaired credential is not private", ErrCommitIndeterminate)
	}
	pathInfo, err = fsys.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect repaired credential path: %v", ErrCommitIndeterminate, err)
	}
	pathIdentity, ok = fsys.FileIdentity(pathInfo)
	if !ok || !SameSecureObject(pathIdentity, identity) || pathInfo.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: repaired credential path changed", ErrCommitIndeterminate)
	}
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, dirPath); err != nil {
		return fmt.Errorf("%w: repaired credential parent changed: %v", ErrCommitIndeterminate, err)
	}
	return nil
}
