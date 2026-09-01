//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestUnixRuntimeLifecycleOpensExactHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	file, holder, err := openUnixRuntimeLifecycle(path, "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if holder.DescriptionID == "" || holder.Mode != proxy.LifecycleShared {
		t.Fatalf("unexpected lifecycle holder: %+v", holder)
	}
	digest, err := proxy.RuntimeDescriptorIdentityDigest(file)
	if err != nil {
		t.Fatal(err)
	}
	if digest == ([32]byte{}) {
		t.Fatal("lifecycle descriptor identity is empty")
	}
}

func TestUnixRuntimeLifecycleRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if file, _, err := openUnixRuntimeLifecycle(link, "supervisor"); err == nil {
		_ = file.Close()
		t.Fatal("symlink lifecycle unexpectedly accepted")
	}
}

func TestUnixRuntimeDescriptorPathIsAbsolute(t *testing.T) {
	path := runtimeDescriptorPath(proxy.RuntimeLifecycleFD)
	if !filepath.IsAbs(path) || filepath.Base(path) != "4" {
		t.Fatalf("unexpected descriptor path %q", path)
	}
}

func TestResolveUnixRuntimeExecutableCanonicalisesSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "cq")
	if err := os.WriteFile(target, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked-cq")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveUnixRuntimeExecutable(link)
	if err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != canonicalTarget {
		t.Fatalf("resolved executable = %q; want %q", resolved, canonicalTarget)
	}
}

func TestResolveUnixRuntimeExecutableRejectsCycle(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first")
	second := filepath.Join(directory, "second")
	if err := os.Symlink(second, first); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, second); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveUnixRuntimeExecutable(first); err == nil {
		t.Fatal("symlink cycle unexpectedly resolved")
	}
}
