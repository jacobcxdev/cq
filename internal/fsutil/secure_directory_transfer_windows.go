//go:build windows

package fsutil

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsSecureDirectoryTransferDomain = "cq/fsutil-secure-directory-transfer/v1"

type SecureDirectoryTransferGrantV1 struct {
	identity SecureFileIdentity
	security [sha256.Size]byte
}

func (grant SecureDirectoryTransferGrantV1) MarshalText() ([]byte, error) {
	if err := grant.validate(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(
		"%s:%016x:%032x:%016x:%016x:%064x",
		windowsSecureDirectoryTransferDomain,
		grant.identity.Device,
		grant.identity.FileID,
		grant.identity.Inode,
		grant.identity.Links,
		grant.security,
	)), nil
}

func (grant *SecureDirectoryTransferGrantV1) UnmarshalText(text []byte) error {
	if grant == nil {
		return fmt.Errorf("%w: nil Windows secure directory transfer grant", ErrUnsafeSecurePath)
	}
	encoded := string(text)
	if encoded != strings.ToLower(encoded) {
		return fmt.Errorf("%w: noncanonical Windows secure directory transfer grant", ErrUnsafeSecurePath)
	}
	parts := strings.Split(encoded, ":")
	if len(parts) != 6 || parts[0] != windowsSecureDirectoryTransferDomain ||
		len(parts[1]) != 16 || len(parts[2]) != 32 || len(parts[3]) != 16 || len(parts[4]) != 16 || len(parts[5]) != 64 {
		return fmt.Errorf("%w: malformed Windows secure directory transfer grant", ErrUnsafeSecurePath)
	}
	device, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return fmt.Errorf("%w: Windows transfer device: %v", ErrUnsafeSecurePath, err)
	}
	fileID, err := hex.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("%w: Windows transfer file ID: %v", ErrUnsafeSecurePath, err)
	}
	inode, err := strconv.ParseUint(parts[3], 16, 64)
	if err != nil {
		return fmt.Errorf("%w: Windows transfer inode: %v", ErrUnsafeSecurePath, err)
	}
	links, err := strconv.ParseUint(parts[4], 16, 64)
	if err != nil {
		return fmt.Errorf("%w: Windows transfer links: %v", ErrUnsafeSecurePath, err)
	}
	security, err := hex.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("%w: Windows transfer security: %v", ErrUnsafeSecurePath, err)
	}
	parsed := SecureDirectoryTransferGrantV1{
		identity: SecureFileIdentity{Device: device, Inode: inode, Links: links},
	}
	copy(parsed.identity.FileID[:], fileID)
	copy(parsed.security[:], security)
	if err := parsed.validate(); err != nil {
		return err
	}
	canonical, err := parsed.MarshalText()
	if err != nil || string(canonical) != encoded {
		return fmt.Errorf("%w: noncanonical Windows secure directory transfer grant", ErrUnsafeSecurePath)
	}
	*grant = parsed
	return nil
}

func (grant SecureDirectoryTransferGrantV1) validate() error {
	if grant.identity.Device == 0 || grant.identity.Inode != 0 || grant.identity.Links == 0 ||
		grant.identity.FileID == ([16]byte{}) || grant.security == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: incomplete Windows secure directory transfer grant", ErrUnsafeSecurePath)
	}
	return nil
}

func DuplicateSecureDirectoryIntoProcess(directory SecureDirectory, targetProcess windows.Handle, targetPID uint32) (windows.Handle, SecureDirectoryTransferGrantV1, error) {
	opened, ok := directory.(*windowsSecureDirectory)
	if !ok || opened == nil || targetProcess == 0 || targetPID == 0 {
		return 0, SecureDirectoryTransferGrantV1{}, ErrSecureCapabilityUnavailable
	}
	actualPID, err := windows.GetProcessId(targetProcess)
	if err != nil {
		return 0, SecureDirectoryTransferGrantV1{}, fmt.Errorf("query Windows transfer target process: %w", err)
	}
	if actualPID != targetPID {
		return 0, SecureDirectoryTransferGrantV1{}, fmt.Errorf("%w: Windows transfer target process", ErrUnsafeSecurePath)
	}
	grant, err := windowsSecureDirectoryTransferGrant(windows.Handle(opened.file.Fd()), opened.file.Name())
	if err != nil {
		return 0, SecureDirectoryTransferGrantV1{}, err
	}
	desiredAccess := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsFileAddFile | windowsFileAddSubdirectory |
		windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	var targetHandle windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(opened.file.Fd()),
		targetProcess,
		&targetHandle,
		desiredAccess,
		false,
		0,
	); err != nil {
		return 0, SecureDirectoryTransferGrantV1{}, fmt.Errorf("duplicate Windows secure directory: %w", err)
	}
	return targetHandle, grant, nil
}

