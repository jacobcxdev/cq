//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOSFileSystemSyncDirWindows(t *testing.T) {
	fsys := OSFileSystem{}
	root := t.TempDir()
	if err := fsys.SyncDir(root); err != nil {
		t.Fatalf("SyncDir(%q) error = %v", root, err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.SyncDir(file); !errors.Is(err, windows.ERROR_DIRECTORY) {
		t.Fatalf("SyncDir(%q) error = %v, want not a directory", file, err)
	}

	missing := filepath.Join(root, "missing")
	if err := fsys.SyncDir(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncDir(%q) error = %v, want not exist", missing, err)
	}
}
