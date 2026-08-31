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
	native := filepath.Join(packageRoot, "node_modules", "@openai", packageName, "vendor", target, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("\x7fELFexact-native"), 0o500); err != nil {
		t.Fatal(err)
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
	if err != nil {
		t.Fatal(err)
	}
	if got != native {
		t.Fatalf("resolved executable = %q, want %q", got, native)
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
