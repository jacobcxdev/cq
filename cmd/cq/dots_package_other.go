//go:build !windows

package main

import (
	"errors"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/dots"
)

func dotsDefaultRoot(home string) (string, error) {
	return filepath.Join(home, ".local", "share", "cq", "dots"), nil
}

func resolveDotsInstallApp() (dots.Settings, string, error) {
	return dots.Settings{}, "", errors.New("Windows Dots package resolver unavailable")
}

func dotsInstallApp(options *CodexDotsInstallCmd, _ func() (dots.Settings, string, error)) error {
	if options.App == "" {
		options.App = "/Applications/ChatGPT.app"
	}
	return nil
}
