//go:build unix

package keyring

import (
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

var resolveCQManifestHome = os.UserHomeDir

func cqManifestHome() (string, error) {
	return resolveCQManifestHome()
}

// CQManifestPath preserves the established Unix manifest location.
func CQManifestPath(_ userdirs.Roots, home string) string {
	return filepath.Join(home, ".cache", "cq", "accounts.json")
}
