package cache

import (
	"os"
	"path/filepath"
)

// DefaultDir returns the shared cache directory used by cq.
func DefaultDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "cq")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "cq")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cq-cache")
	}
	return filepath.Join(home, ".cache", "cq")
}
