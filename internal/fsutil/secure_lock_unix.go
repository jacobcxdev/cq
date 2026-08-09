//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type unixExclusiveLock struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

func (lock *unixExclusiveLock) Stat() (os.FileInfo, error) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil, os.ErrClosed
	}
	return lock.file.Stat()
}

func (OSFileSystem) OpenExclusiveLock(name string, perm os.FileMode) (ExclusiveLock, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	if err := validateSecureEntryName(base); err != nil {
		return nil, err
	}
	directoryFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(directoryFD)
	if err := validateSecureDirectoryFD(directoryFD); err != nil {
		return nil, err
	}
	return openExclusiveLockAt(directoryFD, base, name, perm)
}

func (directory *unixSecureDirectory) OpenExclusiveLock(name string, perm os.FileMode) (ExclusiveLock, error) {
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	return openExclusiveLockAt(int(directory.file.Fd()), name, name, perm)
}

func (directory *unixSecureDirectory) ProbeExclusiveLockHeld(name string, perm os.FileMode) (os.FileInfo, error) {
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, ErrSecureCapabilityUnavailable
	}
	defer file.Close()
	if err := validateSecureFD(fd, perm); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return info, nil
		}
		return nil, fmt.Errorf("probe exclusive filesystem lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
		return nil, fmt.Errorf("release probed filesystem lock: %w", err)
	}
	return info, ErrExclusiveLockNotHeld
}

func openExclusiveLockAt(directoryFD int, name, displayName string, perm os.FileMode) (ExclusiveLock, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(directoryFD, name, flags|unix.O_CREAT|unix.O_EXCL, uint32(perm.Perm()))
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(directoryFD, name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			unix.Close(fd)
		}
	}()
	if err := validateSecureFD(fd, perm); err != nil {
		return nil, err
	}
	if created {
		if err := unix.Fsync(fd); err != nil {
			return nil, err
		}
		if err := unix.Fsync(directoryFD); err != nil {
			return nil, err
		}
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrExclusiveLockHeld
		}
		return nil, fmt.Errorf("acquire exclusive filesystem lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), displayName)
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, ErrSecureCapabilityUnavailable
	}
	closeFD = false
	return &unixExclusiveLock{file: file}, nil
}

func (lock *unixExclusiveLock) Close() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	fd := int(lock.file.Fd())
	unlockErr := unix.Flock(fd, unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}
