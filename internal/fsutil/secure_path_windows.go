//go:build windows

package fsutil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

const (
	windowsShareAll            = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	windowsFileAllAccess       = 0x001F01FF
	windowsFileAddFile         = 0x00000002
	windowsFileAddSubdirectory = 0x00000004
	windowsFileDeleteChild     = 0x00000040
)

type windowsSecureFileInfo struct {
	os.FileInfo
	identity SecureFileIdentity
	security windowsSecurityClassification
	reparse  bool
}

type windowsSecurityClassification struct {
	Owner                           SecurePrincipal
	PrivateDACL                     bool
	AncestorSafe                    bool
	ExternalCredentialDirectorySafe bool
	ExternalCredentialSafe          bool
	ExternalCacheSafe               bool
	ExternalImportFileSafe          bool
}

type windowsPathBoundary struct {
	AnchorPath        string
	AnchorIdentity    SecureFileIdentity
	PostAnchorPrivate bool
}

type windowsSecureBoundaryIdentity interface {
	SecureBoundaryIdentity() (SecureFileIdentity, bool)
}

type windowsBasicFileInfo struct {
	name string
	size int64
	mode os.FileMode
	when time.Time
	raw  windows.ByHandleFileInformation
}

type windowsFileIDInfo struct {
	VolumeSerial uint64
	FileID       [16]byte
}

type windowsFileRemoteProtocolInfo struct {
	StructureVersion         uint16
	StructureSize            uint16
	Protocol                 uint32
	ProtocolMajorVersion     uint16
	ProtocolMinorVersion     uint16
	ProtocolRevision         uint16
	Reserved                 uint16
	Flags                    uint32
	GenericReserved          [8]uint32
	ProtocolSpecificReserved [16]uint32
	ProtocolSpecific         [16]uint32
}

var _ [180 - unsafe.Sizeof(windowsFileRemoteProtocolInfo{})]byte
var _ [unsafe.Sizeof(windowsFileRemoteProtocolInfo{}) - 180]byte

type windowsACEHeader struct {
	Type  uint8
	Flags uint8
	Size  uint16
}

type windowsACE struct {
	Header    windowsACEHeader
	Mask      uint32
	Principal SecurePrincipal
}

func (info windowsSecureFileInfo) Mode() os.FileMode {
	mode := info.FileInfo.Mode() & (os.ModeDir | os.ModeType)
	if info.reparse {
		return mode | os.ModeSymlink
	}
	if info.security.PrivateDACL {
		if info.IsDir() {
			return mode | 0o700
		}
		return mode | 0o600
	}
	if info.security.AncestorSafe && info.IsDir() {
		return mode | 0o755
	}
	return mode
}

func (info windowsBasicFileInfo) Name() string       { return info.name }
func (info windowsBasicFileInfo) Size() int64        { return info.size }
func (info windowsBasicFileInfo) Mode() os.FileMode  { return info.mode }
func (info windowsBasicFileInfo) ModTime() time.Time { return info.when }
func (info windowsBasicFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info windowsBasicFileInfo) Sys() any           { return &info.raw }

func statOSFileSystem(fsys OSFileSystem, name string) (os.FileInfo, error) {
	return fsys.Lstat(name)
}

func (fsys OSFileSystem) Lstat(name string) (os.FileInfo, error) {
	selection, err := fsys.resolveSecureBoundary(name, secureBoundaryExternalFile)
	if err != nil {
		return nil, err
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryExternalFile)
	if err != nil {
		return nil, err
	}
	file, err := openWindowsAbsolutePath(name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE, 0, boundary)
	if err != nil {
		return nil, err
	}
	info, statErr := inspectWindowsHandle(windows.Handle(file.Fd()), name)
	return info, errors.Join(statErr, file.Close())
}

