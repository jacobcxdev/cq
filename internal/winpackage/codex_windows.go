//go:build windows

package winpackage

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const codexFamily = "OpenAI.Codex_2p2nqsd0c76g0"

var (
	packageAPI       = windows.NewLazySystemDLL("kernel32.dll")
	packagesByFamily = packageAPI.NewProc("GetPackagesByPackageFamily")
	packagePath      = packageAPI.NewProc("GetPackagePathByFullName")
)

func OfficialCodexPackage(full string) bool {
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

// RegisteredCodexRoot resolves the one official package registered for the
// current Windows user. It never launches Desktop or reads its profile.
func RegisteredCodexRoot() (string, error) {
	packages, err := registeredCodexPackages()
	if err != nil {
		return "", err
	}
	var selected string
	for _, full := range packages {
		if !OfficialCodexPackage(full) {
			continue
		}
		if selected != "" {
			return "", errors.New("multiple official Codex Desktop packages are registered")
		}
		selected = full
	}
	if selected == "" {
		return "", errors.New("official Codex Desktop package is not registered for this user")
	}
	root, err := codexPackagePath(selected)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !strings.EqualFold(filepath.Base(root), selected) {
		return "", errors.New("Codex Desktop package path is invalid")
	}
	return root, nil
}

func registeredCodexPackages() ([]string, error) {
	family, err := windows.UTF16PtrFromString(codexFamily)
	if err != nil {
		return nil, err
	}
	var count, length uint32
	status, _, _ := packagesByFamily.Call(uintptr(unsafe.Pointer(family)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&length)), 0)
	if status == uintptr(windows.APPMODEL_ERROR_NO_PACKAGE) {
		return nil, errors.New("official Codex Desktop package is not registered for this user")
	}
	if status != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || count == 0 || count > 16 || length == 0 || length > 65536 {
		return nil, fmt.Errorf("query Codex Desktop registration: %w", syscall.Errno(status))
	}
	names := make([]*uint16, count)
	buffer := make([]uint16, length)
	status, _, _ = packagesByFamily.Call(uintptr(unsafe.Pointer(family)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&names[0])), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buffer[0])))
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

func codexPackagePath(full string) (string, error) {
	name, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return "", err
	}
	var length uint32
	status, _, _ := packagePath.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), 0)
	if status != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || length == 0 || length > 32768 {
		return "", fmt.Errorf("query Codex Desktop package path: %w", syscall.Errno(status))
	}
	buffer := make([]uint16, length)
	status, _, _ = packagePath.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buffer[0])))
	if status != 0 {
		return "", fmt.Errorf("read Codex Desktop package path: %w", syscall.Errno(status))
	}
	if length == 0 || int(length) > len(buffer) {
		return "", errors.New("Codex Desktop package path length changed")
	}
	return windows.UTF16ToString(buffer), nil
}

// CheckPath rejects reparse points at every component of a package file path.
func CheckPath(path string) error {
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
