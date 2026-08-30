package fsutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSameSecureObjectUsesCompleteIdentity(t *testing.T) {
	base := SecureFileIdentity{Device: 7, Inode: 11, Links: 1, FileID: [16]byte{15: 9}}
	changes := []SecureFileIdentity{
		{Device: 8, Inode: 11, Links: 1, FileID: [16]byte{15: 9}},
		{Device: 7, Inode: 12, Links: 1, FileID: [16]byte{15: 9}},
		{Device: 7, Inode: 11, Links: 2, FileID: [16]byte{15: 9}},
		{Device: 7, Inode: 11, Links: 1, FileID: [16]byte{15: 10}},
	}
	if !SameSecureObject(base, base) {
		t.Fatal("identical identity did not match")
	}
	for _, changed := range changes {
		if SameSecureObject(base, changed) {
			t.Fatalf("identity %#v matched %#v", base, changed)
		}
	}
}

func TestSecurePrincipalUsesCompleteSID(t *testing.T) {
	first := SecurePrincipal{Kind: SecurePrincipalSID, SIDLength: 12, SID: [68]byte{0: 1, 11: 9}}
	second := first
	second.SID[11] = 10
	if first == second {
		t.Fatal("distinct canonical SIDs compared equal")
	}
}

func TestValidateSecureOwnerUsesPrincipalInspector(t *testing.T) {
	fsys := &principalMemFS{
		MemFS:     NewMemFS(),
		effective: SecurePrincipal{Kind: SecurePrincipalSID, SIDLength: 3, SID: [68]byte{1, 2, 3}},
		owner:     SecurePrincipal{Kind: SecurePrincipalSID, SIDLength: 3, SID: [68]byte{1, 2, 3}},
	}
	if err := fsys.WriteFile("/private", []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := fsys.Stat("/private")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSecureOwner(fsys, info); err != nil {
		t.Fatalf("matching principal rejected: %v", err)
	}
	fsys.owner.SID[2] = 4
	if err := ValidateSecureOwner(fsys, info); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("different principal error = %v, want unsafe", err)
	}
}

