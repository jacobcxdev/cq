//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestLinuxProxyStartWiresOwnedRuntime(t *testing.T) {
	if reflect.ValueOf(runProxyOwnedRuntimeFn).Pointer() != reflect.ValueOf(runLinuxProxyOwnedRuntime).Pointer() {
		t.Fatal("Linux owned runtime remains unavailable")
	}
	if reflect.ValueOf(runProxyAdoptedRuntimeFn).Pointer() != reflect.ValueOf(runLinuxProxyAdoptedRuntime).Pointer() {
		t.Fatal("Linux adopted runtime remains unavailable")
	}
	if reflect.ValueOf(newProxyRuntimeWorkerLauncherFn).Pointer() != reflect.ValueOf(newUnixProxyRuntimeWorkerLauncher).Pointer() {
		t.Fatal("Linux runtime worker launcher remains unavailable")
	}
	if reflect.ValueOf(adoptProxyListenerFn).Pointer() != reflect.ValueOf(adoptUnixProxyListener).Pointer() {
		t.Fatal("Linux listener adoption remains unavailable")
	}
}

func TestLinuxOwnedRuntimeCancelsOnTermination(t *testing.T) {
	original := runLinuxUnixProxyOwnedRuntime
	started := make(chan struct{})
	runLinuxUnixProxyOwnedRuntime = func(ctx context.Context, _ int, _ func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		close(started)
		<-ctx.Done()
		return true, ctx.Err()
	}
	t.Cleanup(func() { runLinuxUnixProxyOwnedRuntime = original })

	type result struct {
		handled bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		handled, err := runLinuxProxyOwnedRuntime(context.Background(), 0, nil)
		done <- result{handled: handled, err: err}
	}()
	<-started
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case terminated := <-done:
		if !terminated.handled || !errors.Is(terminated.err, context.Canceled) {
			t.Fatalf("terminated Linux runtime = %t, %v", terminated.handled, terminated.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Linux runtime ignored termination")
	}
}

func TestLinuxAdoptedRuntimeCancelsOnTermination(t *testing.T) {
	original := runLinuxUnixProxyAdoptedRuntime
	started := make(chan struct{})
	runLinuxUnixProxyAdoptedRuntime = func(ctx context.Context, _ net.Listener, _ func(context.Context, net.Listener, http.Handler) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { runLinuxUnixProxyAdoptedRuntime = original })

	done := make(chan error, 1)
	go func() {
		done <- runLinuxProxyAdoptedRuntime(context.Background(), nil, nil)
	}()
	<-started
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminated adopted Linux runtime = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopted Linux runtime ignored termination")
	}
}
