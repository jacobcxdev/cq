//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// OpenOwnerControlledCQPolicyDirectory retains an owner-controlled direct CQ
// root for private harness policy files without changing its inherited ACL.
func (fsys OSFileSystem) OpenOwnerControlledCQPolicyDirectory(path string) (SecureDirectory, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	selection, err := fsys.resolveSecureBoundary(clean, secureBoundaryCQPrivate)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(clean)
	if !strings.EqualFold(dir, filepath.Join(selection.AnchorPath, "cq")) || filepath.Base(clean) != "harness.json" {
		return nil, fmt.Errorf("%w: policy outside direct CQ root", ErrUnsafeSecurePath)
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
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, dir); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	directory.ownerControlled = true
	return directory, nil
}
