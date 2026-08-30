//go:build windows

package userdirs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	maxAppDataPathUnits = 32_768
	maxShellFolderBytes = maxAppDataPathUnits * 2
	userShellFoldersKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	roamingAppDataValue = "AppData"
	localAppDataValue   = "Local AppData"
)

var (
	procExpandEnvironmentStringsForUser = windows.NewLazySystemDLL("userenv.dll").NewProc("ExpandEnvironmentStringsForUserW")
	procGetUserProfileDirectory         = windows.NewLazySystemDLL("userenv.dll").NewProc("GetUserProfileDirectoryW")
)

type registryValueReader interface {
	GetValue(name string, buffer []byte) (n int, valueType uint32, err error)
}

type registryValueKey interface {
	registryValueReader
	Close() error
}

type shellFolderReader func(name string) (value []byte, valueType uint32, err error)
type tokenOpener func() (windows.Token, func() error, error)
type shellFoldersOpener func(windows.Token) (WindowsUserShellFolders, func() error, error)
type appDataAnchorsResolver func(windows.Token, WindowsUserShellFolders) (AppDataAnchors, error)
type tokenUserSIDReader func(windows.Token) (*windows.SID, error)
type registryKeyOpener func(registry.Key, string, uint32) (registryValueKey, error)
type userEnvironmentExpander func(value string) (string, error)
type expandEnvironmentProc func(token windows.Token, source, destination *uint16, size uint32) (bool, error)
type userProfileProc func(token windows.Token, destination *uint16, size *uint32) (bool, error)

func currentUserAppDataAnchors() (AppDataAnchors, error) {
	return currentUserAppDataAnchorsWith(
		openCurrentUserToken,
		openCurrentUserShellFolders,
		ResolveWindowsAppDataForSubject,
	)
}

func currentUserAppDataAnchorsWith(
	openToken tokenOpener,
	openShellFolders shellFoldersOpener,
	resolve appDataAnchorsResolver,
) (AppDataAnchors, error) {
	token, closeToken, err := openToken()
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("open current process token: %w", err)
	}
	shellFolders, closeShellFolders, err := openShellFolders(token)
	if err != nil {
		err = fmt.Errorf("open current-user shell folders: %w", err)
		err = joinCloseError(err, "current process token", closeToken)
		return AppDataAnchors{}, err
	}

	anchors, err := resolve(token, shellFolders)
	err = joinCloseError(err, "current-user shell folders", closeShellFolders)
	err = joinCloseError(err, "current process token", closeToken)
	if err != nil {
		return AppDataAnchors{}, err
	}
	return anchors, nil
}

func openCurrentUserToken() (windows.Token, func() error, error) {
	tokenAccess := uint32(windows.TOKEN_QUERY | windows.TOKEN_IMPERSONATE | windows.TOKEN_DUPLICATE)
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), tokenAccess, &token); err != nil {
		return 0, nil, err
	}
	return token, token.Close, nil
}

type registryUserShellFolders struct {
	key        registryValueKey
	subjectSID *windows.SID
}

func openCurrentUserShellFolders(token windows.Token) (WindowsUserShellFolders, func() error, error) {
	return openCurrentUserShellFoldersWith(token, tokenUserSID, openRegistryValueKey)
}

func openRegistryValueKey(root registry.Key, path string, access uint32) (registryValueKey, error) {
	return registry.OpenKey(root, path, access)
}

