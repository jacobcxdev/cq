//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/winpackage"
	"golang.org/x/sys/windows"
)

var (
	windowsShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
)

// shellExecuteInfo mirrors SHELLEXECUTEINFOW from <shellapi.h>.
type shellExecuteInfo struct {
	size, mask                        uint32
	window                            windows.Handle
	verb, file, parameters, directory *uint16
	show                              int32
	instance                          windows.Handle
	idList                            unsafe.Pointer
	class                             *uint16
	classKey                          windows.Handle
	hotKey                            uint32
	iconOrMonitor, process            windows.Handle
}

func launchHarnessDesktop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	desktop, stock, err := resolveWindowsHarnessDesktop()
	if err != nil {
		return err
	}
	running, err := windowsHarnessDesktopRunning(desktop)
	if err != nil {
		return err
	}
	if running {
		return errors.New("Codex Desktop is already running; close it before launching with CQ harness profiles")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve CQ executable: %w", err)
	}
	if !filepath.IsAbs(self) || filepath.Clean(self) != self {
		return errors.New("CQ executable path is invalid")
	}
	info, err := os.Lstat(self)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("CQ executable is unavailable or redirected")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	app, err := windows.UTF16PtrFromString(desktop)
	if err != nil {
		return err
	}
	if err := withWindowsHarnessEnvironment(self, stock, func() error { return activateWindowsDesktop(app) }); err != nil {
		return fmt.Errorf("start Codex Desktop: %w", err)
	}
	return nil
}

func activateWindowsDesktop(app *uint16) error {
	const (
		shellExecuteNoAsync = 0x00000100
		shellExecuteNoUI    = 0x00000400
	)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
	if err != nil && !errors.Is(err, syscall.Errno(1)) { // S_FALSE is also a successful COM initialization.
		return err
	}
	defer windows.CoUninitialize()
	// SEE_MASK_NOASYNC keeps activation synchronous while the short-lived CQ
	// process still owns the environment for the new Desktop process.
	info := shellExecuteInfo{mask: shellExecuteNoAsync | shellExecuteNoUI, file: app, show: windows.SW_SHOWNORMAL}
	info.size = uint32(unsafe.Sizeof(info))
	if ok, _, err := windowsShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return err
	}
	return nil
}

func officialWindowsCodexPackage(full string) bool {
	return winpackage.OfficialCodexPackage(full)
}

func withWindowsHarnessEnvironment(self, stock string, start func() error) error {
	for _, setting := range [][2]string{{"CODEX_CLI_PATH", self}, {windowsHarnessStockCLIEnv, stock}} {
		key, value := setting[0], setting[1]
		previous, existed := os.LookupEnv(key)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
		defer func() {
			if existed {
				_ = os.Setenv(key, previous)
			} else {
				_ = os.Unsetenv(key)
			}
		}()
	}
	return start()
}

func registeredHarnessExecutable() string {
	_, stock, err := resolveWindowsHarnessDesktop()
	if err != nil {
		return ""
	}
	return stock
}

func resolveWindowsHarnessDesktop() (desktop, stock string, err error) {
	root, err := winpackage.RegisteredCodexRoot()
	if err != nil {
		return "", "", err
	}
	desktop = filepath.Join(root, "app", "ChatGPT.exe")
	stock = filepath.Join(root, "app", "resources", "codex.exe")
	if err := checkWindowsPackagePath(desktop); err != nil {
		return "", "", fmt.Errorf("Codex Desktop executable: %w", err)
	}
	if err := checkWindowsPackagePath(stock); err != nil {
		return "", "", fmt.Errorf("stock Codex CLI executable: %w", err)
	}
	return desktop, stock, nil
}

func checkWindowsPackagePath(path string) error {
	return winpackage.CheckPath(path)
}

func windowsHarnessDesktopRunning(desktop string) (bool, error) {
	var currentSession uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &currentSession); err != nil {
		return false, fmt.Errorf("inspect current Windows session: %w", err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, fmt.Errorf("inspect Codex Desktop processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false, fmt.Errorf("inspect Codex Desktop processes: %w", err)
	}
	for {
		var session uint32
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "ChatGPT.exe") &&
			windows.ProcessIdToSessionId(entry.ProcessID, &session) == nil && session == currentSession {
			image, err := queryWindowsProcessExecutable(entry.ProcessID)
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				// The process exited after the snapshot.
			} else if err != nil {
				return false, fmt.Errorf("inspect running ChatGPT process: %w", err)
			} else if strings.EqualFold(filepath.Clean(image), desktop) {
				return true, nil
			}
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect Codex Desktop processes: %w", err)
		}
	}
}
