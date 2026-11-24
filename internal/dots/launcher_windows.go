//go:build windows

package dots

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

const windowsDotsShortcutName = "ChatGPT via CQ.lnk"

const windowsDotsShortcutScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$path = $env:CQ_DOTS_SHORTCUT_PATH
$target = $env:CQ_DOTS_SHORTCUT_TARGET
$arguments = 'codex dots launch --root "' + $env:CQ_DOTS_SHORTCUT_ROOT + '"'
if (Test-Path -LiteralPath $path) {
    $existing = $shell.CreateShortcut($path)
    if (-not [string]::Equals($existing.TargetPath, $target, [StringComparison]::OrdinalIgnoreCase) -or $existing.Arguments -ne $arguments) { exit 43 }
}
$shortcut = $shell.CreateShortcut($path)
$shortcut.TargetPath = $target
$shortcut.Arguments = $arguments
$shortcut.WorkingDirectory = $env:CQ_DOTS_SHORTCUT_ROOT
$shortcut.IconLocation = $env:CQ_DOTS_SHORTCUT_ICON
$shortcut.Save()
$check = $shell.CreateShortcut($path)
if (-not [string]::Equals($check.TargetPath, $target, [StringComparison]::OrdinalIgnoreCase) -or $check.Arguments -ne $arguments) { exit 44 }
`

func installWindowsDotsLauncher(root, app, executable string) (string, error) {
	anchors, err := userdirs.WindowsAppDataAnchors()
	if err != nil {
		return "", err
	}
	path := filepath.Join(anchors.RoamingAppData, "Microsoft", "Windows", "Start Menu", "Programs", windowsDotsShortcutName)
	return installWindowsDotsLauncherAt(path, root, app, executable)
}

func installWindowsDotsLauncherAt(path, root, app, executable string) (string, error) {
	for _, value := range []string{path, root, app, executable} {
		if !cleanAbsolute(value) {
			return "", errors.New("Windows Dots launcher paths must be clean and absolute")
		}
	}
	if filepath.Base(path) != windowsDotsShortcutName {
		return "", errors.New("Windows Dots launcher filename invalid")
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateOwnerControlledDirectory(fsys, filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := validateWindowsDotsLauncherFile(fsys, executable); err != nil {
		return "", errors.New("Installed CQ executable is not owner controlled")
	}
	if _, err := os.Lstat(path); err == nil {
		if err := validateWindowsDotsLauncherFile(fsys, path); err != nil {
			return "", errors.New("Existing Windows Dots launcher is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	powershell := filepath.Join(system32, "WindowsPowerShell", "v1.0", "powershell.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsDotsShortcutScript)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	reserved := map[string]string{
		"CQ_DOTS_SHORTCUT_PATH":   path,
		"CQ_DOTS_SHORTCUT_ROOT":   root,
		"CQ_DOTS_SHORTCUT_TARGET": executable,
		"CQ_DOTS_SHORTCUT_ICON":   filepath.Join(app, "app", "ChatGPT.exe"),
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		for name := range reserved {
			if strings.EqualFold(key, name) {
				key = ""
				break
			}
		}
		if key != "" {
			command.Env = append(command.Env, entry)
		}
	}
	for key, value := range reserved {
		command.Env = append(command.Env, key+"="+value)
	}
	if err := command.Run(); err != nil {
		if command.ProcessState != nil && command.ProcessState.ExitCode() == 43 {
			return "", errors.New("Windows Dots launcher belongs to another installation")
		}
		return "", errors.New("Windows Dots Start Menu shortcut could not be saved")
	}
	if err := validateWindowsDotsLauncherFile(fsys, path); err != nil {
		return "", errors.New("Windows Dots Start Menu shortcut is unsafe")
	}
	return path, nil
}

func validateWindowsDotsLauncherFile(fsys fsutil.OSFileSystem, path string) error {
	file, err := fsys.OpenNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return fsutil.ValidateExternalCacheFile(fsys, info)
}
