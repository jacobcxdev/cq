//go:build windows

package installer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestInstallLockRejectsForgedSameFileWindowsHandle(t *testing.T) {
	stateRoot := testInstallLockStateRoot(t)
	locker := FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: stateRoot}
	lock, err := locker.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	path, err := windows.UTF16PtrFromString(filepath.Join(stateRoot, installLockFilename))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	forged := os.NewFile(uintptr(handle), filepath.Join(stateRoot, installLockFilename))
	if forged == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("create forged lock descriptor")
	}
	defer forged.Close()

	if err := locker.ValidateInherited(forged); err == nil {
		t.Fatal("ValidateInherited() accepted a separately opened lock descriptor")
	}
}