func AdoptTransferredSecureDirectory(handle windows.Handle, expected SecureDirectoryTransferGrantV1) (SecureDirectory, error) {
	if err := expected.validate(); err != nil {
		if handle != 0 && handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
		return nil, err
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return nil, fmt.Errorf("%w: invalid transferred Windows directory handle", ErrUnsafeSecurePath)
	}
	flags, err := windowsHandleInformation(handle)
	if err != nil || flags&windows.HANDLE_FLAG_INHERIT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w: inheritable transferred Windows directory handle: %v", ErrUnsafeSecurePath, err)
	}
	file := os.NewFile(uintptr(handle), "transferred-secure-directory")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap transferred Windows directory handle")
	}
	actual, err := windowsSecureDirectoryTransferGrant(handle, file.Name())
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !SameSecureObject(actual.identity, expected.identity) || subtle.ConstantTimeCompare(actual.security[:], expected.security[:]) != 1 {
		return nil, errors.Join(fmt.Errorf("%w: transferred Windows directory grant mismatch", ErrUnsafeSecurePath), file.Close())
	}
	return &windowsSecureDirectory{file: file, childrenPrivate: true}, nil
}

func windowsSecureDirectoryTransferGrant(handle windows.Handle, name string) (SecureDirectoryTransferGrantV1, error) {
	info, err := inspectWindowsHandle(handle, name)
	if err != nil {
		return SecureDirectoryTransferGrantV1{}, err
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.IsDir() || !metadata.security.PrivateDACL || metadata.identity.Device == 0 ||
		metadata.identity.Inode != 0 || metadata.identity.Links == 0 || metadata.identity.FileID == ([16]byte{}) {
		return SecureDirectoryTransferGrantV1{}, fmt.Errorf("%w: Windows secure directory transfer policy", ErrUnsafeSecurePath)
	}
	security, err := windowsSecureDirectorySecurityDigest(metadata.security.Owner)
	if err != nil {
		return SecureDirectoryTransferGrantV1{}, err
	}
	grant := SecureDirectoryTransferGrantV1{identity: metadata.identity, security: security}
	return grant, grant.validate()
}

func windowsSecureDirectorySecurityDigest(owner SecurePrincipal) ([sha256.Size]byte, error) {
	user, ok := currentWindowsPrincipal()
	if !ok || owner != user {
		return [sha256.Size]byte{}, fmt.Errorf("%w: Windows transfer directory owner", ErrUnsafeSecurePath)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	system, systemOK := windowsPrincipal(systemSID)
	admin, adminOK := windowsPrincipal(adminSID)
	if !systemOK || !adminOK {
		return [sha256.Size]byte{}, fmt.Errorf("%w: Windows transfer trusted principal", ErrUnsafeSecurePath)
	}
	material := []byte("cq/fsutil-secure-directory-security/v1\x00")
	material = append(material, []byte("cq-private-user-system-admin-v1")...)
	material = append(material, 0, 1, 0)
	material = appendWindowsTransferPrincipal(material, owner)
	for _, principal := range []SecurePrincipal{user, system, admin} {
		material = appendWindowsTransferPrincipal(material, principal)
		material = binary.BigEndian.AppendUint32(material, windowsFileAllAccess)
		material = append(material, 0)
	}
	return sha256.Sum256(material), nil
}

func appendWindowsTransferPrincipal(destination []byte, principal SecurePrincipal) []byte {
	destination = append(destination, byte(principal.Kind), principal.SIDLength)
	return append(destination, principal.SID[:principal.SIDLength]...)
}

var getHandleInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")

func windowsHandleInformation(handle windows.Handle) (uint32, error) {
	var flags uint32
	result, _, callErr := getHandleInformation.Call(uintptr(handle), uintptr(unsafe.Pointer(&flags)))
	if result == 0 {
		return 0, callErr
	}
	return flags, nil
}
