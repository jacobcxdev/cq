package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
)

// codexNativeHTTPRequestGate is one process-owned, monotonic admission gate.
// Its active count is deliberately private; process shutdown can only close
// admission and wait for the opaque drain signal.
type codexNativeHTTPRequestGate struct {
	mu      sync.Mutex
	closing bool
	active  uint64
	drained chan struct{}
}

func newCodexNativeHTTPRequestGate() *codexNativeHTTPRequestGate {
	return &codexNativeHTTPRequestGate{drained: make(chan struct{})}
}

func (gate *codexNativeHTTPRequestGate) enter() (func(), bool) {
	if gate == nil {
		return nil, false
	}
	gate.mu.Lock()
	if gate.closing {
		gate.mu.Unlock()
		return nil, false
	}
	gate.active++
	gate.mu.Unlock()
	var once sync.Once
	return func() { once.Do(gate.leave) }, true
}

func (gate *codexNativeHTTPRequestGate) leave() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active == 0 {
		return
	}
	gate.active--
	if gate.closing && gate.active == 0 {
		close(gate.drained)
	}
}

func (gate *codexNativeHTTPRequestGate) closeAndDrain(ctx context.Context) error {
	if gate == nil {
		return errors.New("Codex native HTTP request gate unavailable")
	}
	if ctx == nil {
		return errors.New("Codex native HTTP drain context unavailable")
	}
	gate.mu.Lock()
	if !gate.closing {
		gate.closing = true
		if gate.active == 0 {
			close(gate.drained)
		}
	}
	drained := gate.drained
	gate.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeCodexNativeHTTPRejectedRequestBody(body io.Closer) {
	if body == nil {
		return
	}
	defer func() { _ = recover() }()
	_ = body.Close()
}