func (fsys OSFileSystem) windowsPathBoundary(selection secureBoundarySelection, purpose secureBoundaryPurpose) (windowsPathBoundary, error) {
	boundary := windowsPathBoundary{
		AnchorPath:        selection.AnchorPath,
		PostAnchorPrivate: selection.PostAnchorPrivate,
	}
	if identitySource, ok := fsys.secureBoundaryResolver.(windowsSecureBoundaryIdentity); ok {
		boundary.AnchorIdentity, _ = identitySource.SecureBoundaryIdentity()
	}
	if purpose != secureBoundaryCQPrivate || boundary.AnchorIdentity != (SecureFileIdentity{}) {
		return boundary, nil
	}
	if fsys.secureBoundaryResolver != nil {
		return windowsPathBoundary{}, fmt.Errorf("%w: Windows CQ boundary identity", ErrUnsafeSecurePath)
	}
	anchor, err := openWindowsAbsolutePath(
		selection.AnchorPath,
		windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_DIRECTORY_FILE,
		windowsPathBoundary{AnchorPath: selection.AnchorPath},
	)
	if err != nil {
		return windowsPathBoundary{}, err
	}
	info, inspectErr := inspectWindowsHandle(windows.Handle(anchor.Fd()), selection.AnchorPath)
	closeErr := anchor.Close()
	if inspectErr != nil || closeErr != nil {
		return windowsPathBoundary{}, errors.Join(inspectErr, closeErr)
	}
	identity, ok := OSFileSystem{}.FileIdentity(info)
	if !ok || identity == (SecureFileIdentity{}) {
		return windowsPathBoundary{}, fmt.Errorf("%w: Windows CQ boundary identity", ErrUnsafeSecurePath)
	}
	boundary.AnchorIdentity = identity
	return boundary, nil
}

func (fsys OSFileSystem) resolveSecureBoundary(path string, purpose secureBoundaryPurpose) (secureBoundarySelection, error) {
	if fsys.secureBoundaryResolver != nil {
		return fsys.secureBoundaryResolver.ResolveSecureBoundary(path, purpose)
	}
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return secureBoundarySelection{}, err
	}
	switch purpose {
	case secureBoundaryExternalDirectory:
		return secureBoundarySelection{AnchorPath: clean}, nil
	case secureBoundaryExternalFile:
		return secureBoundarySelection{AnchorPath: filepath.Dir(clean)}, nil
	case secureBoundaryCQPrivate:
		anchors, err := userdirs.WindowsAppDataAnchors()
		if err != nil {
			return secureBoundarySelection{}, fmt.Errorf("resolve Windows CQ boundary: %w", err)
		}
		for _, anchor := range []string{anchors.RoamingAppData, anchors.LocalAppData} {
			if windowsPathWithin(filepath.Join(anchor, "cq"), clean) {
				return secureBoundarySelection{AnchorPath: filepath.Clean(anchor), PostAnchorPrivate: true}, nil
			}
		}
		return secureBoundarySelection{}, fmt.Errorf("%w: path outside Windows CQ roots", ErrUnsafeSecurePath)
	default:
		return secureBoundarySelection{}, ErrSecureCapabilityUnavailable
	}
}

func (OSFileSystem) EffectiveUID() uint64 { return 0 }

func (OSFileSystem) FileOwnerUID(os.FileInfo) (uint64, bool) { return 0, false }

func (OSFileSystem) EffectivePrincipal() (SecurePrincipal, bool) {
	return currentWindowsPrincipal()
}

func (OSFileSystem) FileOwnerPrincipal(info os.FileInfo) (SecurePrincipal, bool) {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || metadata.security.Owner.Kind != SecurePrincipalSID {
		return SecurePrincipal{}, false
	}
	return metadata.security.Owner, true
}

func (OSFileSystem) FileIdentity(info os.FileInfo) (SecureFileIdentity, bool) {
	metadata, ok := info.(windowsSecureFileInfo)
	return metadata.identity, ok
}

func (OSFileSystem) ValidateRetainedAncestor(info os.FileInfo) error {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || !metadata.security.AncestorSafe {
		return fmt.Errorf("%w: retained Windows ancestor", ErrUnsafeSecurePath)
	}
	return nil
}

func (OSFileSystem) ValidateExternalCredentialDirectoryInfo(info os.FileInfo) error {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || !metadata.security.ExternalCredentialDirectorySafe {
		return fmt.Errorf("%w: external Windows credential directory", ErrUnsafeSecurePath)
	}
	return nil
}

func (OSFileSystem) ValidateExternalCredential(info os.FileInfo) error {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || !metadata.security.ExternalCredentialSafe {
		return fmt.Errorf("%w: external Windows credential", ErrUnsafeSecurePath)
	}
	return nil
}

