//go:build !windows

package cache

import (
	"os"
	"path/filepath"
	"testing"
)

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

	t.Run("relative XDG fails", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "./relative")
		if _, err := DefaultDir(); err == nil {
			t.Fatal("relative XDG accepted")
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
func TestCacheUserDirsOnlyUsesCacheInputs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "invalid-unused")
	t.Setenv("HOME", "")
	got, err := DefaultDir()
	if err != nil || got != filepath.Join(dir, "cq") {
		t.Fatalf("dir %q, %v", got, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lookup created directories: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", "relative-private")
	if _, err := DefaultDir(); err == nil {
		t.Fatal("invalid cache accepted")
	}
}
