//go:build !windows

package proxy

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func leaseJournalPrivateTestDir(t *testing.T) (string, fsutil.OSFileSystem) {
	t.Helper()
	return t.TempDir(), fsutil.OSFileSystem{}
}
