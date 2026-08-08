package compat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestEnsureEpochCreatesCurrentFloor(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/state/compatibility_epoch"
	if err := EnsureEpoch(fs, path, 1); err != nil {
		t.Fatalf("EnsureEpoch: %v", err)
	}
	got, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "1\n" {
		t.Fatalf("epoch = %q, want 1", got)
	}
}

func TestEnsureEpochAdvancesAndRejectsOlderBinary(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/state/compatibility_epoch"
	if err := fs.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureEpoch(fs, path, 2); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := EnsureEpoch(fs, path, 1); !errors.Is(err, ErrIncompatibleEpoch) {
		t.Fatalf("older binary error = %v, want ErrIncompatibleEpoch", err)
	}
}

func TestEnsureEpochRejectsCorruptState(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/state/compatibility_epoch"
	if err := fs.WriteFile(path, []byte("not-an-integer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureEpoch(fs, path, 1); err == nil {
		t.Fatal("EnsureEpoch error = nil")
	}
}

func TestDefaultEpochPathUsesAbsoluteXDGConfigHome(t *testing.T) {
	fs := fsutil.NewMemFS()
	got, err := DefaultEpochPath(fs, func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return "/custom/config"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/custom/config", "cq", "state", "compatibility_epoch"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

type failingFS struct {
	fsutil.FileSystem
	writeErr error
}

func (f failingFS) WriteFile(string, []byte, os.FileMode) error { return f.writeErr }

func TestEnsureEpochFailsClosedOnWriteFailure(t *testing.T) {
	fs := failingFS{FileSystem: fsutil.NewMemFS(), writeErr: os.ErrPermission}
	if err := EnsureEpoch(fs, "/state/compatibility_epoch", 1); err == nil {
		t.Fatal("EnsureEpoch error = nil")
	}
}
