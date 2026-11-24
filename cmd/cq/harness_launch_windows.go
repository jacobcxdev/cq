//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsCodexPackageFamily = "OpenAI.Codex_2p2nqsd0c76g0"

var (
	windowsPackageAPI            = windows.NewLazySystemDLL("kernel32.dll")
	windowsPackagesByFamily      = windowsPackageAPI.NewProc("GetPackagesByPackageFamily")
	windowsPackagePathByFullName = windowsPackageAPI.NewProc("GetPackagePathByFullName")
)

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
	cmd := exec.Command(desktop)
	cmd.Dir = filepath.Dir(desktop)
	cmd.Env = windowsHarnessChildEnv(os.Environ(), self, stock)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex Desktop: %w", err)
	}
	return cmd.Process.Release()
}

func officialWindowsCodexPackage(full string) bool {
	parts := strings.Split(full, "_")
	if len(parts) != 5 || parts[0] != "OpenAI.Codex" || parts[3] != "" || parts[4] != "2p2nqsd0c76g0" {
		return false
	}
	if parts[2] != "x64" && parts[2] != "arm64" {
		return false
	}
	version := strings.Split(parts[1], ".")
	if len(version) != 4 {
		return false
	}
	for _, component := range version {
		if component == "" {
			return false
		}
		if _, err := strconv.ParseUint(component, 10, 16); err != nil {
			return false
		}
	}
	return true
}

func windowsHarnessChildEnv(base []string, self, stock string) []string {
	env := make([]string, 0, len(base)+2)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok && (strings.EqualFold(key, "CODEX_CLI_PATH") || strings.EqualFold(key, windowsHarnessStockCLIEnv)) {
			continue
		}
		env = append(env, item)
	}
	return append(env, "CODEX_CLI_PATH="+self, windowsHarnessStockCLIEnv+"="+stock)
}

func resolveWindowsHarnessDesktop() (desktop, stock string, err error) {
	packages, err := windowsRegisteredCodexPackages()
	if err != nil {
		return "", "", err
	}
	var selected string
	for _, full := range packages {
		if !officialWindowsCodexPackage(full) {
			continue
		}
		if selected != "" {
			return "", "", errors.New("multiple official Codex Desktop packages are registered")
		}
		selected = full
	}
	if selected == "" {
		return "", "", errors.New("official Codex Desktop package is not registered for this user")
	}
	root, err := windowsCodexPackagePath(selected)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !strings.EqualFold(filepath.Base(root), selected) {
		return "", "", errors.New("Codex Desktop package path is invalid")
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

func windowsRegisteredCodexPackages() ([]string, error) {
	family, err := windows.UTF16PtrFromString(windowsCodexPackageFamily)
	if err != nil {
		return nil, err
	}
	var count, length uint32
	status, _, _ := windowsPackagesByFamily.Call(uintptr(unsafe.Pointer(family)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&length)), 0)
	if status == uintptr(windows.APPMODEL_ERROR_NO_PACKAGE) {
		return nil, errors.New("official Codex Desktop package is not registered for this user")
	}
	if status != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || count == 0 || count > 16 || length == 0 || length > 65536 {
		return nil, fmt.Errorf("query Codex Desktop registration: %w", syscall.Errno(status))
	}
	names := make([]*uint16, count)
	buffer := make([]uint16, length)
	status, _, _ = windowsPackagesByFamily.Call(uintptr(unsafe.Pointer(family)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&names[0])), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buffer[0])))
	if status != 0 {
		return nil, fmt.Errorf("read Codex Desktop registration: %w", syscall.Errno(status))
	}
	if count == 0 || int(count) > len(names) {
		return nil, errors.New("Codex Desktop registration count changed")
	}
	packages := make([]string, 0, count)
	for _, name := range names[:count] {
		if name == nil {
			return nil, errors.New("Codex Desktop registration contains an empty package")
		}
		packages = append(packages, windows.UTF16PtrToString(name))
	}
	return packages, nil
}

func windowsCodexPackagePath(full string) (string, error) {
	name, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return "", err
	}
	var length uint32
	status, _, _ := windowsPackagePathByFullName.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), 0)
	if status != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || length == 0 || length > 32768 {
		return "", fmt.Errorf("query Codex Desktop package path: %w", syscall.Errno(status))
	}
	buffer := make([]uint16, length)
	status, _, _ = windowsPackagePathByFullName.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buffer[0])))
	if status != 0 {
		return "", fmt.Errorf("read Codex Desktop package path: %w", syscall.Errno(status))
	}
	if length == 0 || int(length) > len(buffer) {
		return "", errors.New("Codex Desktop package path length changed")
	}
	return windows.UTF16ToString(buffer), nil
}

func checkWindowsPackagePath(path string) error {
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("invalid package executable path")
	}
	current := volume + `\`
	rest := strings.TrimPrefix(path, current)
	parts := strings.Split(rest, `\`)
	for i, part := range append([]string{""}, parts...) {
		if part != "" {
			current = filepath.Join(current, part)
		}
		wide, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(wide)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("package executable path contains a reparse point")
		}
		if i < len(parts) && attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return errors.New("package executable ancestor is not a directory")
		}
		if i == len(parts) && attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			return errors.New("package executable is not a file")
		}
	}
	return nil
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
