//go:build !windows

package dots

import "errors"

func ResolveRegisteredWindowsApp() (Settings, string, error) {
	return Settings{}, "", errors.New("Windows Codex Desktop registration unavailable")
}
