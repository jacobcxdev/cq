package main

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestParseCodexVersionOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"codex-cli prefix", "codex-cli 0.124.0\n", "0.124.0", true},
		{"bare codex prefix", "codex 0.124.0", "0.124.0", true},
		{"prerelease", "codex-cli 0.124.0-beta.1", "0.124.0-beta.1", true},
		{"empty", "", "", false},
		{"no semver", "unknown output", "", false},
		{"multi-line first", "codex-cli 0.124.0\nextra info", "0.124.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCodexVersionOutput(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("got = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexVersionResolver_PrefersModelsCache(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json",
		[]byte(`{"client_version":"0.99.0","models":[]}`), 0o600)

	r := codexVersionResolver{
		FS:        fsys,
		CachePath: "/home/test/.codex/models_cache.json",
		SubprocessVersion: func() (string, bool) {
			t.Fatal("subprocess must not run when cache is present")
			return "", false
		},
		Fallback: "0.0.0",
	}
	if got := r.Resolve(); got != "0.99.0" {
		t.Errorf("Resolve = %q, want 0.99.0", got)
	}
}

func TestCodexVersionResolver_FallsBackToSubprocessWhenCacheMissing(t *testing.T) {
	r := codexVersionResolver{
		FS:                fsutil.NewMemFS(),
		CachePath:         "/no/such/file.json",
		SubprocessVersion: func() (string, bool) { return "0.124.0", true },
		Fallback:          "0.0.0",
	}
	if got := r.Resolve(); got != "0.124.0" {
		t.Errorf("Resolve = %q, want 0.124.0", got)
	}
}

func TestCodexVersionResolver_PinnedFallbackWhenAllMissing(t *testing.T) {
	r := codexVersionResolver{
		FS:                fsutil.NewMemFS(),
		CachePath:         "/no/such/file.json",
		SubprocessVersion: func() (string, bool) { return "", false },
		Fallback:          "0.124.0",
	}
	if got := r.Resolve(); got != "0.124.0" {
		t.Errorf("Resolve = %q, want 0.124.0 (pinned fallback)", got)
	}
}
