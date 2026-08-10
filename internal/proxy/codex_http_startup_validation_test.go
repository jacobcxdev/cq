package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerCodexHTTPStartupValidationRunsAgainstReadyAttestedListener(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	var calls atomic.Uint64
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(_ context.Context, runtime CodexHTTPStartupValidationRuntime) error {
			calls.Add(1)
			if runtime.ListenerAddress != listener.Addr().String() {
				return errors.New("validation received the wrong listener")
			}
			if runtime.ServingAttestor != attestor {
				return errors.New("validation received the wrong serving attestor")
			}
			lease, err := runtime.ServingAttestor.Acquire(sha256.Sum256([]byte("startup validation binding")))
			if err != nil {
				return errors.New("validation ran before serving authority activation")
			}
			defer lease.Release()
			body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
			if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
				return errors.New("validation could not prove the serving listener")
			}
			if err := lease.Seal(); err != nil {
				return errors.New("validation could not seal the serving proof")
			}
			return nil
		}),
	}

	if err := server.serve(context.Background(), listener); err != nil {
		t.Fatalf("serve error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("startup validation calls = %d, want 1", got)
	}
}

func TestServerCodexHTTPStartupValidationDisabledKeepsServing(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()

	requestServingProofTestHealth(t, listener, "")
	select {
	case err := <-serveDone:
		t.Fatalf("server stopped without explicit startup validation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerCodexHTTPStartupValidationFailureStopsServer(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	want := errors.New("validation rejected evidence")
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(context.Context, CodexHTTPStartupValidationRuntime) error {
			return want
		}),
	}

	err := server.serve(context.Background(), listener)
	if !errors.Is(err, want) {
		t.Fatalf("serve error = %v, want validation failure", err)
	}
}

func TestServerCodexHTTPStartupValidationRequiresServingAttestor(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	var called atomic.Bool
	server := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(context.Context, CodexHTTPStartupValidationRuntime) error {
			called.Store(true)
			return nil
		}),
	}

	err := server.serve(context.Background(), listener)
	if err == nil || !strings.Contains(err.Error(), "serving attestor") {
		t.Fatalf("serve error = %v, want serving attestor requirement", err)
	}
	if called.Load() {
		t.Fatal("startup validation ran without serving authority")
	}
}

func TestServerCodexHTTPStartupValidationRecoversPanic(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	leakedLease := make(chan ServingProofLease, 1)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(_ context.Context, runtime CodexHTTPStartupValidationRuntime) error {
			lease, err := runtime.ServingAttestor.Acquire(sha256.Sum256([]byte("leaked panic lease")))
			if err != nil {
				return err
			}
			leakedLease <- lease
			panic("private validation detail")
		}),
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(context.Background(), listener) }()
	var lease ServingProofLease
	select {
	case lease = <-leakedLease:
	case <-time.After(time.Second):
		t.Fatal("startup validation did not acquire its proof lease")
	}
	defer lease.Release()
	var err error
	select {
	case err = <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("panic recovery waited for an unsealed proof lease")
	}
	if err == nil || !strings.Contains(err.Error(), "startup validation panicked") {
		t.Fatalf("serve error = %v, want panic recovery", err)
	}
	if strings.Contains(err.Error(), "private validation detail") {
		t.Fatalf("serve error leaked panic value: %v", err)
	}
}

func TestServerCodexHTTPStartupValidationCancellationDoesNotWaitForStuckCallback(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	entered := make(chan struct{})
	blocked := make(chan struct{})
	defer close(blocked)
	server := &Server{
		Config:              &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor:     NewServingAttestor(),
		shutdownGracePeriod: 20 * time.Millisecond,
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(_ context.Context, runtime CodexHTTPStartupValidationRuntime) error {
			lease, err := runtime.ServingAttestor.Acquire(sha256.Sum256([]byte("blocked validation lease")))
			if err != nil {
				return err
			}
			defer lease.Release()
			close(entered)
			<-blocked
			return nil
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup validation did not start")
	}
	cancel()
	select {
	case err := <-serveDone:
		if err == nil || !strings.Contains(err.Error(), "startup validation") {
			t.Fatalf("serve error = %v, want incomplete validation", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("server waited for a stuck startup validation callback")
	}
}

func TestServerCodexHTTPStartupValidationForcesDripBodyClosedWithoutMarker(t *testing.T) {
	listener := listenServingAttestorTestTCP4(t)
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := core.tempRoot
	markerPath := codexReadinessPath(t.TempDir(), CodexRoutingHTTP)
	server := &Server{
		Config:              &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor:     NewServingAttestor(),
		CodexNativeHTTP:     core.nativeHTTPHandler(),
		shutdownGracePeriod: 20 * time.Millisecond,
		CodexHTTPStartupValidation: CodexHTTPStartupValidationFunc(func(ctx context.Context, runtime CodexHTTPStartupValidationRuntime) error {
			connection, err := net.Dial("tcp4", runtime.ListenerAddress)
			if err != nil {
				return err
			}
			defer connection.Close()
			if _, err := fmt.Fprintf(connection, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer test-token\r\nContent-Length: 1048576\r\nConnection: close\r\n\r\n{", runtime.ListenerAddress); err != nil {
				return err
			}
			deadline := time.Now().Add(time.Second)
			for {
				core.nativeHTTPHandler().requests.mu.Lock()
				active := core.nativeHTTPHandler().requests.active
				core.nativeHTTPHandler().requests.mu.Unlock()
				if active == 1 {
					break
				}
				if time.Now().After(deadline) {
					return errors.New("drip-body request did not enter native admission")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			drainCtx, cancelDrain := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancelDrain()
			return core.nativeHTTPHandler().CloseAndDrain(drainCtx)
		}),
	}
	started := time.Now()
	err = server.serve(context.Background(), listener)
	if err == nil || !strings.Contains(err.Error(), "startup validation failed") {
		t.Fatalf("serve error = %v, want failed bounded validation", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("outer validation returned after %v, want bounded force-close", elapsed)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed drip-body validation left marker: %v", err)
	}
	if err := core.closeWithTimeout(time.Second); err != nil {
		t.Fatalf("core close after forced connection close: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated authority remained after forced close: %v", err)
	}
}
