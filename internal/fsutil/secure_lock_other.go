//go:build !unix && !windows

package fsutil

import "os"

func (OSFileSystem) OpenExclusiveLock(string, os.FileMode) (ExclusiveLock, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func inspectInheritedExclusiveLockFile(*os.File) (os.FileInfo, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func validateInheritedExclusiveLockHeld(*os.File) error {
	return ErrSecureCapabilityUnavailable
}
