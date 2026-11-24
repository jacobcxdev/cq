//go:build windows

package codex

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsResetAttemptStoreUnderOwnerControlledCache(t *testing.T) {
	cache := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;WD)", sid))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(cache, windows.SE_FILE_OBJECT, flags, sid, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	fsys := fsutil.OSFileSystem{}
	store, err := NewResetAttemptStore(fsys, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(store.FS, filepath.Join(cache, resetAttemptDirName)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Ensure("fixture", "credit", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending("fixture")
	if err != nil || len(pending) != 1 || pending[0].IdempotencyKey != got.IdempotencyKey {
		t.Fatalf("pending reset attempt = %v, error = %v", pending, err)
	}
	info, err := fsys.Stat(cache)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("cache ACL changed: %v, %v", info, err)
	}
}
