//go:build windows

package dots

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"github.com/jacobcxdev/cq/internal/winpackage"
	"golang.org/x/sys/windows"
)

const windowsDotsShortcutName = "ChatGPT via CQ.lnk"
const windowsDotsIconName = "ChatGPT.ico"
const windowsDotsLaunchArguments = "codex dots launch --root "

const windowsDotsShortcutStageScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$path = $env:CQ_DOTS_SHORTCUT_STAGE
$target = $env:CQ_DOTS_SHORTCUT_TARGET
$arguments = '` + windowsDotsLaunchArguments + `"' + $env:CQ_DOTS_SHORTCUT_ROOT + '"'
if ((Test-Path -LiteralPath $path) -ne ($env:CQ_DOTS_SHORTCUT_REPAIR -eq '1')) { exit 45 }
$shortcut = $shell.CreateShortcut($path)
if ($env:CQ_DOTS_SHORTCUT_REPAIR -eq '1' -and
    (-not [string]::Equals($shortcut.TargetPath, $target, [StringComparison]::OrdinalIgnoreCase) -or $shortcut.Arguments -cne $arguments)) { exit 43 }
$shortcut.TargetPath = $target
$shortcut.Arguments = $arguments
$shortcut.WorkingDirectory = $env:CQ_DOTS_SHORTCUT_ROOT
$shortcut.IconLocation = $env:CQ_DOTS_SHORTCUT_ICON
$shortcut.Save()
$check = $shell.CreateShortcut($path)
if (-not [string]::Equals($check.TargetPath, $target, [StringComparison]::OrdinalIgnoreCase) -or $check.Arguments -cne $arguments -or $check.IconLocation -ine ($env:CQ_DOTS_SHORTCUT_ICON + ',0')) { exit 44 }
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
	original, originalInfo, err := readWindowsDotsShortcut(fsys, path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	stageFS, err := windowsStateFS(root)
	if err != nil {
		return "", err
	}
	if err := fsutil.ValidateSecureDirectory(stageFS, root); err != nil {
		return "", err
	}
	stageDirectory, err := stageFS.OpenRetainedReadDirectory(root)
	if err != nil {
		return "", err
	}
	defer stageDirectory.Close()
	icon, err := installWindowsDotsIcon(root, app)
	if err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	stageName := ".cq-dots-shortcut-" + hex.EncodeToString(nonce[:]) + ".lnk"
	stage := filepath.Join(root, stageName)
	repair := "0"
	if originalInfo != nil {
		file, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(original)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			_ = os.Remove(stage)
			return "", err
		}
		repair = "1"
	}
	defer os.Remove(stage)
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	powershell := filepath.Join(system32, "WindowsPowerShell", "v1.0", "powershell.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsDotsShortcutStageScript)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	reserved := map[string]string{
		"CQ_DOTS_SHORTCUT_ROOT":   root,
		"CQ_DOTS_SHORTCUT_TARGET": executable,
		"CQ_DOTS_SHORTCUT_ICON":   icon,
		"CQ_DOTS_SHORTCUT_STAGE":  stage,
		"CQ_DOTS_SHORTCUT_REPAIR": repair,
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
	// The elevated shell can stage a link owned by Administrators. Retain the
	// private directory and accept only a mutation-safe, single-link file from it.
	staged, err := stageDirectory.OpenNoFollow(stageName)
	if err != nil {
		return "", fmt.Errorf("open staged Windows Dots shortcut: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	if err := setWindowsDotsShortcutAppID(stage); err != nil {
		return "", err
	}
	staged, err = stageDirectory.OpenNoFollow(stageName)
	if err != nil {
		return "", fmt.Errorf("open staged Windows Dots shortcut after updating its taskbar identity: %w", err)
	}
	data, readErr := readWindowsDotsShortcutData(staged)
	if err := errors.Join(readErr, staged.Close()); err != nil {
		return "", err
	}
	directoryPath := filepath.Dir(path)
	directory, err := fsutil.OpenOwnerControlledDirectory(fsys, directoryPath)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if originalInfo == nil {
		err = fsutil.SecureAtomicCreateInOwnerControlledDirectory(fsys, directory, directoryPath, filepath.Base(path), data)
	} else {
		originalID, ok := fsys.FileIdentity(originalInfo)
		if !ok {
			return "", errors.New("Windows Dots shortcut identity unavailable")
		}
		err = fsutil.SecureAtomicWriteInOwnerControlledDirectoryChecked(fsys, directory, directoryPath, filepath.Base(path), data, func() error {
			current, info, err := readWindowsDotsShortcut(fsys, path)
			currentID, ok := fsys.FileIdentity(info)
			if err != nil || !ok || currentID != originalID || !bytes.Equal(current, original) {
				return errors.New("Windows Dots shortcut changed during installation")
			}
			return nil
		})
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func readWindowsDotsShortcut(fsys fsutil.OSFileSystem, path string) ([]byte, os.FileInfo, error) {
	file, err := fsys.OpenNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if err := fsutil.ValidateExternalCacheFile(fsys, info); err != nil {
		return nil, nil, err
	}
	data, err := readWindowsDotsShortcutData(file)
	return data, info, err
}

func readWindowsDotsShortcutData(file io.Reader) ([]byte, error) {
	const maxShortcutBytes = 64 << 10
	data, err := httputil.ReadBody(io.LimitReader(file, maxShortcutBytes+1))
	if err != nil || len(data) < 1 || len(data) > maxShortcutBytes {
		return nil, errors.New("Windows Dots shortcut could not be read within its size limit")
	}
	return data, nil
}

func installWindowsDotsIcon(root, app string) (string, error) {
	identity, err := windowsAppIdentityAt(app)
	if err != nil {
		return "", err
	}
	logo := filepath.FromSlash(identity.logo)
	if !filepath.IsLocal(logo) || !strings.EqualFold(filepath.Ext(logo), ".png") {
		return "", errors.New("Windows Dots package logo path invalid")
	}
	registered, registeredErr := winpackage.RegisteredCodexRoot()
	useStoreAssetReader := registeredErr == nil && strings.EqualFold(app, registered)
	icon, err := windowsLauncherIcon(filepath.Join(app, logo), func(path string) ([]byte, error) {
		if err := winpackage.CheckPath(path); err != nil {
			return nil, err
		}
		if useStoreAssetReader {
			return readWindowsStoreIconAsset(path)
		}
		file, err := (fsutil.OSFileSystem{}).OpenNoFollow(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return httputil.ReadBody(file)
	})
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, windowsDotsIconName)
	if err := launchWrite(path, icon, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Registered WindowsApps packages use Store callback ACEs. Read package assets
// without the private-state ACL parser after validating the registered path.
func readWindowsStoreIconAsset(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return nil, errors.New("Windows Store icon asset is not a regular file")
	}
	return httputil.ReadBody(file)
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
