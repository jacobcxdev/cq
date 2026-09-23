//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCLIV2ProxyUnixPreReadySignals(t *testing.T) {
	for _, cause := range []error{context.Canceled, errV2ProxyTerminated} {
		for _, phase := range []string{"before-launch", "worker-boot", "worker-boot-failure"} {
			t.Run(cause.Error()+"/"+phase, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_CONFIG_HOME", root)
				oldLoad, oldRun := loadProxyStartConfigFn, runProxyOwnedRuntimeFn
				t.Cleanup(func() { loadProxyStartConfigFn, runProxyOwnedRuntimeFn = oldLoad, oldRun })
				loadProxyStartConfigFn = func() (*proxy.Config, error) { return &proxy.Config{Port: 19280, LocalToken: "fixture-token"}, nil }
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				worker := &proxyPreReadyWorker{cancel: func() { cancel(cause) }, fail: phase == "worker-boot-failure"}
				launcher := &proxyPreReadyLauncher{worker: worker}
				address := ""
				cleaned := false
				var startupErr error
				runProxyOwnedRuntimeFn = func(inner context.Context, _ int, serve func(context.Context, net.Listener, http.Handler) error) (bool, error) {
					listener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						return true, err
					}
					address = listener.Addr().String()
					defer func() { _ = listener.Close(); cleaned = true }()
					path := filepath.Join(root, "lifecycle")
					if err := os.WriteFile(path, nil, 0600); err != nil {
						return true, err
					}
					file, holder, err := openUnixRuntimeLifecycle(path, "supervisor")
					if err != nil {
						return true, err
					}
					defer file.Close()
					startupErr = proxy.RunAdoptedRuntimeSupervisorConfigured(inner, listener, holder, launcher, &proxy.RuntimeHashCheckpointStore{}, nil,
						proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "fixture"},
						func(*proxy.RuntimeSupervisor) error {
							if phase == "before-launch" {
								cancel(cause)
							}
							return nil
						}, serve)
					return true, wrapUnixProxyRuntimeError("serve owned runtime", wrapUnixProxyRuntimeError("run normal supervisor", startupErr))
				}
				exit, out, stderr := runV2ProxyTest(t, ctx, []string{"proxy", "serve", "--json"}, v2ProxyDependencies{Serve: runProxyStartWithContext})
				want := 0
				if cause == context.Canceled {
					want = 130
				}
				if phase == "worker-boot-failure" {
					want = 1
				}
				if exit != want || strings.Contains(out, `"event"`) || stderr != "" {
					t.Errorf("exit=%d want=%d output=%s stderr=%s startup=%v", exit, want, out, stderr, startupErr)
				}
				if !cleaned {
					t.Fatal("owned cleanup omitted")
				}
				connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
				if err == nil {
					connection.Close()
					t.Fatal("owned listener remained open")
				}
				wantWorkers := 1
				if phase == "before-launch" {
					wantWorkers = 0
				}
				if launcher.calls != wantWorkers || worker.reaped != wantWorkers {
					t.Fatalf("launches=%d reaped=%d want=%d", launcher.calls, worker.reaped, wantWorkers)
				}
			})
		}
	}
}

type proxyPreReadyLauncher struct {
	worker proxy.RuntimeWorkerProcess
	calls  int
}

func (l *proxyPreReadyLauncher) Launch(context.Context, proxy.WorkerManifestV1) (proxy.RuntimeWorkerProcess, error) {
	l.calls++
	return l.worker, nil
}

type proxyPreReadyWorker struct {
	proxy.RuntimeWorkerProcess
	cancel func()
	fail   bool
	reaped int
}

func (w *proxyPreReadyWorker) Boot(ctx context.Context, _ proxy.WorkerManifestV1) (proxy.RuntimeBootAckV1, error) {
	w.cancel()
	err := ctx.Err()
	if w.fail {
		err = errors.Join(err, errors.New("genuine startup failure"))
	}
	return proxy.RuntimeBootAckV1{}, err
}
func (w *proxyPreReadyWorker) StopAndReap(context.Context) (proxy.RuntimeWorkerReleaseV1, error) {
	w.reaped++
	return proxy.RuntimeWorkerReleaseV1{}, nil
}
