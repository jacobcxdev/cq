//go:build !unix && !windows

package fsutil

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformSecureCapabilitiesFailClosed(t *testing.T) {
	fsys := OSFileSystem{}
	dir := t.TempDir()
	if err := ValidateSecureDirectory(fsys, dir); !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("directory validation error = %v, want ErrSecureCapabilityUnavailable", err)
	}
	if err := SecureAtomicWrite(fsys, filepath.Join(dir, "state"), []byte("state")); !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("secure write error = %v, want ErrSecureCapabilityUnavailable", err)
	}
	if _, err := ReadSecureFile(fsys, filepath.Join(dir, "state"), 64); !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("secure read error = %v, want ErrSecureCapabilityUnavailable", err)
	}
	directory, err := fsys.OpenSecureDirectory(dir)
	if directory != nil {
		_ = directory.Close()
	}
	if !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("secure directory error = %v, want ErrSecureCapabilityUnavailable", err)
	}
	lock, err := AcquireExclusiveLock(fsys, filepath.Join(dir, "owner.lock"))
	if lock != nil {
		_ = lock.Close()
	}
	if !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("lock error = %v, want ErrSecureCapabilityUnavailable", err)
	}
}
