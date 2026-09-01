//go:build windows

package installer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestWindowsInstallerTemporaryDirectoryIsPrivate(t *testing.T) {
	root := newWindowsInstallerTestRoot(t)
	temporary := OSTemporaryDirectories{Root: filepath.Join(root, "cache", "installer")}
	directory, err := temporary.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, directory); err != nil {
		t.Fatalf("staging directory is not private: %v", err)
	}
	if err := temporary.Remove(directory); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsStagedExecutableIsPrivate(t *testing.T) {
	directory := filepath.Join(newWindowsInstallerTestRoot(t), "staging")
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, directory); err != nil {
		t.Fatal(err)
	}
	staged, err := writeStagedExecutable(directory, "cq.exe", bytes.NewReader([]byte("cq")), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureRegularFile(fsutil.OSFileSystem{}, staged.Path); err != nil {
		t.Fatalf("staged executable is not private: %v", err)
	}
}

func newWindowsInstallerTestRoot(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	cqRoot := filepath.Join(cache, "cq")
	_, statErr := os.Stat(cqRoot)
	cqRootExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal(statErr)
	}
	root := filepath.Join(cqRoot, "installer-test-"+hex.EncodeToString(random))
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test root: %v", err)
		}
		if !cqRootExisted {
			if err := os.Remove(cqRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove test CQ root: %v", err)
			}
		}
	})
	return root
}
