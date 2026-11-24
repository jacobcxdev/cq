//go:build windows

package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsManagedStoreSaveAndReload(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatalf("SaveNew: %v", err)
	}
	if filepath.Dir(record.Path) != filepath.Join(home, ".codex", "accounts") {
		t.Fatalf("managed path outside isolated home: %q", record.Path)
	}
	loaded, err := store.Load(record.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata.Revision != record.Metadata.Revision || loaded.Credential.AccountID != record.Credential.AccountID {
		t.Fatal("reloaded managed record changed")
	}
}

func TestWindowsManagedStoreSavesUnderReadableAccountsDirectory(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;WD)", sid, sid))
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
	if err := windows.SetNamedSecurityInfo(codexDir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	accountsDir := filepath.Join(codexDir, "accounts")
	if err := os.Mkdir(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(accountsDir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatalf("SaveNew under readable accounts directory: %v", err)
	}
	previous := record.Metadata.Revision
	record.Credential.AccessToken = "updated-access"
	if err := store.Commit(&record, previous); err != nil {
		t.Fatalf("repeat Commit under readable accounts directory: %v", err)
	}
	loaded, err := store.Load(record.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Credential.AccessToken != "updated-access" {
		t.Fatal("repeat Commit did not persist credential")
	}
	info, err := store.FS.Stat(accountsDir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("accounts ACL changed: %v, %v", info, err)
	}
}

func TestWindowsManagedStoreRejectsLinkedCredential(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(record.Path), "linked.auth.json")
	if err := os.Link(record.Path, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := store.Load(record.Path); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("linked managed credential load = %v", err)
	}
}

func TestWindowsManagedStoreRejectsReparseCredential(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	target := record.Path + ".private"
	if err := os.Rename(record.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, record.Path); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	if _, err := store.Load(record.Path); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("reparse managed credential load = %v", err)
	}
}

func TestWindowsManagedStoreRecoversOwnedCommittedCredential(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(projectionCredential("user@example.test", "acct-1", "user-1", "plus", testLoginCredential().CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record.Path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	// Elevated runners can default new files to Administrators ownership.
	// Model the unelevated user login while retaining the inherited DACL.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(record.Path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	info, err := store.FS.Stat(record.Path)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("legacy Windows ACL mode = %v, err = %v", info, err)
	}
	if _, err := store.Load(record.Path); err == nil {
		t.Fatal("Load accepted an unsealed managed credential")
	}
	catalogue := &projectionCatalogueStub{}
	coordinator := &CredentialCoordinator{Store: store, Registry: catalogue, Journal: RemovalJournal{FS: store.FS, StateDir: filepath.Join(home, "state")}}
	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("recover permissions: %v", err)
	}
	if len(catalogue.upserts) != 1 || catalogue.upserts[0].AccountID != "acct-1" {
		t.Fatalf("recovered account projection count = %d", len(catalogue.upserts))
	}
	if _, err := store.Load(record.Path); err != nil {
		t.Fatalf("Load after recovery: %v", err)
	}
	got, err := os.ReadFile(record.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("credential bytes changed during recovery: %v", err)
	}
}

func TestWindowsManagedStoreRejectsWritableCredentialRecovery(t *testing.T) {
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	store := &ManagedStore{FS: fsutil.OSFileSystem{}, Home: home, EnsureEpoch: func() error { return nil }}
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)", sid, sid))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(record.Path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.recoverManagedPermissions(); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("writable credential recovery = %v", err)
	}
	if _, err := store.Load(record.Path); err == nil {
		t.Fatal("writable credential was trusted after recovery")
	}
}

func setWindowsManagedTestACL(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sid, sid)
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
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("isolated home unavailable: %v", err)
	}
}
