//go:build !windows

package dots

func currentDesktopAppPath(s *Settings) (string, error) { return s.AppPath, nil }