func (OSFileSystem) ValidateExternalCache(info os.FileInfo) error {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || !metadata.security.ExternalCacheSafe {
		return fmt.Errorf("%w: external Windows cache", ErrUnsafeSecurePath)
	}
	return nil
}

func (OSFileSystem) ValidateRetainedExternalImportFileInfo(info os.FileInfo) error {
	metadata, ok := info.(windowsSecureFileInfo)
	if !ok || !metadata.security.ExternalImportFileSafe {
		return fmt.Errorf("%w: external Windows import file", ErrUnsafeSecurePath)
	}
	return nil
}

func currentWindowsUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, fmt.Errorf("%w: current Windows SID", ErrUnsafeSecurePath)
	}
	return user.User.Sid.Copy()
}

func currentWindowsPrincipal() (SecurePrincipal, bool) {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return SecurePrincipal{}, false
	}
	return windowsPrincipal(sid)
}

func windowsPrincipal(sid *windows.SID) (SecurePrincipal, bool) {
	if sid == nil || !sid.IsValid() {
		return SecurePrincipal{}, false
	}
	length := sid.Len()
	if length <= 0 || length > len((SecurePrincipal{}).SID) {
		return SecurePrincipal{}, false
	}
	principal := SecurePrincipal{Kind: SecurePrincipalSID, SIDLength: uint8(length)}
	destination := (*windows.SID)(unsafe.Pointer(&principal.SID[0]))
	if err := windows.CopySid(uint32(length), destination, sid); err != nil {
		return SecurePrincipal{}, false
	}
	return principal, true
}

func classifyWindowsSecurityDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, currentUser *windows.SID) (windowsSecurityClassification, error) {
	if descriptor == nil || currentUser == nil || !currentUser.IsValid() {
		return windowsSecurityClassification{}, fmt.Errorf("%w: Windows security descriptor", ErrUnsafeSecurePath)
	}
	owner, ownerDefaulted, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return windowsSecurityClassification{}, fmt.Errorf("%w: Windows security owner", ErrUnsafeSecurePath)
	}
	ownerPrincipal, ok := windowsPrincipal(owner)
	if !ok {
		return windowsSecurityClassification{}, fmt.Errorf("%w: Windows security owner", ErrUnsafeSecurePath)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 {
		return windowsSecurityClassification{}, fmt.Errorf("%w: Windows DACL control", ErrUnsafeSecurePath)
	}
	dacl, daclDefaulted, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return windowsSecurityClassification{}, fmt.Errorf("%w: Windows DACL", ErrUnsafeSecurePath)
	}
	aces, err := parseWindowsACL(dacl)
	if err != nil {
		return windowsSecurityClassification{}, err
	}
	userPrincipal, ok := windowsPrincipal(currentUser)
	if !ok {
		return windowsSecurityClassification{}, fmt.Errorf("%w: current Windows principal", ErrUnsafeSecurePath)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return windowsSecurityClassification{}, err
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return windowsSecurityClassification{}, err
	}
	systemPrincipal, systemOK := windowsPrincipal(systemSID)
	adminPrincipal, adminOK := windowsPrincipal(adminSID)
	if !systemOK || !adminOK {
		return windowsSecurityClassification{}, fmt.Errorf("%w: trusted Windows principal", ErrUnsafeSecurePath)
	}
	trusted := func(principal SecurePrincipal) bool {
		return principal == userPrincipal || principal == systemPrincipal || principal == adminPrincipal
	}
	ownerCurrent := ownerPrincipal == userPrincipal
	ownerTrusted := trusted(ownerPrincipal)
	mutationMask := uint32(windowsFileAddFile | windowsFileAddSubdirectory | windowsFileDeleteChild |
		windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC |
		windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL)
	credentialMask := uint32(windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_EXECUTE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.GENERIC_ALL)
	mutationSafe := true
	credentialSafe := true
	privateCounts := map[SecurePrincipal]int{}
	privateShape := len(aces) == 3 && !ownerDefaulted && !daclDefaulted && control&windows.SE_DACL_PROTECTED != 0
	for _, ace := range aces {
		if ace.Header.Flags&^uint8(windows.VALID_INHERIT_FLAGS) != 0 {
			return windowsSecurityClassification{}, fmt.Errorf("%w: Windows ACE flags", ErrUnsafeSecurePath)
		}
		if trusted(ace.Principal) {
			privateCounts[ace.Principal]++
		} else {
			if ace.Mask&mutationMask != 0 {
				mutationSafe = false
			}
			if ace.Mask&credentialMask != 0 {
				credentialSafe = false
			}
		}
		if ace.Header.Flags != 0 || ace.Mask != windowsFileAllAccess || !trusted(ace.Principal) {
			privateShape = false
		}
	}
	privateShape = privateShape && ownerCurrent && privateCounts[userPrincipal] == 1 && privateCounts[systemPrincipal] == 1 && privateCounts[adminPrincipal] == 1
	return windowsSecurityClassification{
		Owner:                           ownerPrincipal,
		PrivateDACL:                     privateShape,
		AncestorSafe:                    ownerTrusted && mutationSafe,
		ExternalCredentialDirectorySafe: ownerTrusted && mutationSafe,
		ExternalCredentialSafe:          ownerCurrent && credentialSafe,
		ExternalCacheSafe:               ownerCurrent && mutationSafe,
		ExternalImportFileSafe:          ownerTrusted && mutationSafe,
	}, nil
}

