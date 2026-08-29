//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsFileOpened  = 1
	windowsFileCreated = 2
)

type windowsOpenResult struct {
	file        *os.File
	information uintptr
}

type windowsRelativeOpenFunc func(windows.Handle, string, uint32, uint32, uint32, uint32, *windows.SECURITY_DESCRIPTOR) (windowsOpenResult, error)

type windowsSecureDirectory struct {
	file            *os.File
	childrenPrivate bool
	mutex           sync.Mutex
	closed          bool
	closeErr        error
}

type windowsRetainedReadDirectory struct {
	file     *os.File
	mutex    sync.Mutex
	closed   bool
	closeErr error
}

type windowsSecureFile struct {
	file *os.File
}

type windowsRetainedRegularFile struct {
	file      *os.File
	ancestors []*os.File
	closeFile func(*os.File) error
	mutex     sync.Mutex
	closed    bool
	closeErr  error
}

type windowsFileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type windowsFileDispositionInfo struct {
	DeleteFile uint8
}

var _ [1 - unsafe.Sizeof(windowsFileDispositionInfo{})]byte
var _ [unsafe.Sizeof(windowsFileDispositionInfo{}) - 1]byte
var _ [8 - unsafe.Offsetof(windowsFileRenameInfo{}.RootDirectory)]byte
var _ [unsafe.Offsetof(windowsFileRenameInfo{}.RootDirectory) - 8]byte
var _ [16 - unsafe.Offsetof(windowsFileRenameInfo{}.FileNameLength)]byte
var _ [unsafe.Offsetof(windowsFileRenameInfo{}.FileNameLength) - 16]byte
var _ [20 - unsafe.Offsetof(windowsFileRenameInfo{}.FileName)]byte
var _ [unsafe.Offsetof(windowsFileRenameInfo{}.FileName) - 20]byte
var _ [24 - unsafe.Sizeof(windowsFileRenameInfo{})]byte
var _ [unsafe.Sizeof(windowsFileRenameInfo{}) - 24]byte

func (fsys OSFileSystem) OpenDurableDirectory(name string) (DurableDirectory, error) {
	selection, err := fsys.resolveSecureBoundary(name, secureBoundaryCQPrivate)
	if err != nil {
		return nil, err
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryCQPrivate)
	if err != nil {
		return nil, err
	}
	file, err := openWindowsAbsolutePath(
		name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_DIRECTORY_FILE,
		boundary,
	)
	if err != nil {
		return nil, err
	}
	return &windowsSecureDirectory{file: file, childrenPrivate: boundary.PostAnchorPrivate}, nil
}

func (fsys OSFileSystem) openSecureDirectoryAncestor(name string) (DurableDirectory, error) {
	return fsys.OpenDurableDirectory(name)
}

func (fsys OSFileSystem) OpenSecureDirectory(name string) (SecureDirectory, error) {
	directory, err := fsys.OpenDurableDirectory(name)
	if err != nil {
		return nil, err
	}
	opened := directory.(*windowsSecureDirectory)
	if err := validateSecureDirectoryDescriptor(fsys, opened); err != nil {
		_ = opened.Close()
		return nil, err
	}
	return opened, nil
}

