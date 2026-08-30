package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/installstate"
)

const (
	windowsEnvironmentKey          = `Environment`
	windowsUninstallKey            = `Software\Microsoft\Windows\CurrentVersion\Uninstall\cq`
	windowsPathAddedValue          = `CQPathAdded`
	maxWindowsInstallerBytes int64 = 64 << 20
)

type windowsRegistryKind uint8

const (
	windowsRegistryString windowsRegistryKind = iota + 1
	windowsRegistryExpandString
	windowsRegistryDWORD
)

type windowsRegistryValue struct {
	Kind   windowsRegistryKind
	String string
	DWORD  uint32
}

type windowsRegistry interface {
	Get(key, name string) (windowsRegistryValue, bool, error)
	Values(key string) (map[string]windowsRegistryValue, bool, error)
	Set(key, name string, value windowsRegistryValue) error
	DeleteValue(key, name string) error
	DeleteKey(key string) error
}

type windowsMetadataFiles interface {
	Read(path string, limit int64) ([]byte, error)
	Write(path string, body []byte, mode os.FileMode) error
	Remove(path string) error
	MkdirAll(path string, mode os.FileMode) error
	Exists(path string) (bool, error)
}

// WindowsMetadata manages per-user PATH, ARP, and durable cleanup files.
type WindowsMetadata struct {
	Root            string
	SourceInstaller string
	Registry        windowsRegistry
	Files           windowsMetadataFiles
	Broadcast       func() error
}

func (metadata *WindowsMetadata) Install(_ context.Context, installation Installation) (resultErr error) {
	if err := metadata.validate(installation); err != nil {
		return err
	}
	owned, err := metadata.inspectOwnership()
	if err != nil {
		return err
	}
	snapshot, err := metadata.snapshot()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, metadata.restore(snapshot))
		}
	}()

	pathValue, pathExists, err := metadata.Registry.Get(windowsEnvironmentKey, "Path")
	if err != nil {
		return err
	}
	if !pathExists {
		pathValue = windowsRegistryValue{Kind: windowsRegistryExpandString}
	}
	if pathValue.Kind != windowsRegistryString && pathValue.Kind != windowsRegistryExpandString {
		return fmt.Errorf("Windows user PATH has unsupported registry type")
	}
	pathAdded := uint32(0)
	if owned {
		marker, exists, err := metadata.Registry.Get(windowsUninstallKey, windowsPathAddedValue)
		if err != nil {
			return err
		}
		if exists {
			if marker.Kind != windowsRegistryDWORD || marker.DWORD > 1 {
				return fmt.Errorf("invalid CQ PATH ownership marker")
			}
			pathAdded = marker.DWORD
		}
	}
	if !pathContainsDirectory(pathValue.String, metadata.Root, "windows") {
		pathValue.String = appendWindowsPATH(pathValue.String, metadata.Root)
		if err := metadata.Registry.Set(windowsEnvironmentKey, "Path", pathValue); err != nil {
			return err
		}
		pathAdded = 1
		if err := metadata.Broadcast(); err != nil {
			return fmt.Errorf("broadcast Windows PATH update: %w", err)
		}
	}

	if err := metadata.Files.MkdirAll(metadata.Root, 0o700); err != nil {
		return fmt.Errorf("create Windows CQ install root: %w", err)
	}
	installerBody, err := metadata.Files.Read(metadata.SourceInstaller, maxWindowsInstallerBytes)
	if err != nil {
		return fmt.Errorf("read Windows installer executable: %w", err)
	}
	if err := metadata.Files.Write(metadata.helperPath(), installerBody, 0o700); err != nil {
		return fmt.Errorf("write durable Windows installer: %w", err)
	}
	if err := metadata.Files.Write(metadata.scriptPath(), []byte(renderWindowsUninstallScript(metadata.Root)), 0o600); err != nil {
		return fmt.Errorf("write Windows uninstall script: %w", err)
	}

	for name, value := range metadata.arpValues(installation, pathAdded) {
		if err := metadata.Registry.Set(windowsUninstallKey, name, value); err != nil {
			return fmt.Errorf("write Windows uninstall metadata %s: %w", name, err)
		}
	}
	committed = true
	return nil
}

