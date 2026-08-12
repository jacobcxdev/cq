package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
)

var (
	errCodexHTTPStartupValidationPanic      = errors.New("Codex HTTP startup validation panicked")
	errCodexHTTPStartupValidationIncomplete = errors.New("Codex HTTP startup validation did not finish")
)

// CodexHTTPStartupValidationRuntime exposes only the exact listener generation
// needed by an explicitly configured in-process validation callback.
type CodexHTTPStartupValidationRuntime struct {
	ListenerAddress string
	ServingAttestor *ServingAttestor
}

// CodexHTTPStartupValidationFunc validates the production HTTP path after
// http.Server has entered the exact activated listener's Accept loop. It must
// retain any ServingProofLease until its sealed proof and marker write finish.
type CodexHTTPStartupValidationFunc func(context.Context, CodexHTTPStartupValidationRuntime) error

type codexHTTPStartupReadyListener struct {
	net.Listener
	once  sync.Once
	ready chan struct{}
}

type codexHTTPStartupValidationRun struct {
	result   <-chan error
	complete <-chan struct{}
}

func newCodexHTTPStartupReadyListener(listener net.Listener) *codexHTTPStartupReadyListener {
	return &codexHTTPStartupReadyListener{Listener: listener, ready: make(chan struct{})}
}

func (l *codexHTTPStartupReadyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.ready) })
	return l.Listener.Accept()
}

func runCodexHTTPStartupValidation(
	ctx context.Context,
	cancel context.CancelFunc,
	ready <-chan struct{},
	runtime CodexHTTPStartupValidationRuntime,
	validation CodexHTTPStartupValidationFunc,
) codexHTTPStartupValidationRun {
	result := make(chan error, 1)
	complete := make(chan struct{})
	go func() {
		var validationErr error
		defer func() {
			if recover() != nil {
				validationErr = errCodexHTTPStartupValidationPanic
			}
			if validationErr != nil && runtime.ServingAttestor != nil {
				runtime.ServingAttestor.abortUnexpected()
			}
			result <- validationErr
			close(complete)
			cancel()
		}()
		select {
		case <-ready:
			validationErr = validation(ctx, runtime)
		case <-ctx.Done():
			validationErr = ctx.Err()
		}
	}()
	return codexHTTPStartupValidationRun{result: result, complete: complete}
}

func codexHTTPStartupValidationCompleted(run codexHTTPStartupValidationRun) bool {
	select {
	case <-run.complete:
		return true
	default:
		return false
	}
}

func codexHTTPStartupValidationResult(run codexHTTPStartupValidationRun) error {
	select {
	case err := <-run.result:
		return err
	default:
		return errCodexHTTPStartupValidationIncomplete
	}
}
