//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/dots"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func dotsDefaultRoot(string) (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", err
	}
	return filepath.Join(roots.State, "dots"), nil
}

func resolveDotsInstallApp() (dots.Settings, string, error) { return resolveWindowsDotsApp() }

func dotsInstallApp(options *CodexDotsInstallCmd, resolve func() (dots.Settings, string, error)) error {
	if options.Mode == "isolated" {
		return errors.New("Isolated Windows Dots runtime is unavailable")
	}
	if resolve == nil {
		return errors.New("Windows Dots package resolver unavailable")
	}
	pin, _, err := resolve()
	if err != nil {
		return err
	}
	if options.App != "" && !strings.EqualFold(filepath.Clean(options.App), pin.AppPath) {
		return fmt.Errorf("Windows Dots app must be the registered official package")
	}
	options.App = pin.AppPath
	return nil
}

// resolveWindowsDotsApp qualifies the registered official package and renderer.
func resolveWindowsDotsApp() (dots.Settings, string, error) {
	return dots.ResolveRegisteredWindowsApp()
}
