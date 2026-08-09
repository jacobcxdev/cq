//go:build unix

package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	_, _, owner, ok := externalFileIdentity(info)
	return ok && owner == uint64(os.Getuid())
}

func externalFileIdentity(info os.FileInfo) (device, inode, owner uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), uint64(stat.Uid), true
}

func openExternalFileNoFollow(root, path string) (*os.File, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: file containment", ErrExternalUnsafePath)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	directoryFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for index, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		nextFD, openErr := unix.Openat(directoryFD, part, flags, 0)
		closeErr := unix.Close(directoryFD)
		if openErr != nil {
			return nil, openErr
		}
		if closeErr != nil {
			unix.Close(nextFD)
			return nil, closeErr
		}
		if index == len(parts)-1 {
			file := os.NewFile(uintptr(nextFD), path)
			if file == nil {
				unix.Close(nextFD)
				return nil, fmt.Errorf("%w: file descriptor", ErrExternalUnsafePath)
			}
			return file, nil
		}
		directoryFD = nextFD
	}
	return nil, fmt.Errorf("%w: file containment", ErrExternalUnsafePath)
}
