//go:build unix

package userdirs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func (resolver Resolver) Resolve() (Roots, error) {
	if resolver.Getenv == nil || resolver.UserHomeDir == nil {
		return Roots{}, fmt.Errorf("resolve CQ user directories: incomplete resolver")
	}

	var home string
	resolveHome := func() (string, error) {
		if home != "" {
			return home, nil
		}
		resolved, err := resolver.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		home = resolved
		return home, nil
	}

	configBase := resolver.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configBase) {
		resolved, err := resolveHome()
		if err != nil {
			return Roots{}, err
		}
		configBase = filepath.Join(resolved, ".config")
	}
	config := filepath.Join(configBase, "cq")

	cache, err := resolveUnixCache(resolver, resolveHome)
	if err != nil {
		return Roots{}, err
	}

	logs := filepath.Join(config, "state", "logs")
	if runtime.GOOS == "darwin" {
		resolvedHome, err := resolveHome()
		if err != nil {
			return Roots{}, err
		}
		logs = unixLogs(resolvedHome, config)
	}

	roots := Roots{
		Config:  config,
		State:   filepath.Join(config, "state"),
		Cache:   cache,
		Runtime: filepath.Join(config, "state"),
		Logs:    logs,
	}
	for _, root := range []string{roots.Config, roots.State, roots.Cache, roots.Runtime, roots.Logs} {
		if !filepath.IsAbs(root) {
			return Roots{}, fmt.Errorf("CQ root is not absolute: %q", root)
		}
	}
	return roots, nil
}

func Default() (Roots, error) {
	return (Resolver{
		Getenv:       os.Getenv,
		UserHomeDir:  os.UserHomeDir,
		UserCacheDir: os.UserCacheDir,
		TempDir:      os.TempDir,
	}).Resolve()
}

func unixLogs(home, config string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "cq")
	}
	return filepath.Join(config, "state", "logs")
}

func resolveUnixCache(resolver Resolver, resolveHome func() (string, error)) (string, error) {
	if base := resolver.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "cq"), nil
	}
	if resolver.UserCacheDir != nil {
		if base, err := resolver.UserCacheDir(); err == nil && filepath.IsAbs(base) {
			return filepath.Join(base, "cq"), nil
		}
	}
	if home, err := resolveHome(); err == nil {
		return filepath.Join(home, ".cache", "cq"), nil
	}
	if resolver.TempDir == nil || !filepath.IsAbs(resolver.TempDir()) {
		return "", fmt.Errorf("resolve CQ cache directory: no absolute fallback")
	}
	return filepath.Join(resolver.TempDir(), "cq-cache"), nil
}
