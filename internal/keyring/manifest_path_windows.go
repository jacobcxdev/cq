//go:build windows

package keyring

import (
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func cqManifestHome() (string, error) {
	return "", nil
}

// CQManifestPath locates the manifest beneath the resolved Windows cache root.
func CQManifestPath(roots userdirs.Roots, _ string) string {
	return filepath.Join(roots.Cache, "accounts.json")
}
