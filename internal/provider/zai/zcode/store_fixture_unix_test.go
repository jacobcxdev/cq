//go:build !windows

package zcode

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func newZCodeStoreTestFS(t *testing.T) (fsutil.OSFileSystem, string) {
	t.Helper()
	return fsutil.OSFileSystem{}, t.TempDir()
}
