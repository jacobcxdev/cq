//go:build windows

package fsutil

import (
	"os"
)

func (OSFileSystem) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

func (OSFileSystem) EffectiveUID() uint64 { return 0 }

func (OSFileSystem) FileOwnerUID(info os.FileInfo) (uint64, bool) {
	_ = info
	return 0, false
}

func (OSFileSystem) FileIdentity(info os.FileInfo) (SecureFileIdentity, bool) {
	_ = info
	return SecureFileIdentity{}, false
}
