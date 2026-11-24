//go:build windows

package grok

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func secureTestAnchor(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", user.User.Sid.String())
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsStoreDiscoveryInheritedDirectoryWithoutAccounts(t *testing.T) {
	root := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Match a legacy config directory: trusted principals with inheritable
	// access, but no protected private DACL. Only this test's directory changes.
	descriptor, err := windows.SecurityDescriptorFromString("D:AI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateSecureDirectory(fsys, root); err == nil {
		t.Fatal("fixture unexpectedly satisfies the private-directory contract")
	}
	a := NewAuthority(Store{FS: fsys, ConfigDir: root}, nil, nil)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("unconfigured Windows discovery = %v, %v", accounts, err)
	}
	after, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("discovery changed the directory ACL")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("discovery created state: %v, %v", entries, err)
	}
}
