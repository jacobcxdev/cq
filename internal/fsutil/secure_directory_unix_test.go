//go:build unix

package fsutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestUnixSecureDirectoryRenameNoReplaceHasOneWinner(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"first": "one", "second": "two"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()

	var wait sync.WaitGroup
	errorsBySource := make([]error, 2)
	for index, source := range []string{"first", "second"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsBySource[index] = directory.RenameNoReplace(source, "canonical")
		}()
	}
	wait.Wait()
	winners := 0
	losers := 0
	for _, err := range errorsBySource {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, os.ErrExist):
			losers++
		case errors.Is(err, ErrSecureCapabilityUnavailable):
			t.Skip("kernel no-replace rename is unavailable")
		default:
			t.Fatalf("rename error = %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("rename winners/losers = %d/%d, want 1/1", winners, losers)
	}
	got, err := os.ReadFile(filepath.Join(state, "canonical"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one" && string(got) != "two" {
		t.Fatalf("canonical content = %q", got)
	}
}

func TestUnixSecureDirectoryRetainsOpenedIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	held := filepath.Join(root, "held")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(state, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}

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
	if _, err := os.Stat(filepath.Join(state, "value")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory file error = %v, want not exist", err)
	}
	if got, err := os.ReadFile(filepath.Join(held, "value")); err != nil || string(got) != "opened" {
		t.Fatalf("opened directory content = %q, %v; want opened", got, err)
	}
}

type directoryLinkCountChangingFS struct {
	OSFileSystem
	path  string
	child string
	once  sync.Once
	err   error
}

func (fsys *directoryLinkCountChangingFS) Lstat(name string) (os.FileInfo, error) {
	if name == fsys.path {
		fsys.once.Do(func() { fsys.err = os.Mkdir(fsys.child, 0o700) })
		if fsys.err != nil {
			return nil, fsys.err
		}
	}
	return fsys.OSFileSystem.Lstat(name)
}

func TestValidateSecureDirectoryHandleAllowsLinkCountChange(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	fsys := &directoryLinkCountChangingFS{
		path:  state,
		child: filepath.Join(state, "child"),
	}
	if err := ValidateSecureDirectoryHandle(fsys, directory, state); err != nil {
		t.Fatalf("validate retained directory after child creation: %v", err)
	}
}

func TestValidateRetainedDirectoryPathAllowsLinkCountChange(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenDurableDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	fsys := &directoryLinkCountChangingFS{
		path:  state,
		child: filepath.Join(state, "child"),
	}
	if err := validateRetainedDirectoryPath(fsys, directory, state, true); err != nil {
		t.Fatalf("validate retained directory after child creation: %v", err)
	}
}

func TestUnixRenameNoReplaceCheckedRestoresSourceWhenDestinationExists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"source": "source", "destination": "destination"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	identity := unixTestFileIdentity(t, filepath.Join(state, "source"))
	err = directory.(IdentityBoundRenamer).RenameNoReplaceChecked("source", "destination", identity)
	if errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Skip("kernel no-replace rename is unavailable")
	}
	if !errors.Is(err, os.ErrExist) || errors.Is(err, ErrCommitIndeterminate) {
		t.Fatalf("rename error = %v, want existing destination with successful restore", err)
	}
	assertUnixIdentityQuarantineShape(t, state, map[string]string{"source": "source", "destination": "destination"})
}

func TestUnixIdentityBoundOperationsRestoreMismatchedSource(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(SecureDirectory, SecureFileIdentity) error
	}{
		{name: "rename", run: func(directory SecureDirectory, identity SecureFileIdentity) error {
			return directory.(IdentityBoundRenamer).RenameChecked("source", "destination", identity)
		}},
		{name: "remove", run: func(directory SecureDirectory, identity SecureFileIdentity) error {
			return directory.(IdentityBoundRemover).RemoveChecked("source", identity)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "source"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			identity := unixTestFileIdentity(t, filepath.Join(state, "source"))
			identity.Inode++
			err = test.run(directory, identity)
			if !errors.Is(err, ErrUnsafeSecurePath) || errors.Is(err, ErrCommitIndeterminate) {
				t.Fatalf("operation error = %v, want unsafe path with successful restore", err)
			}
			assertUnixIdentityQuarantineShape(t, state, map[string]string{"source": "source"})
		})
	}
}