func TestExternalCredentialPoliciesPreserveUnixModes(t *testing.T) {
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/codex", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCredentialDirectory(fsys, "/codex"); err != nil {
		t.Fatalf("0755 credential directory rejected: %v", err)
	}
	if err := fsys.WriteFile("/codex/auth.json", []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := fsys.Stat("/codex/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCredentialFile(fsys, info); err != nil {
		t.Fatalf("0600 credential rejected: %v", err)
	}
	if err := fsys.Chmod("/codex/auth.json", 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = fsys.Stat("/codex/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCredentialFile(fsys, info); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("0644 credential error = %v, want unsafe", err)
	}
	if err := ValidateRetainedExternalImportFile(fsys, info); err != nil {
		t.Fatalf("0644 retained import rejected: %v", err)
	}
}

func TestExternalCredentialPolicyOwnsNativeModeDecision(t *testing.T) {
	fsys := &externalPolicyMemFS{principalMemFS: principalMemFS{MemFS: NewMemFS()}}
	if err := fsys.MkdirAll("/codex", 0); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCredentialDirectory(fsys, "/codex"); err != nil {
		t.Fatalf("native directory mode rejected before policy: %v", err)
	}
	if err := fsys.WriteFile("/codex/auth.json", []byte("secret"), 0); err != nil {
		t.Fatal(err)
	}
	info, err := fsys.Stat("/codex/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCredentialFile(fsys, info); err != nil {
		t.Fatalf("native credential mode rejected before policy: %v", err)
	}
	if fsys.directoryCalls != 1 || fsys.credentialCalls != 1 {
		t.Fatalf("external policy calls = directory %d credential %d, want 1/1", fsys.directoryCalls, fsys.credentialCalls)
	}
}

type principalMemFS struct {
	*MemFS
	effective SecurePrincipal
	owner     SecurePrincipal
}

func (fsys *principalMemFS) EffectiveUID() uint64 { return 0 }
func (fsys *principalMemFS) FileOwnerUID(os.FileInfo) (uint64, bool) {
	return 0, false
}
func (fsys *principalMemFS) EffectivePrincipal() (SecurePrincipal, bool) {
	return fsys.effective, true
}
func (fsys *principalMemFS) FileOwnerPrincipal(os.FileInfo) (SecurePrincipal, bool) {
	return fsys.owner, true
}

type externalPolicyMemFS struct {
	principalMemFS
	directoryCalls  int
	credentialCalls int
}

func (fsys *externalPolicyMemFS) ValidateExternalCredentialDirectoryInfo(os.FileInfo) error {
	fsys.directoryCalls++
	return nil
}
func (fsys *externalPolicyMemFS) ValidateExternalCredential(os.FileInfo) error {
	fsys.credentialCalls++
	return nil
}
func (*externalPolicyMemFS) ValidateExternalCache(os.FileInfo) error { return nil }
func (*externalPolicyMemFS) ValidateRetainedExternalImportFileInfo(os.FileInfo) error {
	return nil
}

func TestSecureAtomicWriteUsesUniqueExclusiveTemporaryFiles(t *testing.T) {
	t.Parallel()
	fsys := &faultSecureFS{MemFS: NewMemFS()}
	path := "/state/value.json"
	for _, value := range []string{"first", "second"} {
		if err := SecureAtomicWrite(fsys, path, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if len(fsys.createPaths) != 2 {
		t.Fatalf("temporary paths = %v, want two", fsys.createPaths)
	}
	if fsys.createPaths[0] == fsys.createPaths[1] {
		t.Fatalf("temporary paths reused %q", fsys.createPaths[0])
	}
	for _, temporaryPath := range fsys.createPaths {
		if temporaryPath == path+".tmp" {
			t.Fatalf("fixed temporary path used %q", temporaryPath)
		}
		if filepath.Dir(temporaryPath) != filepath.Dir(path) {
			t.Fatalf("temporary path directory = %q, want %q", filepath.Dir(temporaryPath), filepath.Dir(path))
		}
	}
	if got, err := fsys.ReadFile(path); err != nil || string(got) != "second" {
		t.Fatalf("destination = %q, %v; want second", got, err)
	}
}

func TestSecureAtomicCreateInDirectoryDoesNotReplaceExisting(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/value", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()

	err = SecureAtomicCreateInDirectory(fsys, directory, "value", []byte("new"))
	if !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("SecureAtomicCreateInDirectory error = %v, want not-committed exists", err)
	}
	got, readErr := fsys.ReadFile("/state/value")
	if readErr != nil || string(got) != "old" {
		t.Fatalf("destination = %q, %v; want old", got, readErr)
	}
}

func TestSecurePromoteNoReplaceInDirectoryMovesExactSource(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := SecureAtomicCreateInDirectory(fsys, directory, ".value.fresh", []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, identity, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, ".value.fresh", 64)
	if err != nil {
		t.Fatal(err)
	}

	if err := SecurePromoteNoReplaceInDirectory(fsys, directory, ".value.fresh", "value", data, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("/state/.value.fresh"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source error = %v, want not exist", err)
	}
	got, gotIdentity, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, "value", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" || gotIdentity != identity {
		t.Fatalf("installed = %q, %#v; want new, %#v", got, gotIdentity, identity)
	}
}

func TestSecurePromoteNoReplaceInDirectoryDoesNotReplaceExisting(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := SecureAtomicCreateInDirectory(fsys, directory, ".value.fresh", []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, identity, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, ".value.fresh", 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := SecureAtomicCreateInDirectory(fsys, directory, "value", []byte("old")); err != nil {
		t.Fatal(err)
	}

	err = SecurePromoteNoReplaceInDirectory(fsys, directory, ".value.fresh", "value", data, identity)
	if !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("SecurePromoteNoReplaceInDirectory error = %v, want not-committed exists", err)
	}
	gotSource, sourceErr := fsys.ReadFile("/state/.value.fresh")
	gotDestination, destinationErr := fsys.ReadFile("/state/value")
	if sourceErr != nil || destinationErr != nil || string(gotSource) != "new" || string(gotDestination) != "old" {
		t.Fatalf("source/destination = %q/%q, %v/%v; want new/old", gotSource, gotDestination, sourceErr, destinationErr)
	}
}

func TestSecurePromoteNoReplaceInDirectoryRejectsSourceIdentitySwap(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	if err := SecureAtomicCreateInDirectory(fsys, opened, ".value.fresh", []byte("same")); err != nil {
		t.Fatal(err)
	}
	data, identity, err := ReadSecureFileInDirectoryWithIdentity(fsys, opened, ".value.fresh", 64)
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceAfterOpenDirectory{SecureDirectory: opened, fsys: fsys, path: "/state"}
	defer directory.Close()

	err = SecurePromoteNoReplaceInDirectory(fsys, directory, ".value.fresh", "value", data, identity)
	if !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("promotion error = %v, want unsafe not-committed", err)
	}
	if _, err := fsys.ReadFile("/state/value"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination error = %v, want not exist", err)
	}
}

func TestSecureAtomicWriteCrashOutcomes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		step        string
		want        CommitOutcome
		wantContent string
	}{
		{name: "temporary_create", step: "create", want: CommitNotCommitted, wantContent: "old"},
		{name: "temporary_write", step: "write", want: CommitNotCommitted, wantContent: "old"},
		{name: "temporary_sync", step: "sync", want: CommitNotCommitted, wantContent: "old"},
		{name: "rename", step: "rename", want: CommitNotCommitted, wantContent: "old"},
		{name: "directory_sync", step: "sync-dir", want: CommitIndeterminate, wantContent: "new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fsys := &faultSecureFS{MemFS: NewMemFS()}
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/state/value.json", []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			fsys.failStep = test.step
			err := SecureAtomicWrite(fsys, "/state/value.json", []byte("new"))
			if err == nil {
				t.Fatal("SecureAtomicWrite error = nil")
			}
			if got := AtomicWriteOutcome(err); got != test.want {
				t.Fatalf("outcome = %v, want %v (error %v)", got, test.want, err)
			}
			got, readErr := fsys.ReadFile("/state/value.json")
			if readErr != nil || string(got) != test.wantContent {
				t.Fatalf("destination = %q, %v; want %q", got, readErr, test.wantContent)
			}
		})
	}
}

func TestSecureAtomicWriteRejectsDirectoryReplacementBeforeRename(t *testing.T) {
	t.Parallel()
	fsys := &replacingDirectoryFS{MemFS: NewMemFS()}
	err := SecureAtomicWrite(fsys, "/state/value.json", []byte("new"))
	if err == nil {
		t.Fatal("SecureAtomicWrite error = nil after directory replacement")
	}
	if got := AtomicWriteOutcome(err); got != CommitNotCommitted {
		t.Fatalf("outcome = %v, want %v (error %v)", got, CommitNotCommitted, err)
	}
	if _, err := fsys.ReadFile("/state/value.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory destination error = %v, want not exist", err)
	}
}