func parseWindowsACL(acl *windows.ACL) ([]windowsACE, error) {
	if acl == nil {
		return nil, fmt.Errorf("%w: null Windows ACL", ErrUnsafeSecurePath)
	}
	header := unsafe.Slice((*byte)(unsafe.Pointer(acl)), 8)
	declared := int(binary.LittleEndian.Uint16(header[2:4]))
	count := int(binary.LittleEndian.Uint16(header[4:6]))
	if declared < len(header) {
		return nil, fmt.Errorf("%w: short Windows ACL", ErrUnsafeSecurePath)
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(acl)), declared)
	aces := make([]windowsACE, 0, count)
	cursor := len(header)
	for range count {
		if cursor > declared-4 {
			return nil, fmt.Errorf("%w: truncated Windows ACE", ErrUnsafeSecurePath)
		}
		aceHeader := windowsACEHeader{Type: data[cursor], Flags: data[cursor+1], Size: binary.LittleEndian.Uint16(data[cursor+2 : cursor+4])}
		size := int(aceHeader.Size)
		if size < 16 || size > declared-cursor || aceHeader.Type != windows.ACCESS_ALLOWED_ACE_TYPE {
			return nil, fmt.Errorf("%w: unsupported Windows ACE", ErrUnsafeSecurePath)
		}
		mask := binary.LittleEndian.Uint32(data[cursor+4 : cursor+8])
		sidBytes := data[cursor+8 : cursor+size]
		if len(sidBytes) < 8 || sidBytes[0] != 1 || sidBytes[1] > 15 {
			return nil, fmt.Errorf("%w: malformed Windows ACE SID", ErrUnsafeSecurePath)
		}
		sidLength := 8 + 4*int(sidBytes[1])
		if sidLength > len(sidBytes) {
			return nil, fmt.Errorf("%w: truncated Windows ACE SID", ErrUnsafeSecurePath)
		}
		sid := (*windows.SID)(unsafe.Pointer(&sidBytes[0]))
		if !sid.IsValid() || sid.Len() != sidLength {
			return nil, fmt.Errorf("%w: invalid Windows ACE SID", ErrUnsafeSecurePath)
		}
		principal, ok := windowsPrincipal(sid)
		if !ok {
			return nil, fmt.Errorf("%w: Windows ACE principal", ErrUnsafeSecurePath)
		}
		aces = append(aces, windowsACE{Header: aceHeader, Mask: mask, Principal: principal})
		cursor += size
	}
	if cursor > declared {
		return nil, fmt.Errorf("%w: Windows ACL bounds", ErrUnsafeSecurePath)
	}
	return aces, nil
}

func validateWindowsPrivateDACL(handle windows.Handle, currentUser *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	classification, err := classifyWindowsSecurityDescriptor(descriptor, currentUser)
	if err != nil {
		return err
	}
	if !classification.PrivateDACL {
		return fmt.Errorf("%w: private Windows DACL", ErrUnsafeSecurePath)
	}
	return nil
}

