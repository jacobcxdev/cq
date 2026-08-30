package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const installLockFilename = "install.lock"

var ErrInstallationInProgress = errors.New("installation_in_progress")

// InstallLock is held for one complete installer mutation.
type InstallLock interface {
	Close() error
}

// InstallerLocker acquires one non-blocking per-user installer lock.
type InstallerLocker interface {
	Acquire() (InstallLock, error)
}

// FileInstallLocker stores the installer lock under the CQ state root.
type FileInstallLocker struct {
	FS        fsutil.FileSystem
	StateRoot string
}

type fileInstallLock struct {
	fsutil.ExclusiveLock
}

func (lock *fileInstallLock) InheritedFile() (*os.File, error) {
	return fsutil.DuplicateExclusiveLockFile(lock.ExclusiveLock)
}

type installLockContextKey struct{}

// ContextWithInstallLock binds one held lock to installer lifecycle calls.
func ContextWithInstallLock(ctx context.Context, lock InstallLock) context.Context {
	return context.WithValue(ctx, installLockContextKey{}, lock)
}

// InheritedInstallLockFile duplicates the held lock bound to ctx.
func InheritedInstallLockFile(ctx context.Context) (*os.File, error) {
	lock, ok := ctx.Value(installLockContextKey{}).(interface {
		InheritedFile() (*os.File, error)
	})
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return lock.InheritedFile()
}

func (locker FileInstallLocker) Acquire() (InstallLock, error) {
	if locker.FS == nil || locker.StateRoot == "" || !filepath.IsAbs(locker.StateRoot) || filepath.Clean(locker.StateRoot) != locker.StateRoot {
		return nil, fmt.Errorf("invalid installer lock state root")
	}
	if err := fsutil.EnsureSecureDirectory(locker.FS, locker.StateRoot); err != nil {
		return nil, fmt.Errorf("create installer state root: %w", err)
	}
	opener, ok := locker.FS.(fsutil.ExclusiveLocker)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	lock, err := opener.OpenExclusiveLock(filepath.Join(locker.StateRoot, installLockFilename), 0o600)
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return nil, fmt.Errorf("%w: another CQ installation is running", ErrInstallationInProgress)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire CQ installer lock: %w", err)
	}
	return &fileInstallLock{ExclusiveLock: lock}, nil
}

// ValidateInherited proves file is the descriptor for this active lock.
func (locker FileInstallLocker) ValidateInherited(file *os.File) error {
	if locker.FS == nil || locker.StateRoot == "" || !filepath.IsAbs(locker.StateRoot) || filepath.Clean(locker.StateRoot) != locker.StateRoot {
		return fmt.Errorf("invalid installer lock state root")
	}
	return fsutil.ValidateInheritedExclusiveLockFile(locker.FS, filepath.Join(locker.StateRoot, installLockFilename), file)
}