func (metadata *WindowsMetadata) Remove(_ context.Context, installation Installation) (resultErr error) {
	if err := metadata.validate(installation); err != nil {
		return err
	}
	owned, err := metadata.inspectOwnership()
	if err != nil || !owned {
		return err
	}
	snapshot, err := metadata.snapshot()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, metadata.restore(snapshot))
		}
	}()
	marker, exists, err := metadata.Registry.Get(windowsUninstallKey, windowsPathAddedValue)
	if err != nil {
		return err
	}
	if !exists || marker.Kind != windowsRegistryDWORD || marker.DWORD > 1 {
		return fmt.Errorf("invalid CQ PATH ownership marker")
	}
	if marker.DWORD == 1 {
		pathValue, pathExists, err := metadata.Registry.Get(windowsEnvironmentKey, "Path")
		if err != nil {
			return err
		}
		if pathExists {
			updated, removed := removeWindowsPATHEntry(pathValue.String, metadata.Root)
			if removed {
				pathValue.String = updated
				if err := metadata.Registry.Set(windowsEnvironmentKey, "Path", pathValue); err != nil {
					return err
				}
				if err := metadata.Broadcast(); err != nil {
					return fmt.Errorf("broadcast Windows PATH removal: %w", err)
				}
			}
		}
	}
	if err := metadata.Registry.DeleteKey(windowsUninstallKey); err != nil {
		return fmt.Errorf("remove Windows uninstall metadata: %w", err)
	}
	committed = true
	return nil
}

func (metadata *WindowsMetadata) Inspect(_ context.Context, installation Installation) error {
	if err := metadata.validate(installation); err != nil {
		return err
	}
	owned, err := metadata.inspectOwnership()
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("Windows uninstall metadata is absent")
	}
	marker, exists, err := metadata.Registry.Get(windowsUninstallKey, windowsPathAddedValue)
	if err != nil || !exists || marker.Kind != windowsRegistryDWORD || marker.DWORD > 1 {
		return fmt.Errorf("invalid CQ PATH ownership marker")
	}
	for name, want := range metadata.arpValues(installation, marker.DWORD) {
		got, exists, err := metadata.Registry.Get(windowsUninstallKey, name)
		if err != nil || !exists || got != want {
			return fmt.Errorf("Windows uninstall metadata %s differs", name)
		}
	}
	if marker.DWORD == 1 {
		pathValue, exists, err := metadata.Registry.Get(windowsEnvironmentKey, "Path")
		if err != nil || !exists || !pathContainsDirectory(pathValue.String, metadata.Root, "windows") {
			return fmt.Errorf("installer-owned Windows PATH entry is absent")
		}
	}
	for _, path := range []string{metadata.helperPath(), metadata.scriptPath()} {
		exists, err := metadata.Files.Exists(path)
		if err != nil || !exists {
			return fmt.Errorf("Windows uninstall file is absent: %s", path)
		}
	}
	script, err := metadata.Files.Read(metadata.scriptPath(), 1<<20)
	if err != nil || string(script) != renderWindowsUninstallScript(metadata.Root) {
		return fmt.Errorf("Windows uninstall script differs")
	}
	return nil
}

func (metadata *WindowsMetadata) validate(installation Installation) error {
	if metadata == nil || metadata.Registry == nil || metadata.Files == nil || metadata.Broadcast == nil || installation.Owner != installstate.OwnerWinGet {
		return fmt.Errorf("invalid Windows installation metadata")
	}
	if !cleanAbsoluteTargetPath(metadata.Root, "windows") || !cleanAbsoluteTargetPath(metadata.SourceInstaller, "windows") || !equalWindowsDirectory(installation.Executable, windowsJoin(metadata.Root, "cq.exe")) {
		return fmt.Errorf("invalid Windows installation paths")
	}
	return nil
}

func (metadata *WindowsMetadata) inspectOwnership() (bool, error) {
	values, exists, err := metadata.Registry.Values(windowsUninstallKey)
	if err != nil || !exists {
		return false, err
	}
	publisher, publisherOK := values["Publisher"]
	location, locationOK := values["InstallLocation"]
	if !publisherOK || publisher.Kind != windowsRegistryString || publisher.String != "jacobcxdev" || !locationOK || location.Kind != windowsRegistryString || !equalWindowsDirectory(location.String, metadata.Root) {
		return false, fmt.Errorf("%w: Windows uninstall metadata belongs to another installation", installstate.ErrOwnershipConflict)
	}
	return true, nil
}

func (metadata *WindowsMetadata) arpValues(installation Installation, pathAdded uint32) map[string]windowsRegistryValue {
	quotedScript := `"` + metadata.scriptPath() + `"`
	return map[string]windowsRegistryValue{
		"DisplayName":          {Kind: windowsRegistryString, String: "CQ"},
		"DisplayVersion":       {Kind: windowsRegistryString, String: installation.Version},
		"Publisher":            {Kind: windowsRegistryString, String: "jacobcxdev"},
		"InstallLocation":      {Kind: windowsRegistryString, String: metadata.Root},
		"UninstallString":      {Kind: windowsRegistryString, String: quotedScript},
		"QuietUninstallString": {Kind: windowsRegistryString, String: quotedScript},
		"NoModify":             {Kind: windowsRegistryDWORD, DWORD: 1},
		"NoRepair":             {Kind: windowsRegistryDWORD, DWORD: 1},
		windowsPathAddedValue:  {Kind: windowsRegistryDWORD, DWORD: pathAdded},
	}
}

