//go:build !windows

package app

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func resetAutomationTestDir(t *testing.T) (string, fsutil.DurableFileSystem) {
	t.Helper()
	return t.TempDir(), fsutil.OSFileSystem{}
}
