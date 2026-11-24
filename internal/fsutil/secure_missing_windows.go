//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// IsDirectCQDirectory identifies the registered or explicitly scoped CQ root
// without opening it. Callers must still use OpenOwnerControlledCQDirectory.
func (fsys OSFileSystem) IsDirectCQDirectory(path string) bool {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return false
	}
	selection, err := fsys.resolveSecureBoundary(clean, secureBoundaryCQPrivate)
	return err == nil && strings.EqualFold(clean, filepath.Join(selection.AnchorPath, "cq"))
}

// OpenOwnerControlledCQDirectory retains the registered direct CQ root with
// owner-controlled ACLs while preserving the AppData boundary identity.
func (fsys OSFileSystem) OpenOwnerControlledCQDirectory(path string) (SecureDirectory, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	selection, err := fsys.resolveSecureBoundary(clean, secureBoundaryCQPrivate)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(clean, filepath.Join(selection.AnchorPath, "cq")) {
		return nil, fmt.Errorf("%w: directory outside direct CQ root", ErrUnsafeSecurePath)
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryCQPrivate)
	if err != nil {
		return nil, err
	}
	anchorFile, err := openWindowsAbsolutePath(
		selection.AnchorPath,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_DIRECTORY_FILE,
		boundary,
	)
	if err != nil {
		return nil, err
	}
	anchor := &windowsSecureDirectory{file: anchorFile}
	opened, err := anchor.OpenDirectory("cq")
	closeErr := anchor.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	directory := opened.(*windowsSecureDirectory)
	if closeErr != nil {
		return nil, errors.Join(closeErr, directory.Close())
	}
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, clean); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	directory.ownerControlled = true
	return directory, nil
}

// OpenOwnerControlledCQPolicyDirectory restricts the harness exception to its
// one policy filename while reusing the direct CQ root authority check.
func (fsys OSFileSystem) OpenOwnerControlledCQPolicyDirectory(path string) (SecureDirectory, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(clean) != "harness.json" {
		return nil, fmt.Errorf("%w: non-harness policy file", ErrUnsafeSecurePath)
	}
	return fsys.OpenOwnerControlledCQDirectory(filepath.Dir(clean))
}
