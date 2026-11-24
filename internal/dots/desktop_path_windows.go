//go:build windows

package dots

import (
	"errors"
	"path/filepath"
)

func currentDesktopAppPath(s *Settings) (string, error) {
	if s == nil {
		return "", errors.New("Dots settings unavailable")
	}
	current, err := LoadSettings(filepath.Join(filepath.Dir(s.RuntimeRoot), "settings.json"))
	if err != nil {
		return "", err
	}
	if !dotsSamePath(current.RuntimeRoot, s.RuntimeRoot) {
		return "", errors.New("Dots installation changed during app activation")
	}
	return current.AppPath, nil
}
