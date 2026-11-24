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

func TestWindowsMissingPolicyUnderOwnerControlledCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsHarnessACL(t, root, false)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsOwnerControlledHarnessACL(t, dir)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "harness.json")
	policy, err := Load(fsys, path)
	if err != nil || policy.Enabled || policy.Version != 1 {
		t.Fatalf("missing owner-controlled policy = %#v, %v", policy, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created policy: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, path); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("present policy under nonprivate parent = %v", err)
	}
	if _, err := Update(fsys, path, func(*Policy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("update readable policy = %v", err)
	}
}

func TestWindowsPolicyFirstAndRepeatWriteUnderOwnerControlledCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsOwnerControlledHarnessACL(t, root)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Elevated runners may assign Administrators as the new directory's owner.
	// Set only the owner so the child keeps its inherited read-only ACE.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED != 0 {
		t.Fatalf("CQ root lost inherited ACL: control = %v, err = %v", control, err)
	}
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "harness.json")
	for index := 0; index < 2; index++ {
		wantEnabled := index == 0
		updated, err := Update(fsys, path, func(policy *Policy) error {
			policy.Enabled = wantEnabled
			return nil
		})
		if err != nil {
			t.Fatalf("update %d: %v", index, err)
		}
		loaded, err := Load(fsys, path)
		if err != nil || loaded.Enabled != wantEnabled || updated.Enabled != wantEnabled {
			t.Fatalf("load %d = %#v, update = %#v, err = %v", index, loaded, updated, err)
		}
		info, err := fsys.Lstat(dir)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("owner-controlled root changed after update %d: %v, %v", index, info, err)
		}
		for _, name := range []string{"harness.json", "harness.json.lock"} {
			info, err := fsys.Lstat(filepath.Join(dir, name))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s after update %d: %v, %v", name, index, info, err)
			}
		}
	}
}

func TestWindowsMissingPolicyRejectsWritableCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsHarnessACL(t, root, false)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsHarnessACL(t, dir, true)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, filepath.Join(dir, "harness.json")); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("missing policy under writable parent = %v", err)
	}
	if _, err := Update(fsys, filepath.Join(dir, "harness.json"), func(*Policy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("update under writable parent = %v", err)
	}
}

func TestWindowsMissingPolicyRejectsOtherOwnerControlledRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsHarnessACL(t, root, false)
	dir := filepath.Join(root, "other")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsOwnerControlledHarnessACL(t, dir)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, filepath.Join(dir, "harness.json")); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("missing policy outside direct CQ root = %v", err)
	}
	if _, err := Update(fsys, filepath.Join(dir, "harness.json"), func(*Policy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("update outside direct CQ root = %v", err)
	}
}

func TestWindowsMissingPolicyRejectsReparseCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsHarnessACL(t, root, false)
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cq")
	if err := os.Symlink(actual, dir); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, filepath.Join(dir, "harness.json")); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("missing policy behind reparse root = %v", err)
	}
	if _, err := Update(fsys, filepath.Join(dir, "harness.json"), func(*Policy) error { return nil }); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("update behind reparse root = %v", err)
	}
}

func setWindowsOwnerControlledHarnessACL(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;WD)", sid, sid)
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
