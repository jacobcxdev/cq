//go:build unix && !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package fsutil

import "os"

func (OSFileSystem) OpenExclusiveLock(string, os.FileMode) (ExclusiveLock, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func (*unixSecureDirectory) OpenExclusiveLock(string, os.FileMode) (ExclusiveLock, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func (*unixSecureDirectory) ProbeExclusiveLockHeld(string, os.FileMode) (os.FileInfo, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func inspectInheritedExclusiveLockFile(*os.File) (os.FileInfo, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func validateInheritedExclusiveLockHeld(*os.File) error {
	return ErrSecureCapabilityUnavailable
}
