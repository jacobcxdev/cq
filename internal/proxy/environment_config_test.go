package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestCLIV2EnvironmentUpstreamValidation(t *testing.T) {
	for _, value := range []string{"relative", "ftp://example.com", "https:///missing", "https://", "http://example.com:99999", "http://example.com:0"} {
		c := Config{LocalToken: "local", ClaudeUpstream: value, CodexUpstream: "https://example.com"}
		c.setDefaults()
		if err := c.validate(); err == nil {
			t.Errorf("accepted unusable upstream %q", value)
		}
	}
}
func TestContinuityDefaultDirUsesStateAndResilienceHasNoDefault(t *testing.T) {
	roots := userdirs.Roots{Config: "/config", State: "/config/state", Cache: "/cache", Runtime: "/config/state", Logs: "/logs"}
	paths := PathsForRoots(roots)
	c := &Config{}
	if got := c.ResolvedCodexContinuityStateDir(paths.StateDir); got != roots.State {
		t.Fatalf("continuity %q", got)
	}
	if c.ResolvedProxyResilienceStateDir() != "" {
		t.Fatal("resilience invented a root")
	}
	for _, value := range []string{"relative-private-path", " ", "/", "/state/../private", "/state/./private", "/private\x00path"} {
		for _, resilience := range []bool{false, true} {
			c := Config{LocalToken: "local"}
			if resilience {
				c.ProxyResilienceStateDir = value
			} else {
				c.CodexContinuityStateDir = value
			}
			c.setDefaults()
			err := c.validate()
			if err == nil {
				t.Fatal("accepted owned-state path")
			}
			if len(value) > 2 && strings.Contains(err.Error(), value) {
				t.Fatal("echoed private path in error")
			}
		}
	}

}

func TestCLIV2EnvironmentConfigReadOnlyUsesConfigRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix environment roots")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", "invalid-unused")
	t.Setenv("HOME", "")
	if _, err := LoadExistingConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config result %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cq")); !os.IsNotExist(err) {
		t.Fatalf("read-only config created directories: %v", err)
	}
}
func TestCLIV2EnvironmentUpstreamsDefaultAndIgnoreClientOverride(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:19280")
	c := Config{LocalToken: "local"}
	c.setDefaults()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.ClaudeUpstream != "https://api.anthropic.com" || c.CodexUpstream != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("unexpected defaults")
	}
	for _, value := range []string{"http://127.0.0.1:1234/base", "https://example.com/api"} {
		c.ClaudeUpstream = value
		if err := c.validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContinuityDefaultDirDoesNotRequireUnrelatedRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix environment roots")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", "invalid-unused")
	t.Setenv("HOME", "")
	got, err := DefaultCodexCanaryPath()
	if err != nil || got != filepath.Join(dir, "cq", "state", "codex-routing-canary.json") {
		t.Fatalf("canary path %q error %v", got, err)
	}
}
