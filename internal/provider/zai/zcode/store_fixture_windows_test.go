//go:build windows

package zcode

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

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
