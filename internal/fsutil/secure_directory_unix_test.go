//go:build unix

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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
