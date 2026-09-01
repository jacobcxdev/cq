//go:build windows

package compat

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDefaultEpochPathUsesWindowsStateRoot(t *testing.T) {
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultEpochPath(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(roots.State, "compatibility_epoch"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
