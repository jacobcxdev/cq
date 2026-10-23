//go:build unix

package userdirs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func (resolver Resolver) Resolve(wanted ...Root) (Roots, error) {
	var roots Roots
	var home string
	resolveHome := func() (string, error) {
		if home != "" {
			return home, nil
		}
		if resolver.UserHomeDir == nil {
			return "", rootUnavailable()
		}
		value, err := resolver.UserHomeDir()
		if err != nil {
			return "", safeHomeError(err)
		}
		if value == "" {
			return "", rootUnavailable()
		}
		if !filepath.IsAbs(value) || strings.IndexByte(value, 0) >= 0 {
			return "", invalidPath("HOME")
		}
		home = filepath.Clean(value)
		return home, nil
	}
	base := func(name, defaultDir string) (string, error) {
		value := ""
		if resolver.Getenv != nil {
			value = resolver.Getenv(name)
		}
		if value != "" {
			if !filepath.IsAbs(value) || strings.IndexByte(value, 0) >= 0 {
				return "", invalidPath(name)
			}
			return filepath.Clean(value), nil
		}
		h, err := resolveHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, defaultDir), nil
	}
	needsConfig := selected(wanted, ConfigRoot) || selected(wanted, StateRoot) || selected(wanted, RuntimeRoot) || (selected(wanted, LogsRoot) && runtime.GOOS != "darwin")
	if needsConfig {
		b, err := base("XDG_CONFIG_HOME", ".config")
		if err != nil {
			return Roots{}, err
		}
		c := filepath.Join(b, "cq")
		s := filepath.Join(c, "state")
		if selected(wanted, ConfigRoot) {
			roots.Config = c
		}
		if selected(wanted, StateRoot) {
			roots.State = s
		}
		if selected(wanted, RuntimeRoot) {
			roots.Runtime = s
		}
		if selected(wanted, LogsRoot) && runtime.GOOS != "darwin" {
			roots.Logs = filepath.Join(s, "logs")
		}
	}
	if selected(wanted, CacheRoot) {
		dir := ".cache"
		if runtime.GOOS == "darwin" {
			dir = filepath.Join("Library", "Caches")
		}
		b, err := base("XDG_CACHE_HOME", dir)
		if err != nil {
			return Roots{}, err
		}
		roots.Cache = filepath.Join(b, "cq")
	}
	if selected(wanted, LogsRoot) && runtime.GOOS == "darwin" {
		h, err := resolveHome()
		if err != nil {
			return Roots{}, err
		}
		roots.Logs = unixLogs(h, "")
	}
	return roots, nil
}
func Default(wanted ...Root) (Roots, error) {
	return (Resolver{Getenv: os.Getenv, UserHomeDir: os.UserHomeDir}).Resolve(wanted...)
}

// UserHomeDir is the native credential home, independent of client cache overrides.
func UserHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", rootUnavailable()
	}
	if !filepath.IsAbs(home) || strings.IndexByte(home, 0) >= 0 {
		return "", invalidPath("HOME")
	}
	return filepath.Clean(home), nil
}
func unixLogs(home, config string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "cq")
	}
	return filepath.Join(config, "state", "logs")
}
