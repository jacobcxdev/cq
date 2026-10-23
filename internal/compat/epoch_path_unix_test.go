//go:build unix

package compat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestContinuityEpochDirUsesOnlyConfigInputs(t *testing.T) {
	fs := noHomeEpochFS{FileSystem: fsutil.NewMemFS(), t: t}
	dir := t.TempDir()
	env := func(name string) string {
		if name != "XDG_CONFIG_HOME" {
			t.Fatalf("unused env %s", name)
		}
		return dir
	}
	if _, err := DefaultEpochPath(nil, env); err != nil {
		t.Fatalf("unused filesystem required: %v", err)
	}
	got, err := DefaultEpochPath(fs, env)
	if err != nil || got != filepath.Join(dir, "cq", "state", "compatibility_epoch") {
		t.Fatalf("path %q, %v", got, err)
	}
	if _, err := DefaultEpochPath(fs, func(string) string { return "relative-private" }); err == nil {
		t.Fatal("relative config accepted")
	}
}

type noHomeEpochFS struct {
	fsutil.FileSystem
	t *testing.T
}

func (f noHomeEpochFS) UserHomeDir() (string, error) {
	f.t.Fatal("read unused home")
	return "", os.ErrNotExist
}
