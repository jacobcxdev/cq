//go:build !windows

package proxy

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type runtimeExecutableIdentity struct {
	device uint64
	inode  uint64
}

func runtimeExecutableDigest(path string) ([sha256.Size]byte, runtimeExecutableIdentity, error) {
	var empty [sha256.Size]byte
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return empty, runtimeExecutableIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), "runtime-worker-executable")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink == 0 {
		return empty, runtimeExecutableIdentity{}, errors.Join(ErrRuntimeArtifactMismatch, err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return empty, runtimeExecutableIdentity{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, runtimeExecutableIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}
