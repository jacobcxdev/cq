package dots

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This fixture is a child test process with Chromium's exact inherited fd layout.
func TestNativeLifetimeChild(t *testing.T) {
	if os.Getenv("CQ_NATIVE_LIFETIME_FIXTURE") != "1" {
		return
	}
	root := os.Getenv("CQ_NATIVE_FIXTURE_ROOT")
	pipe := os.NewFile(3, "cdp-input")
	disconnected := make(chan struct{})
	go func() { defer func() { _ = recover(); close(disconnected) }(); _, _ = io.Copy(io.Discard, pipe) }()
	for {
		select {
		case <-disconnected:
			_ = os.WriteFile(filepath.Join(root, "disconnected"), []byte("yes"), 0o600)
			os.Exit(2)
		default:
		}
		if _, err := os.Stat(filepath.Join(root, "finish")); err == nil {
			os.Exit(0)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestNativeBrokerCancellationKeepsAppPipeUntilOwnExit(t *testing.T) {
	_, s := installationFixture(t)
	s.RuntimeRoot = nativeTestRoot(t)
	s.IntegrationMode = "native"
	executable := filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(executable, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(output, input); err != nil {
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "CQ_NATIVE_LIFETIME_FIXTURE=1", "CQ_NATIVE_FIXTURE_ROOT="+s.RuntimeRoot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- os.ErrInvalid
			}
		}()
		done <- LaunchNativeBroker(ctx, &s, executable, []string{"-test.run=^TestNativeLifetimeChild$", "--", "--cq-dots-runtime=" + s.RuntimeRoot}, env)
	}()
	finish := func() { _ = os.WriteFile(filepath.Join(s.RuntimeRoot, "finish"), []byte("exit"), 0o600) }
	defer finish()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(s.RuntimeRoot, nativeStateName)); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("broker exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatal("broker did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, err = os.Lstat(filepath.Join(s.RuntimeRoot, nativeSocketName)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatal("IPC did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("broker abandoned live app: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err = os.Stat(filepath.Join(s.RuntimeRoot, "disconnected")); !os.IsNotExist(err) {
		t.Fatal("live app lost debug pipe")
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broker did not reap child")
	}
	if _, err = os.Stat(filepath.Join(s.RuntimeRoot, nativeStateName)); !os.IsNotExist(err) {
		t.Fatal("state was not cleaned")
	}
}
