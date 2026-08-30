package installer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInstallLockSerialisesMutations(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	locker := FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: stateRoot}
	first, err := locker.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	second, err := locker.Acquire()
	if second != nil || !errors.Is(err, ErrInstallationInProgress) {
		t.Fatalf("contended Acquire() = %v, %v", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := locker.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateRoot, installLockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("install lock mode = %o", info.Mode().Perm())
	}
}

func TestInstallLockRequiresAbsoluteStateRootAndLockCapability(t *testing.T) {
	for _, locker := range []FileInstallLocker{
		{FS: fsutil.OSFileSystem{}, StateRoot: "relative"},
		{FS: fileSystemWithoutInstallerLock{FileSystem: fsutil.NewMemFS()}, StateRoot: filepath.Clean(t.TempDir())},
	} {
		if lock, err := locker.Acquire(); lock != nil || err == nil {
			t.Fatalf("Acquire() = %v, %v", lock, err)
		}
	}
}

type fileSystemWithoutInstallerLock struct {
	fsutil.FileSystem
}
