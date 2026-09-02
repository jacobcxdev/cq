//go:build windows

package installer

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsExecutableRemovalAttempts = 300
	windowsExecutableRemovalInterval = 100 * time.Millisecond
)

func removeExecutable(ctx context.Context, fsys installerFileSystem, path string) error {
	return removeExecutableWithRetry(ctx, fsys, path, windowsExecutableRemovalAttempts, windowsExecutableRemovalInterval)
}

func replaceExecutable(ctx context.Context, fsys installerFileSystem, source, destination string) error {
	if err := removeExecutable(ctx, fsys, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsys.Rename(source, destination)
}

func removeExecutableWithRetry(ctx context.Context, fsys installerFileSystem, path string, attempts int, interval time.Duration) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		err = fsys.Remove(path)
		if err == nil {
			return nil
		}
		if !isRetryableWindowsExecutableRemoval(err) {
			return err
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return errors.Join(err, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return err
}

func isRetryableWindowsExecutableRemoval(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
