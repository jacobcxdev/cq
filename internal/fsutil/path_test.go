package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsCleanAbsoluteNonRootPath(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	valid := filepath.Join(volumeRoot, "cq-state")
	tests := map[string]bool{
		"":                                 false,
		"relative":                         false,
		volumeRoot:                         false,
		valid:                              true,
		valid + string(filepath.Separator): false,
	}
	for path, want := range tests {
		if got := IsCleanAbsoluteNonRootPath(path); got != want {
			t.Errorf("IsCleanAbsoluteNonRootPath(%q) = %t, want %t", path, got, want)
		}
	}
}
