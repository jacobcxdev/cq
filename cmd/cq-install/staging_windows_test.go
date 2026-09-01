//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestWindowsInstallerStagingBypassesLegacyCacheRoot(t *testing.T) {
	base := newWindowsInstallerRoutingTestRoot(t)
	legacy := filepath.Join(base, "cache", "installer")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, legacy); err == nil {
		t.Fatal("legacy cache fixture is unexpectedly private")
	}

	roots := userdirs.Roots{
		Cache: filepath.Join(base, "cache"),
		State: filepath.Join(base, "state"),
	}
	temporary := installer.OSTemporaryDirectories{Root: installerTemporaryRoot(roots)}
	directory, err := temporary.Create()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(directory) != filepath.Join(roots.State, "installer") {
		t.Fatalf("staging parent = %q, want private state root", filepath.Dir(directory))
	}
	if err := temporary.Remove(directory); err != nil {
		t.Fatal(err)
	}
}

func newWindowsInstallerRoutingTestRoot(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cqRoot := filepath.Join(cache, "cq")
	_, statErr := os.Stat(cqRoot)
	cqRootExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal(statErr)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cqRoot, "installer-routing-test-"+hex.EncodeToString(random))
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, root); err != nil {
		t.Fatal(err)
	}
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
