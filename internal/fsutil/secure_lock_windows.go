//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

type windowsExclusiveLock struct {
	mu         sync.Mutex
	file       *os.File
	overlapped windows.Overlapped
	closed     bool
	closeErr   error
}

func (lock *windowsExclusiveLock) Stat() (os.FileInfo, error) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil, os.ErrClosed
	}
	return inspectWindowsHandle(windows.Handle(lock.file.Fd()), lock.file.Name())
}

func (lock *windowsExclusiveLock) duplicateFile() (*os.File, error) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil, os.ErrClosed
	}
	current := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		current,
		windows.Handle(lock.file.Fd()),
		current,
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), lock.file.Name())
	if file == nil {
		_ = windows.CloseHandle(duplicate)
		return nil, ErrSecureCapabilityUnavailable
	}
	return file, nil
}

func inspectInheritedExclusiveLockFile(file *os.File) (os.FileInfo, error) {
	return inspectWindowsHandle(windows.Handle(file.Fd()), file.Name())
}

func validateInheritedExclusiveLockHeld(file *os.File) error {
	// Lock handles deny new writers. Flush therefore proves this descriptor
	// retained the write capability exported by the lock owner.
	if err := file.Sync(); err != nil {
		return errors.Join(ErrExclusiveLockNotHeld, err)
	}
	return nil
}

func (lock *windowsExclusiveLock) Close() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return lock.closeErr
	}
	lock.closed = true
	unlockErr := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &lock.overlapped)
	lock.closeErr = errors.Join(unlockErr, lock.file.Close())
	return lock.closeErr
}

func (fsys OSFileSystem) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	directory, err := fsys.OpenSecureDirectory(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	lock, lockErr := directory.OpenExclusiveLock(filepath.Base(name), mode)
	closeErr := directory.Close()
	if lockErr != nil || closeErr != nil {
		if lock != nil {
			closeErr = errors.Join(closeErr, lock.Close())
		}
		return nil, errors.Join(lockErr, closeErr)
	}
	return lock, nil
}

func (directory *windowsSecureDirectory) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	return directory.openExclusiveLock(name, mode, windows.FILE_OPEN_IF)
}

func (directory *windowsSecureDirectory) OpenNewExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	return directory.openExclusiveLock(name, mode, windows.FILE_CREATE)
}

func (directory *windowsSecureDirectory) openExclusiveLock(name string, _ os.FileMode, disposition uint32) (ExclusiveLock, error) {
	descriptor, err := windowsPrivateSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ,
		disposition,
		windows.FILE_NON_DIRECTORY_FILE,
		descriptor,
	)
	if err != nil {
		if disposition == windows.FILE_OPEN_IF && isWindowsLockContention(err) {
			return nil, ErrExclusiveLockHeld
		}
		return nil, err
	}
	created := result.information == windowsFileCreated
	fail := func(cause error) (ExclusiveLock, error) {
		if created {
			return nil, directory.cleanupCreated(result.file, cause)
		}
		return nil, errors.Join(cause, result.file.Close())
	}
	if (disposition == windows.FILE_CREATE && !created) ||
		(disposition == windows.FILE_OPEN_IF && result.information != windowsFileOpened && !created) {
		return fail(fmt.Errorf("%w: unexpected Windows lock open result", ErrUnsafeSecurePath))
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return fail(err)
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || !metadata.security.PrivateDACL || metadata.identity.Links != 1 {
		return fail(fmt.Errorf("%w: Windows lock file policy", ErrUnsafeSecurePath))
	}
	lock := &windowsExclusiveLock{file: result.file}
	if err := lockWindowsFile(lock.file, &lock.overlapped); err != nil {
		return fail(err)
	}
	if created {
		if err := lock.file.Sync(); err != nil {
			_ = windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &lock.overlapped)
			return fail(err)
		}
		if err := directory.Sync(); err != nil {
			_ = windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &lock.overlapped)
			return fail(err)
		}
		if err := validateSecureDirectoryDescriptor(OSFileSystem{}, directory); err != nil {
			_ = windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &lock.overlapped)
			return fail(err)
		}
	}
	return lock, nil
}

func (directory *windowsSecureDirectory) ProbeExclusiveLockHeld(name string, _ os.FileMode) (os.FileInfo, error) {
	result, err := openWindowsRelative(
		windows.Handle(directory.file.Fd()),
		name,
		windows.FILE_GENERIC_READ|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windowsShareAll,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.information != windowsFileOpened {
		return nil, errors.Join(fmt.Errorf("%w: unexpected Windows lock probe result", ErrUnsafeSecurePath), result.file.Close())
	}
	info, err := inspectWindowsHandle(windows.Handle(result.file.Fd()), name)
	if err != nil {
		return nil, errors.Join(err, result.file.Close())
	}
	metadata := info.(windowsSecureFileInfo)
	if !metadata.Mode().IsRegular() || !metadata.security.PrivateDACL || metadata.identity.Links != 1 {
		return nil, errors.Join(fmt.Errorf("%w: Windows lock probe policy", ErrUnsafeSecurePath), result.file.Close())
	}
	var overlapped windows.Overlapped
	if err := lockWindowsFile(result.file, &overlapped); err != nil {
		closeErr := result.file.Close()
		if errors.Is(err, ErrExclusiveLockHeld) && closeErr == nil {
			return info, nil
		}
		return nil, errors.Join(err, closeErr)
	}
	unlockErr := windows.UnlockFileEx(windows.Handle(result.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := result.file.Close()
	return info, errors.Join(ErrExclusiveLockNotHeld, unlockErr, closeErr)
}

func lockWindowsFile(file *os.File, overlapped *windows.Overlapped) error {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrExclusiveLockHeld
	}
	if err != nil {
		return fmt.Errorf("acquire Windows exclusive filesystem lock: %w", err)
	}
	return nil
}

func isWindowsLockContention(err error) bool {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true
	}
	var status windows.NTStatus
	return errors.As(err, &status) && (status == windows.STATUS_SHARING_VIOLATION || status == windows.STATUS_LOCK_NOT_GRANTED)
}
