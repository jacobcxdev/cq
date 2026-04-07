package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultDir(t *testing.T) {
	t.Run("XDG_CACHE_HOME set", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "/tmp/xdg")
		got := DefaultDir()
		want := filepath.Join("/tmp/xdg", "cq")
		if got != want {
			t.Fatalf("DefaultDir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_CACHE_HOME relative path falls through", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "./relative")
		got := DefaultDir()
		if strings.Contains(got, "relative") {
			t.Errorf("relative XDG path should be ignored, got %q", got)
		}
	})

	t.Run("XDG_CACHE_HOME unset", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		got := DefaultDir()
		if filepath.Base(got) != "cq" {
			t.Fatalf("DefaultDir() = %q, want base to be \"cq\"", got)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("DefaultDir() = %q, want absolute path", got)
		}
		cacheBase, err := os.UserCacheDir()
		if err == nil {
			want := filepath.Join(cacheBase, "cq")
			if got != want {
				t.Fatalf("DefaultDir() = %q, want %q", got, want)
			}
		}
	})
}
