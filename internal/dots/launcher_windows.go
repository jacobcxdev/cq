//go:build windows

package dots

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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

const windowsDotsShortcutStageScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$path = $env:CQ_DOTS_SHORTCUT_STAGE
$target = $env:CQ_DOTS_SHORTCUT_TARGET
$arguments = 'codex dots launch --root "' + $env:CQ_DOTS_SHORTCUT_ROOT + '"'
if (Test-Path -LiteralPath $path) { exit 45 }
try {
    $shortcut = $shell.CreateShortcut($path)
    $shortcut.TargetPath = $target
    $shortcut.Arguments = $arguments
    $shortcut.WorkingDirectory = $env:CQ_DOTS_SHORTCUT_ROOT
    $shortcut.IconLocation = $env:CQ_DOTS_SHORTCUT_ICON
    $shortcut.Save()
    $check = $shell.CreateShortcut($path)
    if (-not [string]::Equals($check.TargetPath, $target, [StringComparison]::OrdinalIgnoreCase) -or $check.Arguments -ne $arguments) { exit 44 }
    $staged = Get-Item -LiteralPath $path -Force
    if ($staged.Length -lt 1 -or $staged.Length -gt 65536 -or ($staged.Attributes -band [IO.FileAttributes]::ReparsePoint)) { exit 46 }
    [Console]::Out.Write([Convert]::ToBase64String([IO.File]::ReadAllBytes($path)))
} finally {
    if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -ErrorAction Stop }
}
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
	existing := false
	if _, err := os.Lstat(path); err == nil {
		if err := validateWindowsDotsLauncherFile(fsys, path); err != nil {
			return "", errors.New("Existing Windows Dots launcher is unsafe")
		}
		existing = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	script := windowsDotsShortcutScript
	stage := ""
	if !existing {
		if err := windowsPrivateDirectory(root); err != nil {
			return "", err
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		stage = filepath.Join(root, ".cq-dots-shortcut-"+hex.EncodeToString(nonce[:])+".lnk")
		script = windowsDotsShortcutStageScript
	}
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	powershell := filepath.Join(system32, "WindowsPowerShell", "v1.0", "powershell.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	reserved := map[string]string{
		"CQ_DOTS_SHORTCUT_PATH":   path,
		"CQ_DOTS_SHORTCUT_ROOT":   root,
		"CQ_DOTS_SHORTCUT_TARGET": executable,
		"CQ_DOTS_SHORTCUT_ICON":   filepath.Join(app, "app", "ChatGPT.exe"),
		"CQ_DOTS_SHORTCUT_STAGE":  stage,
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
	output, err := command.Output()
	if err != nil {
		if command.ProcessState != nil && command.ProcessState.ExitCode() == 43 {
			return "", errors.New("Windows Dots launcher belongs to another installation")
		}
		return "", errors.New("Windows Dots Start Menu shortcut could not be saved")
	}
	if !existing {
		if len(output) > 128<<10 {
			return "", errors.New("Windows Dots shortcut exceeds size limit")
		}
		data, err := base64.StdEncoding.DecodeString(string(output))
		if err != nil || len(data) == 0 || len(data) > 64<<10 {
			return "", errors.New("Windows Dots shortcut could not be decoded")
		}
		directoryPath := filepath.Dir(path)
		directory, err := fsutil.OpenOwnerControlledDirectory(fsys, directoryPath)
		if err != nil {
			return "", err
		}
		defer directory.Close()
		if err := fsutil.SecureAtomicCreateInOwnerControlledDirectory(fsys, directory, directoryPath, filepath.Base(path), data); err != nil {
			return "", err
		}
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
