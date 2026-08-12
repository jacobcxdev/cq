//go:build !unix

package codex

import (
	"fmt"
	"os"
)

func ownedByCurrentUser(os.FileInfo) bool { return false }

func externalFileIdentity(os.FileInfo) (device, inode, owner uint64, ok bool) {
	return 0, 0, 0, false
}

func openExternalFileNoFollow(string, string) (*os.File, error) {
	return nil, fmt.Errorf("%w: unsupported platform", ErrExternalUnsafePath)
}
