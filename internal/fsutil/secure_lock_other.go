//go:build !unix && !windows

package fsutil

import "os"

func (OSFileSystem) OpenExclusiveLock(string, os.FileMode) (ExclusiveLock, error) {
	return nil, ErrSecureCapabilityUnavailable
}
