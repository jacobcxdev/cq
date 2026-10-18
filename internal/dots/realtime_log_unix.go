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
	if !info.Mode().IsRegular() || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
		file.Close()
		return nil, errors.New("Dots log must be owned and mode 0600")
	}
	return file, nil
}
