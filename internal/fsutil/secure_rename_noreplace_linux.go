//go:build linux

package fsutil

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func renameNoReplaceAt(oldDirectory int, oldName string, newDirectory int, newName string) error {
	err := unix.Renameat2(oldDirectory, oldName, newDirectory, newName, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("%w: no-replace rename", ErrSecureCapabilityUnavailable)
	}
	return err
}
