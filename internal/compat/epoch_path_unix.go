//go:build !windows

package compat

import (
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func DefaultEpochPath(fs fsutil.FileSystem, getenv func(string) string) (string, error) {
	resolver := userdirs.Resolver{Getenv: getenv}
	if fs != nil {
		resolver.UserHomeDir = fs.UserHomeDir
	}
	roots, err := resolver.Resolve(userdirs.StateRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(roots.State, "compatibility_epoch"), nil
}
