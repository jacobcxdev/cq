//go:build !unix

package fsutil

import "os"

func (OSFileSystem) CreateExclusive(name string, perm os.FileMode) (DurableFile, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

func (OSFileSystem) OpenSecureDirectory(string) (SecureDirectory, error) {
	return nil, ErrSecureCapabilityUnavailable
}

func (OSFileSystem) OpenDurableDirectory(string) (DurableDirectory, error) {
	return nil, ErrSecureCapabilityUnavailable
}