func TestRestoreIdentityQuarantineReportsRetainedNameOnFailure(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{".cq-quarantine-known": "trusted", "source": "replacement"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	cause := errors.New("injected second rename failure")
	err = restoreIdentityQuarantine(".cq-quarantine-known", "source", cause, directory.RenameNoReplace)
	if !errors.Is(err, ErrCommitIndeterminate) || !strings.Contains(err.Error(), `.cq-quarantine-known`) {
		t.Fatalf("restore error = %v, want indeterminate named quarantine", err)
	}
	assertUnixIdentityQuarantineShape(t, state, map[string]string{".cq-quarantine-known": "trusted", "source": "replacement"})
}

func unixTestFileIdentity(t *testing.T, path string) SecureFileIdentity {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := (OSFileSystem{}).FileIdentity(info)
	if !ok {
		t.Fatal("file identity unavailable")
	}
	return identity
}

func assertUnixIdentityQuarantineShape(t *testing.T, state string, expected map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(expected) {
		t.Fatalf("entries = %v, want %v", entries, expected)
	}
	for _, entry := range entries {
		value, ok := expected[entry.Name()]
		if !ok {
			t.Fatalf("unexpected entry %q (expected %v)", entry.Name(), expected)
		}
		body, err := os.ReadFile(filepath.Join(state, entry.Name()))
		if err != nil || string(body) != value {
			t.Fatalf("entry %q = %q, %v; want %q", entry.Name(), body, err, value)
		}
	}
}

func TestSecureAtomicWriteInDirectoryStaysInOpenedNamespace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	held := filepath.Join(root, "held")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(state, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := SecureAtomicWriteInDirectory(OSFileSystem{}, directory, "value", []byte("trusted")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "value")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory file error = %v, want not exist", err)
	}
	if got, err := os.ReadFile(filepath.Join(held, "value")); err != nil || string(got) != "trusted" {
		t.Fatalf("opened directory content = %q, %v; want trusted", got, err)
	}
}

