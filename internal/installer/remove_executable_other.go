//go:build !windows

package installer

import "context"

func removeExecutable(_ context.Context, fsys installerFileSystem, path string) error {
	return fsys.Remove(path)
}

func replaceExecutable(_ context.Context, fsys installerFileSystem, source, destination string) error {
	return fsys.Rename(source, destination)
}
