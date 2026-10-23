//go:build darwin || linux

package dots

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openDotsPrivateLog(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "Dots private log")
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if !info.Mode().IsRegular() || !owned(info) || (mode != 0o600 && mode != 0o644) {
		file.Close()
		return nil, errors.New("Dots log must be owned and mode 0600")
	}
	// Older runtimes created diagnostic logs as 0644. Tighten only the
	// already-open, verified file; never follow a replacement or symlink.
	if mode == 0o644 {
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}
