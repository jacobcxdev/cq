//go:build windows

package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	hwndBroadcast       = 0xffff
	wmSettingChange     = 0x001a
	smtoAbortIfHung     = 0x0002
	broadcastTimeoutMS  = 5000
	maxMetadataFileSize = maxWindowsInstallerBytes
)

var sendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

// DefaultGoDestinationResolver resolves the current user's Go binary path.
func DefaultGoDestinationResolver() GoDestinationResolver {
	return GoDestinationResolver{
		GOOS:        runtime.GOOS,
		Getenv:      os.Getenv,
		GoEnvGOPATH: queryGoEnvGOPATH,
		Stat:        os.Stat,
		Writable:    writableGoDestination,
	}
}

func queryGoEnvGOPATH(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "go", "env", "GOPATH").Output()
	if err != nil {
		return "", err
	}
	if len(output) > 64<<10 {
		return "", fmt.Errorf("go env GOPATH output exceeds size limit")
	}
	return strings.TrimSpace(string(output)), nil
}

func writableGoDestination(directory string) (resultErr error) {
	file, err := os.CreateTemp(directory, ".cq-write-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		resultErr = errors.Join(resultErr, os.Remove(path))
	}()
	return nil
}

// NewWindowsMetadata uses current-user registry and filesystem authorities.
func NewWindowsMetadata(root, sourceInstaller string) *WindowsMetadata {
	return &WindowsMetadata{
		Root:            root,
		SourceInstaller: sourceInstaller,
		Registry:        windowsUserRegistry{},
		Files:           windowsOSMetadataFiles{FS: fsutil.OSFileSystem{}},
		Broadcast:       broadcastWindowsEnvironment,
	}
}

type windowsUserRegistry struct{}

func (windowsUserRegistry) Get(keyPath, name string) (windowsRegistryValue, bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return windowsRegistryValue{}, false, nil
	}
	if err != nil {
		return windowsRegistryValue{}, false, err
	}
	defer key.Close()
	return readWindowsRegistryValue(key, name)
}

func (windowsUserRegistry) Values(keyPath string) (map[string]windowsRegistryValue, bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer key.Close()
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, false, err
	}
	values := make(map[string]windowsRegistryValue, len(names))
	for _, name := range names {
		value, exists, err := readWindowsRegistryValue(key, name)
		if err != nil {
			return nil, false, err
		}
		if exists {
			values[name] = value
		}
	}
	return values, true, nil
}

func readWindowsRegistryValue(key registry.Key, name string) (windowsRegistryValue, bool, error) {
	value, kind, err := key.GetStringValue(name)
	if err == nil {
		switch kind {
		case registry.SZ:
			return windowsRegistryValue{Kind: windowsRegistryString, String: value}, true, nil
		case registry.EXPAND_SZ:
			return windowsRegistryValue{Kind: windowsRegistryExpandString, String: value}, true, nil
		default:
			return windowsRegistryValue{}, false, fmt.Errorf("unsupported registry string type %d", kind)
		}
	}
	if errors.Is(err, registry.ErrNotExist) {
		return windowsRegistryValue{}, false, nil
	}
	integer, kind, integerErr := key.GetIntegerValue(name)
	if integerErr == nil && kind == registry.DWORD && integer <= uint64(^uint32(0)) {
		return windowsRegistryValue{Kind: windowsRegistryDWORD, DWORD: uint32(integer)}, true, nil
	}
	if integerErr != nil && !errors.Is(integerErr, registry.ErrUnexpectedType) {
		return windowsRegistryValue{}, false, integerErr
	}
	return windowsRegistryValue{}, false, fmt.Errorf("unsupported registry value %s", name)
}

func (windowsUserRegistry) Set(keyPath, name string, value windowsRegistryValue) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	switch value.Kind {
	case windowsRegistryString:
		return key.SetStringValue(name, value.String)
	case windowsRegistryExpandString:
		return key.SetExpandStringValue(name, value.String)
	case windowsRegistryDWORD:
		return key.SetDWordValue(name, value.DWORD)
	default:
		return fmt.Errorf("unsupported registry value kind %d", value.Kind)
	}
}

func (windowsUserRegistry) DeleteValue(keyPath, name string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func (windowsUserRegistry) DeleteKey(keyPath string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, readErr := key.ReadValueNames(-1)
	if readErr == nil {
		for _, name := range names {
			readErr = errors.Join(readErr, key.DeleteValue(name))
		}
	}
	closeErr := key.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, keyPath); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

type windowsOSMetadataFiles struct {
	FS fsutil.OSFileSystem
}

func (files windowsOSMetadataFiles) Read(path string, limit int64) ([]byte, error) {
	if limit <= 0 || limit > maxMetadataFileSize {
		return nil, fmt.Errorf("invalid Windows metadata file limit")
	}
	file, err := files.FS.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("Windows metadata source is not a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != info.Size() || int64(len(body)) > limit {
		return nil, fmt.Errorf("Windows metadata source changed while reading")
	}
	return body, nil
}

func (files windowsOSMetadataFiles) Write(path string, body []byte, mode os.FileMode) error {
	if err := fsutil.SecureAtomicWrite(files.FS, path, body); err != nil {
		return err
	}
	if err := files.FS.Chmod(path, mode); err != nil {
		return err
	}
	if err := files.FS.SyncFile(path); err != nil {
		return err
	}
	return files.FS.SyncDir(filepath.Dir(path))
}

func (files windowsOSMetadataFiles) Remove(path string) error {
	return files.FS.Remove(path)
}

func (files windowsOSMetadataFiles) MkdirAll(path string, mode os.FileMode) error {
	return files.FS.MkdirAll(path, mode)
}

func (files windowsOSMetadataFiles) Exists(path string) (bool, error) {
	info, err := files.FS.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("Windows metadata path is not a regular file")
	}
	return true, nil
}

func broadcastWindowsEnvironment() error {
	environment, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return err
	}
	var result uintptr
	returnValue, _, callErr := sendMessageTimeout.Call(
		hwndBroadcast,
		wmSettingChange,
		0,
		uintptr(unsafe.Pointer(environment)),
		smtoAbortIfHung,
		broadcastTimeoutMS,
		uintptr(unsafe.Pointer(&result)),
	)
	if returnValue == 0 {
		return fmt.Errorf("broadcast Windows environment update: %w", callErr)
	}
	return nil
}