type windowsMetadataSnapshot struct {
	path         windowsRegistryValue
	pathExists   bool
	arp          map[string]windowsRegistryValue
	arpExists    bool
	helper       []byte
	helperExists bool
	script       []byte
	scriptExists bool
}

func (metadata *WindowsMetadata) snapshot() (windowsMetadataSnapshot, error) {
	var snapshot windowsMetadataSnapshot
	var err error
	snapshot.path, snapshot.pathExists, err = metadata.Registry.Get(windowsEnvironmentKey, "Path")
	if err != nil {
		return snapshot, err
	}
	snapshot.arp, snapshot.arpExists, err = metadata.Registry.Values(windowsUninstallKey)
	if err != nil {
		return snapshot, err
	}
	snapshot.helper, snapshot.helperExists, err = metadata.captureFile(metadata.helperPath())
	if err != nil {
		return snapshot, err
	}
	snapshot.script, snapshot.scriptExists, err = metadata.captureFile(metadata.scriptPath())
	return snapshot, err
}

func (metadata *WindowsMetadata) restore(snapshot windowsMetadataSnapshot) error {
	var result error
	if snapshot.pathExists {
		result = errors.Join(result, metadata.Registry.Set(windowsEnvironmentKey, "Path", snapshot.path))
	} else {
		result = errors.Join(result, metadata.Registry.DeleteValue(windowsEnvironmentKey, "Path"))
	}
	result = errors.Join(result, metadata.Registry.DeleteKey(windowsUninstallKey))
	if snapshot.arpExists {
		for name, value := range snapshot.arp {
			result = errors.Join(result, metadata.Registry.Set(windowsUninstallKey, name, value))
		}
	}
	result = errors.Join(result, metadata.restoreFile(metadata.helperPath(), snapshot.helper, snapshot.helperExists, 0o700))
	result = errors.Join(result, metadata.restoreFile(metadata.scriptPath(), snapshot.script, snapshot.scriptExists, 0o600))
	result = errors.Join(result, metadata.Broadcast())
	return result
}

func (metadata *WindowsMetadata) captureFile(path string) ([]byte, bool, error) {
	exists, err := metadata.Files.Exists(path)
	if err != nil || !exists {
		return nil, false, err
	}
	body, err := metadata.Files.Read(path, maxWindowsInstallerBytes)
	return body, true, err
}

func (metadata *WindowsMetadata) restoreFile(path string, body []byte, exists bool, mode os.FileMode) error {
	if exists {
		return metadata.Files.Write(path, body, mode)
	}
	if err := metadata.Files.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (metadata *WindowsMetadata) helperPath() string {
	return windowsJoin(metadata.Root, "cq-install.exe")
}
func (metadata *WindowsMetadata) scriptPath() string {
	return windowsJoin(metadata.Root, "uninstall.cmd")
}

func WindowsInstallRoot(localAppData string) (string, error) {
	if !cleanAbsoluteTargetPath(localAppData, "windows") {
		return "", fmt.Errorf("LOCALAPPDATA must be a clean absolute Windows path")
	}
	return windowsJoin(windowsJoin(localAppData, "Programs"), "cq"), nil
}

func windowsJoin(directory, name string) string {
	return strings.TrimRight(strings.ReplaceAll(directory, "/", `\`), `\`) + `\` + name
}

func appendWindowsPATH(current, directory string) string {
	if current == "" {
		return directory
	}
	return strings.TrimRight(current, ";") + ";" + directory
}

func removeWindowsPATHEntry(current, directory string) (string, bool) {
	entries := strings.Split(current, ";")
	removeIndex := -1
	for index, entry := range entries {
		if equalWindowsDirectory(entry, directory) {
			removeIndex = index
		}
	}
	if removeIndex < 0 {
		return current, false
	}
	entries = append(entries[:removeIndex], entries[removeIndex+1:]...)
	return strings.Join(entries, ";"), true
}

func renderWindowsUninstallScript(root string) string {
	helper := windowsJoin(root, "cq-install.exe")
	cq := windowsJoin(root, "cq.exe")
	script := windowsJoin(root, "uninstall.cmd")
	lines := []string{
		"@echo off",
		"setlocal",
		`"` + helper + `" uninstall --owner=winget --silent`,
		`set "cq_exit=%ERRORLEVEL%"`,
		`if not "%cq_exit%"=="0" exit /b %cq_exit%`,
		`cd /d "%TEMP%"`,
		`del /f /q "` + cq + `" >nul 2>&1`,
		`del /f /q "` + helper + `" >nul 2>&1`,
		`del /f /q "` + script + `" >nul 2>&1`,
		`rmdir "` + root + `" >nul 2>&1`,
		"exit /b 0",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}
