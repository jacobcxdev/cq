//go:build !windows

package installer

import "context"

func removeExecutable(_ context.Context, fsys installerFileSystem, path string) error {
	return fsys.Remove(path)
}
