//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// VerifyMissingOwnerControlledCQFile permits a missing file directly below an
// owner-controlled CQ root. It never permits reading a present file there.
func (fsys OSFileSystem) VerifyMissingOwnerControlledCQFile(path string) (err error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return err
	}
	selection, err := fsys.resolveSecureBoundary(clean, secureBoundaryCQPrivate)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	if !strings.EqualFold(dir, filepath.Join(selection.AnchorPath, "cq")) {
		return fmt.Errorf("%w: policy outside direct CQ root", ErrUnsafeSecurePath)
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryCQPrivate)
	if err != nil {
		return err
	}
	anchorFile, err := openWindowsAbsolutePath(
		selection.AnchorPath,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_DIRECTORY_FILE,
		boundary,
	)
	if err != nil {
		return err
	}
	anchor := &windowsSecureDirectory{file: anchorFile}
	defer func() { err = errors.Join(err, anchor.Close()) }()
	opened, err := anchor.OpenDirectory("cq")
	if err != nil {
		return err
	}
	directory := opened.(*windowsSecureDirectory)
	defer func() { err = errors.Join(err, directory.Close()) }()
	if err := ValidateOwnerControlledDirectoryHandle(fsys, directory, dir); err != nil {
		return err
	}
	file, err := directory.OpenNoFollow(filepath.Base(clean))
	if err == nil {
		return errors.Join(fmt.Errorf("%w: policy file exists", ErrUnsafeSecurePath), file.Close())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return ValidateOwnerControlledDirectoryHandle(fsys, directory, dir)
}
