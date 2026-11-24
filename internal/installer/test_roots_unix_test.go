//go:build unix

package installer

import "testing"

func secureInstallerTestRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
