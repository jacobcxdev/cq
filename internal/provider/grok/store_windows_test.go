//go:build windows

package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsGrokLoginInOwnerControlledCQRoot(t *testing.T) {
	root := t.TempDir()
	secureTestAnchor(t, root)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsGrokOwnerControlledACL(t, dir, false)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthority(Store{FS: fsys, ConfigDir: dir}, nil, nil)
	for range 2 {
		if _, err := a.SaveLogin(context.Background(), loginFixture("windows"), "fixture", true); err != nil {
			t.Fatalf("save OAuth callback: %v", err)
		}
		accounts, err := a.Accounts(context.Background())
		if err != nil || len(accounts) != 1 {
			t.Fatalf("repeat login accounts = %d, %v", len(accounts), err)
		}
	}
	if err := fsutil.ValidateOwnerControlledDirectory(fsys, dir); err != nil {
		t.Fatalf("CQ root authority changed: %v", err)
	}
	for _, name := range []string{"grok-accounts.key", "grok-accounts.json"} {
		if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(dir, name)); err != nil {
			t.Fatalf("private %s: %v", name, err)
		}
	}
}

func TestWindowsGrokLoginRejectsUnsafeCQRoot(t *testing.T) {
	for _, kind := range []string{"writable", "reparse"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			secureTestAnchor(t, root)
			dir := filepath.Join(root, "cq")
			if kind == "reparse" {
				actual := filepath.Join(root, "actual")
				if err := os.Mkdir(actual, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, dir); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				setWindowsGrokOwnerControlledACL(t, dir, true)
			}
			fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
			if err != nil {
				t.Fatal(err)
			}
			a := NewAuthority(Store{FS: fsys, ConfigDir: dir}, nil, nil)
			if _, err := a.SaveLogin(t.Context(), loginFixture("windows"), "fixture", true); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
				t.Fatalf("unsafe CQ root accepted: %v", err)
			}
		})
	}
}

func setWindowsGrokOwnerControlledACL(t *testing.T, path string, writable bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	access := "FR"
	if writable {
		access = "FA"
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;%s;;;WD)", sid, sid, access))
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
