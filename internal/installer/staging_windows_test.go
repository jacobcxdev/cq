//go:build windows

package installer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestWindowsInstallerStagingIsPrivate(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cache, "cq-installer-test-"+hex.EncodeToString(random))
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test root: %v", err)
		}
	})

	temporary := OSTemporaryDirectories{Root: filepath.Join(root, "cache", "installer")}
	directory, err := temporary.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, directory); err != nil {
		t.Fatalf("staging directory is not private: %v", err)
	}

	staged, err := writeStagedExecutable(directory, "cq.exe", bytes.NewReader([]byte("cq")), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureRegularFile(fsutil.OSFileSystem{}, staged.Path); err != nil {
		t.Fatalf("staged executable is not private: %v", err)
	}
	if err := temporary.Remove(directory); err != nil {
		t.Fatal(err)
	}
}