func (fsys OSFileSystem) OpenRetainedReadDirectory(name string) (RetainedReadDirectory, error) {
	selection, err := fsys.resolveSecureBoundary(name, secureBoundaryExternalDirectory)
	if err != nil {
		return nil, err
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryExternalDirectory)
	if err != nil {
		return nil, err
	}
	file, err := openWindowsAbsolutePath(
		name,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_DIRECTORY_FILE,
		boundary,
	)
	if err != nil {
		return nil, err
	}
	info, err := inspectWindowsHandle(windows.Handle(file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.IsDir() || !metadata.security.AncestorSafe {
		return nil, errors.Join(fmt.Errorf("%w: Windows retained read directory policy", ErrUnsafeSecurePath), file.Close())
	}
	return &windowsRetainedReadDirectory{file: file}, nil
}

func (fsys OSFileSystem) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	directory, err := fsys.OpenSecureDirectory(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.CreateExclusive(filepath.Base(name), mode)
}

func (fsys OSFileSystem) OpenNoFollow(name string) (SecureReadFile, error) {
	directory, err := fsys.OpenSecureDirectory(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.OpenNoFollow(filepath.Base(name))
}

func (fsys OSFileSystem) OpenRetainedRegularFileNoFollow(path string, policy RetainedRegularFilePolicy) (RetainedRegularFile, error) {
	if policy != RetainedRegularFileReadOnly && policy != RetainedRegularFileExecutableDenyReplacement {
		return nil, fmt.Errorf("%w: invalid retained regular file policy", ErrUnsafeSecurePath)
	}
	selection, err := fsys.resolveSecureBoundary(path, secureBoundaryExternalFile)
	if err != nil {
		return nil, err
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryExternalFile)
	if err != nil {
		return nil, err
	}
	if policy == RetainedRegularFileExecutableDenyReplacement {
		return openWindowsRetainedExecutable(path, boundary)
	}
	file, err := openWindowsAbsolutePath(
		path,
		windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_NON_DIRECTORY_FILE,
		boundary,
	)
	if err != nil {
		return nil, err
	}
	info, err := inspectWindowsHandle(windows.Handle(file.Fd()), path)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || metadata.identity.Links != 1 || !metadata.security.ExternalImportFileSafe {
		return nil, errors.Join(fmt.Errorf("%w: Windows retained regular file policy", ErrUnsafeSecurePath), file.Close())
	}
	return &windowsRetainedRegularFile{file: file}, nil
}

func openWindowsRetainedExecutable(path string, boundary windowsPathBoundary) (RetainedRegularFile, error) {
	return openWindowsRetainedExecutableWith(path, boundary, openWindowsRelative)
}

func openWindowsRetainedExecutableWith(path string, boundary windowsPathBoundary, openRelative windowsRelativeOpenFunc) (RetainedRegularFile, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	anchor, err := validateWindowsAbsolutePath(boundary.AnchorPath)
	if err != nil || !windowsPathWithin(anchor, clean) {
		return nil, fmt.Errorf("%w: Windows retained executable boundary", ErrUnsafeSecurePath)
	}
	volume := filepath.VolumeName(clean)
	rootPath := volume + string(filepath.Separator)
	rootPointer, err := windows.UTF16PtrFromString(rootPath)
	if err != nil {
		return nil, err
	}
	if windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return nil, fmt.Errorf("%w: Windows retained executable drive", ErrUnsafeSecurePath)
	}
	rootHandle, err := windows.CreateFile(
		rootPointer,
		windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(rootHandle), rootPath)
	if current == nil {
		_ = windows.CloseHandle(rootHandle)
		return nil, errors.New("wrap Windows retained executable root")
	}
	currentHeld := false
	held := make([]*os.File, 0)
	closeFailure := func(cause error) error {
		errorsByHandle := []error{cause}
		if !currentHeld && current != nil {
			errorsByHandle = append(errorsByHandle, current.Close())
		}
		for index := len(held) - 1; index >= 0; index-- {
			errorsByHandle = append(errorsByHandle, held[index].Close())
		}
		return errors.Join(errorsByHandle...)
	}
	rootInfo, err := inspectWindowsHandle(rootHandle, rootPath)
	if err != nil {
		return nil, closeFailure(err)
	}
	rootIdentity := rootInfo.(windowsSecureFileInfo).identity
	targetComponents, err := windowsPathComponents(rootPath, clean)
	if err != nil {
		return nil, closeFailure(err)
	}
	anchorComponents, err := windowsPathComponents(rootPath, anchor)
	if err != nil || len(anchorComponents) >= len(targetComponents) {
		return nil, closeFailure(fmt.Errorf("%w: Windows retained executable components", ErrUnsafeSecurePath))
	}
	for index, component := range targetComponents {
		final := index == len(targetComponents)-1
		atOrAfterAnchor := index+1 >= len(anchorComponents)
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
		share := uint32(windowsShareAll)
		options := uint32(windows.FILE_DIRECTORY_FILE)
		if atOrAfterAnchor {
			access = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
			share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
		}
		if final {
			access = windows.FILE_READ_DATA | windows.FILE_EXECUTE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
			share = windows.FILE_SHARE_READ
			options = windows.FILE_NON_DIRECTORY_FILE
		}
		result, openErr := openRelative(
			windows.Handle(current.Fd()),
			component,
			access,
			share,
			windows.FILE_OPEN,
			options,
			nil,
		)
		if openErr != nil {
			return nil, closeFailure(openErr)
		}
		if result.information != windowsFileOpened {
			closeErr := result.file.Close()
			return nil, closeFailure(errors.Join(fmt.Errorf("%w: unexpected Windows retained executable open result", ErrUnsafeSecurePath), closeErr))
		}
		componentPath := filepath.Join(rootPath, filepath.Join(targetComponents[:index+1]...))
		info, inspectErr := inspectWindowsHandle(windows.Handle(result.file.Fd()), componentPath)
		if inspectErr != nil {
			closeErr := result.file.Close()
			return nil, closeFailure(errors.Join(inspectErr, closeErr))
		}
		metadata := info.(windowsSecureFileInfo)
		if metadata.identity.Device != rootIdentity.Device {
			closeErr := result.file.Close()
			return nil, closeFailure(errors.Join(fmt.Errorf("%w: Windows retained executable volume changed", ErrUnsafeSecurePath), closeErr))
		}
		atAnchor := index+1 == len(anchorComponents)
		if atAnchor && boundary.AnchorIdentity != (SecureFileIdentity{}) && !SameSecureObject(metadata.identity, boundary.AnchorIdentity) {
			closeErr := result.file.Close()
			return nil, closeFailure(errors.Join(fmt.Errorf("%w: Windows retained executable anchor identity", ErrUnsafeSecurePath), closeErr))
		}
		if final {
			if !metadata.Mode().IsRegular() || metadata.identity.Links != 1 || !metadata.security.ExternalImportFileSafe ||
				(boundary.PostAnchorPrivate && !metadata.security.PrivateDACL) {
				closeErr := result.file.Close()
				return nil, closeFailure(errors.Join(fmt.Errorf("%w: Windows retained executable file policy", ErrUnsafeSecurePath), closeErr))
			}
			return &windowsRetainedRegularFile{file: result.file, ancestors: held}, nil
		}
		if atOrAfterAnchor && (!metadata.IsDir() || !metadata.security.AncestorSafe ||
			(boundary.PostAnchorPrivate && !atAnchor && !metadata.security.PrivateDACL)) {
			closeErr := result.file.Close()
			return nil, closeFailure(errors.Join(fmt.Errorf("%w: Windows retained executable ancestor policy", ErrUnsafeSecurePath), closeErr))
		}
		if !currentHeld {
			if err := current.Close(); err != nil {
				_ = result.file.Close()
				return nil, closeFailure(err)
			}
		}
		current = result.file
		currentHeld = atOrAfterAnchor
		if currentHeld {
			held = append(held, current)
		}
	}
	return nil, closeFailure(fmt.Errorf("%w: incomplete Windows retained executable open", ErrUnsafeSecurePath))
}

func (directory *windowsSecureDirectory) Stat() (os.FileInfo, error) {
	return inspectWindowsHandle(windows.Handle(directory.file.Fd()), directory.file.Name())
}

func (directory *windowsSecureDirectory) ReadDir() ([]os.DirEntry, error) {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	if directory.closed {
		return nil, os.ErrClosed
	}
	if _, err := directory.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return directory.file.ReadDir(-1)
}

func (directory *windowsSecureDirectory) OpenDirectory(name string) (DurableDirectory, error) {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileOpened {
		return nil, errors.Join(fmt.Errorf("%w: unexpected Windows directory open result", ErrUnsafeSecurePath), result.file.Close())
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, result.file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if directory.childrenPrivate && !metadata.security.PrivateDACL {
		return nil, errors.Join(fmt.Errorf("%w: Windows private child directory", ErrUnsafeSecurePath), result.file.Close())
	}
	if !directory.childrenPrivate && !metadata.security.AncestorSafe {
		return nil, errors.Join(fmt.Errorf("%w: Windows child directory authority", ErrUnsafeSecurePath), result.file.Close())
	}
	return &windowsSecureDirectory{file: result.file, childrenPrivate: directory.childrenPrivate}, nil
}

func (directory *windowsSecureDirectory) Mkdir(name string, _ os.FileMode) error {
	descriptor, err := windowsPrivateSecurityDescriptor()
	if err != nil {
		return err
	}
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|windows.DELETE|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE,
		descriptor,
	)
	if err != nil {
		return err
	}
	if result.information != windowsFileCreated {
		return directory.cleanupCreated(result.file, fmt.Errorf("%w: unexpected Windows directory create result", ErrUnsafeSecurePath))
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return directory.cleanupCreated(result.file, err)
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.IsDir() || !metadata.security.PrivateDACL {
		return directory.cleanupCreated(result.file, fmt.Errorf("%w: created Windows directory policy", ErrUnsafeSecurePath))
	}
	if err := result.file.Close(); err != nil {
		return fmt.Errorf("%w: close created Windows directory: %v", ErrCommitIndeterminate, err)
	}
	return nil
}

func (directory *windowsSecureDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileOpened {
		return nil, errors.Join(fmt.Errorf("%w: unexpected Windows file open result", ErrUnsafeSecurePath), result.file.Close())
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, result.file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || !metadata.security.PrivateDACL || metadata.identity.Links != 1 {
		return nil, errors.Join(fmt.Errorf("%w: Windows secure file policy", ErrUnsafeSecurePath), result.file.Close())
	}
	return &windowsSecureFile{file: result.file}, nil
}

func (directory *windowsSecureDirectory) CreateExclusive(name string, _ os.FileMode) (DurableFile, error) {
	descriptor, err := windowsPrivateSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|windows.DELETE|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE,
		descriptor,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileCreated {
		return nil, directory.cleanupCreated(result.file, fmt.Errorf("%w: unexpected Windows file create result", ErrUnsafeSecurePath))
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, directory.cleanupCreated(result.file, err)
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || !metadata.security.PrivateDACL || metadata.identity.Links != 1 {
		return nil, directory.cleanupCreated(result.file, fmt.Errorf("%w: created Windows file policy", ErrUnsafeSecurePath))
	}
	return &windowsSecureFile{file: result.file}, nil
}

func (directory *windowsSecureDirectory) Rename(oldName, newName string) error {
	return directory.rename(oldName, newName, SecureFileIdentity{}, false, false)
}

func (directory *windowsSecureDirectory) RenameNoReplace(oldName, newName string) error {
	return directory.rename(oldName, newName, SecureFileIdentity{}, true, false)
}

func (directory *windowsSecureDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.rename(oldName, newName, expected, false, true)
}

func (directory *windowsSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.rename(oldName, newName, expected, true, true)
}

func (directory *windowsSecureDirectory) rename(oldName, newName string, expected SecureFileIdentity, noReplace, checked bool) error {
	if err := validateWindowsSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateWindowsSecureEntryName(newName); err != nil {
		return err
	}
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		oldName,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return err
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), oldName)
	if err != nil {
		return errors.Join(err, result.file.Close())
	}
	identity := info.(windowsSecureFileInfo).identity
	if checked && !SameSecureObject(identity, expected) {
		return errors.Join(fmt.Errorf("%w: checked Windows rename source identity", ErrUnsafeSecurePath), result.file.Close())
	}
	if err := renameWindowsHandle(windows.Handle(result.file.Fd()), windows.Handle(directory.file.Fd()), newName, !noReplace); err != nil {
		return errors.Join(err, result.file.Close())
	}
	postInfo, postErr := inspectWindowsHandle(windows.Handle(result.file.Fd()), oldName)
	closeErr := result.file.Close()
	if postErr != nil || closeErr != nil {
		return fmt.Errorf("%w: verify committed Windows rename: %v", ErrCommitIndeterminate, errors.Join(postErr, closeErr))
	}
	postIdentity := postInfo.(windowsSecureFileInfo).identity
	if !SameSecureObject(identity, postIdentity) {
		return fmt.Errorf("%w: renamed Windows source identity changed", ErrCommitIndeterminate)
	}
	destination, err := directory.OpenNoFollow(newName)
	if err != nil {
		return fmt.Errorf("%w: reopen renamed Windows destination: %v", ErrCommitIndeterminate, err)
	}
	destinationInfo, statErr := destination.Stat()
	destinationCloseErr := destination.Close()
	if statErr != nil || destinationCloseErr != nil {
		return fmt.Errorf("%w: verify renamed Windows destination: %v", ErrCommitIndeterminate, errors.Join(statErr, destinationCloseErr))
	}
	destinationIdentity, ok := OSFileSystem{}.FileIdentity(destinationInfo)
	if !ok || !SameSecureObject(identity, destinationIdentity) {
		return fmt.Errorf("%w: renamed Windows destination identity", ErrCommitIndeterminate)
	}
	return nil
}

func (directory *windowsSecureDirectory) Remove(name string) error {
	return directory.remove(name, SecureFileIdentity{}, false)
}

func (directory *windowsSecureDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.remove(name, expected, true)
}

func (directory *windowsSecureDirectory) remove(name string, expected SecureFileIdentity, checked bool) error {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return err
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return errors.Join(err, result.file.Close())
	}
	identity := info.(windowsSecureFileInfo).identity
	if checked && !SameSecureObject(identity, expected) {
		return errors.Join(fmt.Errorf("%w: checked Windows remove source identity", ErrUnsafeSecurePath), result.file.Close())
	}
	if err := deleteWindowsHandle(windows.Handle(result.file.Fd())); err != nil {
		return errors.Join(err, result.file.Close())
	}
	if err := result.file.Close(); err != nil {
		return fmt.Errorf("%w: close removed Windows file: %v", ErrCommitIndeterminate, err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("%w: sync removed Windows file: %v", ErrCommitIndeterminate, err)
	}
	if err := validateSecureDirectoryDescriptor(OSFileSystem{}, directory); err != nil {
		return fmt.Errorf("%w: revalidate Windows directory after remove: %v", ErrCommitIndeterminate, err)
	}
	return nil
}

func (directory *windowsSecureDirectory) Sync() error {
	err := windows.FlushFileBuffers(windows.Handle(directory.file.Fd()))
	if errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return nil
	}
	return err
}

func (directory *windowsSecureDirectory) Close() error {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	if directory.closed {
		return directory.closeErr
	}
	directory.closed = true
	directory.closeErr = directory.file.Close()
	return directory.closeErr
}

func (directory *windowsSecureDirectory) cleanupCreated(file *os.File, cause error) error {
	dispositionErr := deleteWindowsHandle(windows.Handle(file.Fd()))
	closeErr := file.Close()
	if dispositionErr != nil || closeErr != nil {
		return fmt.Errorf("%w: clean created Windows entry: %v", ErrCommitIndeterminate, errors.Join(cause, dispositionErr, closeErr))
	}
	if syncErr := directory.Sync(); syncErr != nil {
		return fmt.Errorf("%w: sync cleaned Windows directory: %v", ErrCommitIndeterminate, errors.Join(cause, syncErr))
	}
	if validationErr := validateSecureDirectoryDescriptor(OSFileSystem{}, directory); validationErr != nil {
		return fmt.Errorf("%w: revalidate cleaned Windows directory: %v", ErrCommitIndeterminate, errors.Join(cause, validationErr))
	}
	return cause
}

func (directory *windowsRetainedReadDirectory) Stat() (os.FileInfo, error) {
	return inspectWindowsHandle(windows.Handle(directory.file.Fd()), directory.file.Name())
}

func (directory *windowsRetainedReadDirectory) ReadDir() ([]os.DirEntry, error) {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	if directory.closed {
		return nil, os.ErrClosed
	}
	if _, err := directory.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return directory.file.ReadDir(-1)
}

func (directory *windowsRetainedReadDirectory) OpenDirectory(name string) (RetainedReadDirectory, error) {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileOpened {
		return nil, errors.Join(fmt.Errorf("%w: unexpected Windows retained directory open result", ErrUnsafeSecurePath), result.file.Close())
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, result.file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.IsDir() || !metadata.security.AncestorSafe {
		return nil, errors.Join(fmt.Errorf("%w: Windows retained child directory policy", ErrUnsafeSecurePath), result.file.Close())
	}
	return &windowsRetainedReadDirectory{file: result.file}, nil
}

func (directory *windowsRetainedReadDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileOpened {
		return nil, errors.Join(fmt.Errorf("%w: unexpected Windows retained file open result", ErrUnsafeSecurePath), result.file.Close())
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, result.file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || metadata.identity.Links != 1 || !metadata.security.ExternalImportFileSafe {
		return nil, errors.Join(fmt.Errorf("%w: Windows retained external file policy", ErrUnsafeSecurePath), result.file.Close())
	}
	return &windowsSecureFile{file: result.file}, nil
}

func (directory *windowsRetainedReadDirectory) Close() error {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	if directory.closed {
		return directory.closeErr
	}
	directory.closed = true
	directory.closeErr = directory.file.Close()
	return directory.closeErr
}

func (file *windowsRetainedRegularFile) Read(data []byte) (int, error) {
	file.mutex.Lock()
	defer file.mutex.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	return file.file.Read(data)
}

func (file *windowsRetainedRegularFile) Stat() (os.FileInfo, error) {
	file.mutex.Lock()
	defer file.mutex.Unlock()
	if file.closed {
		return nil, os.ErrClosed
	}
	return inspectWindowsHandle(windows.Handle(file.file.Fd()), file.file.Name())
}

func (file *windowsRetainedRegularFile) Close() error {
	file.mutex.Lock()
	defer file.mutex.Unlock()
	if file.closed {
		return file.closeErr
	}
	file.closed = true
	errorsByHandle := make([]error, 0, 1+len(file.ancestors))
	errorsByHandle = append(errorsByHandle, file.closeHandle(file.file))
	for index := len(file.ancestors) - 1; index >= 0; index-- {
		errorsByHandle = append(errorsByHandle, file.closeHandle(file.ancestors[index]))
	}
	file.closeErr = errors.Join(errorsByHandle...)
	return file.closeErr
}

func (file *windowsRetainedRegularFile) closeHandle(handle *os.File) error {
	if file.closeFile != nil {
		return file.closeFile(handle)
	}
	return handle.Close()
}

func (file *windowsSecureFile) Read(data []byte) (int, error)  { return file.file.Read(data) }
func (file *windowsSecureFile) Write(data []byte) (int, error) { return file.file.Write(data) }
func (file *windowsSecureFile) Sync() error {
	return windows.FlushFileBuffers(windows.Handle(file.file.Fd()))
}
func (file *windowsSecureFile) Stat() (os.FileInfo, error) {
	return inspectWindowsHandle(windows.Handle(file.file.Fd()), file.file.Name())
}
func (file *windowsSecureFile) Close() error { return file.file.Close() }

func windowsPrivateSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	userText := user.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", userText, userText))
	if err != nil {
		return nil, err
	}
	classification, err := classifyWindowsSecurityDescriptor(descriptor, user)
	if err != nil {
		return nil, err
	}
	if !classification.PrivateDACL {
		return nil, fmt.Errorf("%w: constructed Windows private descriptor", ErrUnsafeSecurePath)
	}
	return descriptor, nil
}

func openWindowsRelative(parent windows.Handle, name string, access, share, disposition, options uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windowsOpenResult, error) {
	if err := validateWindowsSecureEntryName(name); err != nil {
		return windowsOpenResult{}, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windowsOpenResult{}, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		nil,
		0,
		share,
		disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windowsOpenResult{}, mapWindowsOpenError(err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return windowsOpenResult{}, errors.New("wrap relative Windows handle")
	}
	return windowsOpenResult{file: file, information: status.Information}, nil
}

func validateWindowsSecureEntryName(name string) error {
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	if strings.ContainsAny(name, "/\\:\x00") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("%w: Windows secure directory entry name", ErrUnsafeSecurePath)
	}
	for _, character := range name {
		if character < 0x20 {
			return fmt.Errorf("%w: Windows secure directory entry name", ErrUnsafeSecurePath)
		}
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
		len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9'
	if reserved {
		return fmt.Errorf("%w: Windows secure directory entry name", ErrUnsafeSecurePath)
	}
	return nil
}

func renameWindowsHandle(source, root windows.Handle, name string, replace bool) error {
	if err := validateWindowsSecureEntryName(name); err != nil {
		return err
	}
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	nameLength := (len(encoded) - 1) * 2
	baseSize := int(unsafe.Offsetof(windowsFileRenameInfo{}.FileName))
	buffer := make([]byte, baseSize+nameLength)
	information := (*windowsFileRenameInfo)(unsafe.Pointer(&buffer[0]))
	if replace {
		information.Flags = 1
	}
	information.RootDirectory = root
	information.FileNameLength = uint32(nameLength)
	copy(unsafe.Slice(&information.FileName[0], nameLength/2), encoded[:len(encoded)-1])
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(source, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation); err != nil {
		var ntStatus windows.NTStatus
		if errors.As(err, &ntStatus) && ntStatus == windows.STATUS_OBJECT_NAME_COLLISION {
			return fmt.Errorf("rename Windows object: %w", os.ErrExist)
		}
		return fmt.Errorf("rename Windows object: %w", err)
	}
	return nil
}

func deleteWindowsHandle(handle windows.Handle) error {
	information := windowsFileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	)
}