func TestReadSecureFileInDirectoryStaysInOpenedNamespace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	held := filepath.Join(root, "held")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "value"), []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(state, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "value"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSecureFileInDirectory(OSFileSystem{}, directory, "value", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "trusted" {
		t.Fatalf("content = %q, want trusted", got)
	}
}

func TestAcquireExclusiveLockInDirectoryStaysInOpenedNamespace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	held := filepath.Join(root, "held")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(state, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireExclusiveLockInDirectory(OSFileSystem{}, directory, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := os.Stat(filepath.Join(state, "owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory lock error = %v, want not exist", err)
	}
	if info, err := os.Stat(filepath.Join(held, "owner.lock")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("opened directory lock = %#v, %v", info, err)
	}
	second, err := AcquireExclusiveLockInDirectory(OSFileSystem{}, directory, "owner.lock")
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrExclusiveLockHeld) {
		t.Fatalf("second lock error = %v, want ErrExclusiveLockHeld", err)
	}
}

func TestUnixValidateExclusiveLockHeldInDirectory(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	lock, err := AcquireExclusiveLockInDirectory(OSFileSystem{}, directory, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	lockIdentity, ok := (OSFileSystem{}).FileIdentity(lockInfo)
	if !ok {
		t.Fatal("lock identity unavailable")
	}
	if err := ValidateExclusiveLockHeldInDirectory(OSFileSystem{}, directory, "owner.lock", lockIdentity); err != nil {
		t.Fatalf("validate held lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExclusiveLockHeldInDirectory(OSFileSystem{}, directory, "owner.lock", lockIdentity); !errors.Is(err, ErrExclusiveLockNotHeld) {
		t.Fatalf("released lock error = %v, want ErrExclusiveLockNotHeld", err)
	}
}

func TestUnixAcquireNewExclusiveLockNeverOpensExistingFile(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(state, "maintenance.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := (OSFileSystem{}).OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	lock, err := AcquireNewExclusiveLockInDirectory(OSFileSystem{}, directory, "maintenance.lock")
	if lock != nil {
		_ = lock.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("create error = %v, want already exists", err)
	}
}

func TestUnixCheckedDirectoryRemovalAndRestoration(t *testing.T) {
	fsys := OSFileSystem{}
	root := t.TempDir()
	directory, err := OpenOwnerControlledDirectory(fsys, root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	rename := directory.(IdentityBoundRenamer)
	remove := directory.(IdentityBoundRemover)
	for _, name := range []string{"empty", "nonempty", "replacement"} {
		if err = os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity := func(name string) SecureFileIdentity {
		t.Helper()
		info, err := fsys.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		value, ok := fsys.FileIdentity(info)
		if !ok {
			t.Fatal("missing identity")
		}
		return value
	}
	emptyID := identity("empty")
	if err = rename.RenameNoReplaceChecked("empty", "renamed", emptyID); err != nil {
		t.Fatal(err)
	}
	if err = remove.RemoveChecked("renamed", emptyID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "nonempty", "owned"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := identity("nonempty")
	if err = remove.RemoveChecked("nonempty", id); err == nil {
		t.Fatal("removed nonempty directory")
	}
	if value, err := os.ReadFile(filepath.Join(root, "nonempty", "owned")); err != nil || string(value) != "retained" {
		t.Fatalf("nonempty restoration=%q %v", value, err)
	}
	wrong := identity("replacement")
	wrong.Inode++
	if err = remove.RemoveChecked("replacement", wrong); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("wrong identity=%v", err)
	}
	if err = rename.RenameNoReplaceChecked("replacement", "nonempty", identity("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination=%v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "replacement")); err != nil {
		t.Fatalf("rename source not restored: %v", err)
	}
	if err = os.Symlink(filepath.Join(root, "nonempty"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err = remove.RemoveChecked("link", identity("link")); err == nil {
		t.Fatal("removed symlink through checked directory path")
	}
	if _, err = os.Lstat(filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink not restored: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cq-quarantine-") {
			t.Fatalf("quarantine remains %s", entry.Name())
		}
	}
}

func TestUnixFinalFileRemovalCommitBoundary(t *testing.T) {
	for _, which := range []string{"success", "cancel_before", "cancel_quarantine", "unlink_failure", "collision", "wrong_identity", "symlink", "directory"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "checkpoint")
			if err := os.WriteFile(path, []byte("authenticated checkpoint"), 0o600); err != nil {
				t.Fatal(err)
			}
			if which == "symlink" {
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			if which == "directory" {
				_ = os.Remove(path)
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			directory, err := OpenOwnerControlledDirectory(OSFileSystem{}, root)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := (OSFileSystem{}).FileIdentity(info)
			if which == "wrong_identity" {
				id.Inode++
			}
			if which == "collision" {
				if err = os.WriteFile(filepath.Join(root, "final"), []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old := finalFileUnlink
			defer func() { finalFileUnlink = old }()
			if which == "cancel_before" {
				cancel()
			}
			if which == "unlink_failure" || which == "cancel_quarantine" {
				finalFileUnlink = func(int, string, int) error {
					if which == "cancel_quarantine" {
						cancel()
					}
					return syscall.EIO
				}
			}
			committed, err := directory.(FinalFileRemover).RemoveFinalFile(ctx, "checkpoint", "final", id)
			if which == "success" {
				if err != nil || !committed {
					t.Fatalf("commit=%t err=%v", committed, err)
				}
				if _, err = os.Stat(filepath.Join(root, "final")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("final leaf survived")
				}
				return
			}
			if err == nil || committed {
				t.Fatalf("unexpected commit=%t err=%v", committed, err)
			}
			leaf := "checkpoint"
			if which == "unlink_failure" || which == "cancel_quarantine" {
				leaf = "final"
			}
			if _, err = os.Lstat(filepath.Join(root, leaf)); err != nil {
				t.Fatalf("checkpoint lost: %v", err)
			}
		})
	}
}

func TestOwnerControlledAtomicCheckpointCreate(t *testing.T) {
	for _, which := range []string{"private", "public_read", "writable", "symlink", "collision", "cancel"} {
		t.Run(which, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "parent")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if which == "public_read" {
				_ = os.Chmod(root, 0o755)
			}
			if which == "writable" {
				_ = os.Chmod(root, 0o777)
			}
			if which == "symlink" {
				target := root
				root += "-link"
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			}
			directory, err := OpenOwnerControlledDirectory(OSFileSystem{}, root)
			if which == "writable" || which == "symlink" {
				if err == nil {
					directory.Close()
					t.Fatal("unsafe parent accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			if which == "collision" {
				if err = os.WriteFile(filepath.Join(root, "checkpoint"), []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if which == "cancel" {
				cancel()
			}
			err = SecureAtomicCreateInOwnerControlledDirectory(OSFileSystem{}, directory, root, "checkpoint", []byte("checkpoint"), ctx.Err)
			if which == "collision" || which == "cancel" {
				if err == nil {
					t.Fatal("unexpected publication")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(root, "checkpoint"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%v err=%v", info, err)
			}
		})
	}
}

type checkpointWrongOwner struct{ OSFileSystem }

func (checkpointWrongOwner) FileOwnerUID(os.FileInfo) (uint64, bool) {
	return uint64(os.Geteuid()) + 1, true
}
func TestOwnerControlledCheckpointWrongOwner(t *testing.T) {
	root := t.TempDir()
	d, err := OpenOwnerControlledDirectory(OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = SecureAtomicCreateInOwnerControlledDirectory(checkpointWrongOwner{}, d, root, "checkpoint", []byte("secret"), nil); err == nil {
		t.Fatal("wrong owner accepted")
	}
	if _, err = os.Stat(filepath.Join(root, "checkpoint")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong owner published: %v", err)
	}
}
