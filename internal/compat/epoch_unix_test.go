//go:build unix

package compat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestEnsureEpochRejectsUnsafeCurrentFloor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, dir, path string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, dir, path string) {
				t.Helper()
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("3\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "permissive file",
			setup: func(t *testing.T, _, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("3\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "compatibility_epoch")
			test.setup(t, dir, path)
			if err := EnsureEpoch(fsutil.OSFileSystem{}, path, CurrentEpoch); err == nil {
				t.Fatal("EnsureEpoch error = nil for unsafe current floor")
			}
		})
	}
}