func inspectWindowsHandle(handle windows.Handle, name string) (os.FileInfo, error) {
	var basic windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &basic); err != nil {
		return nil, err
	}
	reparse := basic.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	if reparse {
		return nil, fmt.Errorf("%w: Windows reparse point", ErrUnsafeSecurePath)
	}
	var fileID windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fileID)), uint32(unsafe.Sizeof(fileID))); err != nil {
		return nil, err
	}
	if fileID.VolumeSerial == 0 || fileID.FileID == ([16]byte{}) || basic.NumberOfLinks == 0 {
		return nil, fmt.Errorf("%w: incomplete Windows file identity", ErrUnsafeSecurePath)
	}
	var volumeName [64]uint16
	var filesystemName [32]uint16
	var serial, maximumComponent, filesystemFlags uint32
	if err := windows.GetVolumeInformationByHandle(handle, &volumeName[0], uint32(len(volumeName)), &serial, &maximumComponent, &filesystemFlags, &filesystemName[0], uint32(len(filesystemName))); err != nil {
		return nil, err
	}
	if !strings.EqualFold(windows.UTF16ToString(filesystemName[:]), "NTFS") {
		return nil, fmt.Errorf("%w: Windows filesystem is not NTFS", ErrUnsafeSecurePath)
	}
	remote := windowsFileRemoteProtocolInfo{StructureVersion: 2, StructureSize: uint16(unsafe.Sizeof(windowsFileRemoteProtocolInfo{}))}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileRemoteProtocolInfo, (*byte)(unsafe.Pointer(&remote)), uint32(unsafe.Sizeof(remote))); err == nil && (remote.Protocol != 0 || remote.Flags != 0) {
		return nil, fmt.Errorf("%w: remote Windows file", ErrUnsafeSecurePath)
	}
	currentUser, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	security, err := classifyWindowsSecurityDescriptor(descriptor, currentUser)
	if err != nil {
		return nil, err
	}
	isDirectory := basic.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory {
		security.ExternalCredentialSafe = false
		security.ExternalCacheSafe = false
		security.ExternalImportFileSafe = false
	} else {
		security.AncestorSafe = false
		security.ExternalCredentialDirectorySafe = false
		if basic.NumberOfLinks != 1 {
			security.ExternalCredentialSafe = false
			security.ExternalCacheSafe = false
			security.ExternalImportFileSafe = false
		}
	}
	mode := os.FileMode(0)
	if isDirectory {
		mode = os.ModeDir
	}
	size := int64(uint64(basic.FileSizeHigh)<<32 | uint64(basic.FileSizeLow))
	base := windowsBasicFileInfo{
		name: filepath.Base(name), size: size, mode: mode,
		when: time.Unix(0, basic.LastWriteTime.Nanoseconds()), raw: basic,
	}
	return windowsSecureFileInfo{
		FileInfo: base,
		identity: SecureFileIdentity{Device: fileID.VolumeSerial, FileID: fileID.FileID, Links: uint64(basic.NumberOfLinks)},
		security: security,
		reparse:  reparse,
	}, nil
}

