//go:build !windows

package dots

import (
	"errors"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func windowsStateFS(string) (fsutil.OSFileSystem, error) {
	return fsutil.OSFileSystem{}, errors.New("Windows private state unavailable")
}
func windowsPrivateDirectory(string) error { return errors.New("Windows private state unavailable") }
func windowsEnsurePrivateDirectory(string) error {
	return errors.New("Windows private state unavailable")
}
func windowsValidatePrivateFile(string) error { return errors.New("Windows private state unavailable") }
func windowsSaveJSON(string, any) error       { return errors.New("Windows private state unavailable") }
func windowsReadPrivate(string, string, bool) ([]byte, error) {
	return nil, errors.New("Windows private state unavailable")
}
func windowsMigratePrivatePort(string) error { return errors.New("Windows private state unavailable") }
