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
