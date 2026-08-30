package cache

import (
	"fmt"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Dir returns the shared cache directory from resolved CQ roots.
func Dir(roots userdirs.Roots) string { return roots.Cache }

// DefaultDir returns the shared cache directory used by cq.
func DefaultDir() (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", fmt.Errorf("resolve CQ cache directory: %w", err)
	}
	return Dir(roots), nil
}
