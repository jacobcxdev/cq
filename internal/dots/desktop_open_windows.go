//go:build windows

package dots

import (
	"context"
	"errors"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"
)

func openDesktopApp(ctx context.Context, app string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	registered, executable, err := ResolveRegisteredWindowsApp()
	if err != nil {
		return err
	}
	if !dotsSamePath(registered.AppPath, app) {
		return errors.New("Windows Dots application differs from the registered package")
	}
	path, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
	if err != nil && !errors.Is(err, syscall.Errno(1)) {
		return err
	}
	defer windows.CoUninitialize()
	if err := ctx.Err(); err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, path, nil, nil, windows.SW_SHOWNORMAL)
}
