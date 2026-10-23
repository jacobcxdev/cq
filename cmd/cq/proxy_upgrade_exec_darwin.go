//go:build darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

// Duplicate into unused descriptors, without replacing Go's live poller or
// network descriptors. The successor receives their numbers in its private
// entry arguments and authenticates the descriptor contents before use.
func darwinUpgradeExecFiles(ctx context.Context, executable string, files []*os.File, arguments []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(executable) || len(files) != 4 || len(arguments) == 0 {
		return proxy.ErrRuntimeUpgradeReceipt
	}
	var sources [4]int
	for index := range sources {
		sources[index] = -1
	}
	defer func() {
		for _, fd := range sources {
			if fd >= 0 {
				unix.Close(fd)
			}
		}
	}()
	arguments = append([]string(nil), arguments...)
	for index, file := range files {
		if file == nil {
			return proxy.ErrRuntimeUpgradeReceipt
		}
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 100)
		if err != nil {
			return err
		}
		sources[index] = fd
		arguments = append(arguments, strconv.Itoa(fd))
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	syscall.ForkLock.Lock()
	defer func() {
		for index, fd := range sources {
			if fd >= 0 {
				unix.Close(fd)
				sources[index] = -1
			}
		}
		syscall.ForkLock.Unlock()
	}()
	for _, fd := range sources {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			return err
		}
	}
	return unix.Exec(executable, arguments, os.Environ())
}
