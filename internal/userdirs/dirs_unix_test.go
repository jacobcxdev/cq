//go:build unix

package userdirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePreservesUnixXDGAndFallbacks(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Roots
	}{
		{
			name: "XDG",
			env:  map[string]string{"XDG_CONFIG_HOME": "/xdg/config", "XDG_CACHE_HOME": "/xdg/cache"},
			want: Roots{
				Config:  "/xdg/config/cq",
				State:   "/xdg/config/cq/state",
				Cache:   "/xdg/cache/cq",
				Runtime: "/xdg/config/cq/state",
				Logs:    unixLogs("/home/alice", "/xdg/config/cq"),
			},
		},
		{
			name: "relative XDG falls back",
			env:  map[string]string{"XDG_CONFIG_HOME": "relative", "XDG_CACHE_HOME": "relative"},
			want: Roots{
				Config:  "/home/alice/.config/cq",
				State:   "/home/alice/.config/cq/state",
				Cache:   "/host-native/cache/cq",
				Runtime: "/home/alice/.config/cq/state",
				Logs:    unixLogs("/home/alice", "/home/alice/.config/cq"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := (Resolver{
				Getenv:       func(key string) string { return test.env[key] },
				UserHomeDir:  func() (string, error) { return "/home/alice", nil },
				UserCacheDir: func() (string, error) { return "/host-native/cache", nil },
				TempDir:      func() string { return "/tmp" },
			}).Resolve()
			if err != nil || got != test.want {
				t.Fatalf("roots = %#v, error = %v, want %#v", got, err, test.want)
			}
			for _, root := range []string{got.Config, got.State, got.Cache, got.Runtime, got.Logs} {
				if !filepath.IsAbs(root) {
					t.Fatalf("non-absolute root %q in %#v", root, got)
				}
			}
		})
	}
}

func TestResolvePreservesUnixCacheFallbackChain(t *testing.T) {
	base := Resolver{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "/home/alice", nil },
		TempDir:     func() string { return "/tmp" },
	}
	base.UserCacheDir = func() (string, error) { return "", os.ErrPermission }

	got, err := base.Resolve()
	if err != nil || got.Cache != "/home/alice/.cache/cq" {
		t.Fatalf("cache = %q, %v", got.Cache, err)
	}

	base.UserHomeDir = func() (string, error) { return "", os.ErrPermission }
	gotCache, err := resolveUnixCache(base, base.UserHomeDir)
	if err != nil || gotCache != "/tmp/cq-cache" {
		t.Fatalf("cache = %q, %v", gotCache, err)
	}
}
