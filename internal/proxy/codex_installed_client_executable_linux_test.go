//go:build linux

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestResolveCodexInstalledClientExecutableAcceptsNativeBinary(t *testing.T) {
	pathBin := t.TempDir()
	native := filepath.Join(pathBin, "codex")
	if err := os.WriteFile(native, []byte("\x7fELFexact-native"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathBin)
	got, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if got != native {
		t.Fatalf("resolved executable = %q, want %q", got, native)
	}
}

func TestResolveCodexInstalledClientExecutableUsesOfficialNPMNativeBinary(t *testing.T) {
	for _, test := range []struct {
		name      string
		layouts   []string
		wantIndex int
		wantError bool
	}{
		{name: "current bin layout", layouts: []string{"bin/codex"}, wantIndex: 0},
		{name: "legacy codex layout", layouts: []string{"codex/codex"}, wantIndex: 0},
		{name: "ambiguous layouts", layouts: []string{"bin/codex", "codex/codex"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			packageRoot := filepath.Join(root, "lib", "node_modules", "@openai", "codex")
			launcher := filepath.Join(packageRoot, "bin", "codex.js")
			if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\n"), 0o500); err != nil {
				t.Fatal(err)
			}
			packageName, target := linuxCodexNativePackage(runtime.GOARCH)
			if packageName == "" || target == "" {
				t.Fatalf("unsupported test architecture %q", runtime.GOARCH)
			}
			natives := make([]string, 0, len(test.layouts))
			for _, layout := range test.layouts {
				native := filepath.Join(filepath.Dir(packageRoot), packageName, "vendor", target, filepath.FromSlash(layout))
				if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, []byte("\x7fELFexact-native"), 0o500); err != nil {
					t.Fatal(err)
				}
				natives = append(natives, native)
			}
			pathBin := filepath.Join(root, "bin")
			if err := os.Mkdir(pathBin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(launcher, filepath.Join(pathBin, "codex")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", pathBin)

			got, err := resolveCodexInstalledClientExecutable()
			if test.wantError {
				if err == nil || got != "" {
					t.Fatalf("ambiguous executable resolved as %q, error %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != natives[test.wantIndex] {
				t.Fatalf("resolved executable = %q, want %q", got, natives[test.wantIndex])
			}
		})
	}
}

func TestResolveCodexInstalledClientExecutableRejectsNonNativeWrapper(t *testing.T) {
	pathBin := t.TempDir()
	launcher := filepath.Join(pathBin, "codex")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathBin)
	if path, err := resolveCodexInstalledClientExecutable(); err == nil || path != "" {
		t.Fatalf("non-native wrapper resolved as %q, error %v", path, err)
	}
}

func TestLinuxInstalledCodexExecutableVersion(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("set CQ_RUN_CODEX_INSTALLED_ACCEPTANCE=1 to exercise installed Linux Codex")
	}
	prepareCodexAcceptanceTestConfinement(t)
	path, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := captureCodexInstalledExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := runCodexInstalledVersionCommand(ctx, proof.path, proof)
	if err != nil {
		t.Fatal(err)
	}
	if version, ok := parseCodexInstalledVersionOutput(output); !ok || version == "" {
		t.Fatalf("installed Codex version = %q/%v", version, ok)
	}
}
