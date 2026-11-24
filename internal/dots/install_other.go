//go:build !windows

package dots

import (
	"context"
	"errors"
)

func windowsInstall(context.Context, InstallOptions) (map[string]any, error) {
	return nil, errors.New("Windows Dots install unavailable")
}

func windowsAppCheck(*Settings) error { return errors.New("Windows Dots app unavailable") }

func installWindowsDotsLauncher(string, string, string) (string, error) {
	return "", errors.New("Windows Dots launcher unavailable")
}
