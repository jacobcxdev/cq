//go:build !windows

package dots

import "errors"

func windowsRegisteredAppBoundary(_ string, canonicalErr error) (string, error) {
	return "", canonicalErr
}

func ResolveRegisteredWindowsApp() (Settings, string, error) {
	return Settings{}, "", errors.New("Windows Codex Desktop registration unavailable")
}
