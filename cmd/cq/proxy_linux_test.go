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

func TestCLIV2ProxyLinuxExternalSignalCause(t *testing.T) {
	original := runLinuxUnixProxyOwnedRuntime
	t.Cleanup(func() { runLinuxUnixProxyOwnedRuntime = original })
	for _, cause := range []error{context.Canceled, errV2ProxyTerminated} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			ctx = context.WithValue(ctx, proxyForegroundSignalsKey{}, true)
			cleaned := false
			runLinuxUnixProxyOwnedRuntime = func(inner context.Context, _ int, _ func(context.Context, net.Listener, http.Handler) error) (bool, error) {
				defer func() { cleaned = true }()
				cancel(cause)
				<-inner.Done()
				if context.Cause(inner) != cause {
					t.Fatalf("cause=%v want=%v", context.Cause(inner), cause)
				}
				return true, nil
			}
			deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
				if err := ready("127.0.0.1:19280", []string{"codex"}); err != nil {
					return err
				}
				_, err := runLinuxProxyOwnedRuntime(ctx, opts.Port, nil)
				return err
			}}
			exit, _, _ := runV2ProxyTest(t, ctx, []string{"proxy", "serve", "--json"}, deps)
			want := 0
			if cause == context.Canceled {
				want = 130
			}
			if exit != want || !cleaned {
				t.Fatalf("exit=%d want=%d cleaned=%v", exit, want, cleaned)
			}
		})
	}
}
