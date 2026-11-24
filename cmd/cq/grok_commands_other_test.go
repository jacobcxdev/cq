//go:build !windows

package main

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func grokCommandFixtureFS(t *testing.T) (fsutil.FileSystem, string) {
	t.Helper()
	return fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "cq")
}