func TestSecureAtomicWriteReportsIndeterminateAfterRenameDirectoryReplacement(t *testing.T) {
	t.Parallel()
	fsys := &replacingDirectoryFS{MemFS: NewMemFS(), replaceAfterRename: true}
	err := SecureAtomicWrite(fsys, "/state/value.json", []byte("new"))
	if err == nil {
		t.Fatal("SecureAtomicWrite error = nil after post-rename directory replacement")
	}
	if got := AtomicWriteOutcome(err); got != CommitIndeterminate {
		t.Fatalf("outcome = %v, want %v (error %v)", got, CommitIndeterminate, err)
	}
	if _, err := fsys.ReadFile("/state/value.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory destination error = %v, want not exist", err)
	}
}

func TestSecurePathValidationRejectsUnsafeObjects(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	privateFile := filepath.Join(privateDir, "state.json")
	if err := os.WriteFile(privateFile, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("directory symlink", func(t *testing.T) {
		path := filepath.Join(root, "directory-link")
		if err := os.Symlink(privateDir, path); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSecureDirectory(OSFileSystem{}, path); err == nil {
			t.Fatal("ValidateSecureDirectory error = nil")
		}
	})
	t.Run("directory type", func(t *testing.T) {
		if err := ValidateSecureDirectory(OSFileSystem{}, privateFile); err == nil {
			t.Fatal("ValidateSecureDirectory error = nil")
		}
	})
	t.Run("directory mode", func(t *testing.T) {
		path := filepath.Join(root, "permissive-directory")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSecureDirectory(OSFileSystem{}, path); err == nil {
			t.Fatal("ValidateSecureDirectory error = nil")
		}
	})
	t.Run("directory owner", func(t *testing.T) {
		if err := ValidateSecureDirectory(foreignOwnerFS{OSFileSystem{}}, privateDir); err == nil {
			t.Fatal("ValidateSecureDirectory error = nil")
		}
	})
	t.Run("directory special mode", func(t *testing.T) {
		path := filepath.Join(root, "sticky-directory")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSecureDirectory(OSFileSystem{}, path); err == nil {
			t.Fatal("ValidateSecureDirectory error = nil")
		}
	})
	t.Run("file symlink", func(t *testing.T) {
		path := filepath.Join(privateDir, "file-link")
		if err := os.Symlink(privateFile, path); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSecureRegularFile(OSFileSystem{}, path); err == nil {
			t.Fatal("ValidateSecureRegularFile error = nil")
		}
	})
	t.Run("file type", func(t *testing.T) {
		if err := ValidateSecureRegularFile(OSFileSystem{}, privateDir); err == nil {
			t.Fatal("ValidateSecureRegularFile error = nil")
		}
	})
	t.Run("file mode", func(t *testing.T) {
		path := filepath.Join(privateDir, "permissive-file")
		if err := os.WriteFile(path, []byte("state"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSecureRegularFile(OSFileSystem{}, path); err == nil {
			t.Fatal("ValidateSecureRegularFile error = nil")
		}
	})
	t.Run("file owner", func(t *testing.T) {
		if err := ValidateSecureRegularFile(foreignOwnerFS{OSFileSystem{}}, privateFile); err == nil {
			t.Fatal("ValidateSecureRegularFile error = nil")
		}
	})
	t.Run("file special mode", func(t *testing.T) {
		if err := ValidateSecureRegularFile(specialModeFS{OSFileSystem{}}, privateFile); err == nil {
			t.Fatal("ValidateSecureRegularFile error = nil")
		}
	})
}

func TestReadSecureFileUsesNoFollowHandleInsteadOfReadFile(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sidecar.json")
	if err := os.WriteFile(path, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := &readTrapFS{OSFileSystem: OSFileSystem{}}
	got, err := ReadSecureFile(fsys, path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "trusted" {
		t.Fatalf("content = %q, want trusted", got)
	}
	if fsys.readFileCalled {
		t.Fatal("ReadSecureFile called path-following ReadFile")
	}
}

func TestReadSecureFileRejectsSymlinkAndOversizeContent(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sidecar.json")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecureFile(OSFileSystem{}, path, 4); !errors.Is(err, ErrSecureFileTooLarge) {
		t.Fatalf("oversize error = %v, want ErrSecureFileTooLarge", err)
	}
	link := filepath.Join(dir, "sidecar-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecureFile(OSFileSystem{}, link, 64); err == nil {
		t.Fatal("symlink read error = nil")
	}
}

func TestExclusiveLockHasOneCrossProcessOwner(t *testing.T) {
	requireUnixSecureFS(t)
	if os.Getenv("CQ_TEST_EXCLUSIVE_LOCK_HELPER") != "" {
		lock, err := AcquireExclusiveLock(OSFileSystem{}, os.Getenv("CQ_TEST_EXCLUSIVE_LOCK_PATH"))
		if errors.Is(err, ErrExclusiveLockHeld) {
			os.Exit(3)
		}
		if err != nil {
			os.Exit(4)
		}
		if err := lock.Close(); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	}

	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owner.lock")
	first, err := AcquireExclusiveLock(OSFileSystem{}, path)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestExclusiveLockHasOneCrossProcessOwner$")
	command.Env = append(os.Environ(), "CQ_TEST_EXCLUSIVE_LOCK_HELPER=1", "CQ_TEST_EXCLUSIVE_LOCK_PATH="+path)
	if err := command.Run(); err == nil {
		t.Fatal("second process acquired held lock")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("second process error = %v, want held-lock exit 3", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireExclusiveLock(OSFileSystem{}, path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemFSExclusiveLockLifetime(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireExclusiveLock(fsys, "/state/owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireExclusiveLock(fsys, "/state/owner.lock"); !errors.Is(err, ErrExclusiveLockHeld) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second lock error = %v, want ErrExclusiveLockHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireExclusiveLock(fsys, "/state/owner.lock")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExclusiveLockHeldInDirectory(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	lock, err := AcquireExclusiveLockInDirectory(fsys, directory, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	lockIdentity, ok := fsys.FileIdentity(lockInfo)
	if !ok {
		t.Fatal("lock identity unavailable")
	}
	if err := ValidateExclusiveLockHeldInDirectory(fsys, directory, "owner.lock", lockIdentity); err != nil {
		t.Fatalf("validate held lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExclusiveLockHeldInDirectory(fsys, directory, "owner.lock", lockIdentity); !errors.Is(err, ErrExclusiveLockNotHeld) {
		t.Fatalf("released lock error = %v, want ErrExclusiveLockNotHeld", err)
	}
}

func TestValidateExclusiveLockHeldInDirectoryDoesNotCreateMissingLock(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := ValidateExclusiveLockHeldInDirectory(fsys, directory, "missing.lock", SecureFileIdentity{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lock error = %v, want not exist", err)
	}
	if _, err := fsys.Stat("/state/missing.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lock was created: %v", err)
	}
}

func TestSecureAtomicWriteInDirectoryCheckedRejectsFailedPrecondition(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := SecureAtomicWriteInDirectory(fsys, directory, "value", []byte("old")); err != nil {
		t.Fatal(err)
	}
	preconditionErr := errors.New("generation changed")
	checks := 0
	err = SecureAtomicWriteInDirectoryChecked(fsys, directory, "value", []byte("new"), func() error {
		checks++
		got, err := ReadSecureFileInDirectory(fsys, directory, "value", 64)
		if err != nil {
			return err
		}
		if string(got) != "old" {
			return fmt.Errorf("content = %q, want old", got)
		}
		return preconditionErr
	})
	if !errors.Is(err, preconditionErr) || !errors.Is(err, ErrCommitNotCommitted) {
		t.Fatalf("write error = %v, want precondition and not committed", err)
	}
	if checks != 1 {
		t.Fatalf("precondition calls = %d, want 1", checks)
	}
	got, err := ReadSecureFileInDirectory(fsys, directory, "value", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("content = %q, want old", got)
	}
	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "value" {
		t.Fatalf("entries = %#v, want only canonical value", entries)
	}
}

func TestSecureAtomicWriteRejectsTemporaryReplacementBeforeRename(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceTemporaryOnCloseDirectory{SecureDirectory: opened}
	defer directory.Close()
	err = SecureAtomicWriteInDirectory(fsys, directory, "value", []byte("trusted"))
	if err == nil || !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("write error = %v, want unsafe not-committed replacement", err)
	}
	if _, err := fsys.Stat("/state/value"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical replacement was committed: %v", err)
	}
	entries, err := fsys.ReadDir("/state")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("replacement temporary entries = %#v, want one preserved object", entries)
	}
	got, err := fsys.ReadFile(filepath.Join("/state", entries[0].Name()))
	if err != nil || string(got) != "attacker" {
		t.Fatalf("preserved replacement = %q, %v; want attacker", got, err)
	}
}

func TestSecureAtomicWriteRejectsInstalledReplacementDuringDirectorySync(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceInstalledOnSyncDirectory{SecureDirectory: opened, name: "value"}
	defer directory.Close()
	err = SecureAtomicWriteInDirectory(fsys, directory, "value", []byte("trusted"))
	if err == nil || !errors.Is(err, ErrCommitIndeterminate) || !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("write error = %v, want unsafe indeterminate replacement", err)
	}
}

func TestMemFSSecureAtomicWriteRejectsDirectoryDestination(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("/state/value", 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	err = SecureAtomicWriteInDirectory(fsys, directory, "value", []byte("trusted"))
	if err == nil || !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("write error = %v, want unsafe not-committed directory destination", err)
	}
	info, err := fsys.Stat("/state/value")
	if err != nil || !info.IsDir() {
		t.Fatalf("destination = %#v, %v; want retained directory", info, err)
	}
}

func TestEnsureSecureDirectorySyncsCreatedEntriesAndParents(t *testing.T) {
	t.Parallel()
	fsys := &directoryCreationRecordingFS{MemFS: NewMemFS()}
	if err := EnsureSecureDirectory(fsys, "/state/nested"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/state", "/", "/state/nested", "/state"}
	if !slices.Equal(fsys.synced, want) {
		t.Fatalf("synced paths = %v, want %v", fsys.synced, want)
	}
	if fsys.pathSyncCalled {
		t.Fatal("EnsureSecureDirectory used path-based SyncDir")
	}
	fsys.synced = nil
	fsys.pathSyncCalled = false
	if err := EnsureSecureDirectory(fsys, "/state/nested"); err != nil {
		t.Fatal(err)
	}
	want = []string{"/state/nested", "/state"}
	if !slices.Equal(fsys.synced, want) {
		t.Fatalf("existing directory syncs = %v, want %v", fsys.synced, want)
	}
	if fsys.pathSyncCalled {
		t.Fatal("EnsureSecureDirectory used path-based SyncDir on retry")
	}
}

func TestEnsureSecureDirectoryRejectsReplacementDuringParentSync(t *testing.T) {
	t.Parallel()
	fsys := &directoryCreationRecordingFS{MemFS: NewMemFS(), replaceOnParentSync: "/state"}
	err := EnsureSecureDirectory(fsys, "/state")
	if !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("EnsureSecureDirectory error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestEnsureSecureDirectoryRetrySealsEntryAfterParentSyncFailure(t *testing.T) {
	t.Parallel()
	fsys := &directoryCreationRecordingFS{MemFS: NewMemFS(), failSyncOnce: "/"}
	if err := EnsureSecureDirectory(fsys, "/state"); err == nil {
		t.Fatal("first EnsureSecureDirectory error = nil")
	}
	if info, err := fsys.Stat("/state"); err != nil || !info.IsDir() {
		t.Fatalf("created directory = %#v, %v; want retained directory", info, err)
	}
	fsys.synced = nil
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatalf("retry EnsureSecureDirectory: %v", err)
	}
	want := []string{"/state", "/"}
	if !slices.Equal(fsys.synced, want) {
		t.Fatalf("retry syncs = %v, want %v", fsys.synced, want)
	}
	if fsys.pathSyncCalled {
		t.Fatal("retry used path-based SyncDir")
	}
}

func TestEnsureSecureDirectoryRejectsSymlinkParentAndAcceptsCanonicalPath(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	root := t.TempDir()
	actualParent := filepath.Join(root, "actual")
	if err := os.Mkdir(actualParent, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(actualParent, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(actualParent, parentLink); err != nil {
		t.Fatal(err)
	}
	linkedState := filepath.Join(parentLink, "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, linkedState); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("symlink-parent error = %v, want ErrUnsafeSecurePath", err)
	}
	canonicalParent, err := filepath.EvalSymlinks(parentLink)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDirectory(OSFileSystem{}, filepath.Join(canonicalParent, "state")); err != nil {
		t.Fatalf("canonical secure directory: %v", err)
	}
}

func TestReadSecureFileInDirectoryWithIdentityReturnsOpenedIdentity(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/value", []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := fsys.Stat("/state/value")
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity, ok := fsys.FileIdentity(oldInfo)
	if !ok {
		t.Fatal("old identity unavailable")
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	data, identity, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, "value", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "same" || identity != oldIdentity {
		t.Fatalf("opened result = %q %+v, want same %+v", data, identity, oldIdentity)
	}
}

func TestReadSecureFileInDirectoryWithIdentityRejectsReplacementAfterOpen(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/value", []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := fsys.Stat("/state/value")
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity, ok := fsys.FileIdentity(oldInfo)
	if !ok {
		t.Fatal("old identity unavailable")
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceAfterOpenDirectory{SecureDirectory: opened, fsys: fsys, path: "/state"}
	defer directory.Close()
	if _, _, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, "value", 64); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("replacement read error = %v, want ErrUnsafeSecurePath", err)
	}
	currentInfo, err := fsys.Stat("/state/value")
	if err != nil {
		t.Fatal(err)
	}
	currentIdentity, ok := fsys.FileIdentity(currentInfo)
	if !ok || currentIdentity == oldIdentity {
		t.Fatalf("current identity = %+v, want replacement", currentIdentity)
	}
}

func TestReadSecureFileInDirectoryWithIdentityRejectsCanonicalEntryReplacementAfterRead(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/value", []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceAfterReadDirectory{SecureDirectory: opened}
	defer directory.Close()
	if _, _, err := ReadSecureFileInDirectoryWithIdentity(fsys, directory, "value", 64); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("replacement read error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestMemFSExclusiveLockRetainsOpenedIdentity(t *testing.T) {
	t.Parallel()
	fsys := &replacingMemLockFS{MemFS: NewMemFS()}
	if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireExclusiveLock(fsys, "/state/owner.lock")
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil {
		t.Fatal("AcquireExclusiveLock error = nil after MemFS lock path replacement")
	}
}

func TestMemFSDurableHandleDoesNotWriteReplacementPath(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := fsys.CreateExclusive("/state/value", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fsys.mu.Lock()
	fsys.files["/state/value"] = memFile{
		data:    []byte("replacement"),
		modTime: fsys.files["/state/value"].modTime,
		mode:    0o600,
		owner:   fsys.euid,
		inode:   fsys.allocateInodeLocked(),
	}
	fsys.mu.Unlock()
	if _, err := file.Write([]byte("opened")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := fsys.ReadFile("/state/value")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement content = %q, want replacement", got)
	}
}

func TestMemFSSecureDirectoryRetainsOpenedIdentity(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()

	fsys.mu.Lock()
	original := fsys.dirs["/state"]
	delete(fsys.dirs, "/state")
	fsys.dirs["/held"] = original
	fsys.dirs["/state"] = memFile{
		modTime: time.Now(),
		mode:    os.ModeDir | 0o700,
		owner:   fsys.euid,
		inode:   fsys.allocateInodeLocked(),
	}
	fsys.mu.Unlock()

	file, err := directory.CreateExclusive("value", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("opened")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("/state/value"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory file error = %v, want not exist", err)
	}
	if got, err := fsys.ReadFile("/held/value"); err != nil || string(got) != "opened" {
		t.Fatalf("opened directory content = %q, %v; want opened", got, err)
	}
}

func TestMemFSOpenSecureDirectoryRejectsPermissiveDescriptor(t *testing.T) {
	t.Parallel()
	fsys := NewMemFS()
	if err := fsys.MkdirAll("/state", 0o755); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/state")
	if directory != nil {
		_ = directory.Close()
	}
	if !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("OpenSecureDirectory error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestExclusiveLockRejectsMultiplyLinkedPath(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owner.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "other-link")); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireExclusiveLock(OSFileSystem{}, path)
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil {
		t.Fatal("AcquireExclusiveLock error = nil for multiply linked path")
	}
}

func TestExclusiveLockRejectsPathReplacementAfterAcquisition(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owner.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := &replacingLockFS{OSFileSystem: OSFileSystem{}}
	lock, err := AcquireExclusiveLock(fsys, path)
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil {
		t.Fatal("AcquireExclusiveLock error = nil after lock path replacement")
	}
}

func TestExclusiveLockRejectsDirectoryReplacementDuringAcquisition(t *testing.T) {
	requireUnixSecureFS(t)
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(OSFileSystem{}, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owner.lock")
	fsys := &replacingLockDirectoryFS{OSFileSystem: OSFileSystem{}}
	lock, err := AcquireExclusiveLock(fsys, path)
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil {
		t.Fatal("AcquireExclusiveLock error = nil after lock directory replacement")
	}
}

type foreignOwnerFS struct{ OSFileSystem }

type directoryCreationRecordingFS struct {
	*MemFS
	synced              []string
	replaceOnParentSync string
	failSyncOnce        string
	pathSyncCalled      bool
}

func (fsys *directoryCreationRecordingFS) SyncDir(path string) error {
	fsys.pathSyncCalled = true
	return fsys.MemFS.SyncDir(path)
}

func (fsys *directoryCreationRecordingFS) OpenDurableDirectory(path string) (DurableDirectory, error) {
	directory, err := fsys.MemFS.OpenDurableDirectory(path)
	if err != nil {
		return nil, err
	}
	return &recordingDurableDirectory{DurableDirectory: directory, fsys: fsys, path: cleanMemPath(path)}, nil
}

type recordingDurableDirectory struct {
	DurableDirectory
	fsys *directoryCreationRecordingFS
	path string
}

func (directory *recordingDurableDirectory) OpenDirectory(name string) (DurableDirectory, error) {
	child, err := directory.DurableDirectory.OpenDirectory(name)
	if err != nil {
		return nil, err
	}
	return &recordingDurableDirectory{DurableDirectory: child, fsys: directory.fsys, path: cleanMemPath(filepath.Join(directory.path, name))}, nil
}

func (directory *recordingDurableDirectory) Sync() error {
	if err := directory.DurableDirectory.Sync(); err != nil {
		return err
	}
	directory.fsys.synced = append(directory.fsys.synced, directory.path)
	if directory.path == cleanMemPath(directory.fsys.failSyncOnce) {
		directory.fsys.failSyncOnce = ""
		return errors.New("injected retained directory sync failure")
	}
	target := cleanMemPath(directory.fsys.replaceOnParentSync)
	if directory.fsys.replaceOnParentSync != "" && directory.path == cleanMemPath(filepath.Dir(target)) {
		directory.fsys.mu.Lock()
		original := directory.fsys.dirs[target]
		delete(directory.fsys.dirs, target)
		directory.fsys.dirs[target+".held"] = original
		replacement := original
		replacement.inode = directory.fsys.allocateInodeLocked()
		directory.fsys.dirs[target] = replacement
		directory.fsys.mu.Unlock()
		directory.fsys.replaceOnParentSync = ""
	}
	return nil
}

func requireUnixSecureFS(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix secure filesystem contract")
	}
	if _, ok := any(OSFileSystem{}).(SecurePathInspector); !ok {
		t.Skip("secure path inspection is unavailable on this platform")
	}
}

func (foreignOwnerFS) FileOwnerUID(os.FileInfo) (uint64, bool) {
	return uint64(os.Geteuid() + 1), true
}

type specialModeFS struct{ OSFileSystem }

func (fsys specialModeFS) Lstat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return specialModeFileInfo{FileInfo: info, mode: info.Mode() | os.ModeSetuid}, nil
}

type specialModeFileInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (info specialModeFileInfo) Mode() os.FileMode { return info.mode }

type readTrapFS struct {
	OSFileSystem
	readFileCalled bool
}

func (fsys *readTrapFS) ReadFile(string) ([]byte, error) {
	fsys.readFileCalled = true
	return []byte("untrusted"), nil
}

type replacingLockFS struct{ OSFileSystem }

func (fsys *replacingLockFS) OpenSecureDirectory(path string) (SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &replacingLockPathDirectory{SecureDirectory: directory}, nil
}

type replacingLockPathDirectory struct{ SecureDirectory }

func (directory *replacingLockPathDirectory) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	lock, err := directory.SecureDirectory.OpenExclusiveLock(name, mode)
	if err != nil {
		return nil, err
	}
	if err := directory.SecureDirectory.Rename(name, name+".held"); err != nil {
		_ = lock.Close()
		return nil, err
	}
	replacement, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := replacement.Close(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

type replaceAfterOpenDirectory struct {
	SecureDirectory
	fsys *MemFS
	path string
}

type replaceAfterReadDirectory struct{ SecureDirectory }

func (directory *replaceAfterReadDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	file, err := directory.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	return &replaceAfterReadFile{
		SecureReadFile: file,
		directory:      directory.SecureDirectory,
		name:           name,
	}, nil
}

type replaceAfterReadFile struct {
	SecureReadFile
	directory fsutilSecureDirectoryForTest
	name      string
	stats     int
}

type fsutilSecureDirectoryForTest interface {
	Rename(oldName, newName string) error
	CreateExclusive(name string, perm os.FileMode) (DurableFile, error)
}

func (file *replaceAfterReadFile) Stat() (os.FileInfo, error) {
	info, err := file.SecureReadFile.Stat()
	if err != nil {
		return nil, err
	}
	file.stats++
	if file.stats != 2 {
		return info, nil
	}
	if err := file.directory.Rename(file.name, file.name+".held"); err != nil {
		return nil, err
	}
	replacement, err := file.directory.CreateExclusive(file.name, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := replacement.Write([]byte("attacker")); err != nil {
		_ = replacement.Close()
		return nil, err
	}
	if err := replacement.Close(); err != nil {
		return nil, err
	}
	return info, nil
}

type replaceTemporaryOnCloseDirectory struct{ SecureDirectory }

type replaceInstalledOnSyncDirectory struct {
	SecureDirectory
	name string
}

func (directory *replaceInstalledOnSyncDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *replaceInstalledOnSyncDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *replaceInstalledOnSyncDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *replaceInstalledOnSyncDirectory) Sync() error {
	if err := directory.SecureDirectory.Sync(); err != nil {
		return err
	}
	if err := directory.SecureDirectory.Remove(directory.name); err != nil {
		return err
	}
	replacement, err := directory.SecureDirectory.CreateExclusive(directory.name, 0o600)
	if err != nil {
		return err
	}
	return replacement.Close()
}

func (directory *replaceTemporaryOnCloseDirectory) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &replaceTemporaryOnCloseFile{DurableFile: file, directory: directory.SecureDirectory, name: name, mode: mode}, nil
}

type replaceTemporaryOnCloseFile struct {
	DurableFile
	directory SecureDirectory
	name      string
	mode      os.FileMode
}

func (directory *replaceTemporaryOnCloseDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *replaceTemporaryOnCloseDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *replaceTemporaryOnCloseDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(name, expected)
}

func (file *replaceTemporaryOnCloseFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(DurableFileInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *replaceTemporaryOnCloseFile) Close() error {
	if err := file.DurableFile.Close(); err != nil {
		return err
	}
	if err := file.directory.Remove(file.name); err != nil {
		return err
	}
	replacement, err := file.directory.CreateExclusive(file.name, file.mode)
	if err != nil {
		return err
	}
	if _, err := replacement.Write([]byte("attacker")); err != nil {
		_ = replacement.Close()
		return err
	}
	if err := replacement.Sync(); err != nil {
		_ = replacement.Close()
		return err
	}
	return replacement.Close()
}

func (directory *replaceAfterOpenDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	file, err := directory.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory.path, name)
	if err := directory.fsys.Remove(path); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := directory.fsys.WriteFile(path, []byte("same"), 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (directory *replaceAfterOpenDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *replaceAfterOpenDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *replaceAfterOpenDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(name, expected)
}

type replacingLockDirectoryFS struct{ OSFileSystem }

func (fsys *replacingLockDirectoryFS) OpenSecureDirectory(path string) (SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &replacingLockNamespaceDirectory{SecureDirectory: directory, path: path}, nil
}

type replacingLockNamespaceDirectory struct {
	SecureDirectory
	path string
}

func (directory *replacingLockNamespaceDirectory) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	if err := os.Rename(directory.path, directory.path+".held"); err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory.path, 0o700); err != nil {
		return nil, err
	}
	return directory.SecureDirectory.OpenExclusiveLock(name, mode)
}

type faultSecureFS struct {
	*MemFS
	createPaths []string
	failStep    string
}

type replacingDirectoryFS struct {
	*MemFS
	replaceAfterRename bool
}

func (fsys *replacingDirectoryFS) OpenSecureDirectory(path string) (SecureDirectory, error) {
	info, err := fsys.Stat(path)
	if err != nil {
		return nil, err
	}
	return &replacingSecureDirectory{fsys: fsys, path: cleanMemPath(path), info: info}, nil
}

type replacingSecureDirectory struct {
	fsys     *replacingDirectoryFS
	path     string
	info     os.FileInfo
	replaced bool
}

func (dir *replacingSecureDirectory) Stat() (os.FileInfo, error) { return dir.info, nil }

func (dir *replacingSecureDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	return dir.fsys.MemFS.OpenNoFollow(filepath.Join(dir.path, name))
}

func (dir *replacingSecureDirectory) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	file, err := dir.fsys.MemFS.CreateExclusive(filepath.Join(dir.path, name), mode)
	if err != nil {
		return nil, err
	}
	if dir.fsys.replaceAfterRename {
		return file, nil
	}
	return &replaceDirectoryOnSyncFile{DurableFile: file, directory: dir}, nil
}

func (dir *replacingSecureDirectory) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	return dir.fsys.MemFS.OpenExclusiveLock(filepath.Join(dir.path, name), mode)
}

func (dir *replacingSecureDirectory) Rename(oldName, newName string) error {
	if err := dir.fsys.MemFS.Rename(filepath.Join(dir.path, oldName), filepath.Join(dir.path, newName)); err != nil {
		return err
	}
	if dir.fsys.replaceAfterRename {
		dir.replacePath()
	}
	return nil
}

func (dir *replacingSecureDirectory) RenameNoReplace(oldName, newName string) error {
	opened, err := dir.fsys.MemFS.OpenSecureDirectory(dir.path)
	if err != nil {
		return err
	}
	defer opened.Close()
	if err := opened.RenameNoReplace(oldName, newName); err != nil {
		return err
	}
	if dir.fsys.replaceAfterRename {
		dir.replacePath()
	}
	return nil
}

func (dir *replacingSecureDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	opened, err := dir.fsys.MemFS.OpenSecureDirectory(dir.path)
	if err != nil {
		return err
	}
	defer opened.Close()
	if err := opened.(IdentityBoundRenamer).RenameChecked(oldName, newName, expected); err != nil {
		return err
	}
	if dir.fsys.replaceAfterRename {
		dir.replacePath()
	}
	return nil
}

func (dir *replacingSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	opened, err := dir.fsys.MemFS.OpenSecureDirectory(dir.path)
	if err != nil {
		return err
	}
	defer opened.Close()
	if err := opened.(IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected); err != nil {
		return err
	}
	if dir.fsys.replaceAfterRename {
		dir.replacePath()
	}
	return nil
}

func (dir *replacingSecureDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	opened, err := dir.fsys.MemFS.OpenSecureDirectory(dir.path)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.(IdentityBoundRemover).RemoveChecked(name, expected)
}

func (dir *replacingSecureDirectory) Remove(name string) error {
	return dir.fsys.MemFS.Remove(filepath.Join(dir.path, name))
}

func (dir *replacingSecureDirectory) Sync() error  { return dir.fsys.MemFS.SyncDir(dir.path) }
func (dir *replacingSecureDirectory) Close() error { return nil }

func (dir *replacingSecureDirectory) replacePath() {
	if dir.replaced {
		return
	}
	dir.replaced = true
	heldPath := dir.path + ".held"
	dir.fsys.mu.Lock()
	defer dir.fsys.mu.Unlock()
	original := dir.fsys.dirs[dir.path]
	delete(dir.fsys.dirs, dir.path)
	dir.fsys.dirs[heldPath] = original
	prefix := dir.path + "/"
	for path, file := range dir.fsys.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		delete(dir.fsys.files, path)
		dir.fsys.files[heldPath+strings.TrimPrefix(path, dir.path)] = file
	}
	dir.fsys.dirs[dir.path] = memFile{
		modTime: original.modTime,
		mode:    os.ModeDir | 0o700,
		owner:   dir.fsys.euid,
		inode:   dir.fsys.allocateInodeLocked(),
	}
	dir.path = heldPath
}

type replaceDirectoryOnSyncFile struct {
	DurableFile
	directory *replacingSecureDirectory
}

func (file *replaceDirectoryOnSyncFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(DurableFileInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *replaceDirectoryOnSyncFile) Sync() error {
	if err := file.DurableFile.Sync(); err != nil {
		return err
	}
	file.directory.replacePath()
	return nil
}

type replacingMemLockFS struct{ *MemFS }

func (fsys *replacingMemLockFS) OpenSecureDirectory(path string) (SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &replacingLockPathDirectory{SecureDirectory: directory}, nil
}

func (fsys *faultSecureFS) OpenSecureDirectory(path string) (SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &faultSecureDirectory{SecureDirectory: directory, fsys: fsys, path: path}, nil
}

type faultSecureDirectory struct {
	SecureDirectory
	fsys *faultSecureFS
	path string
}

func (directory *faultSecureDirectory) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	directory.fsys.createPaths = append(directory.fsys.createPaths, filepath.Join(directory.path, name))
	if directory.fsys.failStep == "create" {
		return nil, fmt.Errorf("injected create failure")
	}
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &faultDurableFile{DurableFile: file, failStep: directory.fsys.failStep}, nil
}

func (directory *faultSecureDirectory) Rename(oldName, newName string) error {
	if directory.fsys.failStep == "rename" {
		return fmt.Errorf("injected rename failure")
	}
	return directory.SecureDirectory.Rename(oldName, newName)
}

func (directory *faultSecureDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	if directory.fsys.failStep == "rename" {
		return fmt.Errorf("injected rename failure")
	}
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *faultSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	if directory.fsys.failStep == "rename" {
		return fmt.Errorf("injected rename failure")
	}
	return directory.SecureDirectory.(IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *faultSecureDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *faultSecureDirectory) Sync() error {
	if directory.fsys.failStep == "sync-dir" {
		return fmt.Errorf("injected directory sync failure")
	}
	return directory.SecureDirectory.Sync()
}

type faultDurableFile struct {
	DurableFile
	failStep string
}

func (file *faultDurableFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(DurableFileInspector)
	if !ok {
		return nil, ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *faultDurableFile) Write(data []byte) (int, error) {
	if file.failStep == "write" {
		return 0, fmt.Errorf("injected write failure")
	}
	return file.DurableFile.Write(data)
}

func (file *faultDurableFile) Sync() error {
	if file.failStep == "sync" {
		return fmt.Errorf("injected sync failure")
	}
	return file.DurableFile.Sync()
}
