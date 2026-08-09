package fsutil

import (
	"errors"
	"os"
	"testing"
)

func TestAcquireNewExclusiveLockInDirectoryCreatesOnce(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()

	lock, err := AcquireNewExclusiveLockInDirectory(fsys, directory, "maintenance.lock")
	if err != nil {
		t.Fatal(err)
	}
	info, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := fsys.FileIdentity(info)
	if !ok || identity.Inode == 0 || identity.Links != 1 {
		t.Fatalf("new lock identity = %#v, %v; want one named link", identity, ok)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := AcquireNewExclusiveLockInDirectory(fsys, directory, "maintenance.lock")
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create error = %v, want already exists", err)
	}
}

func TestAcquireNewExclusiveLockInDirectoryFailsClosedOnDirectorySync(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	directory := &newLockSyncFailureDirectory{SecureDirectory: opened, err: errors.New("sync failed")}
	defer directory.Close()

	lock, err := AcquireNewExclusiveLockInDirectory(fsys, directory, "maintenance.lock")
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil || !errors.Is(err, directory.err) {
		t.Fatalf("create error = %v, want directory sync failure", err)
	}
	if info, statErr := fsys.Stat("/state/maintenance.lock"); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("failed-sync lock = %#v, %v; want retained fail-closed file", info, statErr)
	}
}

type newLockSyncFailureDirectory struct {
	SecureDirectory
	err error
}

func (directory *newLockSyncFailureDirectory) OpenNewExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	return directory.SecureDirectory.(NewExclusiveLocker).OpenNewExclusiveLock(name, mode)
}

func (directory *newLockSyncFailureDirectory) Sync() error { return directory.err }
