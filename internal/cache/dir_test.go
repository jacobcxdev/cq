package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDirUsesResolvedCacheRoot(t *testing.T) {
	roots := userdirs.Roots{Cache: "/cq/cache"}
	if got := Dir(roots); got != "/cq/cache" {
		t.Fatalf("Dir() = %q, want /cq/cache", got)
	}
}

func TestDefaultDir(t *testing.T) {
	t.Run("XDG_CACHE_HOME set", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "/tmp/xdg")
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("/tmp/xdg", "cq")
		if got != want {
			t.Fatalf("DefaultDir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_CACHE_HOME relative path falls through", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "./relative")
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "relative") {
			t.Errorf("relative XDG path should be ignored, got %q", got)
		}
	})

	t.Run("XDG_CACHE_HOME unset", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		got, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
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
