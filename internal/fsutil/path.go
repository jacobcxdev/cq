package fsutil

import "path/filepath"

// IsCleanAbsoluteNonRootPath reports whether path is canonical, absolute, and
// below its filesystem volume root.
func IsCleanAbsoluteNonRootPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) != path
}