func openCurrentUserShellFoldersWith(
	token windows.Token,
	readTokenSID tokenUserSIDReader,
	openKey registryKeyOpener,
) (WindowsUserShellFolders, func() error, error) {
	subjectSID, err := readTokenSID(token)
	if err != nil {
		return nil, nil, fmt.Errorf("read current token user: %w", err)
	}
	if subjectSID == nil || !subjectSID.IsValid() {
		return nil, nil, fmt.Errorf("current token user SID is invalid")
	}
	subjectSID, err = subjectSID.Copy()
	if err != nil {
		return nil, nil, fmt.Errorf("copy current token user SID: %w", err)
	}
	keyPath := subjectSID.String() + `\` + userShellFoldersKey
	key, err := openKey(registry.USERS, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return nil, nil, err
	}
	shellFolders := &registryUserShellFolders{key: key, subjectSID: subjectSID}
	return shellFolders, shellFolders.key.Close, nil
}

func (shellFolders *registryUserShellFolders) GetValue(
	name string,
	buffer []byte,
) (int, uint32, error) {
	return shellFolders.key.GetValue(name, buffer)
}

func (shellFolders *registryUserShellFolders) SubjectUserSID() (*windows.SID, error) {
	if shellFolders.subjectSID == nil || !shellFolders.subjectSID.IsValid() {
		return nil, fmt.Errorf("User Shell Folders subject SID is invalid")
	}
	return shellFolders.subjectSID.Copy()
}

func resolveWindowsAppDataAnchors(
	token windows.Token,
	shellFolders WindowsUserShellFolders,
) (AppDataAnchors, error) {
	return resolveWindowsAppDataAnchorsWith(
		token,
		shellFolders,
		tokenUserSID,
		callExpandEnvironmentStringsForUser,
		callGetUserProfileDirectory,
	)
}

func resolveWindowsAppDataAnchorsWith(
	token windows.Token,
	shellFolders WindowsUserShellFolders,
	readTokenSID tokenUserSIDReader,
	expand expandEnvironmentProc,
	profile userProfileProc,
) (AppDataAnchors, error) {
	if shellFolders == nil {
		return AppDataAnchors{}, fmt.Errorf("Windows User Shell Folders capability is nil")
	}
	tokenSID, err := readTokenSID(token)
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("read Windows token user: %w", err)
	}
	subjectSID, err := shellFolders.SubjectUserSID()
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("read User Shell Folders subject: %w", err)
	}
	if tokenSID == nil || !tokenSID.IsValid() || subjectSID == nil || !subjectSID.IsValid() {
		return AppDataAnchors{}, fmt.Errorf("Windows token or User Shell Folders subject SID is invalid")
	}
	if !windows.EqualSid(tokenSID, subjectSID) {
		return AppDataAnchors{}, fmt.Errorf("Windows token and User Shell Folders subject differ")
	}

	userProfile, err := userProfileWith(token, profile)
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("resolve Windows user profile: %w", err)
	}
	return windowsAppDataAnchorsWith(
		func(name string) ([]byte, uint32, error) {
			return readStableRegistryValue(shellFolders, name)
		},
		func(value string) (string, error) {
			return expandUserEnvironmentWith(token, value, expand)
		},
		userProfile,
	)
}

func tokenUserSID(token windows.Token) (*windows.SID, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, fmt.Errorf("token user SID is invalid")
	}
	return user.User.Sid, nil
}

func joinCloseError(err error, name string, close func() error) error {
	if close == nil {
		return errors.Join(err, fmt.Errorf("close %s: missing close function", name))
	}
	if closeErr := close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close %s: %w", name, closeErr))
	}
	return err
}

func readStableRegistryValue(key registryValueReader, name string) ([]byte, uint32, error) {
	first, firstType, err := readRegistrySnapshot(key, name)
	if err != nil {
		return nil, firstType, err
	}
	second, secondType, err := readRegistrySnapshot(key, name)
	if err != nil {
		return nil, secondType, err
	}
	if firstType != secondType || !bytes.Equal(first, second) {
		return nil, 0, fmt.Errorf("registry value changed while reading")
	}
	return second, secondType, nil
}

func readRegistrySnapshot(key registryValueReader, name string) ([]byte, uint32, error) {
	var valueType uint32
	for attempt := 0; attempt < 2; attempt++ {
		value, gotType, err := readRegistryValue(key, name)
		valueType = gotType
		if errors.Is(err, registry.ErrShortBuffer) {
			continue
		}
		return value, gotType, err
	}
	return nil, valueType, registry.ErrShortBuffer
}

func readRegistryValue(key registryValueReader, name string) ([]byte, uint32, error) {
	size, valueType, err := key.GetValue(name, nil)
	if err != nil {
		return nil, valueType, err
	}
	if size < 2 || size > maxShellFolderBytes || size%2 != 0 {
		return nil, valueType, fmt.Errorf("registry value has invalid byte size %d", size)
	}
	buffer := make([]byte, size)
	read, readType, err := key.GetValue(name, buffer)
	if err != nil {
		return nil, readType, err
	}
	if read != size || readType != valueType {
		return nil, readType, registry.ErrShortBuffer
	}
	return buffer, valueType, nil
}

func callExpandEnvironmentStringsForUser(
	token windows.Token,
	source, destination *uint16,
	size uint32,
) (bool, error) {
	ok, _, callErr := procExpandEnvironmentStringsForUser.Call(
		uintptr(token),
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(destination)),
		uintptr(size),
	)
	return ok != 0, callErr
}

func expandUserEnvironmentWith(token windows.Token, value string, call expandEnvironmentProc) (string, error) {
	sourceUnits, err := windows.UTF16FromString(value)
	if err != nil {
		return "", fmt.Errorf("encode user environment value: %w", err)
	}
	if len(sourceUnits) > maxAppDataPathUnits {
		return "", fmt.Errorf("user environment value exceeds the Windows path limit")
	}
	buffer := make([]uint16, maxAppDataPathUnits)
	ok, callErr := call(token, &sourceUnits[0], &buffer[0], uint32(len(buffer)))
	if !ok {
		return "", callFailure("ExpandEnvironmentStringsForUserW", callErr)
	}
	return decodeTerminatedUTF16(buffer)
}

func callGetUserProfileDirectory(token windows.Token, destination *uint16, size *uint32) (bool, error) {
	ok, _, callErr := procGetUserProfileDirectory.Call(
		uintptr(token),
		uintptr(unsafe.Pointer(destination)),
		uintptr(unsafe.Pointer(size)),
	)
	return ok != 0, callErr
}

func userProfileWith(token windows.Token, call userProfileProc) (string, error) {
	var size uint32
	ok, callErr := call(token, nil, &size)
	if ok {
		return "", fmt.Errorf("GetUserProfileDirectoryW size query unexpectedly succeeded")
	}
	if !errors.Is(callErr, windows.ERROR_INSUFFICIENT_BUFFER) {
		return "", callFailure("GetUserProfileDirectoryW size query", callErr)
	}
	if size == 0 || size > maxAppDataPathUnits {
		return "", fmt.Errorf("GetUserProfileDirectoryW returned invalid size %d", size)
	}

	buffer := make([]uint16, size)
	returned := size
	ok, callErr = call(token, &buffer[0], &returned)
	if !ok {
		return "", callFailure("GetUserProfileDirectoryW", callErr)
	}
	if returned == 0 || returned > size {
		return "", fmt.Errorf("GetUserProfileDirectoryW returned invalid count %d for buffer size %d", returned, size)
	}
	return decodeExactUTF16(buffer[:returned])
}

func callFailure(name string, err error) error {
	if err == nil {
		return fmt.Errorf("%s returned false without an error", name)
	}
	return fmt.Errorf("%s failed: %w", name, err)
}

func appDataPathWith(
	name string,
	read shellFolderReader,
	expand userEnvironmentExpander,
) (string, error) {
	raw, valueType, err := read(name)
	if err != nil {
		return "", fmt.Errorf("read User Shell Folders %q: %w", name, err)
	}
	value, err := decodeRegistryUTF16(raw)
	if err != nil {
		return "", fmt.Errorf("decode User Shell Folders %q: %w", name, err)
	}

	path := value
	switch valueType {
	case registry.SZ:
	case registry.EXPAND_SZ:
		path, err = expand(value)
		if err != nil {
			return "", fmt.Errorf("expand User Shell Folders %q: %w", name, err)
		}
	default:
		return "", fmt.Errorf("User Shell Folders %q has unsupported registry type %d", name, valueType)
	}

	if err := validateWindowsPath(name, path); err != nil {
		return "", err
	}
	return path, nil
}

func validateWindowsPath(name, path string) error {
	if path == "" {
		return fmt.Errorf("User Shell Folders %q is empty", name)
	}
	units, err := windows.UTF16FromString(path)
	if err != nil {
		return fmt.Errorf("User Shell Folders %q is not NUL-free: %w", name, err)
	}
	if len(units) > maxAppDataPathUnits {
		return fmt.Errorf("User Shell Folders %q exceeds the Windows path limit", name)
	}
	if err := validateWindowsLocalAbsolutePath("Windows path "+name, path); err != nil {
		return err
	}
	return nil
}

func decodeRegistryUTF16(data []byte) (string, error) {
	if len(data) < 2 || len(data)%2 != 0 || len(data) > maxShellFolderBytes {
		return "", fmt.Errorf("invalid UTF-16 byte length")
	}
	units := make([]uint16, len(data)/2)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(data[index*2:])
	}
	return decodeExactUTF16(units)
}

func decodeExactUTF16(units []uint16) (string, error) {
	if len(units) == 0 || units[len(units)-1] != 0 {
		return "", fmt.Errorf("UTF-16 value is not NUL-terminated")
	}
	for _, unit := range units[:len(units)-1] {
		if unit == 0 {
			return "", fmt.Errorf("UTF-16 value contains an embedded NUL")
		}
	}
	return decodeStrictUTF16(units[:len(units)-1])
}

func decodeTerminatedUTF16(units []uint16) (string, error) {
	for index, unit := range units {
		if unit == 0 {
			if index == 0 {
				return "", fmt.Errorf("UTF-16 value is empty")
			}
			return decodeStrictUTF16(units[:index])
		}
	}
	return "", fmt.Errorf("UTF-16 value is not NUL-terminated")
}

func decodeStrictUTF16(units []uint16) (string, error) {
	decoded := make([]rune, 0, len(units))
	for index := 0; index < len(units); index++ {
		unit := units[index]
		switch {
		case 0xd800 <= unit && unit <= 0xdbff:
			if index+1 >= len(units) {
				return "", fmt.Errorf("invalid UTF-16 surrogate pair")
			}
			next := units[index+1]
			if next < 0xdc00 || next > 0xdfff {
				return "", fmt.Errorf("invalid UTF-16 surrogate pair")
			}
			decoded = append(decoded, utf16.DecodeRune(rune(unit), rune(next)))
			index++
		case 0xdc00 <= unit && unit <= 0xdfff:
			return "", fmt.Errorf("invalid UTF-16 surrogate pair")
		default:
			decoded = append(decoded, rune(unit))
		}
	}
	return string(decoded), nil
}

func windowsAppDataAnchorsWith(
	read shellFolderReader,
	expand userEnvironmentExpander,
	userProfile string,
) (AppDataAnchors, error) {
	if err := validateWindowsPath("UserProfile", userProfile); err != nil {
		return AppDataAnchors{}, fmt.Errorf("resolve Windows user profile: %w", err)
	}
	roaming, err := appDataPathWith(roamingAppDataValue, read, expand)
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("resolve Windows roaming data: %w", err)
	}
	local, err := appDataPathWith(localAppDataValue, read, expand)
	if err != nil {
		return AppDataAnchors{}, fmt.Errorf("resolve Windows local data: %w", err)
	}
	return AppDataAnchors{RoamingAppData: roaming, LocalAppData: local, UserProfile: userProfile}, nil
}
