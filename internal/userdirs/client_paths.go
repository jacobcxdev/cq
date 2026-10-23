package userdirs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClientPaths are native client caches, never credential or authority roots.
type ClientPaths struct {
	CodexModels        string
	CodexVersion       string
	ClaudeCapabilities string
}

// ResolveClientPaths resolves only provider's cache inputs against the captured cwd.
func ResolveClientPaths(provider string, cwd string, home string, getenv func(string) string) (ClientPaths, error) {
	var paths ClientPaths
	name, subdir := "", ""
	switch provider {
	case "codex":
		name, subdir = "CODEX_HOME", ".codex"
	case "claude":
		name, subdir = "CLAUDE_CONFIG_DIR", ".claude"
	default:
		return paths, fmt.Errorf("unsupported client provider")
	}
	dir := ""
	if getenv != nil {
		dir = getenv(name)
	}
	if dir == "" {
		if home == "" {
			return paths, rootUnavailable()
		}
		if !filepath.IsAbs(home) || strings.IndexByte(home, 0) >= 0 {
			return paths, invalidPath("HOME")
		}
		dir = filepath.Join(home, subdir)
	}
	if strings.IndexByte(dir, 0) >= 0 {
		return paths, invalidPath(name)
	}
	if !filepath.IsAbs(dir) {
		if !filepath.IsAbs(cwd) || strings.IndexByte(cwd, 0) >= 0 {
			return paths, workingDirectoryUnavailable()
		}
		dir = filepath.Join(cwd, dir)
	}
	dir = filepath.Clean(dir)
	if provider == "codex" {
		paths.CodexModels = filepath.Join(dir, "models_cache.json")
		paths.CodexVersion = paths.CodexModels
	} else {
		paths.ClaudeCapabilities = filepath.Join(dir, "cache", "model-capabilities.json")
	}
	return paths, nil
}

// DefaultClientPaths acquires only the process inputs needed by the selected client.
func DefaultClientPaths(provider string) (ClientPaths, error) {
	return ClientPathsWith(provider, "", "", os.Getenv, UserHomeDir)
}

// ClientPathsWith fills missing cwd/home lazily for filesystem-injected consumers.
// Callers with a command-start cwd pass it so later cwd changes cannot retarget paths.
func ClientPathsWith(provider, cwd, home string, getenv func(string) string, homeDir func() (string, error)) (ClientPaths, error) {
	name := ""
	switch provider {
	case "codex":
		name = "CODEX_HOME"
	case "claude":
		name = "CLAUDE_CONFIG_DIR"
	default:
		return ResolveClientPaths(provider, cwd, home, getenv)
	}
	value := ""
	if getenv != nil {
		value = getenv(name)
	}
	if value == "" && home == "" {
		if homeDir == nil {
			return ClientPaths{}, rootUnavailable()
		}
		var err error
		home, err = homeDir()
		if err != nil {
			return ClientPaths{}, safeHomeError(err)
		}
	}
	if value != "" && !filepath.IsAbs(value) && cwd == "" {
		var err error
		cwd, err = WorkingDirectory()
		if err != nil {
			return ClientPaths{}, err
		}
	}
	// Snapshot the override: environment changes cannot split one resolution.
	return ResolveClientPaths(provider, cwd, home, func(string) string { return value })
}
