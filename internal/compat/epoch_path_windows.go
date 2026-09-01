//go:build windows

package compat

import (
	"fmt"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func DefaultEpochPath(fs fsutil.FileSystem, getenv func(string) string) (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", fmt.Errorf("resolve Windows compatibility root: %w", err)
	}
	return filepath.Join(roots.State, "compatibility_epoch"), nil
}
