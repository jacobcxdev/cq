//go:build windows

package fsutil

import (
	"os"

	"golang.org/x/sys/windows"
)

func syncDirectory(name string) error {
	// Windows does not support flushing directory handles. Validate that the
	// retained path is still a directory after callers sync file contents.
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &os.PathError{Op: "sync", Path: name, Err: windows.ERROR_DIRECTORY}
	}
	return nil
}
