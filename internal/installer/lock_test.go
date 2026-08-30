package installer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestInstallLockSerialisesMutations(t *testing.T) {
	stateRoot := testInstallLockStateRoot(t)
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
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("install lock mode = %o", info.Mode().Perm())
	}
}

func TestInstallLockExportsValidatedInheritedCapability(t *testing.T) {
	stateRoot := testInstallLockStateRoot(t)
	locker := FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: stateRoot}
	lock, err := locker.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	inheritable, ok := lock.(interface {
		InheritedFile() (*os.File, error)
	})
	if !ok {
		t.Fatal("install lock does not expose an inherited descriptor")
	}
	inherited, err := inheritable.InheritedFile()
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	validator, ok := any(locker).(interface {
		ValidateInherited(*os.File) error
	})
	if !ok {
		t.Fatal("install locker does not validate inherited descriptors")
	}
	if err := validator.ValidateInherited(inherited); err != nil {
		t.Fatalf("ValidateInherited() error = %v", err)
	}
	forged, err := os.Open(filepath.Join(stateRoot, installLockFilename))
	if err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if forged != nil {
		defer forged.Close()
		if err := validator.ValidateInherited(forged); err == nil {
			t.Fatal("ValidateInherited() accepted a separately opened lock descriptor")
		}
	}

	foreign, err := os.CreateTemp(t.TempDir(), "foreign-lock")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err := validator.ValidateInherited(foreign); err == nil {
		t.Fatal("ValidateInherited() accepted a foreign descriptor")
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

func testInstallLockStateRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return filepath.Join(t.TempDir(), "state")
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, roots.State); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(roots.State, "install-lock-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove Windows installer lock test root: %v", err)
		}
	})
	return root
}
