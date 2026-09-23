//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
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

func TestCLIV2ProxyMigrationAfterRuntimePreconditions(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	called := false
	sentinel := errors.New("migration stopped before worker launch")
	ctx := context.WithValue(context.Background(), proxyForegroundMigrationKey{}, func(context.Context) error { called = true; return sentinel })
	serve := func(context.Context, net.Listener, http.Handler) error {
		t.Fatal("served after migration failed")
		return nil
	}
	_, err = runUnixProxyOwnedRuntime(ctx, listener.Addr().(*net.TCPAddr).Port, serve)
	if err == nil || called {
		t.Fatalf("migration ran before port ownership: called=%v err=%v", called, err)
	}
	path, err := proxy.DefaultRuntimeLifecyclePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = runUnixProxyOwnedRuntime(ctx, 0, serve)
	if err == nil || called {
		t.Fatalf("migration ran before authority validation: called=%v err=%v", called, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = runUnixProxyOwnedRuntime(ctx, 0, serve)
	if !errors.Is(err, sentinel) || !called {
		t.Fatalf("migration omitted after preconditions: called=%v err=%v", called, err)
	}
}
