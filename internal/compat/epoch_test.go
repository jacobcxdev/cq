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

func TestCurrentEpochThreeRejectsFloorTwoBinary(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/state/compatibility_epoch"
	if CurrentEpoch != 3 {
		t.Fatalf("CurrentEpoch = %d, want 3", CurrentEpoch)
	}
	if err := EnsureEpoch(fs, path, CurrentEpoch); err != nil {
		t.Fatalf("record floor 3: %v", err)
	}
	if err := EnsureEpoch(fs, path, 2); !errors.Is(err, ErrIncompatibleEpoch) {
		t.Fatalf("floor-2 binary error = %v, want ErrIncompatibleEpoch", err)
	}
}

func TestEnsureEpochRequiresSecureCurrentRead(t *testing.T) {
	base := fsutil.NewMemFS()
	path := "/state/compatibility_epoch"
	if err := base.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteFile(path, []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := struct{ fsutil.FileSystem }{FileSystem: base}
	if err := EnsureEpoch(fs, path, 3); !errors.Is(err, fsutil.ErrSecureCapabilityUnavailable) {
		t.Fatalf("EnsureEpoch error = %v, want ErrSecureCapabilityUnavailable", err)
	}
}

func TestEnsureEpochUsesUniqueDurableWriter(t *testing.T) {
	fs := &recordingEpochFS{MemFS: fsutil.NewMemFS()}
	path := "/state/compatibility_epoch"
	if err := EnsureEpoch(fs, path, 3); err != nil {
		t.Fatal(err)
	}
	if len(fs.createPaths) != 1 {
		t.Fatalf("exclusive temporary paths = %v, want one", fs.createPaths)
	}
	if fs.createPaths[0] == path+".tmp" {
		t.Fatalf("fixed temporary path used %q", fs.createPaths[0])
	}
	if filepath.Dir(fs.createPaths[0]) != filepath.Dir(path) {
		t.Fatalf("temporary directory = %q, want %q", filepath.Dir(fs.createPaths[0]), filepath.Dir(path))
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

type recordingEpochFS struct {
	*fsutil.MemFS
	createPaths []string
}

func (fs *recordingEpochFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fs.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &recordingEpochDirectory{SecureDirectory: directory, fs: fs, path: path}, nil
}

type recordingEpochDirectory struct {
	fsutil.SecureDirectory
	fs   *recordingEpochFS
	path string
}

func (directory *recordingEpochDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	directory.fs.createPaths = append(directory.fs.createPaths, filepath.Join(directory.path, name))
	return directory.SecureDirectory.CreateExclusive(name, mode)
}

func (f failingFS) WriteFile(string, []byte, os.FileMode) error { return f.writeErr }

func TestEnsureEpochFailsClosedOnWriteFailure(t *testing.T) {
	fs := failingFS{FileSystem: fsutil.NewMemFS(), writeErr: os.ErrPermission}
	if err := EnsureEpoch(fs, "/state/compatibility_epoch", 1); err == nil {
		t.Fatal("EnsureEpoch error = nil")
	}
}
