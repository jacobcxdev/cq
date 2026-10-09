//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

// Preserve overwritten descriptors and their flags if exec fails. Duplicate
// sources above the reserved range first, so rearrangement cannot alias them.
func darwinUpgradeExecFiles(ctx context.Context, executable string, files []*os.File, arguments []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(executable) || len(files) != 4 || len(arguments) == 0 {
		return proxy.ErrRuntimeUpgradeReceipt
	}
	var sources, originals [4]int
	var flags [4]int
	for index := range sources {
		sources[index] = -1
		originals[index] = -1
	}
	defer func() {
		for _, fd := range append(sources[:], originals[:]...) {
			if fd >= 0 {
				unix.Close(fd)
			}
		}
	}()
	for index, file := range files {
		if file == nil {
			return proxy.ErrRuntimeUpgradeReceipt
		}
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 10)
		if err != nil {
			return err
		}
		sources[index] = fd
		original := proxy.RuntimeListenerFD + index
		oldFlags, err := unix.FcntlInt(uintptr(original), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return err
		}
		flags[index] = oldFlags
		backup, err := unix.FcntlInt(uintptr(original), unix.F_DUPFD_CLOEXEC, 10)
		if err != nil {
			return err
		}
		originals[index] = backup
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	defer func() {
		for index, backup := range originals {
			target := proxy.RuntimeListenerFD + index
			if backup < 0 {
				unix.Close(target)
				continue
			}
			unix.Dup2(backup, target)
			unix.FcntlInt(uintptr(target), unix.F_SETFD, flags[index])
		}
	}()
	for index, source := range sources {
		target := proxy.RuntimeListenerFD + index
		if err := unix.Dup2(source, target); err != nil {
			return err
		}
		if _, err := unix.FcntlInt(uintptr(target), unix.F_SETFD, 0); err != nil {
			return err
		}
	}
	return unix.Exec(executable, arguments, os.Environ())
}
