//go:build !windows

package compat

import (
	"fmt"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func DefaultEpochPath(fs fsutil.FileSystem, getenv func(string) string) (string, error) {
	if getenv != nil {
		if dir := getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
			return filepath.Join(dir, "cq", "state", "compatibility_epoch"), nil
		}
	}
	home, err := fs.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "cq", "state", "compatibility_epoch"), nil
}
