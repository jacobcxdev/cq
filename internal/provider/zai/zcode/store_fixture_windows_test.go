//go:build windows

package zcode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsOpenSessionStoreInOwnerControlledCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsZCodeStoreACL(t, root, false)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsZCodeStoreACL(t, dir, true)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err := OpenSessionStore(fsys, dir, true)
		if err != nil {
			t.Fatalf("open callback store: %v", err)
		}
		if _, err := store.Save(locationFixture(), "fixture", true); err != nil {
			t.Fatalf("save callback session: %v", err)
		}
		accounts, err := store.Accounts()
		if err != nil || len(accounts) != 1 {
			t.Fatalf("repeat callback accounts = %d, %v", len(accounts), err)
		}
	}
	if err := fsutil.ValidateOwnerControlledDirectory(fsys, dir); err != nil {
		t.Fatalf("CQ root authority changed: %v", err)
	}
	for _, name := range []string{"zcode-accounts.key", "zcode-accounts.json"} {
		if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(dir, name)); err != nil {
			t.Fatalf("private %s: %v", name, err)
		}
	}
}

func setWindowsZCodeStoreACL(t *testing.T, path string, readable bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid, sid)
	if readable {
		sddl += "(A;OICI;FR;;;WD)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func newZCodeStoreTestFS(t *testing.T) (fsutil.OSFileSystem, string) {
	t.Helper()
	root := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	cqRoot := filepath.Join(root, "cq")
	fsys, err := fsutil.NewPrivateDestinationFileSystem(cqRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.EnsureSecureDirectory(fsys, cqRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(fsys, cqRoot); err != nil {
		t.Fatal(err)
	}
	return fsys, cqRoot
}
