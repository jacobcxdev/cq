package installer

import (
	"errors"
	"fmt"
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

func (locker FileInstallLocker) Acquire() (InstallLock, error) {
	if locker.FS == nil || locker.StateRoot == "" || !filepath.IsAbs(locker.StateRoot) || filepath.Clean(locker.StateRoot) != locker.StateRoot {
		return nil, fmt.Errorf("invalid installer lock state root")
	}
	if err := locker.FS.MkdirAll(locker.StateRoot, 0o700); err != nil {
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
	return lock, nil
}
