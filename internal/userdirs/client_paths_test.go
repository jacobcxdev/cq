package userdirs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientPathsProviderIsolationAndPrecedence(t *testing.T) {
	cwd, home := filepath.Join(t.TempDir(), "cwd"), filepath.Join(t.TempDir(), "home")
	for _, provider := range []string{"codex", "claude"} {
		for _, value := range []string{"", "relative/../cache", "literal space", "~/$SECRET/$(command)", filepath.Join(cwd, "absolute")} {
			key, subdir := "CODEX_HOME", ".codex"
			if provider == "claude" {
				key, subdir = "CLAUDE_CONFIG_DIR", ".claude"
			}
			calls := 0
			paths, err := ResolveClientPaths(provider, cwd, home, func(name string) string {
				calls++
				if name != key {
					t.Fatalf("read unrelated environment %s", name)
				}
				return value
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := value
			if dir == "" {
				dir = filepath.Join(home, subdir)
			} else if !filepath.IsAbs(dir) {
				dir = filepath.Join(cwd, dir)
			}
			want := ClientPaths{}
			if provider == "codex" {
				want.CodexModels = filepath.Join(dir, "models_cache.json")
				want.CodexVersion = want.CodexModels
			} else {
				want.ClaudeCapabilities = filepath.Join(dir, "cache", "model-capabilities.json")
			}
			if paths != want || calls != 1 {
				t.Fatalf("paths %#v, calls %d; want %#v", paths, calls, want)
			}
		}
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("resolution created cwd: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("resolution created home: %v", err)
	}
}
func TestClientPathsWithoutHomeAndUnavailableCWD(t *testing.T) {
	cwd := t.TempDir()
	for _, provider := range []string{"codex", "claude"} {
		for _, value := range []string{cwd, "relative"} {
			_, err := ClientPathsWith(provider, cwd, "", func(string) string { return value }, func() (string, error) { t.Fatal("read unused native home"); return "", nil })
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			cwd, home, value, code string
			exit                   int
		}{{"", "", "relative", "environment_path_invalid", 2}, {cwd, "", "", "environment_root_unavailable", 4}, {cwd, "relative", "", "environment_path_invalid", 2}, {cwd, cwd, "secret\x00value", "environment_path_invalid", 2}} {
			_, err := ResolveClientPaths(provider, tc.cwd, tc.home, func(string) string { return tc.value })
			var e *EnvironmentError
			if !errors.As(err, &e) || e.Code != tc.code || e.ExitCode != tc.exit || strings.Contains(e.Error(), "secret") {
				t.Fatalf("diagnostic: %v", err)
			}
		}
	}
	if _, err := ResolveClientPaths("gemini", cwd, cwd, func(string) string { t.Fatal("invalid provider read env"); return "" }); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}

func TestClientPathsAbsoluteOverrideWorksWithoutCWDOrHome(t *testing.T) {
	cache := t.TempDir()
	paths, err := ClientPathsWith("codex", "invalid cwd", "", func(string) string { return cache }, func() (string, error) { t.Fatal("read unused home"); return "", nil })
	if err != nil || paths.CodexModels != filepath.Join(cache, "models_cache.json") {
		t.Fatalf("paths %#v error %v", paths, err)
	}
}
