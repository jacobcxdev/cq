//go:build darwin

package fsutil

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func renameNoReplaceAt(oldDirectory int, oldName string, newDirectory int, newName string) error {
	err := unix.RenameatxNp(oldDirectory, oldName, newDirectory, newName, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("%w: no-replace rename", ErrSecureCapabilityUnavailable)
	}
	return err
}