func openWindowsAbsolutePath(path string, finalAccess, finalOptions uint32, boundary windowsPathBoundary) (*os.File, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	anchor, err := validateWindowsAbsolutePath(boundary.AnchorPath)
	if err != nil || !windowsPathWithin(anchor, clean) {
		return nil, fmt.Errorf("%w: Windows path boundary", ErrUnsafeSecurePath)
	}
	volume := filepath.VolumeName(clean)
	rootPath := volume + string(filepath.Separator)
	rootPointer, err := windows.UTF16PtrFromString(rootPath)
	if err != nil {
		return nil, err
	}
	if windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return nil, fmt.Errorf("%w: Windows drive is not fixed", ErrUnsafeSecurePath)
	}
	rootHandle, err := windows.CreateFile(rootPointer, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE, windowsShareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(rootHandle), rootPath)
	if current == nil {
		windows.CloseHandle(rootHandle)
		return nil, errors.New("wrap Windows drive handle")
	}
	currentInfo, err := inspectWindowsHandle(rootHandle, rootPath)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	rootIdentity := currentInfo.(windowsSecureFileInfo).identity
	targetComponents, err := windowsPathComponents(rootPath, clean)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	anchorComponents, err := windowsPathComponents(rootPath, anchor)
	if err != nil || len(anchorComponents) > len(targetComponents) {
		_ = current.Close()
		return nil, fmt.Errorf("%w: Windows anchor components", ErrUnsafeSecurePath)
	}
	for index, component := range targetComponents {
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
		options := uint32(windows.FILE_DIRECTORY_FILE)
		if index == len(targetComponents)-1 {
			access = finalAccess
			options = finalOptions
		}
		child, openErr := openWindowsRelativeRead(current, component, access, options)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		childPath := filepath.Join(rootPath, filepath.Join(targetComponents[:index+1]...))
		childInfo, inspectErr := inspectWindowsHandle(windows.Handle(child.Fd()), childPath)
		if inspectErr != nil {
			_ = child.Close()
			_ = current.Close()
			return nil, inspectErr
		}
		metadata := childInfo.(windowsSecureFileInfo)
		if metadata.identity.Device != rootIdentity.Device {
			_ = child.Close()
			_ = current.Close()
			return nil, fmt.Errorf("%w: Windows volume changed", ErrUnsafeSecurePath)
		}
		atAnchor := index+1 == len(anchorComponents)
		afterAnchor := index+1 > len(anchorComponents)
		if atAnchor {
			if (boundary.PostAnchorPrivate && !metadata.security.AncestorSafe) ||
				(boundary.AnchorIdentity != (SecureFileIdentity{}) && !SameSecureObject(metadata.identity, boundary.AnchorIdentity)) {
				_ = child.Close()
				_ = current.Close()
				return nil, fmt.Errorf("%w: Windows anchor policy", ErrUnsafeSecurePath)
			}
		}
		if afterAnchor && boundary.PostAnchorPrivate && !metadata.security.PrivateDACL {
			_ = child.Close()
			_ = current.Close()
			return nil, fmt.Errorf("%w: Windows private descendant", ErrUnsafeSecurePath)
		}
		if err := current.Close(); err != nil {
			_ = child.Close()
			return nil, err
		}
		current = child
	}
	return current, nil
}

func openWindowsRelativeRead(parent *os.File, name string, access, options uint32) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") {
		return nil, fmt.Errorf("%w: Windows path component", ErrUnsafeSecurePath)
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, &attributes, &status, nil, 0, windowsShareAll, windows.FILE_OPEN, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, mapWindowsOpenError(err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("wrap relative Windows handle")
	}
	return file, nil
}

func mapWindowsOpenError(err error) error {
	if err == nil {
		return nil
	}
	var status windows.NTStatus
	if !errors.As(err, &status) {
		return err
	}
	switch status {
	case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND:
		return fmt.Errorf("open Windows object: %w", os.ErrNotExist)
	case windows.STATUS_OBJECT_NAME_COLLISION:
		return fmt.Errorf("open Windows object: %w", os.ErrExist)
	case windows.STATUS_REPARSE_POINT_ENCOUNTERED:
		return fmt.Errorf("open Windows object: %w", ErrUnsafeSecurePath)
	default:
		return fmt.Errorf("open Windows object: %w", err)
	}
}

func validateWindowsAbsolutePath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, `\\`) {
		return "", fmt.Errorf("%w: Windows absolute path", ErrUnsafeSecurePath)
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || !isWindowsASCIILetter(volume[0]) || volume[1] != ':' || strings.Contains(path[2:], ":") {
		return "", fmt.Errorf("%w: Windows drive path", ErrUnsafeSecurePath)
	}
	return filepath.Clean(path), nil
}

func isWindowsASCIILetter(value byte) bool {
	return 'A' <= value && value <= 'Z' || 'a' <= value && value <= 'z'
}

func windowsPathWithin(anchor, target string) bool {
	anchor = filepath.Clean(anchor)
	target = filepath.Clean(target)
	if !strings.EqualFold(filepath.VolumeName(anchor), filepath.VolumeName(target)) {
		return false
	}
	relative, err := filepath.Rel(anchor, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func windowsPathComponents(root, path string) ([]string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: Windows path components", ErrUnsafeSecurePath)
	}
	if relative == "." {
		return nil, nil
	}
	return strings.Split(relative, string(filepath.Separator)), nil
}
