//go:build windows

package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsPortableWorkspaceNativeUpdate(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsHarnessACL(t, project, false)

	path := filepath.Join(project, ".cq", "harness.json")
	fsys := fsutil.OSFileSystem{}
	for _, name := range []string{"first", "second"} {
		policy, err := UpdateWorkspace(fsys, path, func(policy *WorkspacePolicy) error {
			policy.Profiles[name] = Definition{Changes: Changes{Default: Disabled}}
			return nil
		})
		if err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
		if len(policy.Profiles) == 0 {
			t.Fatal("update returned no profiles")
		}
	}
	policy, err := LoadWorkspace(fsys, path)
	if err != nil || len(policy.Profiles) != 2 {
		t.Fatalf("persisted policy = %#v, %v", policy, err)
	}
	privateFS, err := fsutil.NewPrivateDestinationFileSystem(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(privateFS, filepath.Dir(path)); err != nil {
		t.Fatalf("workspace directory security: %v", err)
	}
	for _, file := range []string{path, path + ".lock"} {
		if err := fsutil.ValidateSecureRegularFile(privateFS, file); err != nil {
			t.Fatalf("workspace file security: %v", err)
		}
	}
}

func TestWindowsPortableWorkspaceRejectsUnsafeProject(t *testing.T) {
	t.Run("broad ACL", func(t *testing.T) {
		project := filepath.Join(t.TempDir(), "project")
		if err := os.Mkdir(project, 0o700); err != nil {
			t.Fatal(err)
		}
		setWindowsHarnessACL(t, project, true)
		path := filepath.Join(project, ".cq", "harness.json")
		if _, err := UpdateWorkspace(fsutil.OSFileSystem{}, path, func(*WorkspacePolicy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
			t.Fatalf("broad project error = %v", err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe project received .cq directory: %v", err)
		}
	})
	t.Run("reparse point", func(t *testing.T) {
		root := t.TempDir()
		actual := filepath.Join(root, "actual")
		if err := os.Mkdir(actual, 0o700); err != nil {
			t.Fatal(err)
		}
		project := filepath.Join(root, "project")
		if err := os.Symlink(actual, project); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		path := filepath.Join(project, ".cq", "harness.json")
		if _, err := UpdateWorkspace(fsutil.OSFileSystem{}, path, func(*WorkspacePolicy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
			t.Fatalf("reparse project error = %v", err)
		}
	})
}

func setWindowsHarnessACL(t *testing.T, project string, broad bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid, sid)
	if broad {
		sddl += "(A;OICI;FA;;;WD)"
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
	if err := windows.SetNamedSecurityInfo(project, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
