//go:build windows

package dots

import (
	"context"
	"errors"
)

func windowsInstall(ctx context.Context, options InstallOptions) (map[string]any, error) {
	if options.Mode == "isolated" {
		return nil, errors.New("Isolated Windows Dots runtime is unavailable")
	}
	pin, _, err := QualifyWindowsApp(options.App)
	if err != nil {
		return nil, err
	}
	return installPrepared(ctx, options, pin)
}

func windowsAppCheck(settings *Settings) error {
	return CheckWindowsApp(settings)
}
